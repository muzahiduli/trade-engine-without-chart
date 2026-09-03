package risk

import (
	"math"
	"testing"
)

func TestPositionSizeCalculation(t *testing.T) {
	// NQ Example: 20 ticks stop distance = 5.0 pts * $20/pt = $100 risk per contract
	// $300 risk / $100 = 3 contracts
	qty := CalculatePositionSize(300.0, 5.0, 20.0, 10)
	if qty != 3 {
		t.Errorf("Expected 3 contracts, got %d", qty)
	}

	// Max contracts clamp test
	qtyMax := CalculatePositionSize(1000.0, 5.0, 20.0, 5)
	if qtyMax != 5 {
		t.Errorf("Expected max 5 contracts, got %d", qtyMax)
	}
}

func TestDynamicRisk(t *testing.T) {
	// Account balance $50,500, MLL $50,000, Buffer $500 (< Penalty threshold $1,000) -> Penalty Risk
	risk, state, buffer := CalculateDynamicRisk(50500, 50000, 1000, 100, 200, 0.25, 400, 100)
	if state != "SURVIVAL" || risk != 100 || buffer != 500 {
		t.Errorf("Expected SURVIVAL state with $100 risk, got %s risk $%.2f buffer $%.2f", state, risk, buffer)
	}

	// Account balance $52,000, MLL $50,000, Buffer $2000 (> Penalty threshold $1,000)
	// Excess = $1000, Base = $200 + (1000 * 0.25) = $450 -> Clamped to MaxCap $400
	riskGrowth, stateGrowth, bufferGrowth := CalculateDynamicRisk(52000, 50000, 1000, 100, 200, 0.25, 400, 100)
	if stateGrowth != "GROWTH" || riskGrowth != 400 || bufferGrowth != 2000 {
		t.Errorf("Expected GROWTH state with $400 risk, got %s risk $%.2f buffer $%.2f", stateGrowth, riskGrowth, bufferGrowth)
	}
}

func TestTargetExits(t *testing.T) {
	// Single target (no partial)
	exits := GetTargetExits(3, 100.0, 2.0, 2.0, false, 0.33, 0.50, true, 0.25, false, nil)
	if len(exits) != 1 {
		t.Fatalf("Expected 1 exit, got %d", len(exits))
	}
	if exits[0].Price != 104.0 || exits[0].Qty != 3 {
		t.Errorf("Expected price 104.0 with qty 3, got price %.2f qty %d", exits[0].Price, exits[0].Qty)
	}

	// Partial exits
	partialExits := GetTargetExits(3, 100.0, 2.0, 2.0, true, 0.3333, 0.50, true, 0.25, false, nil)
	if len(partialExits) < 2 {
		t.Errorf("Expected at least 2 partial exits, got %d", len(partialExits))
	}
}

func TestRecalculateState(t *testing.T) {
	state := &TradeState{
		EntryPrice:   100.0,
		StopPrice:    98.0,
		RiskCash:     200.0,
		PointValue:   20.0,
		TickSize:     0.25,
		MaxContracts: 10,
		SelectedRR:   2.0,
		IsLong:       true,
	}

	RecalculateState(state)

	if state.CalculatedQty != 5 {
		t.Errorf("Expected 5 contracts ($200 / ($2 * $20)), got %d", state.CalculatedQty)
	}
	if math.Abs(state.TargetPrice-104.0) > 0.001 {
		t.Errorf("Expected target price 104.0, got %.2f", state.TargetPrice)
	}
}

