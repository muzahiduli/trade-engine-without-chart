package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"trade-engine-without-chart/internal/logging"
	"trade-engine-without-chart/internal/risk"
)

// HandleHotkey dispatches a keyboard-driven command with strict server-side gating.
func (h *Hub) HandleHotkey(action string) {
	h.Mu.RLock()
	hotkeysEnabled := h.State.EnableHotkeys
	tradingDisabled := h.State.TradingDisabled
	hotkeysArmed := h.State.HotkeysArmed
	posQty := h.State.Position.Quantity
	hasCommitted := h.hasCommittedEntry()
	scaleOutPct := h.State.ScaleOutPercent
	autoBEOffset := h.State.AutoBEOffsetTicks
	h.Mu.RUnlock()

	if !hotkeysEnabled && action != "KILL_SWITCH" {
		h.flashHotkeyStatus(fmt.Sprintf("Hotkey %s ignored: hotkeys disabled", action))
		return
	}
	if tradingDisabled && action != "KILL_SWITCH" {
		h.flashHotkeyStatus("Trading disabled by Kill-Switch")
		return
	}

	// Execution actions require armed hotkeys
	isExecAction := (action == "INSTANT_ENTRY" || action == "BREAKOUT_ENTRY")
	if isExecAction && !hotkeysArmed {
		h.flashHotkeyStatus(fmt.Sprintf("Hotkey %s blocked: execution hotkeys not armed", action))
		return
	}

	// Action Eligibility Rules:
	// Flat-only actions:
	if action == "SWAP_DIRECTION" && hasCommitted {
		h.flashHotkeyStatus("Swap direction blocked: trade is active or entry is working")
		return
	}
	// In-position-only actions:
	inPosActions := map[string]bool{
		"TRAIL_STOP": true, "SCALE_OUT": true, "BREAKEVEN": true, "BREAKEVEN_PLUS": true,
		"CLOSE_25": true, "CLOSE_50": true, "CLOSE_RUNNER": true,
	}
	if inPosActions[action] && posQty <= 0 {
		h.flashHotkeyStatus(fmt.Sprintf("Hotkey %s ignored: no active position", action))
		return
	}

	switch action {
	case "INSTANT_ENTRY":
		h.hotkeyInstantEntry()
	case "BREAKOUT_ENTRY":
		h.hotkeyBreakoutEntry()
	case "TRAIL_STOP":
		h.hotkeyTrailStop()
	case "SWAP_DIRECTION":
		h.hotkeySwapDirection()
	case "SCALE_OUT":
		h.hotkeyScaleOut(scaleOutPct)
	case "CLOSE_25":
		h.hotkeyScaleOut(25.0)
	case "CLOSE_50":
		h.hotkeyScaleOut(50.0)
	case "CLOSE_RUNNER":
		h.hotkeyScaleOut(100.0)
	case "BREAKEVEN":
		h.hotkeyBreakeven(0)
	case "BREAKEVEN_PLUS":
		offset := autoBEOffset
		if offset <= 0 {
			offset = 2
		}
		h.hotkeyBreakeven(offset)
	case "CANCEL_ENTRY":
		h.cancelWorkingEntryOnly()
	case "CANCEL_ORDERS":
		h.cancelOrdersOnly()
	case "FLATTEN":
		h.hotkeyFlatten()
	case "STOP_AT_5M":
		h.hotkeyStopAt5mCandle()
	case "KILL_SWITCH":
		h.emergencyKillSwitch()
	default:
		log.Printf("Hotkey: unknown action %q ignored", action)
	}
}

// flashHotkeyStatus publishes a transient status/log line to the web clients.
func (h *Hub) flashHotkeyStatus(text string) {
	log.Printf("Hotkey: %s", text)
	h.Mu.Lock()
	h.State.LastLog = text
	h.Mu.Unlock()
	go h.BroadcastState()
}

// quoteBandTicks is how far the cached bid/ask may plausibly deviate from the
// live last before the quote is treated as STALE. Market Replay can leave
// bid/ask frozen far behind the traded/last price (a stale ask produced buy
// stops BELOW NinjaTrader's real market â†’ "buy stop cannot be placed below
// the market"). 25 ticks â‰ˆ 6.25 pts on NQ; a normal spread is 1-2 ticks.
const quoteBandTicks = 25.0

