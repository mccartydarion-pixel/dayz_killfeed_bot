//go:build integration

package factionstats

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Real-PostgreSQL leaderboard tests. Fixtures come from world_integration_test.go.

func (w *world) board(org, inst int64, m LeaderboardMetric, search string, limit int, cursor *LeaderboardCursor) *Leaderboard {
	w.t.Helper()
	p, err := w.svc.GetFactionLeaderboard(w.ctx, org, inst, LeaderboardQuery{Metric: m, Search: search, Limit: limit, Cursor: cursor})
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// freshBoard drops the cached leaderboard and returns the whole ranking.
func (w *world) freshBoard(org, inst int64, m LeaderboardMetric) *Leaderboard {
	w.t.Helper()
	w.svc.InvalidateLeaderboard(org, inst)
	return w.board(org, inst, m, "", MaxLeaderboardLimit, nil)
}

func names(p *Leaderboard) []string {
	out := []string{}
	for _, e := range p.Items {
		out = append(out, e.Faction.Name)
	}
	return out
}

func entryFor(t *testing.T, p *Leaderboard, factionID int64) LeaderboardEntry {
	t.Helper()
	for _, e := range p.Items {
		if e.Faction.FactionID == factionID {
			return e
		}
	}
	t.Fatalf("faction %d not on the leaderboard", factionID)
	return LeaderboardEntry{}
}

// richFaction builds a faction that exercises every attribution rule the profile applies: linked and
// unlinked members, a former member with two membership periods, a kill after leaving, a team kill,
// deaths, headshots/longshots and two claimed bounties on separate kills.
func (w *world) richFaction(inst int64, name, tag string, scale int) *repository.HubFaction {
	w.t.Helper()
	guild, server := w.guild1, w.server1a
	if inst == w.inst2 {
		guild, server = w.guild2, w.server2
	}
	lead, leadP := w.linked(guild, name+" Lead")
	a, aP := w.linked(guild, name+" A")
	b, bP := w.linked(guild, name+" B")
	unlinked := w.user()
	f := w.faction(inst, lead, name, tag)
	w.join(f, lead, a)
	bMember := w.join(f, lead, b)
	w.join(f, lead, unlinked)
	if _, err := w.hub.RemoveMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, bMember, lead); err != nil {
		w.t.Fatal(err)
	}
	w.periods(f, lead, span{From: w.day(0)})
	w.periods(f, a, span{From: w.day(0)})
	w.periods(f, unlinked, span{From: w.day(0)})
	w.periods(f, b, span{From: w.day(1), To: w.day(3)}, span{From: w.day(20), To: w.day(22)})
	victims := make([]int64, 8)
	for i := range victims {
		victims[i] = w.outsider(guild, "V")
	}
	at := func(d float64, m int) time.Time { return w.day(d).Add(time.Duration(m) * time.Minute) }
	for i := 0; i < scale; i++ {
		w.kill(guild, server, leadP, victims[i%8], at(5, i), killOpt{headshot: i%3 == 0, longshot: i%4 == 0, distance: 120})
		w.kill(guild, server, aP, victims[(i+1)%8], at(6, i), killOpt{headshot: i%5 == 0})
	}
	w.kill(guild, server, bP, victims[0], at(2, 0), killOpt{})  // counts: b was a member
	w.kill(guild, server, bP, victims[0], at(10, 0), killOpt{}) // after leaving: not counted
	w.kill(guild, server, bP, victims[0], at(21, 0), killOpt{}) // second period: counts
	w.kill(guild, server, leadP, aP, at(7, 0), killOpt{})       // team kill: not counted
	w.death(guild, server, leadP, at(5, scale/2), "UNKNOWN")
	w.death(guild, server, aP, at(7, 0), "UNKNOWN")
	k1 := w.kill(guild, server, aP, victims[1], at(8, 0), killOpt{})
	k2 := w.kill(guild, server, aP, victims[2], at(8, 1), killOpt{})
	w.bounty(guild, server, victims[1], aP, k1, 100, at(8, 0))
	w.bounty(guild, server, victims[2], aP, k2, 25, at(8, 1))
	return f
}

