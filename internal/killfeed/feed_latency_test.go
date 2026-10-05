package killfeed

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestStageStatsNearestRank(t *testing.T) {
	var values []int64
	for i := int64(100); i >= 1; i-- { // 1..100, unsorted
		values = append(values, i)
	}
	got := stageStats(values)
	if got != (StageStats{Samples: 100, P50Ms: 50, P90Ms: 90, P99Ms: 99, MaxMs: 100}) {
		t.Fatalf("1..100: %+v", got)
	}
	if got := stageStats([]int64{7}); got != (StageStats{Samples: 1, P50Ms: 7, P90Ms: 7, P99Ms: 7, MaxMs: 7}) {
		t.Fatalf("single sample: %+v", got)
	}
	if got := stageStats(nil); got != (StageStats{}) {
		t.Fatalf("no samples: %+v", got)
	}
}

// One card with every stage known: each field is the difference between the right two timestamps.
func TestFeedLatencyRecordsEveryStage(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := NewFeedLatencyRegistry()
	r.now = func() time.Time { return now }
	logged := now.Add(-200 * time.Second)
	read := now.Add(-20 * time.Second)
	queued := now.Add(-19 * time.Second)
	posted := now.Add(-15 * time.Second)
	r.Record("KILLFEED", FeedTiming{ServerID: 7, LoggedAt: logged, ReadAt: read, ParsedAt: read.Add(300 * time.Millisecond)}, queued, posted)

	snaps := r.SnapshotServer(7)
	if len(snaps) != 1 || snaps[0].Feed != "KILLFEED" || snaps[0].DeliveredSinceStart != 1 || snaps[0].CardsWithoutServerClock != 0 {
		t.Fatalf("snapshot: %+v", snaps)
	}
	for name, w := range map[string]FeedLatencyWindow{"lastHour": snaps[0].LastHour, "last24Hours": snaps[0].Last24Hours} {
		if w.Cards != 1 || w.GameLogToBotReadMs.P50Ms != 180_000 || w.BotReadToQueuedMs.P50Ms != 1_000 ||
			w.QueuedToDiscordAcceptedMs.P50Ms != 4_000 || w.BotReadToDiscordAcceptedMs.MaxMs != 5_000 ||
			w.GameLogToDiscordAcceptedMs.P99Ms != 185_000 {
			t.Fatalf("%s: %+v", name, w)
		}
	}
	if len(r.SnapshotServer(8)) != 0 {
		t.Fatal("another server's snapshot must be empty")
	}
}

// An unknown server clock is never replaced by a guess: the card still counts and still has the
// stages after the bot read the line, but no game-log stage.
func TestFeedLatencyUnknownServerClockRecordsOnlyLaterStages(t *testing.T) {
	now := time.Now()
	r := NewFeedLatencyRegistry()
	r.Record("KILLFEED", FeedTiming{ServerID: 1, ReadAt: now.Add(-3 * time.Second)}, now.Add(-2*time.Second), now)
	s := r.SnapshotServer(1)[0]
	if s.CardsWithoutServerClock != 1 || s.LastHour.Cards != 1 {
		t.Fatalf("counts: %+v", s)
	}
	if s.LastHour.GameLogToBotReadMs.Samples != 0 || s.LastHour.GameLogToDiscordAcceptedMs.Samples != 0 {
		t.Fatalf("a game-log stage was invented: %+v", s.LastHour)
	}
	if s.LastHour.BotReadToDiscordAcceptedMs.Samples != 1 || s.LastHour.BotReadToDiscordAcceptedMs.P50Ms != 3000 {
		t.Fatalf("later stages missing: %+v", s.LastHour)
	}
}

