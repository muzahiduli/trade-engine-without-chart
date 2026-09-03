package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"trade-engine-without-chart/internal/hub"
	"trade-engine-without-chart/internal/logging"
)

// Version is injected at build time: go build -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	port := flag.Int("port", 8080, "HTTP and WebSocket server port")
	logFile := flag.String("log-file", "logs/engine.log", "Path to JSON log file (empty for stdout only)")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	flag.Parse()

	logger := logging.Init(*logFile, *logLevel)
	logger.Info("trade_engine_starting", "port", *port, "logFile", *logFile, "logLevel", *logLevel)
	logger.Info("trade_engine_version", "version", Version)

	h := hub.NewHub()
	go h.Run()

	// Locate web assets folder
	ex, err := os.Executable()
	baseDir := "."
	if err == nil {
		baseDir = filepath.Dir(ex)
	}

	webCandidates := []string{
		filepath.Join(baseDir, "web"),
		filepath.Join(baseDir, "trade-engine", "web"),
		"./web",
		"./web",
	}

	webDir := "./web"
	for _, c := range webCandidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			webDir = c
			break
		}
	}
	log.Printf("ðŸ“‚ Serving web assets from: %s", webDir)

	// Serve static web UI
	fs := http.FileServer(http.Dir(webDir))
	http.Handle("/", fs)

	// WebSocket handler
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWs(h, w, r)
	})

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("trade-engine OK"))
	})

	// Version endpoint: which commit/ref is running (injected at build time by the launcher)
	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"name":    "trade-engine",
			"version": Version,
		})
	})

	// State inspection endpoint for automated testing
	http.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		h.Mu.RLock()
		defer h.Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"clients": map[string]int{
				"total": len(h.Clients),
			},
			"state": h.State,
		})
	})

	// Bars inspection endpoint
	http.HandleFunc("/api/bars", func(w http.ResponseWriter, r *http.Request) {
		h.Mu.RLock()
		defer h.Mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"count": len(h.BarCache),
			"bars":  h.BarCache,
		})
	})

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	log.Printf("======================================================")
	log.Printf("ðŸš€ Trade Engine starting on http://localhost:%d", *port)
	log.Printf("ðŸ“¡ WebSocket Server listening on ws://localhost:%d/ws", *port)
	log.Printf("ðŸ–¥ï¸  Web Command Center available at http://localhost:%d", *port)
	log.Printf("======================================================")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
