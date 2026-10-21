package caseintel

import (
	"testing"
	"time"
)

func TestShadowBaseBoostOnRetainedBuilds(t *testing.T) {
	now := time.Date(2026, 10, 1, 6, 31, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	build := func(id, player int64, x, z float64) ShadowBuildEvent {
		p := player
		// bootB is local 02:00 on 2026-10-01; 02:30 local is 06:30 UTC at UTC-4.
		return ShadowBuildEvent{Action: "Built", Object: "Wall", Event: Event{ID: id, SourceID: bootB, SourceEndOffset: id * 100,
			Type: "BUILD_ACTION", ADMClock: "02:30:00", IngestedAt: time.Date(2026, 10, 1, 6, 30, 20, 0, time.UTC),
			SubjectID: &p, SubjectX: f(x), SubjectZ: f(z)}}
	}
	base := RegisteredBase{BaseID: "#7", Verified: true, OwnerPlayerID: 10, X: 1000, Z: 2000, Radius: 50,
		RegisteredAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), AuthorizedPlayers: []int64{30}}
	in := ShadowBaseInput{Scope: Core8Scope{1, 2, 3}, UTCOffsetMinutes: &minus4, Telemetry: shadowTelemetry(now), BuildActionsLogged: true,
		Bases:  []RegisteredBase{base},
		Builds: []ShadowBuildEvent{build(1, 10, 1005, 2005), build(2, 20, 1010, 2010), build(3, 30, 1000, 2000), build(4, 20, 3000, 3000)}}

	r := ShadowBaseBoost(in)
	if r.Result.Status != "OBSERVATION_ONLY" || len(r.Result.Findings) != 1 || r.Result.Findings[0].PlayerID != 20 ||
		r.Result.Findings[0].AffectedBaseID != "#7" {
		t.Fatalf("expected one intrusion by player 20: %+v", r.Result)
	}
	if r.Result.CanNotify || r.Result.ViolationEstablished || r.Result.Enforcement != "DISABLED" {
		t.Fatalf("shadow run could notify or enforce: %+v", r.Result)
	}
	if r.Result.Exclusions["AUTHORIZED_PLAYER"] != 2 || r.PlayersChecked != 3 || r.TrustedEvents != 4 || r.BasesChecked != 1 {
		t.Fatalf("owner and friend not excluded or counts wrong: %+v", r)
	}

	late := in
	late.Bases = []RegisteredBase{base}
	late.Bases[0].RegisteredAt = now
	if r := ShadowBaseBoost(late); len(r.Result.Findings) != 0 || r.Result.Exclusions["NOT_REGISTERED_AT_EVENT_TIME"] == 0 {
		t.Fatalf("build before registration counted: %+v", r.Result)
	}

	off := in
	off.BuildActionsLogged = false
	if r := ShadowBaseBoost(off); r.Result.Status != "INSUFFICIENT_EVIDENCE" || len(r.Result.Findings) != 0 {
		t.Fatalf("no build logging must not conclude: %+v", r.Result)
	}
	none := in
	none.Bases = nil
	if r := ShadowBaseBoost(none); r.Result.Status != "INSUFFICIENT_EVIDENCE" {
		t.Fatalf("no registered bases must not conclude: %+v", r.Result)
	}
	stale := in
	stale.Telemetry = shadowTelemetry(now)
	feed := stale.Telemetry.Feeds[TelemetryADMEvents]
	feed.LatestAt = now.Add(-time.Hour)
	stale.Telemetry.Feeds = map[TelemetryKind]TelemetryFeed{TelemetryADMEvents: feed}
	if r := ShadowBaseBoost(stale); r.Result.Status != "SUSPENDED" || len(r.Result.Findings) != 0 {
		t.Fatalf("stale ADM evaluated: %+v", r.Result)
	}
}
