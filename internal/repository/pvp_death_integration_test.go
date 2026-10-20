//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// A PvP kill is the victim's death (docs/DEATH_COUNTS.md): the kill insert writes the victim's
// PVP deaths row, and the 0081 backfill gives one to every kill that predates it.

var pvpDeathSeq atomic.Int64

type pvpDeathWorld struct {
	*locationWorld
	kills *KillRepository
	stats *StatsRepository
}

func newPvPDeathWorld(t *testing.T) *pvpDeathWorld {
	w := newLocationWorld(t)
	return &pvpDeathWorld{locationWorld: w, kills: NewKillRepository(w.db.Pool), stats: NewStatsRepository(w.db.Pool)}
}

func (w *pvpDeathWorld) player(name string) int64 {
	w.t.Helper()
	var id int64
	if err := w.db.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		w.guildID, fmt.Sprintf("pvp-%d-%d", w.suffix, pvpDeathSeq.Add(1)), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *pvpDeathWorld) record(killer, victim int64, at time.Time) KillRecord {
	return KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("fp-%d-%d", w.suffix, pvpDeathSeq.Add(1)),
		KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "M4-A1", WeaponDisplay: "M4-A1", EventTime: &at}
}

func (w *pvpDeathWorld) deathRows(player int64) (total, pvp int) {
	w.t.Helper()
	if err := w.db.Pool.QueryRow(context.Background(), `SELECT COUNT(*), COUNT(*) FILTER (WHERE death_type='PVP') FROM deaths WHERE guild_id=$1 AND player_id=$2`,
		w.guildID, player).Scan(&total, &pvp); err != nil {
		w.t.Fatal(err)
	}
	return
}