func TestBuildExecutionPlan(t *testing.T) {
	// 1. Long Breakout: Entry (20010) > Market (20000) -> StopLimit
	stateLongBreakout := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20010.00,
		StopPrice:       19990.00,
		CalculatedQty:   2,
		TickSize:        0.25,
	}
	plan1 := BuildExecutionPlan(stateLongBreakout, 20000.00)
	if plan1.OrderType != "StopLimit" || plan1.Action != "BUY" {
		t.Errorf("Expected BUY StopLimit for Long breakout, got %s %s", plan1.Action, plan1.OrderType)
	}
	if plan1.StopPrice != 20010.00 || plan1.LimitPrice != 20010.00 {
		t.Errorf("Expected StopPrice=20010 and LimitPrice=20010, got Stp=%.2f Lmt=%.2f", plan1.StopPrice, plan1.LimitPrice)
	}

	// 2. Long Pullback: Entry (19990) <= Market (20000) -> Limit
	stateLongPullback := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      19990.00,
		StopPrice:       19970.00,
		CalculatedQty:   3,
		TickSize:        0.25,
	}
	plan2 := BuildExecutionPlan(stateLongPullback, 20000.00)
	if plan2.OrderType != "Limit" || plan2.Action != "BUY" {
		t.Errorf("Expected BUY Limit for Long pullback, got %s %s", plan2.Action, plan2.OrderType)
	}
	if plan2.LimitPrice != 19990.00 || plan2.StopPrice != 0 {
		t.Errorf("Expected LimitPrice=19990 and StopPrice=0, got Lmt=%.2f Stp=%.2f", plan2.LimitPrice, plan2.StopPrice)
	}

	// 3. Short Breakout: Entry (19980) < Market (20000) -> StopLimit
	stateShortBreakout := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          false,
		EntryPrice:      19980.00,
		StopPrice:       20000.00,
		CalculatedQty:   4,
		TickSize:        0.25,
	}
	plan3 := BuildExecutionPlan(stateShortBreakout, 20000.00)
	if plan3.OrderType != "StopLimit" || plan3.Action != "SELL_SHORT" {
		t.Errorf("Expected SELL_SHORT StopLimit for Short breakout, got %s %s", plan3.Action, plan3.OrderType)
	}

	// 4. Short Pullback: Entry (20020) >= Market (20000) -> Limit
	stateShortPullback := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          false,
		EntryPrice:      20020.00,
		StopPrice:       20040.00,
		CalculatedQty:   4,
		TickSize:        0.25,
	}
	plan4 := BuildExecutionPlan(stateShortPullback, 20000.00)
	if plan4.OrderType != "Limit" || plan4.Action != "SELL_SHORT" {
		t.Errorf("Expected SELL_SHORT Limit for Short pullback, got %s %s", plan4.Action, plan4.OrderType)
	}
}

func TestRoundToTickSize(t *testing.T) {
	// NQ tick size = 0.25
	cases := []struct {
		input    float64
		tick     float64
		expected float64
	}{
		{20000.12, 0.25, 20000.00},
		{20000.13, 0.25, 20000.25},
		{20000.37, 0.25, 20000.25},
		{20000.38, 0.25, 20000.50},
		{20000.75, 0.25, 20000.75},
		{20000.88, 0.25, 20001.00},
	}

	for _, c := range cases {
		result := RoundToTickSize(c.input, c.tick)
		if math.Abs(result-c.expected) > 0.0001 {
			t.Errorf("RoundToTickSize(%.2f, %.2f) = %.2f, expected %.2f", c.input, c.tick, result, c.expected)
		}
	}
}

func TestCalculatePaddedPositionSize(t *testing.T) {
	// Cash risk $500, SL distance 5.0 pts, 2 ticks padding (0.50 pts on 0.25 tick)
	// Padded stop distance = 5.50 pts * $20/pt = $110 risk per contract
	// $500 / $110 = 4.54 -> floor = 4 contracts (never exceeds $500 cash risk)
	qty := CalculatePaddedPositionSize(500.0, 5.0, 2.0, 0.25, 20.0, 10)
	if qty != 4 {
		t.Errorf("Expected 4 contracts with padded slippage (floor-based sizing), got %d", qty)
	}
}

func TestCalculateDynamicRisk_ExactThreshold(t *testing.T) {
	// Buffer == penaltyThreshold (1000): should transition to GROWTH mode
	risk, state, buffer := CalculateDynamicRisk(51000, 50000, 1000, 100, 200, 0.25, 400, 100)
	if state != "GROWTH" || buffer != 1000 || risk != 200 {
		t.Errorf("Expected GROWTH state with base risk $200 at exact threshold, got state=%s risk=%.2f buffer=%.2f", state, risk, buffer)
	}
}

func TestRecalculateState_DynRiskEnabled(t *testing.T) {
	state := &TradeState{
		EntryPrice:       20000.0,
		StopPrice:        19990.0, // 10 pts * $2 (MNQ) = $20/contract
		RiskCash:         100.0,
		PointValue:       2.0,
		TickSize:         0.25,
		MaxContracts:     20,
		SelectedRR:       2.0,
		IsLong:           true,
		EnableDynRisk:    true,
		AccountBalance:   52000,
		MLL:              50000,
		PenaltyThreshold: 1000,
		PenaltyRisk:      50,
		BaseRisk:         200,
		ScaleFactor:      0.25,
		MaxCap:           400,
	}

	RecalculateState(state)

	// In GROWTH mode with buffer 2000, risk is min(400, 200 + (1000 * 0.25)) = $400.
	// $400 / (10 pts * $2) = 20 contracts.
	if state.DynRiskState != "GROWTH" {
		t.Errorf("Expected GROWTH dyn risk state, got %s", state.DynRiskState)
	}
	if state.CalculatedQty != 20 {
		t.Errorf("Expected 20 contracts from dynamic risk ($400 / $20), got %d", state.CalculatedQty)
	}
}

