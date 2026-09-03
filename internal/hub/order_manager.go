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

// SendChangeOrderToNT8 sends an explicit CHANGE_ORDER command to modify a specific order in NT8.
func (h *Hub) SendChangeOrderToNT8(orderId string, newPrice float64) {
	payload, err := json.Marshal(struct {
		OrderId string  `json:"orderId"`
		Price   float64 `json:"price"`
	}{
		OrderId: orderId,
		Price:   newPrice,
	})
	if err == nil {
		log.Printf("Routing CHANGE_ORDER to NT8: OrderId=%s Price=%.2f", orderId, newPrice)
		go h.ForwardToNT8("CHANGE_ORDER", payload)
	}
}

// SendChangeOrderQtyToNT8 sends an explicit CHANGE_ORDER command to modify a specific
// order's QUANTITY in NT8 (price unchanged).
func (h *Hub) SendChangeOrderQtyToNT8(orderId string, newQty int) {
	if orderId == "" || newQty <= 0 {
		return
	}
	payload, err := json.Marshal(struct {
		OrderId string `json:"orderId"`
		Qty     int    `json:"qty"`
	}{
		OrderId: orderId,
		Qty:     newQty,
	})
	if err == nil {
		log.Printf("Routing CHANGE_ORDER (qty) to NT8: OrderId=%s Qty=%d", orderId, newQty)
		go h.ForwardToNT8("CHANGE_ORDER", payload)
	}
}

// dispatchPendingBrackets consolidates all pending entry fills into exact OCO Stop Loss & Take Profit brackets.
func (h *Hub) dispatchPendingBrackets() {
	h.bracketMu.Lock()
	qty := h.pendingFillQty
	exec := h.pendingExec
	h.pendingFillQty = 0
	h.bracketTimer = nil
	h.bracketMu.Unlock()

	if qty <= 0 {
		return
	}

	h.Mu.RLock()
	posQty := h.State.Position.Quantity
	if posQty > 0 && qty > posQty {
		log.Printf("Bracket dispatch: clamping bracket qty from %d to position qty %d", qty, posQty)
		qty = posQty
	}
	isLong := (exec.Action == "Buy")
	accName := exec.AccountName
	if accName == "" {
		accName = h.State.SelectedAccount
	}
	if accName == "" {
		accName = h.State.AccountName
	}
	slPrice := h.State.StopPrice
	targetPrice := h.State.TargetPrice
	targetExits := make([]risk.TargetExit, len(h.State.TargetExits))
	copy(targetExits, h.State.TargetExits)
	usePartial := h.State.IsPartialProfit
	marketRef := h.State.CurrentMarketPrice
	if marketRef <= 0 {
		marketRef = h.State.LastPrice
	}
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	h.Mu.RUnlock()

	// ---- Bracket stop-side sanity guard ----
	if slPrice > 0 && marketRef > 0 {
		badLong := isLong && slPrice >= marketRef
		badShort := !isLong && slPrice <= marketRef
		if badLong || badShort {
			slOffset := 20.0 * tick
			if isLong {
				slPrice = risk.RoundToTickSize(marketRef-slOffset, tick)
			} else {
				slPrice = risk.RoundToTickSize(marketRef+slOffset, tick)
			}
			log.Printf("Bracket guard: stale stop %.2f reset to %.2f (market %.2f)", h.State.StopPrice, slPrice, marketRef)
		}
	}

	// Check if brackets ALREADY exist in working orders
	h.Mu.RLock()
	var existingSL *risk.WorkingOrderInfo
	var existingTP *risk.WorkingOrderInfo
	for i := range h.State.WorkingOrders {
		o := &h.State.WorkingOrders[i]
		if o.State != "Working" && o.State != "Accepted" {
			continue
		}
		if o.Name == "ActiveSL" && existingSL == nil {
			existingSL = o
		} else if (o.Name == "ActiveTP" || o.Name == "ActiveTP_1") && existingTP == nil {
			existingTP = o
		}
	}
	h.Mu.RUnlock()

	if existingSL != nil {
		newSLQty := existingSL.Qty + qty
		if posQty > 0 && newSLQty > posQty {
			newSLQty = posQty
		}
		h.SendChangeOrderQtyToNT8(existingSL.OrderId, newSLQty)
		if existingTP != nil {
			newTPQty := existingTP.Qty + qty
			if posQty > 0 && newTPQty > posQty {
				newTPQty = posQty
			}
			h.SendChangeOrderQtyToNT8(existingTP.OrderId, newTPQty)
		}
		log.Printf("Bracket dispatch: expanded existing brackets (SL=%s qty=%d)", existingSL.OrderId, newSLQty)
		return
	}

	var batch risk.BatchSubmitOrdersCmd
	ocoId := fmt.Sprintf("OCO_%d", time.Now().UnixNano()%100000000)

	exitAction := "SELL"
	if !isLong {
		exitAction = "BUY_TO_COVER"
	}

	// 1. Single Stop Loss Order (consolidated for all filled contracts)
	if slPrice > 0 {
		batch.Orders = append(batch.Orders, risk.SingleOrderCmd{
			AccountName: accName,
			Action:      exitAction,
			OrderType:   "StopMarket",
			Qty:         qty,
			LimitPrice:  0,
			StopPrice:   slPrice,
			OcoId:       ocoId,
			Name:        "ActiveSL",
		})
	}

	// 2. Profit Target Order(s)
	if usePartial && len(targetExits) > 1 {
		rem := qty
		for i, exit := range targetExits {
			ratio := exit.Ratio
			if ratio <= 0 {
				ratio = 1.0 / float64(len(targetExits))
			}
			exitQty := int(math.Round(float64(qty) * ratio))
			if exitQty <= 0 {
				exitQty = 1
			}
			if i == len(targetExits)-1 || exitQty > rem {
				exitQty = rem
			}
			rem -= exitQty
			tpName := fmt.Sprintf("ActiveTP_%d", i+1)
			if exit.Price > 0 && exitQty > 0 {
				batch.Orders = append(batch.Orders, risk.SingleOrderCmd{
					AccountName: accName,
					Action:      exitAction,
					OrderType:   "Limit",
					Qty:         exitQty,
					LimitPrice:  exit.Price,
					StopPrice:   0,
					OcoId:       ocoId,
					Name:        tpName,
				})
			}
			if rem <= 0 {
				break
			}
		}
	} else if targetPrice > 0 {
		batch.Orders = append(batch.Orders, risk.SingleOrderCmd{
			AccountName: accName,
			Action:      exitAction,
			OrderType:   "Limit",
			Qty:         qty,
			LimitPrice:  targetPrice,
			StopPrice:   0,
			OcoId:       ocoId,
			Name:        "ActiveTP",
		})
	}

	if len(batch.Orders) > 0 {
		batchBytes, err := json.Marshal(batch)
		if err == nil {
			log.Printf("Dispatching Consolidated Go-Generated Brackets to NT8 (TotalQty=%d): %s", qty, string(batchBytes))
			go h.ForwardToNT8("SUBMIT_ORDER", batchBytes)
		}
	}
}

