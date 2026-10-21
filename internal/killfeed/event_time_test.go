package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// The derivation: boot date from the ADM filename + the line's clock, converted with the server's
// UTC offset.
func TestLineTimeUTCFromFilenameDateClockAndOffset(t *testing.T) {
	clock, ok := newADMClock("DayZServer_PS4_x64_2026-10-14_20-57-59.ADM")
	if !ok {
		t.Fatal("filename carries a boot timestamp")
	}
	local := clock.at("21:05:00")
	if local == nil {
		t.Fatal("line clock not parsed")
	}
	if got, want := lineTimeUTC(local, -240, true), time.Date(2026, 10, 15, 1, 5, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC-4 server: %v, want %v", got, want)
	}
	if got, want := lineTimeUTC(local, 120, true), time.Date(2026, 10, 14, 19, 5, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC+2 server: %v, want %v", got, want)
	}
	if got, want := lineTimeUTC(local, 0, true), time.Date(2026, 10, 14, 21, 5, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC server: %v, want %v", got, want)
	}
}

// A clock that goes back past midnight within one file is the next day - tracked per file.
func TestLineTimeUTCMidnightRolloverPerFile(t *testing.T) {
	a, _ := newADMClock("DayZServer_PS4_x64_2026-10-14_23-50-00.ADM")
	b, _ := newADMClock("DayZServer_PS4_x64_2026-10-14_23-50-00.ADM") // another engine/file clock
	if got, want := lineTimeUTC(a.at("23:58:00"), 60, true), time.Date(2026, 10, 14, 22, 58, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("before midnight: %v, want %v", got, want)
	}
	if got, want := lineTimeUTC(a.at("00:03:00"), 60, true), time.Date(2026, 10, 14, 23, 3, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("after midnight (local 2026-10-15 00:03 at UTC+1): %v, want %v", got, want)
	}
	if got, want := lineTimeUTC(a.at("00:04:30"), 60, true), time.Date(2026, 10, 14, 23, 4, 30, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("still the next day: %v, want %v", got, want)
	}
	// The other file's clock has not rolled over: its first line is still the 14th.
	if got, want := lineTimeUTC(b.at("23:59:00"), 60, true), time.Date(2026, 10, 14, 22, 59, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("independent file clock: %v, want %v", got, want)
	}
}

func TestLineTimeUTCUnknownOffsetOrDateIsZero(t *testing.T) {
	clock, _ := newADMClock("DayZServer_PS4_x64_2026-10-14_20-57-59.ADM")
	if got := lineTimeUTC(clock.at("21:05:00"), 0, false); !got.IsZero() {
		t.Fatalf("offset unknown must give no time, got %v", got)
	}
	if _, ok := newADMClock("DayZServer_PS4_x64.ADM"); ok {
		t.Fatal("a filename without a boot timestamp has no date")
	}
	e := &Engine{}
	if local := e.clockFor("DayZServer_PS4_x64.ADM").at("21:05:00"); local != nil {
		t.Fatalf("unknown date must give no local time, got %v", local)
	}
	if got := lineTimeUTC(nil, 0, true); !got.IsZero() {
		t.Fatalf("no local time must give no UTC time, got %v", got)
	}
	e.SetServerUTCOffset(func() (int, bool) { return 0, true })
	if got := e.loggedAtUTC(nil); !got.IsZero() {
		t.Fatalf("engine: unknown date must give no time, got %v", got)
	}
	if got := lineTimeUTC(clock.at("25:00:00"), 0, true); !got.IsZero() {
		t.Fatalf("an unparseable clock must give no time, got %v", got)
	}
}

func TestPlausibleEventTimeBounds(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"zero", time.Time{}, false},
		{"just now", now, true},
		{"five minutes ago", now.Add(-5 * time.Minute), true},
		{"23 hours ago", now.Add(-23 * time.Hour), true},
		{"exactly 24 hours ago", now.Add(-24 * time.Hour), true},
		{"25 hours ago", now.Add(-25 * time.Hour), false},
		{"9 minutes ahead", now.Add(9 * time.Minute), true},
		{"exactly 10 minutes ahead", now.Add(10 * time.Minute), true},
		{"11 minutes ahead", now.Add(11 * time.Minute), false},
	}
	for _, c := range cases {
		if got := plausibleEventTime(c.at, now); got != c.want {
			t.Errorf("%s: plausible=%v, want %v", c.name, got, c.want)
		}
	}
}