func TestRecalculateState_ZeroStopDistance(t *testing.T) {
	state := &TradeState{
		EntryPrice:   20000.0,
		StopPrice:    20000.0, // 0 distance
		RiskCash:     100.0,
		PointValue:   2.0,
		TickSize:     0.25,
		MaxContracts: 10,
		SelectedRR:   2.0,
		IsLong:       true,
	}

	// Must not panic or produce NaN/inf
	RecalculateState(state)

	if state.CalculatedQty < 1 {
		t.Errorf("Expected fallback minimum 1 contract on zero SL distance, got %d", state.CalculatedQty)
	}
}

func TestRecalculateState_ShortWithPartial(t *testing.T) {
	state := &TradeState{
		EntryPrice:      20000.0,
		StopPrice:       20020.0, // Short: Stop above entry
		RiskCash:        200.0,
		PointValue:      2.0,
		TickSize:        0.25,
		MaxContracts:    10,
		SelectedRR:      2.0,
		IsLong:          false,
		IsPartialProfit: true,
	}

	RecalculateState(state)

	if state.TargetPrice >= state.EntryPrice {
		t.Errorf("Expected short TargetPrice < EntryPrice, got %.2f >= %.2f", state.TargetPrice, state.EntryPrice)
	}
	if len(state.TargetExits) < 2 {
		t.Fatalf("Expected at least 2 partial exits, got %d", len(state.TargetExits))
	}
	for i, exit := range state.TargetExits {
		if exit.Price >= state.EntryPrice {
			t.Errorf("TargetExit[%d] price %.2f should be below EntryPrice %.2f for short", i, exit.Price, state.EntryPrice)
		}
	}
}

func TestGetTargetExits_CustomPreserved(t *testing.T) {
	customExits := []TargetExit{
		{Ratio: 1.2, Qty: 2, Price: 20024.0},
		{Ratio: 2.5, Qty: 2, Price: 20050.0},
		{Ratio: 4.0, Qty: 1, Price: 20080.0},
	}

	exits := GetTargetExits(5, 20000.0, 20.0, 2.0, true, 0.33, 0.50, true, 0.25, true, customExits)
	if len(exits) != 3 {
		t.Fatalf("Expected 3 custom exits preserved, got %d", len(exits))
	}
	if exits[0].Ratio != 1.2 || exits[1].Ratio != 2.5 || exits[2].Ratio != 4.0 {
		t.Errorf("Expected custom ratios 1.2, 2.5, 4.0, got %.2f, %.2f, %.2f", exits[0].Ratio, exits[1].Ratio, exits[2].Ratio)
	}
	if exits[0].Price != 20024.0 || exits[1].Price != 20050.0 || exits[2].Price != 20080.0 {
		t.Errorf("Expected prices 20024, 20050, 20080, got %.2f, %.2f, %.2f", exits[0].Price, exits[1].Price, exits[2].Price)
	}
}

func TestGetTargetExits_SingleContractPartial(t *testing.T) {
	// With 1 contract and partial profit enabled, should generate at least 2 exits
	exits := GetTargetExits(1, 20000.0, 20.0, 2.0, true, 0.33, 0.50, true, 0.25, false, nil)
	if len(exits) < 2 {
		t.Fatalf("Expected at least 2 target exits even with 1 contract, got %d", len(exits))
	}
}

func TestBuildExecutionPlan_ZeroMarketPrice(t *testing.T) {
	state := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20000.0,
		StopPrice:       19980.0,
		CalculatedQty:   1,
		TickSize:        0.25,
		EntryModel:      "Limit",
	}

	// Market price 0 -> should gracefully default based on EntryModel without panic
	plan := BuildExecutionPlan(state, 0.0)
	if plan.Action != "BUY" {
		t.Errorf("Expected BUY action, got %s", plan.Action)
	}
	if plan.Qty != 1 {
		t.Errorf("Expected Qty 1, got %d", plan.Qty)
	}
}

