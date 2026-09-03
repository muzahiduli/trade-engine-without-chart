package risk

import (
	"fmt"
	"math"
	"strings"
)

// RoundToTickSize rounds a given price to the nearest tick increment.
func RoundToTickSize(price, tickSize float64) float64 {
	if tickSize <= 0 {
		return price
	}
	return math.Round(price/tickSize) * tickSize
}

// CalculatePositionSize calculates position quantity based on cash risk and stop loss distance.
// Uses floor-based sizing so quantity * riskPerContract never exceeds configured risk (except 1 contract min).
func CalculatePositionSize(riskVal, slDistance, pointValue float64, maxContracts int) int {
	return CalculatePositionSizeWithCommission(riskVal, slDistance, pointValue, 0, maxContracts)
}

// CalculatePositionSizeWithCommission calculates floor position size including per-contract commission.
func CalculatePositionSizeWithCommission(riskVal, slDistance, pointValue, commission float64, maxContracts int) int {
	riskPerContract := (slDistance * pointValue) + commission
	if riskPerContract <= 0 {
		return 1
	}

	rawSize := riskVal / riskPerContract
	positionSize := int(math.Floor(rawSize))

	if positionSize < 1 {
		positionSize = 1
	}
	if maxContracts > 0 && positionSize > maxContracts {
		positionSize = maxContracts
	}
	return positionSize
}

// CalculatePaddedPositionSize factors in slippage padding ticks into position sizing.
func CalculatePaddedPositionSize(riskVal, slDistance, slippagePad, tickSize, pointValue float64, maxContracts int) int {
	return CalculatePaddedPositionSizeWithCommission(riskVal, slDistance, slippagePad, tickSize, pointValue, 0, maxContracts)
}

// CalculatePaddedPositionSizeWithCommission factors in slippage padding ticks and commission into position sizing.
func CalculatePaddedPositionSizeWithCommission(riskVal, slDistance, slippagePad, tickSize, pointValue, commission float64, maxContracts int) int {
	stopDistancePadded := slDistance + (slippagePad * tickSize)
	riskPerContract := (stopDistancePadded * pointValue) + commission
	if riskPerContract <= 0 {
		return 1
	}

	rawSize := riskVal / riskPerContract
	qty := int(math.Floor(rawSize))

	if qty < 1 {
		qty = 1
	}
	if maxContracts > 0 && qty > maxContracts {
		qty = maxContracts
	}
	return qty
}

// CalculateDynamicRisk calculates survival penalty or compounding growth risk based on account balance vs MLL.
func CalculateDynamicRisk(accountBalance, mll, penaltyThreshold, penaltyRisk, baseRisk, scaleFactor, maxCap, defaultRisk float64) (float64, string, float64) {
	if accountBalance <= 0 {
		return defaultRisk, "NORMAL", 0
	}

	buffer := accountBalance - mll
	if buffer < penaltyThreshold {
		r := penaltyRisk
		if r <= 0 {
			r = defaultRisk
		}
		return r, "SURVIVAL", buffer
	}

	excess := buffer - penaltyThreshold
	calculatedRisk := baseRisk + (excess * scaleFactor)
	if calculatedRisk > maxCap {
		calculatedRisk = maxCap
	}
	if calculatedRisk <= 0 {
		calculatedRisk = defaultRisk
	}
	return calculatedRisk, "GROWTH", buffer
}

// GetSubsequentTargetsStats computes partial target counts and distribution ratios.
func GetSubsequentTargetsStats(totalQty int, firstExitFraction, subsequentExitFraction float64) (int, float64) {
	subCount := 0
	sumQtyIndex := 0.0

	qty1 := int(math.Round(float64(totalQty) * firstExitFraction))
	if qty1 < 1 {
		qty1 = 1
	}
	if qty1 > totalQty {
		qty1 = totalQty
	}

	remaining := totalQty - qty1
	if remaining <= 0 {
		return subCount, sumQtyIndex
	}

	subQty := int(math.Round(float64(remaining) * subsequentExitFraction))
	if subQty < 1 {
		subQty = 1
	}
	if subQty > remaining {
		subQty = remaining
	}

	tempRemaining := remaining
	for tempRemaining > 0 {
		subCount++
		currentSubQty := subQty
		if currentSubQty >= tempRemaining {
			currentSubQty = tempRemaining
			tempRemaining = 0
		} else {
			tempRemaining -= currentSubQty
		}
		sumQtyIndex += float64(currentSubQty * subCount)
	}

	return subCount, sumQtyIndex
}

