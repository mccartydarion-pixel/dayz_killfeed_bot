//go:build integration

package factionstats

import (
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// --- membership timing ----------------------------------------------------------------------------------

func TestKillsCountOnlyDuringMembershipPeriods(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Lead")
	a, aP := w.linked(w.guild1, "Alpha")
	b, bP := w.linked(w.guild1, "Bravo")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Timekeepers", "TK")
	w.join(f, lead, a)
	mB := w.join(f, lead, b)
	if _, err := w.hub.RemoveMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, mB, lead); err != nil { // B really left
		t.Fatal(err)
	}
	w.periods(f, lead, span{From: w.day(0)})
	// A: joins day 1, leaves day 3, REJOINS day 5 (still a member). B: day 1 to day 3, gone for good.
	w.periods(f, a, span{From: w.day(1), To: w.day(3)}, span{From: w.day(5)})
	w.periods(f, b, span{From: w.day(1), To: w.day(3)})

	// A's kills: day 0.5 (before ever joining), 2 (period 1), 4 (gap after leaving), 6 (period 2).
	for _, day := range []float64{0.5, 2, 4, 6} {
		w.kill(w.guild1, w.server1a, aP, victim, w.day(day), killOpt{})
	}
	// B's kills: before joining, during, after leaving (twice, incl. the very end of time).
	for _, day := range []float64{0.5, 2, 4, 25} {
		w.kill(w.guild1, w.server1a, bP, victim, w.day(day), killOpt{})
	}
	// The leader's kill on day 10 (open period).
	w.kill(w.guild1, w.server1a, leadP, victim, w.day(10), killOpt{})

	st := w.fresh(f)
	if got := st.member(w.discordID(a)).Kills; got != 2 {
		t.Fatalf("A must count only period 1 and period 2 kills (days 2 and 6), got %d", got)
	}
	if got := st.member(w.discordID(b)).Kills; got != 1 {
		t.Fatalf("B must count only the kill made while a member (day 2), got %d", got)
	}
	if got := st.member(w.discordID(lead)).Kills; got != 1 {
		t.Fatalf("leader kills %d", got)
	}
	if st.Summary.Kills != 4 {
		t.Fatalf("faction total = 2 + 1 + 1, got %d", st.Summary.Kills)
	}
	if bm := st.member(w.discordID(b)); bm.Status != StatusFormer || bm.MemberID != nil || bm.Role != nil {
		t.Fatalf("B is a FORMER member whose counted kill remains in the totals: %+v", bm)
	}
	if am := st.member(w.discordID(a)); am.Status != StatusActive || am.Role == nil || *am.Role != "MEMBER" {
		t.Fatalf("A is an active MEMBER: %+v", am)
	}

	// Boundaries are half-open: a kill exactly AT joined_at counts, exactly at left_at does not.
	edge := w.day(20)
	w.periods(f, b, span{From: edge, To: edge.Add(time.Hour)})
	w.kill(w.guild1, w.server1a, bP, victim, edge, killOpt{})
	w.kill(w.guild1, w.server1a, bP, victim, edge.Add(time.Hour), killOpt{})
	if got := w.fresh(f).member(w.discordID(b)).Kills; got != 1 {
		t.Fatalf("joined_at is inclusive and left_at exclusive: want 1, got %d", got)
	}
}

