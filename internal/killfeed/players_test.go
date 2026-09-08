package killfeed

import (
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
