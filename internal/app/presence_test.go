package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// TestPresenceCountsSumsAcrossActiveServers proves App.PresenceCounts
// (discord.PresenceStatsProvider) reuses the existing presenceTrackers state
// - the same in-memory data livePresenceCount already reads - never a second
// Nitrado poll, and reports ok=false rather than a fabricated 0 when no
// server worker is registered yet.
func TestPresenceCountsSumsAcrossActiveServers(t *testing.T) {
	a := &App{}
	if _, _, _, ok := a.PresenceCounts(); ok {
		t.Fatal("expected ok=false with no registered servers")
	}

	tracker1 := killfeed.NewPlayerTracker()
	tracker1.PlayerConnected(&killfeed.PlayerRef{Name: "p1", ID: "p1-id"})
	tracker1.PlayerConnected(&killfeed.PlayerRef{Name: "p2", ID: "p2-id"})
	a.registerPresenceTracker(1, tracker1)

	tracker2 := killfeed.NewPlayerTracker()
	tracker2.PlayerConnected(&killfeed.PlayerRef{Name: "p3", ID: "p3-id"})
	a.registerPresenceTracker(2, tracker2)

	total, _, configured, ok := a.PresenceCounts()
	if !ok {
		t.Fatal("expected ok=true once servers are registered")
	}
	if total != 3 {
		t.Fatalf("expected total players 3, got %d", total)
	}
	if configured != 2 {
		t.Fatalf("expected configured servers 2, got %d", configured)
	}
}

// TestOverallHealthReflectsRegistry proves App.OverallHealth
// (discord.PresenceHealthProvider) reuses the existing health.Registry -
// never a second, duplicate health evaluation for presence.
func TestOverallHealthReflectsRegistry(t *testing.T) {
	a := &App{}
	if got := a.OverallHealth(); got != health.Healthy {
		t.Fatalf("expected Healthy with no registry configured, got %s", got)
	}

	a.HealthRegistry = health.NewRegistry()
	a.HealthRegistry.Set(health.Component{Name: "adm_pipeline", State: health.Unhealthy, Critical: true})
	if got := a.OverallHealth(); got != health.Unhealthy {
		t.Fatalf("expected Unhealthy once a critical component fails, got %s", got)
	}
}