func TestRealJoinAndLeaveRecordPeriodsAndOnlyLaterKillsCount(t *testing.T) {
	w := newWorld(t)
	lead, _ := w.linked(w.guild1, "Lead")
	a, aP := w.linked(w.guild1, "Alpha")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Real Flow", "RF")
	ms := time.Millisecond
	period := func(n int) (joined time.Time, left *time.Time) {
		rows, err := w.db.Pool.Query(w.ctx, `SELECT joined_at, left_at FROM hub_faction_membership_history WHERE faction_id=$1 AND user_id=$2 ORDER BY joined_at OFFSET $3 LIMIT 1`, f.ID, a, n)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("no membership period #%d", n)
		}
		if err := rows.Scan(&joined, &left); err != nil {
			t.Fatal(err)
		}
		return
	}

	// A kill made an hour BEFORE joining never counts; one made just after joining does.
	w.kill(w.guild1, w.server1a, aP, victim, time.Now().UTC().Add(-time.Hour), killOpt{})
	mID := w.join(f, lead, a)
	j1, left1 := period(0)
	if left1 != nil {
		t.Fatal("joining opens an OPEN period")
	}
	w.kill(w.guild1, w.server1a, aP, victim, j1.Add(ms), killOpt{})
	if got := w.fresh(f).member(w.discordID(a)).Kills; got != 1 {
		t.Fatalf("only the kill after joining counts, got %d", got)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_membership_history WHERE faction_id=$1 AND user_id=$2 AND left_at IS NULL`, f.ID, a); n != 1 {
		t.Fatalf("joining opens exactly one period, got %d", n)
	}

	// Leaving closes the period (kept forever). A kill just AFTER leaving does not count.
	time.Sleep(30 * ms)
	if _, err := w.hub.LeaveFaction(w.ctx, f.OrganizationID, f.InstallationID, f.ID, a); err != nil {
		t.Fatal(err)
	}
	_, left := period(0)
	if left == nil {
		t.Fatal("leaving must close the period, not delete it")
	}
	w.kill(w.guild1, w.server1a, aP, victim, left.Add(ms), killOpt{})
	st := w.fresh(f)
	if m := st.member(w.discordID(a)); m == nil || m.Kills != 1 || m.Status != StatusFormer {
		t.Fatalf("after leaving: the earlier kill stays, the later one does not: %+v", m)
	}

	// Rejoining opens a NEW period; the kill made in the gap stays excluded, one made after rejoining counts.
	time.Sleep(30 * ms)
	mID2 := w.join(f, lead, a)
	if mID2 == mID {
		t.Fatal("a rejoin is a new membership")
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_membership_history WHERE faction_id=$1 AND user_id=$2`, f.ID, a); n != 2 {
		t.Fatalf("two periods expected after rejoin, got %d", n)
	}
	j2, _ := period(1)
	w.kill(w.guild1, w.server1a, aP, victim, j2.Add(ms), killOpt{})
	if got := w.fresh(f).member(w.discordID(a)).Kills; got != 2 {
		t.Fatalf("period 1 kill + the rejoin-period kill = 2 (the gap kill is excluded), got %d", got)
	}

	// Removal by the leader closes the period too.
	mems, _, _ := w.hub.Members(w.ctx, f.OrganizationID, f.InstallationID, f.ID, 10)
	for _, m := range mems {
		if m.User.ID == a {
			if _, err := w.hub.RemoveMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, m.ID, lead); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_membership_history WHERE faction_id=$1 AND user_id=$2 AND left_at IS NULL`, f.ID, a); n != 0 {
		t.Fatalf("removal must close the open period, %d still open", n)
	}
}

func TestMigrationBackfillsCurrentMembersWithTheirRealJoinTime(t *testing.T) {
	w := newWorld(t)
	lead, _ := w.linked(w.guild1, "Lead")
	a, _ := w.linked(w.guild1, "Alpha")
	f := w.faction(w.inst1, lead, "Backfilled", "BF")
	w.join(f, lead, a)
	// Simulate the pre-Phase-5 state (members without history) and re-run the migration's backfill statement.
	w.exec(`DELETE FROM hub_faction_membership_history WHERE faction_id=$1`, f.ID)
	w.exec(`INSERT INTO hub_faction_membership_history(organization_id, installation_id, faction_id, user_id, player_identity_id, joined_at)
SELECT f.organization_id, m.installation_id, m.faction_id, m.user_id, m.player_id, m.joined_at
FROM hub_faction_members m JOIN hub_factions f ON f.id = m.faction_id
WHERE m.faction_id=$1 AND NOT EXISTS (SELECT 1 FROM hub_faction_membership_history h WHERE h.faction_id = m.faction_id AND h.user_id = m.user_id AND h.left_at IS NULL)`, f.ID)
	var same int
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM hub_faction_membership_history h JOIN hub_faction_members m ON m.faction_id=h.faction_id AND m.user_id=h.user_id WHERE h.faction_id=$1 AND h.joined_at=m.joined_at AND h.left_at IS NULL`, f.ID).Scan(&same); err != nil || same != 2 {
		t.Fatalf("backfill must use the members' real joined_at: %d %v", same, err)
	}
}