// isValidStopPrice checks whether a proposed stop price is on the VALID side of the live market.
func (h *Hub) isValidStopPrice(o risk.WorkingOrderInfo, newStop float64) bool {
	if newStop <= 0 {
		return false
	}
	isSellSide := o.Action == "SELL" || o.Action == "SELL_SHORT" || o.Action == "Sell" || o.Action == "SellShort"
	bid := h.State.CurrentBid
	ask := h.State.CurrentAsk
	var marketRef float64
	if isSellSide {
		marketRef = bid
		if marketRef <= 0 {
			marketRef = h.State.LastPrice
		}
	} else {
		marketRef = ask
		if marketRef <= 0 {
			marketRef = h.State.LastPrice
		}
	}
	if marketRef <= 0 {
		return true
	}
	if isSellSide {
		return newStop < marketRef
	}
	return newStop > marketRef
}

// mergeBracketsAtSamePrice consolidates working SL and TP bracket orders at identical prices.
func (h *Hub) mergeBracketsAtSamePrice() {
	h.Mu.RLock()
	working := make([]risk.WorkingOrderInfo, len(h.State.WorkingOrders))
	copy(working, h.State.WorkingOrders)
	h.Mu.RUnlock()

	// Clean up cancellingOrders that are no longer in working orders
	workingIds := make(map[string]bool, len(working))
	for _, o := range working {
		workingIds[o.OrderId] = true
	}
	h.cancellingOrders.Range(func(key, value interface{}) bool {
		if id, ok := key.(string); ok && !workingIds[id] {
			h.cancellingOrders.Delete(id)
		}
		return true
	})

	type bracket struct {
		orderId string
		qty     int
	}

	slByPrice := map[float64][]bracket{}
	tpByPrice := map[float64][]bracket{}
	for _, o := range working {
		if o.State != "Working" && o.State != "Accepted" {
			continue
		}
		if _, isCancelling := h.cancellingOrders.Load(o.OrderId); isCancelling {
			continue
		}
		isSL := o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_")
		isTP := o.Name == "ActiveTP" || (len(o.Name) >= 9 && o.Name[:8] == "ActiveTP")
		if !isSL && !isTP {
			continue
		}
		key := o.Price
		if isSL {
			slByPrice[key] = append(slByPrice[key], bracket{o.OrderId, o.Qty})
		} else {
			tpByPrice[key] = append(tpByPrice[key], bracket{o.OrderId, o.Qty})
		}
	}

	mergeGroup := func(group []bracket, label string, price float64) {
		if len(group) < 2 {
			return
		}
		keeper := group[0]
		totalQty := 0
		for _, b := range group {
			totalQty += b.qty
		}
		h.Mu.RLock()
		posQty := h.State.Position.Quantity
		h.Mu.RUnlock()
		if posQty > 0 && totalQty > posQty {
			totalQty = posQty
		}
		h.SendChangeOrderQtyToNT8(keeper.orderId, totalQty)
		for _, b := range group[1:] {
			h.cancellingOrders.Store(b.orderId, true)
			payload, _ := json.Marshal(map[string]string{"orderId": b.orderId})
			go h.ForwardToNT8("CANCEL_ORDER", payload)
		}
		log.Printf("Hotkey: merged %d %s order(s) @ %.2f into one order (qty %d)", len(group), label, price, totalQty)
	}

	for price, group := range slByPrice {
		mergeGroup(group, "ActiveSL", price)
	}
	for price, group := range tpByPrice {
		mergeGroup(group, "ActiveTP", price)
	}
}

