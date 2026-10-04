package main

// ============================================================================
// ORDER BOOK LOGIC - two very different jobs:
//
//   1. CheckEntryOB - a LOOSE confirmation used once, at entry time, on top
//      5 levels. Just needs to mildly favor the trade direction (default
//      1.2x). This is one of the "2 of 4" loose checks for entry.
//
//   2. RunReversalWatcher - "REVERSAL GAGAH": a dedicated goroutine that
//      runs for the ENTIRE lifetime of an open, profitable position,
//      polling the order book every ReversalCheckIntervalMs (default
//      300ms) on the top 15 levels, watching for THREE independent danger
//      signals. If ANY of them fires while the position is in profit
//     beyond ReversalProfitTriggerPct, it immediately signals the manage
//      loop to close 100% of the position at market. This is the system
//      that lets the bot "hold for pump" without getting run over by a
//      sudden dump - it is deliberately the most aggressively-tuned,
//      robust part of the whole bot.
// ============================================================================

import (
	"context"
	"time"
)

// sumVolume adds up the size field across a slice of order book levels.
func sumVolume(levels []OrderBookLevel) float64 {
	sum := 0.0
	for _, l := range levels {
		sum += l.Size
	}
	return sum
}

// CheckEntryOB is the loose order-book entry confirmation (top OBEntryDepth
// levels, default 5). Fails OPEN (returns true) on a fetch error so a
// transient API hiccup never silently blocks an otherwise-good signal -
// the hard gates (candle + RSI) and the other loose checks still apply.
func CheckEntryOB(client *BybitClient, cfg Config, side string) bool {
	ob, err := client.GetOrderBook(cfg.Category, cfg.Symbol, cfg.OBEntryDepth)
	if err != nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return true
	}
	bidVol, askVol := sumVolume(ob.Bids), sumVolume(ob.Asks)
	if bidVol <= 0 || askVol <= 0 {
		return true
	}
	if side == "long" {
		return bidVol >= askVol*cfg.OBEntryRatio
	}
	return askVol >= bidVol*cfg.OBEntryRatio
}

// obSnapshot is one tick of order-book state, kept in a short rolling
// history so the CVD-divergence check can compare "now" against "a few
// ticks ago" without needing a database or external time-series store.
type obSnapshot struct {
	midPrice float64
	bidVol   float64
	askVol   float64
}

// ReversalResult describes what the watcher found, for logging.
type ReversalResult struct {
	Reason string // "VOLUME_FLIP" | "BANDAR_WALL" | "CVD_DIVERGENCE"
	Detail string
}

// RunReversalWatcher is "REVERSAL GAGAH" - launched as its own goroutine
// right after a position opens (see main.go). It runs until either:
//   - it detects a reversal signal while the position is sufficiently in
//     profit, in which case it sends a ReversalResult on resultCh and
//     returns, OR
//   - ctx is canceled by the manage loop (because the position was closed
//     some other way - TP, SL, or manually by the user), in which case it
//    returns silently without sending anything.
//
// pnlFn lets the watcher ask the manage loop for the current live PnL
// percentage without duplicating price-fetching logic or risking a stale
// entry price capture.
func RunReversalWatcher(ctx context.Context, client *BybitClient, cfg Config, side string, pnlFn func(midPrice float64) float64, resultCh chan<- ReversalResult) {
	interval := time.Duration(cfg.ReversalCheckIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	history := make([]obSnapshot, 0, cfg.CVDLookbackTicks+1)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ob, err := client.GetOrderBook(cfg.Category, cfg.Symbol, cfg.OBReversalDepth)
			if err != nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
				continue // transient fetch failure - just try again next tick
			}

			bidVol := sumVolume(ob.Bids)
			askVol := sumVolume(ob.Asks)
			bestBid := ob.Bids[0].Price
			bestAsk := ob.Asks[0].Price
			midPrice := (bestBid + bestAsk) / 2

			snap := obSnapshot{midPrice: midPrice, bidVol: bidVol, askVol: askVol}
			history = append(history, snap)
			if len(history) > cfg.CVDLookbackTicks+1 {
				history = history[len(history)-(cfg.CVDLookbackTicks+1):]
			}

			pnlPct := pnlFn(midPrice)
			if pnlPct < cfg.ReversalProfitTriggerPct {
				// Not profitable enough yet for the bail-out to be allowed
				// to fire - the native hard SL / BE-lock still protects
				// the position regardless, this watcher just isn't armed
				// to react on its own below the profit threshold.
				continue
			}

			if triggered, result := CheckReversalGagah(ob, history, side, cfg); triggered {
				resultCh <- result
				return
			}
		}
	}
}

