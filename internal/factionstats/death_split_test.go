package factionstats

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The PvP/PvE death split (internal/deathstats) on faction figures: kills, deaths and kdRatio keep
// their meaning; pvpDeaths, pveDeaths and pvpKdRatio are added beside them.

func TestBuildStatsSplitsDeaths(t *testing.T) {
	raw := &repository.HubStatsResult{MemberCount: 4, LinkedCount: 3, Members: []repository.HubMemberStats{
		{UserID: 1, Username: "mix", Active: true, Linked: true, Kills: 10, Deaths: 8, PvPDeaths: 3},
		{UserID: 2, Username: "pve-only", Active: true, Linked: true, Kills: 6, Deaths: 4, PvPDeaths: 0},
		{UserID: 3, Username: "pvp-only", Active: true, Linked: true, Kills: 4, Deaths: 2, PvPDeaths: 2},
		{UserID: 4, Username: "unlinked", Active: true, Linked: false, Kills: 99, Deaths: 99, PvPDeaths: 99},
		{UserID: 5, Username: "no-deaths", Active: false, Linked: true, Kills: 5},
	}}
	st := buildStats(raw, 0, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	want := []struct {
		deaths, pvp, pve int64
		kd, pvpKD        float64
	}{
		{8, 3, 5, 1.25, 3.33},
		{4, 0, 4, 1.5, 6}, // no PvP deaths: the PvP K/D is the kill count
		{2, 2, 0, 2, 2},
		{0, 0, 0, 0, 0}, // unlinked: nothing is attributed
		{0, 0, 0, 5, 5}, // no deaths at all: both are the kill count
	}
	for i, w := range want {
		m := st.MemberContributions[i]
		if m.Deaths != w.deaths || m.PvPDeaths != w.pvp || m.PvEDeaths != w.pve || m.KDRatio != w.kd || m.PvPKDRatio != w.pvpKD {
			t.Errorf("member %d (%s) = deaths %d pvp %d pve %d kd %v pvpKd %v, want %+v", i, m.DisplayName, m.Deaths, m.PvPDeaths, m.PvEDeaths, m.KDRatio, m.PvPKDRatio, w)
		}
		if m.PvPDeaths+m.PvEDeaths != m.Deaths {
			t.Errorf("member %d: the split does not add up", i)
		}
	}
	s := st.Summary
	// 25 kills, 14 deaths of which 5 PvP.
	if s.Kills != 25 || s.Deaths != 14 || s.PvPDeaths != 5 || s.PvEDeaths != 9 || s.KDRatio != 1.79 || s.PvPKDRatio != 5 {
		t.Fatalf("summary = %+v", s)
	}

	body, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Summary             map[string]any   `json:"summary"`
		MemberContributions []map[string]any `json:"memberContributions"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]float64{"deaths": 14, "kdRatio": 1.79, "pvpDeaths": 5, "pveDeaths": 9, "pvpKdRatio": 5} {
		if decoded.Summary[k] != v {
			t.Errorf("summary JSON %s = %v, want %v", k, decoded.Summary[k], v)
		}
	}
	for k, v := range map[string]float64{"deaths": 8, "kdRatio": 1.25, "pvpDeaths": 3, "pveDeaths": 5, "pvpKdRatio": 3.33} {
		if decoded.MemberContributions[0][k] != v {
			t.Errorf("member JSON %s = %v, want %v", k, decoded.MemberContributions[0][k], v)
		}
	}
}

func TestLeaderboardStatsSplitDeathsWithoutChangingTheRanking(t *testing.T) {
	a := repository.HubLeaderboardRow{FactionID: 1, Kills: 20, Deaths: 10, PvPDeaths: 2}
	b := repository.HubLeaderboardRow{FactionID: 2, Kills: 20, Deaths: 10, PvPDeaths: 10}
	sa, sb := statsOf(a), statsOf(b)
	if sa.KDRatio != 2 || sa.PvPDeaths != 2 || sa.PvEDeaths != 8 || sa.PvPKDRatio != 10 {
		t.Fatalf("stats a = %+v", sa)
	}
	if sb.KDRatio != 2 || sb.PvPDeaths != 10 || sb.PvEDeaths != 0 || sb.PvPKDRatio != 2 {
		t.Fatalf("stats b = %+v", sb)
	}
	// Every existing board is keyed exactly as before: the split takes no part in any ordering.
	for _, m := range LeaderboardMetrics {
		ka, kb := leaderboardKey(m, a), leaderboardKey(m, b)
		a2, b2 := a, b
		a2.PvPDeaths, b2.PvPDeaths = 0, 0
		if ka != leaderboardKey(m, a2) || kb != leaderboardKey(m, b2) {
			t.Errorf("%s: the PvP split changed a ranking key", m)
		}
	}
	body, _ := json.Marshal(sa)
	var decoded map[string]float64
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["deaths"] != 10 || decoded["kdRatio"] != 2 || decoded["pvpDeaths"] != 2 || decoded["pveDeaths"] != 8 || decoded["pvpKdRatio"] != 10 {
		t.Fatalf("leaderboard stats JSON = %s", body)
	}
}
