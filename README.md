# 🐉 Bandar Reversal Bot — Go / Bybit v5 REST

M1 "candle first, loose but smart" scalper for **1000BONKUSDT** linear
perpetual on Bybit, isolated margin, 30x leverage. Pure Go, raw Bybit v5
REST API, zero third-party dependencies.

---

## ⚠️ RISK DISCLAIMER

**100% of free balance as margin every trade** (`POSITION_PCT=1.0`),
compounding automatically, at **30x leverage**. Entry filters are
deliberately loose. SL is fixed at 1.5%, moves to break-even exactly once,
then **never moves again** for the rest of the trade — it does not trail
further, by design (see "Why SL doesn't trail" below). This is an
aggressive configuration. Only trade funds you're fully prepared to lose.
Not financial advice. No warranty.

**This build was written and manually reviewed for correctness (brace/paren
balance checked, every function cross-referenced) but was NOT compiled in
a live Go toolchain during generation — no Go compiler was available in
the authoring environment.** Run `go build` yourself and fix any compiler
errors, then test on **Bybit testnet** before using real funds.

---

## 🧠 How it works

### Entry — candle first, then RSI, then 2-of-4 loose checks

1. **Candle first (hard gate):** price must sit inside the support zone
   (long) or resistance zone (short), computed from the last 20 closed
   candles with a tolerance of `ZONE_ATR × ATR`. Price in the middle of
   the range → rejected outright, no exceptions.
2. **RSI second (hard gate):** RSI(14) must fall in `39–45` for a long or
   `69–100` for a short. A candle at support with RSI 60 is rejected here.
3. **Loose confirmations (need 2 of 4):** EMA tolerance (`EMA_TOL`, very
   loose at 0.3%), wick ratio (`MIN_WICK`, loose at 0.05), volume ratio
   (`MIN_VOL`, loose at 0.1x average), and order-book pressure
   (`OB_ENTRY_RATIO`, loose at 1.2x, top 5 levels). Any 2 of these 4 passing
   is enough to fire a market entry.

### Profit management — "reversal gagah, SL diam"

- **Fixed SL, no trailing up:** initial stop is 1.5% from entry. Once
  profit hits 0.6%, the SL moves to break-even + 0.05% (covers fees) —
  and then **never moves again** for the rest of that trade. This is
  intentional: a trailing stop that keeps creeping toward price can get
  "hunted" by a brief wick before the real move continues. Locking it at
  BE once removes that risk entirely while still guaranteeing the trade
  can't turn into a real loss.
- **Hold for pump, no fixed TP:** instead of closing at a small fixed
  target, the bot finds the next resistance (long) or support (short) on
  a higher timeframe (`HTF_INTERVAL`, default 15m) and holds until price
  reaches it — re-checked periodically so the target adapts as new HTF
  candles close.
- **REVERSAL GAGAH — the core protection:** a dedicated goroutine runs for
  the entire time a position is open, polling the top 15 order-book levels
  every 300ms, watching for three independent danger signals:
  - **Volume Flip:** opposite-side volume ≥ 3x the supporting side (a
    whale wall forming against you).
  - **Bandar Wall:** a single order ≥ 5x the average level size, sitting
    within 0.3% of price on the dangerous side.
  - **CVD Divergence:** price moving favorably while the volume that's
    supposed to be driving that move is draining fast — a classic fake
    pump/dump tell.

  If **any** of these fires while the position is more than 0.5% in
  profit, the bot closes 100% of the position **immediately** at market
  and logs `BANDAR REVERSAL DETECTED, SECURING PROFIT`.

### Position sizing — compounding

Every entry fetches the **live** free USDT balance and uses 100% of it as
margin at 30x leverage. A win grows the balance, so the next trade is
automatically larger — e.g. balance 10 USDT, profit 2 USDT → next trade
sizes off 12 USDT. No manual intervention needed.

---

## 📦 Files

| File | Purpose |
|---|---|
| `main.go` | Main loop (300ms scan), position sizing/compounding, full trade management (BE-lock, HTF target, reversal integration) |
| `config.go` | `.env` / environment variable loading, all tunables from the spec |
| `strategy.go` | `DetectSetup` — candle-first + RSI + loose EMA/wick/volume checks; ATR/EMA/RSI math; `FindHTFTarget` |
| `orderbook.go` | `CheckEntryOB` (loose entry confirmation) + `CheckReversalGagah` and `RunReversalWatcher` (the dedicated reversal-detection goroutine) |
| `bybit.go` | Full Bybit v5 REST client: klines, orderbook, tickers, instrument info, balance, position, isolated leverage, market orders, native stop-loss |
| `go.mod` | Module definition — zero third-party dependencies |
| `.env.example` | Config template |

---

## 🖥️ Build & run

```bash
# Install Go if needed (check https://go.dev/dl/ for current version)
curl -LO https://go.dev/dl/go1.22.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.22.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && source ~/.bashrc

git clone https://github.com/YOUR_USERNAME/YOUR_REPO.git bandar-reversal-bot
cd bandar-reversal-bot
go build -o bandar-reversal-bot .

cp .env.example .env
nano .env   # fill in your API key/secret

./bandar-reversal-bot
```

Run 24/7 with `systemd`:

```ini
# /etc/systemd/system/bandar-reversal-bot.service
[Unit]
Description=Bandar Reversal Bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/home/YOUR_USERNAME/bandar-reversal-bot
ExecStart=/home/YOUR_USERNAME/bandar-reversal-bot/bandar-reversal-bot
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable bandar-reversal-bot
sudo systemctl start bandar-reversal-bot
journalctl -u bandar-reversal-bot -f
```

---

## ⚙️ Key config (see `.env.example` for the full list)

| Variable | Default | Meaning |
|---|---|---|
| `SYMBOL` | `1000BONKUSDT` | Raw Bybit v5 symbol — no slashes/colons |
| `LEVERAGE` / `ISOLATED` | `30` / `true` | Clipped to exchange max automatically |
| `POSITION_PCT` | `1.0` | 100% of free balance per trade |
| `RSI_LONG_MIN/MAX`, `RSI_SHORT_MIN/MAX` | `39-45`, `69-100` | RSI gate ranges |
| `SL_PCT` | `1.5` | Initial stop-loss, percent |
| `BE_TRIGGER` / `BE_OFFSET_PCT` | `0.6` / `0.05` | Profit % that locks SL to break-even, and the fee buffer above/below entry |
| `HTF_INTERVAL` / `HTF_LOOKBACK` | `15` / `50` | Higher timeframe + lookback for the adaptive "hold for pump" target |
| `OB_FLIP_RATIO` / `OB_WALL_RATIO` / `CVD_DROP_PCT` | `3.0` / `5.0` / `0.3` | Reversal Gagah thresholds |
| `REVERSAL_PROFIT_TRIGGER_PCT` | `0.5` | Minimum profit before the reversal watcher is allowed to close |

---

## 🛑 Final word

Reversal detection is the most heavily-tuned part of this bot per your
spec — three independent signals, checked every 300ms, for as long as a
position is open. Test thoroughly on testnet first, especially the native
stop-loss and isolated-margin calls, since those are the parts most
sensitive to exact Bybit account behavior. 🐉