func TestLeaderboardMatchesFactionProfilesExactly(t *testing.T) {
	w := newWorld(t)
	var fs []*repository.HubFaction
	for i, n := range []string{"Alpha Wolves", "Bravo Bears", "Charlie Crows", "Delta Dogs", "Echo Eagles", "Foxtrot Foxes"} {
		fs = append(fs, w.richFaction(w.inst1, n, fmt.Sprintf("F%d", i), 4+i*3))
	}
	// One faction with no tracked activity at all, and one whose only member has no verified link.
	empty, _ := w.linked(w.guild1, "Empty Lead")
	fe := w.faction(w.inst1, empty, "Quiet Corner", "QC")
	fg := w.faction(w.inst1, w.user(), "Ghost Squad", "GS")
	fs = append(fs, fe, fg)
	for _, f := range fs {
		w.evaluate(f)
	}

	board := w.freshBoard(w.org1, w.inst1, LeaderboardKills)
	if len(board.Items) != len(fs) || board.Total != len(fs) {
		t.Fatalf("every faction is listed: %d of %d", len(board.Items), len(fs))
	}
	for _, f := range fs {
		prof := w.fresh(f).Summary
		e := entryFor(t, board, f.ID)
		got := e.Stats
		if got.Kills != prof.Kills || got.Deaths != prof.Deaths || got.KDRatio != prof.KDRatio || got.Headshots != prof.Headshots || got.Longshots != prof.Longshots ||
			got.BestKillStreak != prof.BestKillStreak || got.BountiesClaimed != prof.BountiesClaimed || got.BountyValueClaimed != prof.BountyValueClaimed ||
			got.AchievementsUnlocked != prof.AchievementsUnlocked {
			t.Errorf("%s: the leaderboard must equal the profile\n leaderboard %+v\n profile     %+v", f.Name, got, prof)
		}
		if e.Faction.MemberCount != prof.MemberCount {
			t.Errorf("%s: member count %d vs %d", f.Name, e.Faction.MemberCount, prof.MemberCount)
		}
		switch {
		case e.Faction.TrackingSince == nil && prof.TrackingSince != nil, e.Faction.TrackingSince != nil && prof.TrackingSince == nil:
			t.Errorf("%s: trackingSince presence differs", f.Name)
		case e.Faction.TrackingSince != nil:
			pt, err := time.Parse(time.RFC3339, *prof.TrackingSince)
			if err != nil || pt.Sub(*e.Faction.TrackingSince).Abs() > time.Second {
				t.Errorf("%s: trackingSince %v vs %v", f.Name, e.Faction.TrackingSince, *prof.TrackingSince)
			}
		}
	}
	// The rich factions really exercise the rules (a vacuous equality would prove nothing).
	fa := entryFor(t, board, fs[0].ID).Stats
	if fa.Kills == 0 || fa.Deaths == 0 || fa.Headshots == 0 || fa.Longshots == 0 || fa.BountiesClaimed != 2 || fa.BountyValueClaimed != 125 || fa.BestKillStreak == 0 {
		t.Fatalf("the fixture must produce non-trivial figures: %+v", fa)
	}
	// Alpha (scale 4): lead 4 + a 4 + a's two bounty kills + b's two counted kills = 12; the team kill and b's late kill are excluded.
	if fa.Kills != 12 {
		t.Fatalf("attribution rules: alpha kills = %d, want 12", fa.Kills)
	}
	if fa.Deaths != 2 {
		t.Fatalf("alpha deaths = %d, want 2", fa.Deaths)
	}
	for _, m := range LeaderboardMetrics {
		p := w.board(w.org1, w.inst1, m, "", 100, nil)
		for i := 1; i < len(p.Items); i++ {
			if lessKey(p.Items[i].key, p.Items[i-1].key) {
				t.Fatalf("%s is not sorted by its key at %d", m, i)
			}
			if p.Items[i].Rank != i+1 {
				t.Fatalf("%s: ordinal ranks: %d at position %d", m, p.Items[i].Rank, i+1)
			}
		}
	}
	// The empty and the unlinked-only factions are listed with zeros and flagged.
	for _, f := range []*repository.HubFaction{fe, fg} {
		e := entryFor(t, board, f.ID)
		if e.HasTrackedActivity || e.Stats.Kills != 0 || e.Faction.MemberCount != 1 {
			t.Errorf("%s: listed with no tracked activity: %+v", f.Name, e)
		}
	}
	if got := w.board(w.org1, w.inst1, LeaderboardKills, "ghost squad", 10, nil); len(got.Items) != 1 {
		t.Fatalf("search finds the new faction: %v", names(got))
	}
}

