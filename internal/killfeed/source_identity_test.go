package killfeed

import (
	"context"
	"testing"
	"time"
)

// Live Sync phase 2 (finding A8): a kill or death read from a real ADM file persists the line's
// physical source - canonical file, end offset, server-local time - the same identity its location
// rows carry, so heatmaps can join them. event_time stays NULL: ADM lines carry no date.
func TestKillAndDeathPersistTheirADMSourceIdentity(t *testing.T) {
	store := newFakePersistenceStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pq := NewPersistenceQueueWithServerID(store, 1, 2, "sess")
	go pq.Run(ctx)
	e := NewEngine(nil, "svc", NewADMParser())
	e.SetPersistence(pq)
	const path = "/games/svc_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	kill := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	if _, err := e.processLineAt(kill, path, 900); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.kills) != 1 {
		t.Fatalf("kills: %d", len(store.kills))
	}
	k := store.kills[0]
	if k.SourceFile != "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM" || k.SourceOffset != 900 {
		t.Fatalf("kill source identity: %q %d", k.SourceFile, k.SourceOffset)
	}
	want := time.Date(2026, 9, 24, 16, 40, 12, 0, time.UTC)
	if k.SourceLocalTime == nil || !k.SourceLocalTime.Equal(want) {
		t.Fatalf("server-local time from the file stamp + line clock: %v", k.SourceLocalTime)
	}
	if k.EventTime != nil {
		t.Fatalf("event_time must not be invented: %v", k.EventTime)
	}
}