// CheckReversalGagah runs all THREE reversal signals once against the
// current order book (+ a short history for CVD) and reports whether ANY
// of them fired, plus which one. This is the literal "gagah" check -
// RunReversalWatcher above just calls this every tick inside its own
// goroutine loop, so the detection logic itself is independently testable.
//
//   a) Volume Flip   - opposite-side volume dominates by OBFlipRatio (3.0x)
//   b) Bandar Wall   - a single order > OBWallRatio (5x) the average size,
//                      sitting within WallDistancePct of price
//   c) CVD Divergence - price moving favorably while the supporting side's
//                       volume drains fast (fake pump/dump)
func CheckReversalGagah(ob OrderBook, history []obSnapshot, side string, cfg Config) (bool, ReversalResult) {
	bidVol := sumVolume(ob.Bids)
	askVol := sumVolume(ob.Asks)

	// --- (a) VOLUME FLIP - whale wall on the opposite side ---
	if side == "long" && bidVol > 0 && askVol > bidVol*cfg.OBFlipRatio {
		return true, ReversalResult{
			Reason: "VOLUME_FLIP",
			Detail: "BANDAR REVERSAL DETECTED, SECURING PROFIT (ask volume whale wall)",
		}
	}
	if side == "short" && askVol > 0 && bidVol > askVol*cfg.OBFlipRatio {
		return true, ReversalResult{
			Reason: "VOLUME_FLIP",
			Detail: "BANDAR REVERSAL DETECTED, SECURING PROFIT (bid volume whale wall)",
		}
	}

	// --- (b) LARGE WALL DETECTION ---
	bestBid, bestAsk := 0.0, 0.0
	if len(ob.Bids) > 0 {
		bestBid = ob.Bids[0].Price
	}
	if len(ob.Asks) > 0 {
		bestAsk = ob.Asks[0].Price
	}
	midPrice := (bestBid + bestAsk) / 2
	if detectWall(ob, side, midPrice, cfg) {
		return true, ReversalResult{
			Reason: "BANDAR_WALL",
			Detail: "BANDAR REVERSAL DETECTED, SECURING PROFIT (large wall near price)",
		}
	}

	// --- (c) CVD DIVERGENCE - needs at least CVDLookbackTicks of history ---
	if len(history) > cfg.CVDLookbackTicks {
		older := history[len(history)-1-cfg.CVDLookbackTicks]
		newer := history[len(history)-1]
		if cvdDivergence(older, newer, side, cfg) {
			return true, ReversalResult{
				Reason: "CVD_DIVERGENCE",
				Detail: "BANDAR REVERSAL DETECTED, SECURING PROFIT (CVD divergence - fake pump/dump)",
			}
		}
	}

	return false, ReversalResult{}
}

// detectWall checks for a single order size far above the average level
// size on the DANGEROUS side (asks for a long, bids for a short), sitting
// within WallDistancePct of the current price - a classic "bandar wall"
// used to cap a move right before reversing it.
func detectWall(ob OrderBook, side string, midPrice float64, cfg Config) bool {
	var levels []OrderBookLevel
	if side == "long" {
		levels = ob.Asks // a sell wall threatens a long
	} else {
		levels = ob.Bids // a buy wall threatens a short
	}
	if len(levels) == 0 {
		return false
	}

	sum := 0.0
	for _, l := range levels {
		sum += l.Size
	}
	avgSize := sum / float64(len(levels))
	if avgSize <= 0 {
		return false
	}

	maxDistance := midPrice * cfg.WallDistancePct
	for _, l := range levels {
		if absF(l.Price-midPrice) > maxDistance {
			continue
		}
		if l.Size > avgSize*cfg.OBWallRatio {
			return true
		}
	}
	return false
}

// cvdDivergence compares an older and a newer order-book snapshot to spot
// a "fake pump" (long risk: price up, bid volume draining) or "fake dump"
// (short risk: price down, ask volume draining).
func cvdDivergence(older, newer obSnapshot, side string, cfg Config) bool {
	if side == "long" {
		priceRising := newer.midPrice > older.midPrice
		if !priceRising || older.bidVol <= 0 {
			return false
		}
		drop := (older.bidVol - newer.bidVol) / older.bidVol
		return drop >= cfg.CVDDropPct
	}

	priceFalling := newer.midPrice < older.midPrice
	if !priceFalling || older.askVol <= 0 {
		return false
	}
	drop := (older.askVol - newer.askVol) / older.askVol
	return drop >= cfg.CVDDropPct
}
