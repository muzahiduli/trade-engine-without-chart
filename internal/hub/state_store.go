package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"trade-engine-without-chart/internal/logging"
	"trade-engine-without-chart/internal/risk"
)

type EnginePrefs struct {
	DefaultAccount string `json:"defaultAccount"`
}

func loadEnginePrefs() EnginePrefs {
	var prefs EnginePrefs
	bytes, err := os.ReadFile("engine_prefs.json")
	if err == nil {
		_ = json.Unmarshal(bytes, &prefs)
	}
	return prefs
}

func saveEnginePrefs(prefs EnginePrefs) {
	bytes, err := json.MarshalIndent(prefs, "", "  ")
	if err == nil {
		_ = os.WriteFile("engine_prefs.json", bytes, 0644)
	}
}

// BroadcastState marshals TradeState and broadcasts SYNC_STATE to connected Web clients.
func (h *Hub) BroadcastState() {
	h.Mu.RLock()
	stateBytes, err := json.Marshal(h.State)
	h.Mu.RUnlock()
	if err != nil {
		return
	}
	h.BroadcastToWeb("SYNC_STATE", stateBytes)
}

// SendStateToClient sends current TradeState to a single client.
func (h *Hub) SendStateToClient(client *Client) {
	h.Mu.RLock()
	stateBytes, err := json.Marshal(h.State)
	h.Mu.RUnlock()
	if err != nil {
		return
	}
	msg := risk.WSMessage{
		Type:    "SYNC_STATE",
		Payload: stateBytes,
	}
	bytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	sendToClient(client, bytes)
}

// SendBarsToClient sends cached historical bars to a client. Empty timeframe
// (legacy) or the engine's tracking label return the tracking stream
// (BarCache); any other label returns that timeframe's cache.
func (h *Hub) SendBarsToClient(client *Client, timeframe string) {
	var bars []risk.ChartBar
	h.Mu.RLock()
	if timeframe == "" || (h.trackingTimeframe != "" && timeframe == h.trackingTimeframe) {
		bars = h.BarCache
	} else {
		bars = h.TimeframeBars[timeframe]
	}
	hasBars := len(bars) > 0
	payload, err := json.Marshal(bars)
	h.Mu.RUnlock()
	if !hasBars || err != nil {
		return
	}
	msg := risk.WSMessage{
		Type:    "SYNC_BARS",
		Payload: payload,
	}
	bytes, err := json.Marshal(msg)
	if err != nil {
		return
	}
	sendToClient(client, bytes)
}

// sameInstrument reports whether a broker-reported instrument refers to the
// same contract as the hub's current instrument. NT8 reports the FULL contract
// name ("MNQ 09-26") on POSITION_UPDATE / ORDERS_UPDATE while market info says
// the base symbol ("MNQ"); strict equality dropped those updates and the hub
// silently stayed FLAT with no working orders â€” the exact "position in NT8 but
// not in the engine" symptom. Matching the base symbol (text before the first
// space, e.g. the expiry suffix) reconciles both. An EMPTY instrument on a
// broker update means the gateway did not report one â€” that is accepted (the
// legacy guard only rejected updates where both sides were non-empty and
// different), so only a non-empty mismatch is filtered.
func sameInstrument(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	base := func(s string) string {
		if i := strings.IndexByte(s, ' '); i > 0 {
			return s[:i]
		}
		return s
	}
	return strings.EqualFold(base(a), base(b))
}

// hasCommittedEntry reports whether an entry is COMMITTED: either a position is
// open, or a GoEntry order is already working (submitted but not yet filled).
// While an entry is committed, bar close / market data must NEVER re-anchor
// Entry/Stop/Target â€” those levels are fixed unless the user moves them. This is
// the anti-"SL/TP moves after candle close" invariant.
//
// Callers MUST hold h.Mu (read or write).
func (h *Hub) hasCommittedEntry() bool {
	if h.State.Position.Quantity > 0 ||
		(h.State.Position.MarketPosition != "" && h.State.Position.MarketPosition != "Flat") {
		return true
	}
	for _, o := range h.State.WorkingOrders {
		if strings.HasPrefix(o.Name, "GoEntry") && (o.State == "Working" || o.State == "Accepted") {
			return true
		}
	}
	return false
}

// freezeAutoTrackOnEntry disables AutoTrack the INSTANT an entry order is
// submitted (not when it fills). From that moment, AutoTrack must never shift
// Entry/Stop/Target again â€” even if the fill / position update races with a bar
// close. Keeping the flag frozen at submit time also makes AutoTrack unusable
// while a resting (unfilled) entry order is working.
func (h *Hub) freezeAutoTrackOnEntry() {
	h.Mu.Lock()
	h.State.IsAutoTrackEnabled = false
	h.Mu.Unlock()
}

// seedPlanningLevelsFromMarket bootstraps the phantom entry/SL/TP NEAR the live
// market the first time any price data arrives, so the plan lines appear right
// at the current price as soon as the page starts â€” instead of the user having
// to click "snap to market". It only runs while the user is still PLANNING
// (flat AND no entry order committed); once a trade is committed the levels are
// the user's and are never touched.
//
// Callers MUST hold h.Mu (write) and must only call after verifying
// EntryPrice <= 0 && !hasCommittedEntry().
func (h *Hub) seedPlanningLevelsFromMarket(price float64) {
	if price <= 0 {
		return
	}
	h.State.EntryPrice = price
	slOffset := 20.0 * h.State.TickSize
	if slOffset <= 0 {
		slOffset = 5.0
	}
	if h.State.SelectedRR > 20.0 || h.State.SelectedRR < 0.1 {
		h.State.SelectedRR = 2.0
	}
	if h.State.IsLong {
		h.State.StopPrice = h.State.EntryPrice - slOffset
		h.State.TargetPrice = h.State.EntryPrice + (slOffset * h.State.SelectedRR)
	} else {
		h.State.StopPrice = h.State.EntryPrice + slOffset
		h.State.TargetPrice = h.State.EntryPrice - (slOffset * h.State.SelectedRR)
	}
}

