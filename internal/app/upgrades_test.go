package app

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func grant(name string, id int64) repository.PriorityGrant {
	return repository.PriorityGrant{Name: name, Reason: "RANKED", PlayerID: id}
}

func TestPlanPriorityOnlyManagesItsOwnNames(t *testing.T) {
	list := []string{"StaffPick", "OldTop", "Bought"}
	granted := []repository.PriorityGrant{grant("OldTop", 1), grant("Bought", 2), grant("GoneFromList", 3)}
	wanted := []repository.PriorityGrant{grant("NewTop", 4), grant("staffpick", 5), grant("NewTop", 4), grant("bad\nname", 6)}
	plan := planPriority(list, granted, wanted, func(g repository.PriorityGrant) bool { return g.Name == "Bought" })
	if !reflect.DeepEqual(plan.list, []string{"StaffPick", "Bought", "NewTop"}) {
		t.Fatalf("list: %v", plan.list)
	}
	if len(plan.added) != 1 || plan.added[0].Name != "NewTop" {
		t.Fatalf("a name staff already added is not taken over, and a bad name is skipped: %+v", plan.added)
	}
	sort.Strings(plan.removed)
	if !reflect.DeepEqual(plan.removed, []string{"Bought", "GoneFromList", "OldTop"}) || !plan.changed {
		t.Fatalf("removed: %v", plan.removed)
	}
	// Nothing to do writes nothing.
	same := planPriority([]string{"A"}, []repository.PriorityGrant{grant("A", 1)}, []repository.PriorityGrant{grant("a", 1)}, func(repository.PriorityGrant) bool { return false })
	if same.changed || len(same.added)+len(same.removed) != 0 {
		t.Fatalf("no change: %+v", same)
	}
}

func TestUpgradeSettingsSwitchesAndLimits(t *testing.T) {
	before := repository.DefaultUpgradeSettings()
	after := before
	after.PriorityRankedTop, after.LifeStoryDMs, after.SeasonRewards = 10, true, [3]int64{0, 500, 0}
	after.WinbackDays = 14
	got := repository.UpgradeSwitches(before, after)
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"lifeStoryDms", "priorityRankedTop", "seasonRewards"}) {
		t.Fatalf("switched on: %v", got)
	}
	if len(repository.UpgradeSwitches(after, after)) != 0 {
		t.Fatal("nothing new")
	}
	bad := after
	bad.PriorityRankedTop = 51
	if bad.Validate() == nil {
		t.Fatal("priority top is capped at 50")
	}
	var th upgradeThrottle
	now := time.Now()
	if !th.due("k", time.Minute, now) || th.due("k", time.Minute, now.Add(30*time.Second)) || !th.due("k", time.Minute, now.Add(61*time.Second)) {
		t.Fatal("throttle")
	}
}

func TestBusiestHoursWrapsMidnight(t *testing.T) {
	var h [24]int
	h[23], h[0], h[12] = 10, 9, 15
	if start, total := busiestHours(h, 2); start != 23 || total != 19 {
		t.Fatalf("23-01 is busiest: %d %d", start, total)
	}
}

func TestRankedTag(t *testing.T) {
	a := repository.RankedAward{Outcome: "AWARDED", Amount: 350, Multiplier: 2, Bonuses: []repository.RankedBonus{{Kind: repository.BonusBounty}, {Kind: repository.BonusRevenge}}}
	if got := rankedTag(a); got != "+350 RP ⚡💀🔁" {
		t.Fatalf("tag: %q", got)
	}
	if rankedTag(repository.RankedAward{Outcome: "COOLDOWN"}) != "" {
		t.Fatal("no tag without RP")
	}
}

func TestWeaponMastery(t *testing.T) {
	if m := weaponMastery(repository.WeaponRecord{Kills: 10}); m.Rank != "" || m.Next != "Bronze" || m.ToNext != 15 {
		t.Fatalf("below bronze: %+v", m)
	}
	if m := weaponMastery(repository.WeaponRecord{Kills: 120}); m.Rank != "Silver" || m.Next != "Gold" || m.ToNext != 130 {
		t.Fatalf("silver: %+v", m)
	}
	if m := weaponMastery(repository.WeaponRecord{Kills: 600}); m.Rank != "Master" || m.Next != "" || m.ToNext != 0 {
		t.Fatalf("master: %+v", m)
	}
}

func TestPlayStreakAndDailyReward(t *testing.T) {
	d := func(n int) time.Time { return time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC) }
	if s := playStreak([]time.Time{d(10), d(9), d(8), d(6)}); s != 3 {
		t.Fatalf("a gap ends the streak: %d", s)
	}
	if s := playStreak([]time.Time{d(10), d(9), d(8), d(7), d(6), d(5), d(4), d(3), d(2)}); s != 7 {
		t.Fatalf("capped at 7: %d", s)
	}
	if dailyReward(10, 5, 3) != 20 || dailyReward(10, 5, 1) != 10 || dailyReward(10, 5, 0) != 0 {
		t.Fatal("reward amounts")
	}
}

func TestCleanGiftMessage(t *testing.T) {
	if got := cleanGiftMessage("  gg\n@everyone  have fun\x07 "); got != "gg @\u200beveryone have fun" {
		t.Fatalf("note: %q", got)
	}
	long := cleanGiftMessage(string(make([]rune, 0)) + strings.Repeat("a", 300))
	if len([]rune(long)) != 200 {
		t.Fatalf("capped: %d", len(long))
	}
}

func TestPickSpotlight(t *testing.T) {
	servers := []repository.NetworkServer{{InstallationID: 1, ActivePlayers7d: 10, Kills7d: 5}, {InstallationID: 2, ActivePlayers7d: 8, Kills7d: 20}, {InstallationID: 3}}
	if p := pickSpotlight(servers, 0); p == nil || p.InstallationID != 2 {
		t.Fatalf("most active wins: %+v", p)
	}
	if p := pickSpotlight(servers, 2); p == nil || p.InstallationID != 1 {
		t.Fatalf("not twice in a row: %+v", p)
	}
	if pickSpotlight([]repository.NetworkServer{{InstallationID: 3}}, 0) != nil {
		t.Fatal("no activity, no pick")
	}
}

func TestAutomationNews(t *testing.T) {
	news := automationNewsFor([]string{"shopProgressDms", "featureAnnouncements", "caseWeeklyDigest", "bountyDms"})
	if len(news) != 2 || news[0][0] != "💌 Bounty updates" {
		t.Fatalf("only player-facing switches, sorted: %v", news)
	}
	embed := buildAutomationNews(news, "Champions")
	if embed.Title != "✨ New on Champions" || !strings.Contains(embed.Description, "Order updates") {
		t.Fatalf("embed: %+v", embed)
	}
}
