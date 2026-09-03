package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

var Log *slog.Logger

// Init initializes the global structured JSON logger.
// If logFilePath is provided, logs are written to both stdout and the specified file.
func Init(logFilePath, levelStr string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(levelStr) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}

	var writer io.Writer = os.Stdout
	if logFilePath != "" {
		if dir := filepath.Dir(logFilePath); dir != "" && dir != "." {
			_ = os.MkdirAll(dir, 0755)
		}
		f, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			writer = io.MultiWriter(os.Stdout, f)
		}
	}

	handler := slog.NewJSONHandler(writer, opts)
	Log = slog.New(handler)
	slog.SetDefault(Log)
	return Log
}

// Get returns the initialized logger or fallback default logger.
func Get() *slog.Logger {
	if Log == nil {
		Log = slog.Default()
	}
	return Log
}