// --- kills, deaths, K/D, headshots, longshots, streaks -------------------------------------------------

func TestCombatFiguresFromRealPersistedEvents(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Lead")
	m, mP := w.linked(w.guild1, "Medic")
	mate, mateP := w.linked(w.guild1, "Mate")
	f := w.faction(w.inst1, lead, "Combat Cell", "CC")
	w.join(f, lead, m)
	w.join(f, lead, mate)
	for _, u := range []int64{lead, m, mate} {
		w.periods(f, u, span{From: w.day(0)})
	}
	v := [6]int64{}
	for i := range v {
		v[i] = w.outsider(w.guild1, "Victim")
	}
	at := func(min int) time.Time { return w.day(1).Add(time.Duration(min) * time.Minute) }

	// Leader: k, k(headshot), k(longshot 150 m) | death | k, k, k | killed by an outsider | k
	w.kill(w.guild1, w.server1a, leadP, v[0], at(1), killOpt{})
	w.kill(w.guild1, w.server1a, leadP, v[1], at(2), killOpt{headshot: true})
	w.kill(w.guild1, w.server1a, leadP, v[2], at(3), killOpt{longshot: true, distance: 150})
	w.death(w.guild1, w.server1a, leadP, at(4), "UNKNOWN")
	w.kill(w.guild1, w.server1a, leadP, v[3], at(5), killOpt{})
	w.kill(w.guild1, w.server1a, leadP, v[4], at(6), killOpt{})
	w.kill(w.guild1, w.server1a, leadP, v[5], at(7), killOpt{})
	w.kill(w.guild1, w.server1a, w.outsider(w.guild1, "Killer"), leadP, at(8), killOpt{}) // a PvP kill OF the leader breaks the streak
	w.kill(w.guild1, w.server1a, leadP, v[0], at(9), killOpt{headshot: true, longshot: true, distance: 210})
	// Medic: two kills and no deaths (K/D must not divide by zero).
	w.kill(w.guild1, w.server1a, mP, v[0], at(10), killOpt{})
	w.kill(w.guild1, w.server1a, mP, v[1], at(11), killOpt{})
	// A team kill (leader kills a fellow member) is NOT a counted kill, but the victim's death is.
	w.kill(w.guild1, w.server1a, leadP, mateP, at(12), killOpt{})
	w.death(w.guild1, w.server1a, mateP, at(12), "SUICIDE") // any death row counts (the project's convention)

	st := w.fresh(f)
	l := st.member(w.discordID(lead))
	if l.Kills != 7 || l.Headshots != 2 || l.Longshots != 2 || l.Deaths != 1 {
		t.Fatalf("leader figures: %+v", l)
	}
	if l.KDRatio != 7 { // 7 kills / 1 death
		t.Fatalf("K/D = kills/deaths = 7.00, got %v", l.KDRatio)
	}
	if l.BestKillStreak != 3 || l.CurrentKillStreak != 1 {
		t.Fatalf("streaks: best 3 (before the death and again after it), current 1 (after being killed): best=%d current=%d", l.BestKillStreak, l.CurrentKillStreak)
	}
	md := st.member(w.discordID(m))
	if md.Kills != 2 || md.Deaths != 0 || md.KDRatio != 2 {
		t.Fatalf("no deaths: K/D is the kill count, never infinity or NaN: %+v", md)
	}
	mt := st.member(w.discordID(mate))
	if mt.Kills != 0 || mt.Deaths != 1 || mt.KDRatio != 0 {
		t.Fatalf("a teammate's death still counts as a death: %+v", mt)
	}
	sum := st.Summary
	if sum.Kills != 9 || sum.Deaths != 2 || sum.Headshots != 2 || sum.Longshots != 2 || sum.BestKillStreak != 3 || sum.CurrentKillStreak != 2 {
		t.Fatalf("summary: %+v", sum)
	}
	// The medic's own streak (2, no death) is the largest CURRENT streak among active members.
	if sum.KDRatio != KDRatio(9, 2) || sum.KDRatio != 4.5 {
		t.Fatalf("faction K/D: %v", sum.KDRatio)
	}
	// Deterministic ranking: kills DESC, deaths ASC, then member id.
	order := []string{w.discordID(lead), w.discordID(m), w.discordID(mate)}
	for i, want := range order {
		if st.MemberContributions[i].DiscordUserID != want {
			t.Fatalf("ranking position %d: got %s want %s", i, st.MemberContributions[i].DiscordUserID, want)
		}
	}
	if sum.MemberCount != 3 || sum.LinkedMemberCount != 3 || sum.TrackingSince == nil {
		t.Fatalf("membership figures: %+v", sum)
	}
}

