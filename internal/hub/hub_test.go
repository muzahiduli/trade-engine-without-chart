package hub

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"trade-engine-without-chart/internal/risk"
)

// mustJSON marshals v to JSON, failing the test on error.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal test payload: %v", err)
	}
	return b
}

// TestHub_PositionUpdate_FullContractName no drop: NT8 sends market info with
// the base symbol ("MNQ") but POSITION_UPDATE / ORDERS_UPDATE with the FULL
// contract name ("MNQ 09-26"). Strict equality used to drop both, so the hub
// stayed FLAT with no working orders while NT8 held a real position â€” the
// "position open in NT8 but not in the engine" bug the playback verification
// caught. Base-symbol matching must accept them.
func TestHub_PositionUpdate_FullContractName(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.InstrumentName = "MNQ"
	h.State.SelectedAccount = "Playback101"

	// Gateway market info: base symbol.
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "UPDATE_PRICES", Payload: mustJSON(t, map[string]interface{}{
		"instrumentName": "MNQ", "accountName": "Playback101",
	})})
	if h.State.InstrumentName != "MNQ" {
		t.Fatalf("expected instrument MNQ, got %q", h.State.InstrumentName)
	}

	// Gateway position update: FULL contract name. Must NOT be dropped.
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "POSITION_UPDATE", Payload: mustJSON(t, risk.PositionInfo{
		MarketPosition: "Long", Quantity: 10, AveragePrice: 29661.275,
		AccountName: "Playback101", InstrumentName: "MNQ 09-26",
	})})
	h.Mu.RLock()
	pos := h.State.Position
	h.Mu.RUnlock()
	if pos.Quantity != 10 || pos.MarketPosition != "Long" {
		t.Fatalf("position update with full contract name was dropped: got %s qty=%d", pos.MarketPosition, pos.Quantity)
	}

	// Working orders with full contract name must also be accepted.
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "ORDERS_UPDATE", Payload: mustJSON(t, []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Action: "SELL", OrderType: "StopMarket", Price: 29655.00, Qty: 9, State: "Working", AccountName: "Playback101", InstrumentName: "MNQ 09-26"},
		{OrderId: "tp1", Name: "ActiveTP", Action: "SELL", OrderType: "Limit", Price: 29670.00, Qty: 9, State: "Working", AccountName: "Playback101", InstrumentName: "MNQ 09-26"},
	})})
	h.Mu.RLock()
	orders := h.State.WorkingOrders
	stop := h.State.StopPrice
	h.Mu.RUnlock()
	if len(orders) != 2 {
		t.Fatalf("expected 2 working orders with full contract name, got %d", len(orders))
	}
	if stop != 29655.00 {
		t.Errorf("expected StopPrice synced from ActiveSL (29655), got %.2f", stop)
	}

	// A DIFFERENT instrument must still be rejected (isolation intact).
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "POSITION_UPDATE", Payload: mustJSON(t, risk.PositionInfo{
		MarketPosition: "Long", Quantity: 3, AveragePrice: 40000,
		AccountName: "Playback101", InstrumentName: "ES 09-26",
	})})
	h.Mu.RLock()
	pos2 := h.State.Position
	h.Mu.RUnlock()
	if pos2.Quantity != 10 {
		t.Fatalf("ES 09-26 position must not clobber MNQ position: got qty=%d", pos2.Quantity)
	}
}
// crash the playback verification found â€” a WEB client disconnects while a
// GET_BARS response goroutine is still delivering bars; closing client.Send
// must never panic a concurrent sendToClient (the hub used to die with
// "send on closed channel").
func TestHub_DisconnectRace_SendOnClosedChannel(t *testing.T) {
	h := NewHub()
	h.bracketTimer = nil
	h.pendingFillQty = 0
	go h.Run()

	web := &Client{Hub: h, Send: make(chan []byte, 2), ClientType: "WEB"}
	h.Register <- web

	// Seed a bar cache so SendBarsToClient has something to deliver.
	h.Mu.Lock()
	h.BarCache = make([]risk.ChartBar, 200)
	for i := range h.BarCache {
		h.BarCache[i] = risk.ChartBar{Time: int64(1783915200 + i), Open: 29670, High: 29672, Low: 29668, Close: 29671, Volume: 100}
	}
	h.Mu.Unlock()

	// Fire GET_BARS (spawns SendBarsToClient goroutine), then immediately
	// disconnect â€” the disconnect closes web.Send while the goroutine may
	// still be sending. Repeat to widen the race window.
	for i := 0; i < 50; i++ {
		h.HandleMessage(web, risk.WSMessage{Type: "GET_BARS", Payload: mustJSON(t, map[string]interface{}{"count": 200})})
		if i%2 == 0 {
			h.Unregister <- web
			web = &Client{Hub: h, Send: make(chan []byte, 2), ClientType: "WEB"}
			h.Register <- web
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Directly verify the guard itself: a send on a closed channel must
	// return false, never panic.
	closed := &Client{Send: make(chan []byte, 1), ClientType: "WEB"}
	close(closed.Send)
	if sendToClient(closed, []byte("x")) {
		t.Fatal("sendToClient on a closed channel must report failure, not panic")
	}

	h.Mu.RLock()
	_ = h.State
	h.Mu.RUnlock()
}

func setupTestHub() (*Hub, *Client, *Client) {
	h := NewHub()
	h.bracketTimer = nil
	h.pendingFillQty = 0

	nt8Client := &Client{
		Hub:        h,
		Send:       make(chan []byte, 100),
		ClientType: "NT8",
	}
	webClient := &Client{
		Hub:        h,
		Send:       make(chan []byte, 100),
		ClientType: "WEB",
	}

	h.Clients[nt8Client] = true
	h.Clients[webClient] = true

	h.State.InstrumentName = "NQ"
	h.State.TickSize = 0.25
	h.State.PointValue = 20.0
	h.State.SelectedAccount = "Sim101"
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.IsLong = true
	h.State.IsPartialProfit = false

	return h, nt8Client, webClient
}

// TestHub_ConsolidatedBracketsOnEntryPartialFills verifies that rapid partial fills
// (e.g. 14 contracts filling 3, 1, 2, 2, 2, 4) consolidate into EXACTLY ONE Stop Loss
// and ONE Target order for the total 14 contracts.
func TestHub_ConsolidatedBracketsOnEntryPartialFills(t *testing.T) {
	h, nt8Client, _ := setupTestHub()

	fills := []int{3, 1, 2, 2, 2, 4} // Total = 14 contracts

	for _, fillQty := range fills {
		exec := risk.ExecutionUpdateInfo{
			ExecutionId: "exec_test",
			OrderId:     "ord_entry_1",
			Name:        "GoEntry",
			Action:      "Buy",
			OrderState:  "PartFilled",
			FillQty:     fillQty,
			FillPrice:   20000.00,
			AccountName: "Sim101",
		}
		execBytes, _ := json.Marshal(exec)
		msg := risk.WSMessage{
			Type:    "EXECUTION_UPDATE",
			Payload: execBytes,
		}
		h.HandleMessage(nt8Client, msg)
	}

	// Wait for the 100ms debounce timer to fire
	time.Sleep(180 * time.Millisecond)

	// Collect any SUBMIT_ORDER messages sent to NT8
	var submitMsg []byte
	found := false
	for len(nt8Client.Send) > 0 {
		msgBytes := <-nt8Client.Send
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err == nil && wsMsg.Type == "SUBMIT_ORDER" {
			submitMsg = wsMsg.Payload
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Expected SUBMIT_ORDER message sent to NT8, but none received")
	}

	var batch risk.BatchSubmitOrdersCmd
	if err := json.Unmarshal(submitMsg, &batch); err != nil {
		t.Fatalf("Failed to unmarshal SUBMIT_ORDER payload: %v", err)
	}

	if len(batch.Orders) != 2 {
		t.Fatalf("Expected exactly 2 orders (1 Stop Loss + 1 Target), got %d", len(batch.Orders))
	}

	var slOrder, tpOrder *risk.SingleOrderCmd
	for i := range batch.Orders {
		if batch.Orders[i].Name == "ActiveSL" {
			slOrder = &batch.Orders[i]
		} else if batch.Orders[i].Name == "ActiveTP" {
			tpOrder = &batch.Orders[i]
		}
	}

	if slOrder == nil {
		t.Fatalf("Expected ActiveSL order in batch, but found none")
	}
	if tpOrder == nil {
		t.Fatalf("Expected ActiveTP order in batch, but found none")
	}

	// Assert full consolidated quantity of 14 contracts
	if slOrder.Qty != 14 {
		t.Errorf("Expected ActiveSL quantity 14, got %d", slOrder.Qty)
	}
	if tpOrder.Qty != 14 {
		t.Errorf("Expected ActiveTP quantity 14, got %d", tpOrder.Qty)
	}

	// Assert order types and prices
	if slOrder.OrderType != "StopMarket" || slOrder.StopPrice != 19980.00 {
		t.Errorf("Unexpected SL order config: Type=%s StopPrice=%.2f", slOrder.OrderType, slOrder.StopPrice)
	}
	if tpOrder.OrderType != "Limit" || tpOrder.LimitPrice != 20040.00 {
		t.Errorf("Unexpected TP order config: Type=%s LimitPrice=%.2f", tpOrder.OrderType, tpOrder.LimitPrice)
	}
	if slOrder.Action != "SELL" || tpOrder.Action != "SELL" {
		t.Errorf("Expected SELL action for Long exit brackets, got SL=%s TP=%s", slOrder.Action, tpOrder.Action)
	}
}

// TestHub_PartialProfitBrackets verifies that when Partial TP is enabled,
// the consolidated contracts are distributed across partial targets without leaving remaining contracts.
func TestHub_PartialProfitBrackets(t *testing.T) {
	h, nt8Client, _ := setupTestHub()

	h.State.IsPartialProfit = true
	h.State.TargetExits = []risk.TargetExit{
		{Ratio: 0.33, Price: 20030.00},
		{Ratio: 0.33, Price: 20050.00},
		{Ratio: 0.34, Price: 20070.00},
	}

	// Fill 10 contracts
	exec := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_10",
		OrderId:     "ord_entry_1",
		Name:        "GoEntry",
		Action:      "Buy",
		OrderState:  "Filled",
		FillQty:     10,
		FillPrice:   20000.00,
		AccountName: "Sim101",
	}
	execBytes, _ := json.Marshal(exec)
	msg := risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: execBytes}
	h.HandleMessage(nt8Client, msg)

	time.Sleep(180 * time.Millisecond)

	var submitMsg []byte
	for len(nt8Client.Send) > 0 {
		msgBytes := <-nt8Client.Send
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err == nil && wsMsg.Type == "SUBMIT_ORDER" {
			submitMsg = wsMsg.Payload
			break
		}
	}

	if submitMsg == nil {
		t.Fatalf("Expected SUBMIT_ORDER message sent to NT8")
	}

	var batch risk.BatchSubmitOrdersCmd
	json.Unmarshal(submitMsg, &batch)

	// 1 SL + 3 TPs = 4 orders
	if len(batch.Orders) != 4 {
		t.Fatalf("Expected 4 bracket orders (1 SL + 3 TPs), got %d", len(batch.Orders))
	}

	totalTpQty := 0
	for _, o := range batch.Orders {
		if strings.HasPrefix(o.Name, "ActiveTP") {
			totalTpQty += o.Qty
		}
	}

	if totalTpQty != 10 {
		t.Errorf("Expected total TP quantity to equal 10, got %d", totalTpQty)
	}
}

// TestHub_ShortPositionBrackets verifies that short positions generate BUY_TO_COVER brackets.
func TestHub_ShortPositionBrackets(t *testing.T) {
	h, nt8Client, _ := setupTestHub()

	h.State.IsLong = false
	h.State.StopPrice = 20020.00
	h.State.TargetPrice = 19960.00

	exec := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_short",
		OrderId:     "ord_entry_short",
		Name:        "GoEntry",
		Action:      "SellShort",
		OrderState:  "Filled",
		FillQty:     5,
		FillPrice:   20000.00,
		AccountName: "Sim101",
	}
	execBytes, _ := json.Marshal(exec)
	msg := risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: execBytes}
	h.HandleMessage(nt8Client, msg)

	time.Sleep(180 * time.Millisecond)

	var submitMsg []byte
	for len(nt8Client.Send) > 0 {
		msgBytes := <-nt8Client.Send
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err == nil && wsMsg.Type == "SUBMIT_ORDER" {
			submitMsg = wsMsg.Payload
			break
		}
	}

	var batch risk.BatchSubmitOrdersCmd
	json.Unmarshal(submitMsg, &batch)

	for _, o := range batch.Orders {
		if o.Action != "BUY_TO_COVER" {
			t.Errorf("Expected action BUY_TO_COVER for short exit order %s, got %s", o.Name, o.Action)
		}
	}
}

// TestHub_ExitFilledPositionClearing verifies that when ActiveTP or ActiveSL fills,
// position quantity is immediately reduced or reset to Flat.
func TestHub_ExitFilledPositionClearing(t *testing.T) {
	h, nt8Client, _ := setupTestHub()

	// Initial position: Long 5 contracts
	h.State.Position = risk.PositionInfo{
		MarketPosition: "Long",
		Quantity:       5,
		AveragePrice:   20000.00,
	}

	// Partial fill of 2 contracts on ActiveTP
	execPartial := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_tp1",
		OrderId:     "ord_tp1",
		Name:        "ActiveTP_1",
		Action:      "Sell",
		OrderState:  "Filled",
		FillQty:     2,
		FillPrice:   20030.00,
	}
	bytesPartial, _ := json.Marshal(execPartial)
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: bytesPartial})

	h.Mu.RLock()
	if h.State.Position.Quantity != 3 {
		t.Errorf("Expected position quantity 3 after partial exit, got %d", h.State.Position.Quantity)
	}
	h.Mu.RUnlock()

	// Full exit of remaining 3 contracts on ActiveSL
	execFull := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_sl",
		OrderId:     "ord_sl",
		Name:        "ActiveSL",
		Action:      "Sell",
		OrderState:  "Filled",
		FillQty:     3,
		FillPrice:   19980.00,
	}
	bytesFull, _ := json.Marshal(execFull)
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: bytesFull})

	h.Mu.RLock()
	if h.State.Position.MarketPosition != "Flat" || h.State.Position.Quantity != 0 {
		t.Errorf("Expected Flat position with 0 quantity, got %s (%d contracts)",
			h.State.Position.MarketPosition, h.State.Position.Quantity)
	}
	h.Mu.RUnlock()
}

// TestHub_FlattenPosition verifies that FLATTEN_POSITION clears position and working orders immediately in Go
// and forwards command to NT8.
func TestHub_FlattenPosition(t *testing.T) {
	h, nt8Client, webClient := setupTestHub()

	h.State.Position = risk.PositionInfo{
		MarketPosition: "Long",
		Quantity:       10,
		AveragePrice:   20000.00,
	}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl_1", Name: "ActiveSL", Price: 19980.00, Qty: 10, State: "Working"},
		{OrderId: "tp_1", Name: "ActiveTP", Price: 20040.00, Qty: 10, State: "Working"},
	}

	h.HandleMessage(webClient, risk.WSMessage{Type: "FLATTEN_POSITION", Payload: []byte(`{}`)})

	h.Mu.RLock()
	if h.State.CommandState != "PENDING_FLATTEN" {
		t.Errorf("Expected CommandState to be PENDING_FLATTEN, got %s", h.State.CommandState)
	}
	// Position must NOT be optimistically cleared before broker confirms
	if h.State.Position.MarketPosition != "Long" || h.State.Position.Quantity != 10 {
		t.Errorf("Expected Position to remain Long until broker confirms, got %s (qty: %d)",
			h.State.Position.MarketPosition, h.State.Position.Quantity)
	}
	h.Mu.RUnlock()

	// Now simulate broker POSITION_UPDATE confirming Flat
	posFlatJson, _ := json.Marshal(risk.PositionInfo{MarketPosition: "Flat", Quantity: 0})
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "POSITION_UPDATE", Payload: posFlatJson})

	h.Mu.RLock()
	if h.State.Position.MarketPosition != "Flat" || h.State.Position.Quantity != 0 {
		t.Errorf("Expected Position to be Flat after broker update, got %s (qty: %d)",
			h.State.Position.MarketPosition, h.State.Position.Quantity)
	}
	if h.State.CommandState != "IDLE" {
		t.Errorf("Expected CommandState to reset to IDLE after flat confirmed, got %s", h.State.CommandState)
	}
	h.Mu.RUnlock()

	// Verify command forwarded to NT8 (wait up to 50ms for goroutine)
	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err != nil || wsMsg.Type != "FLATTEN_POSITION" {
			t.Errorf("Expected FLATTEN_POSITION message, got %s", string(msgBytes))
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Expected FLATTEN_POSITION forwarded to NT8 client, but timed out")
	}
}

