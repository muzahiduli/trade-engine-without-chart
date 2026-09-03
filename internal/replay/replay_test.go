package replay

import (
	"path/filepath"
	"testing"

	"trade-engine-without-chart/internal/hub"
)

// TestPlaybackFixture_EngineMatchesBroker replays a fixture recorded from a
// REAL NinjaTrader playback run into an in-process hub and asserts that after
// every scripted step the engine's position and working orders match the
// broker's recorded snapshot. This is the deterministic, GUI-free form of the
// live verification: identical input bytes â†’ identical hub behavior â†’ identical
// assertions. Runs in `go test ./...` (and CI) with no NT8 / no WebSocket.
//
// NOTE: regenerate the fixture with:
//
//	go run ./cmd/verify-playback scenario -fixture internal/replay/testdata/playback_fixture.jsonl
//
// against a live NinjaTrader playback session â€” that is the one-time
// calibration step; after that, this test is fully self-contained.
func TestPlaybackFixture_EngineMatchesBroker(t *testing.T) {
	fixture, err := Load(filepath.Join("testdata", "playback_fixture.jsonl"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(fixture) == 0 {
		t.Fatal("fixture is empty")
	}

	h := hub.NewHub()
	go h.Run()

	nt8c := &hub.Client{Hub: h, Send: make(chan []byte, 1024), ClientType: "NT8"}
	webc := &hub.Client{Hub: h, Send: make(chan []byte, 1024), ClientType: "WEB"}
	h.Register <- nt8c
	h.Register <- webc
	defer func() {
		h.Unregister <- nt8c
		h.Unregister <- webc
	}()

	if err := Run(h, nt8c, webc, fixture); err != nil {
		t.Fatal(err)
	}
}
