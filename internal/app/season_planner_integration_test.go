//go:build integration

package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSeasonPlanner(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.Seasons = repository.NewSeasonRepository(pool)
	w.a.Ranked = repository.NewRankedRepository(pool)
	w.a.SeasonPlanner = repository.NewSeasonPlannerRepository(pool)
	owner := w.f.OwnerDiscordID
	thresholds := ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}

	season, err := w.a.Seasons.Start(ctx, w.guildID, "Season 1", time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a, b := w.player("Alpha"), w.player("Bravo")
	for i := 0; i < 3; i++ {
		w.plainKill(a, b, time.Now().Add(-time.Duration(i+1)*time.Hour), 120+float64(i))
	}
	if _, err := pool.Exec(ctx, `UPDATE kills SET season_id=$1 WHERE guild_id=$2`, season.ID, w.guildID); err != nil {
		t.Fatal(err)
	}

	// Preview: the active stats season and what it holds; no Ranked season yet.
	rr := w.call(w.a.handleSeasonPlanner, http.MethodGet, w.path("/seasons/planner"), owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("planner: %d %s", rr.Code, rr.Body.String())
	}
	view := decodeBody[struct {
		StatsSeason  *repository.StatsSeasonPreview  `json:"statsSeason"`
		RankedSeason *repository.RankedSeasonPreview `json:"rankedSeason"`
		Items        []repository.SeasonAction       `json:"items"`
	}](t, rr)
	if view.StatsSeason == nil || view.StatsSeason.Kills != 3 || len(view.StatsSeason.TopKillers) != 1 || view.StatsSeason.TopKillers[0].Name != "Alpha" || view.StatsSeason.LongestKill == nil {
		t.Fatalf("stats preview: %+v", view.StatsSeason)
	}
	if view.RankedSeason != nil || len(view.Items) != 0 {
		t.Fatalf("expected no Ranked season and no actions: %+v", view)
	}

	// Only the owner, with the typed phrase, at a sane time.
	runAt := time.Now().UTC().Add(2 * time.Hour)
	statsReq := map[string]any{"kind": "STATS_SEASON", "runAt": runAt, "newSeasonName": "Season 2", "confirm": "SCHEDULE STATS RESET"}
	if rr := w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), "stranger", statsReq, nil); rr.Code == http.StatusOK {
		t.Fatal("non-owner scheduled a reset")
	}
	for name, body := range map[string]map[string]any{
		"no confirm": {"kind": "STATS_SEASON", "runAt": runAt, "newSeasonName": "S2"},
		"past":       {"kind": "STATS_SEASON", "runAt": time.Now().Add(-time.Hour), "newSeasonName": "S2", "confirm": "SCHEDULE STATS RESET"},
		"no name":    {"kind": "STATS_SEASON", "runAt": runAt, "confirm": "SCHEDULE STATS RESET"},
		"no ranked":  {"kind": "RANKED_RESET", "runAt": runAt, "confirm": "SCHEDULE RANKED RESET"},
	} {
		if rr := w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), owner, body, nil); rr.Code == http.StatusOK {
			t.Errorf("%s: expected rejection", name)
		}
	}
	rr = w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), owner, statsReq, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("schedule stats: %d %s", rr.Code, rr.Body.String())
	}
	statsAction := decodeBody[repository.SeasonAction](t, rr)
	if rr := w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), owner, statsReq, nil); rr.Code == http.StatusOK {
		t.Fatal("a second pending stats rollover must be refused")
	}

	// Ranked reset keeps the server's current rules.
	if _, err := w.a.Ranked.StartServerSeason(ctx, w.guildID, w.serverID, 100, thresholds, false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	rr = w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), owner,
		map[string]any{"kind": "RANKED_RESET", "runAt": runAt, "confirm": "SCHEDULE RANKED RESET"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("schedule ranked: %d %s", rr.Code, rr.Body.String())
	}
	rankedAction := decodeBody[repository.SeasonAction](t, rr)
	if rankedAction.ServerID == nil || *rankedAction.ServerID != w.serverID {
		t.Fatalf("ranked action server: %+v", rankedAction)
	}

	// Notices post once.
	notices, err := w.a.SeasonPlanner.PendingNotices(ctx, w.guildID, 10)
	if err != nil || len(notices) != 2 {
		t.Fatalf("notices: %d %v", len(notices), err)
	}
	for _, n := range notices {
		_ = w.a.SeasonPlanner.MarkNoticeSent(ctx, w.guildID, n.ID, time.Now())
	}
	if again, _ := w.a.SeasonPlanner.PendingNotices(ctx, w.guildID, 10); len(again) != 0 {
		t.Fatal("notices must post once")
	}

	// Nothing runs early; at run time both apply exactly once.
	w.a.runSeasonPlanner(ctx, w.guildID, time.Now().UTC())
	if active, _ := w.a.Seasons.GetActiveSeason(ctx, w.guildID); active == nil || active.ID != season.ID {
		t.Fatal("a scheduled change ran early")
	}
	later := runAt.Add(time.Minute)
	w.a.runSeasonPlanner(ctx, w.guildID, later)
	active, err := w.a.Seasons.GetActiveSeason(ctx, w.guildID)
	if err != nil || active == nil || active.Name != "Season 2" {
		t.Fatalf("new stats season: %+v %v", active, err)
	}
	if res, err := w.a.Seasons.GetSeasonResults(ctx, w.guildID, season.ID); err != nil || res == nil || res.TopPlayerKills != 3 {
		t.Fatalf("old season archived with results: %+v %v", res, err)
	}
	var archived, live int
	var rp int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='ARCHIVED'),count(*) FILTER (WHERE status='ACTIVE'),MAX(rp_per_kill) FILTER (WHERE status='ACTIVE') FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&archived, &live, &rp); err != nil || archived != 1 || live != 1 || rp != 100 {
		t.Fatalf("ranked reset with same rules: archived=%d live=%d rp=%d %v", archived, live, rp, err)
	}
	w.a.runSeasonPlanner(ctx, w.guildID, later.Add(time.Hour))
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='ARCHIVED') FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&archived); err != nil || archived != 1 {
		t.Fatalf("a change must run once: archived=%d", archived)
	}
	list := w.call(w.a.handleSeasonPlanner, http.MethodGet, w.path("/seasons/planner"), owner, nil, nil)
	items := decodeBody[struct {
		Items []repository.SeasonAction `json:"items"`
	}](t, list).Items
	for _, it := range items {
		if it.Status != "DONE" || it.ResultDetail == "" {
			t.Fatalf("finished action: %+v", it)
		}
	}

	// Cancel: only pending actions.
	if rr := w.call(w.a.handleCancelSeasonAction, http.MethodPost, w.path("/seasons/planner/x/cancel"), owner, nil, map[string]string{"actionID": strconv.FormatInt(statsAction.ID, 10)}); rr.Code != http.StatusNotFound {
		t.Fatalf("cancel done action: %d", rr.Code)
	}
	statsReq["runAt"] = time.Now().UTC().Add(24 * time.Hour)
	statsReq["newSeasonName"] = "Season 3"
	rr = w.call(w.a.handleScheduleSeasonAction, http.MethodPost, w.path("/seasons/planner"), owner, statsReq, nil)
	next := decodeBody[repository.SeasonAction](t, rr)
	if rr := w.call(w.a.handleCancelSeasonAction, http.MethodPost, w.path("/seasons/planner/x/cancel"), owner, nil, map[string]string{"actionID": strconv.FormatInt(next.ID, 10)}); rr.Code != http.StatusOK {
		t.Fatalf("cancel pending: %d %s", rr.Code, rr.Body.String())
	}

	// An action interrupted mid-run is failed, never re-run.
	var stuck int64
	if err := pool.QueryRow(ctx, `INSERT INTO scheduled_season_actions(guild_id,kind,run_at,new_season_name,status,started_at) VALUES($1,'STATS_SEASON',NOW(),'X','RUNNING',NOW()-INTERVAL '1 hour') RETURNING id`, w.guildID).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	w.a.runSeasonPlanner(ctx, w.guildID, time.Now().UTC())
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM scheduled_season_actions WHERE id=$1`, stuck).Scan(&status); err != nil || status != "FAILED" {
		t.Fatalf("stuck action: %s %v", status, err)
	}
}