func TestInsertKillWritesTheVictimsDeath(t *testing.T) {
	w := newPvPDeathWorld(t)
	ctx := context.Background()
	killer, victim := w.player("Killer"), w.player("Victim")
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	rec := w.record(killer, victim, at)
	rec.SourceFile, rec.SourceOffset = "DayZServer_x.ADM", 4096
	id, err := w.kills.InsertKillReturning(ctx, rec)
	if err != nil || id == 0 {
		t.Fatalf("insert kill: id=%d err=%v", id, err)
	}
	var fp, deathType, sourceFile string
	var serverID, offset int64
	var eventTime time.Time
	if err := w.db.Pool.QueryRow(ctx, `SELECT event_fingerprint, death_type, server_id, event_time, source_file, source_offset FROM deaths WHERE guild_id=$1 AND player_id=$2`,
		w.guildID, victim).Scan(&fp, &deathType, &serverID, &eventTime, &sourceFile, &offset); err != nil {
		t.Fatalf("the victim has no death row: %v", err)
	}
	if fp != PvPDeathFingerprintPrefix+rec.Fingerprint || deathType != DeathTypePVP || serverID != w.serverID || !eventTime.Equal(at) || sourceFile != "DayZServer_x.ADM" || offset != 4096 {
		t.Fatalf("death row = %q %q server=%d at=%s source=%s@%d", fp, deathType, serverID, eventTime, sourceFile, offset)
	}
	if total, _ := w.deathRows(killer); total != 0 {
		t.Fatalf("the killer got %d death rows", total)
	}

	// A replayed kill is rejected whole: no second kill, no second death.
	if _, err := w.kills.InsertKillReturning(ctx, rec); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("replayed kill: %v", err)
	}
	if total, pvp := w.deathRows(victim); total != 1 || pvp != 1 {
		t.Fatalf("after a replay the victim has %d death rows (%d PVP)", total, pvp)
	}

	// A self-kill is not a PvP death; a kill with no resolved victim writes no death.
	if _, err := w.kills.InsertKillReturning(ctx, w.record(killer, killer, at.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.kills.InsertKillReturning(ctx, w.record(killer, 0, at.Add(2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if total, _ := w.deathRows(killer); total != 0 {
		t.Fatalf("a self-kill wrote %d death rows", total)
	}

	// The figures players see: the victim's death counts, and so does a later non-PvP death.
	if err := NewDeathRepository(w.db.Pool).InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: "bleedout-" + rec.Fingerprint,
		PlayerID: victim, DeathType: DeathTypeUnknown, EventTime: ptrTime(at.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	prof, err := w.stats.GetPlayerProfileByPlayerID(ctx, w.guildID, victim)
	if err != nil || prof == nil || prof.Kills != 0 || prof.Deaths != 2 {
		t.Fatalf("victim profile = %+v err=%v, want 0 kills and 2 deaths", prof, err)
	}
	_, deaths, _, _, _, err := NewPlayerServerRepository(w.db.Pool).KillStats(ctx, w.guildID, w.serverID, victim)
	if err != nil || deaths != 2 {
		t.Fatalf("player API deaths = %d err=%v, want 2", deaths, err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestPvPDeathBackfillCoversOldKillsOnce(t *testing.T) {
	w := newPvPDeathWorld(t)
	ctx := context.Background()
	killer, victim, other := w.player("Killer"), w.player("Victim"), w.player("Other")
	at := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	// Kills as they exist before 0081: rows in kills, nothing in deaths.
	old := func(killerID, victimID any, when time.Time) string {
		fp := fmt.Sprintf("old-%d-%d", w.suffix, pvpDeathSeq.Add(1))
		if _, err := w.db.Pool.Exec(ctx, `INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, event_time, created_at)
VALUES($1,$2,'s',$3,$4,$5,$6,$6)`, w.guildID, w.serverID, fp, killerID, victimID, when); err != nil {
			t.Fatal(err)
		}
		return fp
	}
	first := old(killer, victim, at)
	old(killer, victim, at.Add(time.Hour))
	old(victim, victim, at.Add(2*time.Hour)) // self-kill
	old(killer, nil, at.Add(3*time.Hour))    // unresolved victim
	// A kill whose victim already has a death row two seconds later: the same death, not a second one.
	old(killer, other, at.Add(4*time.Hour))
	if err := NewDeathRepository(w.db.Pool).InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("died-%d", w.suffix),
		PlayerID: other, DeathType: DeathTypeUnknown, EventTime: ptrTime(at.Add(4*time.Hour + 2*time.Second))}); err != nil {
		t.Fatal(err)
	}

	for run := 1; run <= 2; run++ { // idempotent
		if _, err := w.db.Pool.Exec(ctx, database.PvPDeathBackfillSQL); err != nil {
			t.Fatal(err)
		}
		if total, pvp := w.deathRows(victim); total != 2 || pvp != 2 {
			t.Fatalf("run %d: victim has %d death rows (%d PVP), want 2", run, total, pvp)
		}
		if total, pvp := w.deathRows(other); total != 1 || pvp != 0 {
			t.Fatalf("run %d: a death already on record was counted again: %d rows (%d PVP)", run, total, pvp)
		}
		if total, _ := w.deathRows(killer); total != 0 {
			t.Fatalf("run %d: the killer got %d death rows", run, total)
		}
	}
	var eventTime, createdAt time.Time
	if err := w.db.Pool.QueryRow(ctx, `SELECT event_time, created_at FROM deaths WHERE guild_id=$1 AND event_fingerprint=$2`, w.guildID, PvPDeathFingerprintPrefix+first).
		Scan(&eventTime, &createdAt); err != nil || !eventTime.Equal(at) || !createdAt.Equal(at) {
		t.Fatalf("backfilled row at=%s created=%s err=%v, want the kill's own time", eventTime, createdAt, err)
	}
	// A kill persisted after the backfill uses the same fingerprint scheme, so re-running the
	// backfill never doubles it.
	rec := w.record(killer, victim, at.Add(10*time.Hour))
	if _, err := w.kills.InsertKillReturning(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := w.db.Pool.Exec(ctx, database.PvPDeathBackfillSQL); err != nil {
		t.Fatal(err)
	}
	if total, _ := w.deathRows(victim); total != 3 {
		t.Fatalf("victim has %d death rows after a live kill and a re-run, want 3", total)
	}
}

func TestDeathHeatmapIncludesPvPDeathsAtTheVictimsPosition(t *testing.T) {
	w := newPvPDeathWorld(t)
	ctx := context.Background()
	killer, victim := w.player("Killer"), w.player("Victim")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	locate := func(player int64, x, z float64, eventType string, when time.Time) {
		if _, err := w.db.Pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at) VALUES($1,$2,$3,'g',$4,$5,$6,$7)`,
			w.guildID, w.serverID, player, x, z, eventType, when); err != nil {
			t.Fatal(err)
		}
	}
	// One PvP death (the kill line carries both positions as KILL rows) and one bleed-out.
	if _, err := w.kills.InsertKillReturning(ctx, w.record(killer, victim, at)); err != nil {
		t.Fatal(err)
	}
	locate(killer, 1100, 1100, "KILL", at)
	locate(victim, 5100, 5100, "KILL", at)
	later := at.Add(10 * time.Minute)
	if err := NewDeathRepository(w.db.Pool).InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fmt.Sprintf("hm-died-%d", w.suffix),
		PlayerID: killer, DeathType: DeathTypeUnknown, EventTime: &later}); err != nil {
		t.Fatal(err)
	}
	locate(killer, 9100, 9100, "DEATH", later)

	cells, err := NewHeatmapRepository(w.db.Pool).AggregateDeaths(ctx, w.guildID, w.serverID, at.Add(-time.Hour), at.Add(time.Hour), 1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[[2]int64]int64{}
	for _, c := range cells {
		got[[2]int64{c.CellX, c.CellZ}] = c.Count
	}
	// The PvP death is where the victim fell (5, 5) - not where the killer stood (1, 1).
	if len(got) != 2 || got[[2]int64{5, 5}] != 1 || got[[2]int64{9, 9}] != 1 {
		t.Fatalf("death heatmap cells = %v", got)
	}
}
