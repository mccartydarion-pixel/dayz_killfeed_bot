//go:build integration

package factionstats

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The PvP/PvE death split on real faction data: the profile and the leaderboard read it from the
// same attribution query, so they agree, and "deaths" still counts every death.
func TestFactionDeathSplitFromRealData(t *testing.T) {
	w := newWorld(t)
	enemy := w.outsider(w.guild1, "Enemy")
	u, p := w.linked(w.guild1, "Splitter")
	f := w.faction(w.inst1, u, "Splitters", "SPL")
	w.periods(f, u, span{From: w.day(0)})

	for i := 1; i <= 12; i++ {
		w.kill(w.guild1, w.server1a, p, enemy, w.day(2).Add(time.Duration(i)*time.Minute), killOpt{distance: 50})
	}
	// 3 deaths to players, 5 to everything else (one of each kind, and a type no code writes today).
	for i := 0; i < 3; i++ {
		w.death(w.guild1, w.server1a, p, w.day(3).Add(time.Duration(i)*time.Minute), repository.DeathTypePVP)
	}
	for i, typ := range []string{repository.DeathTypeUnknown, repository.DeathTypeUnknown, repository.DeathTypeSuicide, "INFECTED", "ANIMAL"} {
		w.death(w.guild1, w.server1a, p, w.day(4).Add(time.Duration(i)*time.Minute), typ)
	}
	// Before the membership began: counted nowhere, PvP or not.
	w.death(w.guild1, w.server1a, p, w.day(-1), repository.DeathTypePVP)

	st := w.fresh(f)
	s := st.Summary
	if s.Kills != 12 || s.Deaths != 8 || s.PvPDeaths != 3 || s.PvEDeaths != 5 || s.KDRatio != 1.5 || s.PvPKDRatio != 4 {
		t.Fatalf("summary = %+v", s)
	}
	var member *MemberContribution
	for i := range st.MemberContributions {
		if st.MemberContributions[i].DisplayName != "" && st.MemberContributions[i].Kills == 12 {
			member = &st.MemberContributions[i]
		}
	}
	if member == nil || member.Deaths != 8 || member.PvPDeaths != 3 || member.PvEDeaths != 5 || member.KDRatio != 1.5 || member.PvPKDRatio != 4 {
		t.Fatalf("member = %+v", member)
	}

	// A faction that only ever died to the world: its PvP K/D is its kill count.
	u2, p2 := w.linked(w.guild1, "Hermit")
	f2 := w.faction(w.inst1, u2, "Hermits", "HMT")
	w.periods(f2, u2, span{From: w.day(0)})
	for i := 1; i <= 4; i++ {
		w.kill(w.guild1, w.server1a, p2, enemy, w.day(2).Add(time.Duration(i)*time.Hour), killOpt{})
	}
	w.death(w.guild1, w.server1a, p2, w.day(5), repository.DeathTypeUnknown)
	w.death(w.guild1, w.server1a, p2, w.day(6), repository.DeathTypeSuicide)

	board := w.freshBoard(w.org1, w.inst1, LeaderboardKD)
	e := entryFor(t, board, f.ID)
	if e.Stats.Deaths != s.Deaths || e.Stats.PvPDeaths != s.PvPDeaths || e.Stats.PvEDeaths != s.PvEDeaths || e.Stats.KDRatio != s.KDRatio || e.Stats.PvPKDRatio != s.PvPKDRatio {
		t.Fatalf("leaderboard %+v differs from the profile %+v", e.Stats, s)
	}
	// The K/D board is still ranked by the overall K/D (its value), whatever the PvP K/D is.
	if e.Value != 1.5 {
		t.Fatalf("K/D board value = %v, want the overall 1.5", e.Value)
	}
	h := entryFor(t, board, f2.ID)
	if h.Stats.Kills != 4 || h.Stats.Deaths != 2 || h.Stats.PvPDeaths != 0 || h.Stats.PvEDeaths != 2 || h.Stats.KDRatio != 2 || h.Stats.PvPKDRatio != 4 || h.Value != 2 {
		t.Fatalf("hermits = %+v value %v", h.Stats, h.Value)
	}
	if h.Rank >= e.Rank {
		t.Fatalf("overall K/D 2.00 must rank above 1.50: hermits #%d, splitters #%d", h.Rank, e.Rank)
	}
	// The deaths board (fewest first) still counts every death.
	deaths := w.freshBoard(w.org1, w.inst1, LeaderboardDeaths)
	if entryFor(t, deaths, f2.ID).Value != 2 || entryFor(t, deaths, f.ID).Value != 8 {
		t.Fatalf("deaths board values: %v / %v", entryFor(t, deaths, f2.ID).Value, entryFor(t, deaths, f.ID).Value)
	}
}
