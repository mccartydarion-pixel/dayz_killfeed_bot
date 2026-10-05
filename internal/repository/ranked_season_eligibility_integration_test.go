//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// Server eligibility for a Ranked season is active + owning guild + console
// platform. game_servers.status is a display label and never gates it.
func TestStartServerSeasonEligibility(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guilds, servers, repo := NewGuildRepository(db.Pool), NewServerRepository(db.Pool), NewRankedRepository(db.Pool)
	guild, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-elig-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	other, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-elig-other-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	newServer := func(name, status, platform string, active bool) int64 {
		s, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("%s-%d", name, suffix), Game: "DayZ", Platform: platform, Status: status, Active: active})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	rules := ranked.Thresholds{3000, 6000, 9000, 12000, 15000, 18000, 25000}
	now := time.Now().UTC()

	for _, status := range []string{"CONNECTED", "ONLINE", "OFFLINE"} {
		id := newServer("ok-"+status, status, "PLAYSTATION", true)
		season, err := repo.StartServerSeason(ctx, guild, id, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now)
		if err != nil || season.RPPerKill != 100 || season.Thresholds != rules || season.Platform != "PLAYSTATION" {
			t.Fatalf("status %s: start must succeed with the owner's rules, got %+v %v", status, season, err)
		}
	}
	xbox := newServer("xbox", "ONLINE", "XBOX", true)
	if _, err := repo.StartServerSeason(ctx, guild, xbox, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); err != nil {
		t.Fatalf("xbox: %v", err)
	}

	inactive := newServer("inactive", "DISCONNECTED", "PLAYSTATION", false)
	pc := newServer("pc", "ONLINE", "PC", true)
	for name, id := range map[string]int64{"inactive": inactive, "pc": pc} {
		if _, err := repo.StartServerSeason(ctx, guild, id, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); !errors.Is(err, ErrRankedServerIneligible) {
			t.Fatalf("%s: expected ErrRankedServerIneligible, got %v", name, err)
		}
	}
	// Tenant scope: a real, active server is never startable from another guild.
	crossGuild := newServer("tenant", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, other, crossGuild, 100, rules, ranked.DefaultSameVictimCooldownMinutes, false, now); !errors.Is(err, ErrRankedServerIneligible) {
		t.Fatalf("cross-guild start must be rejected, got %v", err)
	}
	// Reset with nothing active is its own, explicit error.
	fresh := newServer("fresh", "CONNECTED", "PLAYSTATION", true)
	if _, err := repo.StartServerSeason(ctx, guild, fresh, 100, rules, ranked.DefaultSameVictimCooldownMinutes, true, now); !errors.Is(err, ErrRankedNoActiveSeason) {
		t.Fatalf("reset without a season: expected ErrRankedNoActiveSeason, got %v", err)
	}
	var n int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM ranked_seasons WHERE server_id = ANY($1)`, []int64{inactive, pc, crossGuild, fresh}).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected starts must create no season, got %d %v", n, err)
	}
}

// The same-victim wait is the awarding season's own frozen value: 0 awards every kill, 30 holds
// until exactly thirty minutes, and a reset to another value does not reach back into the archive.
func TestRankedSameVictimWaitIsTheSeasonsOwn(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guild, err := NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-wait-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerRepository(db.Pool).UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "fixture", ProviderServiceID: fmt.Sprintf("ranked-wait-%d", suffix), Game: "dayz", Platform: "XBOX", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	players := NewPlayerRepository(db.Pool)
	attacker, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("wait-attacker-%d", suffix), "Attacker", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	victim, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("wait-victim-%d", suffix), "Victim", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRankedRepository(db.Pool)
	kills := NewKillRepository(db.Pool)
	rules := ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}
	n := 0
	award := func(at time.Time, outcome string) {
		t.Helper()
		n++
		id, err := kills.InsertKillReturning(ctx, KillRecord{GuildID: guild, ServerID: server.ID, SessionID: "ranked-wait", Fingerprint: fmt.Sprintf("ranked-wait-%d-%d", suffix, n), KillerPlayerID: attacker, VictimPlayerID: victim, EventTime: &at})
		if err != nil {
			t.Fatal(err)
		}
		got, err := repo.AwardActiveServerKill(ctx, server.ID, id)
		if err != nil || got.Outcome != outcome {
			t.Fatalf("kill %d at %s = %+v, %v; want %s", n, at.Format(time.TimeOnly), got, err, outcome)
		}
	}
	for _, bad := range []int{-1, 121} {
		if _, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, bad, false, time.Now().UTC()); err == nil {
			t.Fatalf("a wait of %d minutes was accepted", bad)
		}
	}

	// Season 1: no wait. Every kill of the same victim counts, even in the same second.
	start := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	first, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, 0, false, start)
	if err != nil || first.SameVictimCooldownMinutes != 0 {
		t.Fatalf("start with no wait: %+v %v", first, err)
	}
	if active, err := repo.ActiveServerSeason(ctx, guild, server.ID); err != nil || active == nil || active.SameVictimCooldownMinutes != 0 {
		t.Fatalf("active season wait: %+v %v", active, err)
	}
	award(start.Add(time.Minute), "AWARDED")
	award(start.Add(time.Minute), "AWARDED")
	award(start.Add(time.Minute+time.Second), "AWARDED")

	// Season 2: thirty minutes, changed only by the reset.
	second := start.Add(time.Hour)
	next, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, 30, true, second)
	if err != nil || next.SameVictimCooldownMinutes != 30 {
		t.Fatalf("reset to thirty minutes: %+v %v", next, err)
	}
	firstKill := second.Add(time.Minute)
	award(firstKill, "AWARDED")
	award(firstKill.Add(5*time.Minute), "COOLDOWN") // the old fixed rule would have awarded this
	award(firstKill.Add(30*time.Minute-time.Second), "COOLDOWN")
	award(firstKill.Add(30*time.Minute), "AWARDED")

	// Season 3: the default five minutes behaves as before.
	third := second.Add(2 * time.Hour)
	if _, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, ranked.DefaultSameVictimCooldownMinutes, true, third); err != nil {
		t.Fatal(err)
	}
	award(third.Add(time.Minute), "AWARDED")
	award(third.Add(6*time.Minute-time.Second), "COOLDOWN")
	award(third.Add(6*time.Minute), "AWARDED")

	var waits []int32
	if err := db.Pool.QueryRow(ctx, `SELECT array_agg(same_victim_cooldown_minutes ORDER BY starts_at) FROM ranked_seasons WHERE server_id=$1`, server.ID).Scan(&waits); err != nil || len(waits) != 3 || waits[0] != 0 || waits[1] != 30 || waits[2] != 5 {
		t.Fatalf("each season keeps its own wait: %v %v", waits, err)
	}
}

// The wait can change on the ACTIVE season. A kill is judged by the wait in force at its own event
// time, whenever it is processed; decisions already made never change.
func TestRankedSameVictimWaitChangesMidSeason(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guild, err := NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-change-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerRepository(db.Pool).UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "fixture", ProviderServiceID: fmt.Sprintf("ranked-change-%d", suffix), Game: "dayz", Platform: "XBOX", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	players := NewPlayerRepository(db.Pool)
	player := func(name string) int64 {
		id, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("change-%s-%d", name, suffix), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	attacker, victim, victimB, victimC := player("attacker"), player("victim"), player("victim-b"), player("victim-c")
	repo := NewRankedRepository(db.Pool)
	kills := NewKillRepository(db.Pool)
	rules := ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}
	n := 0
	insert := func(victimID int64, at time.Time) int64 {
		t.Helper()
		n++
		id, err := kills.InsertKillReturning(ctx, KillRecord{GuildID: guild, ServerID: server.ID, SessionID: "ranked-change", Fingerprint: fmt.Sprintf("ranked-change-%d-%d", suffix, n), KillerPlayerID: attacker, VictimPlayerID: victimID, EventTime: &at})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	award := func(victimID int64, at time.Time, outcome string) int64 {
		t.Helper()
		id := insert(victimID, at)
		got, err := repo.AwardActiveServerKill(ctx, server.ID, id)
		if err != nil || got.Outcome != outcome {
			t.Fatalf("kill %d at %s = %+v, %v; want %s", n, at.Format(time.TimeOnly), got, err, outcome)
		}
		return id
	}
	type row struct {
		Kill    int64
		Outcome string
		Amount  int64
	}
	ledger := func(seasonID int64) []row {
		t.Helper()
		rows, err := db.Pool.Query(ctx, `SELECT kill_id,outcome,amount FROM ranked_awards WHERE season_id=$1 ORDER BY kill_id`, seasonID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.Kill, &r.Outcome, &r.Amount); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}

	// No active season yet.
	if _, _, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 2, "owner", time.Now().UTC()); !errors.Is(err, ErrRankedNoActiveSeason) {
		t.Fatalf("change without a season: %v", err)
	}

	start := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	season, err := repo.StartServerSeason(ctx, guild, server.ID, 100, rules, 30, false, start)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int{-1, 121} {
		if _, _, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, bad, "owner", start.Add(time.Minute)); err == nil {
			t.Fatalf("a wait of %d minutes was accepted", bad)
		}
	}
	// Another guild cannot change this server's season.
	otherGuild, err := NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-change-other-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.ChangeActiveSeasonCooldown(ctx, otherGuild, server.ID, 2, "intruder", start.Add(time.Minute)); !errors.Is(err, ErrRankedNoActiveSeason) {
		t.Fatalf("cross-guild change: %v", err)
	}

	// Under the thirty-minute wait.
	t0 := start.Add(10 * time.Minute)
	award(victim, t0, "AWARDED")
	award(victim, t0.Add(3*time.Minute), "COOLDOWN")
	// Two kills that happened BEFORE the change but whose award was not decided (a transient
	// failure): one of another victim awarded at t0+1m, repeated at t0+4m.
	award(victimB, t0.Add(time.Minute), "AWARDED")
	lateB := insert(victimB, t0.Add(4*time.Minute)) // 3 minutes after: inside 30, outside 2
	before := ledger(season.ID)

	// The owner shortens the wait to two minutes at t0+5m.
	changeAt := t0.Add(5 * time.Minute)
	changed, previous, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 2, "owner-1", changeAt)
	if err != nil || previous != 30 || changed.ID != season.ID || changed.SameVictimCooldownMinutes != 2 || changed.RPPerKill != 100 || changed.Thresholds != rules || changed.Status != "ACTIVE" || !changed.StartsAt.Equal(start) {
		t.Fatalf("change: %+v previous=%d %v", changed, previous, err)
	}
	if active, err := repo.ActiveServerSeason(ctx, guild, server.ID); err != nil || active == nil || active.ID != season.ID || active.SameVictimCooldownMinutes != 2 {
		t.Fatalf("active season after change: %+v %v", active, err)
	}
	// Past decisions are untouched: same rows, same outcomes, same amounts (the COOLDOWN at t0+3m
	// is not turned into an award even though it is outside the new wait).
	if after := ledger(season.ID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a change rewrote past decisions:\nbefore %v\nafter  %v", before, after)
	}

	// A kill that happened before the change is judged by the OLD wait even though it is
	// reconciled after the change.
	if count, err := repo.ReconcileServerAwards(ctx, server.ID); err != nil || count != 1 {
		t.Fatalf("reconcile: %d %v", count, err)
	}
	if got, err := repo.RecordServerKill(ctx, season.ID, lateB); err != nil || got.Outcome != "COOLDOWN" || got.Amount != 0 {
		t.Fatalf("pre-change kill reconciled after the change = %+v, %v; want COOLDOWN under the old wait", got, err)
	}
	// Same for one processed late through the normal path (victim C: award at t0+2m, repeat at
	// t0+5m-1s, one second before the change).
	award(victimC, t0.Add(2*time.Minute), "AWARDED")
	award(victimC, changeAt.Add(-time.Second), "COOLDOWN")

	// From the change on, the new wait: t0+6m is 6 minutes after the last award for `victim`
	// (inside the old 30, outside the new 2) and now counts; a repeat inside 2 minutes does not.
	award(victim, t0.Add(6*time.Minute), "AWARDED")
	award(victim, t0.Add(7*time.Minute), "COOLDOWN")
	award(victim, t0.Add(8*time.Minute), "AWARDED")
	// A kill exactly at the change instant already uses the new wait.
	award(victimB, changeAt, "AWARDED")

	// Raising it again applies from that moment on, and replays stay idempotent.
	raiseAt := t0.Add(9 * time.Minute)
	if _, previous, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 60, "owner-2", raiseAt); err != nil || previous != 2 {
		t.Fatalf("raise: previous=%d %v", previous, err)
	}
	award(victim, t0.Add(11*time.Minute), "COOLDOWN") // 3 minutes after t0+8m: fine under 2, not under 60
	award(victim, t0.Add(68*time.Minute), "AWARDED")
	// Saving the current value changes nothing and adds no history.
	if _, previous, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 60, "owner-2", raiseAt.Add(time.Hour)); err != nil || previous != 60 {
		t.Fatalf("no-op change: previous=%d %v", previous, err)
	}
	// A change stamped earlier than the latest one (a clock step back) cannot reorder history.
	if _, _, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 5, "owner-3", raiseAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var values []int32
	var who []string
	var ordered bool
	if err := db.Pool.QueryRow(ctx, `SELECT array_agg(cooldown_minutes ORDER BY effective_from,id),array_agg(changed_by ORDER BY effective_from,id),