// syncBracketsToPosition shrinks working SL/TP bracket quantities to match position.
func (h *Hub) syncBracketsToPosition() {
	h.Mu.RLock()
	posQty := h.State.Position.Quantity
	slOrders := []risk.WorkingOrderInfo{}
	tpOrders := []risk.WorkingOrderInfo{}
	for _, o := range h.State.WorkingOrders {
		if o.State != "Working" && o.State != "Accepted" {
			continue
		}
		if o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_") {
			slOrders = append(slOrders, o)
		} else if o.Name == "ActiveTP" || (len(o.Name) >= 9 && o.Name[:8] == "ActiveTP") {
			tpOrders = append(tpOrders, o)
		}
	}

	resizeSide := func(orders []risk.WorkingOrderInfo, targetTotal int) []risk.WorkingOrderInfo {
		total := 0
		for _, o := range orders {
			total += o.Qty
		}
		newTotal := targetTotal
		if newTotal > total {
			newTotal = total
		}
		out := make([]risk.WorkingOrderInfo, 0, len(orders))
		remainingReduce := total - newTotal
		for _, o := range orders {
			reduce := o.Qty
			if reduce > remainingReduce {
				reduce = remainingReduce
			}
			remainingReduce -= reduce
			newQty := o.Qty - reduce
			if newQty <= 0 {
				continue
			}
			o.Qty = newQty
			out = append(out, o)
		}
		return out
	}
	slReduced := resizeSide(slOrders, posQty)
	tpReduced := resizeSide(tpOrders, posQty)

	cmdSL := make([]risk.WorkingOrderInfo, len(slReduced))
	copy(cmdSL, slReduced)
	cmdTP := make([]risk.WorkingOrderInfo, len(tpReduced))
	copy(cmdTP, tpReduced)
	origSL := make([]risk.WorkingOrderInfo, len(slOrders))
	copy(origSL, slOrders)
	origTP := make([]risk.WorkingOrderInfo, len(tpOrders))
	copy(origTP, tpOrders)
	h.Mu.RUnlock()

	origQty := map[string]int{}
	for _, o := range origSL {
		origQty[o.OrderId] = o.Qty
	}
	for _, o := range origTP {
		origQty[o.OrderId] = o.Qty
	}

	changed := 0
	for _, o := range append(cmdSL, cmdTP...) {
		prev := origQty[o.OrderId]
		if prev > 0 && o.Qty != prev {
			h.SendChangeOrderQtyToNT8(o.OrderId, o.Qty)
			changed++
		}
	}

	cancelled := 0
	for _, o := range origSL {
		found := false
		for _, r := range cmdSL {
			if r.OrderId == o.OrderId {
				found = true
				break
			}
		}
		if !found {
			payload, _ := json.Marshal(map[string]string{"orderId": o.OrderId})
			go h.ForwardToNT8("CANCEL_ORDER", payload)
			cancelled++
		}
	}
	for _, o := range origTP {
		found := false
		for _, r := range cmdTP {
			if r.OrderId == o.OrderId {
				found = true
				break
			}
		}
		if !found {
			payload, _ := json.Marshal(map[string]string{"orderId": o.OrderId})
			go h.ForwardToNT8("CANCEL_ORDER", payload)
			cancelled++
		}
	}
	if changed > 0 || cancelled > 0 {
		log.Printf("Hotkey: bracket sync to position â€” %d bracket(s) quantity-reduced, %d cancelled", changed, cancelled)
	}
}

