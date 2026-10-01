//go:build integration

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/planner"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestPeakHoursPlanner(t *testing.T) {
	w := newStandoutWorld(t)
	w.a.Retention = repository.NewRetentionRepository(w.a.DB.Pool)
	ctx := context.Background()
	// A busy two-hour block at 20:00-22:00 UTC on the same weekday for three weeks.
	base := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	for week := 0; week < 3; week++ {
		for h, peak := range map[int]int{20: 30, 21: 28, 4: 1} {
			hour := base.AddDate(0, 0, -7*week).Add(time.Duration(h) * time.Hour)
			if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO server_hourly_activity(guild_id, server_id, hour, peak_players, player_seconds, samples) VALUES($1,$2,$3,$4,100,10)`,
				w.guildID, w.serverID, hour, peak); err != nil {
				t.Fatal(err)
			}
		}
	}
	a, b := w.player("Alpha"), w.player("Bravo")
	w.plainKill(a, b, base.Add(20*time.Hour+10*time.Minute), 50)
	w.plainKill(b, a, base.Add(20*time.Hour+40*time.Minute), 80)

	rr := w.call(w.a.handlePeakHoursPlanner, http.MethodGet, w.path("/planner/peak-hours")+"?days=28&tz=UTC&eventHours=2", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("planner: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[struct {
		TimeZone string       `json:"timeZone"`
		Plan     planner.Plan `json:"plan"`
	}](t, rr)
	dow := int(base.Weekday())
	cell := got.Plan.Grid[dow*24+20]
	if cell.Samples != 3 || cell.AveragePeak != 30 || cell.Kills != 2 {
		t.Fatalf("20:00 cell: %+v", cell)
	}
	if len(got.Plan.EventWindows) == 0 || got.Plan.EventWindows[0].Weekday != dow || got.Plan.EventWindows[0].StartHour != 20 {
		t.Fatalf("best event window: %+v", got.Plan.EventWindows)
	}
	if len(got.Plan.RestartWindows) == 0 || got.Plan.RestartWindows[0].StartHour != 4 {
		t.Fatalf("quietest restart hour: %+v", got.Plan.RestartWindows)
	}
	if rr := w.call(w.a.handlePeakHoursPlanner, http.MethodGet, w.path("/planner/peak-hours")+"?tz=Not/AZone", w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad tz: %d", rr.Code)
	}
	if rr := w.call(w.a.handlePeakHoursPlanner, http.MethodGet, w.path("/planner/peak-hours"), "stranger", nil, nil); rr.Code == http.StatusOK {
		t.Fatal("non-staff must not read the planner")
	}
}
