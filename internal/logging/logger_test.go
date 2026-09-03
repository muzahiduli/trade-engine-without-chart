package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLoggerInit(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "logtest")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logFile := filepath.Join(tmpDir, "test.log")
	logger := Init(logFile, "debug")
	if logger == nil {
		t.Fatal("Expected non-nil logger")
	}

	logger.Info("order_placed", slog.String("orderId", "ord_123"), slog.Int("qty", 5))

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	var entry map[string]interface{}
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatalf("Failed to parse log JSON: %v (raw: %s)", err, string(data))
	}

	if entry["msg"] != "order_placed" || entry["orderId"] != "ord_123" || entry["qty"] != float64(5) {
		t.Errorf("Unexpected log content: %+v", entry)
	}
}

func TestRecordAudit(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "audittest")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	auditPath := filepath.Join(tmpDir, "audit.jsonl")
	SetAuditFilePath(auditPath)

	event := AuditEvent{
		EventType:      "ORDER_SUBMIT",
		AccountName:    "Sim101",
		InstrumentName: "NQ",
		OrderId:        "ord_456",
		Action:         "BUY",
		Qty:            2,
		Price:          20000.0,
		Success:        true,
	}
	RecordAudit(event)

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("Failed to read audit file: %v", err)
	}

	var rec AuditEvent
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("Failed to parse audit JSON: %v", err)
	}

	if rec.EventType != "ORDER_SUBMIT" || rec.AccountName != "Sim101" || rec.OrderId != "ord_456" || rec.Qty != 2 {
		t.Errorf("Unexpected audit record: %+v", rec)
	}
}

