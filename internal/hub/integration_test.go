package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trade-engine-without-chart/internal/risk"

	"github.com/gorilla/websocket"
)

// setupIntegrationServer creates a real HTTP test server running Hub and returns
// the Hub instance, server, adapter WS connection, and web WS connection.
func setupIntegrationServer(t *testing.T) (*Hub, *httptest.Server, *websocket.Conn, *websocket.Conn) {
	h := NewHub()
	go h.Run()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeWs(h, w, r)
	}))

	baseURL := "ws" + strings.TrimPrefix(server.URL, "http")

	// Connect Adapter client
	adapterConn, _, err := websocket.DefaultDialer.Dial(baseURL+"?type=ADAPTER", nil)
	if err != nil {
		server.Close()
		t.Fatalf("Failed to connect ADAPTER WS client: %v", err)
	}

	// Connect Web client
	webConn, _, err := websocket.DefaultDialer.Dial(baseURL+"?type=WEB", nil)
	if err != nil {
		adapterConn.Close()
		server.Close()
		t.Fatalf("Failed to connect WEB WS client: %v", err)
	}

	// Allow registration to process
	time.Sleep(50 * time.Millisecond)

	return h, server, adapterConn, webConn
}

// readNextWSMessage reads the next JSON WSMessage from a WebSocket connection with a timeout.
func readNextWSMessage(t *testing.T, conn *websocket.Conn, timeout time.Duration) *risk.WSMessage {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, msgBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("Failed to read WS message: %v", err)
		return nil
	}
	var msg risk.WSMessage
	if err := json.Unmarshal(msgBytes, &msg); err != nil {
		t.Fatalf("Failed to unmarshal WSMessage: %v (raw: %s)", err, string(msgBytes))
		return nil
	}
	return &msg
}

// readExpectedWSMessage reads messages from a WebSocket until the expected message type is received or timeout expires.
func readExpectedWSMessage(t *testing.T, conn *websocket.Conn, expectedType string, timeout time.Duration) *risk.WSMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("Timed out waiting for message of type '%s'", expectedType)
			return nil
		}
		conn.SetReadDeadline(time.Now().Add(remaining))
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("Failed to read WS message while waiting for '%s': %v", expectedType, err)
			return nil
		}
		var msg risk.WSMessage
		if err := json.Unmarshal(msgBytes, &msg); err != nil {
			continue
		}
		if msg.Type == expectedType {
			return &msg
		}
	}
}

// sendWSMessage sends a WSMessage over a WebSocket connection.
func sendWSMessage(t *testing.T, conn *websocket.Conn, msgType string, payload interface{}) {
	t.Helper()
	pBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}
	msg := risk.WSMessage{
		Type:    msgType,
		Payload: pBytes,
	}
	mBytes, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal WSMessage: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, mBytes); err != nil {
		t.Fatalf("Failed to write WS message: %v", err)
	}
}

// TestIntegration_FullTradeLifecycle exercises the entire trade flow over real WebSockets:
// 1. Adapter pushes market data & account balance
// 2. Web terminal receives SYNC_STATE
// 3. Web terminal places an order (EXECUTE_ORDER)
// 4. Adapter receives Go-computed SUBMIT_ORDER
// 5. Adapter streams EXECUTION_UPDATE fill
// 6. Hub dispatches consolidated brackets to Adapter
func TestIntegration_FullTradeLifecycle(t *testing.T) {
	h, server, adapterConn, webConn := setupIntegrationServer(t)
	defer server.Close()
	defer adapterConn.Close()
	defer webConn.Close()

	// Drain the initial state sent to Web on connect
	readNextWSMessage(t, webConn, 500*time.Millisecond)

	// Step 1: Adapter sends UPDATE_PRICES for MNQ
	priceUpdate := map[string]interface{}{
		"instrumentName":     "MNQ",
		"accountName":        "Playback101",
		"tickSize":           0.25,
		"pointValue":         2.0,
		"accountBalance":     50000.0,
		"currentMarketPrice": 20000.00,
	}
	sendWSMessage(t, adapterConn, "UPDATE_PRICES", priceUpdate)

	// Step 2: Web receives SYNC_STATE with updated MNQ instrument & price levels
	syncMsg := readExpectedWSMessage(t, webConn, "SYNC_STATE", 500*time.Millisecond)
	var state risk.TradeState
	json.Unmarshal(syncMsg.Payload, &state)
	if state.InstrumentName != "MNQ" || state.CurrentMarketPrice != 20000.00 {
		t.Errorf("Unexpected state received by Web: Inst=%s Price=%.2f", state.InstrumentName, state.CurrentMarketPrice)
	}

	// Step 3: Web terminal sends EXECUTE_ORDER
	execCmd := risk.ExecuteOrderCmd{
		Action:      "EXECUTE",
		Direction:   "LONG",
		OrderType:   "Limit",
		EntryPrice:  20000.00,
		StopPrice:   19980.00,
		TargetPrice: 20040.00,
		Qty:         2,
	}
	sendWSMessage(t, webConn, "EXECUTE_ORDER", execCmd)

	// Step 4: Adapter should receive SUBMIT_ORDER with GoEntry (calculated qty = 10)
	submitMsg := readExpectedWSMessage(t, adapterConn, "SUBMIT_ORDER", 500*time.Millisecond)

	var singleOrder risk.SingleOrderCmd
	json.Unmarshal(submitMsg.Payload, &singleOrder)
	if singleOrder.Name != "GoEntry" || singleOrder.Action != "BUY" || singleOrder.Qty <= 0 {
		t.Errorf("Unexpected entry order: Name=%s Action=%s Qty=%d", singleOrder.Name, singleOrder.Action, singleOrder.Qty)
	}

	// Step 5: Adapter simulates fill report (EXECUTION_UPDATE) + position update
	execUpdate := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_sim_1",
		OrderId:     "ord_entry_sim",
		Name:        "GoEntry",
		Action:      "Buy",
		FillPrice:   20000.00,
		FillQty:     singleOrder.Qty,
		OrderState:  "Filled",
		AccountName: "Playback101",
	}
	sendWSMessage(t, adapterConn, "EXECUTION_UPDATE", execUpdate)

	posUpdate := risk.PositionInfo{
		MarketPosition: "Long",
		Quantity:       singleOrder.Qty,
		AveragePrice:   20000.00,
	}
	sendWSMessage(t, adapterConn, "POSITION_UPDATE", posUpdate)

	// Step 6: After fill debounce (100ms), Hub dispatches consolidated bracket SUBMIT_ORDER to Adapter
	bracketMsg := readExpectedWSMessage(t, adapterConn, "SUBMIT_ORDER", 600*time.Millisecond)
	if bracketMsg.Type != "SUBMIT_ORDER" {
		t.Fatalf("Expected bracket SUBMIT_ORDER, got %s", bracketMsg.Type)
	}
	var batch risk.BatchSubmitOrdersCmd
	json.Unmarshal(bracketMsg.Payload, &batch)
	if len(batch.Orders) != 2 {
		t.Fatalf("Expected 2 bracket orders (SL + TP), got %d", len(batch.Orders))
	}
	hasSL := false
	hasTP := false
	for _, o := range batch.Orders {
		if o.Name == "ActiveSL" && o.StopPrice > 0 {
			hasSL = true
		}
		if o.Name == "ActiveTP" && o.LimitPrice > 0 {
			hasTP = true
		}
	}
	if !hasSL || !hasTP {
		t.Errorf("Brackets missing expected SL or TP: %+v", batch.Orders)
	}

	// Step 7: Simulate exit fill on ActiveTP -> Hub clears/reduces position
	exitUpdate := risk.ExecutionUpdateInfo{
		ExecutionId: "exec_sim_tp",
		OrderId:     "ord_tp_sim",
		Name:        "ActiveTP",
		Action:      "Sell",
		FillPrice:   20010.00,
		FillQty:     singleOrder.Qty,
		OrderState:  "Filled",
		AccountName: "Playback101",
	}
	sendWSMessage(t, adapterConn, "EXECUTION_UPDATE", exitUpdate)

	// Wait briefly for exit fill processing
	time.Sleep(50 * time.Millisecond)

	// Verify hub position cleared
	h.Mu.RLock()
	defer h.Mu.RUnlock()
	if h.State.Position.Quantity != 0 {
		t.Errorf("Expected Hub position quantity 0 after TP exit fill, got %d", h.State.Position.Quantity)
	}
}

