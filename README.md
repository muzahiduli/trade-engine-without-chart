# Trade Engine — Without Chart

A charterless trading engine: **all logic lives in Go**, the NinjaTrader chart
hosts the *draggable lines* (zero-logic), and the web side is a **control panel
only** (no tradingview chart, no overlay).

## Architecture

```
┌────────────────────────── NT8 side (2 components, WS clients → :8080) ──────────────────────────┐
│                                                                                                 │
│  TradeEngineStrategy.cs (UNCHANGED)      TradeEngineLineHost.cs (NEW, ZERO LOGIC)               │
│  • executes orders                       • draws Entry / SL / TP lines (Draw.Line)              │
│  • streams market/bars/positions         • user drags any line → NT8 fires                      │
│  • broadcasts POSITION/ORDERS/EXEC       │   OnChartObjectModified → pushes UPDATE_PRICES       │
└──────────────────────┬──────────────────────────────────┬──────────────────────────────────────┘
                       │ events                     drags (UPDATE_PRICES)
                       ▼                                  ▼
┌──────────────────────────────  Go Trade Engine (hub)  ──────────────────────────────────────────┐
│ ALL LOGIC: risk sizing, RR, entry model, brackets (ActiveSL/ActiveTP), protection, hotkeys,    │
│ audit — engine is AUTHORITATIVE; it normalizes every price intent, validates it, and executes  │
│ through the gateway as CHANGE_ORDER / SUBMIT_ORDER / FLATTEN.                                   │
└──────────────────────────────────────────┬──────────────────────────────────────────────────────┘
                                           ▼
                          Web control panel (NO chart, NO overlay):
                          direction/model, risk cash, HUD, execute/flatten, settings, hotkeys
```

### Why this is cleaner than the overlay build

- The lines live **natively on the NinjaTrader chart**, where you actually
  trade. Dragging is first-class NT8 behavior (no canvas↔price mapping bugs, no
  overlay flicker, no badge rendering).
- The engine stays the **single authority**: every line drag is just a *price
  intent* (`UPDATE_PRICES`) that the hub normalizes, validates, and then
  reflects to NT8 as real order changes (same pipeline the web panel used).
- Removing the web chart removes an entire class of UI bugs (the ones this repo
  was created to avoid).

## Components

| Component | Path | Logic |
|---|---|---|
| Go hub + engine | `internal/hub`, `internal/risk` | ALL business logic |
| Deterministic replay verification | `internal/replay` (+ `testdata/playback_fixture.jsonl`) | engine==broker, both directions, on every `go test ./...` |
| Web control panel | `web/` | HUD, risk inputs, buttons, settings, hotkeys — no chart |
| NT8 gateway (unchanged) | `ninjatrader/Strategies/TradeEngineStrategy.cs` | execution + market/bars/position stream |
| NT8 line host (zero logic) | `ninjatrader/Indicators/TradeEngineLineHost.cs` | hosts draggable Entry/SL/TP lines, pushes drags, mirrors engine state |

## Running

```powershell
# 1. Engine (Go hub):
go run ./cmd/server/server.go -port 8080

# 2. Web control panel: in the browser, open
#    http://localhost:8080/   (served by the hub)

# 3. NinjaTrader: open a chart (e.g. MNQ 09-26),
#    - add the TradeEngineLineHost indicator (drag the 3 lines = set SL/TP/entry intent)
#    - run ONE instance of the TradeEngineStrategy gateway (WebSocketUrl :8080)

# 4. Verify deterministically (no NT8 needed):
go test ./...
```

## NT8 compile notes

- Copy the two files into `C:\Users\muzah\Documents\NinjaTrader 8\bin\Custom\`
  (`Strategies\TradeEngineStrategy.cs`, `Indicators\TradeEngineLineHost.cs`)
- Compile in NinjaScript Editor (F5). Confirm the DLL mtime updates —
  **never assume the recompile happened**:
  ```powershell
  (Get-Item "C:\Users\muzah\Documents\NinjaTrader 8\bin\Custom\NinjaTrader.Custom.dll").LastWriteTime
  ```
- `TradeEngineLineHost` never calls `SendOrder()` — structurally it cannot
  execute; safest possible NT8 component.

## Deterministic verification (preserved from the parent repo)

`internal/replay` feeds a recorded playback session
(`testdata/playback_fixture.jsonl`) into an in-process hub and asserts:
- Engine ⇒ NT8: the orders the hub forwards match the recorded engine orders
  (entry, bracket batch, flatten).
- NT8 ⇒ engine: position + working orders converge to recorded broker truth.

Re-record only when the trading flow changes (tool lives in the parent repo):
`go run ./cmd/verify-playback record -hub localhost:8080 -fixture ...`.