// tickReference returns the most reliable live price: the last print from live
// MARKET_DATA ticks (works in Market Replay), else the last bar close, else
// the cached quote as a last resort (fresh boot before any tick/bar). Caller
// must hold h.Mu (read or write).
func (h *Hub) tickReference() float64 {
	if h.State.CurrentMarketPrice > 0 {
		return h.State.CurrentMarketPrice
	}
	if h.State.LastPrice > 0 {
		return h.State.LastPrice
	}
	if len(h.BarCache) > 0 {
		return h.BarCache[len(h.BarCache)-1].Close
	}
	if h.State.CurrentAsk > 0 {
		return h.State.CurrentAsk
	}
	if h.State.CurrentBid > 0 {
		return h.State.CurrentBid
	}
	return 0
}

// usableAsk returns the freshest LONG-quote: the cached Ask when it agrees with
// the live last within the plausibility band, otherwise the live last (stale
// Market Replay quotes are ignored). Caller must hold h.Mu (read or write).
func (h *Hub) usableAsk() float64 {
	ask := h.State.CurrentAsk
	last := h.tickReference()
	if ask <= 0 {
		return last
	}
	if last > 0 {
		tick := h.State.TickSize
		if tick <= 0 {
			tick = 0.25
		}
		if math.Abs(ask-last) > quoteBandTicks*tick {
			return last
		}
	}
	return ask
}

// usableBid is the SHORT-side counterpart of usableAsk.
func (h *Hub) usableBid() float64 {
	bid := h.State.CurrentBid
	last := h.tickReference()
	if bid <= 0 {
		return last
	}
	if last > 0 {
		tick := h.State.TickSize
		if tick <= 0 {
			tick = 0.25
		}
		if math.Abs(bid-last) > quoteBandTicks*tick {
			return last
		}
	}
	return bid
}

// hotkeySwapDirection toggles the selected Long/Short direction and mirrors the
// stop/target to the opposite side of the entry price, keeping geometry symmetric.
// It is strictly rejected if a position or working entry exists.
func (h *Hub) hotkeySwapDirection() {
	h.Mu.Lock()
	if h.hasCommittedEntry() {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Swap direction blocked: trade is active or entry is working")
		return
	}
	h.State.IsLong = !h.State.IsLong
	if h.State.EntryPrice > 0 {
		h.State.StopPrice = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.StopPrice, h.State.TickSize)
		h.State.TargetPrice = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.TargetPrice, h.State.TickSize)
		for i := range h.State.TargetExits {
			h.State.TargetExits[i].Price = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.TargetExits[i].Price, h.State.TickSize)
		}
	}
	risk.RecalculateState(h.State)
	dir := "SHORT"
	if h.State.IsLong {
		dir = "LONG"
	}
	h.Mu.Unlock()

	go h.BroadcastState()
	h.flashHotkeyStatus(fmt.Sprintf("â‡„ Direction â†’ %s", dir))
}

// hotkeyInstantEntry fires an INSTANT entry in the CURRENT direction as a
// LIMIT order: LONG buys at Ask + offset ticks, SHORT sells at Bid - offset
// ticks (offset = InstantEntryOffsetTicks, bounded by MaxEntrySlippageTicks).
func (h *Hub) hotkeyInstantEntry() {
	h.Mu.Lock()
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	mode := h.State.InstantEntryMode
	if mode == "" {
		mode = "AskBid"
	}
	ref := h.tickReference()
	if ref <= 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: no live market reference")
		return
	}
	goLong := h.State.IsLong
	dir := "BUY"
	if !goLong {
		dir = "SELL_SHORT"
	}

	// MARKET mode: submit a market order at the live market reference. No
	// offset pricing; fills at whatever the broker does.
	if mode == "Market" {
		entry := ref
		h.State.EntryPrice = risk.RoundToTickSize(entry, tick)
		risk.RecalculateState(h.State)
		h.Mu.Unlock()
		h.submitEntryMarket("GoEntry")
		h.flashHotkeyStatus(fmt.Sprintf("Instant %s MARKET entry @ %.2f", dir, entry))
		return
	}

	// AskBid mode (default): marketable LIMIT at Ask+N ticks (long) / Bid-N
	// ticks (short) — same as the original behavior.
	offsetTicks := h.State.InstantEntryOffsetTicks
	if h.State.MaxEntrySlippageTicks > 0 && offsetTicks > h.State.MaxEntrySlippageTicks {
		offsetTicks = h.State.MaxEntrySlippageTicks
	}
	offset := float64(offsetTicks) * tick
	if offset < 0 {
		offset = 0
	}
	// Use a quote only when it is plausible vs the live last (see usableAsk);
	// otherwise fall back to the live last price.
	if goLong {
		h.State.EntryPrice = risk.RoundToTickSize(h.usableAsk()+offset, tick)
	} else {
		h.State.EntryPrice = risk.RoundToTickSize(h.usableBid()-offset, tick)
	}
	risk.RecalculateState(h.State)
	entry := h.State.EntryPrice
	h.Mu.Unlock()

	h.submitEntryLimit("GoEntry", entry)
	h.flashHotkeyStatus(fmt.Sprintf("Instant %s entry @ %.2f", dir, entry))
}

