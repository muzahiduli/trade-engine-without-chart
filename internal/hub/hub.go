package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"trade-engine-without-chart/internal/logging"
	"trade-engine-without-chart/internal/risk"
)

// Hub maintains the set of active clients and broadcasts messages to the clients.
type Hub struct {
	// Registered clients.
	Clients map[*Client]bool

	// Inbound messages from the clients.
	Broadcast chan []byte

	// Register requests from the clients.
	Register chan *Client

	// Unregister requests from clients.
	Unregister chan *Client

	// Current engine state
	State *risk.TradeState

	// Mutex protecting state
	Mu sync.RWMutex

	// Historical Bars Cache (primary / legacy stream â€” the engine's tracking
	// series, e.g. 15s). Also the fallback for bars without a timeframe tag.
	BarCache []risk.ChartBar

	// Per-timeframe historical bar caches for the multi-chart web terminal.
	// Keyed by timeframe label ("15s","1m","5m","100t"). The engine's tracking
	// stream lives in BarCache, not here, so the state machine (AutoTrack,
	// planning seed, current price) keeps exactly its current single-stream
	// behavior; the tracking label mirrors into TimeframeBars so any web pane
	// can also display it.
	TimeframeBars map[string][]risk.ChartBar

	// trackingTimeframe is the label of the engine's tracking stream. It
	// adopts the FIRST bar stream the gateway sends: the legacy untagged stream
	// (""), or the primary chart series' label when the NT8 chart matches one
	// of the supported web timeframes. Once set it never changes for this hub.
	trackingTimeframe string

	// lastStateBCast timestamps the most recent full-state broadcast on the
	// high-frequency MARKET_DATA path (see BroadcastStateThrottled). Guards
	// under h.Mu.
	lastStateBCast time.Time

	// hasLiveMarketData is set true the first time real MARKET_DATA ticks
	// arrive. Until then, chart-close-derived prices (SYNC_BARS / BAR_UPDATE /
	// UPDATE_PRICES currentMarketPrice) are the only market reference; once a
	// live tick has been seen, chart closes may be stale (different instrument
	// / delayed chart data) and must NEVER replace the live market price.
	hasLiveMarketData bool

	// Debounced Bracket Assembly
	pendingFillQty   int
	pendingExec      risk.ExecutionUpdateInfo
	bracketTimer     *time.Timer
	bracketMu        sync.Mutex
	cancellingOrders sync.Map
}

// maxCachedBars caps each non-primary per-timeframe bar cache. Sized to hold a
// full trading session of tick/volume bars (a day of 100t bars can exceed 2k).
const maxCachedBars = 10000

func NewHub() *Hub {
	return &Hub{
		Broadcast:     make(chan []byte),
		Register:      make(chan *Client),
		Unregister:    make(chan *Client),
		Clients:       make(map[*Client]bool),
		BarCache:      make([]risk.ChartBar, 0, 1000),
		TimeframeBars: map[string][]risk.ChartBar{},
		State: &risk.TradeState{
			InstrumentName:           "NQ 03-25",
			AccountName:              "Sim101",
			AccountBalance:           100000.0,
			EntryPrice:               0.0,
			StopPrice:                0.0,
			TargetPrice:              0.0,
			CalculatedQty:            1,
			MaxContracts:             10,
			SelectedRR:               2.0,
			IsLong:                   true,
			IsLimitOrder:             true,
			EntryModel:               "Limit",
			RiskCash:                 100.0,
			TickSize:                 0.25,
			PointValue:               20.0,
			CommissionPerContract:    0.0,
			MaxEntrySlippageTicks:    4,
			BreakoutExpirySeconds:    0.0,
			AutoBEOnTP1:              true,
			AutoBEOffsetTicks:        1,
			IsPartialProfit:          false,
			FirstExitFraction:        0.5,
			SubsequentExitFraction:   0.5,
			SlippageCapTicks:         4.0,
			SlippagePadTicks:         1.0,
			IsSlippageSync:           false,
			EnableDynRisk:            false,
			MLL:                      2500.0,
			PenaltyThreshold:         1000.0,
			PenaltyRisk:              50.0,
			BaseRisk:                 100.0,
			ScaleFactor:              1.5,
			MaxCap:                   300.0,
			IsSlLocked:               false,
			IsTpLocked:               false,
			ShowProfitInTicks:        false,
			ShowLines:                true,
			IsAutoTrackEnabled:       false,
			TrackAnchor:              "PriorHighLow",
			TrackTimeframe:           "15s",
			TrackOffsetTicks:         0,
			SelectedAccount:          "",
			AvailableAccounts:        []string{},
			EnableHotkeys:            true,
			HotkeysArmed:             true,
			TradingDisabled:          false,
			IsUnprotectedPosition:    false,
			CommandState:             "IDLE",
			ScaleOutState:            "IDLE",
			InstantEntryOffsetTicks:  1,
			BreakoutEntryOffsetTicks: 1,
			TrailStopOffsetTicks:     1,
			ScaleOutPercent:          50.0,
			ScaleOutTimeoutSeconds:   2.5,
			ScaleOutPriceMode:        "BarHighLow",
			TargetExits: []risk.TargetExit{
				{Qty: 1, Ratio: 2.0, Price: 20040.0},
			},
			Position:      risk.PositionInfo{MarketPosition: "Flat", Quantity: 0},
			WorkingOrders: []risk.WorkingOrderInfo{},
		},
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.Mu.Lock()
			h.Clients[client] = true
			h.Mu.Unlock()
			log.Printf("Client connected: %s (Total clients: %d)", client.ClientType, len(h.Clients))
			if client.ClientType == "WEB" {
				go h.SendStateToClient(client)
				go h.SendBarsToClient(client, "")
			}

		case client := <-h.Unregister:
			h.Mu.Lock()
			if _, ok := h.Clients[client]; ok {
				delete(h.Clients, client)
				close(client.Send)
			}
			h.Mu.Unlock()
			log.Printf("Client disconnected: %s (Total clients: %d)", client.ClientType, len(h.Clients))

		case message := <-h.Broadcast:
			// Under RLock we only collect slow clients; the close/delete must
			// happen under the WRITE lock â€” evicting under RLock raced a
			// concurrent reader's send (send-on-closed-channel panic killed the
			// hub when a WEB client disconnected mid-GET_BARS).
			h.Mu.RLock()
			var slow []*Client
			for client := range h.Clients {
				if !sendToClient(client, message) {
					slow = append(slow, client)
				}
			}
			h.Mu.RUnlock()
			for _, client := range slow {
				h.Mu.Lock()
				if _, ok := h.Clients[client]; ok {
					delete(h.Clients, client)
					close(client.Send)
					log.Printf("Client evicted (slow consumer): %s", client.ClientType)
				}
				h.Mu.Unlock()
			}
		}
	}
}

