package app

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func healthyInputs() healthInputs {
	now := time.Now()
	return healthInputs{
		Binding:     "BOUND",
		WorkerFound: true,
		Pipeline: killfeed.RuntimeDiagnosticSnapshot{
			WorkerRunning: true, SelectedADM: "DayZServer_PS4_x64_2026-09-26_10-00-00.ADM",
			LastMetadataCheck: now, LastMetadataChanged: true, LastDownloadAttempt: now, LastDownloadSuccess: now,
			RemoteModified: now, LastSourceGrowthAt: now, LastPersistenceResult: "SUCCESS", LastPersistenceAt: now,
		},
		Presence:     killfeed.PresenceSnapshot{OnlineCount: 3, Presence: killfeed.PresenceEvidence{State: killfeed.PresenceSnapshotConfirmed, Known: true, EvidenceAt: now}},
		Pending:      &killfeed.PendingEvents{Capacity: 256},
		Counter:      discord.CounterHealth{ChannelID: "vc", State: "OK", LastSuccessAt: now},
		CounterOwned: true,
	}
}

func TestRuntimeHealthHealthyRequiresEvidence(t *testing.T) {
	h := evaluateRuntimeHealth(healthyInputs())
	if h.State != HealthHealthy {
		t.Fatalf("expected HEALTHY, got %s %v", h.State, h.Reasons)
	}
	if h.PlayersOnline == nil || *h.PlayersOnline != 3 || h.PresenceState != killfeed.PresenceSnapshotConfirmed {
		t.Fatalf("presence: %+v", h)
	}
	if h.LastDiscordDeliveryAt == nil || h.LastSourceGrowthAt == nil || h.LastPersistedEventAt == nil {
		t.Fatalf("expected evidence timestamps, got %+v", h)
	}
}

// TestRuntimeHealthNoWorkerIsUnknownNotHealthy: a successful deploy with no
// worker evidence yet must not read as healthy.
func TestRuntimeHealthNoWorkerIsUnknownNotHealthy(t *testing.T) {
	in := healthyInputs()
	in.WorkerFound = false
	h := evaluateRuntimeHealth(in)
	if h.State != HealthUnknown {
		t.Fatalf("expected UNKNOWN, got %s", h.State)
	}
	if h.PlayersOnline != nil {
		t.Fatal("no worker means no player count")
	}
}

func TestRuntimeHealthUnavailable(t *testing.T) {
	for name, mutate := range map[string]func(*healthInputs){
		"server inactive":  func(in *healthInputs) { in.Binding = "INACTIVE" },
		"server not found": func(in *healthInputs) { in.Binding = "NOT_FOUND" },
		"worker stopped":   func(in *healthInputs) { in.Pipeline.WorkerRunning = false },
		"no source":        func(in *healthInputs) { in.Pipeline.SelectedADM = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := healthyInputs()
			mutate(&in)
			if h := evaluateRuntimeHealth(in); h.State != HealthUnavailable || len(h.Reasons) == 0 {
				t.Fatalf("expected UNAVAILABLE with reasons, got %s %v", h.State, h.Reasons)
			}
		})
	}
}

func TestRuntimeHealthDegraded(t *testing.T) {
	cases := map[string]struct {
		mutate func(*healthInputs)
		reason string
	}{
		"presence unknown": {func(in *healthInputs) {
			in.Presence.Presence = killfeed.PresenceEvidence{State: killfeed.PresenceUnknown}
		}, "presence unknown"},
		"counter unknown channel": {func(in *healthInputs) {
			in.Counter = discord.CounterHealth{State: "CONFIG_FAULT", FaultClass: discord.CounterFaultUnknownChannel}
		}, "UNKNOWN_CHANNEL"},
		"killfeed route fault": {func(in *healthInputs) {
			in.Deliveries = []discord.RouteDelivery{{Route: "KILLFEED", Failed: 4, ConsecutiveFailures: 4, LastErrorClass: discord.DeliveryConfigFault}}
		}, "KILLFEED delivery configuration fault"},
		"stuck queue": {func(in *healthInputs) {
			in.Pending = &killfeed.PendingEvents{Depth: 9, OldestAge: 5 * time.Minute, OldestAgeSec: 300}
		}, "persistence queue"},
		"wrong source": {func(in *healthInputs) {
			in.Pipeline.ProbeClassification = "WRONG_OR_INACTIVE_ADM_SOURCE"
		}, "WRONG_OR_INACTIVE_ADM_SOURCE"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := healthyInputs()
			c.mutate(&in)
			h := evaluateRuntimeHealth(in)
			if h.State != HealthDegraded {
				t.Fatalf("expected DEGRADED, got %s %v", h.State, h.Reasons)
			}
			if !strings.Contains(strings.Join(h.Reasons, "|"), c.reason) {
				t.Fatalf("expected reason containing %q, got %v", c.reason, h.Reasons)
			}
		})
	}
}

// TestRuntimeHealthUnknownPresenceHidesCount: unknown presence is null, never 0.
func TestRuntimeHealthUnknownPresenceHidesCount(t *testing.T) {
	in := healthyInputs()
	in.Presence = killfeed.PresenceSnapshot{OnlineCount: 0, Presence: killfeed.PresenceEvidence{State: killfeed.PresenceUnknown}}
	h := evaluateRuntimeHealth(in)
	if h.PlayersOnline != nil {
		t.Fatalf("unknown presence must be null, got %d", *h.PlayersOnline)
	}
	if h.PresenceState != killfeed.PresenceUnknown {
		t.Fatalf("presence state %s", h.PresenceState)
	}
}

// TestRuntimeHealthQuietEmptyServerIsHealthy: a quiet ADM on a server with
// confirmed zero players is normal, not a broken source.
func TestRuntimeHealthQuietEmptyServerIsHealthy(t *testing.T) {
	in := healthyInputs()
	in.Pipeline.ProbeClassification = killfeed.ProbeSourceQuiet
	in.Presence = killfeed.PresenceSnapshot{OnlineCount: 0, Presence: killfeed.PresenceEvidence{State: killfeed.PresenceSnapshotConfirmed, Known: true}}
	if h := evaluateRuntimeHealth(in); h.State != HealthHealthy {
		t.Fatalf("quiet + known empty should be HEALTHY, got %s %v", h.State, h.Reasons)
	}
	in.Presence.OnlineCount = 4
	if h := evaluateRuntimeHealth(in); h.State != HealthDegraded {
		t.Fatalf("quiet with players online should be DEGRADED, got %s", h.State)
	}
}
