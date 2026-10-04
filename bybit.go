package main

// ============================================================================
// BYBIT V5 REST CLIENT
//
// Dependency-free client for the Bybit v5 endpoints this bot needs: market
// data (klines, orderbook, tickers, instrument info), wallet balance,
// position info, isolated margin + leverage, order placement, and the
// native trading-stop endpoint used for the stop-loss.
//
// Every request goes through doRequest, which signs it per Bybit's v5 HMAC
// spec, retries on network errors / HTTP 429 / Bybit's own rate-limit
// retCode with exponential backoff + jitter, and unwraps the
// {retCode, retMsg, result} envelope into a typed error or raw JSON.
// ============================================================================

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	mainnetBaseURL = "https://api.bybit.com"
	testnetBaseURL = "https://api-testnet.bybit.com"

	retCodeRateLimited         = 10006 // Bybit's own "too many visits" code - retry with backoff
	retCodeLeverageNotModified = 110043
)

// BybitClient wraps everything needed to call the v5 REST API.
type BybitClient struct {
	baseURL     string
	apiKey      string
	apiSecret   string
	recvWindow  int64
	httpClient  *http.Client
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration
}

type bybitEnvelope struct {
	RetCode int             `json:"retCode"`
	RetMsg  string          `json:"retMsg"`
	Result  json.RawMessage `json:"result"`
	Time    int64           `json:"time"`
}

// NewBybitClient builds a client. useTestnet switches the base URL.
func NewBybitClient(cfg Config) *BybitClient {
	base := mainnetBaseURL
	if cfg.UseTestnet {
		base = testnetBaseURL
	}
	return &BybitClient{
		baseURL:     base,
		apiKey:      cfg.APIKey,
		apiSecret:   cfg.APISecret,
		recvWindow:  cfg.RecvWindowMs,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		maxRetries:  cfg.MaxRetries,
		baseBackoff: time.Duration(cfg.BaseBackoffMs) * time.Millisecond,
		maxBackoff:  time.Duration(cfg.MaxBackoffMs) * time.Millisecond,
	}
}

// sign computes the Bybit v5 HMAC-SHA256 signature.
// payload = timestamp + apiKey + recvWindow + (queryString for GET | body for POST)
func (c *BybitClient) sign(timestamp string, payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(timestamp + c.apiKey + strconv.FormatInt(c.recvWindow, 10) + payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func buildQueryString(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := url.Values{}
	for _, k := range keys {
		vals.Set(k, params[k])
	}
	return vals.Encode()
}

// doRequest performs a signed request, retrying on network errors / HTTP
// 429 / Bybit rate-limit retCode with exponential backoff + jitter.
// Returns the raw `result` JSON on success.
func (c *BybitClient) doRequest(method, endpoint string, params map[string]string, body interface{}) (json.RawMessage, error) {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyBytes = b
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoffDelay(c.baseBackoff, c.maxBackoff, attempt))
		}

		timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
		query := buildQueryString(params)

		var signPayload string
		var reqURL string
		var reqBody io.Reader

		if method == http.MethodGet {
			signPayload = query
			reqURL = c.baseURL + endpoint
			if query != "" {
				reqURL += "?" + query
			}
			reqBody = nil
		} else {
			signPayload = string(bodyBytes)
			reqURL = c.baseURL + endpoint
			reqBody = bytes.NewReader(bodyBytes)
		}

		signature := c.sign(timestamp, signPayload)

		req, err := http.NewRequest(method, reqURL, reqBody)
		if err != nil {
			lastErr = fmt.Errorf("build request: %w", err)
			continue
		}
		req.Header.Set("X-BAPI-API-KEY", c.apiKey)
		req.Header.Set("X-BAPI-SIGN", signature)
		req.Header.Set("X-BAPI-SIGN-TYPE", "2")
		req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
		req.Header.Set("X-BAPI-RECV-WINDOW", strconv.FormatInt(c.recvWindow, 10))
		if method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request failed: %w", err)
			continue
		}

		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read response body: %w", readErr)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("HTTP 429 rate limited: %s", string(respBytes))
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d server error: %s", resp.StatusCode, string(respBytes))
			continue
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBytes))
		}

		var env bybitEnvelope
		if err := json.Unmarshal(respBytes, &env); err != nil {
			lastErr = fmt.Errorf("decode envelope: %w (body=%s)", err, string(respBytes))
			continue
		}

		if env.RetCode == 0 {
			return env.Result, nil
		}
		if env.RetCode == retCodeLeverageNotModified {
			return env.Result, nil
		}
		if env.RetCode == retCodeRateLimited {
			lastErr = fmt.Errorf("bybit rate limited (retCode=%d): %s", env.RetCode, env.RetMsg)
			continue
		}

		return nil, fmt.Errorf("bybit error retCode=%d retMsg=%s", env.RetCode, env.RetMsg)
	}

	return nil, fmt.Errorf("exhausted %d retries: %w", c.maxRetries, lastErr)
}