// sendToClient delivers a message to a client's outbound queue, returning
// false if the queue is full OR the client disconnected (channel closed).
// Sending to a closed channel panics by default â€” the disconnect path (and
// slow-client eviction) can close client.Send while a background send
// (SendBarsToClient / SendStateToClient) is still in flight, so every
// channel send in the hub goes through this guarded helper.
func sendToClient(client *Client, bytes []byte) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false // channel closed mid-send â€” client is gone
		}
	}()
	select {
	case client.Send <- bytes:
		return true
	default:
		return false
	}
}

// BroadcastToWeb broadcasts a message only to connected WEB clients.
func (h *Hub) BroadcastToWeb(msgType string, payload json.RawMessage) {
	msg := risk.WSMessage{
		Type:    msgType,
		Payload: payload,
	}
	bytes, err := json.Marshal(msg)
	if err != nil {
		return
	}

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	for client := range h.Clients {
		if client.ClientType == "WEB" {
			sendToClient(client, bytes)
		}
	}
}

// ForwardToNT8 forwards execution commands directly to connected adapter clients.
func (h *Hub) ForwardToNT8(msgType string, payload json.RawMessage) {
	msg := risk.WSMessage{
		Type:    msgType,
		Payload: payload,
		ReqId:   fmt.Sprintf("%d", time.Now().UnixNano()),
	}
	bytes, err := json.Marshal(msg)
	if err != nil {
		return
	}

	h.Mu.RLock()
	defer h.Mu.RUnlock()
	for client := range h.Clients {
		if client.ClientType == "NT8" || client.ClientType == "ADAPTER" {
			sendToClient(client, bytes)
		}
	}
}

