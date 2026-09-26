package app

import (
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// Health states. A successful deployment is not evidence of any of these:
// HEALTHY requires a bound server, a running worker reading a selected ADM,
// known presence and no delivery fault.
const (
	HealthHealthy     = "HEALTHY"
	HealthDegraded    = "DEGRADED"
	HealthUnavailable = "UNAVAILABLE"
	HealthUnknown     = "UNKNOWN"
)

// oldestPendingDegradedAfter is how long an event may wait in the
// persistence queue before the installation counts as degraded.
const oldestPendingDegradedAfter = 60 * time.Second

// RuntimeHealth is the per-installation diagnostics block of
// GET /api/runtime/status (docs/runtime-status-api.md).
type RuntimeHealth struct {
	State   string   `json:"state"`
	Reasons []string `json:"reasons"`

	InstallationBinding string `json:"installationBinding"` // BOUND | INACTIVE | NOT_FOUND | OTHER_GUILD

	SelectedADM           *string `json:"selectedAdm"`
	PipelineStatus        *string `json:"pipelineStatus"`
	LastSourceReadAt      *string `json:"lastSourceReadAt"`
	LastSourceGrowthAt    *string `json:"lastSourceGrowthAt"`
	LastPersistedEventAt  *string `json:"lastPersistedEventAt"`
	LastDiscordDeliveryAt *string `json:"lastDiscordDeliveryAt"`

	PlayersOnline      *int    `json:"playersOnline"` // null while presence is UNKNOWN
	PresenceState      string  `json:"presenceState"`
	PresenceEvidenceAt *string `json:"presenceEvidenceAt"`

	PendingEvents           *int     `json:"pendingEvents"`
	OldestPendingAgeSeconds *float64 `json:"oldestPendingAgeSeconds"`
	DroppedEvents           *int64   `json:"droppedEvents"`
	FailedDeliveries        int64    `json:"failedDeliveries"`

	OnlineCounter discord.CounterHealth   `json:"onlineCounter"`
	Deliveries    []discord.RouteDelivery `json:"deliveries"`
}

// healthInputs is everything evaluateRuntimeHealth reads, gathered by the
// caller from in-memory state so the evaluation itself is pure and testable.
type healthInputs struct {
	Binding      string
	WorkerFound  bool
	Pipeline     killfeed.RuntimeDiagnosticSnapshot
	Presence     killfeed.PresenceSnapshot
	Pending      *killfeed.PendingEvents
	Counter      discord.CounterHealth
	CounterOwned bool // this server drives the online counter
	Deliveries   []discord.RouteDelivery
}

func evaluateRuntimeHealth(in healthInputs) RuntimeHealth {
	h := RuntimeHealth{InstallationBinding: in.Binding, Reasons: []string{}, Deliveries: in.Deliveries, OnlineCounter: in.Counter}
	if h.Deliveries == nil {
		h.Deliveries = []discord.RouteDelivery{}
	}
	var unavailable, degraded []string

	switch in.Binding {
	case "BOUND":
	case "INACTIVE":
		unavailable = append(unavailable, "installation server is not active (disconnected)")
	default:
		unavailable = append(unavailable, "installation server binding "+in.Binding)
	}

	h.PresenceState = killfeed.PresenceUnknown
	if in.WorkerFound {
		p := in.Pipeline
		h.SelectedADM = nullableString(p.SelectedADM)
		h.LastSourceReadAt = nullableTime(p.LastDownloadSuccess)
		h.LastSourceGrowthAt = nullableTime(p.LastSourceGrowthAt)
		if p.LastPersistenceResult == "SUCCESS" {
			h.LastPersistedEventAt = nullableTime(p.LastPersistenceAt)
		}
		classification := p.Classification()
		h.PipelineStatus = nullableString(classification)
		switch {
		case !p.WorkerRunning:
			unavailable = append(unavailable, "log worker is not running")
		case p.SelectedADM == "":
			unavailable = append(unavailable, "no ADM source selected")
		case classification == "UNKNOWN":
			// no metadata check yet: evidence pending, not a fault
		case classification == "HEALTHY":
		case classification == killfeed.ProbeSourceQuiet:
			// Quiet with known-empty presence is normal; quiet with players
			// believed online is suspicious.
			if !in.Presence.Presence.Known || in.Presence.OnlineCount > 0 {
				degraded = append(degraded, "ADM source quiet (no new bytes) while presence is unknown or non-zero")
			}
		default:
			degraded = append(degraded, "ADM pipeline: "+classification)
		}

		ev := in.Presence.Presence
		h.PresenceState = ev.State
		h.PresenceEvidenceAt = nullableTime(ev.EvidenceAt)
		if ev.Known {
			count := in.Presence.OnlineCount
			h.PlayersOnline = &count
		} else {
			degraded = append(degraded, "player presence unknown (waiting for a PlayerList snapshot or restart)")
		}

		if in.Pending != nil {
			depth, dropped, age := in.Pending.Depth, in.Pending.Dropped, in.Pending.OldestAgeSec
			h.PendingEvents, h.DroppedEvents, h.OldestPendingAgeSeconds = &depth, &dropped, &age
			if in.Pending.OldestAge > oldestPendingDegradedAfter {
				degraded = append(degraded, "events waiting in the persistence queue for over a minute")
			}
			if dropped > 0 {
				degraded = append(degraded, "persistence queue dropped events")
			}
		}
	}

	if in.CounterOwned {
		switch in.Counter.State {
		case "CONFIG_FAULT":
			degraded = append(degraded, "online counter channel fault: "+in.Counter.FaultClass+" (run /setup repair)")
		case "RETRYING", "RATE_LIMITED":
			degraded = append(degraded, "online counter delivery "+in.Counter.State)
		case "UNBOUND":
			degraded = append(degraded, "online counter channel is not configured")
		}
	}

	var lastDelivery time.Time
	if in.Counter.LastSuccessAt.After(lastDelivery) {
		lastDelivery = in.Counter.LastSuccessAt
	}
	for _, d := range in.Deliveries {
		h.FailedDeliveries += d.Failed
		if d.LastSuccessAt.After(lastDelivery) {
			lastDelivery = d.LastSuccessAt
		}
		switch d.State() {
		case discord.DeliveryConfigFault:
			degraded = append(degraded, d.Route+" delivery configuration fault")
		case "FAILING":
			degraded = append(degraded, d.Route+" delivery failing")
		}
	}
	h.LastDiscordDeliveryAt = nullableTime(lastDelivery)

	switch {
	case len(unavailable) > 0:
		h.State = HealthUnavailable
		h.Reasons = append(unavailable, degraded...)
	case !in.WorkerFound:
		h.State = HealthUnknown
		h.Reasons = append(h.Reasons, "no live worker evidence for this server yet")
	case len(degraded) > 0:
		h.State = HealthDegraded
		h.Reasons = degraded
	case in.Pipeline.Classification() == "UNKNOWN":
		h.State = HealthUnknown
		h.Reasons = append(h.Reasons, "worker has not completed a source check yet")
	default:
		h.State = HealthHealthy
	}
	return h
}
