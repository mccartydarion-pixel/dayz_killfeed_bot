package app

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The public map's delay rule (docs/LIVE_MAP.md): a kill newer than now - delay is held back,
// a kill the client already has (id <= sinceKillId) is not repeated, and the list is newest first.
func TestFilterLiveMapKillsAppliesDelayAndCursor(t *testing.T) {
	now := time.Date(2026, 10, 3, 20, 0, 0, 0, time.UTC)
	cutoff := now.Add(-5 * time.Minute)
	kills := []repository.FightKill{
		{ID: 1, At: now.Add(-30 * time.Minute)},
		{ID: 2, At: now.Add(-10 * time.Minute)},
		{ID: 3, At: cutoff},                    // exactly at the cutoff: visible
		{ID: 4, At: now.Add(-4 * time.Minute)}, // newer than the delay: hidden
		{ID: 5, At: now},
	}
	got := filterLiveMapKills(kills, cutoff, 0, 100)
	if len(got) != 3 || got[0].ID != 3 || got[1].ID != 2 || got[2].ID != 1 {
		t.Fatalf("visible kills = %+v", got)
	}
	if got := filterLiveMapKills(kills, cutoff, 2, 100); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("since 2 = %+v", got)
	}
	if got := filterLiveMapKills(kills, cutoff, 0, 2); len(got) != 2 || got[0].ID != 3 || got[1].ID != 2 {
		t.Fatalf("limit 2 = %+v", got)
	}
	if got := filterLiveMapKills(kills, now, 0, 100); len(got) != 5 || got[0].ID != 5 {
		t.Fatalf("no delay = %+v", got)
	}
	if got := filterLiveMapKills(nil, now, 0, 100); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
}

func TestLiveMapMapFallsBackToChernarus(t *testing.T) {
	if m := liveMapMap("enoch"); m.Key != "enoch" || m.Name != "Livonia" || m.Size != 12800 || m.Guessed {
		t.Fatalf("enoch = %+v", m)
	}
	for _, key := range []string{"", "sakhal", "nope"} {
		if m := liveMapMap(key); m.Key != "chernarusplus" || m.Size != 15360 || !m.Guessed {
			t.Fatalf("%q = %+v", key, m)
		}
	}
}

func TestLiveMapTasksKeepsRestartFields(t *testing.T) {
	tasks := liveMapTasks([]nitrado.ScheduledTask{{ID: 1, ActionMethod: "restart", NextRun: "2026-10-04 00:00:00"}, {ID: 2, ActionMethod: "backup"}})
	if len(tasks) != 2 || tasks[0].ActionMethod != "restart" || tasks[0].NextRun != "2026-10-04 00:00:00" || tasks[1].ActionMethod != "backup" {
		t.Fatalf("tasks = %+v", tasks)
	}
}
