package main

// ============================================================================
// CONFIG - loads every tunable knob from a ".env" file (if present) and the
// real OS environment. Real environment variables always win over the file,
// so you can override anything with `SOME_VAR=x ./bandar-reversal-bot`
// without editing .env.
// ============================================================================

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds every setting the bot needs. Defaults match the spec exactly.
type Config struct {
	// --- Bybit API credentials & environment ---
	APIKey     string
	APISecret  string
	UseTestnet bool

	// --- Symbol, category, leverage, margin mode ---
	Symbol   string // raw Bybit v5 symbol, e.g. "1000BONKUSDT" - NO slashes/colons
	Category string // "linear" for USDT perpetuals
	Leverage int     // requested leverage; clipped to the symbol's real exchange max at startup
	Isolated bool    // true = isolated margin mode (spec requires isolated)

	// --- Position sizing ("compounding", 100% balance per trade) ---
	PositionPct    float64 // fraction of free USDT balance used as margin per trade (1.0 = 100%)
	MinFreeBalance float64 // absolute floor in USDT - refuse to trade below this

	// --- Candle structure (support/resistance) ---
	Lookback  int     // candles used to compute rolling support/resistance
	ATRPeriod int     // candles used for ATR
	ZoneATR   float64 // zone tolerance around support/resistance, as a multiple of ATR

	// --- RSI filter (second gate, after candle zone match) ---
	RSIPeriod   int
	RSILongMin  float64
	RSILongMax  float64
	RSIShortMin float64
	RSIShortMax float64

	// --- Loose confirmations (need 2 of 4 to pass) ---
	EMAPeriod    int
	EMATol       float64 // EMA tolerance fraction, e.g. 0.003 = 0.3%
	MinWick      float64 // minimum wick-to-range ratio
	MinVol       float64 // minimum current volume vs recent average
	OBEntryRatio float64 // order-book entry confirmation ratio (loose, e.g. 1.2x)
	OBEntryDepth int     // levels summed for the entry order-book check (top 5)

	// --- "REVERSAL GAGAH" - the core profit-protection system ---
	OBReversalDepth         int     // levels summed for reversal detection (top 15)
	OBFlipRatio             float64 // volume-flip trigger ratio (e.g. 3.0x = whale sell/buy wall)
	OBWallRatio             float64 // single-order-vs-average-size ratio to flag a "bandar wall"
	WallDistancePct         float64 // how close (fraction of price) a wall must be to count
	CVDLookbackTicks        int     // how many reversal-watcher ticks back to compare for CVD divergence
	CVDDropPct              float64 // how much bid/ask volume must have dropped to count as divergence
	ReversalProfitTriggerPct float64 // minimum profit (fraction) before the reversal watcher is allowed to close
	ReversalCheckIntervalMs int     // how often the dedicated reversal goroutine polls the order book

	// --- SL / break-even (NO trailing beyond BE - moves ONCE, then locked) ---
	SLPct       float64 // initial stop-loss distance, as a PERCENT (e.g. 1.5 = 1.5%)
	BETriggerPct float64 // profit percent at which SL moves to break-even (e.g. 0.6 = 0.6%)
	BEOffsetPct float64 // how far past pure break-even to move SL, to cover fees (e.g. 0.05 = 0.05%)

	// --- "Hold for pump" - higher-timeframe adaptive target instead of a fixed TP ---
	HTFInterval  string // Bybit v5 kline interval string for the higher timeframe, e.g. "15" = 15m
	HTFLookback  int    // candles used to find the next resistance/support on the higher timeframe

	// --- Loop timing ---
	ScanIntervalMs   int // how often to scan for a new signal while flat
	ManageIntervalMs int // how often the main management loop polls price/position while a trade is open

	// --- HTTP / rate limiting ---
	RecvWindowMs  int64
	MaxRetries    int
	BaseBackoffMs int
	MaxBackoffMs  int
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getEnvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// LoadConfig assembles the full Config from .env + environment, applying
// every default from the spec.
func LoadConfig() Config {
	loadDotEnv(".env")

	return Config{
		APIKey:     getEnvStr("BYBIT_API_KEY", ""),
		APISecret:  getEnvStr("BYBIT_API_SECRET", ""),
		UseTestnet: getEnvBool("USE_TESTNET", true),

		Symbol:   getEnvStr("SYMBOL", "1000BONKUSDT"),
		Category: "linear",
		Leverage: getEnvInt("LEVERAGE", 30),
		Isolated: getEnvBool("ISOLATED", true),

		PositionPct:    getEnvFloat("POSITION_PCT", 1.0),
		MinFreeBalance: getEnvFloat("MIN_FREE_BALANCE_USDT", 1.0),

		Lookback:  getEnvInt("LOOKBACK", 20),
		ATRPeriod: getEnvInt("ATR_PERIOD", 14),
		ZoneATR:   getEnvFloat("ZONE_ATR", 1.0),

		RSIPeriod:   getEnvInt("RSI_PERIOD", 14),
		RSILongMin:  getEnvFloat("RSI_LONG_MIN", 39),
		RSILongMax:  getEnvFloat("RSI_LONG_MAX", 45),
		RSIShortMin: getEnvFloat("RSI_SHORT_MIN", 69),
		RSIShortMax: getEnvFloat("RSI_SHORT_MAX", 100),

		EMAPeriod:    getEnvInt("EMA_PERIOD", 9),
		EMATol:       getEnvFloat("EMA_TOL", 0.003),
		MinWick:      getEnvFloat("MIN_WICK", 0.05),
		MinVol:       getEnvFloat("MIN_VOL", 0.1),
		OBEntryRatio: getEnvFloat("OB_ENTRY_RATIO", 1.2),
		OBEntryDepth: getEnvInt("OB_ENTRY_DEPTH", 5),

		OBReversalDepth:          getEnvInt("OB_REVERSAL_DEPTH", 15),
		OBFlipRatio:              getEnvFloat("OB_FLIP_RATIO", 3.0),
		OBWallRatio:              getEnvFloat("OB_WALL_RATIO", 5.0),
		WallDistancePct:          getEnvFloat("WALL_DISTANCE_PCT", 0.003),
		CVDLookbackTicks:         getEnvInt("CVD_LOOKBACK_TICKS", 5),
		CVDDropPct:               getEnvFloat("CVD_DROP_PCT", 0.3),
		ReversalProfitTriggerPct: getEnvFloat("REVERSAL_PROFIT_TRIGGER_PCT", 0.5),
		ReversalCheckIntervalMs:  getEnvInt("REVERSAL_CHECK_INTERVAL_MS", 300),

		SLPct:        getEnvFloat("SL_PCT", 1.5),
		BETriggerPct: getEnvFloat("BE_TRIGGER", 0.6),
		BEOffsetPct:  getEnvFloat("BE_OFFSET_PCT", 0.05),

		HTFInterval: getEnvStr("HTF_INTERVAL", "15"),
		HTFLookback: getEnvInt("HTF_LOOKBACK", 50),

		ScanIntervalMs:   getEnvInt("SCAN_INTERVAL_MS", 300),
		ManageIntervalMs: getEnvInt("MANAGE_INTERVAL_MS", 500),

		RecvWindowMs:  int64(getEnvInt("RECV_WINDOW_MS", 5000)),
		MaxRetries:    getEnvInt("MAX_RETRIES", 5),
		BaseBackoffMs: getEnvInt("BASE_BACKOFF_MS", 400),
		MaxBackoffMs:  getEnvInt("MAX_BACKOFF_MS", 8000),
	}
}