func TestZeroFigureFactionIsAllZerosNeverNaN(t *testing.T) {
	w := newWorld(t)
	lead, _ := w.linked(w.guild1, "Lone")
	f := w.faction(w.inst1, lead, "Quiet Corner", "QC")
	st := w.stats(f)
	s := st.Summary
	if s.Kills != 0 || s.Deaths != 0 || s.KDRatio != 0 || s.Headshots != 0 || s.Longshots != 0 || s.BestKillStreak != 0 || s.CurrentKillStreak != 0 ||
		s.BountiesClaimed != 0 || s.BountyValueClaimed != 0 || s.MemberCount != 1 {
		t.Fatalf("summary of a faction with no combat: %+v", s)
	}
	if len(st.MemberContributions) != 1 {
		t.Fatalf("the leader is listed: %+v", st.MemberContributions)
	}
}

func TestKDRatioConvention(t *testing.T) {
	for _, c := range []struct {
		k, d int64
		want float64
	}{{0, 0, 0}, {5, 0, 5}, {1, 3, 0.33}, {7, 2, 3.5}, {10, 3, 3.33}, {2, 3, 0.67}, {100, 1, 100}} {
		if got := KDRatio(c.k, c.d); got != c.want {
			t.Errorf("KDRatio(%d,%d) = %v, want %v", c.k, c.d, got, c.want)
		}
	}
}

// --- dedupe ------------------------------------------------------------------------------------------------

