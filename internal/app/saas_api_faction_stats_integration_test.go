//go:build integration

package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// End-to-end Faction Hub Phase 5 tests over the real routes and a real PostgreSQL: the stats,
// activity and achievements endpoints, the profile stats block, public access, tenant isolation,
// cache invalidation and the killfeed notification hook.

var statsTestSeq atomic.Int64

// gameContext returns the guild and server ids of an installation fixture.
func (w *factionWorld) gameContext(f installationFixture) (guild, server int64) {
	w.t.Helper()
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT c.guild_id, i.game_server_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, f.InstallationID).Scan(&guild, &server); err != nil {
		w.t.Fatal(err)
	}
	return
}

// linkPlayer gives the synced website user (by Discord id) a verified DayZ identity on the installation's guild.
func (w *factionWorld) linkPlayer(f installationFixture, discordID, gamertag string) int64 {
	w.t.Helper()
	guild, _ := w.gameContext(f)
	var player int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,$3) RETURNING id`, guild, fmt.Sprintf("dz-%d-%d-%s", time.Now().UnixNano(), statsTestSeq.Add(1), gamertag), gamertag).Scan(&player); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'VERIFIED')`, guild, player, discordID); err != nil {
		w.t.Fatal(err)
	}
	return player
}

func (w *factionWorld) insertKill(f installationFixture, killer int64, at time.Time, headshot bool) {
	w.t.Helper()
	guild, server := w.gameContext(f)
	var victim int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Some Victim') RETURNING id`, guild, fmt.Sprintf("dzv-%d-%d", time.Now().UnixNano(), statsTestSeq.Add(1))).Scan(&victim); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO kills(guild_id, server_id, session_id, event_fingerprint, killer_player_id, victim_player_id, headshot, distance, weapon_display, event_time)
VALUES($1,$2,'s',$3,$4,$5,$6,80,'M4-A1',$7)`, guild, server, fmt.Sprintf("api-kill-%d-%d", time.Now().UnixNano(), statsTestSeq.Add(1)), killer, victim, headshot, at); err != nil {
		w.t.Fatal(err)
	}
}

func (w *factionWorld) getJSON(path, actor string) map[string]any {
	w.t.Helper()
	return w.expect(w.do(http.MethodGet, path, actor, nil), http.StatusOK, "GET "+path).JSON(w.t)
}

func TestFactionStatsEndpointShapeAndPublicAccess(t *testing.T) {
	w := newFactionWorld(t)
	leader, member, reader := w.players[0], w.players[1], w.players[2]
	leaderP := w.linkPlayer(w.a1, leader, "ChiefGamertag")
	f := w.createFaction(w.a1, leader, "Numbers Unit", "NU", "OPEN")
	fid := idOf(f)
	fp := fmt.Sprintf("/%d", fid)
	w.joined(leader, fid, member) // an UNLINKED member

	soon := time.Now().UTC().Add(time.Hour)
	w.insertKill(w.a1, leaderP, soon, true)
	w.insertKill(w.a1, leaderP, soon.Add(time.Minute), false)
	w.a.FactionHubStats.Invalidate(w.a1.OrgID, w.a1.InstallationID, fid) // (the killfeed hook does this in production)

	// Any synced user - not a member, not in the organization - may read the stats.
	st := w.getJSON(w.path(w.a1, fp+"/stats"), reader)
	sum := st["summary"].(map[string]any)
	for _, k := range []string{"kills", "deaths", "kdRatio", "headshots", "longshots", "currentKillStreak", "bestKillStreak", "bountiesClaimed", "bountyValueClaimed", "memberCount", "linkedMemberCount", "achievementsUnlocked", "trackingSince"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("summary is missing %q: %v", k, sum)
		}
	}
	if sum["kills"].(float64) != 2 || sum["headshots"].(float64) != 1 || sum["deaths"].(float64) != 0 || sum["kdRatio"].(float64) != 2 ||
		sum["memberCount"].(float64) != 2 || sum["linkedMemberCount"].(float64) != 1 || sum["bestKillStreak"].(float64) != 2 {
		t.Fatalf("summary values: %v", sum)
	}
	if _, ok := st["updatedAt"].(string); !ok {
		t.Fatal("updatedAt is required")
	}
	contrib := st["memberContributions"].([]any)
	if len(contrib) != 2 {
		t.Fatalf("contributions: %v", contrib)
	}
	top := contrib[0].(map[string]any)
	if top["discordUserId"] != leader || top["kills"].(float64) != 2 || top["identity"] != "LINKED" || top["statsEligible"] != true || top["gamertag"] != "ChiefGamertag" || top["role"] != "LEADER" || top["status"] != "ACTIVE" || top["memberId"] == nil {
		t.Fatalf("top contribution: %v", top)
	}
	unl := contrib[1].(map[string]any)
	if unl["identity"] != "UNLINKED" || unl["statsEligible"] != false || unl["kills"].(float64) != 0 || unl["gamertag"] != nil {
		t.Fatalf("the unlinked member has zero figures and says why: %v", unl)
	}

	// The public profile carries the same summary, with no extra call.
	prof := w.getJSON(w.path(w.a1, fp), reader)
	ps, ok := prof["stats"].(map[string]any)
	if !ok || ps["kills"].(float64) != 2 || ps["kdRatio"].(float64) != 2 {
		t.Fatalf("profile stats block: %v", prof["stats"])
	}
	// Organization roles are irrelevant: an org admin reads it too.
	w.getJSON(w.path(w.a1, fp+"/stats"), w.admin)
}

func TestFactionStatsInvalidationOnMembershipAndKillEvents(t *testing.T) {
	w := newFactionWorld(t)
	leader, joiner := w.players[0], w.players[1]
	leaderP := w.linkPlayer(w.a1, leader, "Boss")
	joinerP := w.linkPlayer(w.a1, joiner, "Joiner")
	f := w.createFaction(w.a1, leader, "Fresh Numbers", "FN", "OPEN")
	fid := idOf(f)
	fp := fmt.Sprintf("/%d", fid)
	sumOf := func() map[string]any {
		return w.getJSON(w.path(w.a1, fp+"/stats"), w.players[5])["summary"].(map[string]any)
	}

	if s := sumOf(); s["memberCount"].(float64) != 1 || s["kills"].(float64) != 0 {
		t.Fatalf("baseline: %v", s)
	}
	// The stats are now cached; a membership change through the API must show immediately.
	w.joined(leader, fid, joiner)
	if s := sumOf(); s["memberCount"].(float64) != 2 || s["linkedMemberCount"].(float64) != 2 {
		t.Fatalf("joining must invalidate the cache: %v", s)
	}
	// A kill by a member, announced by the killfeed hook (NotifyCombat), shows immediately although the cache is warm.
	guild, server := w.gameContext(w.a1)
	// (dated just after the joiner's membership period opened, so it is a kill made while a member)
	var joinedAt time.Time
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT joined_at FROM hub_faction_membership_history WHERE faction_id=$1 AND left_at IS NULL ORDER BY joined_at DESC LIMIT 1`, fid).Scan(&joinedAt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	w.insertKill(w.a1, joinerP, joinedAt.Add(time.Millisecond), false)
	if s := sumOf(); s["kills"].(float64) != 0 {
		t.Fatalf("without the notification the warm cache is still served (expected, bounded by the TTL): %v", s)
	}
	w.a.FactionHubStats.NotifyCombat(guild, server, joinerP)
	if s := sumOf(); s["kills"].(float64) != 1 {
		t.Fatalf("a persisted kill must invalidate the cache: %v", s)
	}
	_ = leaderP
	// Leaving: the member becomes FORMER, their counted kill stays in the totals.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/leave"), joiner, nil), http.StatusOK, "leave")
	st := w.getJSON(w.path(w.a1, fp+"/stats"), w.players[5])
	if st["summary"].(map[string]any)["memberCount"].(float64) != 1 || st["summary"].(map[string]any)["kills"].(float64) != 1 {
		t.Fatalf("after leaving: %v", st["summary"])
	}
	found := false
	for _, c := range st["memberContributions"].([]any) {
		m := c.(map[string]any)
		if m["discordUserId"] == joiner {
			found = true
			if m["status"] != "FORMER" || m["role"] != nil || m["memberId"] != nil || m["kills"].(float64) != 1 {
				t.Fatalf("former member row: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("a former member with counted kills stays in the contributions")
	}
	// The kill they make AFTER leaving is not credited.
	var leftAt time.Time
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT left_at FROM hub_faction_membership_history WHERE faction_id=$1 AND left_at IS NOT NULL ORDER BY left_at DESC LIMIT 1`, fid).Scan(&leftAt); err != nil {
		t.Fatal(err)
	}
	w.insertKill(w.a1, joinerP, leftAt.Add(time.Millisecond), false)
	w.a.FactionHubStats.NotifyCombat(guild, server, joinerP)
	if s := sumOf(); s["kills"].(float64) != 1 {
		t.Fatalf("a kill after leaving must not count: %v", s)
	}
}

func TestFactionActivityEndpoint(t *testing.T) {
	w := newFactionWorld(t)
	leader, member, reader := w.players[0], w.players[1], w.players[2]
	leaderP := w.linkPlayer(w.a1, leader, "Chief")
	f := w.createFaction(w.a1, leader, "Public Ledger", "PL", "OPEN")
	fid := idOf(f)
	fp := fmt.Sprintf("/%d", fid)
	secret := "MY PRIVATE APPLICATION TEXT"
	app := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), member, map[string]any{"message": secret}), http.StatusCreated, "apply").JSON(t)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/accept", fp, idOf(app))), leader, nil), http.StatusOK, "accept")
	mid := w.memberIDByDiscord(fid, leader, member)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/members/%d/promote", fp, mid)), leader, nil), http.StatusOK, "promote")
	w.expect(w.do(http.MethodDelete, w.path(w.a1, fmt.Sprintf("%s/members/%d", fp, mid)), leader, nil), http.StatusOK, "remove")
	for i := 0; i < 25; i++ {
		w.insertKill(w.a1, leaderP, time.Now().UTC().Add(time.Hour+time.Duration(i)*time.Second), i%5 == 0)
	}
	w.a.FactionHubStats.Invalidate(w.a1.OrgID, w.a1.InstallationID, fid)

	page := w.getJSON(w.path(w.a1, fp+"/activity"), reader)
	items := page["items"].([]any)
	if len(items) != 20 || page["limit"].(float64) != 20 || page["nextCursor"] == nil {
		t.Fatalf("default page is 20 with a cursor: %d %v", len(items), page["limit"])
	}
	first := items[0].(map[string]any)
	if first["type"] != "KILL" && first["type"] != "HEADSHOT" {
		t.Fatalf("newest first (the latest kill): %v", first)
	}
	for _, k := range []string{"id", "type", "occurredAt", "member"} {
		if _, ok := first[k]; !ok {
			t.Errorf("event missing %q: %v", k, first)
		}
	}
	// Walk every page; collect types; verify no duplicates.
	seen, types := map[string]bool{}, map[string]int{}
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		path := w.path(w.a1, fp+"/activity?limit=7")
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		p := w.getJSON(path, reader)
		for _, it := range p["items"].([]any) {
			m := it.(map[string]any)
			id := m["id"].(string)
			if seen[id] {
				t.Fatalf("event %s appeared on two pages", id)
			}
			seen[id] = true
			types[m["type"].(string)]++
		}
		next, _ := p["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if types["KILL"]+types["HEADSHOT"] != 25 || types["MEMBER_JOINED"] != 1 || types["MEMBER_PROMOTED"] != 1 || types["MEMBER_LEFT"] != 1 || types["FACTION_CREATED"] != 1 {
		t.Fatalf("event mix: %v", types)
	}
	// Privacy: nothing the applicant wrote, no internal ids, a removal reads as MEMBER_LEFT.
	raw := w.do(http.MethodGet, w.path(w.a1, fp+"/activity?limit=100"), reader, nil).Body
	for _, banned := range []string{secret, "\"userId\"", "REMOVED", "reviewed", "message"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("public activity must not contain %q", banned)
		}
	}
	// Parameter contract.
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/activity?limit=0"), reader, nil), http.StatusBadRequest, "limit=0")
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/activity?limit=abc"), reader, nil), http.StatusBadRequest, "limit=abc")
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/activity?cursor=bogus"), reader, nil), http.StatusBadRequest, "bad cursor")
	if got := w.getJSON(w.path(w.a1, fp+"/activity?limit=100000"), reader)["limit"].(float64); got != 100 {
		t.Fatalf("limit clamps to 100, got %v", got)
	}
}

func TestFactionAchievementsEndpoint(t *testing.T) {
	w := newFactionWorld(t)
	leader, reader := w.players[0], w.players[1]
	leaderP := w.linkPlayer(w.a1, leader, "Achiever")
	f := w.createFaction(w.a1, leader, "Trophy Case", "TCA", "OPEN")
	fp := fmt.Sprintf("/%d", idOf(f))

	res := w.getJSON(w.path(w.a1, fp+"/achievements"), reader)
	items := res["items"].([]any)
	if len(items) != 10 || res["total"].(float64) != 10 || res["unlockedCount"].(float64) != 0 {
		t.Fatalf("catalog and counts: %v", res)
	}
	keys := []string{"FIRST_BLOOD", "KILLS_100", "KILLS_500", "KILLS_1000", "HEADHUNTERS", "LONG_RANGE", "BOUNTY_HUNTERS", "KILLING_MACHINE", "FULL_SQUAD", "VETERAN_FACTION"}
	for i, it := range items {
		a := it.(map[string]any)
		if a["key"] != keys[i] {
			t.Fatalf("stable order: %v", a["key"])
		}
		for _, k := range []string{"name", "description", "unlocked", "unlockedAt", "progress", "target", "unit"} {
			if _, ok := a[k]; !ok {
				t.Errorf("achievement missing %q: %v", k, a)
			}
		}
		if a["unlocked"] != false || a["unlockedAt"] != nil {
			t.Fatalf("nothing unlocked yet: %v", a)
		}
	}
	// One real kill unlocks FIRST_BLOOD (the read evaluates what the data already proves).
	w.insertKill(w.a1, leaderP, time.Now().UTC().Add(time.Hour), false)
	w.a.FactionHubStats.Invalidate(w.a1.OrgID, w.a1.InstallationID, idOf(f))
	res = w.getJSON(w.path(w.a1, fp+"/achievements"), reader)
	fb := res["items"].([]any)[0].(map[string]any)
	if fb["unlocked"] != true || fb["unlockedAt"] == nil || fb["progress"].(float64) != 1 || fb["target"].(float64) != 1 || res["unlockedCount"].(float64) != 1 {
		t.Fatalf("FIRST_BLOOD after one kill: %v", fb)
	}
	k100 := res["items"].([]any)[1].(map[string]any)
	if k100["unlocked"] != false || k100["progress"].(float64) != 1 || k100["target"].(float64) != 100 {
		t.Fatalf("KILLS_100 progress is real: %v", k100)
	}
	// The unlock is also an activity event and counted in the stats summary.
	stats := w.getJSON(w.path(w.a1, fp+"/stats"), reader)["summary"].(map[string]any)
	if stats["achievementsUnlocked"].(float64) != 1 {
		t.Fatalf("achievementsUnlocked in the summary: %v", stats)
	}
	act := w.getJSON(w.path(w.a1, fp+"/activity"), reader)["items"].([]any)
	got := false
	for _, it := range act {
		if it.(map[string]any)["type"] == "ACHIEVEMENT_UNLOCKED" {
			got = true
		}
	}
	if !got {
		t.Fatal("ACHIEVEMENT_UNLOCKED must appear in the activity feed")
	}
}

func TestFactionStatsRoutesAreTenantScopedAndAuthenticated(t *testing.T) {
	w := newFactionWorld(t)
	leaderA, leaderB := w.players[0], w.players[1]
	fa := w.createFaction(w.a1, leaderA, "Alpha Stats", "AST", "OPEN")
	fb := w.createFaction(w.b1, leaderB, "Beta Stats", "BST", "OPEN")
	fidA := fmt.Sprintf("/%d", idOf(fa))
	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID}
	for _, ep := range []string{"/stats", "/activity", "/achievements"} {
		for _, scope := range []installationFixture{crossOrg, crossInst} {
			for _, actor := range []string{leaderA, leaderB} {
				if r := w.do(http.MethodGet, w.path(scope, fidA+ep), actor, nil); r.Status != http.StatusNotFound {
					t.Errorf("GET %s under %+v as %s: want 404, got %d %s", ep, scope, actor, r.Status, r.Body)
				}
			}
		}
		// Tenant B's faction id under tenant A's path.
		if r := w.do(http.MethodGet, w.path(w.a1, fmt.Sprintf("/%d%s", idOf(fb), ep)), leaderA, nil); r.Status != http.StatusNotFound {
			t.Errorf("GET %s of another tenant's faction: %d", ep, r.Status)
		}
		// Authentication first.
		w.expect(w.do(http.MethodGet, w.path(w.a1, fidA+ep), "", nil), http.StatusUnauthorized, "no acting user "+ep)
		w.expect(w.do(http.MethodGet, w.path(w.a1, fidA+ep), "never-synced", nil), http.StatusUnauthorized, "unsynced "+ep)
		w.expect(w.do(http.MethodGet, w.path(w.a1, "/abc"+ep), leaderA, nil), http.StatusBadRequest, "bad id "+ep)
		w.expect(w.do(http.MethodPost, w.path(w.a1, fidA+ep), leaderA, nil), http.StatusMethodNotAllowed, "read-only "+ep)
	}
	// Each tenant sees only its own numbers.
	if got := w.getJSON(w.path(w.b1, fmt.Sprintf("/%d/stats", idOf(fb))), leaderB)["summary"].(map[string]any)["memberCount"].(float64); got != 1 {
		t.Fatalf("tenant B stats: %v", got)
	}
}
