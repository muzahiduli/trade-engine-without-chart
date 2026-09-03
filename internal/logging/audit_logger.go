package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEvent captures full trade lifecycle and intent for auditing and replay.
type AuditEvent struct {
	Timestamp      string  `json:"timestamp"`
	EventType      string  `json:"eventType"` // INTENT, HOTKEY, ORDER_SUBMIT, ORDER_CHANGE, ORDER_CANCEL, FILL, REJECT, BRACKET_SYNC, RECOVERY, KILL_SWITCH
	AccountName    string  `json:"accountName"`
	InstrumentName string  `json:"instrumentName"`
	OrderId        string  `json:"orderId,omitempty"`
	Action         string  `json:"action,omitempty"`
	OrderType      string  `json:"orderType,omitempty"`
	Qty            int     `json:"qty,omitempty"`
	Price          float64 `json:"price,omitempty"`
	FillPrice      float64 `json:"fillPrice,omitempty"`
	Slippage       float64 `json:"slippage,omitempty"`
	LatencyMs      int64   `json:"latencyMs,omitempty"`
	Success        bool    `json:"success"`
	Details        string  `json:"details,omitempty"`
}

var (
	auditMu   sync.Mutex
	auditFile = "logs/audit.jsonl"
)

// SetAuditFilePath configures the target file for audit records.
func SetAuditFilePath(path string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditFile = path
}

// RecordAudit logs a structured audit event to audit.jsonl and structured slog.
func RecordAudit(event AuditEvent) {
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	auditMu.Lock()
	defer auditMu.Unlock()

	dir := filepath.Dir(auditFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	f, err := os.OpenFile(auditFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		data, err := json.Marshal(event)
		if err == nil {
			_, _ = f.Write(append(data, '\n'))
		}
		_ = f.Close()
	}

	Get().Info("audit_event",
		slog.String("event_type", event.EventType),
		slog.String("account", event.AccountName),
		slog.String("instrument", event.InstrumentName),
		slog.String("order_id", event.OrderId),
		slog.String("action", event.Action),
		slog.Int("qty", event.Qty),
		slog.Float64("price", event.Price),
		slog.Bool("success", event.Success),
		slog.String("details", event.Details),
	)
}
