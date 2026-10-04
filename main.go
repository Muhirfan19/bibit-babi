package main

// ============================================================================
// BANDAR REVERSAL BOT - Go / Bybit v5 REST
//
// 1000BONKUSDT linear perpetual, isolated, 30x. Candle-first + RSI entry
// gate, loose 2-of-4 confirmations, all-in compounding sizing, fixed SL
// that locks to break-even exactly once (never trails further), an
// adaptive higher-timeframe "hold for pump" target instead of a fixed TP,
// and a dedicated "REVERSAL GAGAH" goroutine watching the order book for
// three independent danger signals the entire time a position is open.
//
// Run with: go build -o bandar-reversal-bot . && ./bandar-reversal-bot
// Config is loaded from ".env" in the working directory.
// ============================================================================

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

const klineInterval = "1" // Bybit v5 interval string for 1-minute candles

func logf(format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05")
	fmt.Printf("%s | %s\n", ts, fmt.Sprintf(format, args...))
}

func main() {
	cfg := LoadConfig()

	if cfg.APIKey == "" || cfg.APISecret == "" {
		fmt.Println("FATAL: BYBIT_API_KEY / BYBIT_API_SECRET missing. Set them in .env or the environment.")
		os.Exit(1)
	}
	if strings.ContainsAny(cfg.Symbol, "/:") {
		fmt.Printf("FATAL: SYMBOL %q looks like a CCXT-style symbol. Bybit's raw v5 API wants the bare "+
			"form, e.g. \"1000BONKUSDT\" - no slashes or colons.\n", cfg.Symbol)
		os.Exit(1)
	}

	printBanner(cfg)

	client := NewBybitClient(cfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		logf("[SHUTDOWN] Signal received. Exiting cleanly. Any open position stays open on Bybit - " +
			"its native stop-loss keeps protecting it (though the reversal watcher and HTF target logic " +
			"stop running with this process). Check the app if you need to close manually.")
		os.Exit(0)
	}()

	logf("[INIT] Fetching instrument info for %s...", cfg.Symbol)
	instrument, err := client.GetInstrumentInfo(cfg.Category, cfg.Symbol)
	if err != nil {
		fmt.Printf("FATAL: could not fetch instrument info for %s: %v\n", cfg.Symbol, err)
		os.Exit(1)
	}
	logf("[INIT] %s: qtyStep=%v minOrderQty=%v priceTick=%v exchangeMaxLeverage=%v",
		cfg.Symbol, instrument.QtyStep, instrument.MinOrderQty, instrument.PriceTick, instrument.MaxLeverage)

	if instrument.MaxLeverage > 0 && float64(cfg.Leverage) > instrument.MaxLeverage {
		clipped := int(instrument.MaxLeverage)
		logf("[INIT] Requested leverage %dx exceeds exchange max %.0fx for %s - clipping to %dx.",
			cfg.Leverage, instrument.MaxLeverage, cfg.Symbol, clipped)
		cfg.Leverage = clipped
	}

	logf("[INIT] Setting ISOLATED margin + leverage to %dx...", cfg.Leverage)
	if err := client.SetIsolatedLeverage(cfg.Category, cfg.Symbol, cfg.Leverage); err != nil {
		// Don't treat this as fatal - very often it just means the account
		// is already in isolated mode at this exact leverage from a
		// previous run. Log it loudly so the user can double check the
		// Bybit app if something actually looks wrong.
		logf("[WARN] SetIsolatedLeverage returned an error (often harmless - account may already be "+
			"isolated at %dx): %v", cfg.Leverage, err)
	}

	free, err := client.GetFreeUSDT()
	if err != nil {
		logf("[WARN] Could not fetch starting free balance: %v", err)
	} else {
		logf("[INIT] Starting free balance: %.4f USDT", free)
	}

	logf("[INIT] Entering unbreakable hunting loop. Ctrl+C to stop gracefully.")
	huntLoop(client, cfg, instrument)
}