// TestHub_ChangeOrder verifies CHANGE_ORDER forwards payload directly to NT8.
func TestHub_ChangeOrder(t *testing.T) {
	h, nt8Client, webClient := setupTestHub()

	changeCmd := struct {
		Action  string  `json:"action"`
		OrderId string  `json:"orderId"`
		Price   float64 `json:"price"`
	}{
		Action:  "CHANGE",
		OrderId: "order_xyz_123",
		Price:   20050.25,
	}
	changeBytes, _ := json.Marshal(changeCmd)

	h.HandleMessage(webClient, risk.WSMessage{Type: "CHANGE_ORDER", Payload: changeBytes})

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err != nil || wsMsg.Type != "CHANGE_ORDER" {
			t.Fatalf("Expected CHANGE_ORDER message, got %s", string(msgBytes))
		}
		var parsed struct {
			OrderId string  `json:"orderId"`
			Price   float64 `json:"price"`
		}
		json.Unmarshal(wsMsg.Payload, &parsed)
		if parsed.OrderId != "order_xyz_123" || parsed.Price != 20050.25 {
			t.Errorf("Unexpected CHANGE_ORDER payload: OrderId=%s Price=%.2f", parsed.OrderId, parsed.Price)
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Expected CHANGE_ORDER forwarded to NT8 client, but timed out")
	}
}

// TestHub_CancelOrder verifies CANCEL_ORDER forwards payload directly to NT8.
func TestHub_CancelOrder(t *testing.T) {
	h, nt8Client, webClient := setupTestHub()

	cancelCmd := struct {
		Action  string `json:"action"`
		OrderId string `json:"orderId"`
	}{
		Action:  "CANCEL",
		OrderId: "ord_cancel_999",
	}
	cancelBytes, _ := json.Marshal(cancelCmd)

	h.HandleMessage(webClient, risk.WSMessage{Type: "CANCEL_ORDER", Payload: cancelBytes})

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err != nil || wsMsg.Type != "CANCEL_ORDER" {
			t.Fatalf("Expected CANCEL_ORDER message, got %s", string(msgBytes))
		}
		var parsed struct {
			OrderId string `json:"orderId"`
		}
		json.Unmarshal(wsMsg.Payload, &parsed)
		if parsed.OrderId != "ord_cancel_999" {
			t.Errorf("Expected OrderId ord_cancel_999, got %s", parsed.OrderId)
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Expected CANCEL_ORDER forwarded to NT8 client, but timed out")
	}
}

// TestHub_ExecuteOrderPlan verifies EXECUTE_ORDER builds a GoEntry plan and forwards to NT8.
func TestHub_ExecuteOrderPlan(t *testing.T) {
	h, nt8Client, webClient := setupTestHub()

	h.State.CurrentMarketPrice = 20000.00
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.RiskCash = 400.00 // $400 / ($20 * 20pts) = 1 contract
	risk.RecalculateState(h.State)

	h.HandleMessage(webClient, risk.WSMessage{Type: "EXECUTE_ORDER", Payload: []byte(`{}`)})

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err != nil || wsMsg.Type != "SUBMIT_ORDER" {
			t.Fatalf("Expected SUBMIT_ORDER message, got %s", string(msgBytes))
		}
		var cmd risk.SingleOrderCmd
		json.Unmarshal(wsMsg.Payload, &cmd)
		if cmd.Name != "GoEntry" || cmd.Action != "BUY" || cmd.Qty != 1 {
			t.Errorf("Unexpected GoEntry cmd: Name=%s Action=%s Qty=%d", cmd.Name, cmd.Action, cmd.Qty)
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Expected SUBMIT_ORDER forwarded to NT8 client, but timed out")
	}
}

// =========================================================================
// HOTKEY HANDLER TESTS
// =========================================================================

func setupHotkeyHub() (*Hub, *Client) {
	h := NewHub()
	h.bracketTimer = nil
	h.pendingFillQty = 0

	nt8Client := &Client{
		Hub:        h,
		Send:       make(chan []byte, 100),
		ClientType: "NT8",
	}
	h.Clients[nt8Client] = true

	h.State.InstrumentName = "NQ"
	h.State.TickSize = 0.25
	h.State.PointValue = 20.0
	h.State.SelectedAccount = "Sim101"
	h.State.CurrentBid = 20000.00
	h.State.CurrentAsk = 20000.25
	h.State.CurrentMarketPrice = 20000.00
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.IsLong = true
	h.State.IsPartialProfit = false
	h.State.RiskCash = 400.00
	h.State.MaxContracts = 10
	h.State.InstantEntryOffsetTicks = 2
	h.State.BreakoutEntryOffsetTicks = 1
	h.State.TrailStopOffsetTicks = 1
	h.State.ScaleOutPercent = 50.0
	h.State.ScaleOutTimeoutSeconds = 0.05

	// Two bars: prior bar (idx0) and current bar (idx1)
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19970, Close: 20000, Volume: 1},
		{Time: 2000, Open: 20000, High: 20020, Low: 19995, Close: 20010, Volume: 1},
	}

	risk.RecalculateState(h.State)
	return h, nt8Client
}

// TestHotkey_InstantEntryLong verifies Shift+F in the LONG-selected direction sets a
// long entry at Ask + offset ticks and submits a GoEntry order to NT8.
func TestHotkey_InstantEntryLong(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.IsLong = true
	origStop := h.State.StopPrice

	h.HandleHotkey("INSTANT_ENTRY")

	// Ask=20000.25 + 2 ticks(0.25) = 20000.75
	wantEntry := 20000.75
	if h.State.EntryPrice != wantEntry {
		t.Errorf("Expected entry %.2f, got %.2f", wantEntry, h.State.EntryPrice)
	}
	if h.State.StopPrice != origStop {
		t.Errorf("Stop should be preserved: expected %.2f, got %.2f", origStop, h.State.StopPrice)
	}
	if !h.State.IsLong {
		t.Errorf("Expected IsLong=true")
	}

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		json.Unmarshal(msgBytes, &wsMsg)
		if wsMsg.Type != "SUBMIT_ORDER" {
			t.Fatalf("Expected SUBMIT_ORDER, got %s", wsMsg.Type)
		}
		var cmd risk.SingleOrderCmd
		json.Unmarshal(wsMsg.Payload, &cmd)
		if cmd.Name != "GoEntry" || cmd.Action != "BUY" {
			t.Errorf("Unexpected cmd: Name=%s Action=%s", cmd.Name, cmd.Action)
		}
		// Regression: instant entry is a LIMIT at the entry price, NEVER a
		// StopLimit (a stop trigger sitting just above the last price got
		// rejected by NT8 as "buy stop below the market").
		if cmd.OrderType != "Limit" {
			t.Errorf("Expected instant entry to be a Limit order, got %s", cmd.OrderType)
		}
		if cmd.LimitPrice != wantEntry {
			t.Errorf("Expected LimitPrice %.2f, got %.2f", wantEntry, cmd.LimitPrice)
		}
		if cmd.StopPrice != 0 {
			t.Errorf("Instant entry must have no stop price, got %.2f", cmd.StopPrice)
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Timed out waiting for SUBMIT_ORDER")
	}
}

// TestHotkey_InstantEntryShort verifies Shift+F in the SHORT-selected direction sets a
// short entry at Bid - offset ticks.
func TestHotkey_InstantEntryShort(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.IsLong = false
	h.HandleHotkey("INSTANT_ENTRY")

	// Bid=20000.00 - 2 ticks(0.25) = 19999.50
	wantEntry := 19999.50
	if h.State.EntryPrice != wantEntry {
		t.Errorf("Expected entry %.2f, got %.2f", wantEntry, h.State.EntryPrice)
	}
	if h.State.IsLong {
		t.Errorf("Expected IsLong=false")
	}
}

// Regression: Market Replay can freeze the cached Ask/Bid far from the live
// last price. Shift+F must ignore that stale quote (fall back to the live
// last) so the limit entry reference is never absurdly far from reality.
func TestHotkey_InstantEntry_StaleQuoteFallsBackToLast(t *testing.T) {
	h, _ := setupHotkeyHub()
	// Stale ask: 20 points above the live last (well beyond the 25-tick band).
	h.State.CurrentMarketPrice = 20000.00
	h.State.CurrentAsk = 20020.00
	h.State.CurrentBid = 20019.75
	h.State.IsLong = true
	h.State.InstantEntryOffsetTicks = 2

	h.HandleHotkey("INSTANT_ENTRY")

	// Entry base must be the LIVE LAST (20000.00) + 2 ticks = 20000.50, NOT the
	// stale ask (20020) â€” otherwise a buy stop could sit below reality.
	want := 20000.50
	if h.State.EntryPrice != want {
		t.Errorf("Stale-quote fallback: expected entry %.2f (from live last), got %.2f", want, h.State.EntryPrice)
	}
}

// TestHotkey_BreakoutEntryLong verifies Shift+R in LONG direction sets entry at prior High + offset.
func TestHotkey_BreakoutEntryLong(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.IsLong = true
	h.HandleHotkey("BREAKOUT_ENTRY")

	// Prior bar High=20010 + 1 tick(0.25) = 20010.25
	wantEntry := 20010.25
	if h.State.EntryPrice != wantEntry {
		t.Errorf("Expected entry %.2f, got %.2f", wantEntry, h.State.EntryPrice)
	}
}

// TestHotkey_BreakoutEntryShort verifies Shift+R in SHORT direction sets entry at prior Low - offset.
func TestHotkey_BreakoutEntryShort(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.IsLong = false
	h.HandleHotkey("BREAKOUT_ENTRY")

	// Prior bar Low=19970 - 1 tick(0.25) = 19969.75
	wantEntry := 19969.75
	if h.State.EntryPrice != wantEntry {
		t.Errorf("Expected entry %.2f, got %.2f", wantEntry, h.State.EntryPrice)
	}
}

// TestHotkey_TrailStopTightenOnly verifies Shift+S tightens a long stop to prior bar Low
// and that it refuses to widen risk. Direction is resolved from the OPEN POSITION.
func TestHotkey_TrailStopTightenOnly(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 3, AveragePrice: 20000}
	h.State.StopPrice = 19990.00 // current stop
	risk.RecalculateState(h.State)

	h.HandleHotkey("TRAIL_STOP")

	// Prior bar Low=19970 - 1 tick = 19969.75, which is BELOW 19990 â†’ would widen â†’ ignored.
	if h.State.StopPrice != 19990.00 {
		t.Errorf("Trail should have been ignored (would widen), got Stop=%.2f", h.State.StopPrice)
	}

	// Now set a farther stop that a trail can actually tighten (long stop moves UP).
	// Prior bar Low=19970 â†’ trail target = 19970 - 1 tick = 19969.75.
	// To make tightening valid, current stop must be BELOW 19969.75, e.g. 19965.
	h.State.StopPrice = 19965.00
	h.HandleHotkey("TRAIL_STOP")
	wantStop := 19969.75
	if h.State.StopPrice != wantStop {
		t.Errorf("Expected tightened stop %.2f, got %.2f", wantStop, h.State.StopPrice)
	}
}

// TestHotkey_SwapDirection verifies S toggles Long/Short and mirrors stop+target
// across the entry price.
func TestHotkey_SwapDirection(t *testing.T) {
	h, _ := setupHotkeyHub()
	// setup: IsLong=true, Entry=20000, Stop=19980, Target=20040
	entry := h.State.EntryPrice
	stop := h.State.StopPrice
	target := h.State.TargetPrice

	h.HandleHotkey("SWAP_DIRECTION")

	if h.State.IsLong {
		t.Errorf("Expected IsLong to flip to false")
	}
	// Mirror: newStop = 2*entry - oldStop = 40000-19980 = 20020
	//         newTarget = 2*entry - oldTarget = 40000-20040 = 19960
	wantStop := risk.RoundToTickSize(2*entry-stop, h.State.TickSize)
	wantTarget := risk.RoundToTickSize(2*entry-target, h.State.TickSize)
	if h.State.StopPrice != wantStop {
		t.Errorf("Expected mirrored stop %.2f, got %.2f", wantStop, h.State.StopPrice)
	}
	if h.State.TargetPrice != wantTarget {
		t.Errorf("Expected mirrored target %.2f, got %.2f", wantTarget, h.State.TargetPrice)
	}
}

// TestHotkey_ScaleOutNoPosition verifies Ctrl+F is ignored when flat.
func TestHotkey_ScaleOutNoPosition(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}
	risk.RecalculateState(h.State)

	h.HandleHotkey("SCALE_OUT")

	select {
	case msgBytes := <-nt8Client.Send:
		t.Errorf("Expected no order when flat, got %s", string(msgBytes))
	case <-time.After(50 * time.Millisecond):
		// success: no order submitted
	}
}

// TestHotkey_ScaleOutInFlightGuard verifies a second Ctrl+F while a ScaleOut order
// is still working is IGNORED (prevents stacking exit orders that can overshoot flat).
func TestHotkey_ScaleOutInFlightGuard(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 10, AveragePrice: 20000}
	h.State.ScaleOutPercent = 50.0        // exitQty = 5
	h.State.ScaleOutTimeoutSeconds = 10.0 // keep the timeout goroutine out of this test's window
	risk.RecalculateState(h.State)

	// First press submits the ScaleOut limit order
	h.HandleHotkey("SCALE_OUT")
	var sawSubmit bool
	deadline := time.After(60 * time.Millisecond)
	for !sawSubmit {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) == nil && m.Type == "SUBMIT_ORDER" {
				sawSubmit = true
			}
		case <-deadline:
			t.Fatalf("Timed out waiting for first ScaleOut SUBMIT_ORDER")
		}
	}

	// Simulate NT8 reporting the ScaleOut order as Working
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "so1", Name: "ScaleOut", OrderType: "Limit", Qty: 5, State: "Working"},
	}

	// Second press must be ignored â€” no additional SUBMIT_ORDER arrives
	h.HandleHotkey("SCALE_OUT")

	deadline = time.After(60 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) == nil && m.Type == "SUBMIT_ORDER" {
				t.Errorf("Expected no second scale-out SUBMIT_ORDER, got %s", string(mb))
				return
			}
		case <-deadline:
			return // success: no submission
		}
	}
}

// TestHotkey_ScaleOutFillReducesPosition verifies a ScaleOut fill reduces position
// via EXECUTION_UPDATE so subsequent sizing never overshoots.
func TestHotkey_ScaleOutFillReducesPosition(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 10, AveragePrice: 20000}
	risk.RecalculateState(h.State)

	// ScaleOut sells 5 (50% of 10), NT8 reports the fill
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: []byte(`{"executionId":"e1","orderId":"o1","name":"ScaleOut","action":"Sell","fillPrice":20000,"fillQty":5,"orderState":"Filled","accountName":"Sim101"}`)})

	if h.State.Position.Quantity != 5 || h.State.Position.MarketPosition != "Long" {
		t.Errorf("Expected position reduced to 5 Long, got %+v", h.State.Position)
	}

	// Full exit to flat â€” position clamps at 0, never negative
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: []byte(`{"executionId":"e2","orderId":"o2","name":"ScaleOut","action":"Sell","fillPrice":20000,"fillQty":5,"orderState":"Filled","accountName":"Sim101"}`)})

	if h.State.Position.Quantity != 0 || h.State.Position.MarketPosition != "Flat" {
		t.Errorf("Expected position clamped to Flat/0, got %+v", h.State.Position)
	}
}

// TestHotkey_ScaleOutReducesBrackets verifies a ScaleOut fill REDUCES the quantity of
// the working TP/SL brackets (via CHANGE_ORDER) instead of cancelling them.
func TestHotkey_ScaleOutReducesBrackets(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 10, AveragePrice: 20000}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", OrderType: "StopMarket", Price: 19980, Qty: 10, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", OrderType: "Limit", Price: 20040, Qty: 10, State: "Working"},
	}
	risk.RecalculateState(h.State)

	// ScaleOut sells 5 of 10 â†’ brackets should be reduced to qty 5 each, NOT cancelled.
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: []byte(`{"executionId":"e1","orderId":"o1","name":"ScaleOut","action":"Sell","fillPrice":20000,"fillQty":5,"orderState":"Filled","accountName":"Sim101"}`)})

	// Expect CHANGE_ORDER(qty=5) for sl1 and tp1 (and no CANCEL_ORDER for them).
	seenChange := map[string]int{}
	seenCancel := map[string]bool{}
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CHANGE_ORDER" {
				var cmd struct {
					OrderId string  `json:"orderId"`
					Qty     int     `json:"qty"`
					Price   float64 `json:"price"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Qty > 0 {
					seenChange[cmd.OrderId] = cmd.Qty
				}
			} else if m.Type == "CANCEL_ORDER" {
				var cmd struct {
					OrderId string `json:"orderId"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil {
					seenCancel[cmd.OrderId] = true
				}
			}
		case <-deadline:
			// collect until quiet
			// check after a final short drain
			for {
				select {
				case mb := <-nt8Client.Send:
					var m risk.WSMessage
					if json.Unmarshal(mb, &m) == nil {
						if m.Type == "CHANGE_ORDER" {
							var cmd struct {
								OrderId string  `json:"orderId"`
								Qty     int     `json:"qty"`
								Price   float64 `json:"price"`
							}
							if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Qty > 0 {
								seenChange[cmd.OrderId] = cmd.Qty
							}
						} else if m.Type == "CANCEL_ORDER" {
							var cmd struct {
								OrderId string `json:"orderId"`
							}
							if json.Unmarshal(m.Payload, &cmd) == nil {
								seenCancel[cmd.OrderId] = true
							}
						}
					}
				default:
					if seenChange["sl1"] != 5 || seenChange["tp1"] != 5 {
						t.Fatalf("Expected CHANGE_ORDER qty=5 for sl1/tp1, got %+v (cancel=%v)", seenChange, seenCancel)
					}
					if seenCancel["sl1"] || seenCancel["tp1"] {
						t.Errorf("Brackets should be reduced, not cancelled: %+v", seenCancel)
					}
					return
				}
			}
		}
	}
}

