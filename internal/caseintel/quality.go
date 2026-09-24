package caseintel

import "time"

// QualityReport describes what the retained ADM observations can and cannot
// establish. These are collection/interpretation diagnostics, NOT player risk
// indicators. Filtered source offsets are not evidence of dropped ADM lines.
type QualityReport struct {
	CoverageStatus string `json:"coverageStatus"`
	TimeStatus string `json:"timeStatus"`
	MovementDetectorStatus string `json:"movementDetectorStatus"`
	SafeSpeedPairs int `json:"safeSpeedPairs"`
	SourceCount int `json:"sourceCount"`
	ObservationCount int `json:"observationCount"`
	WindowTruncated bool `json:"windowTruncated"`
	MissingConnectWindows int `json:"missingConnectWindows"`
	UnobservedEndWindows int `json:"unobservedEndWindows"`
	RepeatedConnectWindows int `json:"repeatedConnectWindows"`
	RespawnWithoutDeathWindows int `json:"respawnWithoutDeathWindows"`
	PostDeathSourceOrderWindows int `json:"postDeathSourceOrderWindows"`
	InvalidOrMissingADMClocks int `json:"invalidOrMissingAdmClocks"`
	SameClockAdjacentObservations int `json:"sameClockAdjacentObservations"`
	ClockDecreasesInSource int `json:"clockDecreasesInSource"`
	Blockers []string `json:"blockers"`
}

func hasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want { return true }
	}
	return false
}

// AssessQuality uses only the bounded, already-authorized reconstruction.
// Even a well-formatted ADM HH:MM:SS has no recorded date/timezone or
// subsecond precision; source line order may differ from gameplay event order.
// Ingestion timestamps cannot repair that gap. Consequently the speed and
// elapsed-time gate remains blocked unconditionally for this telemetry schema.
func AssessQuality(r Reconstruction) QualityReport {
	q := QualityReport{
		CoverageStatus: "FILTERED_SOURCE_EVENTS_ONLY",
		TimeStatus: "CLOCK_ONLY_NO_TRUSTED_ELAPSED_TIME",
		MovementDetectorStatus: "BLOCKED",
		SafeSpeedPairs: 0,
		SourceCount: len(r.Sources),
		WindowTruncated: r.WindowTruncated,
		Blockers: []string{
			"NO_DATED_HIGH_RESOLUTION_EVENT_TIME",
			"EVENT_TRIGGERED_POSITIONS_NOT_CONTINUOUS",
			"SOURCE_EMISSION_ORDER_NOT_GAMEPLAY_ORDER",
			"NO_VERIFIED_ELAPSED_TIME",
			"FILTERED_EVENTS_CANNOT_PROVE_SOURCE_COMPLETENESS",
		},
	}
	if r.WindowTruncated { q.Blockers = append(q.Blockers, "BOUNDED_PAGE_EDGE") }
	if len(r.Sources)>1 { q.Blockers = append(q.Blockers, "NO_CROSS_SOURCE_STITCH") }
	for _, src := range r.Sources {
		q.ObservationCount += src.ObservationCount
		var previousClock string
		for _, window := range src.ConnectionWindows {
			if window.StartReason=="FIRST_OBSERVED" { q.MissingConnectWindows++ }
			if window.EndReason=="UNOBSERVED_END" { q.UnobservedEndWindows++ }
			if hasFlag(window.Flags,"REPEATED_CONNECT_WITHOUT_DISCONNECT") { q.RepeatedConnectWindows++ }
			if hasFlag(window.Flags,"RESPAWN_WITHOUT_RECORDED_DEATH") { q.RespawnWithoutDeathWindows++ }
			if hasFlag(window.Flags,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER") { q.PostDeathSourceOrderWindows++ }
			for _, obs := range window.Observations {
				_,err := time.Parse("15:04:05",obs.ADMClock)
				if err!=nil || len(obs.ADMClock)!=8 {
					q.InvalidOrMissingADMClocks++
					previousClock=""
					continue
				}
				if previousClock!="" {
					if obs.ADMClock==previousClock { q.SameClockAdjacentObservations++ }
					if obs.ADMClock<previousClock { q.ClockDecreasesInSource++ }
				}
				previousClock=obs.ADMClock
			}
		}
	}
	if q.MissingConnectWindows>0 || q.UnobservedEndWindows>0 || q.RepeatedConnectWindows>0 || q.RespawnWithoutDeathWindows>0 {
		q.Blockers=append(q.Blockers,"INCOMPLETE_LIFECYCLE_BOUNDARIES")
	}
	if q.PostDeathSourceOrderWindows>0 { q.Blockers=append(q.Blockers,"POST_DEATH_SOURCE_ORDER_AMBIGUITY") }
	if q.InvalidOrMissingADMClocks>0 { q.Blockers=append(q.Blockers,"ADM_CLOCK_MISSING_OR_INVALID") }
	if q.ClockDecreasesInSource>0 { q.Blockers=append(q.Blockers,"CLOCK_DECREASE_OR_DAY_ROLLOVER_UNRESOLVED") }
	return q
}
