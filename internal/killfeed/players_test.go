package killfeed

import (
	"time"

	"testing"
)

func TestPlayerTrackerConnectDisconnect(t *testing.T) {
	tr := NewPlayerTracker()

	tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "Andromede54"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected 1 online, got %d", tr.OnlineCount())
	}

	// Duplicate connection by same ID refreshes, not double-counts.
	tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "Andromede54"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected duplicate connect to keep 1 online, got %d", tr.OnlineCount())
	}

	tr.PlayerConnected(&PlayerRef{ID: "p2", Name: "NoxiKillNewB"})
	if tr.OnlineCount() != 2 {
		t.Fatalf("expected 2 online, got %d", tr.OnlineCount())
	}

	tr.PlayerDisconnected(&PlayerRef{ID: "p1"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected 1 online after disconnect, got %d", tr.OnlineCount())
	}

	// Unknown disconnect is ignored safely.
	tr.PlayerDisconnected(&PlayerRef{ID: "ghost"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected unknown disconnect ignored, got %d", tr.OnlineCount())
	}
}

func TestPlayerTrackerNameOnlyKeysAreCaseInsensitive(t *testing.T) {
	tracker := NewPlayerTracker()
	tracker.PlayerConnected(&PlayerRef{Name: "Survivor"})
	tracker.PlayerConnected(&PlayerRef{Name: "survivor"})
	if tracker.OnlineCount() != 1 {
		t.Fatalf("expected case-only reconnect to remain one player, got %d", tracker.OnlineCount())
	}
}

func TestPlayerTrackerKeyedByIDNotName(t *testing.T) {
	tr := NewPlayerTracker()
	// Same display name, different IDs -> two distinct players.
	tr.PlayerConnected(&PlayerRef{ID: "a", Name: "Survivor"})
	tr.PlayerConnected(&PlayerRef{ID: "b", Name: "Survivor"})
	if tr.OnlineCount() != 2 {
		t.Fatalf("expected 2 players with same name but different IDs, got %d", tr.OnlineCount())
	}
}

func TestPlayerTrackerNameFallbackAndReset(t *testing.T) {
	tr := NewPlayerTracker()
	// Player with no ID is tracked by name.
	tr.PlayerConnected(&PlayerRef{Name: "NoID"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected name-keyed player tracked, got %d", tr.OnlineCount())
	}

	tr.PlayerConnected(&PlayerRef{ID: "x", Name: "Temp"})
	tr.Reset()
	if tr.OnlineCount() != 0 {
		t.Fatalf("expected reset to clear all players, got %d", tr.OnlineCount())
	}
}

func TestPlayerTrackerReconnect(t *testing.T) {
	tr := NewPlayerTracker()
	tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "Player"})
	tr.PlayerDisconnected(&PlayerRef{ID: "p1"})
	tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "Player"})
	if tr.OnlineCount() != 1 {
		t.Fatalf("expected reconnect to track 1 player, got %d", tr.OnlineCount())
	}
}

func TestPlayerConnectedReportsChangeOnlyOnce(t *testing.T) {
	tr := NewPlayerTracker()
	if !tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "TCP"}) {
		t.Fatal("expected the first connect to report a change")
	}
	if tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "TCP"}) {
		t.Fatal("expected a duplicate connect to report no change")
	}
}

func TestPlayerDisconnectedReportsChangeOnlyOnce(t *testing.T) {
	tr := NewPlayerTracker()
	tr.PlayerConnected(&PlayerRef{ID: "p1", Name: "TCP"})
	if !tr.PlayerDisconnected(&PlayerRef{ID: "p1"}) {
		t.Fatal("expected the first disconnect to report a change")
	}
	if tr.PlayerDisconnected(&PlayerRef{ID: "p1"}) {
		t.Fatal("expected a duplicate disconnect to report no change")
	}
}

// A session is measured between the connect and disconnect lines' own times when both are
// known, so Nitrado's five-minute delivery steps do not stretch it; otherwise between the
// two reads as before.
func TestDisconnectSessionPrefersEventTimes(t *testing.T) {
	tr := NewPlayerTracker()
	readAt := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	tr.now = func() time.Time { return readAt }
	p := &PlayerRef{Name: "Alice", ID: "a1"}
	connAt := time.Date(2026, 10, 15, 17, 15, 32, 0, time.UTC)
	if !tr.PlayerConnectedAt(p, connAt) {
		t.Fatal("first connect must be new")
	}
	// A duplicate "is connected" keeps the first line's time.
	tr.PlayerConnectedAt(p, connAt.Add(10*time.Minute))
	readAt = readAt.Add(time.Hour)
	got, removed := tr.DisconnectSessionAt(p, connAt.Add(33*time.Minute+26*time.Second))
	if !removed || got != 33*time.Minute+26*time.Second {
		t.Fatalf("got %v removed=%v, want 33m26s", got, removed)
	}

	// Unknown line times: the reads decide.
	tr.now = func() time.Time { return readAt }
	tr.PlayerConnectedAt(p, time.Time{})
	readAt = readAt.Add(5 * time.Minute)
	if got, _ := tr.DisconnectSessionAt(p, connAt); got != 5*time.Minute {
		t.Fatalf("got %v, want 5m from the reads", got)
	}
}