bool_and(effective_from>=$2) FROM ranked_season_cooldown_changes WHERE season_id=$1`, season.ID, start).Scan(&values, &who, &ordered); err != nil ||
		fmt.Sprint(values) != "[30 2 60 5]" || fmt.Sprint(who) != "[season-start owner-1 owner-2 owner-3]" || !ordered {
		t.Fatalf("history: %v %v ordered=%v %v", values, who, ordered, err)
	}
	var total, awards int64
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0),count(*) FILTER (WHERE outcome='AWARDED') FROM ranked_awards WHERE season_id=$1`, season.ID).Scan(&total, &awards); err != nil || awards != 7 || total != 700 {
		t.Fatalf("season total=%d awards=%d %v; want 700 from 7 awards", total, awards, err)
	}
	if count, err := repo.ReconcileServerAwards(ctx, server.ID); err != nil || count != 0 {
		t.Fatalf("nothing left to reconcile: %d %v", count, err)
	}

	// A season created without history (before migration 0130, or by a fixture) is seeded with its
	// old value from its start on the first change.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM ranked_season_cooldown_changes WHERE season_id=$1`, season.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE ranked_seasons SET same_victim_cooldown_minutes=30 WHERE id=$1`, season.ID); err != nil {
		t.Fatal(err)
	}
	lateAt := t0.Add(3 * time.Hour)
	if _, previous, err := repo.ChangeActiveSeasonCooldown(ctx, guild, server.ID, 0, "owner-4", lateAt); err != nil || previous != 30 {
		t.Fatalf("change on a season without history: previous=%d %v", previous, err)
	}
	award(victimC, lateAt.Add(-2*time.Hour), "AWARDED")                 // before the change: thirty minutes
	award(victimC, lateAt.Add(-2*time.Hour+10*time.Minute), "COOLDOWN") // still the old wait
	award(victimC, lateAt.Add(time.Second), "AWARDED")                  // after: no wait
	award(victimC, lateAt.Add(time.Second), "AWARDED")
}
