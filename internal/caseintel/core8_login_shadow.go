package caseintel

import "time"

// ShadowLoginInput is one player's bounded window of retained evidence plus
// the server's learned UTC offset and the live telemetry snapshot.
type ShadowLoginInput struct {
	Scope            Core8Scope
	PlayerID         int64
	PlayerName       string // shown on a staff alert; optional
	Events           []Event
	UTCOffsetMinutes *int
	Telemetry        TelemetrySnapshot
	SessionsRetained bool // the C.A.S.E. collector retains connect lines for this server
	WindowTruncated  bool
}

// ShadowLoginReport is a staff-only, read-only shadow run of Suspicious
// Logins over real evidence. It has no thresholds, so its status can only be
// NO_OBSERVATION or OBSERVATION_ONLY; it cannot notify or enforce.
type ShadowLoginReport struct {
	Mode            string         `json:"mode"`
	TimeBasis       string         `json:"timeBasis"`
	TrustedEvents   int            `json:"trustedEvents"`
	UntrustedEvents map[string]int `json:"untrustedEvents"`
	RestartWindows  int            `json:"restartWindows"`
	WindowTruncated bool           `json:"windowTruncated"`
	Result          Core8Result    `json:"result"`
}

// ShadowMaxSampleLag allows for Nitrado exposing ADM bytes minutes after
// they are written. Lines ingested later (backfill) are excluded as stale.
const ShadowMaxSampleLag = 15 * time.Minute

// ShadowSuspiciousLogins resolves trusted event times, derives restart
// windows from boot boundaries and runs the Suspicious Logins evaluator.
func ShadowSuspiciousLogins(in ShadowLoginInput) ShadowLoginReport {
	return evaluateLoginWindow(in, SensitivityBalanced, nil)
}

// EvaluateLoginsForAlert runs Suspicious Logins exactly like the shadow run,
// but with the owner's sensitivity and the module's released thresholds. It
// can only return CanNotify once the module is released (core8_release.go).
func EvaluateLoginsForAlert(in ShadowLoginInput, mode Sensitivity) Core8Result {
	return evaluateLoginWindow(in, mode, ReleasedThresholds("CASE-LOGIN-001")).Result
}

func evaluateLoginWindow(in ShadowLoginInput, mode Sensitivity, thresholds *ValidatedThresholds) ShadowLoginReport {
	times := ResolveSourceTimes(in.Events, in.UTCOffsetMinutes)
	report := ShadowLoginReport{Mode: "SHADOW_OBSERVATION_ONLY", TimeBasis: "ADM_BOOT_STAMP_PLUS_LINE_CLOCK_UTC_VIA_LEARNED_OFFSET",
		UntrustedEvents: map[string]int{}, WindowTruncated: in.WindowTruncated}
	var sessions []SessionEvent
	for _, e := range in.Events {
		t := times[e.ID]
		if !t.Trusted {
			report.UntrustedEvents[t.Reason]++
		} else {
			report.TrustedEvents++
		}
		if e.SubjectID == nil || *e.SubjectID != in.PlayerID {
			continue
		}
		kind := ""
		switch e.Type {
		case "PLAYER_CONNECT":
			kind = "CONNECT"
		case "PLAYER_DISCONNECT":
			kind = "DISCONNECT"
		default:
			continue
		}
		sessions = append(sessions, SessionEvent{Kind: kind, TimedEvidence: TimedEvidence{EvidenceID: e.ID, PlayerID: in.PlayerID,
			EventAt: t.EventAt, TimeTrusted: t.Trusted, ObservedAt: e.IngestedAt.UTC()}})
	}
	restarts := BootRestartWindows(in.Events, times, in.UTCOffsetMinutes)
	report.RestartWindows = len(restarts)

	snap := in.Telemetry
	feeds := map[TelemetryKind]TelemetryFeed{}
	for k, v := range snap.Feeds {
		feeds[k] = v
	}
	timeKnown := in.UTCOffsetMinutes != nil
	feeds[TelemetrySessionEvents] = TelemetryFeed{Available: in.SessionsRetained, Verified: in.SessionsRetained && timeKnown, Static: true}
	feeds[TelemetryRestartSchedule] = TelemetryFeed{Available: timeKnown, Verified: timeKnown, Static: true}
	snap.Feeds = feeds

	params := DefaultCore8Params()
	params.MaxSampleLag = ShadowMaxSampleLag
	ctx := EvalContext{Scope: in.Scope, PlayerID: in.PlayerID, PlayerName: in.PlayerName, Enabled: true, Mode: mode,
		Thresholds: thresholds, Telemetry: snap, Params: params}
	report.Result = EvaluateSuspiciousLogins(ctx, LoginInput{Sessions: sessions, Restarts: restarts})
	return report
}