func backoffDelay(base, max time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base * time.Duration(1<<uint(attempt-1))
	if d > max {
		d = max
	}
	jitter := time.Duration(rand.Int63n(int64(base)))
	return d + jitter
}

// ----------------------------------------------------------------------------
// Typed endpoint wrappers
// ----------------------------------------------------------------------------

// Kline is one OHLCV candle.
type Kline struct {
	StartMs int64
	Open    float64
	High    float64
	Low     float64
	Close   float64
	Volume  float64
}

// GetKlines fetches recent candles for a symbol at the given interval
// ("1" = 1 minute, "15" = 15 minutes, etc per Bybit v5 conventions).
// Returned oldest-first (Bybit's raw response is newest-first).
func (c *BybitClient) GetKlines(category, symbol, interval string, limit int) ([]Kline, error) {
	params := map[string]string{
		"category": category,
		"symbol":   symbol,
		"interval": interval,
		"limit":    strconv.Itoa(limit),
	}
	raw, err := c.doRequest(http.MethodGet, "/v5/market/kline", params, nil)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		List [][]string `json:"list"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode kline: %w", err)
	}
	klines := make([]Kline, 0, len(parsed.List))
	for i := len(parsed.List) - 1; i >= 0; i-- {
		row := parsed.List[i]
		if len(row) < 6 {
			continue
		}
		startMs, _ := strconv.ParseInt(row[0], 10, 64)
		open, _ := strconv.ParseFloat(row[1], 64)
		high, _ := strconv.ParseFloat(row[2], 64)
		low, _ := strconv.ParseFloat(row[3], 64)
		closeP, _ := strconv.ParseFloat(row[4], 64)
		vol, _ := strconv.ParseFloat(row[5], 64)
		klines = append(klines, Kline{startMs, open, high, low, closeP, vol})
	}
	return klines, nil
}

// OrderBookLevel is one [price, size] entry.
type OrderBookLevel struct {
	Price float64
	Size  float64
}

// OrderBook holds top-of-book bids and asks (best first).
type OrderBook struct {
	Bids []OrderBookLevel
	Asks []OrderBookLevel
}

// GetOrderBook fetches the top `depth` levels on each side.
func (c *BybitClient) GetOrderBook(category, symbol string, depth int) (OrderBook, error) {
	params := map[string]string{
		"category": category,
		"symbol":   symbol,
		"limit":    strconv.Itoa(depth),
	}
	raw, err := c.doRequest(http.MethodGet, "/v5/market/orderbook", params, nil)
	if err != nil {
		return OrderBook{}, err
	}
	var parsed struct {
		B [][]string `json:"b"`
		A [][]string `json:"a"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return OrderBook{}, fmt.Errorf("decode orderbook: %w", err)
	}
	toLevels := func(rows [][]string) []OrderBookLevel {
		out := make([]OrderBookLevel, 0, len(rows))
		for _, r := range rows {
			if len(r) < 2 {
				continue
			}
			p, _ := strconv.ParseFloat(r[0], 64)
			s, _ := strconv.ParseFloat(r[1], 64)
			out = append(out, OrderBookLevel{p, s})
		}
		return out
	}
	return OrderBook{Bids: toLevels(parsed.B), Asks: toLevels(parsed.A)}, nil
}