func (h *Hub) handleUpdatePrices(client *Client, payload json.RawMessage) {
	var patch struct {
		EntryPrice         *float64 `json:"entryPrice"`
		StopPrice          *float64 `json:"stopPrice"`
		TargetPrice        *float64 `json:"targetPrice"`
		SelectedRR         *float64 `json:"selectedRR"`
		TickSize           *float64 `json:"tickSize"`
		PointValue         *float64 `json:"pointValue"`
		InstrumentName     *string  `json:"instrumentName"`
		AccountName        *string  `json:"accountName"`
		AvailableAccounts  []string `json:"availableAccounts"`
		AccountBalance     *float64 `json:"accountBalance"`
		IsBalanceEstimated *bool    `json:"isBalanceEstimated"`
		CurrentMarketPrice *float64 `json:"currentMarketPrice"`
		BarSeries          string   `json:"barSeries"`
	}
	if err := json.Unmarshal(payload, &patch); err != nil {
		return
	}

	// The NT8 gateway reports the exact bar series it is configured with
	// (e.g. "15s=Second15;1m=Minute1;5m=Minute5;100t=Volume100" for the fixed
	// build, "100t=Tick100" for the old tick-secondary build, or a
	// "100t=Tick100" PRIMARY when the chart data series is 100-Tick). Log it
	// so the connected strategy's configuration is verifiable in one glance.
	if patch.BarSeries != "" {
		log.Printf("NT8 gateway bar series: %s", patch.BarSeries)
	}

	h.Mu.Lock()

	// Stale price guard
	if client.ClientType == "WEB" {
		liveRef := h.State.CurrentMarketPrice
		if liveRef <= 0 {
			liveRef = h.State.LastPrice
		}
		if liveRef <= 0 {
			// No live market reference yet (fresh boot before the NT8 market /
			// bars sync lands). A WEB price patch cannot be validated against
			// anything, so it may be stale session state pushed by a
			// reconnecting tab â€” accepting it could seed planning levels far
			// from the real market (and, worse, dangerous bracket prices).
			// Reject all price fields; real planning levels come from the NT8
			// market-data / historical-bars sync.
			log.Printf("Stale-price guard: rejecting WEB price patch (no live market reference yet)")
			patch.EntryPrice = nil
			patch.StopPrice = nil
			patch.TargetPrice = nil
		} else {
			reject := func(label string, price *float64) bool {
				if price == nil || *price <= 0 || liveRef <= 0 {
					return false
				}
				dist := math.Abs(*price - liveRef)
				tick := h.State.TickSize
				if tick <= 0 {
					tick = 0.25
				}
				band := 200.0 * tick
				if pct := liveRef * 0.005; pct > band {
					band = pct
				}
				if n := len(h.BarCache); n >= 5 {
					hi := h.BarCache[n-1].High
					lo := h.BarCache[n-1].Low
					for i := n - 1; i >= 0 && i >= n-5; i-- {
						if h.BarCache[i].High > hi {
							hi = h.BarCache[i].High
						}
						if h.BarCache[i].Low < lo {
							lo = h.BarCache[i].Low
						}
					}
					if rng := (hi - lo) * 3; rng > band {
						band = rng
					}
				}
				if dist > band {
					log.Printf("Stale-price guard: ignoring %s %.2f (market %.2f, band %.2f)", label, *price, liveRef, band)
					return true
				}
				return false
			}
			if reject("entryPrice", patch.EntryPrice) {
				patch.EntryPrice = nil
			}
			if reject("stopPrice", patch.StopPrice) {
				patch.StopPrice = nil
			}
			if reject("targetPrice", patch.TargetPrice) {
				patch.TargetPrice = nil
			}
		}
	}

	if client.ClientType == "WEB" {
		normalizeWebPricePatch(h.State, patch.EntryPrice, patch.StopPrice, patch.TargetPrice)
	}

	if patch.SelectedRR != nil && *patch.SelectedRR > 0 {
		h.State.SelectedRR = *patch.SelectedRR
	}
	if patch.EntryPrice != nil {
		h.State.EntryPrice = *patch.EntryPrice
	}
	if patch.StopPrice != nil {
		h.State.StopPrice = *patch.StopPrice
	}
	if patch.TargetPrice != nil {
		h.State.TargetPrice = *patch.TargetPrice
		h.State.HasCustomTargets = true
		if len(h.State.TargetExits) > 0 {
			h.State.TargetExits[len(h.State.TargetExits)-1].Price = *patch.TargetPrice
		} else {
			h.State.TargetExits = []risk.TargetExit{
				{Qty: h.State.CalculatedQty, Ratio: h.State.SelectedRR, Price: *patch.TargetPrice},
			}
		}
		slDist := math.Abs(h.State.EntryPrice - h.State.StopPrice)
		if slDist > 0 {
			tpDist := math.Abs(h.State.TargetPrice - h.State.EntryPrice)
			computedRR := tpDist / slDist
			if computedRR >= 0.1 && computedRR <= 20.0 {
				h.State.SelectedRR = math.Round(computedRR*100) / 100
			}
		}
	}
	if h.State.SelectedRR > 20.0 || h.State.SelectedRR < 0.1 {
		h.State.SelectedRR = 2.0
	}
	if patch.TickSize != nil && *patch.TickSize > 0 {
		h.State.TickSize = *patch.TickSize
	}
	if patch.PointValue != nil && *patch.PointValue > 0 {
		h.State.PointValue = *patch.PointValue
	}
	if patch.InstrumentName != nil {
		h.State.InstrumentName = *patch.InstrumentName
	}
	if patch.AccountName != nil {
		h.State.AccountName = *patch.AccountName
		if h.State.SelectedAccount == "" {
			h.State.SelectedAccount = *patch.AccountName
		}
	}
	if len(patch.AvailableAccounts) > 0 {
		h.State.AvailableAccounts = patch.AvailableAccounts
		if h.State.SelectedAccount == "" {
			prefs := loadEnginePrefs()
			if prefs.DefaultAccount != "" {
				h.State.SelectedAccount = prefs.DefaultAccount
			} else {
				h.State.SelectedAccount = patch.AvailableAccounts[0]
			}
		}
	}
	if patch.AccountBalance != nil {
		h.State.AccountBalance = *patch.AccountBalance
		if patch.IsBalanceEstimated != nil && *patch.IsBalanceEstimated {
			log.Printf("NT8 gateway reported estimated/fallback balance: %.2f", *patch.AccountBalance)
		}
	}
	if patch.CurrentMarketPrice != nil && *patch.CurrentMarketPrice > 0 && !h.hasLiveMarketData {
		h.State.CurrentMarketPrice = *patch.CurrentMarketPrice
		// NOTE: NOT seeding the phantom entry here, and once live MARKET_DATA
		// ticks are flowing this chart-close-derived close is ignored entirely
		// (a stale chart would otherwise poison the stale-price guard's market
		// reference and block legitimate web drags).
		// MARKET_DATA ticks (handleMarketData) may bootstrap the planning
		// entry/SL/TP â€” otherwise a stale chart close could seed levels far
		// from the real market (and dangerous bracket prices).
	}
	risk.RecalculateState(h.State)

	// Independent lines: a WEB patch that explicitly carries a target price
	// means the user drew the target there â€” RecalculateState above just
	// re-derived it from stop distance Ã— R:R, so restore the drawn value. This
	// is what keeps dragging the STOP from silently moving the TARGET line.
	sentTarget := 0.0
	if patch.TargetPrice != nil && *patch.TargetPrice > 0 {
		sentTarget = *patch.TargetPrice
		h.State.TargetPrice = sentTarget
		if len(h.State.TargetExits) > 0 {
			h.State.TargetExits[len(h.State.TargetExits)-1].Price = sentTarget
		}
		h.State.HasCustomTargets = true
	}

	working := make([]risk.WorkingOrderInfo, len(h.State.WorkingOrders))
	copy(working, h.State.WorkingOrders)
	h.Mu.Unlock()

	go h.BroadcastState()

	// WEB-originated price patches are USER ACTIONS (dragging the lines/badges,
	// snap-to-market). Forward genuine, explicit price changes onto the REAL
	// working orders: Entry â†’ GoEntry (while flat), Stop â†’ ActiveSL brackets,
	// Target â†’ ActiveTP brackets. Only patches whose value actually differs from
	// the working order price trigger a CHANGE_ORDER, so silent/auto movement is
	// impossible â€” this restores dragging SL/TP after entry WITHOUT re-enabling
	// the old auto-align behavior (non-price market info from NT8 never enters
	// this loop).
	if client.ClientType == "WEB" && len(working) > 0 {
		for _, o := range working {
			if o.State != "Working" && o.State != "Accepted" {
				continue
			}
			isSL := o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_")
			isTP := o.Name == "ActiveTP" || strings.HasPrefix(o.Name, "ActiveTP")
			if patch.EntryPrice != nil && o.Name == "GoEntry" &&
				h.State.Position.Quantity == 0 && math.Abs(o.Price-*patch.EntryPrice) >= 0.01 {
				// A resting STOP/STOP-LIMIT entry dragged across the market is
				// invalid ("buy stop below market") â€” NT8 rejects the change and
				// the line snaps back. Gate it with the same validity check the
				// bracket stops use; limit entries stay free to rest anywhere.
				isStopType := o.OrderType == "StopMarket" || o.OrderType == "StopLimit"
				if isStopType && !h.isValidStopPrice(o, *patch.EntryPrice) {
					log.Printf("Entry drag: skipping invalid GoEntry price change for %s (entry %.2f priced through market)", o.OrderId, *patch.EntryPrice)
				} else {
					h.SendChangeOrderToNT8(o.OrderId, *patch.EntryPrice)
				}
			}
			if patch.StopPrice != nil && isSL && math.Abs(o.Price-*patch.StopPrice) >= 0.01 {
				if h.isValidStopPrice(o, *patch.StopPrice) {
					h.SendChangeOrderToNT8(o.OrderId, *patch.StopPrice)
				} else {
					log.Printf("Bracket drag: skipping invalid stop-price change for %s (stop %.2f priced through market)", o.OrderId, *patch.StopPrice)
				}
			}
			if patch.TargetPrice != nil && isTP && math.Abs(o.Price-*patch.TargetPrice) >= 0.01 {
				h.SendChangeOrderToNT8(o.OrderId, *patch.TargetPrice)
			}
		}
	}
}

