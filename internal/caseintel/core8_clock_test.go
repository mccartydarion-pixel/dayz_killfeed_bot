package caseintel

import (
	"testing"
	"time"
)

const bootA = "/games/ni1/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-30_22-00-00.ADM"
const bootB = "/games/ni1/noftp/dayzps/config/DayZServer_PS4_x64_2026-10-01_02-00-00.ADM"

// Server is UTC-4 (restart.log "-0400"): local 22:00 is 02:00 UTC next day.
var minus4 = -240

func clockEv(id int64, source string, offset int64, clock string, ingested time.Time) Event {
	return Event{ID: id, SourceID: source, SourceEndOffset: offset, ADMClock: clock, IngestedAt: ingested}
}

func TestResolveSourceTimesAnchorsRolloverAndZone(t *testing.T) {
	ing := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	events := []Event{
		clockEv(3, bootA, 300, "00:10:00", ing.Add(130*time.Minute)), // after midnight
		clockEv(1, bootA, 100, "22:05:00", ing.Add(5*time.Minute)),
		clockEv(2, bootA, 200, "23:59:30", ing.Add(120*time.Minute)),
		clockEv(4, bootA, 400, "00:09:00", ing.Add(131*time.Minute)), // backwards step
		clockEv(5, bootA, 500, "25:00:00", ing),
	}
	got := ResolveSourceTimes(events, &minus4)
	want := map[int64]time.Time{
		1: time.Date(2026, 10, 1, 2, 5, 0, 0, time.UTC),
		2: time.Date(2026, 10, 1, 3, 59, 30, 0, time.UTC),
		3: time.Date(2026, 10, 1, 4, 10, 0, 0, time.UTC),
	}
	for id, w := range want {
		if !got[id].Trusted || !got[id].EventAt.Equal(w) {
			t.Fatalf("event %d: %+v want %s", id, got[id], w)
		}
	}
	if got[4].Trusted || got[4].Reason != "CLOCK_OUT_OF_ORDER" || got[5].Reason != "CLOCK_MALFORMED" {
		t.Fatalf("bad rows trusted: %+v %+v", got[4], got[5])
	}
	for _, e := range events {
		if r := ResolveSourceTimes([]Event{e}, nil)[e.ID]; r.Trusted || r.Reason != "UTC_OFFSET_UNKNOWN" {
			t.Fatalf("time trusted without learned offset: %+v", r)
		}
	}
	if r := ResolveSourceTimes([]Event{clockEv(9, "server.ADM", 1, "10:00:00", ing)}, &minus4)[9]; r.Trusted || r.Reason != "BOOT_STAMP_UNAVAILABLE" {
		t.Fatalf("unstamped source trusted: %+v", r)
	}
}

func TestResolveSourceTimesRejectsHiddenRollover(t *testing.T) {
	ing := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	// Clock moves forward one hour, but a day passed in real ingestion time.
	events := []Event{clockEv(1, bootA, 100, "22:10:00", ing), clockEv(2, bootA, 200, "23:10:00", ing.Add(25*time.Hour)), clockEv(3, bootA, 300, "23:20:00", ing.Add(25*time.Hour))}
	got := ResolveSourceTimes(events, &minus4)
	if !got[1].Trusted || got[2].Trusted || got[3].Trusted || got[2].Reason != "ROLLOVER_AMBIGUOUS" {
		t.Fatalf("hidden rollover trusted: %+v", got)
	}
	// Backfill compresses ingestion (many lines read at once): still trusted.
	backfill := []Event{clockEv(1, bootA, 100, "22:10:00", ing), clockEv(2, bootA, 200, "23:40:00", ing)}
	if got := ResolveSourceTimes(backfill, &minus4); !got[2].Trusted {
		t.Fatalf("backfill untrusted: %+v", got)
	}
}

func TestBootRestartWindows(t *testing.T) {
	ing := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	events := []Event{clockEv(1, bootA, 100, "22:10:00", ing), clockEv(2, bootA, 200, "01:50:00", ing), clockEv(3, bootB, 100, "02:01:00", ing)}
	times := ResolveSourceTimes(events, &minus4)
	w := BootRestartWindows(events, times, &minus4)
	if len(w) != 1 || !w[0].Start.Equal(time.Date(2026, 10, 1, 5, 50, 0, 0, time.UTC)) || !w[0].End.Equal(time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("restart window: %+v", w)
	}
	if BootRestartWindows(events, times, nil) != nil {
		t.Fatalf("restart windows without offset")
	}
}

func shadowTelemetry(now time.Time) TelemetrySnapshot {
	return TelemetrySnapshot{Now: now, PollingDelay: time.Second, PollingDelayLimit: 30 * time.Second,
		Feeds: map[TelemetryKind]TelemetryFeed{TelemetryADMEvents: {Available: true, Verified: true, LatestAt: now.Add(-10 * time.Second), MaxAge: 2 * time.Minute}}}
}