// TestIntegration_WebReconnection verifies that a reconnecting Web client
// receives the current TradeState immediately upon opening its WebSocket connection.
func TestIntegration_WebReconnection(t *testing.T) {
	_, server, adapterConn, webConn1 := setupIntegrationServer(t)
	defer server.Close()
	defer adapterConn.Close()

	// Update some state via adapter
	priceUpdate := map[string]interface{}{
		"instrumentName":     "MNQ",
		"currentMarketPrice": 20150.25,
		"accountName":        "Playback101",
	}
	sendWSMessage(t, adapterConn, "UPDATE_PRICES", priceUpdate)
	time.Sleep(50 * time.Millisecond)

	// Drain state update on webConn1
	readNextWSMessage(t, webConn1, 500*time.Millisecond)
	// Close webConn1 to simulate disconnect
	webConn1.Close()

	// Connect a new Web client (reconnect)
	baseURL := "ws" + strings.TrimPrefix(server.URL, "http")
	webConn2, _, err := websocket.DefaultDialer.Dial(baseURL+"?type=WEB", nil)
	if err != nil {
		t.Fatalf("Failed to reconnect WEB WS client: %v", err)
	}
	defer webConn2.Close()

	// New web client must receive SYNC_STATE containing latest price
	msg := readNextWSMessage(t, webConn2, 500*time.Millisecond)
	if msg.Type != "SYNC_STATE" {
		t.Fatalf("Expected SYNC_STATE on reconnect, got %s", msg.Type)
	}
	var state risk.TradeState
	json.Unmarshal(msg.Payload, &state)
	if state.CurrentMarketPrice != 20150.25 {
		t.Errorf("Expected hydrated state price 20150.25, got %.2f", state.CurrentMarketPrice)
	}
}

// TestIntegration_ConcurrentPriceAndBarUpdates tests high-throughput message exchange
// without race conditions or deadlocks.
func TestIntegration_ConcurrentPriceAndBarUpdates(t *testing.T) {
	_, server, adapterConn, webConn := setupIntegrationServer(t)
	defer server.Close()
	defer adapterConn.Close()
	defer webConn.Close()

	done := make(chan bool)

	// Goroutine streaming bars from Adapter
	go func() {
		for i := 0; i < 20; i++ {
			bar := risk.ChartBar{
				Time:   int64(1000 + i*15),
				Open:   20000 + float64(i),
				High:   20005 + float64(i),
				Low:    19995 + float64(i),
				Close:  20002 + float64(i),
				Volume: 10,
			}
			sendWSMessage(t, adapterConn, "BAR_UPDATE", bar)
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	// Goroutine sending config patches from Web
	go func() {
		for i := 0; i < 20; i++ {
			rr := 1.5 + float64(i%5)*0.5
			cfg := map[string]interface{}{
				"selectedRR": rr,
			}
			sendWSMessage(t, webConn, "SET_CONFIG", cfg)
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	<-done
	<-done
}