func (h *Hub) handleUpdateTarget(client *Client, payload json.RawMessage) {
	var patch struct {
		Index int     `json:"index"`
		Price float64 `json:"price"`
	}
	if err := json.Unmarshal(payload, &patch); err != nil {
		return
	}

	h.Mu.Lock()
	if patch.Index >= 0 && patch.Index < len(h.State.TargetExits) {
		h.State.TargetExits[patch.Index].Price = patch.Price
		dist := math.Abs(h.State.EntryPrice - h.State.StopPrice)
		if dist > 0 {
			tpDist := math.Abs(patch.Price - h.State.EntryPrice)
			newRatio := tpDist / dist
			h.State.TargetExits[patch.Index].Ratio = newRatio
			if patch.Index == len(h.State.TargetExits)-1 {
				h.State.SelectedRR = newRatio
				risk.RecalculateState(h.State)
			}
		}
		h.State.HasCustomTargets = true

		working := make([]risk.WorkingOrderInfo, len(h.State.WorkingOrders))
		copy(working, h.State.WorkingOrders)
		h.Mu.Unlock()

		go h.BroadcastState()

		if client.ClientType == "WEB" && len(working) > 0 {
			targetName := "ActiveTP"
			if patch.Index > 0 {
				targetName = fmt.Sprintf("ActiveTP_%d", patch.Index+1)
			}
			for _, o := range working {
				if (o.State == "Working" || o.State == "Accepted") &&
					(o.Name == targetName) &&
					math.Abs(o.Price-patch.Price) >= 0.01 {
					h.SendChangeOrderToNT8(o.OrderId, patch.Price)
				}
			}
		}
	} else {
		h.Mu.Unlock()
	}
}

