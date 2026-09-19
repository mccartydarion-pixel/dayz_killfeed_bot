//go:build integration

package factionstats

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestStatsQueriesScaleWithTheFactionNotTheGuild loads a guild-sized history (kills, deaths and
// claimed bounties of thousands of OTHER players on the same server) and checks that one faction's
// figures, activity page and achievement evaluation stay fast: the queries are driven by the
// faction's few linked players through the existing kill/death/bounty indexes, never by a scan of
// the guild's history and never one query per member. PERF_KILLS (default 20000) scales it up for
// manual measurements.
func TestStatsQueriesScaleWithTheFactionNotTheGuild(t *testing.T) {
	kills := 20000
	if v, err := strconv.Atoi(os.Getenv("PERF_KILLS")); err == nil && v > 0 {
		kills = v
	}
	w := newWorld(t)
	// 3000 unrelated players plus the faction's 10 linked members.
	w.exec(`INSERT INTO players(guild_id, dayz_player_id, display_name) SELECT $1::bigint, 'perf-' || $2::bigint::text || '-' || g, 'Bulk ' || g FROM generate_series(1,3000) g`, w.guild1, w.suffix)
	lead, leadP := w.linked(w.guild1, "PerfLead")
	f := w.faction(w.inst1, lead, "Perf Faction", "PF")
	w.periods(f, lead, span{From: w.day(0)})
	members := []int64{leadP}
	for i := 0; i < 9; i++ {
		u, p := w.linked(w.guild1, "PerfMember")
		w.join(f, lead, u)
		w.periods(f, u, span{From: w.day(0)})
		members = append(members, p)
	}
	victim := w.outsider(w.guild1, "PerfVictim")

	start := time.Now()
	w.exec(`
WITH pl AS (SELECT array_agg(id ORDER BY id) AS ids FROM players WHERE guild_id=$1 AND dayz_player_id LIKE 'perf-' || $2::bigint::text || '-%')
INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, longshot, distance, event_time)
SELECT $1::bigint,$3::bigint,'perf','pk-' || $2::bigint::text || '-' || g,
       (SELECT ids[1 + (g * 7919) % 3000] FROM pl), (SELECT ids[1 + (g * 104729) % 3000] FROM pl),
       (g % 5 = 0), (g % 11 = 0), 40, $4::timestamptz + (g * interval '30 seconds')
FROM generate_series(1,$5::bigint) g`, w.guild1, w.suffix, w.server1a, w.day(0), kills)
	w.exec(`
WITH pl AS (SELECT array_agg(id ORDER BY id) AS ids FROM players WHERE guild_id=$1 AND dayz_player_id LIKE 'perf-' || $2::bigint::text || '-%')
INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, event_time)
SELECT $1::bigint,$3::bigint,'perf','pd-' || $2::bigint::text || '-' || g, (SELECT ids[1 + (g * 3571) % 3000] FROM pl), 'UNKNOWN', $4::timestamptz + (g * interval '45 seconds')
FROM generate_series(1,$5::bigint) g`, w.guild1, w.suffix, w.server1a, w.day(0), kills/2)
	w.exec(`INSERT INTO bounties(guild_id, server_id, target_player_id, created_by_type, status, reward_points, starts_at, claimed_by_player_id, claimed_kill_id, claimed_at)
SELECT $1::bigint,$2::bigint,$3::bigint,'MANUAL','CLAIMED',10,$4::timestamptz, k.killer_player_id, k.id, $4::timestamptz FROM (SELECT id, killer_player_id FROM kills WHERE guild_id=$1 AND event_fingerprint LIKE 'pk-' || $5::bigint::text || '-%' ORDER BY id LIMIT $6::int) k`,
		w.guild1, w.server1a, victim, w.day(1), w.suffix, kills/5)
	// The faction's own combat: each member gets a few hundred kills, some deaths and bounties.
	for i, p := range members {
		w.exec(`INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, longshot, distance, event_time)
SELECT $1::bigint,$2::bigint,'perf','fm-' || $3::bigint::text || '-' || $4::int::text || '-' || g,$5::bigint,$6::bigint,(g % 4 = 0),(g % 9 = 0),60,$7::timestamptz + (g * interval '20 minutes') FROM generate_series(1,300) g`,
			w.guild1, w.server1a, w.suffix, i, p, victim, w.day(1))
		w.exec(`INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, event_time)
SELECT $1::bigint,$2::bigint,'perf','fd-' || $3::bigint::text || '-' || $4::int::text || '-' || g,$5::bigint,'UNKNOWN',$6::timestamptz + (g * interval '3 hours') FROM generate_series(1,40) g`, w.guild1, w.server1a, w.suffix, i, p, w.day(1))
	}
	t.Logf("loaded %d guild kills, %d deaths, %d claimed bounties in %v", kills, kills/2, kills/5, time.Since(start).Round(time.Millisecond))
	w.exec(`ANALYZE kills`)
	w.exec(`ANALYZE deaths`)
	w.exec(`ANALYZE bounties`)

	timeIt := func(name string, fn func()) time.Duration {
		fn() // warm
		s := time.Now()
		fn()
		d := time.Since(s)
		t.Logf("%-28s %v", name, d.Round(time.Millisecond))
		return d
	}
	sc, err := w.store.Scope(w.ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	d1 := timeIt("ComputeStats (10 members)", func() {
		if _, err := w.store.ComputeStats(w.ctx, sc); err != nil {
			t.Fatal(err)
		}
	})
	d2 := timeIt("Activity page (20)", func() {
		if _, _, err := w.store.Activity(w.ctx, sc, 20, nil); err != nil {
			t.Fatal(err)
		}
	})
	d3 := timeIt("Evaluate achievements", func() {
		if _, err := w.svc.Evaluate(w.ctx, sc); err != nil {
			t.Fatal(err)
		}
	})
	st := w.fresh(f)
	if st.Summary.Kills != 3000 || st.Summary.Deaths != 400 {
		t.Fatalf("the faction's own figures must be exact and unaffected by the guild's history: %+v", st.Summary)
	}
	for name, d := range map[string]time.Duration{"stats": d1, "activity": d2, "evaluate": d3} {
		if d > 5*time.Second {
			t.Errorf("%s took %v: it must scale with the faction, not the guild", name, d)
		}
	}
}
