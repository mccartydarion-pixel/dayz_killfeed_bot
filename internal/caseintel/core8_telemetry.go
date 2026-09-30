package caseintel

import (
	"sort"
	"time"
)

// TelemetryKind names one input feed a Core Eight module depends on. A feed
// is described by the caller from protected, exact-installation reads; this
// package never discovers or polls telemetry itself.
type TelemetryKind string

const (
	TelemetryADMEvents           TelemetryKind = "ADM_EVENTS"
	TelemetryPositionSamples     TelemetryKind = "POSITION_SAMPLES"
	TelemetryTerrainModel        TelemetryKind = "TERRAIN_MODEL"
	TelemetryStructureGeometry   TelemetryKind = "STRUCTURE_GEOMETRY"
	TelemetryVehicleState        TelemetryKind = "VEHICLE_STATE"
	TelemetryBuildActions        TelemetryKind = "BUILD_ACTIONS"
	TelemetryBaseRegistry        TelemetryKind = "BASE_REGISTRY"
	TelemetryInventoryItems      TelemetryKind = "INVENTORY_ITEM_TRANSACTIONS"
	TelemetryPlatformAttestation TelemetryKind = "PLATFORM_ATTESTATION"
	TelemetrySessionEvents       TelemetryKind = "SESSION_EVENTS"
	TelemetryRestartSchedule     TelemetryKind = "RESTART_SCHEDULE"
)

// requiredTelemetry is the per-module data contract. A detector whose feeds
// are not all available, verified and fresh cannot reach a conclusion.
var requiredTelemetry = map[string][]TelemetryKind{
	"CASE-BASE-001":     {TelemetryADMEvents, TelemetryBuildActions, TelemetryBaseRegistry},
	"CASE-SKYWALK-001":  {TelemetryADMEvents, TelemetryPositionSamples, TelemetryTerrainModel, TelemetryStructureGeometry, TelemetryVehicleState},
	"CASE-DUPE-001":     {TelemetryADMEvents, TelemetrySessionEvents, TelemetryRestartSchedule, TelemetryInventoryItems},
	"CASE-PC-XBOX-001":  {TelemetryPlatformAttestation},
	"CASE-NOCLIP-001":   {TelemetryADMEvents, TelemetryPositionSamples, TelemetryStructureGeometry},
	"CASE-UNDERMAP-001": {TelemetryADMEvents, TelemetryPositionSamples, TelemetryTerrainModel, TelemetryStructureGeometry},
	"CASE-LOGIN-001":    {TelemetryADMEvents, TelemetrySessionEvents, TelemetryRestartSchedule},
	"CASE-TELEPORT-001": {TelemetryADMEvents, TelemetryPositionSamples, TelemetryVehicleState, TelemetryRestartSchedule},
}

// RequiredTelemetry returns a copy of the module's data contract, or nil for
// a module outside the Core Eight catalog.
func RequiredTelemetry(moduleID string) []TelemetryKind {
	return append([]TelemetryKind(nil), requiredTelemetry[moduleID]...)
}

// TelemetryFeed describes one feed for one installation and game server.
// Static feeds (map models, registries) have no freshness limit but must be
// verified; streaming feeds need LatestAt within MaxAge of the snapshot.
type TelemetryFeed struct {
	Available   bool
	Verified    bool
	Unsupported bool // the console source cannot provide this feed at all
	Static      bool
	LatestAt    time.Time
	MaxAge      time.Duration
}

// TelemetrySnapshot is one exact-installation health view. Callers must not
// populate it from runtime log lines alone.
type TelemetrySnapshot struct {
	Now                        time.Time
	Feeds                      map[TelemetryKind]TelemetryFeed
	PollingDelay               time.Duration
	PollingDelayLimit          time.Duration
	ProcessingError            bool
	DuplicateEvents            bool
	LastSuccessfulEvaluationAt time.Time
}

// AssessCore8Health derives module-specific health from the module's data
// contract and reuses AssessDetectorHealth for the shared state machine. Any
// stale required feed suspends conclusions, not only ADM and positions.
func AssessCore8Health(moduleID string, enabled bool, snap TelemetrySnapshot) DetectorHealth {
	required := requiredTelemetry[moduleID]
	in := DetectorHealthInput{ModuleID: moduleID, Enabled: enabled, SourceSupported: len(required) > 0,
		ModuleValidated: clientModuleMode(moduleID) == "VALIDATED_SHADOW", RequiredTelemetryPresent: len(required) > 0,
		EvidenceComplete: true, ProcessingError: snap.ProcessingError, DuplicateEvents: snap.DuplicateEvents,
		Now: snap.Now, LastSuccessfulEvaluationAt: snap.LastSuccessfulEvaluationAt,
		PollingDelay: snap.PollingDelay, PollingDelayLimit: snap.PollingDelayLimit}
	var missing, stale []string
	for _, kind := range required {
		feed := snap.Feeds[kind]
		if feed.Unsupported {
			in.SourceSupported = false
		}
		if !feed.Available || !feed.Verified {
			in.RequiredTelemetryPresent = false
			missing = append(missing, string(kind)+"_UNAVAILABLE")
			if kind == TelemetryADMEvents {
				// Report absence as missing telemetry, not as a stale source.
				in.LatestSourceAt, in.SourceMaxAge = snap.Now, time.Hour
			}
			continue
		}
		if feed.Static {
			continue
		}
		switch kind {
		case TelemetryADMEvents:
			in.LatestSourceAt, in.SourceMaxAge = feed.LatestAt, feed.MaxAge
		case TelemetryPositionSamples:
			in.PositionRequired, in.LatestPositionAt, in.PositionMaxAge = true, feed.LatestAt, feed.MaxAge
		default:
			if feed.MaxAge <= 0 || feed.LatestAt.IsZero() || feed.LatestAt.After(snap.Now) || snap.Now.Sub(feed.LatestAt) > feed.MaxAge {
				stale = append(stale, string(kind)+"_STALE")
			}
		}
	}
	if !containsKind(required, TelemetryADMEvents) {
		// Modules without an ADM dependency are judged by their own feeds only.
		in.LatestSourceAt, in.SourceMaxAge = snap.Now, time.Hour
	}
	out := AssessDetectorHealth(in)
	if out.State == "DISABLED" || out.State == "UNSUPPORTED" || out.State == "ERROR" {
		return out
	}
	if len(stale) > 0 && out.State != "DEGRADED" {
		out.State, out.Reasons = "DEGRADED", nil
	}
	if out.State == "DEGRADED" {
		out.Reasons = append(out.Reasons, stale...)
	} else {
		out.Reasons = append(out.Reasons, missing...)
	}
	sort.Strings(out.Reasons)
	out.ConclusionsSuspended = out.State != "ACTIVE"
	return out
}

func containsKind(kinds []TelemetryKind, want TelemetryKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