// submitEntryLimit sends a LIMIT GoEntry at `limitPrice` â€” instant entries are
// limits at Ask+N / Bid-N ticks, never stop orders, so they cannot be rejected
// for being priced through the market. Brackets are assembled by the hub on
// fill exactly as for any other entry.
func (h *Hub) submitEntryLimit(orderName string, limitPrice float64) {
	h.freezeAutoTrackOnEntry()
	h.cancelUnfilledEntryOrders()

	h.Mu.RLock()
	acc := h.State.SelectedAccount
	if acc == "" {
		acc = h.State.AccountName
	}
	inst := h.State.InstrumentName
	qty := h.State.CalculatedQty
	if qty <= 0 {
		qty = 1
	}
	action := "BUY"
	if !h.State.IsLong {
		action = "SELL_SHORT"
	}
	h.Mu.RUnlock()

	cmd := risk.SingleOrderCmd{
		AccountName: acc,
		Instrument:  inst,
		Action:      action,
		OrderType:   "Limit",
		Qty:         qty,
		LimitPrice:  limitPrice,
		StopPrice:   0,
		OcoId:       "",
		Name:        orderName,
	}
	bytes, err := json.Marshal(cmd)
	if err != nil {
		h.flashHotkeyStatus("Hotkey: instant entry marshal failed")
		return
	}
	log.Printf("Hotkey: Submitting %s LIMIT entry to NT8: %s", action, string(bytes))
	go h.ForwardToNT8("SUBMIT_ORDER", bytes)
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "HOTKEY",
		AccountName:    acc,
		InstrumentName: inst,
		Action:         action,
		OrderType:      "Limit",
		Qty:            qty,
		Price:          limitPrice,
		Success:        true,
		Details:        "Instant marketable limit entry submitted",
	})
	go h.BroadcastState()
}

// submitEntryMarket sends a MARKET GoEntry — used by the instant-entry hotkey
// when InstantEntryMode == "Market". Brackets are assembled by the hub on fill
// exactly as for any other entry.
func (h *Hub) submitEntryMarket(orderName string) {
	h.freezeAutoTrackOnEntry()
	h.cancelUnfilledEntryOrders()

	h.Mu.RLock()
	acc := h.State.SelectedAccount
	if acc == "" {
		acc = h.State.AccountName
	}
	inst := h.State.InstrumentName
	qty := h.State.CalculatedQty
	if qty <= 0 {
		qty = 1
	}
	action := "BUY"
	if !h.State.IsLong {
		action = "SELL_SHORT"
	}
	h.Mu.RUnlock()

	cmd := risk.SingleOrderCmd{
		AccountName: acc,
		Instrument:  inst,
		Action:      action,
		OrderType:   "Market",
		Qty:         qty,
		LimitPrice:  0,
		StopPrice:   0,
		OcoId:       "",
		Name:        orderName,
	}
	bytes, err := json.Marshal(cmd)
	if err != nil {
		h.flashHotkeyStatus("Hotkey: instant market entry marshal failed")
		return
	}
	log.Printf("Hotkey: Submitting %s MARKET entry to NT8: %s", action, string(bytes))
	go h.ForwardToNT8("SUBMIT_ORDER", bytes)
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "HOTKEY",
		AccountName:    acc,
		InstrumentName: inst,
		Action:         action,
		OrderType:      "Market",
		Qty:            qty,
		Price:          0,
		Success:        true,
		Details:        "Instant market entry order submitted",
	})
	go h.BroadcastState()
}

