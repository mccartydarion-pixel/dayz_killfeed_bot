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
	"github.com/yourname/dayz-killfeed/internal/rewards"
)

func TestRewardsAutomation(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.Rewards = repository.NewRewardRepository(pool)
	w.a.Ranked = repository.NewRankedRepository(pool)
	w.a.Seasons = repository.NewSeasonRepository(pool)
	owner := w.f.OwnerDiscordID
	now := time.Now().UTC()
	balance := func(player int64) int64 {
		var b int64
		_ = pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, w.guildID, player).Scan(&b)
		return b
	}

	// Rules: Bronze tier, weekly 2h on 2 days, season top 1. Only the owner may set them.
	save := func(actor string, body map[string]any) *rewards.Rule {
		rr := w.call(w.a.handleSaveRewardRule, http.MethodPut, w.path("/rewards/rules"), actor, body, nil)
		if rr.Code != http.StatusOK {
			return nil
		}
		r := decodeBody[rewards.Rule](t, rr)
		return &r
	}
	if save("stranger", map[string]any{"kind": "SEASON_TOP", "points": 5, "places": 1, "enabled": true}) != nil {
		t.Fatal("non-owner saved a reward")
	}
	if rr := w.call(w.a.handleSaveRewardRule, http.MethodPut, w.path("/rewards/rules"), owner, map[string]any{"kind": "RANK_REACHED", "tier": "UNRANKED", "points": 5, "enabled": true}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid tier: %d", rr.Code)
	}
	bronze := save(owner, map[string]any{"kind": "RANK_REACHED", "tier": "BRONZE", "points": 200, "enabled": true})
	weekly := save(owner, map[string]any{"kind": "WEEKLY_ACTIVE", "points": 50, "minHours": 2, "minDays": 2, "enabled": true})
	top := save(owner, map[string]any{"kind": "SEASON_TOP", "points": 1000, "places": 1, "enabled": true})
	if bronze == nil || weekly == nil || top == nil {
		t.Fatal("owner could not save rules")
	}
	// Saving again updates in place (one rule per kind and tier).
	if again := save(owner, map[string]any{"kind": "RANK_REACHED", "tier": "BRONZE", "points": 250, "enabled": true}); again == nil || again.ID != bronze.ID || again.Points != 250 {
		t.Fatalf("upsert: %+v", again)
	}
	// Rules count from their creation: backdate so last week and the season below are after it.
	if _, err := pool.Exec(ctx, `UPDATE reward_rules SET created_at=NOW()-INTERVAL '30 days' WHERE guild_id=$1`, w.guildID); err != nil {
		t.Fatal(err)
	}

	a, b, c := w.player("Ace"), w.player("Bee"), w.player("Cee")
	// Ranked: Ace reaches Bronze (thresholds 100/300/...; 100 RP per kill -> 4 kills = 400 RP).
	if _, err := w.a.Ranked.StartServerSeason(ctx, w.guildID, w.serverID, 100, ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}, ranked.DefaultSameVictimCooldownMinutes, false, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		id, _ := w.killAt(a, b, now.Add(-time.Duration(40-i*6)*time.Minute), 100, 100)
		if _, err := w.a.Ranked.AwardActiveServerKill(ctx, w.serverID, id); err != nil {
			t.Fatal(err)
		}
	}
	// Weekly: Bee played 3 days last week, Cee only one.
	start, _, _ := rewards.LastFullWeek(now)
	for d := 0; d < 3; d++ {
		w.activeOn(b, start.AddDate(0, 0, d), 3600, 1)
	}
	w.activeOn(c, start.AddDate(0, 0, 1), 4*3600, 2)
	// Season: Cee topped a season that ended yesterday.
	season, err := w.a.Seasons.Start(ctx, w.guildID, "S1", now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	w.plainKill(c, b, now.Add(-48*time.Hour), 10)
	if _, err := pool.Exec(ctx, `UPDATE kills SET season_id=$1 WHERE guild_id=$2 AND killer_player_id=$3`, season.ID, w.guildID, c); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.Seasons.FinalizeSeason(ctx, w.guildID, season.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	paid, err := w.a.evaluateRewards(ctx, w.guildID, now)
	if err != nil || paid != 3 {
		t.Fatalf("first evaluation: paid=%d err=%v", paid, err)
	}
	if balance(a) != 250 || balance(b) != 50 || balance(c) != 1000 {
		t.Fatalf("balances: a=%d b=%d c=%d", balance(a), balance(b), balance(c))
	}
	if again, err := w.a.evaluateRewards(ctx, w.guildID, now.Add(time.Hour)); err != nil || again != 0 {
		t.Fatalf("each reward pays once: %d %v", again, err)
	}

	view := w.call(w.a.handleRewards, http.MethodGet, w.path("/rewards"), owner, nil, nil)
	body := decodeBody[struct {
		Rules   []rewards.Rule            `json:"rules"`
		Payouts []repository.RewardPayout `json:"payouts"`
	}](t, view)
	if len(body.Rules) != 3 || len(body.Payouts) != 3 {
		t.Fatalf("view: %+v", body)
	}
	if rr := w.call(w.a.handleDeleteRewardRule, http.MethodDelete, w.path("/rewards/rules/x"), owner, nil, map[string]string{"ruleID": strconv.FormatInt(weekly.ID, 10)}); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
}
