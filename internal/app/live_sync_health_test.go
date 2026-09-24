package app

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/server"
)

// Live Sync phase 2.1: the health mapping reads the keys the real state snapshot provides.
func TestNitradoHealthUsesTheStateContract(t *testing.T) {
	st := server.NewState()
	st.SetNitrado(true, true, "dayzps", "gameserver", "started")
	comps := map[string]health.Component{}
	for _, c := range stateHealthComponents(st.Snapshot()) {
		comps[c.Name] = c
	}
	if comps["nitrado"].State != health.Healthy {
		t.Fatalf("an authenticated Nitrado connection is healthy: %+v", comps["nitrado"])
	}
	st.SetNitrado(false, false, "", "", "")
	for _, c := range stateHealthComponents(st.Snapshot()) {
		if c.Name == "nitrado" && c.State != health.Degraded {
			t.Fatalf("a failed Nitrado connection is still reported: %+v", c)
		}
	}
}

func TestADMSourceComponentStates(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 30, 0, 0, time.UTC)
	quiet := killfeed.ADMSourceHealth{LastCycleAt: now.Add(-time.Second), LastChangeAt: now.Add(-time.Hour)}
	if c := admSourceComponent(1, quiet, now); c.State != health.Healthy || !strings.HasPrefix(c.Message, "QUIET") || c.Name != "adm_source_1" {
		t.Fatalf("a quiet server is healthy, and says QUIET: %+v", c)
	}
	transport := quiet
	transport.TransportStreak = 4
	if c := admSourceComponent(1, transport, now); c.State != health.Degraded || !strings.HasPrefix(c.Message, "TRANSPORT_ERROR") {
		t.Fatalf("real failures are not masked: %+v", c)
	}
	stalled := quiet
	stalled.LastCycleAt = now.Add(-10 * time.Minute)
	if c := admSourceComponent(1, stalled, now); c.State != health.Unhealthy || !strings.HasPrefix(c.Message, "WORKER_STALLED") {
		t.Fatalf("a stalled worker is unhealthy: %+v", c)
	}
}
