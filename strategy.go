package main

// ============================================================================
// STRATEGY - "CANDLE FIRST, LOOSE BUT SMART"
//
// Gate order, exactly per spec:
//   1. CANDLE FIRST (hard gate): price must be inside the support zone
//      (for a long candidate) or resistance zone (for a short candidate),
//      both computed from the last Lookback CLOSED candles with a zone
//      tolerance of ZoneATR * ATR. If price sits in the middle of the
//      range (neither zone), the whole signal is rejected - no exceptions.
//   2. RSI SECOND (hard gate): once a side is chosen by step 1, RSI(14)
//      must fall inside that side's configured range (39-45 for long,
//      69-100 for short). A support bounce with RSI 60 is rejected here,
//      even though the candle structure looked fine.
//   3. LOOSE CONFIRMATIONS (soft, need 2 of 4): EMA tolerance check, wick
//      ratio, volume ratio - all three computed here - PLUS the order-book
//      entry check from orderbook.go, combined by the caller (main.go)
//      since it requires a live API call this function intentionally
//      doesn't make (keeps DetectSetup pure/testable on candle data alone).
// ============================================================================

import "math"

// Signal describes one actionable setup that has ALREADY passed the
// candle-zone and RSI hard gates. LooseCount is how many of the three
// candle-only loose checks (EMA/wick/volume) passed - main.go adds the
// order-book check on top of this to reach the final "2 of 4" decision.
type Signal struct {
	Side       string // "long" or "short"
	Structure  float64 // the support/resistance level that triggered this
	ATR        float64
	RSI        float64
	LooseCount int // 0-3, from EMA/wick/volume (order-book check added separately)
}

// ATR computes the Average True Range over the last `period` candles.
func ATR(klines []Kline, period int) (float64, bool) {
	if len(klines) < period+1 {
		return 0, false
	}
	start := len(klines) - period
	sum := 0.0
	for i := start; i < len(klines); i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close
		tr := max3(high-low, absF(high-prevClose), absF(low-prevClose))
		sum += tr
	}
	return sum / float64(period), true
}

// EMASeries computes the full EMA series over the given klines' closes.
func EMASeries(klines []Kline, period int) []float64 {
	if len(klines) < period {
		return nil
	}
	k := 2.0 / (float64(period) + 1.0)
	vals := make([]float64, len(klines))
	vals[0] = klines[0].Close
	for i := 1; i < len(klines); i++ {
		vals[i] = klines[i].Close*k + vals[i-1]*(1-k)
	}
	return vals
}

// RSI computes a standard Wilder-smoothed RSI over the given closes,
// returning the value as of the LAST element. Returns false if there
// isn't enough history (need at least period+1 closes).
func RSI(closes []float64, period int) (float64, bool) {
	if len(closes) < period+1 {
		return 0, false
	}
	gains, losses := 0.0, 0.0
	for i := 1; i <= period; i++ {
		diff := closes[i] - closes[i-1]
		if diff > 0 {
			gains += diff
		} else {
			losses += -diff
		}
	}
	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	for i := period + 1; i < len(closes); i++ {
		diff := closes[i] - closes[i-1]
		var gain, loss float64
		if diff > 0 {
			gain = diff
		} else {
			loss = -diff
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
	}

	if avgLoss == 0 {
		return 100, true
	}
	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))
	return rsi, true
}