func (h *Hub) handleSetConfig(payload json.RawMessage) {
	var patch struct {
		IsLong                   *bool    `json:"isLong"`
		EntryModel               *string  `json:"entryModel"`
		RiskCash                 *float64 `json:"riskCash"`
		MaxContracts             *int     `json:"maxContracts"`
		SelectedRR               *float64 `json:"selectedRR"`
		IsPartialProfit          *bool    `json:"isPartialProfit"`
		FirstExitFraction        *float64 `json:"firstExitFraction"`
		SubsequentExitFraction   *float64 `json:"subsequentExitFraction"`
		SlippageCapTicks         *float64 `json:"slippageCapTicks"`
		SlippagePadTicks         *float64 `json:"slippagePadTicks"`
		IsSlippageSync           *bool    `json:"isSlippageSync"`
		EnableDynRisk            *bool    `json:"enableDynRisk"`
		MLL                      *float64 `json:"mll"`
		PenaltyThreshold         *float64 `json:"penaltyThreshold"`
		PenaltyRisk              *float64 `json:"penaltyRisk"`
		BaseRisk                 *float64 `json:"baseRisk"`
		ScaleFactor              *float64 `json:"scaleFactor"`
		MaxCap                   *float64 `json:"maxCap"`
		CommissionPerContract    *float64 `json:"commissionPerContract"`
		MaxEntrySlippageTicks    *int     `json:"maxEntrySlippageTicks"`
		BreakoutExpirySeconds    *float64 `json:"breakoutExpirySeconds"`
		AutoBEOnTP1              *bool    `json:"autoBEOnTP1"`
		AutoBEOffsetTicks        *int     `json:"autoBEOffsetTicks"`
		HotkeysArmed             *bool    `json:"hotkeysArmed"`
		TradingDisabled          *bool    `json:"tradingDisabled"`
		IsSlLocked               *bool    `json:"isSlLocked"`
		IsTpLocked               *bool    `json:"isTpLocked"`
		ShowProfitInTicks        *bool    `json:"showProfitInTicks"`
		ShowLines                *bool    `json:"showLines"`
		IsAutoTrackEnabled       *bool    `json:"isAutoTrackEnabled"`
		TrackAnchor              *string  `json:"trackAnchor"`
		TrackTimeframe           *string  `json:"trackTimeframe"`
		TrackOffsetTicks         *int     `json:"trackOffsetTicks"`
		SelectedAccount          *string  `json:"selectedAccount"`
		EnableHotkeys            *bool    `json:"enableHotkeys"`
		InstantEntryOffsetTicks  *int     `json:"instantEntryOffsetTicks"`
		BreakoutEntryOffsetTicks *int     `json:"breakoutEntryOffsetTicks"`
		TrailStopOffsetTicks     *int     `json:"trailStopOffsetTicks"`
		ScaleOutPercent          *float64 `json:"scaleOutPercent"`
		ScaleOutTimeoutSeconds   *float64 `json:"scaleOutTimeoutSeconds"`
		ScaleOutPriceMode        *string  `json:"scaleOutPriceMode"`
		InstantEntryMode         *string  `json:"instantEntryMode"`
	}
	if err := json.Unmarshal(payload, &patch); err != nil {
		return
	}

	h.Mu.Lock()
	if patch.SelectedAccount != nil && *patch.SelectedAccount != "" {
		h.State.SelectedAccount = *patch.SelectedAccount
		saveEnginePrefs(EnginePrefs{DefaultAccount: *patch.SelectedAccount})
	}
	if patch.IsLong != nil {
		if h.hasCommittedEntry() {
			log.Printf("Direction change rejected: trade is committed (position or working entry)")
		} else {
			oldLong := h.State.IsLong
			h.State.IsLong = *patch.IsLong
			if h.State.IsLong != oldLong && h.State.EntryPrice > 0 {
				h.State.StopPrice = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.StopPrice, h.State.TickSize)
				h.State.TargetPrice = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.TargetPrice, h.State.TickSize)
				for i := range h.State.TargetExits {
					h.State.TargetExits[i].Price = risk.RoundToTickSize(2*h.State.EntryPrice-h.State.TargetExits[i].Price, h.State.TickSize)
				}
			}
		}
	}
	if patch.EntryModel != nil {
		h.State.EntryModel = *patch.EntryModel
		h.State.IsLimitOrder = (*patch.EntryModel != "Market")
	}
	// If TP is LOCKED during a risk-cash recalibration, RecalculateState at the
	// end would silently re-derive the target from the new stop distance. We
	// remember the user's locked target here and re-apply it right after the
	// recalc so the locked line never moves.
	preserveLockedTarget := 0.0
	if patch.RiskCash != nil && *patch.RiskCash > 0 {
		h.State.RiskCash = *patch.RiskCash
		slDist := math.Abs(h.State.EntryPrice - h.State.StopPrice)
		riskPerContract := slDist * h.State.PointValue
		if riskPerContract > 0 {
			if h.State.IsSlLocked {
				// SL is LOCKED: changing risk must NEVER move the stop (or the
				// target â€” those are the user's committed levels). Only the
				// position SIZE adapts to the new risk at the locked distance;
				// RecalculateState at the end recomputes CalculatedQty.
				// (no price writes here)
			} else {
				// SL unlocked: keep the dollar risk exact by recalibrating the
				// stop distance (position size stays the rounded qty).
				qty := int(h.State.RiskCash / riskPerContract)
				if qty < 1 {
					qty = 1
				}
				if h.State.MaxContracts > 0 && qty > h.State.MaxContracts {
					qty = h.State.MaxContracts
				}
				newSlDist := h.State.RiskCash / (float64(qty) * h.State.PointValue)
				if h.State.IsLong {
					h.State.StopPrice = h.State.EntryPrice - newSlDist
				} else {
					h.State.StopPrice = h.State.EntryPrice + newSlDist
				}
				if !h.State.IsTpLocked {
					if h.State.IsLong {
						h.State.TargetPrice = h.State.EntryPrice + (newSlDist * h.State.SelectedRR)
					} else {
						h.State.TargetPrice = h.State.EntryPrice - (newSlDist * h.State.SelectedRR)
					}
					h.State.HasCustomTargets = false
				} else if h.State.TargetPrice > 0 {
					// TP locked: keep the user's target exactly where it is.
					preserveLockedTarget = h.State.TargetPrice
				}
			}
		}
	}
	if patch.MaxContracts != nil && *patch.MaxContracts > 0 {
		h.State.MaxContracts = *patch.MaxContracts
	}
	if patch.SelectedRR != nil {
		h.State.SelectedRR = *patch.SelectedRR
		h.State.HasCustomTargets = false
	}
	if patch.IsPartialProfit != nil {
		h.State.IsPartialProfit = *patch.IsPartialProfit
		h.State.HasCustomTargets = false
	}
	if patch.FirstExitFraction != nil {
		h.State.FirstExitFraction = *patch.FirstExitFraction
		h.State.HasCustomTargets = false
	}
	if patch.SubsequentExitFraction != nil {
		h.State.SubsequentExitFraction = *patch.SubsequentExitFraction
		h.State.HasCustomTargets = false
	}
	if patch.SlippageCapTicks != nil {
		h.State.SlippageCapTicks = *patch.SlippageCapTicks
	}
	if patch.SlippagePadTicks != nil {
		h.State.SlippagePadTicks = *patch.SlippagePadTicks
	}
	if patch.IsSlippageSync != nil {
		h.State.IsSlippageSync = *patch.IsSlippageSync
	}
	if patch.EnableDynRisk != nil {
		h.State.EnableDynRisk = *patch.EnableDynRisk
	}
	if patch.MLL != nil {
		h.State.MLL = *patch.MLL
	}
	if patch.PenaltyThreshold != nil {
		h.State.PenaltyThreshold = *patch.PenaltyThreshold
	}
	if patch.PenaltyRisk != nil {
		h.State.PenaltyRisk = *patch.PenaltyRisk
	}
	if patch.BaseRisk != nil {
		h.State.BaseRisk = *patch.BaseRisk
	}
	if patch.ScaleFactor != nil {
		h.State.ScaleFactor = *patch.ScaleFactor
	}
	if patch.MaxCap != nil {
		h.State.MaxCap = *patch.MaxCap
	}
	if patch.IsSlLocked != nil {
		h.State.IsSlLocked = *patch.IsSlLocked
	}
	if patch.IsTpLocked != nil {
		h.State.IsTpLocked = *patch.IsTpLocked
	}
	if patch.ShowProfitInTicks != nil {
		h.State.ShowProfitInTicks = *patch.ShowProfitInTicks
	}
	if patch.ShowLines != nil {
		h.State.ShowLines = *patch.ShowLines
	}
	if patch.IsAutoTrackEnabled != nil {
		if *patch.IsAutoTrackEnabled && h.hasCommittedEntry() {
			// A trade is ACTIVE (position open) or an entry order is committed
			// (working GoEntry). AutoTrack is a pre-trade planning tool that may
			// only re-anchor the phantom entry â€” turning it on now would never
			// affect the active trade (every shift site is guarded), and
			// showing ON would be misleading. Reject the enable: the button
			// snaps back to OFF on the next SYNC_STATE.
			h.State.IsAutoTrackEnabled = false
			log.Printf("AutoTrack enable rejected: trade is committed (position or entry order working)")
		} else {
			h.State.IsAutoTrackEnabled = *patch.IsAutoTrackEnabled
			if h.State.IsAutoTrackEnabled && !h.hasCommittedEntry() && len(h.BarCache) >= 2 {
				priorBar := h.BarCache[len(h.BarCache)-2]
				tick := h.State.TickSize
				if tick <= 0 {
					tick = 0.25
				}
				offset := float64(h.State.TrackOffsetTicks) * tick
				var targetEntry float64
				if h.State.IsLong {
					targetEntry = priorBar.High + offset
				} else {
					targetEntry = priorBar.Low - offset
				}
				targetEntry = risk.RoundToTickSize(targetEntry, tick)
				if h.State.EntryPrice > 0 && targetEntry > 0 {
					delta := targetEntry - h.State.EntryPrice
					h.State.EntryPrice = targetEntry
					if !h.State.IsSlLocked {
						h.State.StopPrice = risk.RoundToTickSize(h.State.StopPrice+delta, tick)
					}
					if !h.State.IsTpLocked {
						h.State.TargetPrice = risk.RoundToTickSize(h.State.TargetPrice+delta, tick)
						for i := range h.State.TargetExits {
							h.State.TargetExits[i].Price = risk.RoundToTickSize(h.State.TargetExits[i].Price+delta, tick)
						}
					}
				} else if targetEntry > 0 {
					h.State.EntryPrice = targetEntry
					slOffset := 20.0 * tick
					if h.State.IsLong {
						h.State.StopPrice = h.State.EntryPrice - slOffset
						h.State.TargetPrice = h.State.EntryPrice + (slOffset * h.State.SelectedRR)
					} else {
						h.State.StopPrice = h.State.EntryPrice + slOffset
						h.State.TargetPrice = h.State.EntryPrice - (slOffset * h.State.SelectedRR)
					}
				}
			}
		}
	}
	if patch.TrackAnchor != nil {
		h.State.TrackAnchor = *patch.TrackAnchor
	}
	if patch.TrackTimeframe != nil {
		// Restrict tracking to the supported bar series; the adoption logic in
		// handleSyncBars/handleBarUpdate watches for this label.
		switch *patch.TrackTimeframe {
		case "15s", "1m", "5m", "100t":
			h.State.TrackTimeframe = *patch.TrackTimeframe
		}
	}
	if patch.TrackOffsetTicks != nil {
		h.State.TrackOffsetTicks = *patch.TrackOffsetTicks
	}
	if patch.EnableHotkeys != nil {
		h.State.EnableHotkeys = *patch.EnableHotkeys
	}
	if patch.InstantEntryOffsetTicks != nil {
		h.State.InstantEntryOffsetTicks = *patch.InstantEntryOffsetTicks
	}
	if patch.BreakoutEntryOffsetTicks != nil {
		h.State.BreakoutEntryOffsetTicks = *patch.BreakoutEntryOffsetTicks
	}
	if patch.TrailStopOffsetTicks != nil {
		h.State.TrailStopOffsetTicks = *patch.TrailStopOffsetTicks
	}
	if patch.ScaleOutPercent != nil {
		h.State.ScaleOutPercent = *patch.ScaleOutPercent
	}
	if patch.ScaleOutTimeoutSeconds != nil {
		h.State.ScaleOutTimeoutSeconds = *patch.ScaleOutTimeoutSeconds
	}
	if patch.CommissionPerContract != nil && *patch.CommissionPerContract >= 0 {
		h.State.CommissionPerContract = *patch.CommissionPerContract
	}
	if patch.MaxEntrySlippageTicks != nil && *patch.MaxEntrySlippageTicks >= 0 {
		h.State.MaxEntrySlippageTicks = *patch.MaxEntrySlippageTicks
	}
	if patch.BreakoutExpirySeconds != nil && *patch.BreakoutExpirySeconds >= 0 {
		h.State.BreakoutExpirySeconds = *patch.BreakoutExpirySeconds
	}
	if patch.AutoBEOnTP1 != nil {
		h.State.AutoBEOnTP1 = *patch.AutoBEOnTP1
	}
	if patch.AutoBEOffsetTicks != nil {
		h.State.AutoBEOffsetTicks = *patch.AutoBEOffsetTicks
	}
	if patch.HotkeysArmed != nil {
		h.State.HotkeysArmed = *patch.HotkeysArmed
	}
	if patch.TradingDisabled != nil {
		h.State.TradingDisabled = *patch.TradingDisabled
	}
	if patch.ScaleOutPriceMode != nil {
		switch *patch.ScaleOutPriceMode {
		case "BarHighLow", "AskBid", "Candle1m":
			h.State.ScaleOutPriceMode = *patch.ScaleOutPriceMode
		}
	}
	if patch.InstantEntryMode != nil {
		switch *patch.InstantEntryMode {
		case "AskBid", "Market":
			h.State.InstantEntryMode = *patch.InstantEntryMode
		}
	}

	if preserveLockedTarget <= 0 && h.State.IsTpLocked && h.State.TargetPrice > 0 {
		preserveLockedTarget = h.State.TargetPrice
	}
	risk.RecalculateState(h.State)
	// Re-apply a TP the user locked during a risk-cash recalibration or anchor:
	// the recalc above re-derived it from the new stop distance, but a locked target never
	// moves (SL/TP lines move only when the user moves them).
	if preserveLockedTarget > 0 {
		h.State.TargetPrice = preserveLockedTarget
		if len(h.State.TargetExits) > 0 {
			h.State.TargetExits[len(h.State.TargetExits)-1].Price = preserveLockedTarget
		}
	}
	h.Mu.Unlock()
	go h.BroadcastState()
	go h.ForwardToNT8("SET_CONFIG", payload)
}