// hotkeyBreakoutEntry places a breakout entry at prior bar High/Low Â± offset
// in the CURRENT direction. Rejects trigger if already crossed.
func (h *Hub) hotkeyBreakoutEntry() {
	h.Mu.Lock()
	bars := h.trackingBars()
	if len(bars) < 2 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: need at least 2 bars to compute prior High/Low")
		return
	}
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	goLong := h.State.IsLong
	priorBar := bars[len(bars)-2] // prior bar on the configured tracking series
	offset := float64(h.State.BreakoutEntryOffsetTicks) * tick
	var entry float64
	if goLong {
		entry = risk.RoundToTickSize(priorBar.High+offset, tick)
	} else {
		entry = risk.RoundToTickSize(priorBar.Low-offset, tick)
	}

	ref := h.tickReference()
	if goLong && entry <= ref && ref > 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("Breakout ignored: Long trigger %.2f already crossed by market %.2f", entry, ref))
		return
	}
	if !goLong && entry >= ref && ref > 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("Breakout ignored: Short trigger %.2f already crossed by market %.2f", entry, ref))
		return
	}

	h.State.EntryPrice = entry
	risk.RecalculateState(h.State)
	expirySec := h.State.BreakoutExpirySeconds
	h.Mu.Unlock()

	h.submitEntry("GoEntry")
	h.flashHotkeyStatus(fmt.Sprintf("âš¡ Breakout entry @ %.2f", entry))

	if expirySec > 0 {
		time.AfterFunc(time.Duration(expirySec*float64(time.Second)), func() {
			h.cancelUnfilledEntryOrders()
		})
	}
}

// submitEntry composes the entry order from the freshly recalculated state and
// routes it to NT8. It mirrors HandleMessage's EXECUTE_ORDER case exactly.
func (h *Hub) submitEntry(orderName string) {
	h.freezeAutoTrackOnEntry()
	h.cancelUnfilledEntryOrders()

	h.Mu.RLock()
	plan := risk.BuildExecutionPlan(h.State, h.State.CurrentMarketPrice)
	inst := h.State.InstrumentName
	h.Mu.RUnlock()

	entryCmd := risk.SingleOrderCmd{
		AccountName: plan.AccountName,
		Instrument:  inst,
		Action:      plan.Action,
		OrderType:   plan.OrderType,
		Qty:         plan.Qty,
		LimitPrice:  plan.LimitPrice,
		StopPrice:   plan.StopPrice,
		OcoId:       "",
		Name:        orderName,
	}
	entryBytes, err := json.Marshal(entryCmd)
	if err == nil {
		log.Printf("Hotkey: Submitting entry order to NT8: %s", string(entryBytes))
		go h.ForwardToNT8("SUBMIT_ORDER", entryBytes)
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "HOTKEY",
			AccountName:    plan.AccountName,
			InstrumentName: inst,
			Action:         plan.Action,
			OrderType:      plan.OrderType,
			Qty:            plan.Qty,
			Price:          plan.LimitPrice,
			Success:        true,
			Details:        "Hotkey entry order submitted",
		})
	} else {
		log.Printf("Hotkey: error marshaling entry order: %v", err)
	}
	go h.BroadcastState()
}

// cancelUnfilledEntryOrders cancels any GoEntry* order that is still working (unfilled).
func (h *Hub) cancelUnfilledEntryOrders() {
	h.Mu.RLock()
	var pending []risk.WorkingOrderInfo
	for _, o := range h.State.WorkingOrders {
		if strings.HasPrefix(o.Name, "GoEntry") &&
			(o.State == "Working" || o.State == "Accepted") {
			pending = append(pending, o)
		}
	}
	h.Mu.RUnlock()

	for _, o := range pending {
		payload, _ := json.Marshal(map[string]string{"orderId": o.OrderId})
		go h.ForwardToNT8("CANCEL_ORDER", payload)
		log.Printf("Hotkey: cancelled unfilled entry order %s (%s)", o.OrderId, o.Name)
	}
}