func TestReplayedKillCannotCountTwice(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Lead")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "No Doubles", "ND")
	w.periods(f, lead, span{From: w.day(0)})
	fp := "replay-fingerprint"
	insert := func() error {
		_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, event_time) VALUES($1,$2,'s',$3,$4,$5,$6)`,
			w.guild1, w.server1a, fp, leadP, victim, w.day(2))
		return err
	}
	if err := insert(); err != nil {
		t.Fatal(err)
	}
	if err := insert(); err == nil {
		t.Fatal("the (guild, fingerprint) unique key must reject a replayed kill - the source of the dedupe stats inherit")
	}
	if got := w.fresh(f).Summary.Kills; got != 1 {
		t.Fatalf("a replayed kill must count once, got %d", got)
	}
	// Re-reading (an idempotent query) never changes a figure either.
	if got := w.fresh(f).Summary.Kills; got != 1 {
		t.Fatalf("recomputing is stable, got %d", got)
	}
}

// --- bounties ----------------------------------------------------------------------------------------------

func TestBountyContributionFromClaimedBountyRows(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Hunter")
	mate, mateP := w.linked(w.guild1, "Mate")
	f := w.faction(w.inst1, lead, "Bounty Board", "BB")
	w.join(f, lead, mate)
	w.periods(f, lead, span{From: w.day(0)})
	w.periods(f, mate, span{From: w.day(5)}) // joins day 5
	target := w.outsider(w.guild1, "Wanted")

	// One kill claims TWO stacked bounties (100 + 50) -> 2 claims, value 150.
	k1 := w.kill(w.guild1, w.server1a, leadP, target, w.day(2), killOpt{})
	w.bounty(w.guild1, w.server1a, target, leadP, k1, 100, w.day(2))
	w.bounty(w.guild1, w.server1a, target, leadP, k1, 50, w.day(2))
	// A claim by the mate BEFORE they joined the faction: not theirs to bring.
	k2 := w.kill(w.guild1, w.server1a, mateP, target, w.day(3), killOpt{})
	w.bounty(w.guild1, w.server1a, target, mateP, k2, 999, w.day(3))
	// A claim after joining counts.
	k3 := w.kill(w.guild1, w.server1a, mateP, target, w.day(6), killOpt{})
	w.bounty(w.guild1, w.server1a, target, mateP, k3, 25, w.day(6))
	// An unclaimed (ACTIVE) bounty is not a claim.
	w.exec(`INSERT INTO bounties(guild_id, server_id, target_player_id, created_by_type, status, reward_points, starts_at) VALUES($1,$2,$3,'MANUAL','ACTIVE',500,$4)`, w.guild1, w.server1a, target, w.day(1))
	// A bounty claimed on the OTHER server of the same guild does not count.
	k4 := w.kill(w.guild1, w.server1b, leadP, target, w.day(7), killOpt{})
	w.bounty(w.guild1, w.server1b, target, leadP, k4, 777, w.day(7))

	st := w.fresh(f)
	l, m := st.member(w.discordID(lead)), st.member(w.discordID(mate))
	if l.BountiesClaimed != 2 || l.BountyValueClaimed != 150 {
		t.Fatalf("leader bounties: %+v", l)
	}
	if m.BountiesClaimed != 1 || m.BountyValueClaimed != 25 {
		t.Fatalf("mate bounties (only the claim made while a member): %+v", m)
	}
	if st.Summary.BountiesClaimed != 3 || st.Summary.BountyValueClaimed != 175 {
		t.Fatalf("faction bounties: %+v", st.Summary)
	}
	// Replaying the claim cannot count it again: the claim is the bounty ROW (claimed once) and the
	// kill is unique per (guild, fingerprint).
	if got := w.fresh(f).Summary.BountiesClaimed; got != 3 {
		t.Fatalf("recompute is stable: %d", got)
	}
	// A friendly claim (the hunter killed a fellow member) is not a counted kill, so no bounty credit.
	k5 := w.kill(w.guild1, w.server1a, leadP, mateP, w.day(8), killOpt{})
	w.bounty(w.guild1, w.server1a, mateP, leadP, k5, 300, w.day(8))
	if got := w.fresh(f).Summary.BountiesClaimed; got != 3 {
		t.Fatalf("a bounty claimed by killing a teammate must not count, got %d", got)
	}
}

// --- identity ----------------------------------------------------------------------------------------------

func TestOnlyVerifiedLinkedIdentityIsAttributed(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "SharedName")
	unlinked := w.user() // a member with no gamertag link at all
	pending := w.user()  // a link that was never verified
	pendingP := w.player(w.guild1, "dz-pending-"+time_(w), "PendingGuy")
	w.link(w.guild1, pendingP, pending, "PENDING")
	f := w.faction(w.inst1, lead, "Identity Gate", "IG")
	w.join(f, lead, unlinked)
	w.join(f, lead, pending)
	for _, u := range []int64{lead, unlinked, pending} {
		w.periods(f, u, span{From: w.day(0)})
	}
	victim := w.outsider(w.guild1, "Victim")

	// Kills by a player whose display NAME equals the unlinked member's username: never attributed by name.
	sameName := w.player(w.guild1, "dz-impostor-"+time_(w), "user"+"X")
	_ = sameName
	var uname string
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT discord_username FROM app_users WHERE id=$1`, unlinked).Scan(&uname); err != nil {
		t.Fatal(err)
	}
	namesake := w.player(w.guild1, "dz-namesake-"+time_(w), uname)
	w.kill(w.guild1, w.server1a, namesake, victim, w.day(2), killOpt{})
	w.kill(w.guild1, w.server1a, pendingP, victim, w.day(2), killOpt{}) // PENDING link: not verified
	w.kill(w.guild1, w.server1a, leadP, victim, w.day(2), killOpt{})    // the one verified identity

	st := w.fresh(f)
	if st.Summary.Kills != 1 {
		t.Fatalf("only the verified identity's kill counts, got %d", st.Summary.Kills)
	}
	for _, u := range []int64{unlinked, pending} {
		m := st.member(w.discordID(u))
		if m.Identity != IdentityUnlinked || m.StatsEligible || m.Kills != 0 || m.Deaths != 0 || m.KDRatio != 0 || m.Gamertag != nil {
			t.Fatalf("an unlinked member has no attributed figures and a clear eligibility state: %+v", m)
		}
	}
	if l := st.member(w.discordID(lead)); l.Identity != IdentityLinked || !l.StatsEligible || l.Kills != 1 || l.Gamertag == nil || *l.Gamertag != "SharedName" {
		t.Fatalf("linked member: %+v", l)
	}
	if st.Summary.MemberCount != 3 || st.Summary.LinkedMemberCount != 1 {
		t.Fatalf("linked vs total members: %+v", st.Summary)
	}

	// Verifying the link later attributes the member's kills made while they were in the faction.
	w.exec(`UPDATE player_links SET status='VERIFIED' WHERE player_id=$1`, pendingP)
	st = w.fresh(f)
	if m := st.member(w.discordID(pending)); m.Kills != 1 || m.Identity != IdentityLinked {
		t.Fatalf("once verified, the kills made during membership are attributed: %+v", m)
	}
}