// splitTargetOrder splits a working TakeProfit order in half.
func (h *Hub) splitTargetOrder(payload json.RawMessage) {
	var req struct {
		OrderId string  `json:"orderId"`
		Qty     int     `json:"qty"`
		Price   float64 `json:"price"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.OrderId == "" {
		log.Printf("SPLIT_TARGET: invalid payload %s", string(payload))
		return
	}

	h.Mu.RLock()
	var orderId string
	var orderAction string
	var orderPrice float64
	var orderQty int
	for _, o := range h.State.WorkingOrders {
		if o.OrderId == req.OrderId && (o.State == "Working" || o.State == "Accepted") {
			orderId = o.OrderId
			orderAction = o.Action
			orderPrice = o.Price
			orderQty = o.Qty
			break
		}
	}
	if orderId == "" || orderQty < 2 {
		h.Mu.RUnlock()
		h.flashHotkeyStatus("Split ignored: order not working or qty < 2")
		return
	}
	isLong := orderAction == "SELL" || orderAction == "Sell"
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	slDist := math.Abs(h.State.EntryPrice - h.State.StopPrice)
	if slDist <= 0 {
		slDist = 20 * tick
	}
	halfQty := orderQty / 2
	farQty := orderQty - halfQty
	farPrice := risk.RoundToTickSize(orderPrice+slDist, tick)
	if !isLong {
		farPrice = risk.RoundToTickSize(orderPrice-slDist, tick)
	}
	accountName := h.State.SelectedAccount
	if accountName == "" {
		accountName = h.State.AccountName
	}
	h.Mu.RUnlock()

	h.SendChangeOrderQtyToNT8(orderId, halfQty)

	newTp := risk.SingleOrderCmd{
		AccountName: accountName,
		Action:      orderAction,
		OrderType:   "Limit",
		Qty:         farQty,
		LimitPrice:  farPrice,
		StopPrice:   0,
		OcoId:       "",
		Name:        "ActiveTP",
	}
	bytes, err := json.Marshal(newTp)
	if err == nil {
		log.Printf("Split TP: %d @ %.2f stays, new %d @ %.2f", halfQty, orderPrice, farQty, farPrice)
		go h.ForwardToNT8("SUBMIT_ORDER", bytes)
	}
	h.flashHotkeyStatus(fmt.Sprintf("Â½ Split TP â†’ %d @ %.2f + %d @ %.2f", halfQty, orderPrice, farQty, farPrice))
}

// reconcileBrokerProtection checks if an open position exists without any working StopLoss order.
// If missing protection is detected, it flags an explicit alert and audit event.
func (h *Hub) reconcileBrokerProtection() {
	h.Mu.Lock()
	defer h.Mu.Unlock()

	pos := h.State.Position
	if pos.Quantity <= 0 || pos.MarketPosition == "Flat" {
		h.State.IsUnprotectedPosition = false
		h.State.ProtectionAlert = ""
		return
	}

	hasSL := false
	for _, o := range h.State.WorkingOrders {
		if (o.State == "Working" || o.State == "Accepted") &&
			(o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_") || o.OrderType == "StopMarket") {
			hasSL = true
			break
		}
	}

	if !hasSL {
		h.State.IsUnprotectedPosition = true
		alertMsg := fmt.Sprintf("CRITICAL: Position open (%s %d @ %.2f) with NO working Stop Loss!",
			pos.MarketPosition, pos.Quantity, pos.AveragePrice)
		h.State.ProtectionAlert = alertMsg
		log.Printf("ProtectionGuard: %s", alertMsg)
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "PROTECTION_ALERT",
			AccountName:    h.State.SelectedAccount,
			InstrumentName: h.State.InstrumentName,
			Action:         pos.MarketPosition,
			Qty:            pos.Quantity,
			Price:          pos.AveragePrice,
			Success:        false,
			Details:        alertMsg,
		})
	} else {
		h.State.IsUnprotectedPosition = false
		h.State.ProtectionAlert = ""
	}
}

// hotkeyBreakeven moves the working ActiveSL order to the position average fill price (+ optional offset ticks).
// It strictly enforces the tighten-only guard so it never widens risk.
func (h *Hub) hotkeyBreakeven(offsetTicks int) {
	h.Mu.Lock()
	if h.State.TradingDisabled {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Trading disabled by Kill-Switch")
		return
	}
	pos := h.State.Position
	if pos.Quantity <= 0 || pos.MarketPosition == "Flat" || pos.AveragePrice <= 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus("Breakeven ignored: no open position")
		return
	}

	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	isLong := pos.MarketPosition == "Long"
	offset := float64(offsetTicks) * tick
	var newStop float64
	if isLong {
		newStop = risk.RoundToTickSize(pos.AveragePrice+offset, tick)
	} else {
		newStop = risk.RoundToTickSize(pos.AveragePrice-offset, tick)
	}

	curStop := h.State.StopPrice
	// Tighten-only guard
	if isLong && newStop <= curStop && curStop > 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("BE ignored: would widen long risk (BE %.2f <= current %.2f)", newStop, curStop))
		return
	}
	if !isLong && newStop >= curStop && curStop > 0 {
		h.Mu.Unlock()
		h.flashHotkeyStatus(fmt.Sprintf("BE ignored: would widen short risk (BE %.2f >= current %.2f)", newStop, curStop))
		return
	}

	h.State.StopPrice = newStop
	if !h.State.IsTpLocked {
		risk.RecalculateState(h.State)
	}

	var slOrders []risk.WorkingOrderInfo
	for _, o := range h.State.WorkingOrders {
		if (o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_") || o.OrderType == "StopMarket") &&
			(o.State == "Working" || o.State == "Accepted") {
			slOrders = append(slOrders, o)
		}
	}
	working := make([]risk.WorkingOrderInfo, len(slOrders))
	copy(working, slOrders)
	newStopVal := h.State.StopPrice
	acc := h.State.SelectedAccount
	inst := h.State.InstrumentName
	h.Mu.Unlock()

	for _, o := range working {
		h.SendChangeOrderToNT8(o.OrderId, newStopVal)
	}
	go h.BroadcastState()
	h.flashHotkeyStatus(fmt.Sprintf("ðŸŽ¯ Stop to BE (+%d) â†’ %.2f", offsetTicks, newStopVal))
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "ORDER_CHANGE",
		AccountName:    acc,
		InstrumentName: inst,
		Action:         "BREAKEVEN",
		Price:          newStopVal,
		Success:        true,
		Details:        fmt.Sprintf("Moved stop to breakeven +%d ticks", offsetTicks),
	})
}

// cancelWorkingEntryOnly cancels any GoEntry* order that is working, leaving positions and brackets untouched.
func (h *Hub) cancelWorkingEntryOnly() {
	h.cancelUnfilledEntryOrders()
	h.flashHotkeyStatus("Cancelled working entry order")
}

// cancelOrdersOnly cancels all resting working orders on the selected account without market-flattening.
func (h *Hub) cancelOrdersOnly() {
	h.Mu.RLock()
	acc := h.State.SelectedAccount
	if acc == "" {
		acc = h.State.AccountName
	}
	h.Mu.RUnlock()

	payload, _ := json.Marshal(map[string]string{"accountName": acc})
	go h.ForwardToNT8("CANCEL_ORDER", payload)
	h.flashHotkeyStatus("Cancelled all working orders (position untouched)")
}

// emergencyKillSwitch cancels all orders, flattens position, and locks trading.
func (h *Hub) emergencyKillSwitch() {
	h.Mu.Lock()
	h.State.TradingDisabled = true
	h.State.HotkeysArmed = false
	h.State.EnableHotkeys = false
	h.State.CommandState = "PENDING_KILL_SWITCH"
	acc := h.State.SelectedAccount
	if acc == "" {
		acc = h.State.AccountName
	}
	inst := h.State.InstrumentName
	h.Mu.Unlock()

	go h.BroadcastState()
	payload, _ := json.Marshal(map[string]string{
		"accountName": acc,
		"instrument":  inst,
	})
	go h.ForwardToNT8("FLATTEN_POSITION", payload)
	h.flashHotkeyStatus("ðŸš¨ KILL SWITCH ACTIVATED â€” Trading disabled, flattening position!")
	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "KILL_SWITCH",
		AccountName:    acc,
		InstrumentName: inst,
		Success:        true,
		Details:        "Kill switch activated: all orders cancelled, flatten sent, trading disabled",
	})
}