// hotkeyTrailStop tightens the active StopLoss order to the prior bar High/Low Â± offset.
func (h *Hub) hotkeyTrailStop() {
	h.Mu.Lock()
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	bars := h.trackingBars()
	if len(bars) < 2 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: need at least 2 bars to compute prior High/Low")
		return
	}
	if h.State.Position.Quantity <= 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: no position to trail")
		return
	}
	goLong := h.State.Position.MarketPosition == "Long"
	var anchor risk.ChartBar
	if h.State.TrackAnchor == "CurrentBarHighLow" {
		anchor = bars[len(bars)-1]
	} else {
		anchor = bars[len(bars)-2] // prior bar on the configured tracking series
	}
	offset := float64(h.State.TrailStopOffsetTicks) * tick

	var newStop float64
	if goLong {
		newStop = risk.RoundToTickSize(anchor.Low-offset, tick)
	} else {
		newStop = risk.RoundToTickSize(anchor.High+offset, tick)
	}

	// Tighten-only guard
	curStop := h.State.StopPrice
	if goLong && newStop <= curStop {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("Trail ignored: would widen long risk (new %.2f <= current %.2f)", newStop, curStop))
		return
	}
	if !goLong && newStop >= curStop {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("Trail ignored: would widen short risk (new %.2f >= current %.2f)", newStop, curStop))
		return
	}

	h.State.StopPrice = newStop
	if !h.State.IsTpLocked {
		risk.RecalculateState(h.State)
	}

	var slOrders []risk.WorkingOrderInfo
	for _, o := range h.State.WorkingOrders {
		if (o.Name == "ActiveSL" || o.OrderType == "StopMarket") &&
			(o.State == "Working" || o.State == "Accepted") {
			slOrders = append(slOrders, o)
		}
	}
	working := make([]risk.WorkingOrderInfo, len(slOrders))
	copy(working, slOrders)
	newStopVal := h.State.StopPrice
	h.Mu.Unlock()

	for _, o := range working {
		h.SendChangeOrderToNT8(o.OrderId, newStopVal)
	}
	go h.BroadcastState()
	h.flashHotkeyStatus(fmt.Sprintf("ðŸ”’ Trail SL â†’ %.2f", newStopVal))
}