// HandleMessage processes incoming messages from clients and delegates to specialized handlers.
func (h *Hub) HandleMessage(client *Client, msg risk.WSMessage) {
	switch msg.Type {
	case "UPDATE_PRICES":
		h.handleUpdatePrices(client, msg.Payload)

	case "UPDATE_TARGET":
		h.handleUpdateTarget(client, msg.Payload)

	case "SET_CONFIG":
		h.handleSetConfig(msg.Payload)

	case "MARKET_DATA":
		h.handleMarketData(msg.Payload)

	case "HOTKEY":
		var hk risk.HotkeyCmd
		if err := json.Unmarshal(msg.Payload, &hk); err == nil {
			h.HandleHotkey(hk.Action)
		}

	case "EXECUTE_ORDER":
		h.Mu.RLock()
		if h.State.TradingDisabled {
			h.Mu.RUnlock()
			log.Printf("EXECUTE_ORDER rejected: trading is disabled by Kill-Switch")
			return
		}
		if h.hasCommittedEntry() {
			h.Mu.RUnlock()
			log.Printf("EXECUTE_ORDER rejected: an entry is already committed")
			return
		}
		plan := risk.BuildExecutionPlan(h.State, h.State.CurrentMarketPrice)
		inst := h.State.InstrumentName
		h.Mu.RUnlock()

		h.freezeAutoTrackOnEntry()

		entryCmd := risk.SingleOrderCmd{
			AccountName: plan.AccountName,
			Instrument:  inst,
			Action:      plan.Action,
			OrderType:   plan.OrderType,
			Qty:         plan.Qty,
			LimitPrice:  plan.LimitPrice,
			StopPrice:   plan.StopPrice,
			OcoId:       "",
			Name:        "GoEntry",
		}
		entryBytes, err := json.Marshal(entryCmd)
		if err == nil {
			log.Printf("Submitting Go-Computed Entry Order to NT8: %s", string(entryBytes))
			go h.ForwardToNT8("SUBMIT_ORDER", entryBytes)
			go logging.RecordAudit(logging.AuditEvent{
				EventType:      "ORDER_SUBMIT",
				AccountName:    plan.AccountName,
				InstrumentName: inst,
				Action:         plan.Action,
				OrderType:      plan.OrderType,
				Qty:            plan.Qty,
				Price:          plan.LimitPrice,
				Success:        true,
				Details:        "Go-computed entry order submitted",
			})
		} else {
			log.Printf("Error marshaling entry order: %v", err)
		}
		go h.BroadcastState()

	case "CANCEL_ORDER":
		log.Printf("Routing CANCEL_ORDER to NT8: %s", string(msg.Payload))
		go h.ForwardToNT8("CANCEL_ORDER", msg.Payload)
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "ORDER_CANCEL",
			AccountName:    h.State.SelectedAccount,
			InstrumentName: h.State.InstrumentName,
			Success:        true,
			Details:        string(msg.Payload),
		})

	case "CHANGE_ORDER":
		log.Printf("Routing CHANGE_ORDER to NT8: %s", string(msg.Payload))
		go h.ForwardToNT8("CHANGE_ORDER", msg.Payload)
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "ORDER_CHANGE",
			AccountName:    h.State.SelectedAccount,
			InstrumentName: h.State.InstrumentName,
			Success:        true,
			Details:        string(msg.Payload),
		})

	case "SPLIT_TARGET":
		h.splitTargetOrder(msg.Payload)

	case "FLATTEN_POSITION":
		log.Printf("Routing FLATTEN_POSITION to NT8")
		h.Mu.Lock()
		h.State.CommandState = "PENDING_FLATTEN"
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
		go logging.RecordAudit(logging.AuditEvent{
			EventType:      "FLATTEN",
			AccountName:    acc,
			InstrumentName: inst,
			Success:        true,
			Details:        "FLATTEN_POSITION routed to NT8",
		})

	case "LOG_MESSAGE":
		var logMsg struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Payload, &logMsg); err == nil {
			// Also write to the engine log so NT8-side events (e.g. order
			// rejections) are visible here, not just in NT8's Output window.
			log.Printf("NT8 LOG: %s", logMsg.Text)
			h.Mu.Lock()
			h.State.LastLog = logMsg.Text
			h.Mu.Unlock()
			go h.BroadcastState()
		}

	case "GET_STATE":
		go h.SendStateToClient(client)

	case "SYNC_BARS":
		h.handleSyncBars(client, msg.Payload)

	case "BAR_UPDATE":
		h.handleBarUpdate(msg.Payload)

	case "SUBSCRIBE":
		log.Printf("Forwarding SUBSCRIBE command to NT8: %s", string(msg.Payload))
		go h.ForwardToNT8("SUBSCRIBE", msg.Payload)

	case "GET_BARS":
		// Payload: {"count":N} (legacy primary) or {"count":N,"timeframe":"15s"}.
		go h.ForwardToNT8("GET_BARS", msg.Payload)
		var req struct {
			Timeframe string `json:"timeframe"`
		}
		_ = json.Unmarshal(msg.Payload, &req)
		go h.SendBarsToClient(client, req.Timeframe)

	case "AVAILABLE_ACCOUNTS":
		h.handleAvailableAccounts(msg.Payload)

	case "POSITION_UPDATE":
		h.handlePositionUpdate(msg.Payload)

	case "ORDERS_UPDATE":
		h.handleOrdersUpdate(msg.Payload)

	case "EXECUTION_UPDATE":
		h.handleExecutionUpdate(msg.Payload)

	case "COMMAND_ACK":
		var ack risk.CommandAck
		if err := json.Unmarshal(msg.Payload, &ack); err == nil {
			log.Printf("NT8 COMMAND_ACK: cmd=%s orderId=%s success=%v msg=%s", ack.Command, ack.OrderId, ack.Success, ack.Message)
			h.Mu.Lock()
			if ack.Success {
				h.State.CommandState = "CONFIRMED"
			} else {
				h.State.CommandState = "REJECTED"
				h.State.LastLog = fmt.Sprintf("REJECTED: %s (%s)", ack.Command, ack.Message)
			}
			h.Mu.Unlock()
			go h.BroadcastState()
			go h.BroadcastToWeb("COMMAND_ACK", msg.Payload)
			go logging.RecordAudit(logging.AuditEvent{
				EventType:      "COMMAND_ACK",
				AccountName:    h.State.SelectedAccount,
				InstrumentName: h.State.InstrumentName,
				OrderId:        ack.OrderId,
				Action:         ack.Command,
				Success:        ack.Success,
				Details:        ack.Message,
			})
		}

	case "HEARTBEAT":
		// Gateway keep-alive received
	}
}
