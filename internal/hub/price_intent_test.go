package hub

import (
	"testing"

	"trade-engine-without-chart/internal/risk"
)

func TestNormalizeWebPricePatchOwnsServerRules(t *testing.T) {
	state := &risk.TradeState{
		IsLong:     true,
		EntryPrice: 100,
		TickSize:   0.25,
		CurrentBid: 99,
		CurrentAsk: 99.25,
		Position:   risk.PositionInfo{MarketPosition: "Flat"},
	}
	entry, stop, target := 101.12, 100.10, 99.90
	normalizeWebPricePatch(state, &entry, &stop, &target)
	if entry != 101 || stop != 100 || target != 101.25 {
		t.Fatalf("flat long normalization = entry %.2f stop %.2f target %.2f", entry, stop, target)
	}
}

func TestNormalizeWebPricePatchClampsLiveStopToMarket(t *testing.T) {
	state := &risk.TradeState{
		IsLong:     true,
		EntryPrice: 100,
		TickSize:   0.25,
		CurrentBid: 101,
		LastPrice:  101,
		Position:   risk.PositionInfo{MarketPosition: "Long", Quantity: 2},
	}
	stop := 101.12
	normalizeWebPricePatch(state, nil, &stop, nil)
	if stop != 100.75 {
		t.Fatalf("live long stop = %.2f, want 100.75", stop)
	}
}