// TestHotkey_ScaleOutSyncsBracketsToPosition verifies that after a scale-out fill, BOTH
// the SL side and TP side are sized to the LIVE position â€” even when the hub's mirror
// of working orders is stale on one side (the bug that left SL=3 while TP=2).
func TestHotkey_ScaleOutSyncsBracketsToPosition(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	// Position 3. The mirror is STALE/ASYNCHRONOUS: SL shows qty 4 (not yet echoed as 3),
	// TP shows qty 3. A scale-out of 1 will take position to 2.
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 3, AveragePrice: 20000}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", OrderType: "StopMarket", Price: 19980, Qty: 4, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", OrderType: "Limit", Price: 20040, Qty: 3, State: "Working"},
	}
	risk.RecalculateState(h.State)

	// ScaleOut sells 1 of 3 â†’ position becomes 2. Both SL and TP MUST sync to qty 2
	// (never SL=3 / TP=2 â€” the old bug).
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: []byte(`{"executionId":"e1","orderId":"o1","name":"ScaleOut","action":"Sell","fillPrice":20000,"fillQty":1,"orderState":"Filled","accountName":"Sim101"}`)})

	seenChange := map[string]int{}
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CHANGE_ORDER" {
				var cmd struct {
					OrderId string `json:"orderId"`
					Qty     int    `json:"qty"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Qty > 0 {
					seenChange[cmd.OrderId] = cmd.Qty
				}
			}
		case <-deadline:
			if h.State.Position.Quantity != 2 {
				t.Errorf("Expected position 2, got %d", h.State.Position.Quantity)
			}
			if seenChange["sl1"] != 2 {
				t.Errorf("Expected SL synced to qty 2 (position), got %d â€” stale-mirror bug", seenChange["sl1"])
			}
			if seenChange["tp1"] != 2 {
				t.Errorf("Expected TP synced to qty 2 (position), got %d", seenChange["tp1"])
			}
			return
		}
	}
}

// TestHotkey_Flatten verifies FLATTEN clears position state and forwards FLATTEN_POSITION.
func TestHotkey_Flatten(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 4, AveragePrice: 20000}

	h.HandleHotkey("FLATTEN")

	// Local position must NOT be optimistically cleared until broker confirms
	if h.State.Position.Quantity != 4 || h.State.Position.MarketPosition != "Long" {
		t.Errorf("Expected position to stay Long until confirmed, got %+v", h.State.Position)
	}
	if h.State.CommandState != "PENDING_FLATTEN" {
		t.Errorf("Expected CommandState PENDING_FLATTEN, got %s", h.State.CommandState)
	}

	// Broker confirms flat
	posFlatJson, _ := json.Marshal(risk.PositionInfo{MarketPosition: "Flat", Quantity: 0})
	h.HandleMessage(nt8Client, risk.WSMessage{Type: "POSITION_UPDATE", Payload: posFlatJson})
	if h.State.Position.Quantity != 0 || h.State.Position.MarketPosition != "Flat" {
		t.Errorf("Expected flat position after broker confirmation, got %+v", h.State.Position)
	}

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		json.Unmarshal(msgBytes, &wsMsg)
		if wsMsg.Type != "FLATTEN_POSITION" {
			t.Errorf("Expected FLATTEN_POSITION, got %s", wsMsg.Type)
		}
	case <-time.After(50 * time.Millisecond):
		t.Errorf("Timed out waiting for FLATTEN_POSITION")
	}
}

// TestHotkey_SecondEntryCancelsUnfilled verifies that pressing an entry hotkey again
// cancels any previously-placed UNFILLED GoEntry order before submitting a new one â€”
// so a user who is already in a trade never stacks a second resting entry.
func TestHotkey_SecondEntryCancelsUnfilled(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.IsLong = true

	// Simulate a prior resting (unfilled) entry from an earlier Shift+F
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "old_entry", Name: "GoEntry", OrderType: "Limit", Price: 20000.50, Qty: 5, State: "Working"},
	}
	risk.RecalculateState(h.State)

	h.HandleHotkey("INSTANT_ENTRY")

	// Expect: CANCEL_ORDER for old_entry AND a fresh SUBMIT_ORDER for the new GoEntry.
	seenCancelOld := false
	seenNewSubmit := false
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CANCEL_ORDER" {
				var cmd struct {
					OrderId string `json:"orderId"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.OrderId == "old_entry" {
					seenCancelOld = true
				}
			} else if m.Type == "SUBMIT_ORDER" {
				var cmd risk.SingleOrderCmd
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Name == "GoEntry" {
					seenNewSubmit = true
				}
			}
		case <-deadline:
			if !seenCancelOld {
				t.Errorf("Expected CANCEL_ORDER for the prior unfilled entry old_entry")
			}
			if !seenNewSubmit {
				t.Errorf("Expected a new SUBMIT_ORDER GoEntry")
			}
			return
		}
	}
}

