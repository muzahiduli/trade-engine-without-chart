package hub

import (
	"strings"

	"trade-engine-without-chart/internal/risk"
)

// normalizeWebPricePatch is the single server-side boundary for prices coming
// from the browser. The browser supplies a gesture; Go owns tick alignment and
// all trading-side validity rules before state or NT8 orders are changed.
func normalizeWebPricePatch(state *risk.TradeState, entry, stop, target *float64) {
	if state == nil {
		return
	}
	tick := state.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	if entry != nil {
		*entry = risk.RoundToTickSize(*entry, tick)
		if !positionIsLive(state) {
			for _, order := range state.WorkingOrders {
				if !strings.HasPrefix(order.Name, "GoEntry") ||
					(order.State != "Working" && order.State != "Accepted") ||
					(order.OrderType != "StopMarket" && order.OrderType != "StopLimit") {
					continue
				}
				*entry = normalizeStopSide(state, *entry, order.Action, tick)
				break
			}
		}
	}
	effectiveEntry := state.EntryPrice
	if entry != nil {
		effectiveEntry = *entry
	}
	if stop != nil {
		*stop = normalizeStopPrice(state, effectiveEntry, *stop, tick)
	}
	if target != nil {
		*target = risk.RoundToTickSize(*target, tick)
		if !positionIsLive(state) && effectiveEntry > 0 {
			if state.IsLong && *target <= effectiveEntry {
				*target = effectiveEntry + tick
			}
			if !state.IsLong && *target >= effectiveEntry {
				*target = effectiveEntry - tick
			}
		}
	}
}

func normalizeStopPrice(state *risk.TradeState, entry, value, tick float64) float64 {
	value = risk.RoundToTickSize(value, tick)
	if positionIsLive(state) {
		long := state.IsLong
		if strings.EqualFold(state.Position.MarketPosition, "Long") {
			long = true
		} else if strings.EqualFold(state.Position.MarketPosition, "Short") {
			long = false
		}
		action := "BUY"
		if long {
			action = "SELL"
		}
		return normalizeStopSide(state, value, action, tick)
	}
	if entry > 0 {
		if state.IsLong && value >= entry {
			return risk.RoundToTickSize(entry-tick, tick)
		}
		if !state.IsLong && value <= entry {
			return risk.RoundToTickSize(entry+tick, tick)
		}
	}
	return value
}

func normalizeStopSide(state *risk.TradeState, value float64, action string, tick float64) float64 {
	sellSide := strings.EqualFold(action, "SELL") || strings.EqualFold(action, "SELL_SHORT") || strings.EqualFold(action, "SELLSHORT")
	market := state.CurrentAsk
	if sellSide {
		market = state.CurrentBid
	}
	if market <= 0 {
		market = state.LastPrice
	}
	if market <= 0 {
		return value
	}
	if sellSide && value >= market {
		value = risk.RoundToTickSize(market, tick) - tick
		for value >= market {
			value -= tick
		}
	}
	if !sellSide && value <= market {
		value = risk.RoundToTickSize(market, tick) + tick
		for value <= market {
			value += tick
		}
	}
	return risk.RoundToTickSize(value, tick)
}

func positionIsLive(state *risk.TradeState) bool {
	if state == nil {
		return false
	}
	return state.Position.Quantity > 0 && !strings.EqualFold(state.Position.MarketPosition, "Flat")
}