func printBanner(cfg Config) {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println(" BANDAR REVERSAL BOT - Go / Bybit v5 REST")
	fmt.Printf(" Symbol=%s | Leverage(requested)=%dx | Isolated=%v | Testnet=%v\n",
		cfg.Symbol, cfg.Leverage, cfg.Isolated, cfg.UseTestnet)
	fmt.Printf(" PositionPct=%.0f%% | SL=%.2f%% (locks to BE+%.2f%% once at +%.2f%% profit, never trails further)\n",
		cfg.PositionPct*100, cfg.SLPct, cfg.BEOffsetPct, cfg.BETriggerPct)
	fmt.Printf(" RSI(long)=%.0f-%.0f | RSI(short)=%.0f-%.0f | ZoneATR=%.2fx | Need 2-of-4 loose confirms\n",
		cfg.RSILongMin, cfg.RSILongMax, cfg.RSIShortMin, cfg.RSIShortMax, cfg.ZoneATR)
	fmt.Printf(" REVERSAL GAGAH: flipRatio=%.1fx wallRatio=%.1fx cvdDrop=%.0f%% | triggers above +%.2f%% profit\n",
		cfg.OBFlipRatio, cfg.OBWallRatio, cfg.CVDDropPct*100, cfg.ReversalProfitTriggerPct)
	fmt.Println(strings.Repeat("=", 78))
}

// ----------------------------------------------------------------------------
// HUNTING LOOP
// ----------------------------------------------------------------------------

func huntLoop(client *BybitClient, cfg Config, instrument InstrumentInfo) {
	scanInterval := time.Duration(cfg.ScanIntervalMs) * time.Millisecond

	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logf("[LOOP GUARD] Recovered from panic: %v\n%s", r, debug.Stack())
					time.Sleep(1 * time.Second)
				}
			}()
			runOneCycle(client, cfg, instrument, scanInterval)
		}()
	}
}

func minKlineLimit(cfg Config) int {
	n := cfg.Lookback + cfg.ATRPeriod + cfg.RSIPeriod + 10
	if n < 60 {
		n = 60
	}
	return n
}

func runOneCycle(client *BybitClient, cfg Config, instrument InstrumentInfo, scanInterval time.Duration) {
	pos, err := client.GetPosition(cfg.Category, cfg.Symbol)
	if err != nil {
		logf("[WARN] fetch position failed: %v", err)
		time.Sleep(scanInterval)
		return
	}

	if pos.IsOpen() {
		side := "long"
		if pos.Side == "Sell" {
			side = "short"
		}
		klines, err := client.GetKlines(cfg.Category, cfg.Symbol, klineInterval, minKlineLimit(cfg))
		if err != nil || len(klines) < cfg.ATRPeriod+2 {
			logf("[WARN] could not recompute ATR for existing position (%v); retrying shortly.", err)
			time.Sleep(scanInterval)
			return
		}
		closed := klines[:len(klines)-1]
		atr, ok := ATR(closed, cfg.ATRPeriod)
		if !ok || atr <= 0 {
			logf("[WARN] ATR unavailable for existing position; retrying shortly.")
			time.Sleep(scanInterval)
			return
		}
		ManagePosition(client, cfg, instrument, side, pos.EntryPrice, pos.Size, atr)
		return
	}

	klines, err := client.GetKlines(cfg.Category, cfg.Symbol, klineInterval, minKlineLimit(cfg))
	if err != nil {
		logf("[WARN] fetch kline failed: %v", err)
		time.Sleep(scanInterval)
		return
	}
	if len(klines) == 0 {
		time.Sleep(scanInterval)
		return
	}

	sig := DetectSetup(klines, cfg)
	if sig == nil {
		time.Sleep(scanInterval)
		return
	}

	obPass := CheckEntryOB(client, cfg, sig.Side)
	totalLoose := sig.LooseCount
	if obPass {
		totalLoose++
	}

	logf("[SIGNAL] %s side=%s rsi=%.2f structure=%.8f looseCount=%d/4 (ob_pass=%v)",
		cfg.Symbol, strings.ToUpper(sig.Side), sig.RSI, sig.Structure, totalLoose, obPass)

	if totalLoose < 2 {
		logf("[SIGNAL SKIP] only %d/4 loose confirmations passed (need >=2) - skipping.", totalLoose)
		time.Sleep(scanInterval)
		return
	}

	price, err := client.GetLastPrice(cfg.Category, cfg.Symbol)
	if err != nil {
		logf("[WARN] fetch last price for entry failed: %v", err)
		time.Sleep(scanInterval)
		return
	}

	if err := OpenPosition(client, cfg, instrument, sig.Side, price); err != nil {
		logf("[SKIP] entry failed: %v", err)
		time.Sleep(scanInterval)
		return
	}

	time.Sleep(500 * time.Millisecond)
	fresh, err := client.GetPosition(cfg.Category, cfg.Symbol)
	if err != nil || !fresh.IsOpen() {
		logf("[SYNC] no position found right after entry order - assuming it never actually opened " +
			"or was instantly closed. Resuming hunt.")
		return
	}

	side := "long"
	if fresh.Side == "Sell" {
		side = "short"
	}
	ManagePosition(client, cfg, instrument, side, fresh.EntryPrice, fresh.Size, sig.ATR)
}