// hotkeyScaleOut closes pct% of the position as a limit order with timeout fallback to market.
// It assigns a unique actionId to prevent concurrent scale-outs and double-exits.
func (h *Hub) hotkeyScaleOut(pct float64) {
	h.Mu.Lock()
	pos := h.State.Position
	if pos.Quantity <= 0 || pos.MarketPosition == "Flat" {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: no position to scale out")
		return
	}
	if h.State.ScaleOutState == "WORKING_LIMIT" {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Scale-out ignored: a scale-out is already in flight")
		return
	}
	for _, o := range h.State.WorkingOrders {
		if (o.Name == "ScaleOut" || o.Name == "ScaleOutMarket") &&
			(o.State == "Working" || o.State == "Accepted") {
			h.Mu.Unlock()
			h.flashHotkeyStatus("Scale-out ignored: a scale-out order is already working")
			return
		}
	}
	bars := h.trackingBars()
	isLong := pos.MarketPosition == "Long"
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	if pct <= 0 {
		pct = 50.0
	}
	exitQty := int(math.Floor(float64(pos.Quantity) * pct / 100.0))
	if exitQty < 1 {
		exitQty = 1
	}
	if exitQty > pos.Quantity {
		exitQty = pos.Quantity
	}

	priceMode := h.State.ScaleOutPriceMode
	if priceMode == "" {
		priceMode = "BarHighLow"
	}
	var limitPrice float64
	if priceMode == "AskBid" {
		if isLong {
			limitPrice = risk.RoundToTickSize(h.usableBid(), tick)
		} else {
			limitPrice = risk.RoundToTickSize(h.usableAsk(), tick)
		}
	} else {
		// "BarHighLow" (default) uses the tracking bar (usually 15s);
		// "Candle1m" uses the current 1m candle high/low.
		var src []risk.ChartBar
		if priceMode == "Candle1m" {
			src = h.TimeframeBars["1m"]
		} else {
			src = bars
		}
		if len(src) < 1 {
			h.Mu.Unlock()
			h.flashHotkeyStatus("Hotkey ignored: no bar data for scale-out pricing")
			return
		}
		curBar := src[len(src)-1]
		if isLong {
			limitPrice = risk.RoundToTickSize(curBar.High, tick)
		} else {
			limitPrice = risk.RoundToTickSize(curBar.Low, tick)
		}
	}

	exitAction := "SELL"
	if !isLong {
		exitAction = "BUY_TO_COVER"
	}
	accountName := h.State.SelectedAccount
	if accountName == "" {
		accountName = h.State.AccountName
	}
	inst := h.State.InstrumentName
	timeoutSec := h.State.ScaleOutTimeoutSeconds

	actionId := fmt.Sprintf("SO_%d", time.Now().UnixNano()%1000000)
	h.State.ScaleOutState = "WORKING_LIMIT"
	h.Mu.Unlock()

	scaleCmd := risk.SingleOrderCmd{
		AccountName: accountName,
		Instrument:  inst,
		Action:      exitAction,
		OrderType:   "Limit",
		Qty:         exitQty,
		LimitPrice:  limitPrice,
		StopPrice:   0,
		OcoId:       "",
		Name:        "ScaleOut",
		Tag:         actionId,
	}
	scaleBytes, err := json.Marshal(scaleCmd)
	if err != nil {
		h.Mu.Lock()
		h.State.ScaleOutState = "IDLE"
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey: scale-out order marshal failed")
		return
	}
	log.Printf("Hotkey: Scale-out (%s) %s %d @ %.2f (limit)", actionId, exitAction, exitQty, limitPrice)
	go h.ForwardToNT8("SUBMIT_ORDER", scaleBytes)
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "HOTKEY",
		AccountName:    accountName,
		InstrumentName: inst,
		Action:         exitAction,
		OrderType:      "Limit",
		Qty:            exitQty,
		Price:          limitPrice,
		Success:        true,
		Details:        fmt.Sprintf("Scale-out limit submitted with actionId %s", actionId),
	})

	posQtyAtSubmit := pos.Quantity
	timeoutDur := time.Duration(timeoutSec * float64(time.Second))
	go func() {
		time.Sleep(timeoutDur)

		h.Mu.RLock()
		var orderId string
		for _, o := range h.State.WorkingOrders {
			if o.Name == "ScaleOut" && (o.State == "Working" || o.State == "Accepted") {
				orderId = o.OrderId
				break
			}
		}
		posNow := h.State.Position.Quantity
		h.Mu.RUnlock()

		if orderId == "" || posQtyAtSubmit-posNow >= exitQty {
			h.Mu.Lock()
			h.State.ScaleOutState = "IDLE"
			h.Mu.Unlock()
			return
		}

		cancelBytes, _ := json.Marshal(map[string]string{
			"orderId":     orderId,
			"accountName": accountName,
		})
		go h.ForwardToNT8("CANCEL_ORDER", cancelBytes)
		log.Printf("Hotkey: Scale-out timeout (%s) â€” cancelling limit %s, awaiting confirmation", actionId, orderId)

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(150 * time.Millisecond)
			h.Mu.RLock()
			stillThere := false
			for _, o := range h.State.WorkingOrders {
				if (o.OrderId == orderId || o.Name == "ScaleOut") && (o.State == "Working" || o.State == "Accepted") {
					stillThere = true
					break
				}
			}
			posNow = h.State.Position.Quantity
			h.Mu.RUnlock()

			if !stillThere {
				break
			}
			if posQtyAtSubmit-posNow >= exitQty {
				h.Mu.Lock()
				h.State.ScaleOutState = "IDLE"
				h.Mu.Unlock()
				return
			}
		}

		h.Mu.RLock()
		stillThere := false
		for _, o := range h.State.WorkingOrders {
			if (o.OrderId == orderId || o.Name == "ScaleOut") && (o.State == "Working" || o.State == "Accepted") {
				stillThere = true
				break
			}
		}
		posNow = h.State.Position.Quantity
		h.Mu.RUnlock()

		if stillThere {
			h.Mu.Lock()
			h.State.ScaleOutState = "IDLE"
			h.Mu.Unlock()
			log.Printf("Hotkey: Scale-out timeout (%s) â€” limit still working after cancel; aborting market conversion to avoid double-exit", actionId)
			h.flashHotkeyStatus("âš ï¸ Scale-out limit not cancelled â€” no market fallback (avoid double exit)")
			return
		}

		remainingQty := exitQty - (posQtyAtSubmit - posNow)
		if remainingQty <= 0 {
			h.Mu.Lock()
			h.State.ScaleOutState = "IDLE"
			h.Mu.Unlock()
			return
		}
		if remainingQty > posNow {
			remainingQty = posNow
		}
		mktCmd := risk.SingleOrderCmd{
			AccountName: accountName,
			Instrument:  inst,
			Action:      exitAction,
			OrderType:   "Market",
			Qty:         remainingQty,
			LimitPrice:  0,
			StopPrice:   0,
			OcoId:       "",
			Name:        "ScaleOutMarket",
			Tag:         actionId + "_MKT",
		}
		mktBytes, _ := json.Marshal(mktCmd)
		log.Printf("Hotkey: Scale-out timeout (%s) â†’ market %s %d (limit cancelled, qty adjusted)", actionId, exitAction, remainingQty)
		go h.ForwardToNT8("SUBMIT_ORDER", mktBytes)
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "HOTKEY",
			AccountName:    accountName,
			InstrumentName: inst,
			Action:         exitAction,
			OrderType:      "Market",
			Qty:            remainingQty,
			Success:        true,
			Details:        fmt.Sprintf("Scale-out timeout market fallback executed for actionId %s", actionId),
		})
		h.Mu.Lock()
		h.State.ScaleOutState = "IDLE"
		h.Mu.Unlock()
	}()

	h.flashHotkeyStatus(fmt.Sprintf("ðŸ“Š Scale out %d @ %.2f (limit)", exitQty, limitPrice))
}