func time_(w *world) string {
	return time.Now().Format("150405.000000") + "-" + string(rune('a'+w.next()%26))
}

func TestSameGamertagInAnotherGuildIsNotTheSameIdentity(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Ghost")
	f := w.faction(w.inst1, lead, "Context Matters", "CM")
	w.periods(f, lead, span{From: w.day(0)})
	victim1, victim2 := w.outsider(w.guild1, "V1"), w.outsider(w.guild2, "V2")
	// Another guild has a player with the very same in-game name and DayZ id, linked to a DIFFERENT user.
	other, _ := w.user(), 0
	twin := w.player(w.guild2, "dz-twin-"+time_(w), "Ghost")
	w.link(w.guild2, twin, other, "VERIFIED")
	w.kill(w.guild2, w.server2, twin, victim2, w.day(2), killOpt{}) // the twin's kill in guild 2
	w.kill(w.guild1, w.server1a, leadP, victim1, w.day(2), killOpt{})
	if got := w.fresh(f).Summary.Kills; got != 1 {
		t.Fatalf("a same-named player in another guild must not add kills, got %d", got)
	}
}

// --- multi-server and tenant isolation ---------------------------------------------------------------------

func TestServersOfOneGuildStayIsolated(t *testing.T) {
	w := newWorld(t)
	user, player := w.linked(w.guild1, "Roamer") // one identity, one guild, TWO installations (two servers)
	leaderB, _ := w.linked(w.guild1, "OtherLeader")
	fa := w.faction(w.inst1, user, "Server A Clan", "SA")
	fb := w.faction(w.inst1b, leaderB, "Server B Clan", "SB")
	w.join(fb, leaderB, user) // the same user is also in a faction on the other installation
	w.periods(fa, user, span{From: w.day(0)})
	w.periods(fb, user, span{From: w.day(0)})
	w.periods(fb, leaderB, span{From: w.day(0)})
	victim := w.outsider(w.guild1, "Victim")

	for i := 0; i < 3; i++ {
		w.kill(w.guild1, w.server1a, player, victim, w.day(2+float64(i)), killOpt{})
	}
	for i := 0; i < 5; i++ {
		w.kill(w.guild1, w.server1b, player, victim, w.day(2+float64(i)), killOpt{headshot: true})
	}
	w.death(w.guild1, w.server1b, player, w.day(9), "UNKNOWN")
	sa, sb := w.fresh(fa).Summary, w.fresh(fb).Summary
	if sa.Kills != 3 || sa.Headshots != 0 || sa.Deaths != 0 {
		t.Fatalf("the faction on server A sees only server A: %+v", sa)
	}
	if sb.Kills != 5 || sb.Headshots != 5 || sb.Deaths != 1 {
		t.Fatalf("the faction on server B sees only server B: %+v", sb)
	}
	// A kill whose server is unknown (legacy NULL server_id) is not provably this faction's: not counted.
	w.exec(`INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, event_time) VALUES($1,NULL,'s',$2,$3,$4,$5)`, w.guild1, "legacy-"+time_(w), player, victim, w.day(4))
	if got := w.fresh(fa).Summary.Kills; got != 3 {
		t.Fatalf("a kill with no server is not counted, got %d", got)
	}
}