// GetLastPrice fetches the latest traded price via the tickers endpoint.
func (c *BybitClient) GetLastPrice(category, symbol string) (float64, error) {
	params := map[string]string{"category": category, "symbol": symbol}
	raw, err := c.doRequest(http.MethodGet, "/v5/market/tickers", params, nil)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		List []struct {
			LastPrice string `json:"lastPrice"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("decode tickers: %w", err)
	}
	if len(parsed.List) == 0 {
		return 0, errors.New("empty ticker list")
	}
	price, err := strconv.ParseFloat(parsed.List[0].LastPrice, 64)
	if err != nil {
		return 0, fmt.Errorf("parse last price: %w", err)
	}
	return price, nil
}

// InstrumentInfo carries exchange rules needed for sizing and rounding.
type InstrumentInfo struct {
	QtyStep     float64
	MinOrderQty float64
	PriceTick   float64
	MaxLeverage float64
}

func (c *BybitClient) GetInstrumentInfo(category, symbol string) (InstrumentInfo, error) {
	params := map[string]string{"category": category, "symbol": symbol}
	raw, err := c.doRequest(http.MethodGet, "/v5/market/instruments-info", params, nil)
	if err != nil {
		return InstrumentInfo{}, err
	}
	var parsed struct {
		List []struct {
			LotSizeFilter struct {
				QtyStep     string `json:"qtyStep"`
				MinOrderQty string `json:"minOrderQty"`
			} `json:"lotSizeFilter"`
			PriceFilter struct {
				TickSize string `json:"tickSize"`
			} `json:"priceFilter"`
			LeverageFilter struct {
				MaxLeverage string `json:"maxLeverage"`
			} `json:"leverageFilter"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return InstrumentInfo{}, fmt.Errorf("decode instruments-info: %w", err)
	}
	if len(parsed.List) == 0 {
		return InstrumentInfo{}, errors.New("empty instrument list")
	}
	item := parsed.List[0]
	qtyStep, _ := strconv.ParseFloat(item.LotSizeFilter.QtyStep, 64)
	minQty, _ := strconv.ParseFloat(item.LotSizeFilter.MinOrderQty, 64)
	tick, _ := strconv.ParseFloat(item.PriceFilter.TickSize, 64)
	maxLev, _ := strconv.ParseFloat(item.LeverageFilter.MaxLeverage, 64)
	return InstrumentInfo{QtyStep: qtyStep, MinOrderQty: minQty, PriceTick: tick, MaxLeverage: maxLev}, nil
}