func TestShadowSuspiciousLoginsOnRetainedEvidence(t *testing.T) {
	player := int64(42)
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	var events []Event
	id := int64(1)
	// Local 23:00 onward (03:00 UTC); ingestion lags 30s, like live polling.
	add := func(source string, localMin int, typ string) {
		boot, _ := BootStamp(source)
		local := time.Date(boot.Year(), boot.Month(), boot.Day(), 23, 0, 0, 0, time.UTC).Add(time.Duration(localMin) * time.Minute)
		events = append(events, Event{ID: id, SourceID: source, SourceEndOffset: id * 100, Type: typ, ADMClock: local.Format("15:04:05"),
			IngestedAt: local.Add(4*time.Hour + 30*time.Second), SubjectID: &player})
		id++
	}
	add(bootA, 0, "PLAYER_CONNECT")
	for i := 0; i < 4; i++ {
		add(bootA, 10+i*2, "PLAYER_DISCONNECT")
		add(bootA, 11+i*2, "PLAYER_CONNECT")
	}
	in := ShadowLoginInput{Scope: Core8Scope{1, 2, 3}, PlayerID: player, Events: events, UTCOffsetMinutes: &minus4,
		Telemetry: shadowTelemetry(now), SessionsRetained: true}
	r := ShadowSuspiciousLogins(in)
	if r.TrustedEvents != len(events) || len(r.Result.Findings) != 1 || r.Result.Status != "OBSERVATION_ONLY" ||
		r.Result.CanNotify || r.Result.Findings[0].Tier != TierObserved || !hasString(r.Result.Reasons, "THRESHOLD_NOT_VALIDATED") {
		t.Fatalf("shadow burst: %+v", r)
	}

	// Without the learned offset nothing is trusted and no conclusion is drawn.
	in.UTCOffsetMinutes = nil
	if r := ShadowSuspiciousLogins(in); r.TrustedEvents != 0 || len(r.Result.Findings) != 0 || r.Result.Status != "INSUFFICIENT_EVIDENCE" {
		t.Fatalf("untrusted clock evaluated: %+v", r)
	}
	in.UTCOffsetMinutes = &minus4

	// Backfill: ingested hours after the event, excluded as stale.
	stale := append([]Event(nil), events...)
	for i := range stale {
		stale[i].IngestedAt = stale[i].IngestedAt.Add(3 * time.Hour)
	}
	in.Events = stale
	if r := ShadowSuspiciousLogins(in); len(r.Result.Findings) != 0 || r.Result.Exclusions["SAMPLE_STALE"] == 0 {
		t.Fatalf("backfill evaluated: %+v", r)
	}
	in.Events = events

	// Collector not retaining connect lines: insufficient evidence.
	in.SessionsRetained = false
	if r := ShadowSuspiciousLogins(in); r.Result.Status != "INSUFFICIENT_EVIDENCE" || len(r.Result.Findings) != 0 {
		t.Fatalf("no collector evaluated: %+v", r)
	}
	in.SessionsRetained = true

	// Stale ADM polling suspends conclusions.
	in.Telemetry = shadowTelemetry(now)
	f := in.Telemetry.Feeds[TelemetryADMEvents]
	f.LatestAt = now.Add(-time.Hour)
	in.Telemetry.Feeds[TelemetryADMEvents] = f
	if r := ShadowSuspiciousLogins(in); r.Result.Status != "SUSPENDED" || len(r.Result.Findings) != 0 {
		t.Fatalf("stale ADM evaluated: %+v", r)
	}
}

func TestShadowLoginsExcludesReconnectAcrossBootRestart(t *testing.T) {
	player := int64(42)
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	mk := func(id int64, source string, local string, typ string) Event {
		boot, _ := BootStamp(source)
		l, _ := time.Parse("15:04:05", local)
		at := time.Date(boot.Year(), boot.Month(), boot.Day(), l.Hour(), l.Minute(), l.Second(), 0, time.UTC)
		if at.Before(boot) {
			at = at.AddDate(0, 0, 1)
		}
		return Event{ID: id, SourceID: source, SourceEndOffset: id * 100, Type: typ, ADMClock: local, IngestedAt: at.Add(4*time.Hour + 20*time.Second), SubjectID: &player}
	}
	// Four disconnect/connect pairs, each straddling a boot boundary: all restarts.
	events := []Event{mk(1, bootA, "01:59:00", "PLAYER_DISCONNECT"), mk(2, bootB, "02:01:00", "PLAYER_CONNECT")}
	r := ShadowSuspiciousLogins(ShadowLoginInput{Scope: Core8Scope{1, 2, 3}, PlayerID: player, Events: events, UTCOffsetMinutes: &minus4,
		Telemetry: shadowTelemetry(now), SessionsRetained: true})
	if r.RestartWindows != 1 || len(r.Result.Findings) != 0 || r.Result.Exclusions["SERVER_RESTART"] != 1 {
		t.Fatalf("restart reconnect not excluded: %+v", r)
	}
}