// A converted line time in the future of the read, or more than a day old, is a wrong clock (a
// stale offset, a wrong date) and is left out of the game-log stages instead of skewing them.
func TestFeedLatencyRejectsImplausibleServerClock(t *testing.T) {
	now := time.Now()
	r := NewFeedLatencyRegistry()
	read := now.Add(-time.Second)
	r.Record("KILLFEED", FeedTiming{ServerID: 1, LoggedAt: read.Add(time.Hour), ReadAt: read}, read, now)       // an hour ahead
	r.Record("KILLFEED", FeedTiming{ServerID: 1, LoggedAt: read.Add(-25 * time.Hour), ReadAt: read}, read, now) // over a day old
	r.Record("KILLFEED", FeedTiming{ServerID: 1, LoggedAt: read.Add(900 * time.Millisecond), ReadAt: read}, read, now)
	s := r.SnapshotServer(1)[0]
	if s.CardsWithImplausibleServerClock != 2 || s.CardsWithoutServerClock != 0 {
		t.Fatalf("counts: %+v", s)
	}
	// Only the third card (same second, within tolerance) has a game-log stage, clamped at zero.
	if got := s.LastHour.GameLogToBotReadMs; got.Samples != 1 || got.MaxMs != 0 {
		t.Fatalf("game log -> read: %+v", got)
	}
	if s.LastHour.Cards != 3 || s.LastHour.BotReadToDiscordAcceptedMs.Samples != 3 {
		t.Fatalf("every card keeps its later stages: %+v", s.LastHour)
	}
}

func TestFeedLatencyWindowsAndBound(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := NewFeedLatencyRegistry()
	r.now = func() time.Time { return now }
	rec := func(postedAgo, took time.Duration) {
		posted := now.Add(-postedAgo)
		r.Record("DEATH_FEED", FeedTiming{ServerID: 3, ReadAt: posted.Add(-took)}, posted.Add(-took), posted)
	}
	rec(30*time.Hour, 9*time.Second) // outside both windows
	rec(5*time.Hour, 7*time.Second)  // last 24 hours only
	rec(10*time.Minute, time.Second) // both
	s := r.SnapshotServer(3)[0]
	if s.LastHour.Cards != 1 || s.LastHour.BotReadToDiscordAcceptedMs.MaxMs != 1000 {
		t.Fatalf("last hour: %+v", s.LastHour)
	}
	if s.Last24Hours.Cards != 2 || s.Last24Hours.BotReadToDiscordAcceptedMs.MaxMs != 7000 {
		t.Fatalf("last 24 hours: %+v", s.Last24Hours)
	}
	if s.DeliveredSinceStart != 3 {
		t.Fatalf("delivered since start: %d", s.DeliveredSinceStart)
	}

	// Memory is bounded: the ring keeps the newest feedLatencyMaxSamples cards.
	for i := 0; i < feedLatencyMaxSamples+50; i++ {
		rec(time.Minute, 2*time.Second)
	}
	fs := r.series[feedLatencyKey{3, "DEATH_FEED"}]
	if len(fs.ring) != feedLatencyMaxSamples {
		t.Fatalf("ring holds %d samples, want %d", len(fs.ring), feedLatencyMaxSamples)
	}
	if got := r.SnapshotServer(3)[0]; got.LastHour.Cards != feedLatencyMaxSamples || got.LastHour.BotReadToDiscordAcceptedMs.MaxMs != 2000 {
		t.Fatalf("after overflow the oldest cards are gone: cards=%d max=%d", got.LastHour.Cards, got.LastHour.BotReadToDiscordAcceptedMs.MaxMs)
	}
}

