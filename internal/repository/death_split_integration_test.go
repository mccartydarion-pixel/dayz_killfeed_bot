//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/deathstats"
)

// PvP and PvE deaths (internal/deathstats, docs/DEATH_COUNTS.md) through every query that counts
// deaths for a player: "deaths" keeps its meaning (every row) and the PvP figure is the rows whose
// death_type is PVP.

// otherDeath writes a non-PvP deaths row of the given type on the world's server.
func (w *pvpDeathWorld) otherDeath(player int64, deathType string, at time.Time) {
	w.t.Helper()
	if err := NewDeathRepository(w.db.Pool).InsertDeath(context.Background(), DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fmt.Sprintf("other-%d-%d", w.suffix, pvpDeathSeq.Add(1)), PlayerID: player, DeathType: deathType, EventTime: &at}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *pvpDeathWorld) kill(killer, victim int64, at time.Time) {
	w.t.Helper()
	if _, err := w.kills.InsertKillReturning(context.Background(), w.record(killer, victim, at)); err != nil {
		w.t.Fatal(err)
	}
}

func TestDeathSplitAcrossEveryPlayerQuery(t *testing.T) {
	w := newPvPDeathWorld(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tick := func() time.Time { at = at.Add(time.Minute); return at }

	mixed, pveOnly, pvpOnly, untouched := w.player("Mixed"), w.player("PveOnly"), w.player("PvpOnly"), w.player("Untouched")

	// Mixed: 6 kills; killed by players 2 times; 4 other deaths, one of each kind the table can hold
	// (and one type no code writes today - anything that is not PVP is a PvE death).
	for i := 0; i < 6; i++ {
		w.kill(mixed, pvpOnly, tick())
	}
	w.kill(pvpOnly, mixed, tick())
	w.kill(untouched, mixed, tick())
	w.otherDeath(mixed, DeathTypeUnknown, tick())
	w.otherDeath(mixed, DeathTypeSuicide, tick())
	w.otherDeath(mixed, "INFECTED", tick())
	w.otherDeath(mixed, "ENVIRONMENT", tick())
	// PveOnly: 3 kills, never killed by a player, 2 other deaths.
	for i := 0; i < 3; i++ {
		w.kill(pveOnly, pvpOnly, tick())
	}
	w.otherDeath(pveOnly, DeathTypeUnknown, tick())
	w.otherDeath(pveOnly, DeathTypeSuicide, tick())
	// PvpOnly: 1 kill (above), killed 9 times by players (6 + 3 above), no other death.
	// Untouched: 1 kill (above), no deaths at all.
	// A self-kill is a kill and writes no death of any kind.
	w.kill(untouched, untouched, tick())

	type want struct {
		kills, deaths, pvp, pve int64
		kd, pvpKD               float64
	}
	wants := map[int64]want{
		mixed:     {6, 6, 2, 4, 1, 3},
		pveOnly:   {3, 2, 0, 2, 1.5, 3}, // no PvP deaths: the PvP K/D is the kill count
		pvpOnly:   {1, 9, 9, 0, 1.0 / 9, 1.0 / 9},
		untouched: {2, 0, 0, 0, 2, 2}, // no deaths: both are the kill count
	}
	names := map[int64]string{mixed: "Mixed", pveOnly: "PveOnly", pvpOnly: "PvpOnly", untouched: "Untouched"}

	// The source of truth the figures are checked against: the rows themselves, classified in Go.
	for player, wt := range wants {
		rows, err := w.db.Pool.Query(ctx, `SELECT death_type FROM deaths WHERE guild_id=$1 AND player_id=$2`, w.guildID, player)
		if err != nil {
			t.Fatal(err)
		}
		var pvp, pve int64
		for rows.Next() {
			var typ string
			if err := rows.Scan(&typ); err != nil {
				t.Fatal(err)
			}
			if deathstats.IsPvP(typ) {
				pvp++
			} else {
				pve++
			}
		}
		rows.Close()
		if pvp != wt.pvp || pve != wt.pve {
			t.Fatalf("%s: rows classify as %d PvP / %d PvE, want %d / %d", names[player], pvp, pve, wt.pvp, wt.pve)
		}
	}

	server := NewPlayerServerRepository(w.db.Pool)
	cards := NewCardRepository(w.db.Pool)
	for player, wt := range wants {
		name := names[player]
		// The Discord profile, by id and by name.
		byID, err := w.stats.GetPlayerProfileByPlayerID(ctx, w.guildID, player)
		if err != nil || byID == nil {
			t.Fatalf("%s: profile by id: %v", name, err)
		}
		byName, err := w.stats.GetPlayerProfile(ctx, w.guildID, name)
		if err != nil || byName == nil {
			t.Fatalf("%s: profile by name: %v", name, err)
		}
		for _, prof := range []*PlayerProfile{byID, byName} {
			if prof.Kills != wt.kills || prof.Deaths != wt.deaths || prof.PvPDeaths != wt.pvp || prof.PvEDeaths() != wt.pve || prof.KD() != wt.kd || prof.PvPKD() != wt.pvpKD {
				t.Errorf("%s: profile = %d kills, %d deaths (%d PvP, %d PvE), K/D %v, PvP K/D %v; want %+v",
					name, prof.Kills, prof.Deaths, prof.PvPDeaths, prof.PvEDeaths(), prof.KD(), prof.PvPKD(), wt)
			}
		}
		// The player API.
		cs, err := server.CombatStats(ctx, w.guildID, w.serverID, player)
		if err != nil {
			t.Fatalf("%s: combat stats: %v", name, err)
		}
		if int64(cs.Kills) != wt.kills || int64(cs.Deaths) != wt.deaths || int64(cs.PvPDeaths) != wt.pvp || int64(cs.PvEDeaths()) != wt.pve {
			t.Errorf("%s: combat stats = %+v, want %+v", name, cs, wt)
		}
		// KillStats, the older shape of the same query, still returns every death.
		k, d, _, _, _, err := server.KillStats(ctx, w.guildID, w.serverID, player)
		if err != nil || int64(k) != wt.kills || int64(d) != wt.deaths {
			t.Errorf("%s: KillStats = %d/%d err=%v, want %d/%d", name, k, d, err, wt.kills, wt.deaths)
		}
		// The Champion Card.
		card, err := cards.Stats(ctx, w.guildID, w.serverID, player)
		if err != nil || card == nil {
			t.Fatalf("%s: card stats: %v", name, err)
		}
		if int64(card.Kills) != wt.kills || int64(card.Deaths) != wt.deaths || int64(card.PvPDeaths) != wt.pvp {
			t.Errorf("%s: card = %d kills, %d deaths, %d PvP; want %+v", name, card.Kills, card.Deaths, card.PvPDeaths, wt)
		}
	}

	// The staff player directory.
	dir, err := w.repo.ListPlayerDirectory(ctx, w.guildID, w.serverID, PlayerDirectoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range dir {
		wt, ok := wants[e.PlayerID]
		if !ok {
			continue
		}
		seen++
		if e.Kills != wt.kills || e.Deaths != wt.deaths || e.PvPDeaths != wt.pvp {
			t.Errorf("%s: directory = %d kills, %d deaths, %d PvP; want %+v", e.Gamertag, e.Kills, e.Deaths, e.PvPDeaths, wt)
		}
	}
	if seen != len(wants) {
		t.Fatalf("the directory lists %d of the %d players", seen, len(wants))
	}

	// The K/D board keeps its meaning and order; the PvP K/D board ranks the same players by kills
	// per PvP death.
	board := func(entries []LeaderboardEntry, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, e := range entries {
			out = append(out, e.DisplayName+" "+e.Value)
		}
		return out
	}
	overall := board(w.stats.TopByKD(ctx, w.guildID, 10, 1))
	if got, want := fmt.Sprint(overall), "[Untouched 2.00 PveOnly 1.50 Mixed 1.00 PvpOnly 0.11]"; got != want {
		t.Errorf("K/D board = %s, want %s", got, want)
	}
	pvp := board(w.stats.TopByPvPKD(ctx, w.guildID, 10, 1))
	// Mixed and PveOnly tie at 3.00: more kills first, as on the K/D board.
	if got, want := fmt.Sprint(pvp), "[Mixed 3.00 PveOnly 3.00 Untouched 2.00 PvpOnly 0.11]"; got != want {
		t.Errorf("PvP K/D board = %s, want %s", got, want)
	}
	// The minimum-kills gate applies to both.
	if got := board(w.stats.TopByPvPKD(ctx, w.guildID, 10, 3)); fmt.Sprint(got) != "[Mixed 3.00 PveOnly 3.00]" {
		t.Errorf("PvP K/D board with 3 kills minimum = %v", got)
	}
	if got := board(w.stats.TopByKD(ctx, w.guildID, 10, 3)); fmt.Sprint(got) != "[PveOnly 1.50 Mixed 1.00]" {
		t.Errorf("K/D board with 3 kills minimum = %v", got)
	}
	// The deaths board still counts every death.
	if got := board(w.stats.TopByDeaths(ctx, w.guildID, 10)); fmt.Sprint(got) != "[PvpOnly 9 Mixed 6 PveOnly 2]" {
		t.Errorf("deaths board = %v", got)
	}
}

// A death on another server of the same guild counts in the guild-wide profile and not in the
// server-scoped figures - for the split exactly as for the total.
func TestDeathSplitRespectsServerScope(t *testing.T) {
	w := newPvPDeathWorld(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	player, enemy := w.player("Roamer"), w.player("Enemy")
	var other int64
	if err := w.db.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		w.guildID, fmt.Sprintf("split-svc-%d", w.suffix)).Scan(&other); err != nil {
		t.Fatal(err)
	}
	w.kill(enemy, player, at)                                 // PvP, this server
	w.otherDeath(player, DeathTypeUnknown, at.Add(time.Hour)) // PvE, this server
	elsewhere := w.record(enemy, player, at.Add(2*time.Hour))
	elsewhere.ServerID = other
	if _, err := w.kills.InsertKillReturning(ctx, elsewhere); err != nil { // PvP, the other server
		t.Fatal(err)
	}

	prof, err := w.stats.GetPlayerProfileByPlayerID(ctx, w.guildID, player)
	if err != nil || prof == nil || prof.Deaths != 3 || prof.PvPDeaths != 2 || prof.PvEDeaths() != 1 {
		t.Fatalf("guild-wide profile = %+v err=%v, want 3 deaths (2 PvP, 1 PvE)", prof, err)
	}
	cs, err := NewPlayerServerRepository(w.db.Pool).CombatStats(ctx, w.guildID, w.serverID, player)
	if err != nil || cs.Deaths != 2 || cs.PvPDeaths != 1 || cs.PvEDeaths() != 1 {
		t.Fatalf("this server = %+v err=%v, want 2 deaths (1 PvP, 1 PvE)", cs, err)
	}
	cs, err = NewPlayerServerRepository(w.db.Pool).CombatStats(ctx, w.guildID, other, player)
	if err != nil || cs.Deaths != 1 || cs.PvPDeaths != 1 || cs.PvEDeaths() != 0 {
		t.Fatalf("the other server = %+v err=%v, want 1 PvP death", cs, err)
	}
}