// GetTargetExits calculates scaled profit targets and exit quantities.
func GetTargetExits(totalQty int, entry, slDist, selectedRR float64, isPartialProfit bool, firstExitFraction, subsequentExitFraction float64, isLong bool, tickSize float64, hasCustom bool, existingExits []TargetExit) []TargetExit {
	exits := make([]TargetExit, 0)
	if totalQty <= 0 {
		totalQty = 1
	}

	if !isPartialProfit || selectedRR <= 1.0 {
		var targetPr float64
		if isLong {
			targetPr = entry + selectedRR*slDist
		} else {
			targetPr = entry - selectedRR*slDist
		}
		targetPr = RoundToTickSize(targetPr, tickSize)
		exits = append(exits, TargetExit{Qty: totalQty, Ratio: selectedRR, Price: targetPr})
		return exits
	}

	qty1 := int(math.Round(float64(totalQty) * firstExitFraction))
	if qty1 < 1 {
		qty1 = 1
	}
	if qty1 >= totalQty && totalQty > 1 {
		qty1 = totalQty - 1
	}

	var targetPr1 float64
	if isLong {
		targetPr1 = entry + slDist
	} else {
		targetPr1 = entry - slDist
	}
	targetPr1 = RoundToTickSize(targetPr1, tickSize)

	remaining := totalQty - qty1
	if remaining <= 0 {
		var targetPr2 float64
		if isLong {
			targetPr2 = entry + selectedRR*slDist
		} else {
			targetPr2 = entry - selectedRR*slDist
		}
		targetPr2 = RoundToTickSize(targetPr2, tickSize)

		exits = append(exits, TargetExit{Qty: 1, Ratio: 1.0, Price: targetPr1})
		exits = append(exits, TargetExit{Qty: 1, Ratio: selectedRR, Price: targetPr2})
		return exits
	}

	subCount, sumQtyIndex := GetSubsequentTargetsStats(totalQty, firstExitFraction, subsequentExitFraction)

	maxRatio := selectedRR
	if sumQtyIndex > 0 {
		maxRatio = 1.0 + float64(totalQty)*(selectedRR-1.0)*float64(subCount)/sumQtyIndex
	}

	subQty := int(math.Round(float64(remaining) * subsequentExitFraction))
	if subQty < 1 {
		subQty = 1
	}
	if subQty > remaining {
		subQty = remaining
	}

	exits = append(exits, TargetExit{Qty: qty1, Ratio: 1.0, Price: targetPr1})

	subIdx := 0
	for remaining > 0 {
		subIdx++
		currentSubQty := subQty
		if currentSubQty >= remaining {
			currentSubQty = remaining
			remaining = 0
		} else {
			remaining -= currentSubQty
		}

		ratio := 1.0
		if subCount > 0 {
			ratio = 1.0 + float64(subIdx)*(maxRatio-1.0)/float64(subCount)
		}

		var targetPrSub float64
		if isLong {
			targetPrSub = entry + ratio*slDist
		} else {
			targetPrSub = entry - ratio*slDist
		}
		targetPrSub = RoundToTickSize(targetPrSub, tickSize)
		exits = append(exits, TargetExit{Qty: currentSubQty, Ratio: ratio, Price: targetPrSub})
	}

	if hasCustom && len(existingExits) == len(exits) && slDist > 0 {
		for i := range exits {
			exits[i].Ratio = existingExits[i].Ratio
			if isLong {
				exits[i].Price = RoundToTickSize(entry + exits[i].Ratio*slDist, tickSize)
			} else {
				exits[i].Price = RoundToTickSize(entry - exits[i].Ratio*slDist, tickSize)
			}
		}
	}

	return exits
}

// CalculateStopLimitLimitPrice returns the cap price for a Stop-Limit order.
func CalculateStopLimitLimitPrice(entryPrice, slippageCap, tickSize float64, isLong bool) float64 {
	offset := slippageCap * tickSize
	if isLong {
		return entryPrice + offset
	}
	return entryPrice - offset
}

// CalculateSlippageRisk calculates potential risk exposure given a specific slippage in ticks.
func CalculateSlippageRisk(qty int, slDistance, slippageTicks, tickSize, pointValue float64) float64 {
	stopDistancePadded := slDistance + (slippageTicks * tickSize)
	return float64(qty) * stopDistancePadded * pointValue
}

