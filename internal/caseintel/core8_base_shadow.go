package caseintel

import (
	"sort"
	"strconv"
)

// ShadowBuildEvent is one retained ADM build line plus the builder's faction
// (current membership; the dashboard says so).
type ShadowBuildEvent struct {
	Event     Event
	Action    string
	Object    string
	FactionID *int64
}

// ShadowBaseInput is a bounded server-wide window of build evidence and the
// server's registered bases.
type ShadowBaseInput struct {
	Scope              Core8Scope
	Builds             []ShadowBuildEvent
	Bases              []RegisteredBase
	UTCOffsetMinutes   *int
	Telemetry          TelemetrySnapshot
	BuildActionsLogged bool // the C.A.S.E. collector retains build lines for this server
	WindowTruncated    bool
}

// ShadowBaseReport is a staff-only, read-only shadow run of Base Boosting. It
// has no thresholds, so it can only observe; it cannot notify or enforce.
type ShadowBaseReport struct {
	Mode            string         `json:"mode"`
	TimeBasis       string         `json:"timeBasis"`
	BuildActions    int            `json:"buildActions"`
	TrustedEvents   int            `json:"trustedEvents"`
	UntrustedEvents map[string]int `json:"untrustedEvents"`
	BasesChecked    int            `json:"basesChecked"`
	PlayersChecked  int            `json:"playersChecked"`
	WindowTruncated bool           `json:"windowTruncated"`
	Result          Core8Result    `json:"result"`
}

// ShadowBaseBoost resolves trusted build times and runs the Base Boosting
// evaluator for every builder in the window, combining the results.
func ShadowBaseBoost(in ShadowBaseInput) ShadowBaseReport {
	report := ShadowBaseReport{Mode: "SHADOW_OBSERVATION_ONLY", TimeBasis: "ADM_BOOT_STAMP_PLUS_LINE_CLOCK_UTC_VIA_LEARNED_OFFSET",
		UntrustedEvents: map[string]int{}, BasesChecked: len(in.Bases), WindowTruncated: in.WindowTruncated, BuildActions: len(in.Builds)}
	events := make([]Event, 0, len(in.Builds))
	for _, b := range in.Builds {
		events = append(events, b.Event)
	}
	times := ResolveSourceTimes(events, in.UTCOffsetMinutes)

	actions := map[int64][]BuildAction{}
	unpositioned := 0
	for _, b := range in.Builds {
		t := times[b.Event.ID]
		if t.Trusted {
			report.TrustedEvents++
		} else {
			report.UntrustedEvents[t.Reason]++
		}
		if b.Event.SubjectID == nil || *b.Event.SubjectID <= 0 {
			continue
		}
		if b.Event.SubjectX == nil || b.Event.SubjectZ == nil {
			unpositioned++
			continue
		}
		pid := *b.Event.SubjectID
		actions[pid] = append(actions[pid], BuildAction{
			TimedEvidence: TimedEvidence{EvidenceID: b.Event.ID, PlayerID: pid, EventAt: t.EventAt, TimeTrusted: t.Trusted,
				ObservedAt: b.Event.IngestedAt.UTC()},
			Action: b.Action, Object: b.Object, X: *b.Event.SubjectX, Z: *b.Event.SubjectZ,
			Altitude: b.Event.SubjectAltitude, FactionID: b.FactionID})
	}

	snap := in.Telemetry
	feeds := map[TelemetryKind]TelemetryFeed{}
	for k, v := range snap.Feeds {
		feeds[k] = v
	}
	timeKnown := in.UTCOffsetMinutes != nil
	feeds[TelemetryBuildActions] = TelemetryFeed{Available: in.BuildActionsLogged, Verified: in.BuildActionsLogged && timeKnown, Static: true}
	registry := len(in.Bases) > 0
	feeds[TelemetryBaseRegistry] = TelemetryFeed{Available: registry, Verified: registry, Static: true}
	snap.Feeds = feeds
	params := DefaultCore8Params()
	params.MaxSampleLag = ShadowMaxSampleLag
	evalFor := func(playerID int64, acts []BuildAction) Core8Result {
		return EvaluateBaseBoost(EvalContext{Scope: in.Scope, PlayerID: playerID, Enabled: true, Mode: SensitivityBalanced,
			Thresholds: nil, Telemetry: snap, Params: params}, BaseBoostInput{Actions: acts, Bases: in.Bases})
	}

	// A run with no actions reports the detector's health on its own.
	combined := evalFor(1, nil)
	if combined.Status != "NO_OBSERVATION" {
		report.Result = combined
		return report
	}
	players := make([]int64, 0, len(actions))
	for pid := range actions {
		players = append(players, pid)
	}
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	report.PlayersChecked = len(players)
	reasons := map[string]bool{}
	for _, r := range combined.Reasons {
		reasons[r] = true
	}
	for _, pid := range players {
		res := evalFor(pid, actions[pid])
		for k, v := range res.Exclusions {
			combined.Exclusions[k] += v
		}
		for _, r := range res.Reasons {
			reasons[r] = true
		}
		combined.Findings = append(combined.Findings, res.Findings...)
	}
	if unpositioned > 0 {
		combined.Exclusions["POSITION_MISSING"] += unpositioned
	}
	sort.Slice(combined.Findings, func(i, j int) bool { return combined.Findings[i].EventAt.After(combined.Findings[j].EventAt) })
	if len(combined.Findings) > 0 {
		combined.Status = "OBSERVATION_ONLY"
	}
	combined.Reasons = combined.Reasons[:0]
	for r := range reasons {
		combined.Reasons = append(combined.Reasons, r)
	}
	sort.Strings(combined.Reasons)
	// Shadow runs never notify, whatever the per-player results said.
	combined.CanNotify, combined.ViolationEstablished, combined.Enforcement = false, false, "DISABLED"
	return report.withResult(combined)
}

func (r ShadowBaseReport) withResult(res Core8Result) ShadowBaseReport {
	r.Result = res
	return r
}

// BaseIDLabel formats a registered base's numeric id for findings.
func BaseIDLabel(id int64) string { return "#" + strconv.FormatInt(id, 10) }
