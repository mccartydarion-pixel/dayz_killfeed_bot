//go:build integration

package factionstats

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// bulkKills inserts n kills by killer on (guild, server) one second apart starting at start:
// every 4th is a headshot and every 10th a longshot, so 100 kills = 25 headshots + 10 longshots.
func (w *world) bulkKills(guild, server, killer, victim int64, start time.Time, from, to int) {
	w.t.Helper()
	w.exec(`INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, longshot, distance, event_time)
SELECT $1,$2,'bulk',$3 || g,$4,$5,(g % 4 = 0),(g % 10 = 0),50,$6::timestamptz + (g * interval '1 second')
FROM generate_series($7::int,$8::int) g`, guild, server, fmt.Sprintf("bulk-%d-%d-", w.suffix, w.next()), killer, victim, start, from, to)
}

func unlockedKeys(t *testing.T, items []Achievement) map[string]Achievement {
	t.Helper()
	out := map[string]Achievement{}
	for _, a := range items {
		if a.Unlocked {
			out[a.Key] = a
		}
	}
	return out
}

func (w *world) achievements(f *repository.HubFaction) []Achievement {
	w.t.Helper()
	w.svc.Invalidate(f.OrganizationID, f.InstallationID, f.ID) // tests write rows directly; production invalidates through NotifyCombat
	items, err := w.svc.GetFactionAchievements(w.ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return items
}

func (w *world) evaluate(f *repository.HubFaction) []string {
	w.t.Helper()
	sc, err := w.store.Scope(w.ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	won, err := w.svc.Evaluate(w.ctx, sc)
	if err != nil {
		w.t.Fatal(err)
	}
	return won
}

func TestAchievementThresholdsBeforeExactAndAfter(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Killer")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Threshold Club", "TC")
	w.periods(f, lead, span{From: w.day(0)})

	// No kills: nothing but the always-true conditions is unlocked, and progress is real.
	items := w.achievements(f)
	if got := unlockedKeys(t, items); len(got) != 0 {
		t.Fatalf("a new faction has no combat achievements: %v", got)
	}
	for _, a := range items {
		if a.Key == "KILLS_100" && (a.Progress != 0 || a.Target != 100 || a.Unit != "kills") {
			t.Fatalf("progress/target: %+v", a)
		}
	}

	// 1 kill -> FIRST_BLOOD only.
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 1, 1)
	if won := w.evaluate(f); len(won) != 1 || won[0] != "FIRST_BLOOD" {
		t.Fatalf("one kill unlocks only FIRST_BLOOD, got %v", won)
	}

	// 99 kills: still below KILLS_100 / HEADHUNTERS(25 headshots) / LONG_RANGE(10); KILLING_MACHINE (streak 10) is met.
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 2, 99)
	items = w.achievements(f)
	got := unlockedKeys(t, items)
	for _, k := range []string{"KILLS_100", "KILLS_500", "KILLS_1000", "HEADHUNTERS", "LONG_RANGE"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s must not be unlocked at 99 kills (24 headshots, 9 longshots)", k)
		}
	}
	if _, ok := got["KILLING_MACHINE"]; !ok {
		t.Fatalf("99 kills without dying is a 99 streak, which proves KILLING_MACHINE: %v", got)
	}
	for _, a := range items {
		if a.Key == "KILLS_100" && a.Progress != 99 {
			t.Fatalf("progress must be the real count: %+v", a)
		}
		if a.Key == "HEADHUNTERS" && a.Progress != 24 {
			t.Fatalf("headshot progress: %+v", a)
		}
	}

	// The 100th kill is exactly the threshold: KILLS_100, HEADHUNTERS (25th headshot) and LONG_RANGE (10th longshot).
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 100, 100)
	won := w.evaluate(f)
	if strings.Join(sortedCopy(won), ",") != "HEADHUNTERS,KILLS_100,LONG_RANGE" {
		t.Fatalf("exactly at the threshold: %v", won)
	}
	// unlocked_at is when it was REALLY earned: the 100th counted kill.
	for _, a := range w.achievements(f) {
		if a.Key == "KILLS_100" {
			want := w.day(1).Add(100 * time.Second).UTC().Format(time.RFC3339)
			if a.UnlockedAt == nil || *a.UnlockedAt != want {
				t.Fatalf("KILLS_100 unlockedAt = %v, want the 100th kill's time %s", a.UnlockedAt, want)
			}
		}
		if a.Key == "FIRST_BLOOD" {
			want := w.day(1).Add(1 * time.Second).UTC().Format(time.RFC3339)
			if a.UnlockedAt == nil || *a.UnlockedAt != want {
				t.Fatalf("FIRST_BLOOD unlockedAt = %v, want %s", a.UnlockedAt, want)
			}
		}
	}

	// Past the threshold: nothing new, nothing duplicated.
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 101, 130)
	if won := w.evaluate(f); len(won) != 0 {
		t.Fatalf("already-unlocked achievements are never re-unlocked: %v", won)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, f.ID); n != 5 {
		t.Fatalf("FIRST_BLOOD, KILLS_100, HEADHUNTERS, LONG_RANGE, KILLING_MACHINE: %d rows", n)
	}
	// A locked achievement's progress is capped at its target once unlocked, real otherwise.
	for _, a := range w.achievements(f) {
		if a.Key == "KILLS_500" && (a.Unlocked || a.Progress != 130) {
			t.Fatalf("KILLS_500 progress: %+v", a)
		}
		if a.Key == "KILLS_100" && a.Progress != 100 {
			t.Fatalf("an unlocked achievement shows full progress: %+v", a)
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func TestStreakBountyMembershipAndAgeAchievements(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Ace")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Milestones", "MS")
	w.periods(f, lead, span{From: w.day(0)})

	// KILLING_MACHINE needs a streak of 10: 9 + a death + 9 is NOT a streak of 18 (nor 10).
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 1, 9)
	w.death(w.guild1, w.server1a, leadP, w.day(1).Add(30*time.Second), "UNKNOWN")
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(2), 1, 9)
	if got := unlockedKeys(t, w.achievements(f)); got["KILLING_MACHINE"].Unlocked {
		t.Fatal("two streaks of 9 split by a death are not a 10-streak")
	}
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(2), 10, 10)
	if got := unlockedKeys(t, w.achievements(f)); !got["KILLING_MACHINE"].Unlocked {
		t.Fatal("a 10th kill in a row proves the streak")
	}

	// BOUNTY_HUNTERS at 5 claimed bounties: 4 -> no, 5 -> yes.
	target := w.outsider(w.guild1, "Wanted")
	for i := 0; i < 4; i++ {
		k := w.kill(w.guild1, w.server1a, leadP, target, w.day(3).Add(time.Duration(i)*time.Minute), killOpt{})
		w.bounty(w.guild1, w.server1a, target, leadP, k, 10, w.day(3).Add(time.Duration(i)*time.Minute))
	}
	if got := unlockedKeys(t, w.achievements(f)); got["BOUNTY_HUNTERS"].Unlocked {
		t.Fatal("4 bounties are below the target")
	}
	k := w.kill(w.guild1, w.server1a, leadP, target, w.day(4), killOpt{})
	w.bounty(w.guild1, w.server1a, target, leadP, k, 10, w.day(4))
	if got := unlockedKeys(t, w.achievements(f)); !got["BOUNTY_HUNTERS"].Unlocked {
		t.Fatal("the 5th claimed bounty unlocks BOUNTY_HUNTERS")
	}

	// FULL_SQUAD at 5 members: 4 -> no, 5 -> yes.
	for i := 0; i < 3; i++ {
		u := w.user()
		w.join(f, lead, u)
	}
	if got := unlockedKeys(t, w.achievements(f)); got["FULL_SQUAD"].Unlocked {
		t.Fatal("4 members is not a full squad")
	}
	w.join(f, lead, w.user())
	if got := unlockedKeys(t, w.achievements(f)); !got["FULL_SQUAD"].Unlocked {
		t.Fatal("5 members is a full squad")
	}
	// It stays unlocked when members leave (an unlock is an earned fact).
	mems, _, _ := w.hub.Members(w.ctx, f.OrganizationID, f.InstallationID, f.ID, 10)
	for _, m := range mems[1:] {
		if _, err := w.hub.RemoveMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, m.ID, lead); err != nil {
			t.Fatal(err)
		}
	}
	if got := unlockedKeys(t, w.achievements(f)); !got["FULL_SQUAD"].Unlocked {
		t.Fatal("FULL_SQUAD must stay unlocked")
	}

	// VETERAN_FACTION: 29 days no, 31 days yes, unlockedAt = created_at + 30 days.
	w.exec(`UPDATE hub_factions SET created_at = NOW() - interval '29 days' WHERE id=$1`, f.ID)
	if got := unlockedKeys(t, w.achievements(f)); got["VETERAN_FACTION"].Unlocked {
		t.Fatal("29 days is not 30")
	}
	w.exec(`UPDATE hub_factions SET created_at = NOW() - interval '31 days' WHERE id=$1`, f.ID)
	got := unlockedKeys(t, w.achievements(f))
	if !got["VETERAN_FACTION"].Unlocked {
		t.Fatal("31 days qualifies")
	}
	var created time.Time
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT created_at FROM hub_factions WHERE id=$1`, f.ID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if want := created.Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339); got["VETERAN_FACTION"].UnlockedAt == nil || *got["VETERAN_FACTION"].UnlockedAt != want {
		t.Fatalf("VETERAN unlockedAt = %v, want %s", got["VETERAN_FACTION"].UnlockedAt, want)
	}
}

func TestConcurrentEvaluationUnlocksExactlyOnce(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Racer")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Photo Finish", "PF")
	w.periods(f, lead, span{From: w.day(0)})
	w.bulkKills(w.guild1, w.server1a, leadP, victim, w.day(1), 1, 100)
	sc, err := w.store.Scope(w.ctx, f.OrganizationID, f.InstallationID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	{
		const workers = 12
		start := make(chan struct{})
		results := make([][]string, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = w.svc.Evaluate(w.ctx, sc)
			}(i)
		}
		close(start)
		wg.Wait()
		winners := map[string]int{}
		for i, e := range errs {
			if e != nil {
				t.Fatalf("evaluator %d: %v", i, e)
			}
			for _, k := range results[i] {
				winners[k]++
			}
		}
		for k, n := range winners {
			if n != 1 {
				t.Fatalf("%s was reported unlocked by %d concurrent evaluators, want exactly 1", k, n)
			}
		}
		if len(winners) != 5 {
			t.Fatalf("the five earned achievements must each be won once: %v", winners)
		}
		if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, f.ID); n != 5 {
			t.Fatalf("duplicate unlock rows: %d", n)
		}
		// The database itself refuses a duplicate.
		if _, err := w.db.Pool.Exec(w.ctx, `INSERT INTO hub_faction_achievement_unlocks(organization_id, installation_id, faction_id, achievement_key, unlocked_at) VALUES($1,$2,$3,'FIRST_BLOOD',NOW())`, f.OrganizationID, f.InstallationID, f.ID); err == nil {
			t.Fatal("the unique (faction, achievement) key must reject a duplicate")
		}
	}
}

func TestHistoricalBackfillIsIdempotentAndSilent(t *testing.T) {
	w := newWorld(t)
	qual, qualP := w.linked(w.guild1, "Veteran")
	quiet, _ := w.linked(w.guild1, "Quiet")
	victim := w.outsider(w.guild1, "Victim")
	fq := w.faction(w.inst1, qual, "Already Qualified", "AQ")
	fn := w.faction(w.inst1b, quiet, "Never Qualified", "NQ")
	w.periods(fq, qual, span{From: w.day(0)})
	w.bulkKills(w.guild1, w.server1a, qualP, victim, w.day(1), 1, 120) // qualified long before any evaluator ran
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, fq.ID); n != 0 {
		t.Fatal("nothing has been evaluated yet")
	}
	factions, unlocked, err := w.svc.ReconcileAll(w.ctx, 2) // tiny batches exercise the paging
	if err != nil {
		t.Fatal(err)
	}
	if factions < 2 || unlocked < 5 {
		t.Fatalf("the backfill must evaluate every faction and unlock what already qualifies: %d %d", factions, unlocked)
	}
	rowsAfterFirst := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, fq.ID)
	if rowsAfterFirst != 5 { // FIRST_BLOOD, KILLS_100, HEADHUNTERS, LONG_RANGE, KILLING_MACHINE
		t.Fatalf("expected 5 unlocks for the qualified faction, got %d", rowsAfterFirst)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, fn.ID); n != 0 {
		t.Fatalf("a faction that has not earned anything unlocks nothing, got %d", n)
	}
	// Running it again neither double-unlocks nor changes the recorded moments.
	var before time.Time
	if err := w.db.Pool.QueryRow(w.ctx, `SELECT unlocked_at FROM hub_faction_achievement_unlocks WHERE faction_id=$1 AND achievement_key='KILLS_100'`, fq.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, unlocked2, err := w.svc.ReconcileAll(w.ctx, 50); err != nil || unlocked2 != 0 {
		t.Fatalf("a second reconcile must unlock nothing new: %d %v", unlocked2, err)
	}
	var after time.Time
	_ = w.db.Pool.QueryRow(w.ctx, `SELECT unlocked_at FROM hub_faction_achievement_unlocks WHERE faction_id=$1 AND achievement_key='KILLS_100'`, fq.ID).Scan(&after)
	if !before.Equal(after) || w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, fq.ID) != rowsAfterFirst {
		t.Fatal("reconciliation is idempotent")
	}
}

func TestKillEventQueueEvaluatesAffectedFactionsOnly(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Live")
	other, _ := w.linked(w.guild1, "Bystander")
	victim := w.outsider(w.guild1, "Victim")
	f := w.faction(w.inst1, lead, "Event Driven", "ED")
	fo := w.faction(w.inst1b, other, "Bystanders", "BY")
	w.periods(f, lead, span{From: w.day(0)})
	k := w.kill(w.guild1, w.server1a, leadP, victim, time.Now().UTC(), killOpt{})
	_ = k
	// The killfeed hook: a persisted kill notifies; the worker evaluates the killer's factions.
	w.svc.NotifyCombat(w.guild1, w.server1a, leadP)
	if n := w.svc.ProcessPending(w.ctx); n != 1 {
		t.Fatalf("only the killer's faction is evaluated, got %d", n)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1 AND achievement_key='FIRST_BLOOD'`, f.ID); n != 1 {
		t.Fatalf("FIRST_BLOOD must be unlocked by the event-driven path: %d", n)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_faction_achievement_unlocks WHERE faction_id=$1`, fo.ID); n != 0 {
		t.Fatal("a faction on another server is untouched")
	}
	// Many kills for the same player collapse into one evaluation.
	for i := 0; i < 5; i++ {
		w.svc.NotifyCombat(w.guild1, w.server1a, leadP)
	}
	if n := w.svc.ProcessPending(w.ctx); n != 1 {
		t.Fatalf("a burst is one evaluation per faction, got %d", n)
	}
	// A death only invalidates; it queues no evaluation.
	w.svc.NotifyCombat(w.guild1, w.server1a, 0)
	if n := w.svc.ProcessPending(w.ctx); n != 0 {
		t.Fatalf("deaths queue nothing, got %d", n)
	}
}

// --- activity ------------------------------------------------------------------------------------------------

func TestActivityFeedOrderingPaginationAndPrivacy(t *testing.T) {
	w := newWorld(t)
	lead, leadP := w.linked(w.guild1, "Chief")
	a, aP := w.linked(w.guild1, "Alpha")
	b, _ := w.linked(w.guild1, "Bravo")
	victim := w.outsider(w.guild1, "Enemy One")
	f := w.faction(w.inst1, lead, "Public Record", "PR")

	// Membership and management events through the REAL operations (with an application message that must never leak).
	app, err := w.hub.Apply(w.ctx, f.OrganizationID, f.InstallationID, f.ID, a, "SECRET APPLICATION MESSAGE do not leak")
	if err != nil {
		t.Fatal(err)
	}
	_, mA, err := w.hub.AcceptApplication(w.ctx, f.OrganizationID, f.InstallationID, f.ID, app.ID, lead)
	if err != nil {
		t.Fatal(err)
	}
	mB := w.join(f, lead, b)
	if _, err := w.hub.PromoteMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, mA.ID, lead); err != nil {
		t.Fatal(err)
	}
	if _, err := w.hub.DemoteMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, mA.ID, lead); err != nil {
		t.Fatal(err)
	}
	name := "Public Record Reborn"
	if _, err := w.hub.UpdateFaction(w.ctx, f.OrganizationID, f.InstallationID, f.ID, lead, repository.HubFactionUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.hub.ReplaceLogo(w.ctx, f.OrganizationID, f.InstallationID, f.ID, lead, repository.NewLogoAsset{
		PublicID: "00000000-0000-4000-8000-00000000000" + fmt.Sprint(w.next()%10), StorageKey: fmt.Sprintf("factions/%d/%d/%d/stats-%d.png", f.OrganizationID, f.InstallationID, f.ID, w.suffix),
		ContentType: "image/png", SizeBytes: 1000, Width: 256, Height: 256}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.hub.TransferLeadership(w.ctx, f.OrganizationID, f.InstallationID, f.ID, lead, mA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.hub.RemoveMember(w.ctx, f.OrganizationID, f.InstallationID, f.ID, mB, a); err != nil { // removal reads as a plain leave
		t.Fatal(err)
	}
	// Combat events with explicit times, including two kills at the SAME instant (a keyset tie).
	now := time.Now().UTC()
	tie := now.Add(2 * time.Hour)
	w.periods(f, lead, span{From: w.day(0)})
	w.periods(f, a, span{From: w.day(0)})
	w.kill(w.guild1, w.server1a, leadP, victim, now.Add(1*time.Hour), killOpt{})
	w.kill(w.guild1, w.server1a, leadP, victim, tie, killOpt{headshot: true, weapon: "SVD"})
	w.kill(w.guild1, w.server1a, aP, victim, tie, killOpt{longshot: true, distance: 312.34, weapon: "Mosin"})
	kb := w.kill(w.guild1, w.server1a, aP, victim, now.Add(3*time.Hour), killOpt{})
	w.bounty(w.guild1, w.server1a, victim, aP, kb, 100, now.Add(3*time.Hour))
	w.bounty(w.guild1, w.server1a, victim, aP, kb, 40, now.Add(3*time.Hour))
	// A server record set with one of the faction's kills, and one on another server (excluded).
	w.exec(`INSERT INTO record_events(guild_id, record_type, player_id, kill_id, new_value, created_at) VALUES($1,'LONGEST_KILL',$2,$3,312.34,$4)`, w.guild1, aP, kb, now.Add(3*time.Hour+time.Minute))
	kOther := w.kill(w.guild1, w.server1b, aP, victim, now.Add(4*time.Hour), killOpt{})
	w.exec(`INSERT INTO record_events(guild_id, record_type, player_id, kill_id, new_value, created_at) VALUES($1,'LONGEST_KILL',$2,$3,999,$4)`, w.guild1, aP, kOther, now.Add(4*time.Hour))
	// An achievement unlock.
	w.evaluate(f)

	// Read everything in one page, then page by 3 and by 1, and compare.
	full, err := w.svc.GetFactionRecentActivity(w.ctx, f.OrganizationID, f.InstallationID, f.ID, MaxActivityLimit, nil)
	if err != nil {
		t.Fatal(err)
	}
	if full.NextCursor != nil || len(full.Items) < 15 {
		t.Fatalf("one big page holds it all: %d items cursor=%v", len(full.Items), full.NextCursor)
	}
	types := map[string]int{}
	for _, it := range full.Items {
		types[it.Type]++
	}
	for _, want := range []string{ActivityFactionCreated, ActivityMemberJoined, ActivityMemberLeft, ActivityMemberPromoted, ActivityMemberDemoted, ActivityLeadershipTransferred,
		ActivityFactionUpdated, ActivityFactionLogoChanged, ActivityKill, ActivityHeadshot, ActivityLongshot, ActivityBountyClaimed, ActivityServerRecord, ActivityAchievementUnlocked} {
		if types[want] == 0 {
			t.Errorf("missing event type %s in %v", want, types)
		}
	}
	if types[ActivityBountyClaimed] != 1 || types[ActivityServerRecord] != 1 {
		t.Fatalf("stacked bounties on one kill are ONE event; the other server's record is excluded: %v", types)
	}
	if types[ActivityMemberJoined] != 2 || types[ActivityMemberLeft] != 1 {
		t.Fatalf("membership events: %v", types)
	}
	// The other server's kill never appears.
	for _, it := range full.Items {
		if it.ID == "k"+fmt.Sprint(kOther) {
			t.Fatal("an event from another server leaked into the feed")
		}
	}
	// Newest first: occurredAt is non-increasing.
	for i := 1; i < len(full.Items); i++ {
		if full.Items[i].OccurredAt > full.Items[i-1].OccurredAt {
			t.Fatalf("feed must be newest first: %s after %s", full.Items[i].OccurredAt, full.Items[i-1].OccurredAt)
		}
	}
	for _, size := range []int{1, 3, 7} {
		var got []string
		cursor := ""
		for pages := 0; ; pages++ {
			var cur *repository.HubActivityCursor
			if cursor != "" {
				c, ok := DecodeCursor(cursor)
				if !ok {
					t.Fatalf("cursor %q must decode", cursor)
				}
				cur = c
			}
			page, err := w.svc.GetFactionRecentActivity(w.ctx, f.OrganizationID, f.InstallationID, f.ID, size, cur)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > size || page.Limit != size {
				t.Fatalf("page size %d: %d items limit=%d", size, len(page.Items), page.Limit)
			}
			for _, it := range page.Items {
				got = append(got, it.ID+"/"+it.Type)
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
			if pages > 100 {
				t.Fatal("pagination did not terminate")
			}
		}
		var want []string
		for _, it := range full.Items {
			want = append(want, it.ID+"/"+it.Type)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("paging by %d must reproduce the full feed exactly (no gaps, no repeats, tie-safe):\n got %v\nwant %v", size, got, want)
		}
	}
	// Limits: default 20, clamped to 100, 0 -> default.
	if p, _ := w.svc.GetFactionRecentActivity(w.ctx, f.OrganizationID, f.InstallationID, f.ID, 0, nil); p.Limit != DefaultActivityLimit {
		t.Fatalf("default limit: %d", p.Limit)
	}
	if p, _ := w.svc.GetFactionRecentActivity(w.ctx, f.OrganizationID, f.InstallationID, f.ID, 100000, nil); p.Limit != MaxActivityLimit {
		t.Fatalf("max limit: %d", p.Limit)
	}

	// Public safety: no application message, no internal numeric ids, no moderation detail.
	raw, _ := json.Marshal(full)
	body := string(raw)
	for _, banned := range []string{"SECRET APPLICATION MESSAGE", "message", "userId", "\"applicationId\"", "REMOVED", "removed", "reviewedBy", "actorUserId", "subjectUserId", "storage", "asset"} {
		if strings.Contains(body, banned) {
			t.Errorf("public activity must not contain %q", banned)
		}
	}
	// Event shapes.
	for _, it := range full.Items {
		switch it.Type {
		case ActivityHeadshot:
			if it.Details["weapon"] != "SVD" || it.Details["victim"] != "Enemy One" || it.Details["headshot"] != true || it.Member == nil {
				t.Fatalf("headshot event: %+v", it)
			}
		case ActivityLongshot:
			if it.Details["distanceMeters"] != 312.3 || it.Details["longshot"] != true {
				t.Fatalf("longshot event: %+v", it)
			}
		case ActivityBountyClaimed:
			if it.Details["rewardPoints"] != int64(140) || it.Details["bounties"] != int64(2) {
				t.Fatalf("bounty event: %+v", it)
			}
		case ActivityMemberPromoted:
			if it.Details["role"] != "OFFICER" {
				t.Fatalf("promotion event: %+v", it)
			}
		case ActivityLeadershipTransferred:
			if it.Member == nil || it.Details["previousLeader"] == nil {
				t.Fatalf("transfer event: %+v", it)
			}
		case ActivityAchievementUnlocked:
			if it.Details["key"] == nil || it.Details["name"] == nil || it.Member != nil {
				t.Fatalf("achievement event: %+v", it)
			}
		}
	}
	// A tampered cursor is rejected.
	for _, bad := range []string{"", "not-base64!!", "Zm9v", encodeCursor(now, 99, 1)} {
		if _, ok := DecodeCursor(bad); ok && bad != "" {
			t.Errorf("cursor %q must be rejected", bad)
		}
	}
}