// CalculateAutoTrackPrice solves for the entry price when auto-tracking price action.
func CalculateAutoTrackPrice(priorHighLow float64, trackOffset int, tickSize float64, isLong, isBreakout bool) float64 {
	offset := float64(trackOffset) * tickSize
	if isBreakout {
		if isLong {
			return priorHighLow + offset
		}
		return priorHighLow - offset
	}
	// Pullback
	if isLong {
		return priorHighLow - offset
	}
	return priorHighLow + offset
}

// RecalculateState recomputes all derived fields in a TradeState.
func RecalculateState(state *TradeState) {
	if state.TickSize <= 0 {
		state.TickSize = 0.25
	}
	if state.PointValue <= 0 {
		state.PointValue = 20.0 // Default for NQ/ES if unset
	}
	if state.RiskCash <= 0 {
		state.RiskCash = 100.0
	}
	if state.MaxContracts <= 0 {
		state.MaxContracts = 10
	}
	if state.SlippagePadTicks < 0 {
		state.SlippagePadTicks = 0
	}
	if state.CommissionPerContract < 0 {
		state.CommissionPerContract = 0
	}
	if state.FirstExitFraction <= 0 {
		state.FirstExitFraction = 0.3333
	}
	if state.SubsequentExitFraction <= 0 {
		state.SubsequentExitFraction = 0.50
	}

	// Dynamic Risk Calculation if enabled
	effectiveRisk := state.RiskCash
	if state.EnableDynRisk {
		calculatedRisk, dynState, buffer := CalculateDynamicRisk(
			state.AccountBalance,
			state.MLL,
			state.PenaltyThreshold,
			state.PenaltyRisk,
			state.BaseRisk,
			state.ScaleFactor,
			state.MaxCap,
			state.RiskCash,
		)
		effectiveRisk = calculatedRisk
		state.DynRiskState = dynState
		state.DynRiskBuffer = buffer
	} else {
		state.DynRiskState = "MANUAL"
		state.DynRiskBuffer = 0
	}

	// Ensure prices are tick-rounded
	state.EntryPrice = RoundToTickSize(state.EntryPrice, state.TickSize)
	state.StopPrice = RoundToTickSize(state.StopPrice, state.TickSize)

	slDist := math.Abs(state.EntryPrice - state.StopPrice)
	// Only synthesize a default stop when the user actually HAS an entry level
	// with no stop yet. With no entry at all (fresh boot, or hub restarted
	// mid-trade) we must NEVER invent Stop/Target prices — those levels only
	// come from the user or from real working bracket orders. Inventing them
	// was the source of the "SL/TP move after candle close" bug family.
	if slDist == 0 && state.EntryPrice > 0 {
		defaultDist := 20 * state.TickSize // 20 ticks default
		if state.IsLong {
			state.StopPrice = RoundToTickSize(state.EntryPrice-defaultDist, state.TickSize)
		} else {
			state.StopPrice = RoundToTickSize(state.EntryPrice+defaultDist, state.TickSize)
		}
		slDist = defaultDist
	}

	// Position sizing needs a reference distance even when no levels exist yet
	// (pre-entry planning on a fresh boot): use a nominal 20 ticks for the size
	// math ONLY — without writing any price into the state.
	sizeDist := slDist
	if sizeDist <= 0 {
		sizeDist = 20 * state.TickSize
	}

	// Calculate Position Size with padding if set, using floor-based sizing and commission
	if state.SlippagePadTicks > 0 {
		state.CalculatedQty = CalculatePaddedPositionSizeWithCommission(
			effectiveRisk, sizeDist, state.SlippagePadTicks, state.TickSize, state.PointValue, state.CommissionPerContract, state.MaxContracts,
		)
	} else {
		state.CalculatedQty = CalculatePositionSizeWithCommission(
			effectiveRisk, sizeDist, state.PointValue, state.CommissionPerContract, state.MaxContracts,
		)
	}

	// Detect if 1 contract minimum exceeds configured risk
	costPerContract := (sizeDist * state.PointValue) + state.CommissionPerContract
	if effectiveRisk > 0 && costPerContract > effectiveRisk {
		state.IsRiskExceeded = true
		state.RiskExcessAmount = costPerContract - effectiveRisk
	} else {
		state.IsRiskExceeded = false
		state.RiskExcessAmount = 0
	}

	// Partial Profit requires at least 2 contracts for multiple targets
	if state.IsPartialProfit && state.CalculatedQty < 2 && state.MaxContracts >= 2 {
		state.CalculatedQty = 2
	}

	// Calculate Target Exits — ONLY when an entry level exists. With no entry
	// level, target prices are never fabricated: they stay exactly as they were
	// (usually 0 until the user draws them or a real bracket syncs in).
	if state.EntryPrice > 0 {
		state.TargetExits = GetTargetExits(
			state.CalculatedQty,
			state.EntryPrice,
			slDist,
			state.SelectedRR,
			state.IsPartialProfit,
			state.FirstExitFraction,
			state.SubsequentExitFraction,
			state.IsLong,
			state.TickSize,
			state.HasCustomTargets,
			state.TargetExits,
		)

		// Update Target Price fields for NT8 visual sync
		if len(state.TargetExits) > 0 {
			state.TargetPrice = state.TargetExits[len(state.TargetExits)-1].Price
			state.TargetPrice1 = state.TargetExits[0].Price
		} else {
			if state.IsLong {
				state.TargetPrice = RoundToTickSize(state.EntryPrice+state.SelectedRR*slDist, state.TickSize)
			} else {
				state.TargetPrice = RoundToTickSize(state.EntryPrice-state.SelectedRR*slDist, state.TickSize)
			}
			state.TargetPrice1 = state.TargetPrice
		}
	} else {
		state.TargetPrice1 = state.TargetPrice
	}

	// Actual Dollar Risk & Reward (including commission)
	state.ActualRiskAmount = float64(state.CalculatedQty) * ((slDist * state.PointValue) + state.CommissionPerContract)

	rewardSum := 0.0
	for _, exit := range state.TargetExits {
		dist := math.Abs(exit.Price - state.EntryPrice)
		rewardSum += float64(exit.Qty) * dist * state.PointValue
	}
	state.ActualRewardAmount = rewardSum

	if state.ActualRiskAmount > 0 {
		state.CalculatedRR = math.Round((state.ActualRewardAmount/state.ActualRiskAmount)*100) / 100
	} else {
		state.CalculatedRR = state.SelectedRR
	}

	// Slippage Exposure Grid
	state.Slippage0Risk = CalculateSlippageRisk(state.CalculatedQty, slDist, 0, state.TickSize, state.PointValue)
	state.Slippage4Risk = CalculateSlippageRisk(state.CalculatedQty, slDist, 4, state.TickSize, state.PointValue)
	state.Slippage8Risk = CalculateSlippageRisk(state.CalculatedQty, slDist, 8, state.TickSize, state.PointValue)

	// Effective entry order type — mirrors BuildExecutionPlan's breakout
	// decision so the web terminal renders exactly what the engine would
	// submit: a long entry ABOVE the market (or short entry BELOW it) is a
	// STOP-LIMIT breakout entry, everything else is a LIMIT pullback resting
	// order. Before any live market reference exists (fresh boot) we show the
	// user-configured model instead — there is nothing to judge breakout
	// against yet. The UI no longer re-derives this from prices.
	if state.CurrentMarketPrice > 0 && state.EntryPrice > 0 {
		state.IsBreakout = (state.IsLong && state.EntryPrice > state.CurrentMarketPrice) ||
			(!state.IsLong && state.EntryPrice < state.CurrentMarketPrice)
		if state.IsBreakout {
			state.EffectiveEntryModel = "STOP-LIMIT"
		} else {
			state.EffectiveEntryModel = "LIMIT"
		}
	} else {
		state.IsBreakout = false
		state.EffectiveEntryModel = strings.ToUpper(state.EntryModel)
		if state.EffectiveEntryModel == "" {
			state.EffectiveEntryModel = "LIMIT"
		}
	}

	if state.IsRiskExceeded {
		state.Status = fmt.Sprintf("WARN: 1 contract ($%.2f) exceeds risk ($%.2f) by $%.2f | Qty: %d",
			costPerContract, effectiveRisk, state.RiskExcessAmount, state.CalculatedQty)
	} else {
		state.Status = fmt.Sprintf("READY | Qty: %d | Risk: $%.2f | Reward: $%.2f (1:%.2f)",
			state.CalculatedQty, state.ActualRiskAmount, state.ActualRewardAmount, state.CalculatedRR)
	}
}