func TestLeaderboardRankingsFromRealData(t *testing.T) {
	w := newWorld(t)
	victim := w.outsider(w.guild1, "Victim")
	mk := func(name, tag string, kills, hs, ls, deaths, breakEvery, bounties int) *repository.HubFaction {
		u, p := w.linked(w.guild1, name)
		f := w.faction(w.inst1, u, name, tag)
		w.periods(f, u, span{From: w.day(0)})
		for i := 1; i <= kills; i++ {
			at := w.day(2).Add(time.Duration(i) * time.Minute)
			w.kill(w.guild1, w.server1a, p, victim, at, killOpt{headshot: i <= hs, longshot: i <= ls, distance: 90})
			if breakEvery > 0 && i%breakEvery == 0 {
				w.death(w.guild1, w.server1a, p, at.Add(30*time.Second), "UNKNOWN")
			}
		}
		for i := 0; i < deaths; i++ {
			w.death(w.guild1, w.server1a, p, w.day(3).Add(time.Duration(i)*time.Minute), "UNKNOWN")
		}
		for i := 0; i < bounties; i++ {
			at := w.day(4).Add(time.Duration(i) * time.Minute)
			k := w.kill(w.guild1, w.server1a, p, victim, at, killOpt{})
			w.bounty(w.guild1, w.server1a, victim, p, k, 10*(i+1), at)
		}
		return f
	}
	//                          kills hs ls deaths break bounties
	fa := mk("Alpha", "AAA", 30, 3, 0, 5, 10, 0)   // 30 kills, deaths 3+5 = 8, best streak 10, K/D 3.75
	fb := mk("Bravo", "BBB", 12, 9, 1, 0, 0, 0)    // 12 kills, 9 headshots, streak 12, K/D 12
	fc := mk("Charlie", "CCC", 20, 0, 12, 4, 5, 0) // 12 longshots, deaths 4+4 = 8, streak 5, K/D 2.5
	fd := mk("Delta", "DDD", 5, 0, 0, 0, 0, 4)     // 9 kills, 4 bounties worth 100, streak 9, K/D 9
	for _, f := range []*repository.HubFaction{fa, fb, fc, fd} {
		w.evaluate(f)
	}
	expect := func(m LeaderboardMetric, want ...string) {
		t.Helper()
		if got := names(w.freshBoard(w.org1, w.inst1, m)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", m, got, want)
		}
	}
	expect(LeaderboardKills, "Alpha", "Charlie", "Bravo", "Delta")
	expect(LeaderboardHeadshots, "Bravo", "Alpha", "Charlie", "Delta") // 9, 3, then 0/0 by kills DESC
	expect(LeaderboardLongshots, "Charlie", "Bravo", "Alpha", "Delta") // 12, 1, then 0/0 by kills DESC
	expect(LeaderboardBestStreak, "Bravo", "Alpha", "Delta", "Charlie")
	expect(LeaderboardBountiesClaimed, "Delta", "Alpha", "Charlie", "Bravo")
	expect(LeaderboardBountyValue, "Delta", "Alpha", "Charlie", "Bravo")
	expect(LeaderboardKD, "Bravo", "Delta", "Alpha", "Charlie") // 12.00, 9.00, 3.75, 2.50
	expect(LeaderboardDeaths, "Bravo", "Delta", "Alpha", "Charlie")
	board := func(m LeaderboardMetric) *Leaderboard { return w.board(w.org1, w.inst1, m, "", 10, nil) }
	if e := entryFor(t, board(LeaderboardKD), fb.ID); e.Value != 12 || e.Stats.KDRatio != 12 {
		t.Fatalf("zero deaths: K/D equals the kill count: %+v", e)
	}
	if e := entryFor(t, board(LeaderboardKD), fa.ID); e.Value != 3.75 {
		t.Fatalf("K/D is a two-decimal value: %v", e.Value)
	}
	if e := entryFor(t, board(LeaderboardBountyValue), fd.ID); e.Value != 100 || e.Stats.BountiesClaimed != 4 {
		t.Fatalf("bounty value: %+v", e)
	}
	if e := entryFor(t, board(LeaderboardBestStreak), fb.ID); e.Value != 12 {
		t.Fatalf("best streak: %v", e.Value)
	}
	// ACHIEVEMENTS counts exactly the recorded system unlocks, and sorts descending.
	byAch := w.freshBoard(w.org1, w.inst1, LeaderboardAchievements)
	for i := 1; i < len(byAch.Items); i++ {
		if byAch.Items[i].Stats.AchievementsUnlocked > byAch.Items[i-1].Stats.AchievementsUnlocked {
			t.Fatalf("achievements must sort descending: %v", names(byAch))
		}
	}
	unlocked := false
	for _, e := range byAch.Items {
		n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, e.Faction.FactionID)
		if n != e.Stats.AchievementsUnlocked || int(e.Value) != n {
			t.Fatalf("%s: %d unlock rows vs %d (value %v) on the leaderboard", e.Faction.Name, n, e.Stats.AchievementsUnlocked, e.Value)
		}
		unlocked = unlocked || n > 0
	}
	if !unlocked {
		t.Fatal("the fixture should unlock at least one achievement")
	}
}

