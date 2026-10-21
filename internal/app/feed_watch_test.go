package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// The watch's clocks: players-online and failing-source durations run only while the condition
// holds, an unknown count neither starts nor stops one, and silence is never counted from before
// the process began watching.
func TestFeedWatchClocks(t *testing.T) {
	var w feedWatch
	t0 := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	running := func(known bool, players int) feedSample {
		return feedSample{WorkerRunning: true, PlayersKnown: known, PlayersOnline: players, SourceState: killfeed.ADMQuiet}
	}

	if v := w.view(7, t0); v.Watching {
		t.Fatal("an unwatched server has no view")
	}
	w.observe(7, running(true, 0), at(0))
	if v := w.view(7, at(10)); !v.Watching || v.PlayersOnlineFor != 0 || v.LogSilentFor != 10*time.Minute || v.PlayerListSeen {
		t.Fatalf("empty server = %+v", v)
	}
	w.observe(7, running(true, 3), at(10))
	w.observe(7, running(false, 0), at(20)) // one failed count read
	w.observe(7, running(true, 2), at(30))
	if v := w.view(7, at(40)); v.PlayersOnlineFor != 30*time.Minute || v.Sample.PlayersOnline != 2 {
		t.Fatalf("players online since minute 10 = %+v", v)
	}
	// A line read at minute 35 restarts the silence clock; a player list is remembered.
	s := running(true, 2)
	s.LastLogLineAt, s.LastPlayerList = at(35), at(35)
	w.observe(7, s, at(36))
	if v := w.view(7, at(40)); v.LogSilentFor != 5*time.Minute || !v.PlayerListSeen {
		t.Fatalf("after a line = %+v", v)
	}
	// The worker is restarted (self-healing): its counters start empty, the watch remembers.
	w.observe(7, running(true, 2), at(38))
	if v := w.view(7, at(40)); v.LogSilentFor != 5*time.Minute || !v.PlayerListSeen || v.Sample.LastPlayerList != at(35) || v.PlayersOnlineFor != 30*time.Minute {
		t.Fatalf("after a worker restart = %+v", v)
	}
	// Everybody leaves: the players clock stops.
	w.observe(7, running(true, 0), at(41))
	if v := w.view(7, at(50)); v.PlayersOnlineFor != 0 {
		t.Fatalf("empty again = %+v", v)
	}

	// The failing-source clock.
	bad := running(true, 0)
	bad.SourceState = killfeed.ADMTransportError
	w.observe(7, bad, at(50))
	w.observe(7, bad, at(60))
	if v := w.view(7, at(66)); v.SourceBadFor != 16*time.Minute {
		t.Fatalf("failing source = %+v", v)
	}
	w.observe(7, running(true, 0), at(67))
	if v := w.view(7, at(70)); v.SourceBadFor != 0 {
		t.Fatalf("recovered source = %+v", v)
	}

	// A line from before the process started watching does not count as recent silence proof,
	// and a stopped worker forgets everything, so a restarted one starts fresh.
	var fresh feedWatch
	old := running(true, 4)
	old.LastLogLineAt = at(-600)
	fresh.observe(9, old, at(0))
	if v := fresh.view(9, at(5)); v.LogSilentFor != 5*time.Minute || v.PlayersOnlineFor != 5*time.Minute {
		t.Fatalf("just started watching = %+v", v)
	}
	fresh.observe(9, feedSample{}, at(6))
	if v := fresh.view(9, at(7)); v.Watching {
		t.Fatalf("a stopped worker must be forgotten: %+v", v)
	}
}

// With no worker manager nothing is sampled and nothing panics.
func TestFeedWatchTickWithoutRuntime(t *testing.T) {
	a := &App{}
	a.feedWatchTick(time.Now())
	if s := a.sampleServer(1, time.Now()); s.WorkerRunning {
		t.Fatalf("sample without a runtime = %+v", s)
	}
}