func TestStatsAreTenantScoped(t *testing.T) {
	w := newWorld(t)
	lead, _ := w.linked(w.guild1, "Lead")
	f := w.faction(w.inst1, lead, "Tenant Locked", "TL")
	lead2, _ := w.linked(w.guild2, "Lead2")
	f2 := w.faction(w.inst2, lead2, "Other Tenant", "OT")

	type probe struct {
		name         string
		org, inst, f int64
	}
	for _, p := range []probe{
		{"other org, real installation", w.org2, w.inst1, f.ID},
		{"real org, other installation of the org", w.org1, w.inst1b, f.ID},
		{"both wrong", w.org2, w.inst2, f.ID},
		{"other tenant's faction id under my path", w.org1, w.inst1, f2.ID},
	} {
		if _, err := w.svc.GetFactionStats(w.ctx, p.org, p.inst, p.f); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("stats %s: want ErrNotFound, got %v", p.name, err)
		}
		if _, err := w.svc.GetFactionRecentActivity(w.ctx, p.org, p.inst, p.f, 10, nil); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("activity %s: want ErrNotFound, got %v", p.name, err)
		}
		if _, err := w.svc.GetFactionAchievements(w.ctx, p.org, p.inst, p.f); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("achievements %s: want ErrNotFound, got %v", p.name, err)
		}
		if _, err := w.store.Scope(w.ctx, p.org, p.inst, p.f); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("scope %s: want ErrNotFound, got %v", p.name, err)
		}
	}
	// A warm cache never serves another tenant: prime it for the real triple, then ask with a wrong one.
	if _, err := w.svc.GetFactionStats(w.ctx, w.org1, w.inst1, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.GetFactionStats(w.ctx, w.org2, w.inst1, f.ID); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("cache must be keyed by organization + installation + faction: %v", err)
	}
	if _, err := w.svc.GetFactionStats(w.ctx, w.org1, w.inst1b, f.ID); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("cache must be keyed by installation: %v", err)
	}
}

// --- streak details ----------------------------------------------------------------------------------------

func TestStreaksNeverCarryAcrossAMembershipGap(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Streaker")
	f := w.faction(w.inst1, lead, "Streak Keepers", "SK")
	victim := w.outsider(w.guild1, "Victim")
	// Two membership periods; 4 kills in the first, 4 in the second; the player died in the gap
	// (a death OUTSIDE any period is not the faction's death). A single 8-streak would be a lie.
	w.periods(f, lead, span{From: w.day(0), To: w.day(2)}, span{From: w.day(4)})
	for i := 0; i < 4; i++ {
		w.kill(w.guild1, w.server1a, leadP, victim, w.day(1).Add(time.Duration(i)*time.Minute), killOpt{})
		w.kill(w.guild1, w.server1a, leadP, victim, w.day(5).Add(time.Duration(i)*time.Minute), killOpt{})
	}
	w.death(w.guild1, w.server1a, leadP, w.day(3), "UNKNOWN")
	st := w.fresh(f)
	if m := st.member(w.discordID(lead)); m.Kills != 8 || m.BestKillStreak != 4 || m.CurrentKillStreak != 4 || m.Deaths != 0 {
		t.Fatalf("each period is its own streak: %+v", m)
	}
}
