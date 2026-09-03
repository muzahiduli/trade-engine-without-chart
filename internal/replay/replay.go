// Package replay: deterministic playback verification without NinjaTrader.
//
// A fixture is a JSONL transcript of a REAL playback run:
//
//	{"kind":"broker","type":"UPDATE_PRICES|MARKET_DATA|SYNC_BARS|BAR_UPDATE|POSITION_UPDATE|ORDERS_UPDATE|EXECUTION_UPDATE", "payload":...}
//	{"kind":"cmd","via":"hub","type":"UPDATE_PRICES|EXECUTE_ORDER|FLATTEN_POSITION", "payload":...}
//	{"kind":"outbound","type":"SUBMIT_ORDER|FLATTEN_POSITION|CHANGE_ORDER|CANCEL_ORDER", "payload":...}  // engine -> broker intent
//	{"kind":"step","name":..., "expectPos":..., "expectOrders":...}
//
// Replaying it into an in-process Hub reproduces the live verification
// deterministically â€” identical input bytes â†’ identical hub behavior â†’
// identical assertions â€” so it runs in `go test ./...` (and CI) with no NT8,
// no WebSocket, and no GUI.
//
// The test covers BOTH directions the live scenario proved:
//   - Engine â‡’ NT8: the orders the hub forwards to the broker (GoEntry entry,
//     ActiveSL/ActiveTP brackets, flatten) must match the recorded engine
//     outbound orders.
//   - NT8 â‡’ engine: after each step the hub's position and working orders must
//     match the broker snapshot recorded at the end of that step.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"trade-engine-without-chart/internal/hub"
	"trade-engine-without-chart/internal/risk"
)

// Record is one fixture line.
type Record struct {
	Kind    string          `json:"kind"`
	Type    string          `json:"type"`
	Name    string          `json:"name,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Ms      int             `json:"ms,omitempty"`
	// step expectations
	ExpectPos    risk.PositionInfo       `json:"expectPos,omitempty"`
	ExpectOrders []risk.WorkingOrderInfo `json:"expectOrders,omitempty"`
}

// hubMsg is the wire envelope for messages the hub forwards to an NT8 client.
type hubMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Load reads a fixture file into records.
func Load(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("fixture line: %w: %s", err, line[:min(80, len(line))])
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// Run feeds the fixture into an in-process Hub.
//
//   - broker   â†’ NT8 client message (the gateway stream)
//   - cmd      â†’ WEB client message (the engine's command path)
//   - outbound â†’ expected: read the hub's forwarded order off nt8c.Send and
//     compare (normalized â€” volatile IDs excluded)
//   - step     â†’ assert hub position + working orders == recorded broker truth
//
// Timing: EXECUTE_ORDER and bracket assembly forward asynchronously (the hub
// uses `go ForwardToNT8` + a ~100ms bracket debounce), so each outbound check
// drains nt8c.Send until the expected set is covered.
func Run(h *hub.Hub, nt8c, webc *hub.Client, fixture []Record) error {
	expected := make([]Record, 0)
	for _, rec := range fixture {
		switch rec.Kind {
		case "broker":
			h.HandleMessage(nt8c, risk.WSMessage{Type: rec.Type, Payload: rec.Payload})
		case "cmd":
			h.HandleMessage(webc, risk.WSMessage{Type: rec.Type, Payload: rec.Payload})
		case "pause":
			// Give an async hub action (e.g. the 100ms bracket debounce after a
			// fill) the same time it had live, before the next broker echo.
			if rec.Ms > 0 {
				time.Sleep(time.Duration(rec.Ms) * time.Millisecond)
			}
		case "outbound":
			expected = append(expected, rec)
		case "step":
			want := make([]string, 0)
			for _, r := range expected {
				want = append(want, normalizeOutbound(r.Type, r.Payload)...)
			}
			got, err := drainOutbound(nt8c, want, 3*time.Second)
			if err != nil {
				return fmt.Errorf("step %q: outbound drain: %w", rec.Name, err)
			}
			if err := assertOutbound(expected, got); err != nil {
				return fmt.Errorf("step %q (engine â‡’ NT8 orders): %w", rec.Name, err)
			}
			if err := assertStep(h, rec); err != nil {
				return fmt.Errorf("step %q (NT8 â‡’ engine state): %w", rec.Name, err)
			}
			expected = nil // consumed; next step compares only its own orders
		}
	}
	return nil
}

// drainOutbound reads messages the hub forwarded to the NT8 client until the
// expected set is covered or the deadline passes. Brackets are dispatched on a
// ~100ms debounce after fills, so this must not rely on a single quiet window.
func drainOutbound(nt8c *hub.Client, expected []string, deadline time.Duration) ([]string, error) {
	var out []string
	need := map[string]int{}
	for _, e := range expected {
		need[e]++
	}
	have := map[string]int{}
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		// Stop as soon as every expected message has been observed.
		complete := true
		for k, n := range need {
			if have[k] < n {
				complete = false
				break
			}
		}
		if complete && len(out) > 0 {
			return out, nil
		}
		select {
		case b := <-nt8c.Send:
			var m hubMsg
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			for _, n := range normalizeOutbound(m.Type, m.Payload) {
				out = append(out, n)
				have[n]++
			}
		case <-time.After(100 * time.Millisecond):
			// no message this tick; keep waiting until deadline
		}
	}
	return out, fmt.Errorf("timed out waiting for forwarded orders (have=%v want=%d)", have, len(expected))
}

// normalizeOutbound renders an engineâ†’broker command as comparable strings,
// dropping volatile fields (order IDs, OCO IDs, timestamps). A SUBMIT_ORDER
// may be a single order OR a bracket batch {"orders":[...]} â€” both expand.
func normalizeOutbound(msgType string, payload json.RawMessage) []string {
	switch msgType {
	case "SUBMIT_ORDER":
		var batch struct {
			Orders []struct {
				AccountName string  `json:"accountName"`
				Instrument  string  `json:"instrument"`
				Action      string  `json:"action"`
				OrderType   string  `json:"orderType"`
				Qty         int     `json:"qty"`
				LimitPrice  float64 `json:"limitPrice"`
				StopPrice   float64 `json:"stopPrice"`
				Name        string  `json:"name"`
			} `json:"orders"`
		}
		if err := json.Unmarshal(payload, &batch); err == nil && len(batch.Orders) > 0 {
			var out []string
			for _, o := range batch.Orders {
				out = append(out, fmt.Sprintf("SUBMIT_ORDER|%s|%s|%s|%s|%d|%.2f|%.2f|%s",
					o.AccountName, o.Instrument, o.Action, o.OrderType, o.Qty, o.LimitPrice, o.StopPrice, o.Name))
			}
			return out
		}
		var o struct {
			AccountName string  `json:"accountName"`
			Instrument  string  `json:"instrument"`
			Action      string  `json:"action"`
			OrderType   string  `json:"orderType"`
			Qty         int     `json:"qty"`
			LimitPrice  float64 `json:"limitPrice"`
			StopPrice   float64 `json:"stopPrice"`
			Name        string  `json:"name"`
		}
		if json.Unmarshal(payload, &o) != nil {
			return nil
		}
		return []string{fmt.Sprintf("SUBMIT_ORDER|%s|%s|%s|%s|%d|%.2f|%.2f|%s",
			o.AccountName, o.Instrument, o.Action, o.OrderType, o.Qty, o.LimitPrice, o.StopPrice, o.Name)}
	case "FLATTEN_POSITION", "CHANGE_ORDER", "CANCEL_ORDER":
		var p struct {
			AccountName string `json:"accountName"`
			Instrument  string `json:"instrument"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return nil
		}
		return []string{fmt.Sprintf("%s|%s|%s", msgType, p.AccountName, p.Instrument)}
	}
	return nil
}