// GetFreeUSDT reads the unified account's free (available) USDT balance.
func (c *BybitClient) GetFreeUSDT() (float64, error) {
	params := map[string]string{"accountType": "UNIFIED", "coin": "USDT"}
	raw, err := c.doRequest(http.MethodGet, "/v5/account/wallet-balance", params, nil)
	if err != nil {
		return 0, err
	}
	var parsed struct {
		List []struct {
			Coin []struct {
				Coin                string `json:"coin"`
				WalletBalance       string `json:"walletBalance"`
				AvailableToWithdraw string `json:"availableToWithdraw"`
			} `json:"coin"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, fmt.Errorf("decode wallet-balance: %w", err)
	}
	for _, acct := range parsed.List {
		for _, coin := range acct.Coin {
			if coin.Coin != "USDT" {
				continue
			}
			val := coin.AvailableToWithdraw
			if val == "" {
				val = coin.WalletBalance
			}
			f, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return 0, fmt.Errorf("parse balance: %w", err)
			}
			return f, nil
		}
	}
	return 0, errors.New("USDT balance not found in wallet-balance response")
}

// Position mirrors the fields we care about from /v5/position/list.
type Position struct {
	Symbol      string
	Side        string // "Buy" (long) or "Sell" (short), empty if flat
	Size        float64
	EntryPrice  float64
	PositionIdx int
}

func (p Position) IsOpen() bool {
	return p.Size > 0 && p.Side != ""
}

func (c *BybitClient) GetPosition(category, symbol string) (Position, error) {
	params := map[string]string{"category": category, "symbol": symbol}
	raw, err := c.doRequest(http.MethodGet, "/v5/position/list", params, nil)
	if err != nil {
		return Position{}, err
	}
	var parsed struct {
		List []struct {
			Symbol      string `json:"symbol"`
			Side        string `json:"side"`
			Size        string `json:"size"`
			AvgPrice    string `json:"avgPrice"`
			PositionIdx int    `json:"positionIdx"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Position{}, fmt.Errorf("decode position list: %w", err)
	}
	for _, p := range parsed.List {
		size, _ := strconv.ParseFloat(p.Size, 64)
		if size <= 0 {
			continue
		}
		entry, _ := strconv.ParseFloat(p.AvgPrice, 64)
		return Position{Symbol: p.Symbol, Side: p.Side, Size: size, EntryPrice: entry, PositionIdx: p.PositionIdx}, nil
	}
	return Position{}, nil
}

// SetIsolatedLeverage switches the symbol to isolated margin mode AND sets
// leverage in one call, per the spec ("isolated, leverage 30x"). If the
// account is already in isolated mode at this leverage, Bybit may return a
// "not modified"-style error - we log it as a warning rather than treating
// the whole startup as fatal, since the account is very likely already in
// the desired state from a previous run.
func (c *BybitClient) SetIsolatedLeverage(category, symbol string, leverage int) error {
	body := map[string]interface{}{
		"category":     category,
		"symbol":       symbol,
		"tradeMode":    1, // 1 = isolated margin, 0 = cross margin
		"buyLeverage":  strconv.Itoa(leverage),
		"sellLeverage": strconv.Itoa(leverage),
	}
	_, err := c.doRequest(http.MethodPost, "/v5/position/switch-isolated", nil, body)
	return err
}

// SetLeverage adjusts leverage only (used as a fallback / for re-applying
// leverage without touching margin mode, e.g. on bot restart).
func (c *BybitClient) SetLeverage(category, symbol string, leverage int) error {
	body := map[string]interface{}{
		"category":     category,
		"symbol":       symbol,
		"buyLeverage":  strconv.Itoa(leverage),
		"sellLeverage": strconv.Itoa(leverage),
	}
	_, err := c.doRequest(http.MethodPost, "/v5/position/set-leverage", nil, body)
	return err
}

// PlaceMarketOrderParams configures a market order.
type PlaceMarketOrderParams struct {
	Category   string
	Symbol     string
	Side       string // "Buy" or "Sell"
	Qty        string
	ReduceOnly bool
}

// PlaceMarketOrder fires an instant market order and returns the exchange
// order ID.
func (c *BybitClient) PlaceMarketOrder(p PlaceMarketOrderParams) (string, error) {
	body := map[string]interface{}{
		"category":   p.Category,
		"symbol":     p.Symbol,
		"side":       p.Side,
		"orderType":  "Market",
		"qty":        p.Qty,
		"reduceOnly": p.ReduceOnly,
	}
	raw, err := c.doRequest(http.MethodPost, "/v5/order/create", nil, body)
	if err != nil {
		return "", err
	}
	var parsed struct {
		OrderId string `json:"orderId"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("decode order create: %w", err)
	}
	return parsed.OrderId, nil
}

// SetStopLoss attaches (or amends) the native exchange-side stop-loss on an
// OPEN position via /v5/position/trading-stop. This lives on Bybit's
// servers, so it keeps protecting the position even if this bot's process
// or VPS goes down. Per the spec, this is called at entry (initial SL) and
// then AT MOST ONCE MORE (moving to break-even) - never trailed further.
func (c *BybitClient) SetStopLoss(category, symbol string, positionIdx int, stopLossPrice string) error {
	body := map[string]interface{}{
		"category":    category,
		"symbol":      symbol,
		"positionIdx": positionIdx,
		"tpslMode":    "Full",
		"stopLoss":    stopLossPrice,
	}
	_, err := c.doRequest(http.MethodPost, "/v5/position/trading-stop", nil, body)
	return err
}

// ClosePosition fires a reduce-only market order to flatten a position.
func (c *BybitClient) ClosePosition(category, symbol, closeSide, qty string) error {
	_, err := c.PlaceMarketOrder(PlaceMarketOrderParams{
		Category:   category,
		Symbol:     symbol,
		Side:       closeSide,
		Qty:        qty,
		ReduceOnly: true,
	})
	return err
}

// formatFloat trims a float to a fixed number of decimal places without
// scientific notation - used for qty/price strings sent to Bybit.
func formatFloat(f float64, decimals int) string {
	s := strconv.FormatFloat(f, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" {
		s = "0"
	}
	return s
}

// decimalsFromStep infers how many decimal places to format a qty/price
// string with, based on the exchange's step/tick size (e.g. 0.001 -> 3).
func decimalsFromStep(step float64) int {
	if step <= 0 {
		return 0
	}
	s := strconv.FormatFloat(step, 'f', -1, 64)
	dot := strings.IndexByte(s, '.')
	if dot == -1 {
		return 0
	}
	return len(s) - dot - 1
}