// The summary line is written at most every feedLatencyLogEvery, only for a feed that delivered
// something since its last line, and carries numbers only.
func TestFeedLatencyLogLineOnlyWithTrafficAndNumbersOnly(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	r := NewFeedLatencyRegistry()
	r.now = func() time.Time { return now }
	lines := func() int { return strings.Count(buf.String(), "event=feed_latency") }

	r.LogIfDue(9)
	if lines() != 0 {
		t.Fatal("logged for a server with no cards")
	}
	r.Record("KILLFEED", FeedTiming{ServerID: 9, LoggedAt: now.Add(-90 * time.Second), ReadAt: now.Add(-2 * time.Second)}, now.Add(-time.Second), now)
	r.LogIfDue(9)
	if lines() != 0 {
		t.Fatal("logged before the interval passed")
	}
	now = now.Add(feedLatencyLogEvery)
	r.LogIfDue(8) // another server's cycle never logs this one
	r.LogIfDue(9)
	if lines() != 1 {
		t.Fatalf("want one line after the interval, got %d: %s", lines(), buf.String())
	}
	now = now.Add(feedLatencyLogEvery)
	r.LogIfDue(9)
	if lines() != 1 {
		t.Fatal("logged again without new cards")
	}
	out := buf.String()
	for _, want := range []string{"server_id=9", "feed=KILLFEED", "cards=1", "game_log_to_discord_p50_ms=90000", "bot_read_to_discord_p50_ms=2000", "queued_to_discord_p90_ms=1000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log line lacks %q: %s", want, out)
		}
	}
}

type timingPublisher struct{ kills []*Event }

func (p *timingPublisher) PublishKill(ev *Event) error {
	p.kills = append(p.kills, ev)
	return nil
}

// The engine stamps every kill with when its bytes were downloaded and - only when the server's UTC
// offset is known - the line's own time in UTC. The line time comes from the ADM file's date and the
// line's clock; the offset is whatever the source reports at that moment.
func TestEngineStampsReadTimeAndConvertsLineTimeOnlyWithKnownOffset(t *testing.T) {
	const path = "/games/x/noftp/dayzps/config/DayZServer_PS4_x64_2026-10-05_14-00-00.ADM"
	header := "AdminLog started on 2026-10-05 at 14:00:00\n"
	kill := func(clock, victim string) string {
		return clock + ` | Player "` + victim + `" (DEAD) (id=v` + victim + ` pos=<1.0, 2.0, 3.0>) killed by Player "K" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 20.0 meters` + "\n"
	}
	content := header
	fake := &fakeLogSource{logs: []nitrado.LogFile{{Name: "DayZServer_PS4_x64_2026-10-05_14-00-00.ADM", Path: path, Directory: "/games/x/noftp/dayzps/config",
		Size: int64(len(content)), Modified: time.Now().Add(-time.Minute), Type: "ADM"}}, content: []byte(content)}
	e := NewEngine(fake, "svc-timing", NewADMParser())
	pub := &timingPublisher{}
	e.SetKillPublisher(pub)
	offsetKnown, offsetMinutes := false, 0
	e.SetServerUTCOffset(func() (int, bool) { return offsetMinutes, offsetKnown })
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

	grow(kill("14:05:09", "A"))
	before := time.Now()
	poll()
	if len(pub.kills) != 1 {
		t.Fatalf("kills published: %d", len(pub.kills))
	}
	first := pub.kills[0]
	if !first.LoggedAt.IsZero() {
		t.Fatalf("offset unknown, but a UTC line time was produced: %v", first.LoggedAt)
	}
	if first.ReadAt.Before(before) || first.ReadAt.After(time.Now()) || first.ReadAt.After(first.DetectedAt) {
		t.Fatalf("read time %v not within the poll (parsed %v)", first.ReadAt, first.DetectedAt)
	}

	offsetKnown, offsetMinutes = true, -240 // the server clock is UTC-4
	grow(kill("14:07:30", "B"))
	poll()
	if len(pub.kills) != 2 {
		t.Fatalf("kills published: %d", len(pub.kills))
	}
	if want := time.Date(2026, 10, 5, 18, 7, 30, 0, time.UTC); !pub.kills[1].LoggedAt.Equal(want) {
		t.Fatalf("line time in UTC = %v, want %v", pub.kills[1].LoggedAt, want)
	}
	if !pub.kills[1].Timestamp.IsZero() {
		t.Fatal("the measurement must not set Event.Timestamp (it is part of the durable fingerprint)")
	}
	if got := TimingOf(pub.kills[1]); !got.LoggedAt.Equal(pub.kills[1].LoggedAt) || !got.ReadAt.Equal(pub.kills[1].ReadAt) {
		t.Fatalf("TimingOf: %+v", got)
	}
}
