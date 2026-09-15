package killfeed

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// newPresenceEngine builds an Engine with the real ADM parser, no log source
// needed since these tests drive processLines directly.
func newPresenceEngine() *Engine {
	return NewEngine(nil, "svc-1", NewADMParser())
}

func TestPresenceConnectTest(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{`16:16:10 | Player "TCP" (id=a001) is connected`})
	if got := e.players.OnlineCount(); got != 1 {
		t.Fatalf("expected 1 online after connect, got %d", got)
	}
}

func TestPresenceDuplicateConnectTest(t *testing.T) {
	e := newPresenceEngine()
	line := `16:16:10 | Player "TCP" (id=a001) is connected`
	e.processLines([]string{line, line})
	if got := e.players.OnlineCount(); got != 1 {
		t.Fatalf("expected duplicate connect to stay at 1, got %d", got)
	}
}

func TestPresenceSecondPlayerTest(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{
		`16:16:10 | Player "TCP" (id=a001) is connected`,
		`16:17:00 | Player "PLAYER2" (id=b002) is connected`,
	})
	if got := e.players.OnlineCount(); got != 2 {
		t.Fatalf("expected 2 online after two distinct connects, got %d", got)
	}
}

func TestPresenceDisconnectTest(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{
		`16:16:10 | Player "TCP" (id=a001) is connected`,
		`16:17:00 | Player "PLAYER2" (id=b002) is connected`,
	})
	e.processLines([]string{`16:20:00 | Player "TCP" (id=a001) has been disconnected`})
	if got := e.players.OnlineCount(); got != 1 {
		t.Fatalf("expected 1 online after one disconnect, got %d", got)
	}
	e.processLines([]string{`16:21:00 | Player "PLAYER2" (id=b002) has been disconnected`})
	if got := e.players.OnlineCount(); got != 0 {
		t.Fatalf("expected 0 online after both disconnect, got %d", got)
	}
}

func TestPresenceDuplicateDisconnectTest(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{`16:16:10 | Player "TCP" (id=a001) is connected`})
	line := `16:20:00 | Player "TCP" (id=a001) has been disconnected`
	e.processLines([]string{line, line})
	if got := e.players.OnlineCount(); got != 0 {
		t.Fatalf("expected 0 online, never negative, got %d", got)
	}
}

// TestPresenceRotationTest is the section 5/17/25 regression: switching which
// ADM file is being read (rotation) must never clear live presence.
func TestPresenceRotationTest(t *testing.T) {
	e := newPresenceEngine()
	e.processLines([]string{`16:16:10 | Player "TCP" (id=a001) is connected`})
	if got := e.players.OnlineCount(); got != 1 {
		t.Fatalf("expected 1 online before rotation, got %d", got)
	}

	// Simulate the engine switching to a newly discovered ADM file.
	e.selectLog(nitrado.LogFile{Path: "/logs/rotated.ADM"})
	if got := e.players.OnlineCount(); got != 1 {
		t.Fatalf("expected rotation to preserve presence at 1, got %d", got)
	}

	e.processLines([]string{`16:25:00 | Player "TCP" (id=a001) has been disconnected`})
	if got := e.players.OnlineCount(); got != 0 {
		t.Fatalf("expected disconnect after rotation to reach 0, got %d", got)
	}
}

// TestPresencePerEngineIsolation is the multi-server regression: each Engine
// (one per ServerWorker) must own an independent PlayerTracker.
func TestPresencePerEngineIsolation(t *testing.T) {
	serverA := newPresenceEngine()
	serverB := newPresenceEngine()

	serverA.processLines([]string{`16:16:10 | Player "TCP" (id=a001) is connected`})
	serverB.processLines([]string{
		`16:16:10 | Player "PLAYER2" (id=b002) is connected`,
		`16:17:00 | Player "PLAYER3" (id=b003) is connected`,
	})

	if got := serverA.players.OnlineCount(); got != 1 {
		t.Fatalf("expected server A to show 1, got %d", got)
	}
	if got := serverB.players.OnlineCount(); got != 2 {
		t.Fatalf("expected server B to show 2, got %d", got)
	}
}

func TestPresenceOnPlayersChangedFiresOnlyOnRealChange(t *testing.T) {
	e := newPresenceEngine()
	updates := 0
	e.OnPlayersChanged(func(int) { updates++ })

	line := `16:16:10 | Player "TCP" (id=a001) is connected`
	e.processLines([]string{line})
	e.processLines([]string{line}) // duplicate connect: must not fire again
	if updates != 1 {
		t.Fatalf("expected exactly 1 change notification for connect, got %d", updates)
	}

	disconnect := `16:20:00 | Player "TCP" (id=a001) has been disconnected`
	e.processLines([]string{disconnect})
	e.processLines([]string{disconnect}) // duplicate disconnect: must not fire again
	if updates != 2 {
		t.Fatalf("expected exactly 2 total change notifications, got %d", updates)
	}
}