// BuildExecutionPlan creates a completely resolved ExecutionPlan where all decisions
// (breakout vs pullback, order type, slippage caps, limit/stop prices, bracket levels)
// are 100% computed on the Go backend.
func BuildExecutionPlan(state *TradeState, currentMarketPrice float64) ExecutionPlan {
	targetAcc := state.SelectedAccount
	if targetAcc == "" {
		targetAcc = state.AccountName
	}

	plan := ExecutionPlan{
		AccountName:   targetAcc,
		Qty:           state.CalculatedQty,
		StopLossPrice: state.StopPrice,
		TargetExits:   state.TargetExits,
	}

	if plan.Qty <= 0 {
		plan.Qty = 1
	}

	if state.IsLong {
		plan.Action = "BUY"
	} else {
		plan.Action = "SELL_SHORT"
	}

	tick := state.TickSize
	if tick <= 0 {
		tick = 0.25
	}

	if state.EntryPrice <= 0 && currentMarketPrice > 0 {
		state.EntryPrice = currentMarketPrice
		slOffset := 20.0 * tick
		if state.IsLong {
			state.StopPrice = state.EntryPrice - slOffset
			state.TargetPrice = state.EntryPrice + (slOffset * state.SelectedRR)
		} else {
			state.StopPrice = state.EntryPrice + slOffset
			state.TargetPrice = state.EntryPrice - (slOffset * state.SelectedRR)
		}
		RecalculateState(state)
		plan.Qty = state.CalculatedQty
		plan.StopLossPrice = state.StopPrice
		plan.TargetExits = state.TargetExits
	}

	// Determine Breakout condition vs current market price
	isBreakout := false
	if currentMarketPrice > 0 {
		if state.IsLong && state.EntryPrice > currentMarketPrice {
			isBreakout = true
		} else if !state.IsLong && state.EntryPrice < currentMarketPrice {
			isBreakout = true
		}
	}

	// Automatic smart order type based purely on price vs current market price:
	// - If BUY and Entry > Market: STOP-LIMIT (Breakout)
	// - If BUY and Entry <= Market: LIMIT (Pullback resting order)
	// - If SELL_SHORT and Entry < Market: STOP-LIMIT (Breakout)
	// - If SELL_SHORT and Entry >= Market: LIMIT (Pullback resting order)
	// LimitPrice is ALWAYS set to EntryPrice (zero unwanted padding!)
	if isBreakout {
		plan.OrderType = "StopLimit"
		plan.StopPrice = state.EntryPrice
		// Sanitize the stop trigger against the tightest market reference the
		// hub has (Ask for buys, Bid for sells, else the last price): NinjaTrader
		// rejects buy stop/stop-limit triggers AT OR BELOW the market (and sell
		// stops AT OR ABOVE). The breakout decision above used the possibly
		// stale last price, so clamp the trigger to >= 1 tick beyond the live
		// reference instead of letting the broker reject the order.
		plan.StopPrice = sanitizeStopTrigger(state.IsLong, plan.StopPrice, state.CurrentBid, state.CurrentAsk, currentMarketPrice, tick)
		plan.LimitPrice = plan.StopPrice
	} else {
		plan.OrderType = "Limit"
		plan.LimitPrice = state.EntryPrice
		plan.StopPrice = 0
	}

	return plan
}

// sanitizeStopTrigger ensures a stop entry trigger sits on the VALID side of
// the market: >= 1 tick above the ask (long) or <= 1 tick below the bid
// (short), falling back to the last-price reference when bid/ask are unknown
// (e.g. Market Replay without a live quote) OR when the quote deviates from
// the live last beyond the plausibility band (stale quotes caused buy stops
// below NinjaTrader's real market).
func sanitizeStopTrigger(isLong bool, trigger, bid, ask, last, tick float64) float64 {
	band := 25.0 * tick
	if isLong {
		ref := ask
		if ref <= 0 || (last > 0 && math.Abs(ref-last) > band) {
			ref = last
		}
		if ref > 0 && trigger <= ref {
			return RoundToTickSize(ref+tick, tick)
		}
		return trigger
	}
	ref := bid
	if ref <= 0 || (last > 0 && math.Abs(ref-last) > band) {
		ref = last
	}
	if ref > 0 && trigger >= ref {
		return RoundToTickSize(ref-tick, tick)
	}
	return trigger
}