func TestLeaderboardTiesAreDeterministic(t *testing.T) {
	w := newWorld(t)
	victim := w.outsider(w.guild1, "Victim")
	var players []int64
	for i := 0; i < 5; i++ { // five identical factions
		u, p := w.linked(w.guild1, fmt.Sprintf("Twin %d", i))
		f := w.faction(w.inst1, u, fmt.Sprintf("Twin Faction %d", i), fmt.Sprintf("T%d", i))
		w.periods(f, u, span{From: w.day(0)})
		for k := 1; k <= 6; k++ {
			w.kill(w.guild1, w.server1a, p, victim, w.day(1).Add(time.Duration(k)*time.Minute), killOpt{headshot: k <= 2})
		}
		w.death(w.guild1, w.server1a, p, w.day(2), "UNKNOWN")
		players = append(players, p)
	}
	for _, m := range LeaderboardMetrics {
		var first []string
		for run := 0; run < 6; run++ {
			got := names(w.freshBoard(w.org1, w.inst1, m))
			if first == nil {
				first = got
			} else if fmt.Sprint(got) != fmt.Sprint(first) {
				t.Fatalf("%s: ties must order identically every time: %v vs %v", m, got, first)
			}
		}
		for i := range first { // a full tie ends in faction id ascending (creation order)
			if want := fmt.Sprintf("Twin Faction %d", i); first[i] != want {
				t.Errorf("%s: position %d = %s, want %s (id ASC on a full tie)", m, i+1, first[i], want)
			}
		}
	}
	// A real tie-breaker beats the id: on equal headshots, more kills leads.
	w.kill(w.guild1, w.server1a, players[4], victim, w.day(1).Add(time.Hour), killOpt{})
	if got := names(w.freshBoard(w.org1, w.inst1, LeaderboardHeadshots)); got[0] != "Twin Faction 4" {
		t.Fatalf("on equal headshots the faction with more kills leads: %v", got)
	}
}

func TestLeaderboardPaginationAndSearchRealData(t *testing.T) {
	w := newWorld(t)
	victim := w.outsider(w.guild1, "Victim")
	for i := 0; i < 37; i++ {
		u, p := w.linked(w.guild1, "P")
		f := w.faction(w.inst1, u, fmt.Sprintf("Page Faction %02d", i), fmt.Sprintf("P%02d", i))
		w.periods(f, u, span{From: w.day(0)})
		for k := 0; k < i%6; k++ { // heavy ties
			w.kill(w.guild1, w.server1a, p, victim, w.day(1).Add(time.Duration(k)*time.Minute), killOpt{})
		}
	}
	full := w.freshBoard(w.org1, w.inst1, LeaderboardKills)
	if len(full.Items) != 37 {
		t.Fatalf("all factions: %d", len(full.Items))
	}
	for _, size := range []int{1, 5, 10, 36, 37, 38} {
		seen := map[int64]bool{}
		var order []string
		var cursor *LeaderboardCursor
		for pages := 0; ; pages++ {
			p := w.board(w.org1, w.inst1, LeaderboardKills, "", size, cursor)
			for _, e := range p.Items {
				if seen[e.Faction.FactionID] {
					t.Fatalf("size %d: %s appeared twice", size, e.Faction.Name)
				}
				seen[e.Faction.FactionID] = true
				order = append(order, e.Faction.Name)
			}
			if p.NextCursor == nil {
				break
			}
			var ok bool
			if cursor, ok = DecodeLeaderboardCursor(*p.NextCursor); !ok {
				t.Fatal("bad cursor")
			}
			if pages > 50 {
				t.Fatal("did not terminate")
			}
		}
		if len(seen) != 37 || fmt.Sprint(order) != fmt.Sprint(names(full)) {
			t.Fatalf("size %d: %d factions seen; the pages must equal the full ranking", size, len(seen))
		}
	}
	p := w.board(w.org1, w.inst1, LeaderboardKills, "PAGE FACTION 1", 50, nil)
	if p.Total != 10 || len(p.Items) != 10 { // "Page Faction 10".."19"
		t.Fatalf("search total: %d / %d", p.Total, len(p.Items))
	}
	for _, e := range p.Items {
		if e.Rank != entryFor(t, full, e.Faction.FactionID).Rank {
			t.Fatal("search must not change ranks")
		}
	}
	if p = w.board(w.org1, w.inst1, LeaderboardKills, "p07", 5, nil); len(p.Items) != 1 || p.Items[0].Faction.Tag != "P07" {
		t.Fatalf("tag search: %v", names(p))
	}
}