// An implausible converted time leaves Timestamp zero (stored as NULL) and warns at most every
// eventTimeWarnEvery; the latency measurement's LoggedAt is untouched.
func TestStampEventTimeRejectsImplausibleAndRateLimitsWarning(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e := &Engine{serverID: 7}
	stale := &Event{Type: EventPlayerKill, LoggedAt: now.Add(-30 * time.Hour)}
	e.stampEventTime(stale, now)
	if !stale.Timestamp.IsZero() || stale.TimestampDerived {
		t.Fatalf("stale time stored: %v derived=%v", stale.Timestamp, stale.TimestampDerived)
	}
	if !e.lastEventTimeWarn.Equal(now) {
		t.Fatalf("first implausible time must warn: lastWarn=%v", e.lastEventTimeWarn)
	}
	ahead := &Event{Type: EventPlayerKill, LoggedAt: now.Add(time.Hour)}
	e.stampEventTime(ahead, now.Add(time.Minute))
	if !ahead.Timestamp.IsZero() {
		t.Fatalf("future time stored: %v", ahead.Timestamp)
	}
	if !e.lastEventTimeWarn.Equal(now) {
		t.Fatalf("a second implausible time within %v must not warn again: lastWarn=%v", eventTimeWarnEvery, e.lastEventTimeWarn)
	}
	e.stampEventTime(ahead, now.Add(eventTimeWarnEvery))
	if !e.lastEventTimeWarn.Equal(now.Add(eventTimeWarnEvery)) {
		t.Fatalf("after %v the warning is due again: lastWarn=%v", eventTimeWarnEvery, e.lastEventTimeWarn)
	}
	if !ahead.LoggedAt.Equal(now.Add(time.Hour)) {
		t.Fatal("LoggedAt (latency measurement) must not be touched")
	}
	fine := &Event{Type: EventPlayerKill, LoggedAt: now.Add(-3 * time.Minute)}
	e.stampEventTime(fine, now)
	if !fine.Timestamp.Equal(now.Add(-3*time.Minute)) || !fine.TimestampDerived {
		t.Fatalf("plausible time not stored: %v derived=%v", fine.Timestamp, fine.TimestampDerived)
	}
	unknown := &Event{Type: EventPlayerKill}
	e.stampEventTime(unknown, now)
	if !unknown.Timestamp.IsZero() {
		t.Fatalf("no converted time must give no Timestamp, got %v", unknown.Timestamp)
	}
}

// End to end through the engine: a kill line read from a file whose boot date is today gets its
// real UTC time as Event.Timestamp (so it is persisted as kills.event_time and shown on the card),
// and its fingerprint is exactly what the same line produced before the derivation existed - a
// deploy must never make the durable dedupe see history as new kills.
func TestEngineDerivesEventTimeWithoutChangingFingerprint(t *testing.T) {
	const offsetMinutes = -240 // the server clock is UTC-4
	boot := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	// The filename and header carry the boot in the server's LOCAL clock.
	bootLocal := boot.Add(offsetMinutes * time.Minute)
	name := "DayZServer_PS4_x64_" + bootLocal.Format("2006-01-02_15-04-05") + ".ADM"
	path := "/games/x/noftp/dayzps/config/" + name
	header := "AdminLog started on " + bootLocal.Format("2006-01-02") + " at " + bootLocal.Format("15:04:05") + "\n"
	kill := func(clock, victim string) string {
		return clock + ` | Player "` + victim + `" (DEAD) (id=v` + victim + ` pos=<1.0, 2.0, 3.0>) killed by Player "K" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 20.0 meters` + "\n"
	}
	content := header
	fake := &fakeLogSource{logs: []nitrado.LogFile{{Name: name, Path: path, Directory: "/games/x/noftp/dayzps/config",
		Size: int64(len(content)), Modified: time.Now().Add(-time.Minute), Type: "ADM"}}, content: []byte(content)}
	e := NewEngine(fake, "svc-event-time", NewADMParser())
	pub := &timingPublisher{}
	e.SetKillPublisher(pub)
	e.SetServerUTCOffset(func() (int, bool) { return offsetMinutes, true })
	ctx := context.Background()
	grow := func(more string) {
		content += more
		fake.content = []byte(content)
		fake.logs[0].Size = int64(len(content))
		fake.logs[0].Modified = fake.logs[0].Modified.Add(time.Second)
	}
	poll := func() {
		t.Helper()
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	poll() // discovery + selection
	poll() // first read (header)

	// The line's clock is the server-local clock: the UTC instant shifted by the offset.
	at := boot.Add(7 * time.Minute)
	local := at.Add(time.Duration(offsetMinutes) * time.Minute)
	grow(kill(local.Format("15:04:05"), "A"))
	poll()
	if len(pub.kills) != 1 {
		t.Fatalf("kills published: %d", len(pub.kills))
	}
	ev := pub.kills[0]
	if !ev.Timestamp.Equal(at) || !ev.TimestampDerived {
		t.Fatalf("Event.Timestamp = %v (derived=%v), want %v", ev.Timestamp, ev.TimestampDerived, at)
	}
	if !ev.LoggedAt.Equal(at) {
		t.Fatalf("Timestamp and LoggedAt must come from the one derivation: %v vs %v", ev.Timestamp, ev.LoggedAt)
	}
	if got := eventTimePtr(ev); got == nil || !got.Equal(at) {
		t.Fatalf("persisted event_time = %v, want %v", got, at)
	}
	if ev.Timestamp.Location() != time.UTC {
		t.Fatalf("Timestamp must be UTC, got %v", ev.Timestamp.Location())
	}
	// The durable fingerprint is the pre-derivation one: the same line with no Timestamp at all.
	before := *ev
	before.Timestamp, before.TimestampDerived = time.Time{}, false
	if fingerprint(ev) != fingerprint(&before) {
		t.Fatalf("fingerprint changed by the derived event time: %s vs %s", fingerprint(ev), fingerprint(&before))
	}
	if !e.dedupe.Contains(&before) {
		t.Fatal("the dedupe cache must recognise the same line fingerprinted the old way")
	}
}
