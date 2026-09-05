package risk

import "encoding/json"

// WSMessage represents the envelope for all WebSocket communication.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	ReqId   string          `json:"reqId,omitempty"`
}

// TargetExit represents an individual bracket target exit.
type TargetExit struct {
	Qty   int     `json:"qty"`
	Ratio float64 `json:"ratio"`
	Price float64 `json:"price"`
}

// ChartBar represents an OHLCV candlestick bar for charting.
type ChartBar struct {
	Time   int64   `json:"time"` // Unix timestamp in seconds
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume float64 `json:"volume"`
	// Timeframe tags which bar series this bar belongs to ("15s", "1m", "5m",
	// "100t"). Empty means the legacy/primary stream (the NT8 strategy's own
	// chart series).
	Timeframe string `json:"timeframe,omitempty"`
	// BarIndex is the NT8 series-local bar index (the position in the series'
	// bar collection). It is the IDENTITY of a tick/volume bar: forming-candle
	// re-emissions reuse the SAME index (so consumers upsert in place — one
	// live candle, never a phantom per tick), while genuinely distinct
	// same-second closed bars have different indices and must both be kept.
	BarIndex int64 `json:"barIndex,omitempty"`
}

// SubscribeCmd represents a subscription request from Web to NT8.
type SubscribeCmd struct {
	Instrument    string `json:"instrument"`
	Timeframe     int    `json:"timeframe"`
	TimeframeType string `json:"timeframeType"`
}