// BroadcastStateThrottled broadcasts the full TradeState to web clients at
// most once per window, for the high-frequency MARKET_DATA path ONLY. Market
// data is ingested at FULL tick rate (the hub's state is always fresh, and the
// small MARKET_DATA message is passed to the web on every tick), but a full
// SYNC_STATE per tick made the web re-render the whole UI at feed speed â€” this
// spaces those expensive resyncs out while prices still flow at full rate.
func (h *Hub) BroadcastStateThrottled() {
	h.Mu.Lock()
	if !h.lastStateBCast.IsZero() && time.Since(h.lastStateBCast) < 250*time.Millisecond {
		h.Mu.Unlock()
		return
	}
	h.lastStateBCast = time.Now()
	h.Mu.Unlock()
	h.BroadcastState()
}

func (h *Hub) handleMarketData(payload json.RawMessage) {
	var md risk.MarketDataUpdate
	if err := json.Unmarshal(payload, &md); err != nil {
		return
	}

	h.Mu.Lock()
	changed := false
	if md.Bid > 0 || md.Ask > 0 || md.Last > 0 {
		// Real MARKET_DATA ticks are the authoritative live market reference.
		// Once seen, chart-close-derived prices are never allowed to replace it.
		h.hasLiveMarketData = true
	}
	if md.Bid > 0 && md.Bid != h.State.CurrentBid {
		h.State.CurrentBid = md.Bid
		changed = true
	}
	if md.Ask > 0 && md.Ask != h.State.CurrentAsk {
		h.State.CurrentAsk = md.Ask
		changed = true
	}
	if md.Last > 0 {
		h.State.LastPrice = md.Last
		if h.State.CurrentMarketPrice <= 0 || math.Abs(h.State.CurrentMarketPrice-md.Last) >= 0.01 {
			h.State.CurrentMarketPrice = md.Last
			changed = true
		}
		// Live ticks may arrive BEFORE the historical-bars sync (page start):
		// seed the plan lines near the current price immediately so they are
		// visible the moment the page opens â€” no "snap" required.
		if h.State.EntryPrice <= 0 && !h.hasCommittedEntry() {
			h.seedPlanningLevelsFromMarket(md.Last)
			risk.RecalculateState(h.State)
			changed = true
		}
	}
	h.Mu.Unlock()
	if changed {
		// Every tick reaches the web as a small MARKET_DATA price message below,
		// but the full-state re-render is spaced out (see BroadcastStateThrottled).
		go h.BroadcastToWeb("MARKET_DATA", payload)
		go h.BroadcastStateThrottled()
	}
}