func TestLeaderboardIsolationAcrossInstallationsServersAndOrganizations(t *testing.T) {
	w := newWorld(t)
	// inst1 and inst1b: the same organization AND the same Discord guild, two DayZ servers.
	shared, sharedP := w.linked(w.guild1, "Roamer")
	l1, l1P := w.linked(w.guild1, "LeadOne")
	l2, _ := w.linked(w.guild1, "LeadTwo")
	victim := w.outsider(w.guild1, "Victim")
	f1 := w.faction(w.inst1, l1, "Server One Clan", "S1")
	f1b := w.faction(w.inst1b, l2, "Server Two Clan", "S2")
	w.join(f1, l1, shared)
	w.join(f1b, l2, shared) // the same person belongs to a faction on each installation
	w.periods(f1, shared, span{From: w.day(0)})
	w.periods(f1b, shared, span{From: w.day(0)})
	w.periods(f1, l1, span{From: w.day(0)})
	for i := 0; i < 4; i++ {
		w.kill(w.guild1, w.server1a, sharedP, victim, w.day(1).Add(time.Duration(i)*time.Minute), killOpt{})
	}
	for i := 0; i < 9; i++ {
		w.kill(w.guild1, w.server1b, sharedP, victim, w.day(1).Add(time.Duration(i)*time.Minute), killOpt{headshot: true})
	}
	w.kill(w.guild1, w.server1a, l1P, victim, w.day(1), killOpt{})
	// Another organization, another guild.
	l3, l3P := w.linked(w.guild2, "Alien")
	f2 := w.faction(w.inst2, l3, "Other Org Clan", "OO")
	w.periods(f2, l3, span{From: w.day(0)})
	w.kill(w.guild2, w.server2, l3P, w.outsider(w.guild2, "V2"), w.day(1), killOpt{})

	b1 := w.freshBoard(w.org1, w.inst1, LeaderboardKills)
	if fmt.Sprint(names(b1)) != "[Server One Clan]" {
		t.Fatalf("installation 1 lists only its own factions: %v", names(b1))
	}
	if e := b1.Items[0]; e.Stats.Kills != 5 || e.Stats.Headshots != 0 {
		t.Fatalf("installation 1 counts only its server's kills (4 + 1), never the other server's: %+v", e.Stats)
	}
	b1b := w.freshBoard(w.org1, w.inst1b, LeaderboardKills)
	if fmt.Sprint(names(b1b)) != "[Server Two Clan]" || b1b.Items[0].Stats.Kills != 9 || b1b.Items[0].Stats.Headshots != 9 {
		t.Fatalf("installation 1b (same guild, other server): %v %+v", names(b1b), b1b.Items[0].Stats)
	}
	b2 := w.freshBoard(w.org2, w.inst2, LeaderboardKills)
	if fmt.Sprint(names(b2)) != "[Other Org Clan]" || b2.Items[0].Stats.Kills != 1 {
		t.Fatalf("other organization: %v", names(b2))
	}
	if got := w.board(w.org1, w.inst1, LeaderboardKills, "Other Org", 10, nil); len(got.Items) != 0 || got.Total != 0 {
		t.Fatalf("no leakage through search: %v", names(got))
	}
	for _, p := range [][2]int64{{w.org2, w.inst1}, {w.org1, w.inst2}, {w.org2, w.inst1b}, {w.org1, 999999999}} {
		if _, err := w.svc.GetFactionLeaderboard(w.ctx, p[0], p[1], LeaderboardQuery{}); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("org %d installation %d: want ErrNotFound, got %v", p[0], p[1], err)
		}
	}
	if p := w.fresh(f1).Summary; p.Kills != 5 {
		t.Fatalf("profile parity: %+v", p)
	}
}

