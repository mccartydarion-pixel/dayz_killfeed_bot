//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// Query hygiene (docs/PERFORMANCE.md): the rewritten TopByKD keeps the legacy query's exact
// results, the two list endpoints that used to issue one query per row return the same rows from
// one, the 0111 indexes exist with the expressions the queries use, and the retention sweep's
// DELETE prunes only what is older than its cutoff, in bounded batches.

func openHygieneDB(t *testing.T) (context.Context, *database.DB) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

// legacyTopByKD is the query TopByKD ran before the pre-aggregated rewrite (three correlated
// COUNT(*) subqueries per player), kept here verbatim as the reference semantics.
const legacyTopByKD = `
SELECT display_name, kills, deaths FROM (
  SELECT p.display_name,
         (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) AS kills,
         (SELECT COUNT(*) FROM deaths d WHERE d.guild_id=$1 AND d.player_id=p.id) AS deaths
  FROM players p
  WHERE p.guild_id=$1
    AND (SELECT COUNT(*) FROM kills k WHERE k.guild_id=$1 AND k.killer_player_id=p.id) >= $3
) ranked
ORDER BY (kills::float / GREATEST(deaths,1)) DESC, kills DESC, display_name ASC
LIMIT $2`

func TestTopByKDMatchesLegacySemantics(t *testing.T) {
	ctx, db := openHygieneDB(t)
	guilds := NewGuildRepository(db.Pool)
	players := NewPlayerRepository(db.Pool)
	kills := NewKillRepository(db.Pool)
	deaths := NewDeathRepository(db.Pool)
	stats := NewStatsRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-kd-legacy-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID) })

	newPlayer := func(name string) int64 {
		id, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-%s-%d", name, suffix), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Distinct names keep the legacy ordering (which has no player-id tie breaker) deterministic.
	alpha, bravo, charlie, delta, echo, foxtrot := newPlayer("Alpha"), newPlayer("Bravo"), newPlayer("Charlie"), newPlayer("Delta"), newPlayer("Echo"), newPlayer("Foxtrot")
	n := 0
	kill := func(killer, victim int64) {
		n++
		if err := kills.InsertKill(ctx, KillRecord{GuildID: guildID, KillerPlayerID: killer, VictimPlayerID: victim, Fingerprint: fmt.Sprintf("kd-legacy-%d-%d", suffix, n), WeaponDisplay: "Test Weapon"}); err != nil {
			t.Fatal(err)
		}
	}
	death := func(player int64) {
		n++
		if err := deaths.InsertDeath(ctx, DeathRecord{GuildID: guildID, PlayerID: player, Fingerprint: fmt.Sprintf("kd-legacy-death-%d-%d", suffix, n), DeathType: DeathTypeUnknown}); err != nil {
			t.Fatal(err)
		}
	}
	// alpha and bravo tie at 4/2 = 2.00 (name breaks it); delta 2/0 -> 2.00 with fewer kills;
	// echo 1/0 -> 1.00; charlie 0 kills, dies a lot; foxtrot never appears in kills or deaths.
	for i := 0; i < 4; i++ {
		kill(alpha, charlie)
		kill(bravo, charlie)
	}
	kill(delta, foxtrot)
	kill(delta, foxtrot)
	kill(echo, charlie)
	death(alpha)
	death(alpha)
	death(bravo)
	death(bravo)
	death(charlie)
	// A kill nobody is credited for (unknown killer) counts for no one.
	n++
	if err := kills.InsertKill(ctx, KillRecord{GuildID: guildID, VictimPlayerID: echo, Fingerprint: fmt.Sprintf("kd-legacy-%d-%d", suffix, n)}); err != nil {
		t.Fatal(err)
	}
	_ = foxtrot

	type row struct {
		name          string
		kills, deaths int64
	}
	legacy := func(limit, minKills int) []row {
		t.Helper()
		rows, err := db.Pool.Query(ctx, legacyTopByKD, guildID, limit, minKills)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.name, &r.kills, &r.deaths); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	for _, c := range []struct{ limit, minKills int }{{10, 0}, {10, 1}, {10, 2}, {10, 3}, {2, 0}, {3, 1}, {10, 99}} {
		want := legacy(c.limit, c.minKills)
		got, err := stats.TopByKD(ctx, guildID, c.limit, c.minKills)
		if err != nil {
			t.Fatalf("TopByKD(limit=%d,minKills=%d): %v", c.limit, c.minKills, err)
		}
		if len(got) != len(want) {
			t.Fatalf("limit=%d minKills=%d: got %d rows %+v, legacy %d rows %+v", c.limit, c.minKills, len(got), got, len(want), want)
		}
		for i := range want {
			kd := float64(want[i].kills)
			if want[i].deaths > 0 {
				kd = float64(want[i].kills) / float64(want[i].deaths)
			}
			if got[i].DisplayName != want[i].name || got[i].Value != fmt.Sprintf("%.2f", kd) {
				t.Fatalf("limit=%d minKills=%d row %d: got %+v, legacy %+v", c.limit, c.minKills, i, got[i], want[i])
			}
		}
	}
	// Spot-check the expected board so the comparison above is not vacuous.
	got, err := stats.TopByKD(ctx, guildID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantNames := []string{"Alpha", "Bravo", "Delta", "Echo", "Charlie", "Foxtrot"}
	if len(got) != len(wantNames) {
		t.Fatalf("got %+v", got)
	}
	for i, name := range wantNames {
		if got[i].DisplayName != name {
			t.Fatalf("row %d: got %+v, want %s", i, got[i], name)
		}
	}
	if got[0].Value != "2.00" || got[2].Value != "2.00" || got[3].Value != "1.00" || got[4].Value != "0.00" {
		t.Fatalf("unexpected KD values %+v", got)
	}
}

func TestListFactionsForModerationLeadersFromOneQuery(t *testing.T) {
	w := newHubWorld(t)
	u1, u2, u3 := w.newUser(), w.newUser(), w.newUser()
	f1 := w.create(w.inst1, u1, "Moderated One", "MO1", "OPEN")
	f2 := w.create(w.inst1, u2, "Moderated Two", "MO2", "OPEN")
	f3 := w.create(w.inst1, u3, "Moderated Three", "MO3", "CLOSED")
	// Faction three loses its leader row outright: the list must still include it, leaderless.
	if _, err := w.db.Pool.Exec(w.ctx, `DELETE FROM hub_faction_members WHERE faction_id=$1`, f3.ID); err != nil {
		t.Fatal(err)
	}
	list, err := w.repo.ListFactionsForModeration(w.ctx, w.org1, w.inst1, 50)
	if err != nil {
		t.Fatal(err)
	}
	wantLeader := map[int64]int64{f1.ID: u1, f2.ID: u2}
	seen := map[int64]bool{}
	for _, m := range list {
		seen[m.ID] = true
		if want, ok := wantLeader[m.ID]; ok {
			if m.Leader == nil || m.Leader.User.ID != want || m.Leader.FactionID != m.ID || m.Leader.RoleKey != "LEADER" {
				t.Fatalf("faction %d: leader %+v, want user %d", m.ID, m.Leader, want)
			}
		} else if m.ID == f3.ID && m.Leader != nil {
			t.Fatalf("faction %d has no members but a leader %+v", m.ID, m.Leader)
		}
	}
	for _, id := range []int64{f1.ID, f2.ID, f3.ID} {
		if !seen[id] {
			t.Fatalf("faction %d missing from %+v", id, list)
		}
	}
}

func TestListOwnerEventsTopThreePerEventFromOneQuery(t *testing.T) {
	ctx, db := openHygieneDB(t)
	guilds := NewGuildRepository(db.Pool)
	players := NewPlayerRepository(db.Pool)
	events := NewEventRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-owner-events-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID) })
	var ids []int64
	for _, name := range []string{"P1", "P2", "P3", "P4"} {
		id, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-%s-%d", name, suffix), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	now := time.Now().UTC()
	newEvent := func(name, status string) int64 {
		id, err := events.CreateOwnerEvent(ctx, guildID, CompetitiveEvent{Type: "KILLS", Name: name, Status: status, StartsAt: &now, Config: []byte(`{}`)}, "", false, [3]int{0, 0, 0}, "tester")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	active, ended, draft := newEvent("Active", "ACTIVE"), newEvent("Ended", "ENDED"), newEvent("Draft", "DRAFT")
	score := func(event, player int64, score float64, kills int64) {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO event_scores(event_id,player_id,score,kills) VALUES($1,$2,$3,$4)`, event, player, score, kills); err != nil {
			t.Fatal(err)
		}
	}
	// Active: four scorers, two tied on score (kills break it); only three may be listed.
	score(active, ids[0], 10, 1)
	score(active, ids[1], 30, 3)
	score(active, ids[2], 20, 2)
	score(active, ids[3], 20, 4)
	// Ended: one scorer. Draft: scores exist but a draft shows no leaders.
	score(ended, ids[2], 5, 1)
	score(draft, ids[0], 99, 9)

	list, err := events.ListOwnerEvents(ctx, guildID, 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]OwnerEvent{}
	for _, e := range list {
		byID[e.ID] = e
	}
	leaders := func(id int64) []string {
		var out []string
		for _, l := range byID[id].Leaders {
			out = append(out, fmt.Sprintf("%s:%v:%d", l.Name, l.Score, l.Kills))
		}
		return out
	}
	if got := strings.Join(leaders(active), ","); got != "P2:30:3,P4:20:4,P3:20:2" {
		t.Fatalf("active leaders: %s", got)
	}
	if got := strings.Join(leaders(ended), ","); got != "P3:5:1" {
		t.Fatalf("ended leaders: %s", got)
	}
	if e, ok := byID[draft]; !ok || len(e.Leaders) != 0 || e.Leaders == nil {
		t.Fatalf("draft must list no leaders (and never null): %+v", e)
	}
}

func TestQueryHygieneIndexesServeTheirQueries(t *testing.T) {
	ctx, db := openHygieneDB(t)
	for _, idx := range []struct{ name, expr string }{
		{"idx_players_guild_lower_name", "lower(display_name)"},
		{"idx_kills_guild_lower_weapon", "lower(weapon_display)"},
		{"idx_kills_server_event_window", "COALESCE(event_time, created_at)"},
		{"idx_live_sync_records_detected", "detected_at"},
		{"idx_combat_anomaly_flags_created", "created_at"},
	} {
		var def string
		if err := db.Pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname=$1`, idx.name).Scan(&def); err != nil {
			t.Fatalf("%s: %v", idx.name, err)
		}
		if !strings.Contains(def, idx.expr) {
			t.Fatalf("%s does not index %q: %s", idx.name, idx.expr, def)
		}
	}
	// The planner must be able to pick the expression indexes for the predicates as the
	// repositories write them. The tables are tiny here, so sequential scans are disabled for
	// this one connection to make the choice about applicability, not cost.
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var guildID int64
	if err := conn.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("hygiene-explain-%d", time.Now().UnixNano())).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID) })
	// Seed enough rows, with real statistics, that each expression index is the selective
	// choice. On an empty table every index on the guild_id prefix costs the same and the
	// planner's pick between them is arbitrary (CI chose idx_kills_guild_lower_weapon for the
	// time-window query), which says nothing about applicability.
	if _, err := conn.Exec(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name, last_seen_at)
		SELECT $1, 'hygiene-explain-' || g, 'Player ' || g, NOW() FROM generate_series(1, 400) AS g`, guildID); err != nil {
		t.Fatal(err)
	}
	var serverID int64
	if err := conn.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status, display_name)
		VALUES($1, 'qa-fixture', $2, 'dayz', 'PLAYSTATION', 'ACTIVE', 'hygiene explain') RETURNING id`, guildID, fmt.Sprintf("hygiene-explain-%d", guildID)).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, weapon_display, created_at)
		SELECT $1, $2, 'hygiene-explain', 'hygiene-explain-' || g, 'Weapon ' || (g % 40), NOW() - (g || ' minutes')::interval
		FROM generate_series(1, 1200) AS g`, guildID, serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE players; ANALYZE kills`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"idx_players_guild_lower_name", `SELECT id FROM players p WHERE p.guild_id=$1 AND LOWER(p.display_name)=LOWER($2)`, []any{guildID, "Someone"}},
		{"idx_kills_guild_lower_weapon", `SELECT COUNT(*) FROM kills k WHERE guild_id=$1 AND LOWER(weapon_display)=LOWER($2)`, []any{guildID, "M4"}},
		{"idx_kills_server_event_window", `SELECT id FROM kills k WHERE k.guild_id=$1 AND k.server_id=$2 AND COALESCE(k.event_time, k.created_at) >= $3 AND COALESCE(k.event_time, k.created_at) < $4`,
			[]any{guildID, serverID, time.Now().Add(-time.Hour), time.Now()}},
	} {
		rows, err := conn.Query(ctx, "EXPLAIN "+c.sql, c.args...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		if !strings.Contains(strings.Join(plan, "\n"), c.name) {
			t.Fatalf("%s is not used by its query:\n%s", c.name, strings.Join(plan, "\n"))
		}
	}
}

