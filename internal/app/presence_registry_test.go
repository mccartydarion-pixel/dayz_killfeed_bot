package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestPresenceRegistryReturnsExactWorkerTracker(t *testing.T) {
	a := &App{}
	trackerA := killfeed.NewPlayerTracker()
	trackerA.PlayerConnected(&killfeed.PlayerRef{ID: "p1", Name: "TCP"})
	trackerB := killfeed.NewPlayerTracker()
	trackerB.PlayerConnected(&killfeed.PlayerRef{ID: "p2", Name: "PLAYER2"})
	trackerB.PlayerConnected(&killfeed.PlayerRef{ID: "p3", Name: "PLAYER3"})

	a.registerPresenceTracker(1, trackerA)
	a.registerPresenceTracker(2, trackerB)

	if count, ok := a.livePresenceCount(1); !ok || count != 1 {
		t.Fatalf("expected server 1 count 1, got count=%d ok=%v", count, ok)
	}
	if count, ok := a.livePresenceCount(2); !ok || count != 2 {
		t.Fatalf("expected server 2 count 2, got count=%d ok=%v", count, ok)
	}
}

func TestPresenceRegistryUnknownServerIsUnavailable(t *testing.T) {
	a := &App{}
	if _, ok := a.livePresenceCount(99); ok {
		t.Fatal("expected no live count for a server with no registered worker")
	}
}

func TestPresenceRegistryForgetsStoppedWorker(t *testing.T) {
	a := &App{}
	tracker := killfeed.NewPlayerTracker()
	tracker.PlayerConnected(&killfeed.PlayerRef{ID: "p1", Name: "TCP"})
	a.registerPresenceTracker(5, tracker)
	a.unregisterPresenceTracker(5)
	if _, ok := a.livePresenceCount(5); ok {
		t.Fatal("expected diagnostics to stop reading a worker's tracker once it stops")
	}
}