func (h *Hub) handleSyncBars(client *Client, payload json.RawMessage) {
	var bars []risk.ChartBar
	if err := json.Unmarshal(payload, &bars); err != nil || len(bars) == 0 {
		return
	}

	// Route by the timeframe tag: the engine's tracking stream (the legacy
	// untagged stream, or the stream matching State.TrackTimeframe when
	// present) feeds the state machine; every other tag fills its own cache
	// for the multi-pane web terminal.
	tf := bars[0].Timeframe

	h.Mu.Lock()
	isTracking := false
	if tf == "" {
		isTracking = true // legacy untagged stream
	} else if h.State.TrackTimeframe != "" && tf == h.State.TrackTimeframe {
		// The user-configurable tracking timeframe (default 15s): the engine's
		// AutoTrack/planning logic anchors on this stream's bars REGARDLESS of
		// which timeframes the web panes display.
		h.trackingTimeframe = tf
		isTracking = true
	} else if h.trackingTimeframe == "" {
		h.trackingTimeframe = tf // first labeled stream wins (NT8 primary)
		isTracking = true
	} else {
		isTracking = (tf == h.trackingTimeframe)
	}

	if isTracking {
		h.BarCache = dedupeBars(bars)
		lastBar := bars[len(bars)-1]
		if lastBar.Close > 0 && !h.hasLiveMarketData {
			h.State.CurrentMarketPrice = lastBar.Close
			// Chart closes are provisional until the first live MARKET_DATA tick
			// (which then becomes the authoritative market reference). Once live
			// ticks flow, a stale chart close must never poison the market
			// reference â€” that would make the stale-price guard reject legit
			// web drags. Not seeding the phantom entry here either.
		}
		h.mirrorTracking(tf)
	} else {
		h.TimeframeBars[tf] = dedupeBars(bars)
	}
	// Broadcast the CANONICAL cache â€” deduped, time-ascending, capped â€” NOT the
	// raw NT8 payload. NT8 answers both SUBSCRIBE and GET_BARS with a full
	// history, so a pane subscribe can deliver the same batch twice; re-syncing
	// the deduped cache means the web only ever receives ONE canonical batch per
	// series. The web's own merge logic is then consistent with the hub, and the
	// "same-time bar kept twice because two captures differ in the forming
	// candle's volume" duplicate is impossible at the broadcast boundary.
	broadcast := h.BarCache
	if !isTracking {
		broadcast = h.TimeframeBars[tf]
	}
	canonical, err := json.Marshal(broadcast)
	h.Mu.Unlock()
	log.Printf("Received %d historical bars (tf=%q, tracking=%q) from %s", len(bars), tf, h.trackingTimeframe, client.ClientType)
	if err != nil {
		return
	}
	go h.BroadcastToWeb("SYNC_BARS", canonical)
}