// TestBuildExecutionPlan_StopTriggerSanitized pins the stop-entry sanitizer:
// a buy stop/stop-limit trigger at or below the live Ask (or the last price
// fallback) is clamped to >= 1 tick beyond it, so NinjaTrader's "buy stop
// cannot be placed below the market" rejection can't happen; the reverse for
// sells. Triggers already on the valid side are left untouched.
func TestBuildExecutionPlan_StopTriggerSanitized(t *testing.T) {
	// Long breakout with entry below/at the live Ask → clamped above the Ask
	// (market/last must be consistent with the ask so it isn't seen as stale).
	state := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20010.00,
		StopPrice:       19990.00,
		CurrentAsk:      20010.25,
		CalculatedQty:   1,
		TickSize:        0.25,
	}
	plan := BuildExecutionPlan(state, 20009.75)
	if plan.OrderType != "StopLimit" {
		t.Fatalf("Expected StopLimit, got %s", plan.OrderType)
	}
	if plan.StopPrice != 20010.50 {
		t.Errorf("Long stop at Ask (%v) must clamp to Ask+tick (20010.50), got %.2f", state.CurrentAsk, plan.StopPrice)
	}
	if plan.LimitPrice != plan.StopPrice {
		t.Errorf("LimitPrice must follow the sanitized StopPrice, got %.2f", plan.LimitPrice)
	}

	// Long breakout well above the Ask → untouched.
	state2 := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20015.00,
		StopPrice:       19990.00,
		CurrentAsk:      20012.00,
		CalculatedQty:   1,
		TickSize:        0.25,
	}
	plan2 := BuildExecutionPlan(state2, 20009.75)
	if plan2.StopPrice != 20015.00 {
		t.Errorf("Valid long stop must be untouched, got %.2f", plan2.StopPrice)
	}

	// Short breakout with entry above/at the live Bid → clamped below the Bid.
	state3 := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          false,
		EntryPrice:      19980.00,
		StopPrice:       20000.00,
		CurrentBid:      19979.75,
		CalculatedQty:   1,
		TickSize:        0.25,
	}
	plan3 := BuildExecutionPlan(state3, 19980.25)
	if plan3.OrderType != "StopLimit" {
		t.Fatalf("Expected StopLimit for short breakout, got %s", plan3.OrderType)
	}
	if plan3.StopPrice != 19979.50 {
		t.Errorf("Short stop at Bid (%v) must clamp to Bid-tick (19979.50), got %.2f", state3.CurrentBid, plan3.StopPrice)
	}

	// No bid/ask (e.g. Market Replay): the sanitizer falls back to the last
	// price; the entry is already above it (isBreakout) so nothing changes.
	state4 := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20010.00,
		StopPrice:       19990.00,
		CalculatedQty:   1,
		TickSize:        0.25,
	}
	plan4 := BuildExecutionPlan(state4, 20000.00)
	if plan4.OrderType != "StopLimit" || plan4.StopPrice != 20010.00 {
		t.Errorf("No bid/ask fallback: entry already above last price, stop must be untouched (StopLimit 20010.00), got %s %.2f", plan4.OrderType, plan4.StopPrice)
	}

	// STALE QUOTE: the Ask deviates from the live last beyond the plausibility
	// band — it must be ignored (fall back to the last) instead of inflating
	// the trigger to a bogus level.
	state5 := &TradeState{
		SelectedAccount: "Sim101",
		IsLong:          true,
		EntryPrice:      20005.00, // breakout vs last 20000
		StopPrice:       19990.00,
		CurrentAsk:      20100.00, // stale: 100 points off the live last
		CalculatedQty:   1,
		TickSize:        0.25,
	}
	plan5 := BuildExecutionPlan(state5, 20000.00)
	if plan5.OrderType != "StopLimit" || plan5.StopPrice != 20005.00 {
		t.Errorf("Stale ask must be ignored: stop should stay 20005.00 (vs last 20000), got %s %.2f", plan5.OrderType, plan5.StopPrice)
	}
}