func TestDataRetentionPrunesOnlyOldRowsInBatches(t *testing.T) {
	ctx, db := openHygieneDB(t)
	guilds := NewGuildRepository(db.Pool)
	players := NewPlayerRepository(db.Pool)
	repo := NewDataRetentionRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-retention-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID) })
	killer, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-ret-k-%d", suffix), "Killer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	victim, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-ret-v-%d", suffix), "Victim", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	insert := func(age time.Duration, n int) {
		for i := 0; i < n; i++ {
			if _, err := db.Pool.Exec(ctx, `INSERT INTO combat_anomaly_flags(guild_id,killer_player_id,victim_player_id,flag_type,window_started_at,window_ended_at,created_at)
VALUES($1,$2,$3,'REPEATED_PAIR_KILLS',$4,$4,$4)`, guildID, killer, victim, now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert(20*24*time.Hour, 7) // older than the cutoff
	insert(13*24*time.Hour, 3) // inside the window
	insert(0, 2)
	count := func() int {
		var c int
		if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM combat_anomaly_flags WHERE guild_id=$1`, guildID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	table := RetentionTable{Table: "combat_anomaly_flags", TimeColumn: "created_at"}
	cutoff := now.Add(-14 * 24 * time.Hour)

	n, err := repo.PruneOlderThan(ctx, table, cutoff, 5)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || count() != 7 {
		t.Fatalf("first batch: deleted %d, %d left (want 5 and 7)", n, count())
	}
	// The sweep loops until a batch deletes nothing; the recent rows survive.
	var total int64 = n
	for {
		n, err := repo.PruneOlderThan(ctx, table, cutoff, 5)
		if err != nil {
			t.Fatal(err)
		}
		total += n
		if n == 0 {
			break
		}
	}
	if total != 7 || count() != 5 {
		t.Fatalf("total deleted %d, %d left (want 7 and 5)", total, count())
	}
	var oldest time.Time
	if err := db.Pool.QueryRow(ctx, `SELECT MIN(created_at) FROM combat_anomaly_flags WHERE guild_id=$1`, guildID).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if !oldest.After(cutoff) {
		t.Fatalf("a row older than the cutoff survived: %v <= %v", oldest, cutoff)
	}

	// Identifiers are never interpolated unless they are plain lower-case names.
	for _, bad := range []RetentionTable{{Table: "kills; DROP TABLE kills", TimeColumn: "created_at"}, {Table: "combat_anomaly_flags", TimeColumn: "created_at OR TRUE"}, {Table: "", TimeColumn: "x"}} {
		if _, err := repo.PruneOlderThan(ctx, bad, cutoff, 5); err == nil {
			t.Fatalf("%+v must be refused", bad)
		}
	}
	// An oversized or non-positive batch is clamped, never unbounded.
	if _, err := repo.PruneOlderThan(ctx, table, cutoff, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.PruneOlderThan(ctx, table, cutoff, MaxRetentionBatch*10); err != nil {
		t.Fatal(err)
	}
}
