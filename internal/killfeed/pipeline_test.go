package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

type recordingPublisher struct {
	kills []*Event
}

func (r *recordingPublisher) PublishKill(ev *Event) error {
	r.kills = append(r.kills, ev)
	return nil
}

// TestEnginePublishesEachExplicitKillExactlyOnce feeds an explicit kill line twice
// (simulating a re-read/rotation overlap) and asserts the publisher is called once.
func TestEnginePublishesEachExplicitKillExactlyOnce(t *testing.T) {
	killLine := `16:40:12 | Player "ookylianoo" (DEAD) (id=v9 pos=<1.0, 2.0, 3.0>) killed by Player "MmeyAFK_7" (id=k9 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`

	fake := &fakeLogSource{
		logs: []nitrado.LogFile{{
			Name:     "DayZServer_PS4_x64_test.ADM",
			Path:     "/logs/test.ADM",
			Size:     int64(len(killLine) + 1),
			Modified: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
			Type:     "ADM",
		}},
		content: []byte(killLine + "\n"),
	}

	engine := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	engine.SetKillPublisher(pub)

	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil { // discovery -> select
		t.Fatalf("discovery failed: %v", err)
	}
	if err := engine.PollOnce(ctx); err != nil { // read + parse + publish
		t.Fatalf("first read failed: %v", err)
	}

	if len(pub.kills) != 1 {
		t.Fatalf("expected exactly 1 published kill, got %d", len(pub.kills))
	}
	if pub.kills[0].Killer.Name != "MmeyAFK_7" || pub.kills[0].Victim.Name != "ookylianoo" {
		t.Fatalf("unexpected published kill: %+v", pub.kills[0])
	}
	if engine.Metrics().ExplicitKillsParsed != 1 {
		t.Fatalf("expected explicit_kills_parsed=1, got %d", engine.Metrics().ExplicitKillsParsed)
	}
	if engine.Metrics().DiscordKillsPublished != 1 {
		t.Fatalf("expected discord_kills_published=1, got %d", engine.Metrics().DiscordKillsPublished)
	}

	// Simulate the same line being reprocessed (replay): dedupe must drop it.
	engine.processLines([]string{killLine})
	if len(pub.kills) != 1 {
		t.Fatalf("expected dedupe to prevent a second publish, got %d", len(pub.kills))
	}
	if engine.Metrics().DuplicateEventsDropped != 1 {
		t.Fatalf("expected duplicate_events_dropped=1, got %d", engine.Metrics().DuplicateEventsDropped)
	}
}