// assertOutbound checks the recorded engineâ†’broker orders equal the orders the
// replay hub actually forwarded (multiset comparison; batch/bracket order can
// vary slightly).
func assertOutbound(expected []Record, got []string) error {
	want := make([]string, 0)
	for _, r := range expected {
		want = append(want, normalizeOutbound(r.Type, r.Payload)...)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(want, "|") != strings.Join(got, "|") {
		return fmt.Errorf("\n  recorded(engineâ†’broker): %v\n  replayed(hubâ†’broker): %v", want, got)
	}
	return nil
}

func assertStep(h *hub.Hub, rec Record) error {
	h.Mu.RLock()
	pos := h.State.Position
	orders := append([]risk.WorkingOrderInfo(nil), h.State.WorkingOrders...)
	h.Mu.RUnlock()

	if strings.ToLower(strings.TrimSpace(pos.MarketPosition)) != strings.ToLower(strings.TrimSpace(rec.ExpectPos.MarketPosition)) {
		return fmt.Errorf("position side: engine=%s broker=%s", pos.MarketPosition, rec.ExpectPos.MarketPosition)
	}
	if pos.Quantity != rec.ExpectPos.Quantity {
		return fmt.Errorf("position qty: engine=%d broker=%d", pos.Quantity, rec.ExpectPos.Quantity)
	}
	ek := orderKeys(orders)
	bk := orderKeys(rec.ExpectOrders)
	if strings.Join(ek, "|") != strings.Join(bk, "|") {
		return fmt.Errorf("working orders mismatch:\n  engine=%v\n  broker=%v", ek, bk)
	}
	return nil
}

// orderKeys normalizes working orders the same way the live scenario compared
// them: name/action/price/qty/state, skipping terminal states.
func orderKeys(orders []risk.WorkingOrderInfo) []string {
	var out []string
	for _, o := range orders {
		if o.State == "Filled" || o.State == "Cancelled" || o.State == "Canceled" {
			continue
		}
		out = append(out, fmt.Sprintf("%s|%s|%.2f|%d|%s", o.Name, strings.ToUpper(o.Action), o.Price, o.Qty, strings.ToUpper(o.State)))
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