// ----------------------------------------------------------------------------
// POSITION SIZING & ENTRY - "100% balance, compounding" per spec
// ----------------------------------------------------------------------------

func decimalsForQty(instrument InstrumentInfo) int {
	return decimalsFromStep(instrument.QtyStep)
}

func decimalsForPrice(instrument InstrumentInfo) int {
	return decimalsFromStep(instrument.PriceTick)
}

func roundDownToStep(qty, step float64) float64 {
	if step <= 0 {
		return qty
	}
	return math.Floor(qty/step) * step
}

func roundToTick(price, tick float64) float64 {
	if tick <= 0 {
		return price
	}
	return math.Round(price/tick) * tick
}

// CalcPositionSize fetches the LIVE free USDT balance and applies
// PositionPct * Leverage. This is where compounding happens automatically:
// balance 10 USDT -> profit 2 USDT -> next trade sizes off 12 USDT.
func CalcPositionSize(client *BybitClient, cfg Config, instrument InstrumentInfo, price float64) (float64, float64, error) {
	free, err := client.GetFreeUSDT()
	if err != nil {
		return 0, 0, fmt.Errorf("fetch free balance: %w", err)
	}
	if free < cfg.MinFreeBalance {
		return 0, free, fmt.Errorf(
			"free balance %.4f USDT is below the configured minimum %.4f - refusing to trade dust amounts",
			free, cfg.MinFreeBalance)
	}

	pct := cfg.PositionPct
	if pct > 1.0 {
		pct = 1.0 // hard safety ceiling - never exceed 100% even on a config mistake
	}

	margin := free * pct
	notional := margin * float64(cfg.Leverage)
	rawQty := notional / price

	qty := roundDownToStep(rawQty, instrument.QtyStep)
	if qty < instrument.MinOrderQty {
		return 0, free, fmt.Errorf(
			"computed qty %.8f is below exchange minimum %.8f - balance too small for %s at current price",
			qty, instrument.MinOrderQty, cfg.Symbol)
	}
	return qty, free, nil
}