// TestHotkey_MergeSamePriceBrackets verifies ORDERS_UPDATE with two ActiveSL orders at
// the SAME price consolidates them: one CHANGE_ORDER (qty=sum) on the keeper and a
// CANCEL_ORDER for the duplicate â€” so overlapping lines become a single order.
func TestHotkey_MergeSamePriceBrackets(t *testing.T) {
	h, nt8Client := setupHotkeyHub()

	// Simulate NT8 reporting two SL brackets at the same price (e.g. after dragging).
	ordersJSON := `[{"orderId":"slA","name":"ActiveSL","action":"SELL","orderType":"StopMarket","price":19990.00,"qty":3,"state":"Working"},
	                {"orderId":"slB","name":"ActiveSL","action":"SELL","orderType":"StopMarket","price":19990.00,"qty":5,"state":"Working"},
	                {"orderId":"tpA","name":"ActiveTP","action":"SELL","orderType":"Limit","price":20040.00,"qty":3,"state":"Working"},
	                {"orderId":"tpB","name":"ActiveTP","action":"SELL","orderType":"Limit","price":20040.00,"qty":5,"state":"Working"}]`

	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "ORDERS_UPDATE", Payload: []byte(ordersJSON)})

	// Expect: change qty of keeper slA to 8 (3+5), cancel slB; same for tpAâ†’8, cancel tpB.
	deadline := time.After(150 * time.Millisecond)
	changed := map[string]int{}
	cancelled := map[string]bool{}
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CHANGE_ORDER" {
				var cmd struct {
					OrderId string `json:"orderId"`
					Qty     int    `json:"qty"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Qty > 0 {
					changed[cmd.OrderId] = cmd.Qty
				}
			} else if m.Type == "CANCEL_ORDER" {
				var cmd struct {
					OrderId string `json:"orderId"`
				}
				if json.Unmarshal(m.Payload, &cmd) == nil {
					cancelled[cmd.OrderId] = true
				}
			}
		case <-deadline:
			if changed["slA"] != 8 {
				t.Errorf("Expected keeper slA qty 8, got %v", changed)
			}
			if changed["tpA"] != 8 {
				t.Errorf("Expected keeper tpA qty 8, got %v", changed)
			}
			if !cancelled["slB"] {
				t.Errorf("Expected duplicate slB cancelled, got %v", cancelled)
			}
			if !cancelled["tpB"] {
				t.Errorf("Expected duplicate tpB cancelled, got %v", cancelled)
			}
			return
		}
	}
}

// TestHotkey_NoMergeDifferentPrices verifies SL/TP orders at DIFFERENT prices are
// never merged.
func TestHotkey_NoMergeDifferentPrices(t *testing.T) {
	h, nt8Client := setupHotkeyHub()

	ordersJSON := `[{"orderId":"slA","name":"ActiveSL","orderType":"StopMarket","price":19990.00,"qty":3,"state":"Working"},
	                {"orderId":"slB","name":"ActiveSL","orderType":"StopMarket","price":19970.00,"qty":5,"state":"Working"}]`

	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "ORDERS_UPDATE", Payload: []byte(ordersJSON)})

	// No CHANGE_ORDER qty / CANCEL_ORDER should be generated.
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			json.Unmarshal(mb, &m)
			if m.Type == "CANCEL_ORDER" || m.Type == "CHANGE_ORDER" {
				t.Errorf("Unexpected %s for different prices: %s", m.Type, string(mb))
				return
			}
		case <-deadline:
			return // success: nothing emitted
		}
	}
}

// TestHotkey_ScaleOutTimeoutSequenced verifies the timeout fallback never submits the
// market exit concurrently with the limit: it must CANCEL first, wait for confirmations
// (via ORDERS_UPDATE), and only then submit the market. Simulates the double-exit bug
// (limit still resting when market fires) to ensure it cannot recur.
func TestHotkey_ScaleOutTimeoutSequenced(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 10, AveragePrice: 20000}
	h.State.ScaleOutPercent = 50.0        // exitQty = 5
	h.State.ScaleOutTimeoutSeconds = 0.02 // fast timeout for test
	risk.RecalculateState(h.State)

	// 1. Press Ctrl+F â†’ submits the ScaleOut limit order.
	h.HandleHotkey("SCALE_OUT")
	var limitId string
	deadline := time.After(100 * time.Millisecond)
	found := false
	for !found {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) == nil && m.Type == "SUBMIT_ORDER" {
				var cmd risk.SingleOrderCmd
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Name == "ScaleOut" {
					limitId = "fakeLimitId"
					found = true
				}
			}
		case <-deadline:
			t.Fatalf("Timed out waiting for ScaleOut limit submit")
		}
	}

	// 2. NT8 reports the limit as Working.
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: limitId, Name: "ScaleOut", OrderType: "Limit", Qty: 5, Price: 20010, State: "Working"},
	}

	// 3. Let the timeout elapse; expect a CANCEL_ORDER for the limit, and NO market
	//    order while the limit is still reported working.
	deadline = time.After(300 * time.Millisecond)
	seenCancel := false
	seenMarketWhileWorking := false
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CANCEL_ORDER" {
				seenCancel = true
			} else if m.Type == "SUBMIT_ORDER" {
				var cmd risk.SingleOrderCmd
				json.Unmarshal(m.Payload, &cmd)
				if cmd.Name == "ScaleOutMarket" {
					seenMarketWhileWorking = true
				}
			}
		case <-deadline:
			if !seenCancel {
				t.Errorf("Expected CANCEL_ORDER for the ScaleOut limit after timeout")
			}
			if seenMarketWhileWorking {
				t.Errorf("BUG: market scale-out submitted while limit still working (double-exit risk)")
			}
			goto cleared
		}
	}

cleared:
	// 4. Now NT8 confirms the limit was cancelled (leaves working set) and the position
	//    is untouched (no fill happened). The hub should then submit the market exit
	//    for the outstanding 5.
	h.State.WorkingOrders = nil
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "ORDERS_UPDATE", Payload: []byte(`[]`)})

	deadline = time.After(300 * time.Millisecond)
	seenMarket := false
	for !seenMarket {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) == nil && m.Type == "SUBMIT_ORDER" {
				var cmd risk.SingleOrderCmd
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Name == "ScaleOutMarket" && cmd.Qty == 5 {
					seenMarket = true
				}
			}
		case <-deadline:
			t.Errorf("Expected ScaleOutMarket qty=5 after limit cancel confirmed")
			return
		}
	}
}

// TestIsValidStopPrice verifies the stop-price market-side guard that prevents NT8's
// "stop price can't be changed above the market" rejection.
func TestIsValidStopPrice(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.CurrentBid = 20000.00
	h.State.CurrentAsk = 20000.25

	// Long (SELL stop): valid stop must be BELOW the bid.
	sl := risk.WorkingOrderInfo{Action: "Sell", OrderType: "StopMarket", Price: 19980}
	if !h.isValidStopPrice(sl, 19990) {
		t.Errorf("Expected 19990 sell-stop valid (below market)")
	}
	if h.isValidStopPrice(sl, 20000.50) {
		t.Errorf("Expected 20000.50 sell-stop INVALID (above market)")
	}
	if h.isValidStopPrice(sl, 20000.00) {
		t.Errorf("Expected sell-stop AT market INVALID (priced through)")
	}

	// Short (BUY_TO_COVER stop): valid stop must be ABOVE the ask.
	sc := risk.WorkingOrderInfo{Action: "BuyToCover", OrderType: "StopMarket", Price: 20020}
	if !h.isValidStopPrice(sc, 20010) {
		t.Errorf("Expected 20010 buy-stop valid (above market)")
	}
	if h.isValidStopPrice(sc, 19999.50) {
		t.Errorf("Expected 19999.50 buy-stop INVALID (below market)")
	}

	// No market data yet â†’ treated as valid (let NT8 decide).
	h.State.CurrentBid = 0
	h.State.CurrentAsk = 0
	h.State.LastPrice = 0
	if !h.isValidStopPrice(sl, 20000.50) {
		t.Errorf("Expected no-market-data change allowed")
	}
}

// TestSplitTarget verifies SPLIT_TARGET reduces the original TP to half qty and
// submits a NEW TP one R further out with the other half.
func TestSplitTarget(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	// Long position: entry 20000, stop 19980 â†’ slDist 20 (80 ticks at 0.25).
	// TP currently 20040 (qty 6). Split â†’ 3 @ 20040 stays, 3 @ 20060 new.
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 6, AveragePrice: 20000}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "tp1", Name: "ActiveTP", Action: "Sell", OrderType: "Limit", Price: 20040.00, Qty: 6, State: "Working"},
		{OrderId: "sl1", Name: "ActiveSL", Action: "Sell", OrderType: "StopMarket", Price: 19980.00, Qty: 6, State: "Working"},
	}
	risk.RecalculateState(h.State)

	h.HandleMessage(&Client{ClientType: "WEB", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "SPLIT_TARGET", Payload: []byte(`{"orderId":"tp1","qty":6,"price":20040}`)})

	seenChangeQty := 0
	seenNewTp := 0
	asPrice := 0.0
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "CHANGE_ORDER" {
				var cmd struct {
					Qty int `json:"qty"`
				}
				json.Unmarshal(m.Payload, &cmd)
				if cmd.Qty == 3 {
					seenChangeQty++
				}
			} else if m.Type == "SUBMIT_ORDER" {
				var cmd risk.SingleOrderCmd
				if json.Unmarshal(m.Payload, &cmd) == nil && cmd.Name == "ActiveTP" && cmd.Qty == 3 {
					seenNewTp++
					asPrice = cmd.LimitPrice
				}
			}
		case <-deadline:
			if seenChangeQty == 0 {
				t.Errorf("Expected CHANGE_ORDER qty=3 on original TP")
			}
			if seenNewTp != 1 {
				t.Errorf("Expected 1 new ActiveTP submit, got %d", seenNewTp)
			}
			if asPrice != 20060.00 {
				t.Errorf("Expected new TP at 20060.00 (one R further), got %.2f", asPrice)
			}
			return
		}
	}
}

// TestUpdatePricesStaleGuard verifies that a WEB client sending a price ~hundreds of
// points away from the live market is IGNORED (stale session state must never be
// applied or routed to NT8 â€” the root cause of "stop price can't be changed above
// the market" errors).
func TestUpdatePricesStaleGuard(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	// Live market ~29600, entry/stop/target near market.
	h.State.CurrentMarketPrice = 29635.00
	h.State.LastPrice = 29635.00
	h.State.EntryPrice = 29630.00
	h.State.StopPrice = 29610.00
	h.State.TargetPrice = 29670.00
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 29630, High: 29640, Low: 29620, Close: 29635, Volume: 1},
		{Time: 2000, Open: 29635, High: 29645, Low: 29625, Close: 29638, Volume: 1},
	}
	risk.RecalculateState(h.State)

	// A stale web sends entryPrice far from the market (old session price).
	h.HandleMessage(&Client{ClientType: "WEB", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "UPDATE_PRICES", Payload: []byte(`{"entryPrice":29347,"stopPrice":29342,"targetPrice":29357}`)})

	// Entry must remain at the sane level â€” stale values ignored.
	h.Mu.RLock()
	entryVal := h.State.EntryPrice
	stopVal := h.State.StopPrice
	targetVal := h.State.TargetPrice
	h.Mu.RUnlock()
	if math.Abs(entryVal-29630.00) > 0.01 {
		t.Errorf("Stale entryPrice should be ignored, got %.2f", entryVal)
	}
	if math.Abs(stopVal-29610.00) > 0.01 {
		t.Errorf("Stale stopPrice should be ignored, got %.2f", stopVal)
	}
	if math.Abs(targetVal-29670.00) > 0.01 {
		t.Errorf("Stale targetPrice should be ignored, got %.2f", targetVal)
	}

	// No CHANGE_ORDER should have been routed for stale prices.
	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		json.Unmarshal(mb, &m)
		if m.Type == "CHANGE_ORDER" {
			t.Errorf("No CHANGE_ORDER should be routed for stale prices, got %s", string(mb))
		}
	default:
		// success â€” nothing routed (or only broadcast state)
	}
}

// TestDispatchBracketsSnapsStaleStop verifies dispatchPendingBrackets resets a
// stale stop price that sits on the wrong side of the market before submitting
// brackets (otherwise NT8 rejects the StopMarket submission).
func TestDispatchBracketsSnapsStaleStop(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	// Long position; live market 29635; stale wrong-side stop ABOVE market (should
	// be below for a long).
	h.State.CurrentMarketPrice = 29635.00
	h.State.LastPrice = 29635.00
	h.State.EntryPrice = 29630.00
	h.State.StopPrice = 29690.00 // stale: sell stop above market â†’ invalid
	h.State.TargetPrice = 29670.00
	h.State.TickSize = 0.25
	h.BarCache = []risk.ChartBar{
		{Time: 2000, Open: 29630, High: 29640, Low: 29625, Close: 29635, Volume: 1},
	}
	risk.RecalculateState(h.State)

	// Simulate a GoEntry fill that triggers bracket dispatch.
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 29630}
	h.HandleMessage(&Client{ClientType: "NT8", Send: make(chan []byte, 10)},
		risk.WSMessage{Type: "EXECUTION_UPDATE", Payload: []byte(`{"executionId":"e1","orderId":"entry1","name":"GoEntry","action":"Buy","fillPrice":29630,"fillQty":2,"orderState":"Filled","accountName":"Sim101"}`)})

	// The dispatched ActiveSL stop must be BELOW the market (not the stale 29690).
	sawSubmit := false
	deadline := time.After(300 * time.Millisecond)
	for !sawSubmit {
		select {
		case mb := <-nt8Client.Send:
			var m risk.WSMessage
			if json.Unmarshal(mb, &m) != nil {
				continue
			}
			if m.Type == "SUBMIT_ORDER" {
				var batch risk.BatchSubmitOrdersCmd
				json.Unmarshal(m.Payload, &batch)
				for _, o := range batch.Orders {
					if o.Name == "ActiveSL" {
						sawSubmit = true
						if o.StopPrice >= 29635.00 {
							t.Errorf("ActiveSL stop should be below market, got %.2f", o.StopPrice)
						}
					}
				}
			}
		case <-deadline:
			t.Errorf("Timed out waiting for bracket dispatch")
			return
		}
	}
}

func TestHub_SetConfig_DirectionFlip(t *testing.T) {
	h, _, _ := setupTestHub()
	// Initial state: Entry=20000, Stop=19980 (diff=20), Target=20040 (diff=40), IsLong=true
	flipToShort := false
	payload, _ := json.Marshal(map[string]interface{}{
		"isLong": flipToShort,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.IsLong != false {
		t.Errorf("Expected IsLong to be false, got %v", h.State.IsLong)
	}
	// Stop should be flipped above entry: 20000 + 20 = 20020
	if h.State.StopPrice != 20020.00 {
		t.Errorf("Expected StopPrice to be 20020.00 on flip, got %.2f", h.State.StopPrice)
	}
	// Target should be flipped below entry: 20000 - 40 = 19960
	if h.State.TargetPrice != 19960.00 {
		t.Errorf("Expected TargetPrice to be 19960.00 on flip, got %.2f", h.State.TargetPrice)
	}
}

func TestHub_SetConfig_RiskCashChange(t *testing.T) {
	h, _, _ := setupTestHub()
	// PointValue=20, Entry=20000, Stop=19980 (slDist=20 pts -> $400/contract)
	// Change RiskCash to $800 -> should adjust stop distance and target proportionally
	newRisk := 800.0
	payload, _ := json.Marshal(map[string]interface{}{
		"riskCash": newRisk,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.RiskCash != 800.0 {
		t.Errorf("Expected RiskCash 800, got %.2f", h.State.RiskCash)
	}
	if h.State.CalculatedQty < 1 {
		t.Errorf("Expected CalculatedQty >= 1, got %d", h.State.CalculatedQty)
	}
}

func TestHub_SetConfig_SelectedRR(t *testing.T) {
	h, _, _ := setupTestHub()
	newRR := 3.5
	payload, _ := json.Marshal(map[string]interface{}{
		"selectedRR": newRR,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.SelectedRR != 3.5 {
		t.Errorf("Expected SelectedRR 3.5, got %.2f", h.State.SelectedRR)
	}
	if h.State.HasCustomTargets != false {
		t.Error("Expected HasCustomTargets to be false after setting selectedRR")
	}
}

func TestHub_SetConfig_PartialProfitToggle(t *testing.T) {
	h, _, _ := setupTestHub()
	enablePartial := true
	payload, _ := json.Marshal(map[string]interface{}{
		"isPartialProfit": enablePartial,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if !h.State.IsPartialProfit {
		t.Error("Expected IsPartialProfit to be true")
	}
	if len(h.State.TargetExits) < 2 {
		t.Errorf("Expected at least 2 target exits when partial profit enabled, got %d", len(h.State.TargetExits))
	}
}

func TestHub_SetConfig_AccountSwitch(t *testing.T) {
	h, _, _ := setupTestHub()
	newAcc := "Playback101"
	payload, _ := json.Marshal(map[string]interface{}{
		"selectedAccount": newAcc,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.SelectedAccount != "Playback101" {
		t.Errorf("Expected SelectedAccount 'Playback101', got '%s'", h.State.SelectedAccount)
	}
}

func TestHub_UpdatePrices_PartialPatch(t *testing.T) {
	h, _, _ := setupTestHub()
	// Original entry=20000, stop=19980, target=20040, selectedRR=2.0
	// Send ONLY stopPrice = 19990 (slDist narrows from 20 to 10)
	newStop := 19990.00
	payload, _ := json.Marshal(map[string]interface{}{
		"stopPrice": newStop,
	})

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.StopPrice != 19990.00 {
		t.Errorf("Expected StopPrice 19990.00, got %.2f", h.State.StopPrice)
	}
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("Expected EntryPrice to remain 20000.00, got %.2f", h.State.EntryPrice)
	}
	// TargetPrice recalculates to maintain 2:1 RR: 20000 + (10 * 2) = 20020.00
	if h.State.TargetPrice != 20020.00 {
		t.Errorf("Expected TargetPrice 20020.00 (maintaining 2:1 RR), got %.2f", h.State.TargetPrice)
	}
}

func TestHub_UpdatePrices_TargetDragUpdatesRR(t *testing.T) {
	h, _, _ := setupTestHub()
	// Entry=20000, Stop=19980 (slDist = 20)
	// Update target to 20060 (tpDist = 60 -> RR = 3.0)
	newTarget := 20060.00
	payload, _ := json.Marshal(map[string]interface{}{
		"targetPrice": newTarget,
	})

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.TargetPrice != 20060.00 {
		t.Errorf("Expected TargetPrice 20060.00, got %.2f", h.State.TargetPrice)
	}
	if math.Abs(h.State.SelectedRR-3.0) > 0.05 {
		t.Errorf("Expected SelectedRR to recalculate to 3.0, got %.2f", h.State.SelectedRR)
	}
	if !h.State.HasCustomTargets {
		t.Error("Expected HasCustomTargets to be true after target drag")
	}
}

// TestHub_UpdatePrices_WEB_NoMarketRef_Rejected is the regression for the
// fresh-boot hazard: a reconnecting WEB tab pushing stale session prices (e.g.
// 29347 from an old session) before NT8 market data arrives must NOT seed the
// hub's planning levels â€” otherwise instant entry could build brackets far from
// the real market (a TP below the entry for a long).
func TestHub_UpdatePrices_WEB_NoMarketRef_Rejected(t *testing.T) {
	h, _, _ := setupTestHub()
	// Fresh boot: NO currentMarketPrice / lastPrice / bid / ask yet.
	h.State.CurrentMarketPrice = 0
	h.State.LastPrice = 0
	h.State.CurrentBid = 0
	h.State.CurrentAsk = 0
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0

	payload, _ := json.Marshal(map[string]interface{}{
		"entryPrice":  29347.00,
		"stopPrice":   29342.00,
		"targetPrice": 29357.00,
	})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 0 {
		t.Errorf("WEB patch seeded entry without market reference: got %.2f, want 0", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("WEB patch seeded stop without market reference: got %.2f, want 0", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("WEB patch seeded target without market reference: got %.2f, want 0", h.State.TargetPrice)
	}
}

func TestHub_UpdatePrices_ActiveOrderModification(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	// Live market reference so the WEB price patch can be validated (WEB
	// patches without market data are now rejected as unverifiable).
	h.State.CurrentMarketPrice = 20000.00
	// Simulate resting GoEntry order
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "order_entry_99", Name: "GoEntry", Action: "Buy", OrderType: "Limit", Price: 20000.00, Qty: 2, State: "Working"},
	}

	// WEB client drags entry to 20005.00
	newEntry := 20005.00
	payload, _ := json.Marshal(map[string]interface{}{
		"entryPrice": newEntry,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: payload,
	})

	// NT8 client should receive a CHANGE_ORDER for order_entry_99
	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		json.Unmarshal(mb, &m)
		if m.Type != "CHANGE_ORDER" {
			t.Errorf("Expected CHANGE_ORDER, got %s", m.Type)
		}
		var change struct {
			OrderId string  `json:"orderId"`
			Price   float64 `json:"price"`
		}
		json.Unmarshal(m.Payload, &change)
		if change.OrderId != "order_entry_99" || change.Price != 20005.00 {
			t.Errorf("Expected CHANGE_ORDER for order_entry_99 @ 20005.00, got %s @ %.2f", change.OrderId, change.Price)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("Timed out waiting for CHANGE_ORDER to be routed to NT8")
	}
}

func TestHub_UpdatePrices_NoAutoInitInPosition(t *testing.T) {
	h, _, _ := setupTestHub()
	// Simulate being in an active position
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 5, AveragePrice: 20000.00}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00

	// Market moves to 20100 (> 50 pts away)
	newMkt := 20100.00
	payload, _ := json.Marshal(map[string]interface{}{
		"currentMarketPrice": newMkt,
	})

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	// Entry/Stop/Target should NOT be auto-aligned when in position
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("EntryPrice was auto-aligned while in position: %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19980.00 {
		t.Errorf("StopPrice was auto-aligned while in position: %.2f", h.State.StopPrice)
	}
}

func TestHub_UpdateTarget_IndexedTargetDrag(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsPartialProfit = true
	risk.RecalculateState(h.State)

	if len(h.State.TargetExits) < 2 {
		t.Fatalf("Need at least 2 target exits for test, got %d", len(h.State.TargetExits))
	}

	// Drag target index 0 (intermediate target) to a custom price
	targetIdx := 0
	newPrice := 20025.00
	payload, _ := json.Marshal(map[string]interface{}{
		"index": targetIdx,
		"price": newPrice,
	})

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "UPDATE_TARGET",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.TargetExits[targetIdx].Price != 20025.00 {
		t.Errorf("Expected TargetExit[0] price 20025.00, got %.2f", h.State.TargetExits[targetIdx].Price)
	}
	if !h.State.HasCustomTargets {
		t.Error("Expected HasCustomTargets to be true")
	}
}

func TestHub_BarUpdate_AutoTrack(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackOffsetTicks = 2
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00

	// Seed bar cache with prior bar
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
	}

	// New bar arrives with Time=1015 (> 1000)
	// Prior bar High is 20010. Target entry = High (20010) + offset (2 * 0.25 = 0.50) = 20010.50
	// Delta = 20010.50 - 20000.00 = +10.50
	newBar := risk.ChartBar{
		Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 50,
	}
	payload, _ := json.Marshal(newBar)

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.EntryPrice != 20010.50 {
		t.Errorf("Expected auto-tracked EntryPrice 20010.50, got %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19990.50 {
		t.Errorf("Expected StopPrice shifted by delta to 19990.50, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20050.50 {
		t.Errorf("Expected TargetPrice shifted by delta to 20050.50, got %.2f", h.State.TargetPrice)
	}
}

func TestHub_BarUpdate_SameTimestampUpdate(t *testing.T) {
	h, _, _ := setupTestHub()
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 10},
	}

	// Same timestamp bar with updated Close
	updatedBar := risk.ChartBar{
		Time: 1000, Open: 20000, High: 20015, Low: 19990, Close: 20012, Volume: 25,
	}
	payload, _ := json.Marshal(updatedBar)

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if len(h.BarCache) != 1 {
		t.Fatalf("Expected BarCache length to remain 1 on same timestamp update, got %d", len(h.BarCache))
	}
	if h.BarCache[0].Close != 20012 {
		t.Errorf("Expected updated close 20012, got %.2f", h.BarCache[0].Close)
	}
}

func TestHub_AvailableAccounts(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.SelectedAccount = ""

	accounts := []string{"Sim101", "Playback101", "LiveAccount"}
	payload, _ := json.Marshal(accounts)

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "AVAILABLE_ACCOUNTS",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if len(h.State.AvailableAccounts) != 3 {
		t.Errorf("Expected 3 available accounts, got %d", len(h.State.AvailableAccounts))
	}
	if h.State.SelectedAccount == "" {
		t.Error("Expected SelectedAccount to be auto-populated")
	}
}

func TestHub_ForwardToNT8_AdapterType(t *testing.T) {
	h := NewHub()
	adapterClient := &Client{
		Hub:        h,
		Send:       make(chan []byte, 10),
		ClientType: "ADAPTER",
	}
	h.Clients[adapterClient] = true

	cmdPayload := []byte(`{"action":"FLATTEN"}`)
	h.ForwardToNT8("FLATTEN_POSITION", cmdPayload)

	select {
	case msgBytes := <-adapterClient.Send:
		var m risk.WSMessage
		json.Unmarshal(msgBytes, &m)
		if m.Type != "FLATTEN_POSITION" {
			t.Errorf("Expected FLATTEN_POSITION message forwarded to ADAPTER client, got %s", m.Type)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("Timed out waiting for message to be forwarded to ADAPTER client")
	}
}

func TestHub_UpdatePrices_InitialMarketPrice(t *testing.T) {
	h, _, _ := setupTestHub()
	// Zero out levels
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.IsLong = true

	initialMkt := 20500.00
	payload, _ := json.Marshal(map[string]interface{}{
		"currentMarketPrice": initialMkt,
	})

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: payload,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	// UPDATE_PRICES (Close[0]-derived) updates the live market reference but
	// must NOT seed the planning levels â€” chart closes can be stale or from a
	// different instrument than the live feed. ONLY live MARKET_DATA ticks seed
	// (see TestHub_MarketData_SeedsPlanningLevelsWhenFlat).
	if h.State.CurrentMarketPrice != 20500.00 {
		t.Errorf("Expected CurrentMarketPrice updated to 20500.00, got %.2f", h.State.CurrentMarketPrice)
	}
	if h.State.EntryPrice != 0 {
		t.Errorf("Expected EntryPrice NOT seeded from UPDATE_PRICES, got %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("Expected StopPrice NOT seeded from UPDATE_PRICES, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("Expected TargetPrice NOT seeded from UPDATE_PRICES, got %.2f", h.State.TargetPrice)
	}
}

// TestHub_BarsSync_NoSeedingFromBarClose: chart closes (SYNC_BARS / BAR_UPDATE)
// must never bootstrap the planning entry/SL/TP â€” they may be stale or from a
// different instrument than the live feed. Only MARKET_DATA ticks seed.
func TestHub_BarsSync_NoSeedingFromBarClose(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.IsLong = true

	// SYNC_BARS with a flat, uncommitted hub and a 20500 close.
	bars := []risk.ChartBar{
		{Time: 1000, Open: 20490, High: 20510, Low: 20480, Close: 20500, Volume: 100},
	}
	p, _ := json.Marshal(bars)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "SYNC_BARS", Payload: p})

	// BAR_UPDATE new bar with a far-away close (20100).
	bar := risk.ChartBar{Time: 1015, Open: 20050, High: 20150, Low: 20000, Close: 20100, Volume: 50}
	pb, _ := json.Marshal(bar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: pb})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 0 {
		t.Errorf("Bar data seeded entry (chart close may be stale): got %.2f, want 0", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("Bar data seeded stop: got %.2f, want 0", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("Bar data seeded target: got %.2f, want 0", h.State.TargetPrice)
	}
	if h.State.CurrentMarketPrice != 20100.00 {
		t.Errorf("Expected CurrentMarketPrice = last close 20100.00, got %.2f", h.State.CurrentMarketPrice)
	}
}

// TestHub_BarSync_DoesNotClobberLiveMarketRef is the regression for the
// drag-blocking bug: after live MARKET_DATA ticks arrive, chart closes (SYNC_BARS
// / BAR_UPDATE / UPDATE_PRICES) must NEVER replace the live market reference â€”
// a stale chart close used to poison the stale-price guard and reject the user's
// real SL/TP drags.
func TestHub_BarSync_DoesNotClobberLiveMarketRef(t *testing.T) {
	h, _, _ := setupTestHub()
	// Live tick first: authoritative reference is 29732.
	md := risk.MarketDataUpdate{Bid: 29732.00, Ask: 29733.00, Last: 29732.50}
	p, _ := json.Marshal(md)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "MARKET_DATA", Payload: p})

	// Now a STALE chart close arrives (29356) via every chart path.
	syncBars := []risk.ChartBar{{Time: 1000, Open: 29355, High: 29360, Low: 29340, Close: 29356, Volume: 1}}
	ps, _ := json.Marshal(syncBars)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "SYNC_BARS", Payload: ps})

	bar := risk.ChartBar{Time: 1015, Open: 29355, High: 29360, Low: 29340, Close: 29356, Volume: 1}
	pb, _ := json.Marshal(bar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: pb})

	upd, _ := json.Marshal(map[string]interface{}{"currentMarketPrice": 29356.25})
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: upd})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.CurrentMarketPrice != 29732.50 {
		t.Errorf("Chart close clobbered the live market reference: got %.2f, want 29732.50", h.State.CurrentMarketPrice)
	}
}

// TestHub_BarSync_ProvisionalBeforeLiveTick: before ANY live tick, chart closes
// are the only available reference and are used provisionally; the first live
// tick then takes over.
func TestHub_BarSync_ProvisionalBeforeLiveTick(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0

	bars := []risk.ChartBar{{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 1}}
	ps, _ := json.Marshal(bars)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "SYNC_BARS", Payload: ps})

	h.Mu.RLock()
	provisional := h.State.CurrentMarketPrice
	h.Mu.RUnlock()
	if provisional != 20005.00 {
		t.Fatalf("Expected provisional market ref from chart close 20005.00, got %.2f", provisional)
	}

	md := risk.MarketDataUpdate{Bid: 20100.00, Ask: 20100.50, Last: 20100.25}
	p, _ := json.Marshal(md)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "MARKET_DATA", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.CurrentMarketPrice != 20100.25 {
		t.Errorf("Live tick did not take over the market reference: got %.2f, want 20100.25", h.State.CurrentMarketPrice)
	}
}

func TestHub_BarUpdate_CacheCap1000(t *testing.T) {
	h, _, _ := setupTestHub()
	h.BarCache = make([]risk.ChartBar, 0, 1050)

	// Add 1005 bars sequentially
	for i := 0; i < 1005; i++ {
		bar := risk.ChartBar{
			Time:  int64(1000 + i*15),
			Open:  20000,
			High:  20010,
			Low:   19990,
			Close: 20005,
		}
		p, _ := json.Marshal(bar)
		h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
			Type:    "BAR_UPDATE",
			Payload: p,
		})
	}

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if len(h.BarCache) != 1000 {
		t.Errorf("Expected BarCache trimmed to exactly 1000 bars, got %d", len(h.BarCache))
	}
	// Last bar should have the latest timestamp: 1000 + 1004*15 = 16060
	if h.BarCache[len(h.BarCache)-1].Time != 16060 {
		t.Errorf("Expected last bar time 16060, got %d", h.BarCache[len(h.BarCache)-1].Time)
	}
}

func TestHub_ExecutionUpdate_NonGoEntryIgnored(t *testing.T) {
	h, nt8Client, _ := setupTestHub()

	// Fill report for an order named "ManualOrder" or "ExternalEntry"
	exec := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_ext_1",
		OrderId:     "ord_ext_1",
		Name:        "ManualOrder",
		Action:      "Buy",
		OrderState:  "Filled",
		FillQty:     2,
		FillPrice:   20000.00,
		AccountName: "Playback101",
	}
	p, _ := json.Marshal(exec)

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "EXECUTION_UPDATE",
		Payload: p,
	})

	// Wait 250ms (longer than 100ms debounce timer)
	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		json.Unmarshal(mb, &m)
		if m.Type == "SUBMIT_ORDER" {
			t.Errorf("Non-GoEntry fill triggered bracket dispatch unexpectedly: %s", string(mb))
		}
	case <-time.After(250 * time.Millisecond):
		// Expected: no brackets submitted for non-GoEntry fill
	}
}

func TestHub_AvailableAccountsPreference(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.SelectedAccount = ""

	// Save engine preference
	saveEnginePrefs(EnginePrefs{DefaultAccount: "Playback101"})

	accounts := []string{"Sim101", "Playback101", "Live1"}
	p, _ := json.Marshal(accounts)

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "AVAILABLE_ACCOUNTS",
		Payload: p,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	if h.State.SelectedAccount != "Playback101" {
		t.Errorf("Expected default preference 'Playback101' selected over first item, got '%s'", h.State.SelectedAccount)
	}
}

func TestHub_BarUpdate_SlLockedDoesNotMoveStop(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.IsSlLocked = true
	h.State.IsLong = true
	h.State.TrackOffsetTicks = 0
	h.State.TickSize = 0.25
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19950.00
	h.State.TargetPrice = 20100.00

	// Seed BarCache
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 20000, High: 20005, Low: 19995, Close: 20000},
	}

	// Bar 2 arrives (new timestamp) with High = 20010.00
	newBar := risk.ChartBar{
		Time:  1015,
		Open:  20000,
		High:  20010,
		Low:   19998,
		Close: 20008,
	}
	p, _ := json.Marshal(newBar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: p,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	// Entry should have moved to prior bar high (20005.00)
	if h.State.EntryPrice != 20005.00 {
		t.Errorf("Expected EntryPrice tracked to 20005.00, got %.2f", h.State.EntryPrice)
	}
	// StopPrice MUST remain strictly at 19950.00 because IsSlLocked is true!
	if h.State.StopPrice != 19950.00 {
		t.Errorf("Expected StopPrice to stay locked at 19950.00, got %.2f", h.State.StopPrice)
	}
}

func TestHub_NoResetOnMarketMove(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19950.00
	h.State.TargetPrice = 20100.00
	h.State.TickSize = 0.25
	h.State.IsLong = true

	// Market moves 100 points away (20100.00)
	bar := risk.ChartBar{
		Time:  2000,
		Open:  20100,
		High:  20110,
		Low:   20090,
		Close: 20100,
	}
	p, _ := json.Marshal(bar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: p,
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	// Entry, Stop, and Target MUST NOT be reset to bar.Close
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("Expected EntryPrice preserved at 20000.00, got %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19950.00 {
		t.Errorf("Expected StopPrice preserved at 19950.00, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20100.00 {
		t.Errorf("Expected TargetPrice preserved at 20100.00, got %.2f", h.State.TargetPrice)
	}
}

// ============================================================================
// Regression suite: "SL/TP (and entry) must NEVER move on bar close
// unless the user actually moves them" â€” even with AutoTrack enabled, once a
// trade is entered (position open OR a GoEntry order is working), bar closes
// must not shift the levels, and market/bar data must not re-anchor them.
// ============================================================================

// TestHub_BarUpdate_InPosition_AutoTrackDoesNotMoveLevels is the direct
// regression for "I'm in a position, AutoTrack shows on, and the bar closed â€”
// SL/TP must not move." Even with AutoTrack enabled, an open position freezes
// the levels forever.
func TestHub_BarUpdate_InPosition_AutoTrackDoesNotMoveLevels(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackOffsetTicks = 2
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	// Position is OPEN: the trade is entered.
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Price: 19980.00, Qty: 2, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", Price: 20040.00, Qty: 2, State: "Working"},
	}

	// Prior bar high 20010 â†’ AutoTrack would target entry 20010.50 if it ran.
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
	}
	newBar := risk.ChartBar{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 50}
	p, _ := json.Marshal(newBar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("EntryPrice moved on bar close while in position: got %.2f, want 20000.00", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19980.00 {
		t.Errorf("StopPrice moved on bar close while in position: got %.2f, want 19980.00", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20040.00 {
		t.Errorf("TargetPrice moved on bar close while in position: got %.2f, want 20040.00", h.State.TargetPrice)
	}
}

// TestHub_BarUpdate_WorkingEntry_AutoTrackDoesNotMoveLevels covers the
// submitâ†’fill race: the entry ORDER was submitted and is still working
// (position update hasn't arrived yet), AutoTrack is still flag-on â€” a bar
// close must still NOT shift the levels. This was the bug window where SL/TP
// jumped on candle close right after pressing the entry button.
func TestHub_BarUpdate_WorkingEntry_AutoTrackDoesNotMoveLevels(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackOffsetTicks = 2
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	// Entry order committed but NOT yet filled: position still flat.
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "g1", Name: "GoEntry", Price: 20000.00, Qty: 2, State: "Working"},
	}

	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
	}
	newBar := risk.ChartBar{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 50}
	p, _ := json.Marshal(newBar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("EntryPrice moved on bar close while entry order working: got %.2f, want 20000.00", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19980.00 {
		t.Errorf("StopPrice moved on bar close while entry order working: got %.2f, want 19980.00", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20040.00 {
		t.Errorf("TargetPrice moved on bar close while entry order working: got %.2f, want 20040.00", h.State.TargetPrice)
	}
}

// TestHub_InstantEntry_FreezesAutoTrackAndBarCloseKeepsLevels is the full
// end-to-end regression for the reported bug: AutoTrack on â†’ user presses the
// instant-entry hotkey â†’ AutoTrack must flip OFF immediately, and a subsequent
// bar close must leave Entry/SL/TP exactly where the user left them.
func TestHub_InstantEntry_FreezesAutoTrackAndBarCloseKeepsLevels(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackOffsetTicks = 2
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.CurrentBid = 20000.00
	h.State.CurrentAsk = 20000.25
	h.State.InstantEntryOffsetTicks = 1

	// Press the instant-entry hotkey: entry order is submitted.
	h.HandleHotkey("INSTANT_ENTRY")

	h.Mu.RLock()
	frozen := !h.State.IsAutoTrackEnabled
	entryAfterSubmit := h.State.EntryPrice
	stopAfterSubmit := h.State.StopPrice
	targetAfterSubmit := h.State.TargetPrice
	h.Mu.RUnlock()
	if !frozen {
		t.Fatal("AutoTrack was NOT disabled at entry submission")
	}
	if entryAfterSubmit <= 0 {
		t.Fatalf("Expected live entry price set by instant entry, got %.2f", entryAfterSubmit)
	}

	// A bar close now arrives BEFORE the fill / position update (the race).
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
	}
	newBar := risk.ChartBar{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 50}
	p, _ := json.Marshal(newBar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != entryAfterSubmit {
		t.Errorf("EntryPrice moved on bar close after entry submitted: got %.2f, want %.2f", h.State.EntryPrice, entryAfterSubmit)
	}
	if h.State.StopPrice != stopAfterSubmit {
		t.Errorf("StopPrice moved on bar close after entry submitted: got %.2f, want %.2f", h.State.StopPrice, stopAfterSubmit)
	}
	if h.State.TargetPrice != targetAfterSubmit {
		t.Errorf("TargetPrice moved on bar close after entry submitted: got %.2f, want %.2f", h.State.TargetPrice, targetAfterSubmit)
	}
}

// TestHub_ExecuteOrder_DisablesAutoTrack verifies the web EXECUTE button goes
// through the same freeze: AutoTrack is off the moment EXECUTE_ORDER lands.
func TestHub_ExecuteOrder_DisablesAutoTrack(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.CurrentMarketPrice = 20000.00

	payload := []byte(`{"action":"EXECUTE","direction":"LONG","orderType":"Market"}`)
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "EXECUTE_ORDER", Payload: payload})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.IsAutoTrackEnabled {
		t.Error("AutoTrack was NOT disabled by EXECUTE_ORDER")
	}
}

// TestHub_SetConfigAutoTrackEnable_WorkingEntry_DoesNotAnchor verifies that
// toggling AutoTrack ON while an entry order is committed must not re-anchor
// the levels to the prior bar.
func TestHub_SetConfigAutoTrackEnable_WorkingEntry_DoesNotAnchor(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.TrackOffsetTicks = 2
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "g1", Name: "GoEntry", Price: 20000.00, Qty: 2, State: "Working"},
	}
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
	}

	payload, _ := json.Marshal(map[string]interface{}{"isAutoTrackEnabled": true})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: payload})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("SET_CONFIG AutoTrack anchored entry to prior bar while entry working: got %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 19980.00 {
		t.Errorf("SET_CONFIG AutoTrack moved stop while entry working: got %.2f", h.State.StopPrice)
	}
}

// TestHub_SetConfig_AutoTrackEnable_WhileCommitted_Rejected verifies the
// guarantee "even if I turn AutoTrack ON after a trade is placed, it must not
// affect the trade": while a position is open OR an entry order is committed,
// the enable is REJECTED (flag stays false), so AutoTrack can never appear to
// be tracking an active trade.
func TestHub_SetConfig_AutoTrackEnable_WhileCommitted_Rejected(t *testing.T) {
	cases := []struct {
		name     string
		position risk.PositionInfo
		working  []risk.WorkingOrderInfo
	}{
		{
			name:     "position open",
			position: risk.PositionInfo{MarketPosition: "Long", Quantity: 2},
		},
		{
			name: "GoEntry working (unfilled)",
			working: []risk.WorkingOrderInfo{
				{OrderId: "g1", Name: "GoEntry", Price: 20000.00, Qty: 2, State: "Working"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := setupTestHub()
			h.State.Position = tc.position
			h.State.WorkingOrders = tc.working
			h.State.IsAutoTrackEnabled = false

			payload, _ := json.Marshal(map[string]interface{}{"isAutoTrackEnabled": true})
			h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: payload})

			h.Mu.RLock()
			defer h.Mu.RUnlock()
			if h.State.IsAutoTrackEnabled {
				t.Error("AutoTrack was enabled while a trade is committed â€” it must be rejected")
			}
		})
	}
}

// TestHub_SetConfig_AutoTrackEnable_Flat_Allowed confirms the enable is still
// allowed while planning (flat, no entry order), so pre-trade tracking works.
func TestHub_SetConfig_AutoTrackEnable_Flat_Allowed(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = false
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}

	payload, _ := json.Marshal(map[string]interface{}{"isAutoTrackEnabled": true})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: payload})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if !h.State.IsAutoTrackEnabled {
		t.Error("AutoTrack enable was rejected while flat (planning) â€” it must be allowed")
	}
}

// expectChangeOrder waits for a CHANGE_ORDER routed to the NT8 client and
// returns it; fails the test if nothing arrives within the timeout.
func expectChangeOrder(t *testing.T, nt8Client *Client) (orderId string, price float64) {
	t.Helper()
	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		json.Unmarshal(mb, &m)
		if m.Type != "CHANGE_ORDER" {
			t.Fatalf("Expected CHANGE_ORDER routed to NT8, got %s", m.Type)
		}
		var change struct {
			OrderId string  `json:"orderId"`
			Price   float64 `json:"price"`
		}
		json.Unmarshal(m.Payload, &change)
		return change.OrderId, change.Price
	case <-time.After(400 * time.Millisecond):
		t.Fatal("Timed out waiting for CHANGE_ORDER to be routed to NT8")
		return "", 0
	}
}

// expectNoChangeOrder asserts that NO CHANGE_ORDER reaches NT8 within the wait
// window (used for blocked/market-info cases).
func expectNoChangeOrder(t *testing.T, nt8Client *Client, wait time.Duration) {
	t.Helper()
	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		json.Unmarshal(mb, &m)
		t.Errorf("Unexpected message routed to NT8 (want no CHANGE_ORDER): %s %s", m.Type, string(m.Payload))
	case <-time.After(wait):
	}
}

// TestHub_UpdatePrices_StopDrag_InPosition_MovesActiveSL is the regression for
// "I can't move my SL/TP after entering a trade": dragging the stop in a
// position must route a CHANGE_ORDER to the ACTUAL working ActiveSL bracket â€”
// the red line IS the order.
func TestHub_UpdatePrices_StopDrag_InPosition_MovesActiveSL(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20000.00}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.CurrentBid = 20000.00
	h.State.CurrentAsk = 20000.50
	h.State.LastPrice = 20000.25
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Action: "SELL", OrderType: "StopMarket", Price: 19980.00, Qty: 2, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", Action: "SELL", OrderType: "Limit", Price: 20040.00, Qty: 2, State: "Working"},
	}

	payload, _ := json.Marshal(map[string]interface{}{"stopPrice": 19990.00})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	orderId, price := expectChangeOrder(t, nt8Client)
	if orderId != "sl1" || price != 19990.00 {
		t.Errorf("Expected CHANGE_ORDER for sl1 @ 19990.00, got %s @ %.2f", orderId, price)
	}
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.StopPrice != 19990.00 {
		t.Errorf("Expected hub StopPrice synced to 19990.00, got %.2f", h.State.StopPrice)
	}
}

// TestHub_UpdatePrices_TargetDrag_InPosition_MovesActiveTP is the same
// guarantee for the take-profit bracket.
func TestHub_UpdatePrices_TargetDrag_InPosition_MovesActiveTP(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20000.00}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.CurrentBid = 20000.00
	h.State.CurrentAsk = 20000.50
	h.State.LastPrice = 20000.25
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Action: "SELL", OrderType: "StopMarket", Price: 19980.00, Qty: 2, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", Action: "SELL", OrderType: "Limit", Price: 20040.00, Qty: 2, State: "Working"},
	}

	payload, _ := json.Marshal(map[string]interface{}{"targetPrice": 20060.00})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	orderId, price := expectChangeOrder(t, nt8Client)
	if orderId != "tp1" || price != 20060.00 {
		t.Errorf("Expected CHANGE_ORDER for tp1 @ 20060.00, got %s @ %.2f", orderId, price)
	}
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.TargetPrice != 20060.00 {
		t.Errorf("Expected hub TargetPrice synced to 20060.00, got %.2f", h.State.TargetPrice)
	}
}

// TestHub_UpdatePrices_StopDrag_InvalidSide_Clamped: a stop dragged through the
// market is clamped by Go to the nearest valid tick before NT8 sees it.
func TestHub_UpdatePrices_StopDrag_InvalidSide_Clamped(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20000.00}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.CurrentBid = 20000.00
	h.State.LastPrice = 20000.25
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Action: "SELL", OrderType: "StopMarket", Price: 19980.00, Qty: 2, State: "Working"},
	}

	// Sell stop dragged ABOVE the market (20050 >= bid 20000) â†’ clamp below bid.
	payload, _ := json.Marshal(map[string]interface{}{"stopPrice": 20050.00})
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	orderID, price := expectChangeOrder(t, nt8Client)
	if orderID != "sl1" || price != 19999.75 {
		t.Fatalf("Expected clamped CHANGE_ORDER sl1 @ 19999.75, got %s @ %.2f", orderID, price)
	}
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.StopPrice != 19999.75 {
		t.Fatalf("Expected canonical stop 19999.75, got %.2f", h.State.StopPrice)
	}
}

// TestHub_UpdatePrices_Nt8MarketInfo_NoBracketForward: connect-time market info
// from NT8 (no price patches) must never route a CHANGE_ORDER â€” this is the
// anti-auto-align guarantee that survives the drag fix.
func TestHub_UpdatePrices_Nt8MarketInfo_NoBracketForward(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20000.00}
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Action: "SELL", OrderType: "StopMarket", Price: 19980.00, Qty: 2, State: "Working"},
		{OrderId: "tp1", Name: "ActiveTP", Action: "SELL", OrderType: "Limit", Price: 20040.00, Qty: 2, State: "Working"},
	}

	payload, _ := json.Marshal(map[string]interface{}{"currentMarketPrice": 20100.00})
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	expectNoChangeOrder(t, nt8Client, 200*time.Millisecond)
}

// TestHub_BarUpdate_InPosition_NoReanchorFromClose: after a hub restart
// mid-trade, EntryPrice is 0 â€” a bar close must NOT bootstrap it (and SL/TP)
// from the close; the levels only come from the real order/position data.
func TestHub_BarUpdate_InPosition_NoReanchorFromClose(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.IsAutoTrackEnabled = false
	// Position is open (hub restarted mid-trade).
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20005.00}

	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 100},
	}
	newBar := risk.ChartBar{Time: 1015, Open: 20005, High: 20100, Low: 20000, Close: 20095, Volume: 50}
	p, _ := json.Marshal(newBar)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "BAR_UPDATE", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 0 {
		t.Errorf("EntryPrice was re-anchored from bar close while in position: got %.2f, want 0", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("StopPrice was re-anchored from bar close while in position: got %.2f, want 0", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("TargetPrice was re-anchored from bar close while in position: got %.2f, want 0", h.State.TargetPrice)
	}
}

// TestHub_SyncBars_InPosition_NoReanchorFromLastClose is the same guarantee
// for the historical-bars sync that runs on reconnect / restart.
func TestHub_SyncBars_InPosition_NoReanchorFromLastClose(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20005.00}

	bars := []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 100},
		{Time: 1015, Open: 20005, High: 20100, Low: 20000, Close: 20095, Volume: 50},
	}
	p, _ := json.Marshal(bars)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "SYNC_BARS", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 0 {
		t.Errorf("EntryPrice was re-anchored from SYNC_BARS while in position: got %.2f, want 0", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("StopPrice was re-anchored from SYNC_BARS while in position: got %.2f, want 0", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("TargetPrice was re-anchored from SYNC_BARS while in position: got %.2f, want 0", h.State.TargetPrice)
	}
}

// TestHub_UpdatePrices_InPosition_NoReanchor: NT8's UPDATE_PRICES
// (currentMarketPrice) must not bootstrap entry/SL/TP while in position.
func TestHub_UpdatePrices_InPosition_NoReanchor(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.IsLong = true
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20005.00}

	payload, _ := json.Marshal(map[string]interface{}{"currentMarketPrice": 20500.00})
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "UPDATE_PRICES", Payload: payload})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.CurrentMarketPrice != 20500.00 {
		t.Errorf("CurrentMarketPrice should still update to 20500.00, got %.2f", h.State.CurrentMarketPrice)
	}
	if h.State.EntryPrice != 0 {
		t.Errorf("EntryPrice was re-anchored from UPDATE_PRICES while in position: got %.2f, want 0", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("StopPrice was re-anchored from UPDATE_PRICES while in position: got %.2f, want 0", h.State.StopPrice)
	}
	if h.State.TargetPrice != 0 {
		t.Errorf("TargetPrice was re-anchored from UPDATE_PRICES while in position: got %.2f, want 0", h.State.TargetPrice)
	}
}

// TestHub_PositionUpdate_SetsEntryFromAveragePriceWhenMissing: when a position
// opens and the entry price is missing (hub restarted mid-trade), the entry
// line must come from the REAL average fill price â€” and must never overwrite a
// user-set entry price.
func TestHub_PositionUpdate_SetsEntryFromAveragePriceWhenMissing(t *testing.T) {
	// Case 1: entry missing â†’ restored from average fill price.
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	pos := risk.PositionInfo{MarketPosition: "Long", Quantity: 3, AveragePrice: 20010.50}
	p, _ := json.Marshal(pos)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "POSITION_UPDATE", Payload: p})

	h.Mu.RLock()
	if h.State.EntryPrice != 20010.50 {
		t.Errorf("Expected EntryPrice restored from average fill 20010.50, got %.2f", h.State.EntryPrice)
	}
	h.Mu.RUnlock()

	// Case 2: entry already set â†’ position update must NOT overwrite it.
	h2, _, _ := setupTestHub()
	h2.State.EntryPrice = 20000.00
	p2, _ := json.Marshal(pos)
	h2.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "POSITION_UPDATE", Payload: p2})

	h2.Mu.RLock()
	defer h2.Mu.RUnlock()
	if h2.State.EntryPrice != 20000.00 {
		t.Errorf("Position update overwrote user entry price: got %.2f, want 20000.00", h2.State.EntryPrice)
	}
}

// TestHub_MarketData_SeedsPlanningLevelsWhenFlat verifies the "lines show up
// near the price when I start the page" guarantee: the first live tick seeds
// the phantom entry/SL/TP around the current market (long: entry=last,
// stop=entry-20t, target=entry+20t*RR) while flat â€” no snap-to-market needed.
// Subsequent ticks must NOT move the seeded levels.
func TestHub_MarketData_SeedsPlanningLevelsWhenFlat(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.IsLong = true

	// First live tick (last trade = 29650.50)
	md := risk.MarketDataUpdate{Bid: 29650.00, Ask: 29650.75, Last: 29650.50}
	p, _ := json.Marshal(md)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "MARKET_DATA", Payload: p})

	h.Mu.RLock()
	entry := h.State.EntryPrice
	stop := h.State.StopPrice
	target := h.State.TargetPrice
	h.Mu.RUnlock()

	// slOffset = 20 * 0.25 = 5.0
	if entry != 29650.50 {
		t.Errorf("Expected entry seeded at last price 29650.50, got %.2f", entry)
	}
	if stop != 29645.50 {
		t.Errorf("Expected stop seeded at entry-5.00 = 29645.50, got %.2f", stop)
	}
	if target != 29660.50 {
		t.Errorf("Expected target seeded at entry+10.00 = 29660.50, got %.2f", target)
	}

	// A later tick must NOT re-seed or move the levels.
	md2 := risk.MarketDataUpdate{Bid: 29670.00, Ask: 29670.75, Last: 29670.50}
	p2, _ := json.Marshal(md2)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "MARKET_DATA", Payload: p2})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != entry {
		t.Errorf("Later tick moved the plan entry: got %.2f, want %.2f", h.State.EntryPrice, entry)
	}
	if h.State.StopPrice != stop {
		t.Errorf("Later tick moved the plan stop: got %.2f, want %.2f", h.State.StopPrice, stop)
	}
	if h.State.TargetPrice != target {
		t.Errorf("Later tick moved the plan target: got %.2f, want %.2f", h.State.TargetPrice, target)
	}
}

// TestHub_MarketData_NoSeedingWhileCommitted: once a trade is committed (position
// open OR GoEntry working), live ticks must NEVER seed/move the entry/SL/TP â€”
// the user's levels win, even if the hub restarted mid-trade and lost them.
func TestHub_MarketData_NoSeedingWhileCommitted(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.EntryPrice = 0
	h.State.StopPrice = 0
	h.State.TargetPrice = 0
	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.IsLong = true
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20005.00}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl1", Name: "ActiveSL", Price: 19980.00, Qty: 2, State: "Working"},
	}

	md := risk.MarketDataUpdate{Bid: 20000.00, Ask: 20000.75, Last: 20000.50}
	p, _ := json.Marshal(md)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{Type: "MARKET_DATA", Payload: p})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 0 {
		t.Errorf("Live tick seeded entry while committed: got %.2f, want 0 (levels come from orders)", h.State.EntryPrice)
	}
	if h.State.StopPrice != 0 {
		t.Errorf("Live tick seeded stop while committed: got %.2f, want 0", h.State.StopPrice)
	}
}

// riskChangePayload builds a SET_CONFIG patch that only changes riskCash.
func riskChangePayload(t *testing.T, risk float64) []byte {
	t.Helper()
	p, err := json.Marshal(map[string]interface{}{"riskCash": risk})
	if err != nil {
		t.Fatalf("marshal riskCash: %v", err)
	}
	return p
}

// TestHub_SetConfig_RiskCash_SlLocked_DoesNotMoveLevels is the regression for
// "SL lock is on, I change risk from 100 to 200, and the SL moved." With the SL
// locked, a risk change must NEVER move the stop (or the target â€” both are the
// user's committed levels); only the position size adapts.
func TestHub_SetConfig_RiskCash_SlLocked_DoesNotMoveLevels(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsSlLocked = true
	h.State.RiskCash = 100.0
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.TickSize = 0.25
	h.State.PointValue = 20.0
	h.State.SelectedRR = 2.0

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: riskChangePayload(t, 200.0)})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.StopPrice != 19980.00 {
		t.Errorf("SL moved despite IsSlLocked on risk change: got %.2f, want 19980.00", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20040.00 {
		t.Errorf("Target moved despite IsSlLocked on risk change: got %.2f, want 20040.00", h.State.TargetPrice)
	}
	if h.State.RiskCash != 200.0 {
		t.Errorf("RiskCash not applied: got %.2f, want 200.00", h.State.RiskCash)
	}
}

// TestHub_SetConfig_RiskCash_Unlocked_RecalibratesLevels verifies the intended
// behavior when the SL is NOT locked: changing risk recalibrates the stop
// distance (and the RR-scaled target) to keep the dollar risk exact.
func TestHub_SetConfig_RiskCash_Unlocked_RecalibratesLevels(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsSlLocked = false
	h.State.IsTpLocked = false
	h.State.RiskCash = 100.0
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00 // dist 20.00 â†’ risk/contract = 20*20 = 400
	h.State.TargetPrice = 20040.00
	h.State.TickSize = 0.25
	h.State.PointValue = 20.0
	h.State.SelectedRR = 2.0

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: riskChangePayload(t, 200.0)})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	// qty = 200/400 â†’ 0 â†’ clamped to 1; newSlDist = 200/(1*20) = 10.00
	if h.State.StopPrice != 19990.00 {
		t.Errorf("Expected stop recalibrated to 19990.00, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20020.00 {
		t.Errorf("Expected target recalibrated to 20020.00, got %.2f", h.State.TargetPrice)
	}
}

// TestHub_SetConfig_RiskCash_TpLocked_KeepsTarget: with the SL unlocked but the
// TP locked, a risk change recalibrates the stop but must leave the user's
// target exactly where it is â€” even across RecalculateState.
func TestHub_SetConfig_RiskCash_TpLocked_KeepsTarget(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsSlLocked = false
	h.State.IsTpLocked = true
	h.State.RiskCash = 100.0
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20060.00 // user-set fixed target (60 pts out)
	h.State.TickSize = 0.25
	h.State.PointValue = 20.0
	h.State.SelectedRR = 2.0

	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{Type: "SET_CONFIG", Payload: riskChangePayload(t, 200.0)})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.StopPrice != 19990.00 {
		t.Errorf("Expected stop recalibrated to 19990.00 (SL unlocked), got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20060.00 {
		t.Errorf("TP moved despite IsTpLocked on risk change: got %.2f, want 20060.00", h.State.TargetPrice)
	}
	if len(h.State.TargetExits) > 0 && h.State.TargetExits[len(h.State.TargetExits)-1].Price != 20060.00 {
		t.Errorf("Last target exit not preserved with locked TP: got %.2f", h.State.TargetExits[len(h.State.TargetExits)-1].Price)
	}
}

// TestHub_MultiTimeframeBarRouting verifies the per-timeframe bar caches: the
// 15s stream is always the engine's tracking stream when present (AutoTrack on
// the web's default execution timeframe), every other labeled stream only
// fills its own pane cache, and untagged (legacy gateway) bars keep the old
// single-stream behaviour.
func TestHub_MultiTimeframeBarRouting(t *testing.T) {
	h, _, _ := setupTestHub()

	syncPayload := func(bars []risk.ChartBar) []byte {
		b, _ := json.Marshal(bars)
		return b
	}
	barPayload := func(b risk.ChartBar) []byte {
		out, _ := json.Marshal(b)
		return out
	}

	// 1. First labeled SYNC ("100t") â†’ becomes the tracking stream.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: syncPayload([]risk.ChartBar{
			{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 10, Timeframe: "100t"},
			{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 20, Timeframe: "100t"},
		}),
	})

	h.Mu.RLock()
	if h.trackingTimeframe != "100t" {
		t.Errorf("Expected tracking timeframe '100t', got %q", h.trackingTimeframe)
	}
	if len(h.BarCache) != 2 {
		t.Errorf("Expected tracking BarCache of 2 bars, got %d", len(h.BarCache))
	}
	if len(h.TimeframeBars["100t"]) != 2 {
		t.Errorf("Expected TimeframeBars['100t'] mirrored to 2 bars, got %d", len(h.TimeframeBars["100t"]))
	}
	h.Mu.RUnlock()

	// 2. SYNC "15s" â†’ the 15s stream takes over tracking (prefer-15s rule);
	//    the 100t pane cache is untouched.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: syncPayload([]risk.ChartBar{
			{Time: 1000, Open: 19995, High: 20008, Low: 19990, Close: 20000, Volume: 5, Timeframe: "15s"},
			{Time: 1016, Open: 20000, High: 20012, Low: 19995, Close: 20008, Volume: 8, Timeframe: "15s"},
		}),
	})

	h.Mu.RLock()
	if h.trackingTimeframe != "15s" {
		t.Errorf("Expected tracking to prefer '15s', got %q", h.trackingTimeframe)
	}
	if len(h.BarCache) != 2 || h.BarCache[1].Close != 20008 {
		t.Errorf("Expected BarCache to now hold the 15s bars, got %d bars, last close %.2f", len(h.BarCache), h.BarCache[len(h.BarCache)-1].Close)
	}
	if len(h.TimeframeBars["15s"]) != 2 {
		t.Errorf("Expected TimeframeBars['15s'] mirrored to 2 bars, got %d", len(h.TimeframeBars["15s"]))
	}
	if len(h.TimeframeBars["100t"]) != 2 {
		t.Errorf("100t cache must survive the tracking switch, got %d bars", len(h.TimeframeBars["100t"]))
	}
	h.Mu.RUnlock()

	// 3. BAR_UPDATE "15s" (new bar) â†’ tracking stream: BarCache + mirror grow.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: barPayload(risk.ChartBar{Time: 1031, Open: 20008, High: 20020, Low: 20002, Close: 20018, Volume: 9, Timeframe: "15s"}),
	})

	h.Mu.RLock()
	if len(h.TimeframeBars["15s"]) != 3 {
		t.Errorf("Expected TimeframeBars['15s'] to grow to 3, got %d", len(h.TimeframeBars["15s"]))
	}
	if len(h.BarCache) != 3 || h.BarCache[2].Close != 20018 {
		t.Errorf("15s BAR_UPDATE must update tracking BarCache: got %d bars, last close %.2f", len(h.BarCache), h.BarCache[len(h.BarCache)-1].Close)
	}
	h.Mu.RUnlock()

	// 4. BAR_UPDATE "100t" (new bar) â†’ non-tracking pane cache only.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: barPayload(risk.ChartBar{Time: 1030, Open: 20012, High: 20025, Low: 20005, Close: 20022, Volume: 30, Timeframe: "100t"}),
	})

	h.Mu.RLock()
	if len(h.TimeframeBars["100t"]) != 3 {
		t.Errorf("100t BAR_UPDATE must grow its own cache: got %d bars", len(h.TimeframeBars["100t"]))
	}
	if len(h.BarCache) != 3 || h.BarCache[2].Close != 20018 {
		t.Errorf("100t BAR_UPDATE must not touch tracking BarCache: got %d bars, last close %.2f", len(h.BarCache), h.BarCache[len(h.BarCache)-1].Close)
	}
	h.Mu.RUnlock()
}

// TestHub_LegacyUntaggedStreamKeepsSingleCache ensures a gateway that has not
// been upgraded (untagged bars) keeps the exact legacy behaviour: everything
// lands in BarCache and no tracking label is adopted.
func TestHub_LegacyUntaggedStreamKeepsSingleCache(t *testing.T) {
	h, _, _ := setupTestHub()

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{
			{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 10},
			{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 20},
		}),
	})
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{Time: 1030, Open: 20012, High: 20022, Low: 20005, Close: 20020, Volume: 5}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.trackingTimeframe != "" {
		t.Errorf("Untagged stream must not adopt a tracking label, got %q", h.trackingTimeframe)
	}
	if len(h.BarCache) != 3 {
		t.Errorf("Expected all 3 legacy bars in BarCache, got %d", len(h.BarCache))
	}
	if len(h.TimeframeBars[""]) != 0 {
		t.Errorf("Untagged bars must never enter a labeled cache, got %d", len(h.TimeframeBars[""]))
	}
}

// TestHub_TickBarStream_IsolationAndOrder pins the bar-routing contract for
// tick/volume ("100t") streams. Regression context: when the NT8 gateway
// re-emitted a SINGLE forming tick bar under a new timestamp every second (the
// chopped-candle bug), the hub had to keep every emission inside the correct
// timeframe cache, in time order, without touching the tracking stream or any
// other timeframe â€” and the fixed gateway now emits one closed bar per
// rollover. This test locks in both behaviors.
func TestHub_TickBarStream_IsolationAndOrder(t *testing.T) {
	h, _, _ := setupTestHub()

	// Tracking stream (15s) first â†’ engine tracks 15s.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{
			{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 5, Timeframe: "15s"},
		}),
	})

	// Clean historical 100t sync.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{
			{Time: 2000, Open: 20010, High: 20020, Low: 20005, Close: 20015, Volume: 100, Timeframe: "100t"},
			{Time: 2010, Open: 20015, High: 20025, Low: 20010, Close: 20020, Volume: 100, Timeframe: "100t"},
		}),
	})

	// Fixed-gateway closed bar (volume resets to a small count â†’ new bar).
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{Time: 2020, Open: 20020, High: 20030, Low: 20015, Close: 20028, Volume: 100, Timeframe: "100t"}),
	})

	// A burst of OLD-style sliced emissions of ONE forming bar (same
	// open/high, rising volume, fresh timestamp each time) â€” the exact shape
	// of the reported bug.
	for i := 0; i < 6; i++ {
		h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
			Type: "BAR_UPDATE",
			Payload: mustJSON(t, risk.ChartBar{
				Time: 2030 + int64(i), Open: 20028, High: 20032, Low: 20022, Close: 20024, Volume: 20 + float64(i)*12, Timeframe: "100t",
			}),
		})
	}

	// A healthy next closed bar after the "slices".
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{Time: 2040, Open: 20024, High: 20034, Low: 20020, Close: 20030, Volume: 7, Timeframe: "100t"}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()

	// All 100t emissions land in TimeframeBars["100t"] only, in time order.
	bars := h.TimeframeBars["100t"]
	want := 2 + 1 + 6 + 1 // sync(2) + closed(1) + slices(6) + next(1)
	if len(bars) != want {
		t.Fatalf("Expected %d bars in TimeframeBars['100t'], got %d", want, len(bars))
	}
	for i := 1; i < len(bars); i++ {
		if bars[i].Time <= bars[i-1].Time {
			t.Errorf("TimeframeBars['100t'] not time-ascending at %d: %d <= %d", i, bars[i].Time, bars[i-1].Time)
		}
	}
	if bars[len(bars)-1].Close != 20030 {
		t.Errorf("Expected last 100t bar close 20030, got %.2f", bars[len(bars)-1].Close)
	}

	// Tracking stream untouched: still the single 15s bar.
	if h.trackingTimeframe != "15s" {
		t.Errorf("Tracking timeframe changed by 100t traffic: %q", h.trackingTimeframe)
	}
	if len(h.BarCache) != 1 || h.BarCache[0].Close != 20005 {
		t.Errorf("BarCache (tracking) must be untouched by 100t stream: got %d bars, last close %.2f", len(h.BarCache), h.BarCache[len(h.BarCache)-1].Close)
	}
	// No cross-timeframe leakage.
	if len(h.TimeframeBars["15s"]) != 1 {
		t.Errorf("15s cache must stay exactly the synced bar, got %d", len(h.TimeframeBars["15s"]))
	}
}

// TestHub_TrackTimeframeConfig verifies the engine's AutoTrack/planning series
// follows the user-configurable State.TrackTimeframe (e.g. tracking 1m bars
// even though panes might display other timeframes), and that the TrackAnchor
// "CurrentBarHighLow" anchors on the newest bar instead of the prior bar.
func TestHub_TrackTimeframeConfig(t *testing.T) {
	h, _, _ := setupTestHub()

	// Configure tracking on 1m.
	h.HandleMessage(&Client{ClientType: "WEB"}, risk.WSMessage{
		Type:    "SET_CONFIG",
		Payload: mustJSON(t, map[string]interface{}{"trackTimeframe": "1m"}),
	})
	h.Mu.RLock()
	if h.State.TrackTimeframe != "1m" {
		h.Mu.RUnlock()
		t.Fatalf("Expected TrackTimeframe '1m', got %q", h.State.TrackTimeframe)
	}
	h.Mu.RUnlock()

	// A 15s sync arrives first: NOT the configured tracking series.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{
			{Time: 1000, Open: 19990, High: 20000, Low: 19985, Close: 19995, Volume: 5, Timeframe: "15s"},
		}),
	})
	h.Mu.RLock()
	if len(h.TimeframeBars["15s"]) != 1 {
		h.Mu.RUnlock()
		t.Fatalf("15s stream should fill its own cache, got %d", len(h.TimeframeBars["15s"]))
	}
	h.Mu.RUnlock()

	// The 1m sync becomes the tracking stream.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{
			{Time: 2000, Open: 20000, High: 20010, Low: 19990, Close: 20005, Volume: 5, Timeframe: "1m"},
		}),
	})
	h.Mu.RLock()
	if h.trackingTimeframe != "1m" {
		h.Mu.RUnlock()
		t.Fatalf("Expected tracking to follow TrackTimeframe '1m', got %q", h.trackingTimeframe)
	}
	if len(h.BarCache) != 1 || h.BarCache[0].High != 20010 {
		h.Mu.RUnlock()
		t.Fatalf("BarCache should hold the 1m bars, got %d bars", len(h.BarCache))
	}
	h.Mu.RUnlock()

	// CurrentBarHighLow anchor: AutoTrack re-anchors on the NEWEST bar.
	h.State.IsAutoTrackEnabled = true
	h.State.TrackAnchor = "CurrentBarHighLow"
	h.State.IsLong = true
	h.State.TickSize = 0.25
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.State.SelectedRR = 2.0

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{
			Time: 2060, Open: 20005, High: 20020, Low: 20000, Close: 20018, Volume: 100, Timeframe: "1m",
		}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	// Newest bar high = 20020 (+offset 0) â†’ entry should re-anchor to 20020.
	if h.State.EntryPrice != 20020.00 {
		t.Errorf("CurrentBarHighLow anchor: expected EntryPrice 20020.00, got %.2f", h.State.EntryPrice)
	}
	// Delta = +20 â†’ stop moves to 20000, target to 20060.
	if h.State.StopPrice != 20000.00 {
		t.Errorf("Expected stop shifted to 20000.00, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20060.00 {
		t.Errorf("Expected target shifted to 20060.00, got %.2f", h.State.TargetPrice)
	}
}

// TestHub_TrackAnchor_PriorHighLow pins the default anchor still uses the
// PRIOR (closed) bar, not the newest.
func TestHub_TrackAnchor_PriorHighLow(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackAnchor = "PriorHighLow"
	h.State.IsLong = true
	h.State.TickSize = 0.25
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19990, High: 20010, Low: 19985, Close: 20005, Volume: 5},
		{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 8},
	}

	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{ // new bar closes â†’ AutoTrack runs
			Time: 1030, Open: 20012, High: 20020, Low: 20005, Close: 20018, Volume: 100,
		}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	// Prior bar high = 20015 (+0 offset) â†’ new entry 20015.
	if h.State.EntryPrice != 20015.00 {
		t.Errorf("PriorHighLow anchor: expected EntryPrice 20015.00, got %.2f", h.State.EntryPrice)
	}
}

// TestHub_FlatGoEntryDrag_StopPricedThrough_Clamped pins the flat entry rule:
// Go clamps a stop entry to a valid tick before changing the NT8 order.
func TestHub_FlatGoEntryDrag_StopPricedThrough_Clamped(t *testing.T) {
	h, _, _ := setupTestHub()
	webClient := &Client{Send: make(chan []byte, 32), ClientType: "WEB"}
	nt8Client := &Client{Send: make(chan []byte, 32), ClientType: "NT8"}
	h.Clients[webClient] = true
	h.Clients[nt8Client] = true

	h.State.TickSize = 0.25
	h.State.CurrentAsk = 20000.25 // buy-side reference for the guard
	h.State.CurrentMarketPrice = 20000.00
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "e1", Name: "GoEntry", OrderType: "StopLimit", Action: "BUY", Price: 20010.00, Qty: 1, State: "Working"},
	}

	// Drag the entry DOWN across the market (19990 < ask) â†’ clamp above ask.
	h.HandleMessage(webClient, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: mustJSON(t, map[string]interface{}{"entryPrice": 19990.00}),
	})
	orderID, price := expectChangeOrder(t, nt8Client)
	if orderID != "e1" || price != 20000.50 {
		t.Fatalf("Expected clamped CHANGE_ORDER e1 @ 20000.50, got %s @ %.2f", orderID, price)
	}

	// Drag the entry UP (still above the ask) â†’ change goes through.
	h.HandleMessage(webClient, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: mustJSON(t, map[string]interface{}{"entryPrice": 20015.00}),
	})
	select {
	case msg := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		json.Unmarshal(msg, &wsMsg)
		if wsMsg.Type != "CHANGE_ORDER" {
			t.Fatalf("Expected CHANGE_ORDER, got %s", wsMsg.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Expected CHANGE_ORDER for a valid stop-entry drag")
	}
}

// TestHub_FlatLimitEntryDrag_Unrestricted pins that a resting LIMIT entry can
// be dragged anywhere (limits are never priced-through).
func TestHub_FlatLimitEntryDrag_Unrestricted(t *testing.T) {
	h, _, _ := setupTestHub()
	webClient := &Client{Send: make(chan []byte, 32), ClientType: "WEB"}
	nt8Client := &Client{Send: make(chan []byte, 32), ClientType: "NT8"}
	h.Clients[webClient] = true
	h.Clients[nt8Client] = true

	h.State.TickSize = 0.25
	h.State.CurrentAsk = 20000.25
	h.State.CurrentMarketPrice = 20000.00
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "e1", Name: "GoEntry", OrderType: "Limit", Action: "BUY", Price: 20000.00, Qty: 1, State: "Working"},
	}

	// Drag a limit entry below the market â†’ change MUST go through.
	h.HandleMessage(webClient, risk.WSMessage{
		Type:    "UPDATE_PRICES",
		Payload: mustJSON(t, map[string]interface{}{"entryPrice": 19990.00}),
	})
	select {
	case msg := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		json.Unmarshal(msg, &wsMsg)
		if wsMsg.Type != "CHANGE_ORDER" {
			t.Fatalf("Expected CHANGE_ORDER for limit entry drag, got %s", wsMsg.Type)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Expected CHANGE_ORDER for a limit entry drag")
	}
}

// TestHub_StopDrag_PreservesDrawnTarget pins the "lines are independent"
// behavior: dragging ONLY the stop (patch carries the user's target unchanged)
// must NOT let RecalculateState re-derive the target from the new stop
// distance Ã— R:R â€” the drawn target stays exactly where the user put it.
func TestHub_StopDrag_PreservesDrawnTarget(t *testing.T) {
	h, _, _ := setupTestHub()
	webClient := &Client{Send: make(chan []byte, 32), ClientType: "WEB"}
	nt8Client := &Client{Send: make(chan []byte, 32), ClientType: "NT8"}
	h.Clients[webClient] = true
	h.Clients[nt8Client] = true

	h.State.TickSize = 0.25
	h.State.SelectedRR = 2.0
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19995.00
	h.State.TargetPrice = 20030.00
	h.State.CurrentMarketPrice = 20000.00
	h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}

	// Drag the stop to 19990 (new slDist = 10). The web sends the drawn target
	// along unchanged; recalc would derive target = 20000 + 2Ã—10 = 20020, but
	// the drawn line (20030) must win.
	h.HandleMessage(webClient, risk.WSMessage{
		Type: "UPDATE_PRICES",
		Payload: mustJSON(t, map[string]interface{}{
			"entryPrice":  20000.00,
			"stopPrice":   19990.00,
			"targetPrice": 20030.00,
		}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.StopPrice != 19990.00 {
		t.Errorf("Expected stop dragged to 19990.00, got %.2f", h.State.StopPrice)
	}
	if h.State.TargetPrice != 20030.00 {
		t.Errorf("Drawn target must NOT be re-derived by the stop drag: expected 20030.00, got %.2f", h.State.TargetPrice)
	}
	if !h.State.HasCustomTargets {
		t.Errorf("Expected HasCustomTargets=true after a user-drawn target patch")
	}
}

// TestHub_AutoTrack_CurrentBarHighLow_LiveUpdate pins the fix for the
// "2 minutes ago high" complaint: with the CurrentBarHighLow anchor, AutoTrack
// must re-anchor on LIVE forming-bar updates (same timestamp, rising high) â€”
// not only on new-bar closes â€” so the phantom entry follows the current bar's
// high in real time.
func TestHub_AutoTrack_CurrentBarHighLow_LiveUpdate(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackAnchor = "CurrentBarHighLow"
	h.State.IsLong = true
	h.State.TickSize = 0.25
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19995, High: 20010, Low: 19990, Close: 20005, Volume: 5},
	}

	// Forming-bar update: SAME timestamp, high rises 20010 â†’ 20030.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{
			Time: 1000, Open: 19995, High: 20030, Low: 19990, Close: 20025, Volume: 8,
		}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20030.00 {
		t.Errorf("CurrentBarHighLow live update: expected EntryPrice 20030.00, got %.2f", h.State.EntryPrice)
	}
	if h.State.StopPrice != 20010.00 {
		t.Errorf("Expected stop shifted +30 to 20010.00, got %.2f", h.State.StopPrice)
	}
}

// TestHub_AutoTrack_PriorHighLow_IgnoresLiveUpdate pins that the prior-bar
// anchor is close-driven: live same-timestamp updates must NOT move it.
func TestHub_AutoTrack_PriorHighLow_IgnoresLiveUpdate(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackAnchor = "PriorHighLow"
	h.State.IsLong = true
	h.State.TickSize = 0.25
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19995, High: 20010, Low: 19990, Close: 20005, Volume: 5},
		{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012, Volume: 8},
	}

	// Forming-bar update (same timestamp as last bar) â€” prior bar unchanged.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type: "BAR_UPDATE",
		Payload: mustJSON(t, risk.ChartBar{
			Time: 1015, Open: 20005, High: 20018, Low: 20000, Close: 20016, Volume: 9,
		}),
	})

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20000.00 {
		t.Errorf("PriorHighLow must ignore live updates: entry moved to %.2f", h.State.EntryPrice)
	}
}

// TestHub_AutoTrack_RespectsTrackTimeframe verifies that AutoTrack anchors on the
// user's configured TrackTimeframe rather than being hardcoded to 15s BarCache.
func TestHub_AutoTrack_RespectsTrackTimeframe(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.TrackAnchor = "PriorHighLow"
	h.State.TrackTimeframe = "1m"
	h.State.TrackOffsetTicks = 0
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20040.00

	// 15s primary cache has a prior bar with High 20010
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 20000, High: 20010, Low: 19990, Close: 20005},
		{Time: 1015, Open: 20005, High: 20015, Low: 20000, Close: 20012},
	}
	// 1m tracking cache has a prior bar with High 20050
	h.TimeframeBars["1m"] = []risk.ChartBar{
		{Time: 960, Open: 20020, High: 20050, Low: 20015, Close: 20045},
		{Time: 1020, Open: 20045, High: 20055, Low: 20040, Close: 20052},
	}

	h.Mu.Lock()
	h.reanchorAutoTrack()
	h.Mu.Unlock()

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.EntryPrice != 20050.00 {
		t.Errorf("AutoTrack should anchor to 1m prior High (20050.00), got %.2f", h.State.EntryPrice)
	}
}

// TestHub_AutoTrack_PreservesLockedTP verifies that when IsTpLocked is true,
// AutoTrack moves Entry and SL but keeps TargetPrice and TargetExits frozen.
func TestHub_AutoTrack_PreservesLockedTP(t *testing.T) {
	h, _, _ := setupTestHub()
	h.State.IsAutoTrackEnabled = true
	h.State.IsTpLocked = true
	h.State.IsSlLocked = false
	h.State.TrackAnchor = "PriorHighLow"
	h.State.TrackOffsetTicks = 0
	h.State.TickSize = 0.25
	h.State.IsLong = true
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19980.00
	h.State.TargetPrice = 20080.00 // locked target
	h.State.TargetExits = []risk.TargetExit{
		{Qty: 1, Ratio: 4.0, Price: 20080.00},
	}

	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 20000, High: 20020, Low: 19990, Close: 20015},
		{Time: 1015, Open: 20015, High: 20025, Low: 20010, Close: 20022},
	}

	h.Mu.Lock()
	h.reanchorAutoTrack()
	h.Mu.Unlock()

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	// Entry moved +20 to 20020.00
	if h.State.EntryPrice != 20020.00 {
		t.Errorf("Expected EntryPrice 20020.00, got %.2f", h.State.EntryPrice)
	}
	// Stop moved +20 to 20000.00
	if h.State.StopPrice != 20000.00 {
		t.Errorf("Expected StopPrice 20000.00, got %.2f", h.State.StopPrice)
	}
	// Target should remain locked at 20080.00
	if h.State.TargetPrice != 20080.00 {
		t.Errorf("Locked TargetPrice should remain 20080.00, got %.2f", h.State.TargetPrice)
	}
}

// TestHotkey_ScaleOut_AskBidMode verifies that ScaleOut in AskBid mode prices off live bid/ask.
func TestHotkey_ScaleOut_AskBidMode(t *testing.T) {
	h, nt8Client := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 4, AveragePrice: 20000}
	h.State.ScaleOutPercent = 50.0
	h.State.ScaleOutPriceMode = "AskBid"
	h.State.CurrentBid = 20015.25
	h.State.CurrentAsk = 20015.50
	h.State.CurrentMarketPrice = 20015.25
	h.State.LastPrice = 20015.25
	h.State.TickSize = 0.25

	h.HandleHotkey("SCALE_OUT")

	select {
	case mb := <-nt8Client.Send:
		var m risk.WSMessage
		if err := json.Unmarshal(mb, &m); err != nil {
			t.Fatalf("Failed unmarshaling: %v", err)
		}
		if m.Type != "SUBMIT_ORDER" {
			t.Fatalf("Expected SUBMIT_ORDER, got %s", m.Type)
		}
		var cmd risk.SingleOrderCmd
		if err := json.Unmarshal(m.Payload, &cmd); err != nil {
			t.Fatalf("Failed unmarshaling order: %v", err)
		}
		// In AskBid mode for Long position, limitPrice should be usableBid = 20015.25
		if cmd.LimitPrice != 20015.25 {
			t.Errorf("Expected limitPrice 20015.25 from live bid, got %.2f", cmd.LimitPrice)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timed out waiting for ScaleOut SUBMIT_ORDER")
	}
}

// TestHotkey_TrailStop_CurrentBarHighLow verifies Trail Stop can anchor to CurrentBarHighLow.
func TestHotkey_TrailStop_CurrentBarHighLow(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.Position = risk.PositionInfo{MarketPosition: "Long", Quantity: 2, AveragePrice: 20000}
	h.State.TrackAnchor = "CurrentBarHighLow"
	h.State.TrailStopOffsetTicks = 1
	h.State.TickSize = 0.25
	h.State.StopPrice = 19980.00
	h.BarCache = []risk.ChartBar{
		{Time: 1000, Open: 19985, High: 20005, Low: 19985, Close: 20000},
		{Time: 1015, Open: 20000, High: 20025, Low: 19998, Close: 20020}, // forming bar low = 19998
	}

	h.HandleHotkey("TRAIL_STOP")

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	// Forming bar Low is 19998.00. Offset is 1 tick (0.25). New stop = 19997.75
	if h.State.StopPrice != 19997.75 {
		t.Errorf("Expected Trail Stop at 19997.75 (forming bar low - 1 tick), got %.2f", h.State.StopPrice)
	}
}

// TestHotkey_SwapDirection_PreservesCustomTargets verifies swapping direction keeps HasCustomTargets true
// and mirrors existing TargetExits symmetrically.
func TestHotkey_SwapDirection_PreservesCustomTargets(t *testing.T) {
	h, _ := setupHotkeyHub()
	h.State.IsLong = true
	h.State.IsPartialProfit = true
	h.State.CalculatedQty = 2
	h.State.EntryPrice = 20000.00
	h.State.StopPrice = 19990.00 // slDist = 10 pts
	h.State.TargetPrice = 20030.00
	h.State.HasCustomTargets = true
	h.State.TargetExits = []risk.TargetExit{
		{Qty: 1, Ratio: 1.5, Price: 20015.00},
		{Qty: 1, Ratio: 3.0, Price: 20030.00},
	}

	h.HandleHotkey("SWAP_DIRECTION")

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.IsLong {
		t.Errorf("Expected IsLong=false after swap")
	}
	if !h.State.HasCustomTargets {
		t.Errorf("Expected HasCustomTargets to remain true after swap")
	}
	if h.State.StopPrice != 20010.00 {
		t.Errorf("Expected mirrored StopPrice 20010.00, got %.2f", h.State.StopPrice)
	}
	if len(h.State.TargetExits) != 2 {
		t.Fatalf("Expected 2 TargetExits, got %d", len(h.State.TargetExits))
	}
	if h.State.TargetExits[0].Price != 19985.00 {
		t.Errorf("Expected first exit mirrored to 19985.00, got %.2f", h.State.TargetExits[0].Price)
	}
	if h.State.TargetExits[1].Price != 19970.00 {
		t.Errorf("Expected second exit mirrored to 19970.00, got %.2f", h.State.TargetExits[1].Price)
	}
}

// TestHub_SyncBars_KeepsSameTimeDistinctBars pins the "mirror NT8 exactly"
// rule: same-time bars with DIFFERENT values (NT8's end-of-session 4:59:59
// pair â€” a real bar plus a tiny closing bar at the same timestamp) are BOTH
// kept, while identical re-syncs collapse to a single copy.
func TestHub_SyncBars_KeepsSameTimeDistinctBars(t *testing.T) {
	h, _, _ := setupTestHub()
	// Seed the 15s tracking stream first so 100t lands in its own cache (like
	// real usage â€” 15s is the default TrackTimeframe).
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{{Time: 1000, Open: 29500, High: 29510, Low: 29490, Close: 29505, Volume: 5, Timeframe: "15s"}}),
	})

	// NT8 closes the session with two bars at the same time but different data.
	realLast := risk.ChartBar{Time: 2000, Open: 29513, High: 29516, Low: 29512, Close: 29510.25, Volume: 131, Timeframe: "100t"}
	tinyEnd := risk.ChartBar{Time: 2000, Open: 29512, High: 29513, Low: 29509, Close: 29509.5, Volume: 6, Timeframe: "100t"}
	prior := risk.ChartBar{Time: 1900, Open: 29513, High: 29515, Low: 29512, Close: 29513, Volume: 123, Timeframe: "100t"}
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{prior, realLast, tinyEnd}),
	})

	h.Mu.RLock()
	if len(h.TimeframeBars["100t"]) != 3 {
		h.Mu.RUnlock()
		t.Fatalf("Same-time distinct bars must BOTH be kept + prior: got %d bars", len(h.TimeframeBars["100t"]))
	}
	bars := h.TimeframeBars["100t"]
	// Order: [prior(t1900), realLast(t2000), tinyEnd(t2000)]
	if bars[1].Close != 29510.25 || bars[2].Close != 29509.5 {
		h.Mu.RUnlock()
		t.Errorf("Expected both end-of-session bars preserved in order, got closes %.2f / %.2f", bars[1].Close, bars[2].Close)
	}
	h.Mu.RUnlock()

	// Re-sending the identical batch must collapse to the same 3 (no pile-up).
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{prior, realLast, tinyEnd, prior}),
	})
	h.Mu.RLock()
	if len(h.TimeframeBars["100t"]) != 3 {
		h.Mu.RUnlock()
		t.Errorf("Identical re-sync must collapse to 3 bars, got %d", len(h.TimeframeBars["100t"]))
	}
	h.Mu.RUnlock()
}

// TestHub_SyncBars_KeepsMidSessionSameSecondBars: same-timestamp bars are not
// just a session-close artifact â€” fast 100t markets close multiple bars in one
// second (NT8 shows e.g. THREE candles at 3:59:20). All must be kept.
func TestHub_SyncBars_KeepsMidSessionSameSecondBars(t *testing.T) {
	h, _, _ := setupTestHub()
	// Seed 15s tracking first so 100t routes to its own cache.
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, []risk.ChartBar{{Time: 1000, Open: 29500, High: 29510, Low: 29490, Close: 29505, Volume: 5, Timeframe: "15s"}}),
	})
	batch := []risk.ChartBar{
		{Time: 1000, Open: 29500, High: 29504, Low: 29499, Close: 29503, Volume: 100, Timeframe: "100t"},
		{Time: 1000, Open: 29503, High: 29506, Low: 29502, Close: 29505, Volume: 100, Timeframe: "100t"},
		{Time: 1000, Open: 29505, High: 29508, Low: 29504, Close: 29507, Volume: 100, Timeframe: "100t"},
		{Time: 1001, Open: 29507, High: 29509, Low: 29506, Close: 29508, Volume: 100, Timeframe: "100t"},
	}
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, batch),
	})
	h.Mu.RLock()
	bars := h.TimeframeBars["100t"]
	if len(bars) != 4 {
		h.Mu.RUnlock()
		t.Fatalf("All three same-second bars must be kept + the next bar: got %d", len(bars))
	}
	if bars[0].Time != 1000 || bars[1].Time != 1000 || bars[2].Time != 1000 {
		h.Mu.RUnlock()
		t.Errorf("Same-second cluster must be intact at the front, got times %d %d %d", bars[0].Time, bars[1].Time, bars[2].Time)
	}
	h.Mu.RUnlock()
	// Identical repeats within a re-sync collapse to a single copy.
	batch2 := append(append([]risk.ChartBar{}, batch...), batch[0])
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: mustJSON(t, batch2),
	})
	h.Mu.RLock()
	if len(h.TimeframeBars["100t"]) != 4 {
		h.Mu.RUnlock()
		t.Errorf("Re-sync with one identical repeat must stay at 4 bars, got %d", len(h.TimeframeBars["100t"]))
	}
	h.Mu.RUnlock()
}

// TestHub_BarUpdate_FormingCandle_UpsertsByBarIndex pins the 100t live-candle
// rule: the gateway now re-emits the FORMING candle on every tick with the
// SAME BarIndex. The hub must REPLACE that index in place (one live candle â€”
// this was the "100t chart lags until close" symptom when we only emitted
// closed bars) while genuinely distinct closed bars (different indices) append,
// even in the same second. Bars without an index keep the legacy merge rule.
func TestHub_BarUpdate_FormingCandle_UpsertsByBarIndex(t *testing.T) {
	h, _, _ := setupTestHub()
	h.trackingTimeframe = "15s" // 100t is a non-tracking pane

	sendBar := func(b risk.ChartBar) {
		h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
			Type:    "BAR_UPDATE",
			Payload: mustJSON(t, b),
		})
	}

	// Forming candle index 100 re-emitted on every tick (rising volume).
	sendBar(risk.ChartBar{Time: 1783915200, Open: 29700, High: 29710, Low: 29695, Close: 29698, Volume: 40, BarIndex: 100, Timeframe: "100t"})
	sendBar(risk.ChartBar{Time: 1783915200, Open: 29700, High: 29711, Low: 29695, Close: 29700, Volume: 60, BarIndex: 100, Timeframe: "100t"})
	sendBar(risk.ChartBar{Time: 1783915200, Open: 29700, High: 29712, Low: 29695, Close: 29703, Volume: 80, BarIndex: 100, Timeframe: "100t"})

	h.Mu.RLock()
	if got := len(h.TimeframeBars["100t"]); got != 1 {
		h.Mu.RUnlock()
		t.Fatalf("forming re-emissions (same BarIndex) must stay ONE live candle, got %d", got)
	}
	last := h.TimeframeBars["100t"][0]
	h.Mu.RUnlock()
	if last.Volume != 80 || last.High != 29712 || last.Close != 29703 {
		t.Errorf("forming candle must carry the LAST tick's values, got v=%v h=%v c=%v", last.Volume, last.High, last.Close)
	}

	// A distinct closed bar closes in the SAME second (different BarIndex) â†’ append.
	sendBar(risk.ChartBar{Time: 1783915200, Open: 29705, High: 29715, Low: 29695, Close: 29710, Volume: 100, BarIndex: 101, Timeframe: "100t"})

	h.Mu.RLock()
	if got := len(h.TimeframeBars["100t"]); got != 2 {
		h.Mu.RUnlock()
		t.Fatalf("same-second distinct closed bar (new BarIndex) must append, got %d", got)
	}
	h.Mu.RUnlock()

	// Forming candle index 102 opens â†’ appended as the new live candle.
	sendBar(risk.ChartBar{Time: 1783915201, Open: 29710, High: 29712, Low: 29705, Close: 29711, Volume: 12, BarIndex: 102, Timeframe: "100t"})
	h.Mu.RLock()
	if got := len(h.TimeframeBars["100t"]); got != 3 {
		h.Mu.RUnlock()
		t.Fatalf("new forming index must append, got %d casino bars", got)
	}
	h.Mu.RUnlock()

	// Stale out-of-order frame for an older index must NOT resurrect/overwrite.
	sendBar(risk.ChartBar{Time: 1783915199, Open: 29700, High: 29730, Low: 29690, Close: 29720, Volume: 150, BarIndex: 100, Timeframe: "100t"})
	h.Mu.RLock()
	first := h.TimeframeBars["100t"][0]
	got := len(h.TimeframeBars["100t"])
	h.Mu.RUnlock()
	if got != 3 || first.Volume != 80 {
		t.Errorf("stale older-time frame with the same index must be dropped (got %d bars, first V=%v)", got, first.Volume)
	}
}

// TestHub_PositionUpdate_SyncsBracketQuantities verifies that when position is 1 contract
// but an ActiveSL order has quantity 2 (e.g. from sizing or duplicate brackets),
// receiving POSITION_UPDATE automatically shrinks the bracket order quantity to match position.
func TestHub_PositionUpdate_SyncsBracketQuantities(t *testing.T) {
	h, nt8Client, _ := setupTestHub()
	h.State.SelectedAccount = "Playback101"
	h.State.InstrumentName = "NQ"
	h.State.WorkingOrders = []risk.WorkingOrderInfo{
		{OrderId: "sl_1", Name: "ActiveSL", Price: 19980.00, Qty: 2, State: "Working"},
	}

	// Position update arrives indicating actual broker position is only 1 contract
	posPayload := []byte(`{"marketPosition":"Long","quantity":1,"averagePrice":20000.00,"accountName":"Playback101","instrumentName":"NQ"}`)
	h.HandleMessage(&Client{ClientType: "NT8"}, risk.WSMessage{
		Type:    "POSITION_UPDATE",
		Payload: posPayload,
	})

	select {
	case msgBytes := <-nt8Client.Send:
		var wsMsg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &wsMsg); err != nil || wsMsg.Type != "CHANGE_ORDER" {
			t.Fatalf("Expected CHANGE_ORDER to resize bracket, got %s", string(msgBytes))
		}
		var changeCmd struct {
			OrderId string `json:"orderId"`
			Qty     int    `json:"qty"`
		}
		json.Unmarshal(wsMsg.Payload, &changeCmd)
		if changeCmd.OrderId != "sl_1" || changeCmd.Qty != 1 {
			t.Errorf("Expected CHANGE_ORDER OrderId=sl_1 Qty=1, got OrderId=%s Qty=%d", changeCmd.OrderId, changeCmd.Qty)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Timed out waiting for CHANGE_ORDER to resize bracket")
	}
}
