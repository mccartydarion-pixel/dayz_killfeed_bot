package app

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestRecapWindowIsMondayNoonToTuesdayNoonUTC(t *testing.T) {
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) // a Monday
	if got := repository.RecapWeekStart(monday.Add(50 * time.Hour)); !got.Equal(monday) {
		t.Fatalf("week start: %v", got)
	}
	if got := repository.RecapWeekStart(monday.Add(-time.Minute)); !got.Equal(monday.AddDate(0, 0, -7)) {
		t.Fatalf("Sunday night belongs to the week before: %v", got)
	}
	cases := []struct {
		at   time.Time
		open bool
	}{
		{monday.Add(11 * time.Hour), false},
		{monday.Add(12 * time.Hour), true},
		{monday.Add(35 * time.Hour), true},
		{monday.Add(36 * time.Hour), false},
		{monday.Add(4 * 24 * time.Hour), false},
	}
	for _, c := range cases {
		week, open := recapWindow(c.at)
		if open != c.open || !week.Equal(monday.AddDate(0, 0, -7)) {
			t.Fatalf("%v: open=%v week=%v", c.at, open, week)
		}
	}
}

func TestTierLevelsAndBonusSettings(t *testing.T) {
	if ranked.Unranked.Level() != 0 || ranked.Rookie.Level() != 1 || ranked.Master.Level() != 7 || ranked.Tier("X").Level() != -1 {
		t.Fatal("tier levels")
	}
	s := repository.DefaultRankedBonusSettings()
	if s.AnyKillBonus() || s.Validate() != nil {
		t.Fatal("defaults are valid and off")
	}
	s.RevengeMinutes = 500
	if s.Validate() == nil {
		t.Fatal("revenge window is capped")
	}
}

func TestRankedCards(t *testing.T) {
	wanted := buildWantedCard(repository.WantedPlayer{Name: "Mike", Reason: repository.WantedStreak, Streak: 6, Bounty: 100}, "Champions")
	if wanted.Title != "💀 Bounty on Mike" || !strings.Contains(wanted.Description, "6-kill streak") || !strings.Contains(wanted.Description, "+100 RP") {
		t.Fatalf("wanted: %+v", wanted)
	}
	claim := buildBountyClaimCard(repository.BountyClaim{KillerName: "Dan", VictimName: "Mike", Detail: "was #1 on the server", BountyRP: 100, TotalRP: 200})
	if !strings.Contains(claim.Description, "**Dan** killed **Mike**, who was #1 on the server, and collected the **+100 RP** bounty (200 RP for the kill)") {
		t.Fatalf("claim: %s", claim.Description)
	}
	up := buildRankUpCard(repository.RankUp{Name: "Dan", To: ranked.Gold, RP: 1000, Position: 2}, "Champions", "https://site.example/")
	// Numbers carry thousands separators and the server is named in the footer.
	if up.Title != "🏅 Dan reached Gold" || up.Description != "1,000 RP • #2" || up.Footer == nil || up.Footer.Text != "Champions" {
		t.Fatalf("rank-up: %+v %+v", up, up.Footer)
	}
	// The tier icon is the thumbnail, served by the website; without a site address there is none.
	if up.Thumbnail == nil || up.Thumbnail.URL != "https://site.example/ranks/gold.png" {
		t.Fatalf("rank-up thumbnail: %+v", up.Thumbnail)
	}
	if bare := buildRankUpCard(repository.RankUp{Name: "Dan", To: ranked.Gold, RP: 1000, Position: 2}, "Champions", ""); bare.Thumbnail != nil || bare.Title != up.Title {
		t.Fatalf("rank-up without a site: %+v", bare.Thumbnail)
	}
	dm := buildRankUpDM(repository.RankUp{Name: "Dan", To: ranked.Master, RP: 9000, Position: 1}, "Champions", "https://site.example/dashboard/player", "https://site.example")
	if e := dm.Embeds[0]; e.Thumbnail == nil || e.Thumbnail.URL != "https://site.example/ranks/master.png" || e.Title != "🏅 You reached Master" {
		t.Fatalf("rank-up DM: %+v %+v", e, e.Thumbnail)
	}
	if e := buildRankUpDM(repository.RankUp{To: ranked.Master}, "Champions", "", "").Embeds[0]; e.Thumbnail != nil {
		t.Fatalf("rank-up DM without a site: %+v", e.Thumbnail)
	}
	recap := buildWeeklyRecapCard(repository.RankedRecap{WeekStart: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), Kills: 0}, "")
	if len(recap.Fields) != 0 || !strings.Contains(recap.Description, "Sep 28 to Oct 4") {
		t.Fatalf("recap: %+v", recap)
	}
}