func TestLeaderboardIdentityAndMembershipTiming(t *testing.T) {
	w := newWorld(t)
	victim := w.outsider(w.guild1, "Victim")
	lead, leadP := w.linked(w.guild1, "Lead")
	joiner, joinerP := w.linked(w.guild1, "Joiner")
	leaver, leaverP := w.linked(w.guild1, "Leaver")
	unlinked := w.user()
	pending := w.user()
	pendingP := w.player(w.guild1, fmt.Sprintf("dz-pend-%d-%d", w.suffix, w.next()), "PendingGuy")
	w.link(w.guild1, pendingP, pending, "PENDING")
	namesake := w.player(w.guild1, fmt.Sprintf("dz-name-%d-%d", w.suffix, w.next()), "Lead") // same in-game name as the leader, no link
	f := w.faction(w.inst1, lead, "Clock Watchers", "CW")
	for _, u := range []int64{joiner, leaver, unlinked, pending} {
		w.join(f, lead, u)
	}
	w.periods(f, lead, span{From: w.day(0)})
	w.periods(f, unlinked, span{From: w.day(0)})
	w.periods(f, pending, span{From: w.day(0)})
	w.periods(f, joiner, span{From: w.day(10)})                         // joins on day 10
	w.periods(f, leaver, span{From: w.day(0), To: w.day(5)})            // leaves on day 5
	w.kill(w.guild1, w.server1a, joinerP, victim, w.day(2), killOpt{})  // before joining: not counted
	w.kill(w.guild1, w.server1a, joinerP, victim, w.day(12), killOpt{}) // while a member: counted
	w.kill(w.guild1, w.server1a, leaverP, victim, w.day(2), killOpt{})  // while a member: counted
	w.kill(w.guild1, w.server1a, leaverP, victim, w.day(8), killOpt{})  // after leaving: not counted
	w.kill(w.guild1, w.server1a, leadP, victim, w.day(3), killOpt{})
	w.kill(w.guild1, w.server1a, namesake, victim, w.day(3), killOpt{}) // a name match is not an identity
	w.kill(w.guild1, w.server1a, pendingP, victim, w.day(3), killOpt{}) // an unverified link is not an identity

	if got := entryFor(t, w.freshBoard(w.org1, w.inst1, LeaderboardKills), f.ID).Stats.Kills; got != 3 {
		t.Fatalf("timing and identity: %d kills, want 3", got)
	}
	if p := w.fresh(f).Summary; p.Kills != 3 {
		t.Fatalf("the profile agrees: %+v", p)
	}
	// Verifying the link attributes that member's kills made while a member.
	w.exec(`UPDATE player_links SET status='VERIFIED' WHERE player_id=$1`, pendingP)
	if got := entryFor(t, w.freshBoard(w.org1, w.inst1, LeaderboardKills), f.ID).Stats.Kills; got != 4 {
		t.Fatalf("after verification: %d, want 4", got)
	}
}

func TestLeaderboardCacheInvalidationOnRealEvents(t *testing.T) {
	w := newWorld(t)
	victim := w.outsider(w.guild1, "Victim")
	lead, leadP := w.linked(w.guild1, "Lead")
	f := w.faction(w.inst1, lead, "Live Board", "LB")
	w.periods(f, lead, span{From: w.day(0)})
	get := func(m LeaderboardMetric) LeaderboardEntry {
		return entryFor(t, w.board(w.org1, w.inst1, m, "", 50, nil), f.ID)
	}
	if get(LeaderboardKills).Stats.Kills != 0 {
		t.Fatal("baseline")
	}
	// A direct write is invisible while the cache is warm (bounded by the TTL) ...
	w.kill(w.guild1, w.server1a, leadP, victim, w.day(25), killOpt{})
	if get(LeaderboardKills).Stats.Kills != 0 {
		t.Fatal("warm cache expected")
	}
	// ... the notification the killfeed sends after a persisted kill invalidates it.
	w.svc.NotifyCombat(w.guild1, w.server1a, leadP)
	if got := get(LeaderboardKills).Stats.Kills; got != 1 {
		t.Fatalf("a persisted kill must invalidate the leaderboard: %d", got)
	}
	// A death.
	w.death(w.guild1, w.server1a, leadP, w.day(26), "UNKNOWN")
	if get(LeaderboardDeaths).Stats.Deaths != 0 {
		t.Fatal("warm cache expected")
	}
	w.svc.NotifyCombat(w.guild1, w.server1a, 0)
	if got := get(LeaderboardDeaths).Stats.Deaths; got != 1 {
		t.Fatalf("a death must invalidate: %d", got)
	}
	// A bounty claim.
	k := w.kill(w.guild1, w.server1a, leadP, victim, w.day(27), killOpt{})
	w.bounty(w.guild1, w.server1a, victim, leadP, k, 60, w.day(27))
	w.svc.NotifyCombat(w.guild1, w.server1a, leadP)
	if e := get(LeaderboardBountyValue); e.Value != 60 || e.Stats.BountiesClaimed != 1 {
		t.Fatalf("a bounty claim must invalidate: %+v", e)
	}
	// A membership change.
	w.join(f, lead, w.user())
	if got := get(LeaderboardKills).Faction.MemberCount; got != 1 {
		t.Fatalf("warm cache expected: %d", got)
	}
	w.svc.Invalidate(f.OrganizationID, f.InstallationID, f.ID)
	if got := get(LeaderboardKills).Faction.MemberCount; got != 2 {
		t.Fatalf("a membership change must invalidate: %d", got)
	}
	// A new faction appears once creation invalidates.
	other, _ := w.linked(w.guild1, "New")
	nf := w.faction(w.inst1, other, "Brand New", "BN")
	if got := w.board(w.org1, w.inst1, LeaderboardKills, "brand new", 5, nil); len(got.Items) != 0 {
		t.Fatal("warm cache expected")
	}
	w.svc.Invalidate(nf.OrganizationID, nf.InstallationID, nf.ID)
	if got := w.board(w.org1, w.inst1, LeaderboardKills, "brand new", 5, nil); len(got.Items) != 1 {
		t.Fatal("a new faction must appear once creation invalidates")
	}
	// An achievement unlock: Evaluate records it and invalidates.
	before := get(LeaderboardAchievements).Stats.AchievementsUnlocked
	won := w.evaluate(f)
	if len(won) == 0 {
		t.Fatal("expected at least one unlock")
	}
	if after := get(LeaderboardAchievements).Stats.AchievementsUnlocked; after != before+len(won) {
		t.Fatalf("an unlock must invalidate: %d -> %d (won %v)", before, after, won)
	}
	// Another organization's cache is a separate entry.
	if _, err := w.svc.GetFactionLeaderboard(w.ctx, w.org2, w.inst2, LeaderboardQuery{}); err != nil {
		t.Fatal(err)
	}
}