// OpenPosition fires a market entry order, sized via CalcPositionSize.
func OpenPosition(client *BybitClient, cfg Config, instrument InstrumentInfo, side string, price float64) error {
	qty, free, err := CalcPositionSize(client, cfg, instrument, price)
	if err != nil {
		return err
	}

	orderSide := "Buy"
	if side == "short" {
		orderSide = "Sell"
	}
	qtyStr := formatFloat(qty, decimalsForQty(instrument))

	logf("[ENTRY] MARKET %s %s qty=%s (leverage=%dx, balance=%.4f USDT, compounding active)",
		orderSide, cfg.Symbol, qtyStr, cfg.Leverage, free)

	_, err = client.PlaceMarketOrder(PlaceMarketOrderParams{
		Category:   cfg.Category,
		Symbol:     cfg.Symbol,
		Side:       orderSide,
		Qty:        qtyStr,
		ReduceOnly: false,
	})
	if err != nil {
		return fmt.Errorf("place market entry: %w", err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// TRADE MANAGEMENT
// ----------------------------------------------------------------------------

// pnlPercent returns PnL as a PERCENT value (e.g. 0.6 means 0.6%), matching
// how SLPct/BETriggerPct/BEOffsetPct/ReversalProfitTriggerPct are expressed
// in config (human-friendly percent numbers, not raw fractions).
func pnlPercent(side string, entry, price float64) float64 {
	if entry == 0 {
		return 0
	}
	var frac float64
	if side == "long" {
		frac = (price - entry) / entry
	} else {
		frac = (entry - price) / entry
	}
	return frac * 100
}

func initialSLPrice(side string, entry float64, cfg Config) float64 {
	if side == "long" {
		return entry * (1 - cfg.SLPct/100)
	}
	return entry * (1 + cfg.SLPct/100)
}

func breakEvenSLPrice(side string, entry float64, cfg Config) float64 {
	if side == "long" {
		return entry * (1 + cfg.BEOffsetPct/100)
	}
	return entry * (1 - cfg.BEOffsetPct/100)
}

// refreshHTFTarget pulls fresh higher-timeframe candles and recomputes the
// "hold for pump" adaptive target. Called at management start and
// periodically during the hold, so the target naturally updates as new
// HTF candles close (e.g. an old resistance gets replaced by a fresh,
// closer one, or a broken level is skipped in favor of the next one up).
func refreshHTFTarget(client *BybitClient, cfg Config, side string, price float64) (float64, bool) {
	htfKlines, err := client.GetKlines(cfg.Category, cfg.Symbol, cfg.HTFInterval, cfg.HTFLookback+2)
	if err != nil || len(htfKlines) < 2 {
		return 0, false
	}
	closed := htfKlines[:len(htfKlines)-1] // exclude the still-forming HTF candle
	return FindHTFTarget(closed, side, price)
}

// closePositionNow fetches the live position size (ground truth - never
// trust a stale qty variable) and fires a reduce-only market close.
func closePositionNow(client *BybitClient, cfg Config, instrument InstrumentInfo, side, reason string) {
	pos, err := client.GetPosition(cfg.Category, cfg.Symbol)
	if err != nil || !pos.IsOpen() {
		logf("[CLOSE] %s: position already flat when attempting to close for reason=%s.", cfg.Symbol, reason)
		return
	}
	closeSide := "Sell"
	if side == "short" {
		closeSide = "Buy"
	}
	qtyStr := formatFloat(pos.Size, decimalsForQty(instrument))
	logf("[EXIT] MARKET REDUCE-ONLY %s %s qty=%s | reason=%s", closeSide, cfg.Symbol, qtyStr, reason)
	if err := client.ClosePosition(cfg.Category, cfg.Symbol, closeSide, qtyStr); err != nil {
		logf("[ERROR] close position failed (reason=%s): %v", reason, err)
	}
}

// ManagePosition runs the full lifecycle of one open trade:
//   1. Attach the initial hard SL (or, on a bot restart mid-trade that's
//      already past the BE trigger, go straight to the locked BE stop
//      instead of re-widening protection).
//   2. Launch the REVERSAL GAGAH goroutine for the whole hold.
//   3. Loop: move SL to break-even ONCE when the trigger is hit (and never
//      again - per spec, no trailing beyond BE), refresh the "hold for
//      pump" HTF target periodically, and close on whichever happens
//      first: HTF target reached, or the reversal watcher fires.
//   4. Detect a manual close (position disappearing) every tick and bail
//      out cleanly if so.
// Note: qty and atr are accepted for symmetry with the caller's data (and
// so future extensions have them on hand) but are not read directly below
// - position size is always re-fetched live from GetPosition before any
// close (never trusted from a stale snapshot), and the SL in this spec is
// a fixed percentage of entry (SLPct), not ATR-based, so atr isn't needed
// for the stop calculation itself.
func ManagePosition(client *BybitClient, cfg Config, instrument InstrumentInfo, side string, entryPrice, qty, atr float64) {
	_ = qty
	_ = atr
	priceDecimals := decimalsForPrice(instrument)

	startPrice, err := client.GetLastPrice(cfg.Category, cfg.Symbol)
	if err != nil {
		startPrice = entryPrice
	}
	startPnl := pnlPercent(side, entryPrice, startPrice)

	beArmed := startPnl >= cfg.BETriggerPct
	var slPrice float64
	if beArmed {
		slPrice = breakEvenSLPrice(side, entryPrice, cfg)
	} else {
		slPrice = initialSLPrice(side, entryPrice, cfg)
	}
	slPrice = roundToTick(slPrice, instrument.PriceTick)
	slStr := formatFloat(slPrice, priceDecimals)

	if err := client.SetStopLoss(cfg.Category, cfg.Symbol, 0, slStr); err != nil {
		logf("[SAFETY] Native SL placement failed (%v); closing position rather than running unprotected.", err)
		closePositionNow(client, cfg, instrument, side, "SL_PLACEMENT_FAILED")
		return
	}

	if beArmed {
		logf("[MANAGE] %s %s entry=%.8f SL=%.8f (already past BE trigger on reattach - locked at BE+fee, "+
			"will NEVER move again)", cfg.Symbol, side, entryPrice, slPrice)
	} else {
		logf("[MANAGE] %s %s entry=%.8f initial_SL=%.8f (-%.2f%%) - will move to BE ONCE at +%.2f%% profit, "+
			"then lock forever", cfg.Symbol, side, entryPrice, slPrice, cfg.SLPct, cfg.BETriggerPct)
	}

	htfTarget, htfOk := refreshHTFTarget(client, cfg, side, startPrice)
	if htfOk {
		logf("[TARGET] %s hold-for-pump target (HTF %s): %.8f", cfg.Symbol, cfg.HTFInterval, htfTarget)
	} else {
		logf("[TARGET] %s no higher-timeframe target available yet - holding, reversal watcher stays active.", cfg.Symbol)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan ReversalResult, 1)
	pnlFn := func(mid float64) float64 { return pnlPercent(side, entryPrice, mid) }
	go RunReversalWatcher(ctx, client, cfg, side, pnlFn, resultCh)

	ticker := time.NewTicker(time.Duration(cfg.ManageIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	htfRefreshEvery := 10 // refresh the HTF target every N manage ticks, not every single one
	tickCount := 0

	for {
		select {
		case res := <-resultCh:
			logf("[REVERSAL GAGAH] %s | %s", res.Reason, res.Detail)
			closePositionNow(client, cfg, instrument, side, "REVERSAL_"+res.Reason)
			return

		case <-ticker.C:
			tickCount++

			pos, err := client.GetPosition(cfg.Category, cfg.Symbol)
			if err != nil {
				logf("[WARN] manage: fetch position failed: %v", err)
				continue
			}
			if !pos.IsOpen() {
				logf("[SYNC] %s position no longer open (manual close or already closed) - resuming hunt.", cfg.Symbol)
				return
			}

			price, err := client.GetLastPrice(cfg.Category, cfg.Symbol)
			if err != nil {
				logf("[WARN] manage: fetch last price failed: %v", err)
				continue
			}
			pnl := pnlPercent(side, entryPrice, price)

			// --- Move SL to break-even EXACTLY ONCE. After this, the SL
			// is never touched again for the rest of the trade - per spec,
			// this prevents it from being "hunted" by a trailing stop that
			// keeps creeping closer to price. ---
			if !beArmed && pnl >= cfg.BETriggerPct {
				newSL := roundToTick(breakEvenSLPrice(side, entryPrice, cfg), instrument.PriceTick)
				newSLStr := formatFloat(newSL, priceDecimals)
				if err := client.SetStopLoss(cfg.Category, cfg.Symbol, 0, newSLStr); err != nil {
					logf("[WARN] %s: failed to lock SL at break-even (%v); will retry next tick.", cfg.Symbol, err)
				} else {
					beArmed = true
					slPrice = newSL
					logf("[BE-LOCK] %s +%.2f%% reached. SL locked at %.8f (break-even + fee buffer) - "+
						"THIS IS PERMANENT, SL will not move again for this trade.", cfg.Symbol, pnl, slPrice)
				}
			}

			// --- Periodically refresh the "hold for pump" target ---
			if tickCount%htfRefreshEvery == 0 {
				if t, ok := refreshHTFTarget(client, cfg, side, price); ok {
					if !htfOk || t != htfTarget {
						logf("[TARGET] %s hold-for-pump target updated: %.8f -> %.8f", cfg.Symbol, htfTarget, t)
					}
					htfTarget, htfOk = t, true
				}
			}

			// --- Hold-for-pump target reached: take profit here ---
			if htfOk {
				hit := (side == "long" && price >= htfTarget) || (side == "short" && price <= htfTarget)
				if hit {
					logf("[TARGET HIT] %s price=%.8f target=%.8f (+%.2f%% profit) - taking profit at HTF level.",
						cfg.Symbol, price, htfTarget, pnl)
					closePositionNow(client, cfg, instrument, side, "HTF_TARGET_REACHED")
					return
				}
			}
		}
	}
}
