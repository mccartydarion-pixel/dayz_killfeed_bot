package app

import (
	"fmt"
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

	PlayersOnline      *int    `json:"playersOnline"` // the counter's authoritative reading; null while unknown
	PlayersSource      string  `json:"playersSource"` // NITRADO_QUERY | NITRADO_SERVER_STOPPED | ADM_PLAYER_LIST | ADM_BOOT_RESET | UNKNOWN
	PresenceState      string  `json:"presenceState"` // ADM evidence: UNKNOWN | SNAPSHOT_CONFIRMED | BOOT_RESET
	PresenceEvidenceAt *string `json:"presenceEvidenceAt"`
	// PresenceDisagreementSince is set while Nitrado and the proven ADM
	// tracker disagree (Nitrado is shown).
	PresenceDisagreementSince *string `json:"presenceDisagreementSince"`

	PendingEvents           *int     `json:"pendingEvents"`
	OldestPendingAgeSeconds *float64 `json:"oldestPendingAgeSeconds"`
	DroppedEvents           *int64   `json:"droppedEvents"`
	FailedDeliveries        int64    `json:"failedDeliveries"`

	// VerifiedRoles counts VERIFIED links by Verified-role delivery state
	// (PENDING / ASSIGNED / FAILED / MEMBER_GONE; "" = verified before
	// role tracking existed). A verified link is not an assigned role.
	VerifiedRoles map[string]int `json:"verifiedRoles,omitempty"`

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
	// CounterStatus is the online counter loop's latest evaluation for this
	// server (nil when this server is not the counter's public server or it
	// has not evaluated yet).
	CounterStatus *onlineCounterStatus
	Now           time.Time
	RoleSync      map[string]int
	Deliveries    []discord.RouteDelivery
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
		h.PlayersSource = counterSourceUnknown
		switch {
		case in.CounterStatus != nil && in.CounterStatus.Reading.Known:
			// One authority: the same reading the voice counter shows.
			count := in.CounterStatus.Reading.Count
			h.PlayersOnline, h.PlayersSource = &count, in.CounterStatus.Source
		case in.CounterStatus == nil && ev.Known:
			// Not the counter's server: ADM evidence is the only source.
			count := in.Presence.OnlineCount
			h.PlayersOnline, h.PlayersSource = &count, "ADM_"+ev.State
		default:
			degraded = append(degraded, "current player count unknown (no fresh Nitrado query and no complete ADM evidence)")
		}
		if in.CounterStatus != nil && in.CounterStatus.Disagreement != nil {
			d := in.CounterStatus.Disagreement
			h.PresenceDisagreementSince = nullableTime(d.Since)
			now := in.Now
			if now.IsZero() {
				now = time.Now()
			}
			if now.Sub(d.Since) > onlineCounterADMTrust {
				degraded = append(degraded, fmt.Sprintf("Nitrado (%d) and ADM player list (%d) have disagreed since %s", d.NitradoCount, d.ADMCount, d.Since.UTC().Format(time.RFC3339)))
			}
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

	h.VerifiedRoles = in.RoleSync
	if n := in.RoleSync["FAILED"]; n > 0 {
		degraded = append(degraded, fmt.Sprintf("%d verified link(s) whose Verified role could not be assigned yet (retrying)", n))
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