// TestLeaderboardScalesWithManyFactions loads 120 factions x 5 members and a guild-sized history and checks
// the leaderboard stays one grouped statement. PERF_KILLS scales the history (default 20000).
func TestLeaderboardScalesWithManyFactions(t *testing.T) {
	kills := 20000
	if v, err := strconv.Atoi(os.Getenv("PERF_KILLS")); err == nil && v > 0 {
		kills = v
	}
	const factions, perFaction = 120, 5
	w := newWorld(t)
	w.exec(`INSERT INTO players(guild_id, dayz_player_id, display_name) SELECT $1::bigint, 'lbperf-' || $2::bigint::text || '-' || g, 'Bulk ' || g FROM generate_series(1,3000) g`, w.guild1, w.suffix)
	var memberPlayers []int64
	var first *repository.HubFaction
	start := time.Now()
	for i := 0; i < factions; i++ {
		lead, leadP := w.linked(w.guild1, "L")
		f := w.faction(w.inst1, lead, fmt.Sprintf("Scale Faction %03d", i), fmt.Sprintf("S%03d", i))
		if first == nil {
			first = f
		}
		w.periods(f, lead, span{From: w.day(0)})
		memberPlayers = append(memberPlayers, leadP)
		for j := 1; j < perFaction; j++ {
			u, p := w.linked(w.guild1, "M")
			w.join(f, lead, u)
			w.periods(f, u, span{From: w.day(0)})
			memberPlayers = append(memberPlayers, p)
		}
	}
	t.Logf("created %d factions (%d members) in %v", factions, len(memberPlayers), time.Since(start).Round(time.Millisecond))

	load := time.Now()
	w.exec(`
WITH pl AS (SELECT array_agg(id ORDER BY id) AS ids FROM players WHERE guild_id=$1 AND dayz_player_id LIKE 'lbperf-' || $2::bigint::text || '-%')
INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, longshot, distance, event_time)
SELECT $1::bigint,$3::bigint,'perf','lbk-' || $2::bigint::text || '-' || g,
       CASE WHEN g % 3 = 0 THEN ($6::bigint[])[1 + (g * 7) % cardinality($6::bigint[])] ELSE (SELECT ids[1 + (g * 7919) % 3000] FROM pl) END,
       (SELECT ids[1 + (g * 104729) % 3000] FROM pl),
       (g % 5 = 0), (g % 11 = 0), 40, $4::timestamptz + (g * interval '30 seconds')
FROM generate_series(1,$5::bigint) g`, w.guild1, w.suffix, w.server1a, w.day(0), kills, memberPlayers)
	w.exec(`
INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, event_time)
SELECT $1::bigint,$3::bigint,'perf','lbd-' || $2::bigint::text || '-' || g, ($6::bigint[])[1 + (g * 13) % cardinality($6::bigint[])], 'UNKNOWN', $4::timestamptz + (g * interval '45 seconds')
FROM generate_series(1,$5::bigint) g`, w.guild1, w.suffix, w.server1a, w.day(0), kills/2, memberPlayers)
	w.exec(`INSERT INTO bounties(guild_id, server_id, target_player_id, created_by_type, status, reward_points, starts_at, claimed_by_player_id, claimed_kill_id, claimed_at)
SELECT $1::bigint,$2::bigint,k.victim_player_id,'MANUAL','CLAIMED',10,$4::timestamptz, k.killer_player_id, k.id, $4::timestamptz
FROM (SELECT id, killer_player_id, victim_player_id FROM kills WHERE guild_id=$1 AND event_fingerprint LIKE 'lbk-' || $3::bigint::text || '-%' ORDER BY id LIMIT $5::int) k`,
		w.guild1, w.server1a, w.suffix, w.day(1), kills/5)
	for _, tbl := range []string{"kills", "deaths", "bounties", "hub_faction_members", "hub_faction_membership_history"} {
		w.exec(`ANALYZE ` + tbl)
	}
	t.Logf("loaded %d kills, %d deaths, %d claimed bounties in %v", kills, kills/2, kills/5, time.Since(load).Round(time.Millisecond))

	timeIt := func(name string, fn func()) time.Duration {
		fn() // warm
		s := time.Now()
		fn()
		d := time.Since(s)
		t.Logf("%-42s %v", name, d.Round(time.Millisecond))
		return d
	}
	d1 := timeIt("leaderboard table, one statement (120)", func() {
		if _, err := w.store.Leaderboard(w.ctx, w.org1, w.inst1); err != nil {
			t.Fatal(err)
		}
	})
	d2 := timeIt("cached pages (9 metrics + search)", func() {
		w.svc.InvalidateLeaderboard(w.org1, w.inst1)
		_ = w.board(w.org1, w.inst1, LeaderboardKills, "", 25, nil) // fills the cache
	})
	d3 := timeIt("cache hit: 9 metric pages with search", func() {
		for _, m := range LeaderboardMetrics {
			_ = w.board(w.org1, w.inst1, m, "scale faction 01", 25, nil)
		}
	})
	sc, err := w.store.Scope(w.ctx, first.OrganizationID, first.InstallationID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = timeIt("one faction profile stats (comparison)", func() {
		if _, err := w.store.ComputeStats(w.ctx, sc); err != nil {
			t.Fatal(err)
		}
	})

	// 120 factions exceed the 100-row page maximum: walk the whole ranking by cursor.
	board := &Leaderboard{}
	w.svc.InvalidateLeaderboard(w.org1, w.inst1)
	var cursor *LeaderboardCursor
	for {
		p := w.board(w.org1, w.inst1, LeaderboardKills, "", MaxLeaderboardLimit, cursor)
		board.Items = append(board.Items, p.Items...)
		if p.NextCursor == nil {
			break
		}
		cursor, _ = DecodeLeaderboardCursor(*p.NextCursor)
	}
	if len(board.Items) != factions || !board.Items[0].HasTrackedActivity {
		t.Fatalf("%d factions ranked", len(board.Items))
	}
	var totalKills, totalDeaths int64
	for _, e := range board.Items {
		totalKills += e.Stats.Kills
		totalDeaths += e.Stats.Deaths
	}
	// A third of the kills and every death belong to faction members; nobody kills a teammate here.
	if totalKills != int64(kills/3) || totalDeaths != int64(kills/2) {
		t.Fatalf("leaderboard totals %d kills / %d deaths, want %d / %d", totalKills, totalDeaths, kills/3, kills/2)
	}
	// Spot-check parity on real volume.
	prof := w.fresh(first).Summary
	if e := entryFor(t, board, first.ID); e.Stats.Kills != prof.Kills || e.Stats.Deaths != prof.Deaths || e.Stats.BountiesClaimed != prof.BountiesClaimed || e.Stats.BestKillStreak != prof.BestKillStreak {
		t.Fatalf("profile parity at volume: %+v vs %+v", e.Stats, prof)
	}
	if d1 > 5*time.Second || d2 > 5*time.Second || d3 > 200*time.Millisecond {
		t.Errorf("leaderboard too slow: table %v, first page %v, cached pages %v", d1, d2, d3)
	}
}