func closesOf(klines []Kline) []float64 {
	out := make([]float64, len(klines))
	for i, k := range klines {
		out[i] = k.Close
	}
	return out
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// DetectSetup runs the full candle-first + RSI pipeline and returns a
// Signal (with LooseCount from the 3 candle-only confirmations) or nil if
// either hard gate fails. `klines` should include the still-forming candle
// as the last element (as returned by GetKlines) - it is excluded
// internally so only closed data drives the decision.
func DetectSetup(klines []Kline, cfg Config) *Signal {
	minNeeded := cfg.Lookback + cfg.ATRPeriod + cfg.RSIPeriod + 5
	if len(klines) < minNeeded {
		return nil
	}
	closed := klines[:len(klines)-1]

	atr, ok := ATR(closed, cfg.ATRPeriod)
	if !ok || atr <= 0 {
		return nil
	}

	last := closed[len(closed)-1]
	window := closed[len(closed)-cfg.Lookback:]

	support := window[0].Low
	resistance := window[0].High
	for _, k := range window {
		if k.Low < support {
			support = k.Low
		}
		if k.High > resistance {
			resistance = k.High
		}
	}

	price := last.Close
	zone := atr * cfg.ZoneATR
	nearSupport := absF(price-support) <= zone
	nearResistance := absF(resistance-price) <= zone

	// --- GATE 1: CANDLE FIRST ---
	var side string
	var structure float64
	switch {
	case nearSupport && !nearResistance:
		side, structure = "long", support
	case nearResistance && !nearSupport:
		side, structure = "short", resistance
	case nearSupport && nearResistance:
		// Tight range where both zones overlap - pick whichever is closer.
		if absF(price-support) <= absF(resistance-price) {
			side, structure = "long", support
		} else {
			side, structure = "short", resistance
		}
	default:
		// Price is in the middle of the range - REJECT, no entry.
		return nil
	}

	// --- GATE 2: RSI ---
	rsi, ok := RSI(closesOf(closed), cfg.RSIPeriod)
	if !ok {
		return nil
	}
	if side == "long" {
		if rsi < cfg.RSILongMin || rsi > cfg.RSILongMax {
			return nil
		}
	} else {
		if rsi < cfg.RSIShortMin || rsi > cfg.RSIShortMax {
			return nil
		}
	}

	// --- LOOSE CONFIRMATIONS (candle-only subset: EMA, wick, volume) ---
	looseCount := 0

	if emaVals := EMASeries(closed, cfg.EMAPeriod); emaVals != nil {
		currentEMA := emaVals[len(emaVals)-1]
		var emaPass bool
		if side == "long" {
			emaPass = price > currentEMA*(1-cfg.EMATol)
		} else {
			emaPass = price < currentEMA*(1+cfg.EMATol)
		}
		if emaPass {
			looseCount++
		}
	}

	rng := math.Max(last.High-last.Low, 1e-12)
	var wickRatio float64
	if side == "long" {
		wickRatio = (math.Min(last.Open, last.Close) - last.Low) / rng
	} else {
		wickRatio = (last.High - math.Max(last.Open, last.Close)) / rng
	}
	if wickRatio >= cfg.MinWick {
		looseCount++
	}

	volLookback := 20
	if len(closed) < volLookback {
		volLookback = len(closed)
	}
	volSum := 0.0
	for _, k := range closed[len(closed)-volLookback:] {
		volSum += k.Volume
	}
	avgVol := volSum / float64(volLookback)
	if avgVol > 0 && last.Volume > avgVol*cfg.MinVol {
		looseCount++
	}

	return &Signal{Side: side, Structure: structure, ATR: atr, RSI: rsi, LooseCount: looseCount}
}

// FindHTFTarget looks at higher-timeframe candles to find the next
// resistance (for a long) or support (for a short) ABOVE/BELOW the current
// price - this is the "hold for pump" adaptive target used instead of a
// fixed take-profit. Returns ok=false if no higher-timeframe data is
// available yet, in which case the caller should fall back to a sane
// default (e.g. keep holding and recheck next cycle).
func FindHTFTarget(htfKlines []Kline, side string, currentPrice float64) (float64, bool) {
	if len(htfKlines) == 0 {
		return 0, false
	}
	if side == "long" {
		target := 0.0
		found := false
		for _, k := range htfKlines {
			if k.High > currentPrice && (!found || k.High < target) {
				// nearest swing high still ABOVE current price
				target = k.High
				found = true
			}
		}
		if !found {
			// price already above all recent HTF highs - use the highest
			// available high plus nothing extra; caller can extend further
			// using ATR if desired.
			for _, k := range htfKlines {
				if k.High > target {
					target = k.High
				}
			}
			found = target > currentPrice
		}
		return target, found
	}

	// short
	target := 0.0
	found := false
	for _, k := range htfKlines {
		if k.Low < currentPrice && (!found || k.Low > target) {
			target = k.Low
			found = true
		}
	}
	if !found {
		for i, k := range htfKlines {
			if i == 0 || k.Low < target {
				target = k.Low
			}
		}
		found = target < currentPrice && target > 0
	}
	return target, found
}
