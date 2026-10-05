//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The interactive read paths were rewritten for speed (docs: internal/database/
// interactive_reads_schema.go). These tests run the query text each one replaced next to the new
// method on the same seeded rows and require identical results, the pattern
// TestTopByKDMatchesLegacySemantics established.

const legacyTopByKills = `
SELECT p.display_name, COUNT(k.id) AS kills
FROM players p
JOIN kills k ON k.killer_player_id=p.id AND k.guild_id=p.guild_id
WHERE p.guild_id=$1
GROUP BY p.id, p.display_name
ORDER BY kills DESC, p.display_name ASC, p.id ASC
LIMIT $2`

const legacyTopByDeaths = `
SELECT p.display_name, COUNT(d.id) AS death_count
FROM players p
JOIN deaths d ON d.player_id=p.id AND d.guild_id=p.guild_id
WHERE p.guild_id=$1
GROUP BY p.id, p.display_name
ORDER BY death_count DESC, p.display_name ASC, p.id ASC
LIMIT $2`

const legacyKillStats = `
SELECT
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3),
  (SELECT COUNT(*) FROM deaths WHERE guild_id=$1 AND server_id=$2 AND player_id=$3),
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND headshot),
  (SELECT COUNT(*) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3 AND longshot),
  (SELECT COALESCE(MAX(distance),0) FROM kills WHERE guild_id=$1 AND server_id=$2 AND killer_player_id=$3)`

// legacyDirectoryCounts is the part of the old directory query that produced the three counts:
// every kill, death and open warning of the server grouped by player, joined to the player list.
const legacyDirectoryCounts = `
SELECT p.id, COALESCE(k.n,0), COALESCE(d.n,0), COALESCE(w.n,0)
FROM players p
LEFT JOIN (SELECT killer_player_id, COUNT(*) n FROM kills WHERE guild_id=$1 AND server_id=$2 GROUP BY killer_player_id) k ON k.killer_player_id=p.id
LEFT JOIN (SELECT player_id, COUNT(*) n FROM deaths WHERE guild_id=$1 AND server_id=$2 GROUP BY player_id) d ON d.player_id=p.id
LEFT JOIN (SELECT player_id, COUNT(*) n FROM player_warnings WHERE guild_id=$1 AND cleared=false GROUP BY player_id) w ON w.player_id=p.id
WHERE p.guild_id=$1`

// legacyLatestLocation is the statement LatestLocation ran before it shared LatestLocations.
const legacyLatestLocation = `
SELECT ` + locationEventCols + ` FROM player_location_events e
WHERE guild_id=$1 AND server_id=$2 AND player_id=$3
ORDER BY observed_at DESC, id DESC LIMIT 1`

// interactiveWorld is one guild with two servers, forty players and a spread of kills, deaths,
// warnings and location rows, plus a second guild whose rows must never leak in.
type interactiveWorld struct {
	guildID, otherGuildID int64
	serverA, serverB      int64
	players               []int64
	outsider              int64 // a player of the other guild
}

func seedInteractiveWorld(t *testing.T, w *locationWorld) interactiveWorld {
	t.Helper()
	ctx := context.Background()
	pool := w.db.Pool
	iw := interactiveWorld{guildID: w.guildID, serverA: w.serverID}
	must := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	if err := pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		w.guildID, fmt.Sprintf("interactive-b-%d", w.suffix)).Scan(&iw.serverB); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("interactive-other-%d", w.suffix)).Scan(&iw.otherGuildID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, iw.otherGuildID) })
	if err := pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,'Outsider') RETURNING id`,
		iw.otherGuildID, fmt.Sprintf("outsider-%d", w.suffix)).Scan(&iw.outsider); err != nil {
		t.Fatal(err)
	}
	// Forty players; several share a display name so the name and id tie breakers both matter.
	rows, err := pool.Query(ctx, `
INSERT INTO players(guild_id, dayz_player_id, display_name)
SELECT $1, 'interactive-' || $2::text || '-' || g, 'Player ' || (g % 30) FROM generate_series(1, 40) g
RETURNING id`, w.guildID, fmt.Sprint(w.suffix))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		iw.players = append(iw.players, id)
	}
	rows.Close()
	if len(iw.players) != 40 {
		t.Fatalf("seeded %d players", len(iw.players))
	}
	base := iw.players[0]
	// 600 kills over both servers: skewed killers (so there are ties and players with none),
	// some headshots and longshots, some without a distance, every 25th without a killer.
	must(`
INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, distance, headshot, longshot, created_at)
SELECT $1, CASE WHEN g % 3 = 0 THEN $3::bigint ELSE $2::bigint END, 'interactive', 'interactive-' || $4::text || '-' || g,
       CASE WHEN g % 25 = 0 THEN NULL ELSE $5::bigint + ((g * g) % 23) END,
       $5::bigint + ((g * 7) % 40),
       CASE WHEN g % 6 = 0 THEN NULL ELSE (g * 37 % 800)::float8 END, g % 4 = 0, g % 9 = 0, NOW() - (g || ' minutes')::interval
FROM generate_series(1, 600) g`, w.guildID, iw.serverA, iw.serverB, fmt.Sprint(w.suffix), base)
	must(`
INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, created_at)
SELECT $1, CASE WHEN g % 4 = 0 THEN $3::bigint ELSE $2::bigint END, 'interactive', 'interactive-death-' || $4::text || '-' || g,
       $5::bigint + ((g * 11) % 31), 'PVP', NOW() - (g || ' minutes')::interval
FROM generate_series(1, 500) g`, w.guildID, iw.serverA, iw.serverB, fmt.Sprint(w.suffix), base)
	// A kill in this guild credited to a player of another guild, and rows of the other guild:
	// neither may be counted.
	must(`INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id) VALUES($1,$2,'interactive',$3,$4,$5)`,
		w.guildID, iw.serverA, fmt.Sprintf("interactive-foreign-%d", w.suffix), iw.outsider, base)
	must(`INSERT INTO kills(guild_id, session_id, event_fingerprint, killer_player_id, victim_player_id) VALUES($1,'interactive',$2,$3,$3)`,
		iw.otherGuildID, fmt.Sprintf("interactive-other-%d", w.suffix), iw.outsider)
	must(`INSERT INTO deaths(guild_id, session_id, event_fingerprint, player_id, death_type) VALUES($1,'interactive',$2,$3,'PVP')`,
		iw.otherGuildID, fmt.Sprintf("interactive-other-death-%d", w.suffix), iw.outsider)
	// Warnings: open and cleared.
	must(`INSERT INTO player_warnings(guild_id, player_id, reason, cleared) SELECT $1, $2::bigint + (g % 7), 'test', g % 3 = 0 FROM generate_series(1, 20) g`, w.guildID, base)
	return iw
}

func TestTopByKillsAndDeathsMatchLegacySemantics(t *testing.T) {
	w := newLocationWorld(t)
	iw := seedInteractiveWorld(t, w)
	ctx := context.Background()
	stats := NewStatsRepository(w.db.Pool)

	legacy := func(q string, limit int) []LeaderboardEntry {
		t.Helper()
		rows, err := w.db.Pool.Query(ctx, q, iw.guildID, limit)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []LeaderboardEntry
		for rows.Next() {
			var e LeaderboardEntry
			var n int64
			if err := rows.Scan(&e.DisplayName, &n); err != nil {
				t.Fatal(err)
			}
			e.Value = fmt.Sprintf("%d", n)
			out = append(out, e)
		}
		return out
	}
	for _, limit := range []int{1, 3, 10, 25, 100} {
		got, err := stats.TopByKills(ctx, iw.guildID, limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := legacy(legacyTopByKills, limit); !reflect.DeepEqual(got, want) {
			t.Fatalf("TopByKills limit %d:\n got  %v\n want %v", limit, got, want)
		}
		got, err = stats.TopByDeaths(ctx, iw.guildID, limit)
		if err != nil {
			t.Fatal(err)
		}
		if want := legacy(legacyTopByDeaths, limit); !reflect.DeepEqual(got, want) {
			t.Fatalf("TopByDeaths limit %d:\n got  %v\n want %v", limit, got, want)
		}
	}
	if top, _ := stats.TopByKills(ctx, iw.guildID, 100); len(top) < 10 || len(top) > 23 {
		t.Fatalf("the seed should rank between 10 and 23 killers, got %d", len(top))
	}
	// A guild with no kills has an empty board, not an error.
	if got, err := stats.TopByKills(ctx, iw.otherGuildID+1_000_000, 10); err != nil || len(got) != 0 {
		t.Fatalf("empty guild: %v %v", got, err)
	}
}

func TestKillStatsMatchesLegacySemantics(t *testing.T) {
	w := newLocationWorld(t)
	iw := seedInteractiveWorld(t, w)
	ctx := context.Background()
	repo := NewPlayerServerRepository(w.db.Pool)
	checked, withKills := 0, 0
	for _, serverID := range []int64{iw.serverA, iw.serverB} {
		for _, playerID := range append(append([]int64{}, iw.players...), iw.outsider, iw.players[39]+1_000_000) {
			var wk, wd, wh, wl int
			var wLongest float64
			if err := w.db.Pool.QueryRow(ctx, legacyKillStats, iw.guildID, serverID, playerID).Scan(&wk, &wd, &wh, &wl, &wLongest); err != nil {
				t.Fatal(err)
			}
			k, d, h, l, longest, err := repo.KillStats(ctx, iw.guildID, serverID, playerID)
			if err != nil {
				t.Fatal(err)
			}
			if k != wk || d != wd || h != wh || l != wl || longest != wLongest {
				t.Fatalf("KillStats(server %d, player %d) = %d/%d/%d/%d/%.1f, legacy %d/%d/%d/%d/%.1f", serverID, playerID, k, d, h, l, longest, wk, wd, wh, wl, wLongest)
			}
			checked++
			if k > 0 {
				withKills++
			}
		}
	}
	if checked != 84 || withKills < 20 {
		t.Fatalf("checked %d players, %d with kills: the seed is not exercising the query", checked, withKills)
	}
}

func TestPlayerDirectoryCountsAndLocationsMatchLegacy(t *testing.T) {
	w := newLocationWorld(t)
	iw := seedInteractiveWorld(t, w)
	ctx := context.Background()

	// Locations: an open boot session, some players connected with rows in it, some with rows
	// only in an older boot, some with none.
	const oldBoot, boot = "DayZServer_PS4_x64_2026-09-23_04-00-00.ADM", "DayZServer_PS4_x64_2026-09-24_04-00-00.ADM"
	if err := w.repo.SetCurrentADMSession(ctx, w.guildID, w.serverID, boot, nil); err != nil {
		t.Fatal(err)
	}
	for n, p := range iw.players {
		switch n % 4 {
		case 0: // connected, positions in the current boot after the connect
			w.connect(p, time.Now().UTC())
			w.observe(p, boot, int64(1000+n*10), "CONNECT", float64(n), 10, float64(n))
			w.observe(p, boot, int64(1000+n*10+5), "PLAYER_LIST", float64(100+n), 10, float64(100+n))
			// Two rows observed at the same instant: the higher id is the newest.
			if _, err := w.db.Pool.Exec(ctx, `
INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at,source_file,source_offset)
SELECT guild_id,server_id,player_id,gamertag,x+1,z+1,'HIT',observed_at,source_file,source_offset+1 FROM player_location_events
WHERE server_id=$1 AND player_id=$2 ORDER BY observed_at DESC, id DESC LIMIT 1`, w.serverID, p); err != nil {
				t.Fatal(err)
			}
			// And a newer row on the guild's other server, which must not be returned for this one.
			if _, err := w.db.Pool.Exec(ctx, `
INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at)
VALUES($1,$2,$3,'x',9000,9000,'PLAYER_LIST',NOW() + INTERVAL '1 hour')`, w.guildID, iw.serverB, p); err != nil {
				t.Fatal(err)
			}
		case 1: // seen only in the previous boot, not connected now
			w.observe(p, oldBoot, int64(500+n), "PLAYER_LIST", float64(200+n), 10, float64(200+n))
		case 2: // connected, but only an old-boot position
			w.connect(p, time.Now().UTC())
			w.observe(p, oldBoot, int64(700+n), "HIT", float64(300+n), 10, float64(300+n))
		}
	}

	// Every page size and filter returns the legacy counts.
	want := map[int64][3]int64{}
	rows, err := w.db.Pool.Query(ctx, legacyDirectoryCounts, iw.guildID, iw.serverA)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var c [3]int64
		if err := rows.Scan(&id, &c[0], &c[1], &c[2]); err != nil {
			t.Fatal(err)
		}
		want[id] = c
	}
	rows.Close()
	online := true
	seen := map[int64]bool{}
	nonZero := 0
	for _, filter := range []PlayerDirectoryFilter{{Limit: 200}, {Limit: 7}, {Limit: 200, Online: &online}, {Limit: 200, Query: "Player 1"}} {
		list, err := w.repo.ListPlayerDirectory(ctx, iw.guildID, iw.serverA, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) == 0 {
			t.Fatalf("filter %+v returned nothing", filter)
		}
		for idx, e := range list {
			if idx > 0 && list[idx-1].PlayerID <= e.PlayerID {
				t.Fatalf("directory is not newest-id first: %d then %d", list[idx-1].PlayerID, e.PlayerID)
			}
			c := want[e.PlayerID]
			if e.Kills != c[0] || e.Deaths != c[1] || e.WarningCount != c[2] {
				t.Fatalf("player %d: kills/deaths/warnings %d/%d/%d, legacy %d/%d/%d", e.PlayerID, e.Kills, e.Deaths, e.WarningCount, c[0], c[1], c[2])
			}
			if e.Kills > 0 || e.Deaths > 0 || e.WarningCount > 0 {
				nonZero++
			}
			seen[e.PlayerID] = true
			// The batched locations are exactly what the per-player lookups return.
			cur, err := w.repo.CurrentLocation(ctx, iw.guildID, iw.serverA, e.PlayerID)
			if err != nil {
				t.Fatal(err)
			}
			var last *LocationEvent
			if legacy, err := scanLocationEvent(w.db.Pool.QueryRow(ctx, legacyLatestLocation, iw.guildID, iw.serverA, e.PlayerID)); err == nil {
				last = &legacy
			} else if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal(err)
			}
			if single, err := w.repo.LatestLocation(ctx, iw.guildID, iw.serverA, e.PlayerID); err != nil || !reflect.DeepEqual(single, last) {
				t.Fatalf("player %d LatestLocation: %+v (%v), legacy %+v", e.PlayerID, single, err, last)
			}
			if !reflect.DeepEqual(e.CurrentLocation, cur) {
				t.Fatalf("player %d current location: batched %+v, single %+v", e.PlayerID, e.CurrentLocation, cur)
			}
			if !reflect.DeepEqual(e.LastKnownLocation, last) {
				t.Fatalf("player %d last known location: batched %+v, single %+v", e.PlayerID, e.LastKnownLocation, last)
			}
		}
	}
	if len(seen) != 40 || nonZero < 40 {
		t.Fatalf("directory covered %d players, %d non-zero count rows", len(seen), nonZero)
	}

	// The batch lookups by themselves: known, unknown and foreign players, and an empty list.
	ids := append(append([]int64{}, iw.players...), iw.outsider, iw.players[39]+1_000_000)
	current, err := w.repo.CurrentLocations(ctx, iw.guildID, iw.serverA, ids)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := w.repo.LatestLocations(ctx, iw.guildID, iw.serverA, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 10 || len(latest) != 30 {
		t.Fatalf("batch lookups: %d current (want 10), %d latest (want 30)", len(current), len(latest))
	}
	for id, loc := range current {
		if !loc.CurrentSession || loc.SourceFile != boot || loc.PlayerID != id || loc.EventType != "HIT" {
			t.Fatalf("current location of %d: %+v", id, loc)
		}
	}
	if empty, err := w.repo.LatestLocations(ctx, iw.guildID, iw.serverA, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty batch: %v %v", empty, err)
	}
	// The guild's other server sees only its own rows, and a mismatched guild sees nothing.
	other, err := w.repo.LatestLocations(ctx, iw.guildID, iw.serverB, ids)
	if err != nil || len(other) != 10 {
		t.Fatalf("other server: %d rows (%v), want its own 10", len(other), err)
	}
	for _, loc := range other {
		if loc.X != 9000 {
			t.Fatalf("a position leaked across servers: %+v", loc)
		}
	}
	if foreign, err := w.repo.LatestLocations(ctx, iw.otherGuildID, iw.serverA, ids); err != nil || len(foreign) != 0 {
		t.Fatalf("positions leaked across guilds: %v %v", foreign, err)
	}
}

// Migration 0126: the partial index exists, holds only connected rows, and serves the
// "who is online on this server" predicate as the repositories write it.
func TestConnectedPlayersIndexServesOnlineReads(t *testing.T) {
	w := newLocationWorld(t)
	ctx := context.Background()
	var def string
	if err := w.db.Pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname='idx_player_server_activity_connected'`).Scan(&def); err != nil {
		t.Fatalf("index missing: %v", err)
	}
	if !strings.Contains(def, "(guild_id, server_id)") || !strings.Contains(def, "WHERE currently_connected") {
		t.Fatalf("unexpected definition: %s", def)
	}
	if _, err := w.db.Pool.Exec(ctx, `
WITH p AS (INSERT INTO players(guild_id, dayz_player_id, display_name)
           SELECT $1, 'connected-idx-' || $3::text || '-' || g, 'P' || g FROM generate_series(1, 3000) g RETURNING id)
INSERT INTO player_server_activity(guild_id, server_id, player_id, currently_connected)
SELECT $1, $2, id, id % 100 = 0 FROM p`, w.guildID, w.serverID, fmt.Sprint(w.suffix)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Pool.Exec(ctx, `ANALYZE player_server_activity`); err != nil {
		t.Fatal(err)
	}
	rows, err := w.db.Pool.Query(ctx, `EXPLAIN SELECT COUNT(*) FROM player_server_activity a WHERE a.guild_id=$1 AND a.server_id=$2 AND a.currently_connected`, w.guildID, w.serverID)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "idx_player_server_activity_connected") {
		t.Fatalf("the online count does not use the partial index:\n%s", plan.String())
	}
	var n int
	if err := w.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM player_server_activity a WHERE a.guild_id=$1 AND a.server_id=$2 AND a.currently_connected`, w.guildID, w.serverID).Scan(&n); err != nil || n != 30 {
		t.Fatalf("online count = %d (%v), want 30", n, err)
	}
}