// TradeState represents the central state of the Risk/Reward trading engine.
type TradeState struct {
	// Instrument Details
	InstrumentName    string   `json:"instrumentName"`
	AccountName       string   `json:"accountName"`
	AvailableAccounts []string `json:"availableAccounts"`
	SelectedAccount   string   `json:"selectedAccount"`
	TickSize          float64  `json:"tickSize"`
	PointValue        float64  `json:"pointValue"`

	// Price Levels
	EntryPrice   float64 `json:"entryPrice"`
	StopPrice    float64 `json:"stopPrice"`
	TargetPrice  float64 `json:"targetPrice"`  // Final / Target 2
	TargetPrice1 float64 `json:"targetPrice1"` // First partial target

	// Execution Model & Direction
	IsLong       bool   `json:"isLong"`
	EntryModel   string `json:"entryModel"` // "Limit", "StopLimit", "StopMarket", "Market"
	IsLimitOrder bool   `json:"isLimitOrder"`

	// Risk & Quantity Settings
	RiskCash     float64 `json:"riskCash"`
	MaxContracts int     `json:"maxContracts"`
	SelectedRR   float64 `json:"selectedRR"`

	// Partial Profit Settings
	IsPartialProfit        bool    `json:"isPartialProfit"`
	FirstExitFraction      float64 `json:"firstExitFraction"`
	SubsequentExitFraction float64 `json:"subsequentExitFraction"`
	HasCustomTargets       bool    `json:"hasCustomTargets"`

	// Slippage Management
	SlippageCapTicks float64 `json:"slippageCapTicks"`
	SlippagePadTicks float64 `json:"slippagePadTicks"`
	IsSlippageSync   bool    `json:"isSlippageSync"`

	// Dynamic Risk Management
	EnableDynRisk    bool    `json:"enableDynRisk"`
	AccountBalance   float64 `json:"accountBalance"`
	MLL              float64 `json:"mll"`
	PenaltyThreshold float64 `json:"penaltyThreshold"`
	PenaltyRisk      float64 `json:"penaltyRisk"`
	BaseRisk         float64 `json:"baseRisk"`
	ScaleFactor      float64 `json:"scaleFactor"`
	MaxCap           float64 `json:"maxCap"`
	DynRiskState     string  `json:"dynRiskState"`
	DynRiskBuffer    float64 `json:"dynRiskBuffer"`

	// Commission & Slippage Overhead
	CommissionPerContract float64 `json:"commissionPerContract"`
	MaxEntrySlippageTicks int     `json:"maxEntrySlippageTicks"`
	BreakoutExpirySeconds float64 `json:"breakoutExpirySeconds"`

	// Auto-Breakeven on TP1
	AutoBEOnTP1       bool `json:"autoBEOnTP1"`
	AutoBEOffsetTicks int  `json:"autoBEOffsetTicks"`

	// Locks & Display Settings
	IsSlLocked        bool `json:"isSlLocked"`
	IsTpLocked        bool `json:"isTpLocked"`
	ShowProfitInTicks bool `json:"showProfitInTicks"`
	ShowLines         bool `json:"showLines"`

	// Auto-Tracking
	IsAutoTrackEnabled bool   `json:"isAutoTrackEnabled"`
	TrackAnchor        string `json:"trackAnchor"` // "PriorHighLow", "CurrentBarHighLow"
	// TrackTimeframe is the bar series the engine's AutoTrack / planning
	// anchors on ("15s" | "1m" | "5m" | "100t"), independent of which
	// timeframes the web panes display.
	TrackTimeframe   string `json:"trackTimeframe"`
	TrackOffsetTicks int    `json:"trackOffsetTicks"`

	// Live Market Data (streamed from NT8 via OnMarketData)
	CurrentBid float64 `json:"currentBid"`
	CurrentAsk float64 `json:"currentAsk"`
	LastPrice  float64 `json:"lastPrice"`

	// Hotkey Management
	EnableHotkeys            bool    `json:"enableHotkeys"`
	HotkeysArmed             bool    `json:"hotkeysArmed"`
	// HotkeyAddonConnected reports whether the NT8 hotkey-forwarding AddOn is
	// connected to the hub right now.
	HotkeyAddonConnected bool `json:"hotkeyAddonConnected"`
	// HotkeyForwardingEnabled is the AddOn's current forward on/off state
	// (toggled with plain 'L' in NinjaTrader; reported via HOTKEY_STATUS).
	HotkeyForwardingEnabled bool `json:"hotkeyForwardingEnabled"`
	InstantEntryOffsetTicks  int     `json:"instantEntryOffsetTicks"`
	InstantEntryMode         string  `json:"instantEntryMode"` // "AskBid" (buy ask/sell bid) or "Market"
	BreakoutEntryOffsetTicks int     `json:"breakoutEntryOffsetTicks"`
	TrailStopOffsetTicks     int     `json:"trailStopOffsetTicks"`
	ScaleOutPercent          float64 `json:"scaleOutPercent"`
	ScaleOutTimeoutSeconds   float64 `json:"scaleOutTimeoutSeconds"`
	// ScaleOutPriceMode: "BarHighLow" (tracking bar H/L), "AskBid" (sell bid/buy
	// ask), "Candle1m" (current 1m candle H/L).
	ScaleOutPriceMode string `json:"scaleOutPriceMode"`

	// Calculated Outputs (Engine Computes)
	CalculatedQty      int          `json:"calculatedQty"`
	ActualRiskAmount   float64      `json:"actualRiskAmount"`
	ActualRewardAmount float64      `json:"actualRewardAmount"`
	CalculatedRR       float64      `json:"calculatedRR"`
	TargetExits        []TargetExit `json:"targetExits"`
	Slippage0Risk      float64      `json:"slippage0Risk"`
	Slippage4Risk      float64      `json:"slippage4Risk"`
	Slippage8Risk      float64      `json:"slippage8Risk"`
	IsRiskExceeded     bool         `json:"isRiskExceeded"`
	RiskExcessAmount   float64      `json:"riskExcessAmount"`

	// Effective entry order type: the ORDER the engine would actually submit
	// for the current entry vs market ("LIMIT" pullback resting order or
	// "STOP-LIMIT" breakout entry), mirroring BuildExecutionPlan. The web
	// terminal renders this field verbatim instead of re-deriving the
	// decision client-side (client-side derivation drifted from the engine
	// and produced misleading execute-button/badge text). IsBreakout is the
	// same decision as a boolean (long entry above market / short entry
	// below market) for the AUTO badge label.
	EffectiveEntryModel string `json:"effectiveEntryModel"`
	IsBreakout          bool   `json:"isBreakout"`

	// System & Safety Status
	Status                string             `json:"status"`
	LastLog               string             `json:"lastLog"`
	CurrentMarketPrice    float64            `json:"currentMarketPrice"`
	TradingDisabled       bool               `json:"tradingDisabled"`
	IsUnprotectedPosition bool               `json:"isUnprotectedPosition"`
	ProtectionAlert       string             `json:"protectionAlert,omitempty"`
	CommandState          string             `json:"commandState,omitempty"` // "IDLE", "PENDING", "CONFIRMED", "REJECTED"
	ScaleOutState         string             `json:"scaleOutState,omitempty"`
	Position              PositionInfo       `json:"position"`
	WorkingOrders         []WorkingOrderInfo `json:"workingOrders"`
}