// hotkeyFlatten emergency-exits the position by routing flatten to broker and marking CommandState pending.
func (h *Hub) hotkeyFlatten() {
	h.Mu.Lock()
	acc := h.State.SelectedAccount
	if acc == "" {
		acc = h.State.AccountName
	}
	inst := h.State.InstrumentName
	h.State.CommandState = "PENDING_FLATTEN"
	h.Mu.Unlock()

	go h.BroadcastState()
	payload, _ := json.Marshal(map[string]string{
		"accountName": acc,
		"instrument":  inst,
	})
	go h.ForwardToNT8("FLATTEN_POSITION", payload)
	h.flashHotkeyStatus("ðŸ›‘ Flatten command sent to broker â€” awaiting confirmation")
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "HOTKEY",
		AccountName:    acc,
		InstrumentName: inst,
		Action:         "FLATTEN",
		Success:        true,
		Details:        "Hotkey flatten command routed to NT8",
	})
}

// hotkeyStopAt5mCandle — the R hotkey: set the stop loss below the low of the
// current 5m candle (long) / above the high (short), and set the entry line to
// the current market price. This only moves PLAN lines (state) — the user then
// executes normally; brackets are assembled on fill as usual.
func (h *Hub) hotkeyStopAt5mCandle() {
	h.Mu.Lock()
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	bars := h.TimeframeBars["5m"]
	if len(bars) < 1 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: no 5m bar data")
		return
	}
	goLong := h.State.IsLong
	market := h.State.CurrentMarketPrice
	if market <= 0 {
		market = h.State.LastPrice
	}
	if market <= 0 {
		market = h.tickReference()
	}
	if market <= 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Hotkey ignored: no market reference")
		return
	}
	curBar := bars[len(bars)-1]
	var stop float64
	if goLong {
		stop = risk.RoundToTickSize(curBar.Low-tick, tick)
	} else {
		stop = risk.RoundToTickSize(curBar.High+tick, tick)
	}
	h.State.EntryPrice = risk.RoundToTickSize(market, tick)
	h.State.StopPrice = stop
	risk.RecalculateState(h.State)
	h.Mu.Unlock()

	go h.BroadcastState()
	h.flashHotkeyStatus(fmt.Sprintf("R: entry @ %.2f, stop @ %.2f (5m candle)", h.State.EntryPrice, stop))
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "HOTKEY",
		AccountName:    h.State.AccountName,
		InstrumentName: h.State.InstrumentName,
		Action:         "STOP_AT_5M",
		Success:        true,
		Details:        fmt.Sprintf("Entry=%.2f Stop=%.2f from 5m candle low/high", h.State.EntryPrice, stop),
	})
}