func TestRecalculateState_EffectiveEntryModel(t *testing.T) {
	// The effective entry model mirrors BuildExecutionPlan's breakout decision:
	// long entry ABOVE market / short entry BELOW market → "STOP-LIMIT", all
	// else → "LIMIT". With no market reference yet (fresh boot) it falls back
	// to the user-configured model so the execute button never shows junk.

	// 1. Long breakout: entry 20010 > market 20000 → STOP-LIMIT + breakout.
	longBreakout := &TradeState{
		EntryPrice:        20010.00,
		StopPrice:         19990.00,
		CurrentMarketPrice: 20000.00,
		IsLong:            true,
	}
	RecalculateState(longBreakout)
	if longBreakout.EffectiveEntryModel != "STOP-LIMIT" || !longBreakout.IsBreakout {
		t.Errorf("Long breakout: expected STOP-LIMIT + breakout, got %q breakout=%v", longBreakout.EffectiveEntryModel, longBreakout.IsBreakout)
	}

	// 2. Long pullback: entry 19990 <= market 20000 → LIMIT, not breakout.
	longPullback := &TradeState{
		EntryPrice:        19990.00,
		StopPrice:         19970.00,
		CurrentMarketPrice: 20000.00,
		IsLong:            true,
	}
	RecalculateState(longPullback)
	if longPullback.EffectiveEntryModel != "LIMIT" || longPullback.IsBreakout {
		t.Errorf("Long pullback: expected LIMIT + no breakout, got %q breakout=%v", longPullback.EffectiveEntryModel, longPullback.IsBreakout)
	}

	// 3. Short breakout: entry 19980 < market 20000 → STOP-LIMIT + breakout.
	shortBreakout := &TradeState{
		EntryPrice:        19980.00,
		StopPrice:         20000.00,
		CurrentMarketPrice: 20000.00,
	}
	RecalculateState(shortBreakout)
	if shortBreakout.EffectiveEntryModel != "STOP-LIMIT" || !shortBreakout.IsBreakout {
		t.Errorf("Short breakout: expected STOP-LIMIT + breakout, got %q breakout=%v", shortBreakout.EffectiveEntryModel, shortBreakout.IsBreakout)
	}

	// 4. Short pullback: entry 20020 >= market 20000 → LIMIT, not breakout.
	shortPullback := &TradeState{
		EntryPrice:        20020.00,
		StopPrice:         20040.00,
		CurrentMarketPrice: 20000.00,
	}
	RecalculateState(shortPullback)
	if shortPullback.EffectiveEntryModel != "LIMIT" || shortPullback.IsBreakout {
		t.Errorf("Short pullback: expected LIMIT + no breakout, got %q breakout=%v", shortPullback.EffectiveEntryModel, shortPullback.IsBreakout)
	}

	// 5. No market reference yet → configured model fallback ("Limit" default).
	fresh := &TradeState{EntryPrice: 20000.00, StopPrice: 19980.00, IsLong: true}
	RecalculateState(fresh)
	if fresh.EffectiveEntryModel != "LIMIT" || fresh.IsBreakout {
		t.Errorf("Fresh boot: expected fallback LIMIT + no breakout, got %q breakout=%v", fresh.EffectiveEntryModel, fresh.IsBreakout)
	}

	// 6. Explicit configured model without market → honored verbatim.
	explicit := &TradeState{EntryPrice: 20000.00, StopPrice: 19980.00, EntryModel: "StopMarket"}
	RecalculateState(explicit)
	if explicit.EffectiveEntryModel != "STOPMARKET" {
		t.Errorf("Explicit StopMarket without market: expected STOPMARKET fallback, got %q", explicit.EffectiveEntryModel)
	}
}

func TestFloorBasedSizingAndRiskExceeded(t *testing.T) {
	// Stop distance = 3 pts * $20/pt = $60 risk per contract
	// Risk budget = $100
	// Rounding would do 100/60 = 1.666 -> 2 contracts ($120 > $100 budget)
	// Floor sizing MUST return 1 contract ($60 <= $100 budget)
	qty := CalculatePositionSize(100.0, 3.0, 20.0, 10)
	if qty != 1 {
		t.Errorf("Floor sizing expected 1 contract, got %d", qty)
	}

	// With commission: 3 pts * $20 = $60 + $5 commission = $65/contract
	// Budget $120 -> 120 / 65 = 1.846 -> floor = 1
	qtyWithComm := CalculatePositionSizeWithCommission(120.0, 3.0, 20.0, 5.0, 10)
	if qtyWithComm != 1 {
		t.Errorf("Floor sizing with commission expected 1, got %d", qtyWithComm)
	}

	// Risk Exceeded Test: Budget = $50, but 1 contract has 5 pts * $20 = $100 risk
	// Must return Qty = 1, but IsRiskExceeded = true and RiskExcessAmount = $50
	state := &TradeState{
		EntryPrice:   100.0,
		StopPrice:    95.0, // 5 pts
		RiskCash:     50.0, // $50 budget
		PointValue:   20.0,
		TickSize:     0.25,
		MaxContracts: 5,
		IsLong:       true,
	}
	RecalculateState(state)
	if state.CalculatedQty != 1 {
		t.Errorf("Expected 1 contract minimum, got %d", state.CalculatedQty)
	}
	if !state.IsRiskExceeded {
		t.Errorf("Expected IsRiskExceeded=true when 1 contract ($100) > budget ($50)")
	}
	if math.Abs(state.RiskExcessAmount-50.0) > 0.001 {
		t.Errorf("Expected RiskExcessAmount=$50, got %.2f", state.RiskExcessAmount)
	}
}