// WorkingOrderInfo represents an active resting order on the broker/exchange.
type WorkingOrderInfo struct {
	OrderId        string  `json:"orderId"`
	Name           string  `json:"name"`
	Action         string  `json:"action"`
	OrderType      string  `json:"orderType"`
	Price          float64 `json:"price"`
	Qty            int     `json:"qty"`
	State          string  `json:"state"`
	AccountName    string  `json:"accountName,omitempty"`
	InstrumentName string  `json:"instrumentName,omitempty"`
	Tag            string  `json:"tag,omitempty"`
}

// PositionInfo represents current open market position details.
type PositionInfo struct {
	MarketPosition   string  `json:"marketPosition"` // "Flat", "Long", "Short"
	Quantity         int     `json:"quantity"`
	AveragePrice     float64 `json:"averagePrice"`
	UnrealizedPnL    float64 `json:"unrealizedPnL"`
	UnrealizedPoints float64 `json:"unrealizedPoints"`
	AccountName      string  `json:"accountName,omitempty"`
	InstrumentName   string  `json:"instrumentName,omitempty"`
}

// ExecutionPlan is the completely computed, explicit order instruction sent to broker/NT8.
// The execution client (NT8) performs ZERO decision-making or math when executing this.
type ExecutionPlan struct {
	AccountName   string       `json:"accountName"`   // Target broker account
	Action        string       `json:"action"`        // "BUY" or "SELL_SHORT"
	OrderType     string       `json:"orderType"`     // "Market", "Limit", "StopLimit", "StopMarket"
	Qty           int          `json:"qty"`           // Total position quantity
	LimitPrice    float64      `json:"limitPrice"`    // Limit price (0 if not applicable)
	StopPrice     float64      `json:"stopPrice"`     // Stop price for Stop/StopLimit orders (0 if not applicable)
	StopLossPrice float64      `json:"stopLossPrice"` // Exact Stop Loss price for brackets
	TargetExits   []TargetExit `json:"targetExits"`   // Array of {qty, ratio, price} for brackets
}

// ExecuteOrderCmd represents an execution request routed to NinjaTrader or other brokers.
type ExecuteOrderCmd struct {
	Action      string       `json:"action"` // "EXECUTE", "CANCEL"
	Direction   string       `json:"direction"`
	OrderType   string       `json:"orderType"`
	EntryPrice  float64      `json:"entryPrice"`
	StopPrice   float64      `json:"stopPrice"`
	TargetPrice float64      `json:"targetPrice"`
	Qty         int          `json:"qty"`
	TargetExits []TargetExit `json:"targetExits"`
	IsAutoTrack bool         `json:"isAutoTrack"`
}

// ExecutionUpdateInfo represents execution reports streamed from NT8.
type ExecutionUpdateInfo struct {
	ExecutionId string  `json:"executionId"`
	OrderId     string  `json:"orderId"`
	Name        string  `json:"name"`
	Action      string  `json:"action"`
	FillPrice   float64 `json:"fillPrice"`
	FillQty     int     `json:"fillQty"`
	OrderState  string  `json:"orderState"`
	AccountName string  `json:"accountName"`
}

// SingleOrderCmd represents an explicit individual order to be submitted by the remote adapter.
type SingleOrderCmd struct {
	AccountName string  `json:"accountName"`
	Instrument  string  `json:"instrument,omitempty"`
	Action      string  `json:"action"`    // "BUY", "SELL", "SELL_SHORT", "BUY_TO_COVER"
	OrderType   string  `json:"orderType"` // "Limit", "StopMarket", "StopLimit", "Market"
	Qty         int     `json:"qty"`
	LimitPrice  float64 `json:"limitPrice"`
	StopPrice   float64 `json:"stopPrice"`
	OcoId       string  `json:"ocoId"`
	Name        string  `json:"name"`
	Tag         string  `json:"tag,omitempty"`
}

// BatchSubmitOrdersCmd represents a batch of orders (e.g. SL + TP OCO brackets).
type BatchSubmitOrdersCmd struct {
	Orders []SingleOrderCmd `json:"orders"`
}

// CommandAck represents broker or engine acknowledgements.
type CommandAck struct {
	Command string `json:"command"`
	OrderId string `json:"orderId"`
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// MarketDataUpdate represents a live bid/ask/last tick streamed from the NT8 gateway.
type MarketDataUpdate struct {
	Bid  float64 `json:"bid"`
	Ask  float64 `json:"ask"`
	Last float64 `json:"last"`
}

// HotkeyCmd represents a keyboard-driven trade management command dispatched from the web terminal.
type HotkeyCmd struct {
	Action string `json:"action"` // "INSTANT_ENTRY" | "BREAKOUT_ENTRY" | "TRAIL_STOP" | "SWAP_DIRECTION" | "SCALE_OUT" | "FLATTEN"
}