// dedupeBars collapses FULLY IDENTICAL copies (same time+OHLC+volume â€” i.e.
// repeated re-syncs of the same historical bar) to one, keeps every DISTINCT
// bar â€” including bars that share a timestamp but differ in values (NT8
// closes multiple 100t bars in one second plus a session-close duplicate) â€”
// and returns the list time-ascending (stable).
func dedupeBars(bars []risk.ChartBar) []risk.ChartBar {
	if len(bars) <= 1 {
		return bars
	}
	seen := make(map[string]bool, len(bars))
	out := make([]risk.ChartBar, 0, len(bars))
	for _, b := range bars {
		key := fmt.Sprintf("%d|%d|%v|%v|%v|%v|%v", b.Time, b.BarIndex, b.Open, b.High, b.Low, b.Close, b.Volume)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

func (h *Hub) handleBarUpdate(payload json.RawMessage) {
	var bar risk.ChartBar
	if err := json.Unmarshal(payload, &bar); err != nil {
		return
	}

	tf := bar.Timeframe
	h.Mu.Lock()

	// Adopt the tracking stream (mirrors handleSyncBars): prefer the stream
	// matching State.TrackTimeframe whenever it exists; otherwise the first
	// labeled stream (the NT8 chart primary) wins.
	if tf != "" && h.State.TrackTimeframe != "" && tf == h.State.TrackTimeframe {
		h.trackingTimeframe = tf
	} else if tf != "" && h.trackingTimeframe == "" {
		h.trackingTimeframe = tf
	}

	// Non-tracking timeframe streams only refresh their own pane cache â€” they
	// never touch the engine's tracking state (AutoTrack anchors, planning
	// seed, current price all stay bound to the tracking stream).
	if tf != "" && tf != h.trackingTimeframe {
		store := h.TimeframeBars[tf]
		if len(store) > 0 && bar.BarIndex > 0 {
			// BarIndex-identified pane (tick/volume, e.g. 100t): REPLACE the
			// bar with the same index (a forming-candle re-emission updates the
			// existing candle in place) or APPEND a new index (a closed bar).
			// Genuinely distinct same-second bars have different indices, so
			// both survive; phantom forming snapshots cannot accumulate.
			replaced := false
			for i := len(store) - 1; i >= 0; i-- {
				if store[i].BarIndex == bar.BarIndex {
					if bar.Time < store[i].Time {
						break // out-of-order stale frame; drop
					}
					store[i] = bar
					replaced = true
					break
				}
			}
			if !replaced && bar.Time >= store[len(store)-1].Time {
				store = append(store, bar)
				if len(store) > maxCachedBars {
					store = store[len(store)-maxCachedBars:]
				}
			}
		} else if len(store) > 0 {
			lastIdx := len(store) - 1
			if store[lastIdx].Time == bar.Time {
				// Same timestamp: keep BOTH when the values differ (NT8 closes
				// multiple 100t bars in one second); drop only an identical
				// repeat of the last bar.
				if !(store[lastIdx].Time == bar.Time && store[lastIdx].Open == bar.Open &&
					store[lastIdx].High == bar.High && store[lastIdx].Low == bar.Low &&
					store[lastIdx].Close == bar.Close && store[lastIdx].Volume == bar.Volume) {
					store = append(store, bar)
					if len(store) > maxCachedBars {
						store = store[len(store)-maxCachedBars:]
					}
				}
			} else if bar.Time > store[lastIdx].Time {
				store = append(store, bar)
				if len(store) > maxCachedBars {
					store = store[len(store)-maxCachedBars:]
				}
			}
			// Older-than-last frames are ignored (the new gateway emits one
			// bar per close in time order; a later SYNC heals any gap).
		} else {
			store = append(store, bar)
		}
		h.TimeframeBars[tf] = store
		h.Mu.Unlock()
		go h.BroadcastToWeb("BAR_UPDATE", payload)
		return
	}

	if len(h.BarCache) > 0 {
		lastIdx := len(h.BarCache) - 1
		if h.BarCache[lastIdx].Time == bar.Time {
			h.BarCache[lastIdx] = bar
			// Forming-bar update (same bar, live values): the CurrentBarHighLow
			// anchor tracks the live bar as it rises/falls; prior-bar and EMA
			// anchors are close-driven and must not move on forming updates.
			if h.State.IsAutoTrackEnabled && !h.hasCommittedEntry() && h.State.TrackAnchor == "CurrentBarHighLow" {
				h.reanchorAutoTrack()
			}
		} else if bar.Time > h.BarCache[lastIdx].Time {
			h.BarCache = append(h.BarCache, bar)
			if len(h.BarCache) > 1000 {
				h.BarCache = h.BarCache[len(h.BarCache)-1000:]
			}
			// AutoTrack may only re-anchor the phantom entry/SL/TP while the
			// user is still PLANNING (flat AND no entry order committed). Once
			// an entry is submitted â€” even if the fill/position update races
			// behind this bar close â€” the levels are the user's and must not
			// move: "SL/TP never moves unless I actually move it."
			if h.State.IsAutoTrackEnabled && !h.hasCommittedEntry() {
				h.reanchorAutoTrack()
			}
		}
	} else {
		h.BarCache = append(h.BarCache, bar)
	}
	if bar.Close > 0 && !h.hasLiveMarketData {
		h.State.CurrentMarketPrice = bar.Close
		// Provisional market reference before the first live tick; once live
		// MARKET_DATA flows it is authoritative the stale-price guard ignores
		// chart closes entirely. Not seeding the phantom entry from bar closes.
		// can be stale/different-instrument; only live MARKET_DATA ticks seed
		// the planning entry/SL/TP (see handleMarketData).
	}
	h.mirrorTracking(tf)
	h.Mu.Unlock()
	go h.BroadcastToWeb("BAR_UPDATE", payload)
}

// mirrorTracking keeps the labeled cache for the engine's tracking stream in
// sync with BarCache, so any web pane can display the tracking timeframe.
// Callers MUST hold h.Mu (write). tf == "" (legacy untagged stream) is skipped.
func (h *Hub) mirrorTracking(tf string) {
	if tf != "" {
		h.TimeframeBars[tf] = h.BarCache
	}
}

// trackingBars returns the bar cache the engine's AutoTrack anchors on. When
// the user has configured a TrackTimeframe ("1m"/"5m"/"100t") that stream is
// cached in TimeframeBars; otherwise it is the tracking BarCache (default
// 15s). Caller must hold h.Mu (read or write).
func (h *Hub) trackingBars() []risk.ChartBar {
	tf := h.State.TrackTimeframe
	if tf != "" && tf != h.trackingTimeframe {
		if bars := h.TimeframeBars[tf]; len(bars) > 0 {
			return bars
		}
	}
	return h.BarCache
}

// trackTargetFromBars computes the AutoTrack target entry price from the
// anchored bar series per the configured TrackAnchor:
//   - "CurrentBarHighLow": the most recent (forming) bar's High/Low
//   - "PriorHighLow" (default): the prior (closed) bar's High/Low
//
// Returns 0 when no bar qualifies. Caller must hold h.Mu (read or write).
func (h *Hub) trackTargetFromBars(bars []risk.ChartBar) float64 {
	if len(bars) == 0 {
		return 0
	}
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	offset := float64(h.State.TrackOffsetTicks) * tick

	var anchor risk.ChartBar
	switch h.State.TrackAnchor {
	case "CurrentBarHighLow":
		anchor = bars[len(bars)-1]
	case "PriorHighLow":
		if len(bars) < 2 {
			return 0
		}
		anchor = bars[len(bars)-2]
	default:
		if len(bars) < 2 {
			return 0
		}
		anchor = bars[len(bars)-2]
	}

	if h.State.IsLong {
		return anchor.High + offset
	}
	return anchor.Low - offset
}

// reanchorAutoTrack moves the phantom entry/SL/TP to the tracked series'
// anchor (see trackTargetFromBars). It only runs while the user is still
// PLANNING (flat AND no entry order committed) â€” AutoTrack must never move
// committed levels. Caller MUST hold h.Mu (write).
func (h *Hub) reanchorAutoTrack() {
	targetEntry := h.trackTargetFromBars(h.trackingBars())
	if targetEntry <= 0 {
		return
	}
	tick := h.State.TickSize
	if tick <= 0 {
		tick = 0.25
	}
	targetEntry = risk.RoundToTickSize(targetEntry, tick)
	if h.State.EntryPrice > 0 && math.Abs(targetEntry-h.State.EntryPrice) >= tick {
		delta := targetEntry - h.State.EntryPrice
		h.State.EntryPrice = targetEntry
		if !h.State.IsSlLocked {
			h.State.StopPrice = risk.RoundToTickSize(h.State.StopPrice+delta, tick)
		}
		if !h.State.IsTpLocked {
			h.State.TargetPrice = risk.RoundToTickSize(h.State.TargetPrice+delta, tick)
			for i := range h.State.TargetExits {
				h.State.TargetExits[i].Price = risk.RoundToTickSize(h.State.TargetExits[i].Price+delta, tick)
			}
		}
		preserveLockedTarget := 0.0
		if h.State.IsTpLocked && h.State.TargetPrice > 0 {
			preserveLockedTarget = h.State.TargetPrice
		}
		risk.RecalculateState(h.State)
		if preserveLockedTarget > 0 {
			h.State.TargetPrice = preserveLockedTarget
			if len(h.State.TargetExits) > 0 {
				h.State.TargetExits[len(h.State.TargetExits)-1].Price = preserveLockedTarget
			}
		}
		go h.BroadcastState()
	}
}

func (h *Hub) handleAvailableAccounts(payload json.RawMessage) {
	var accounts []string
	if err := json.Unmarshal(payload, &accounts); err != nil || len(accounts) == 0 {
		return
	}

	h.Mu.Lock()
	h.State.AvailableAccounts = accounts
	if h.State.SelectedAccount == "" {
		prefs := loadEnginePrefs()
		if prefs.DefaultAccount != "" {
			h.State.SelectedAccount = prefs.DefaultAccount
		} else {
			h.State.SelectedAccount = accounts[0]
		}
	}
	h.Mu.Unlock()
	log.Printf("Received %d available accounts from NT8: %v", len(accounts), accounts)
	go h.BroadcastState()
}

func (h *Hub) handlePositionUpdate(payload json.RawMessage) {
	var pos risk.PositionInfo
	if err := json.Unmarshal(payload, &pos); err != nil {
		return
	}

	h.Mu.Lock()
	// Account and instrument isolation
	if pos.AccountName != "" && h.State.SelectedAccount != "" &&
		!strings.EqualFold(pos.AccountName, h.State.SelectedAccount) {
		h.Mu.Unlock()
		return
	}
	if !sameInstrument(pos.InstrumentName, h.State.InstrumentName) {
		h.Mu.Unlock()
		return
	}

	h.State.Position = pos
	if pos.Quantity == 0 || pos.MarketPosition == "Flat" {
		if h.State.CommandState == "PENDING_FLATTEN" {
			h.State.CommandState = "IDLE"
		}
	}
	// If a position is open but the entry price was lost (hub restarted /
	// reconnected mid-trade), restore it from the REAL average fill price â€”
	// never from a bar close. Does not touch a user-set entry price.
	if (pos.Quantity > 0 || (pos.MarketPosition != "" && pos.MarketPosition != "Flat")) &&
		h.State.EntryPrice <= 0 && pos.AveragePrice > 0 {
		tick := h.State.TickSize
		if tick <= 0 {
			tick = 0.25
		}
		h.State.EntryPrice = risk.RoundToTickSize(pos.AveragePrice, tick)
	}
	h.Mu.Unlock()
	go h.BroadcastState()
	if pos.Quantity > 0 {
		h.syncBracketsToPosition()
	}
	h.reconcileBrokerProtection()
}

func (h *Hub) handleOrdersUpdate(payload json.RawMessage) {
	var orders []risk.WorkingOrderInfo
	if err := json.Unmarshal(payload, &orders); err != nil {
		return
	}

	h.Mu.Lock()
	// Filter orders strictly to current SelectedAccount and InstrumentName
	var filtered []risk.WorkingOrderInfo
	for _, o := range orders {
		if o.AccountName != "" && h.State.SelectedAccount != "" &&
			!strings.EqualFold(o.AccountName, h.State.SelectedAccount) {
			continue
		}
		if !sameInstrument(o.InstrumentName, h.State.InstrumentName) {
			continue
		}
		filtered = append(filtered, o)
	}

	h.State.WorkingOrders = filtered
	if h.State.Position.Quantity > 0 {
		for _, o := range filtered {
			if o.State != "Working" && o.State != "Accepted" {
				continue
			}
			if o.Name == "ActiveSL" || strings.HasPrefix(o.Name, "ActiveSL_") {
				if o.Price > 0 {
					h.State.StopPrice = o.Price
				}
			} else if o.Name == "ActiveTP" || strings.HasPrefix(o.Name, "ActiveTP") {
				if o.Price > 0 {
					h.State.TargetPrice = o.Price
				}
			}
		}
	}
	h.Mu.Unlock()
	go h.BroadcastState()
	h.mergeBracketsAtSamePrice()
	h.Mu.RLock()
	posQty := h.State.Position.Quantity
	h.Mu.RUnlock()
	if posQty > 0 {
		h.syncBracketsToPosition()
	}
	h.reconcileBrokerProtection()
}

func (h *Hub) handleExecutionUpdate(payload json.RawMessage) {
	var exec risk.ExecutionUpdateInfo
	if err := json.Unmarshal(payload, &exec); err != nil {
		return
	}

	h.Mu.RLock()
	selAcc := h.State.SelectedAccount
	inst := h.State.InstrumentName
	autoBE := h.State.AutoBEOnTP1
	autoBEOffset := h.State.AutoBEOffsetTicks
	h.Mu.RUnlock()

	// Account isolation guard
	if exec.AccountName != "" && selAcc != "" && !strings.EqualFold(exec.AccountName, selAcc) {
		return
	}

	log.Printf("Received EXECUTION_UPDATE: OrderId=%s Name=%s Action=%s State=%s FillQty=%d FillPrice=%.2f Account=%s",
		exec.OrderId, exec.Name, exec.Action, exec.OrderState, exec.FillQty, exec.FillPrice, exec.AccountName)

	go logging.RecordAudit(logging.AuditEvent{
		EventType:      "FILL",
		AccountName:    exec.AccountName,
		InstrumentName: inst,
		OrderId:        exec.OrderId,
		Action:         exec.Action,
		Qty:            exec.FillQty,
		FillPrice:      exec.FillPrice,
		Success:        true,
		Details:        fmt.Sprintf("Order %s state %s", exec.Name, exec.OrderState),
	})

	if strings.HasPrefix(exec.Name, "GoEntry") && exec.FillQty > 0 &&
		(exec.OrderState == "Filled" || exec.OrderState == "PartFilled") {

		h.Mu.Lock()
		h.State.IsAutoTrackEnabled = false
		h.Mu.Unlock()

		h.bracketMu.Lock()
		h.pendingFillQty += exec.FillQty
		h.pendingExec = exec
		if h.bracketTimer != nil {
			h.bracketTimer.Stop()
		}
		h.bracketTimer = time.AfterFunc(100*time.Millisecond, func() {
			h.dispatchPendingBrackets()
		})
		h.bracketMu.Unlock()
	}

	if (strings.HasPrefix(exec.Name, "ActiveTP") || strings.HasPrefix(exec.Name, "ActiveSL") ||
		strings.HasPrefix(exec.Name, "ScaleOut")) && exec.FillQty > 0 {
		h.Mu.Lock()
		if h.State.Position.Quantity <= exec.FillQty {
			h.State.Position = risk.PositionInfo{MarketPosition: "Flat", Quantity: 0}
		} else {
			h.State.Position.Quantity -= exec.FillQty
		}
		h.Mu.Unlock()
		log.Printf("Exit filled (%s): Position is now %s (Qty: %d)", exec.Name, h.State.Position.MarketPosition, h.State.Position.Quantity)
		go h.BroadcastState()

		// Auto-Breakeven on TP1 fill
		if autoBE && (strings.HasPrefix(exec.Name, "ActiveTP_1") || exec.Name == "ActiveTP") &&
			(exec.OrderState == "Filled" || exec.OrderState == "PartFilled") {
			log.Printf("Auto-BE on TP1 triggered by %s fill", exec.Name)
			go h.hotkeyBreakeven(autoBEOffset)
		}

		if strings.HasPrefix(exec.Name, "ScaleOut") {
			h.syncBracketsToPosition()
		}
		h.reconcileBrokerProtection()
	}
}
