//go:build integration

package app

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"testing"
	"time"
)

// End-to-end Faction Hub Phase 6 tests over the real routes and a real PostgreSQL: the leaderboard
// endpoint's contract (shape, value types, parameters), public access, tenant isolation, privacy,
// pagination/search and cache invalidation through the real handlers.

func (w *factionWorld) lb(f installationFixture, query, actor string) map[string]any {
	w.t.Helper()
	return w.getJSON(w.path(f, "/leaderboard"+query), actor)
}

func lbNames(page map[string]any) []string {
	out := []string{}
	for _, it := range page["items"].([]any) {
		out = append(out, it.(map[string]any)["name"].(string))
	}
	return out
}

func TestFactionLeaderboardShapeAndPublicAccess(t *testing.T) {
	w := newFactionWorld(t)
	leaderA, leaderB, leaderC, reader := w.players[0], w.players[1], w.players[2], w.players[3]
	pa := w.linkPlayer(w.a1, leaderA, "Alpha Boss")
	pb := w.linkPlayer(w.a1, leaderB, "Bravo Boss")
	fa := w.createFaction(w.a1, leaderA, "Alpha Company", "ALP", "OPEN")
	fb := w.createFaction(w.a1, leaderB, "Bravo Company", "BRV", "OPEN")
	w.createFaction(w.a1, leaderC, "Quiet Company", "QUI", "OPEN") // no linked members: no tracked activity
	soon := time.Now().UTC().Add(time.Hour)
	for i := 0; i < 3; i++ {
		w.insertKill(w.a1, pa, soon.Add(time.Duration(i)*time.Minute), i == 0)
	}
	w.insertKill(w.a1, pb, soon, true)
	w.insertKill(w.a1, pb, soon.Add(time.Minute), true)
	w.expect(w.do(http.MethodPut, w.path(w.a1, fmt.Sprintf("/%d", idOf(fa))), leaderA, map[string]any{"flagKey": "RED", "armbandKey": "BLUE"}), http.StatusOK, "branding")
	w.a.FactionHubStats.InvalidateLeaderboard(w.a1.OrgID, w.a1.InstallationID)

	// Any synced user - not a member, not in the organization - may read it.
	page := w.lb(w.a1, "", reader)
	for _, k := range []string{"metric", "direction", "items", "nextCursor", "limit", "total", "updatedAt"} {
		if _, ok := page[k]; !ok {
			t.Errorf("response is missing %q: %v", k, page)
		}
	}
	if page["metric"] != "KILLS" || page["direction"] != "DESC" || page["limit"].(float64) != 25 || page["total"].(float64) != 3 || page["nextCursor"] != nil {
		t.Fatalf("defaults: %v", page)
	}
	if fmt.Sprint(lbNames(page)) != "[Alpha Company Bravo Company Quiet Company]" {
		t.Fatalf("ranking: %v", lbNames(page))
	}
	first := page["items"].([]any)[0].(map[string]any)
	for _, k := range []string{"rank", "factionId", "name", "tag", "slug", "logo", "flagKey", "armbandKey", "primaryColor", "secondaryColor", "memberCount", "value", "trackingSince", "hasTrackedActivity", "stats"} {
		if _, ok := first[k]; !ok {
			t.Errorf("entry is missing %q: %v", k, first)
		}
	}
	if first["rank"].(float64) != 1 || int64(first["factionId"].(float64)) != idOf(fa) || first["tag"] != "ALP" || first["flagKey"] != "RED" || first["armbandKey"] != "BLUE" ||
		first["value"].(float64) != 3 || first["memberCount"].(float64) != 1 || first["hasTrackedActivity"] != true || first["logo"] != nil {
		t.Fatalf("first entry: %v", first)
	}
	if ts, _ := first["trackingSince"].(string); ts == "" {
		t.Fatalf("trackingSince is set for a faction with a membership history: %v", first["trackingSince"])
	}
	stats := first["stats"].(map[string]any)
	if stats["kills"].(float64) != 3 || stats["headshots"].(float64) != 1 || stats["deaths"].(float64) != 0 || stats["kdRatio"].(float64) != 3 {
		t.Fatalf("stats: %v", stats)
	}
	quiet := page["items"].([]any)[2].(map[string]any)
	if quiet["hasTrackedActivity"] != false || quiet["value"].(float64) != 0 || quiet["rank"].(float64) != 3 {
		t.Fatalf("a faction with no tracked activity stays listed with zeros: %v", quiet)
	}

	// The figures equal the profile's.
	prof := w.getJSON(w.path(w.a1, fmt.Sprintf("/%d", idOf(fa))), reader)["stats"].(map[string]any)
	for _, k := range []string{"kills", "deaths", "kdRatio", "headshots", "longshots", "bestKillStreak", "bountiesClaimed", "bountyValueClaimed", "achievementsUnlocked"} {
		if prof[k] != stats[k] {
			t.Errorf("%s: leaderboard %v vs profile %v", k, stats[k], prof[k])
		}
	}

	// Value types: KD is a decimal, every other metric an integer (no fractional part on the wire).
	rawKD := w.do(http.MethodGet, w.path(w.a1, "/leaderboard?metric=KD"), reader, nil).Body
	if !regexp.MustCompile(`"value":\s*3[,}\s]`).Match(rawKD) {
		t.Fatalf("KD value: %s", rawKD)
	}
	for _, m := range []string{"KILLS", "DEATHS", "HEADSHOTS", "LONGSHOTS", "BEST_STREAK", "BOUNTIES_CLAIMED", "BOUNTY_VALUE", "ACHIEVEMENTS", "kills", " Kd "} {
		p := w.lb(w.a1, "?metric="+url.QueryEscape(m), reader)
		if len(p["items"].([]any)) != 3 {
			t.Errorf("metric %q: %v", m, p)
		}
	}
	if d := w.lb(w.a1, "?metric=DEATHS", reader)["direction"]; d != "ASC" {
		t.Fatalf("DEATHS is ascending (fewer is better): %v", d)
	}
	if got := lbNames(w.lb(w.a1, "?metric=HEADSHOTS", reader)); got[0] != "Bravo Company" { // 2 vs 1
		t.Fatalf("headshots ranking: %v", got)
	}
	_ = fb

	// Privacy: nothing private, no overall score, no season fields.
	raw := w.do(http.MethodGet, w.path(w.a1, "/leaderboard?limit=100"), reader, nil).Body
	for _, banned := range []string{"\"userId\"", "discordUserId", "gamertag", "email", "overallRating", "powerScore", "combatScore", "skillScore", "season", "\"score\"", "description", "recruit"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("the public leaderboard must not contain %q", banned)
		}
	}
	// An organization admin reads the same thing (roles are irrelevant).
	w.lb(w.a1, "", w.admin)
}

func TestFactionLeaderboardParameterContract(t *testing.T) {
	w := newFactionWorld(t)
	reader := w.players[0]
	// Five factions, each with its own leader.
	for i, name := range []string{"Red One", "Red Two", "Blue One", "Blue Two", "Green One"} {
		w.createFaction(w.a1, w.players[1+i], name, fmt.Sprintf("T%d", i), "OPEN")
	}
	bad := map[string]string{
		"metric=OVERALL":         "unknown metric",
		"metric=POWER_SCORE":     "an invented metric",
		"metric=kills;drop":      "injection attempt",
		"metric=KILLS,DEATHS":    "several metrics",
		"limit=0":                "limit 0",
		"limit=-1":               "negative limit",
		"limit=abc":              "non-numeric limit",
		"cursor=bogus":           "garbage cursor",
		"metric=KD&cursor=bogus": "garbage cursor with metric",
		"q=" + longString(200):   "over-long search",
	}
	for q, what := range bad {
		w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard?"+q), reader, nil), http.StatusBadRequest, what)
	}
	if got := w.lb(w.a1, "?limit=100000", reader)["limit"].(float64); got != 100 {
		t.Fatalf("limit clamps to 100, got %v", got)
	}
	// Search: name or tag, case-insensitive; rank is the global position.
	p := w.lb(w.a1, "?q=RED", reader)
	if fmt.Sprint(lbNames(p)) != "[Red One Red Two]" || p["total"].(float64) != 2 {
		t.Fatalf("name search: %v", p)
	}
	if got := lbNames(w.lb(w.a1, "?q=t3", reader)); fmt.Sprint(got) != "[Blue Two]" {
		t.Fatalf("tag search: %v", got)
	}
	if got := w.lb(w.a1, "?q=nothing-like-this", reader); got["total"].(float64) != 0 || len(got["items"].([]any)) != 0 {
		t.Fatalf("no match: %v", got)
	}
	// Pagination by cursor: 2 + 2 + 1, no duplicates, and the cursor is bound to its metric.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "?limit=2&metric=KILLS"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		pg := w.lb(w.a1, path, reader)
		for _, n := range lbNames(pg) {
			if seen[n] {
				t.Fatalf("%s appeared on two pages", n)
			}
			seen[n] = true
		}
		pages++
		next, _ := pg["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if pages > 5 {
			t.Fatal("did not terminate")
		}
	}
	if pages != 3 || len(seen) != 5 {
		t.Fatalf("pages %d, factions %d", pages, len(seen))
	}
	// (the last cursor is empty; take a mid-walk one for the wrong-metric check)
	mid := w.lb(w.a1, "?limit=2", reader)["nextCursor"].(string)
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard?limit=2&metric=DEATHS&cursor="+url.QueryEscape(mid)), reader, nil), http.StatusBadRequest, "a cursor from another metric")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard?limit=2&cursor="+url.QueryEscape(mid)), reader, nil), http.StatusOK, "the cursor works with its own metric")
}

func longString(n int) string { return string(bytes.Repeat([]byte("x"), n)) }

func TestFactionLeaderboardTenantIsolationAndAuth(t *testing.T) {
	w := newFactionWorld(t)
	leaderA, leaderB, reader := w.players[0], w.players[1], w.players[2]
	pa := w.linkPlayer(w.a1, leaderA, "Alpha Boss")
	pb := w.linkPlayer(w.b1, leaderB, "Beta Boss")
	w.createFaction(w.a1, leaderA, "Alpha Stats", "AST", "OPEN")
	w.createFaction(w.b1, leaderB, "Beta Stats", "BST", "OPEN")
	w.insertKill(w.a1, pa, time.Now().UTC().Add(time.Hour), false)
	for i := 0; i < 5; i++ {
		w.insertKill(w.b1, pb, time.Now().UTC().Add(time.Hour+time.Duration(i)*time.Minute), false)
	}
	w.a.FactionHubStats.InvalidateLeaderboard(w.a1.OrgID, w.a1.InstallationID)
	w.a.FactionHubStats.InvalidateLeaderboard(w.b1.OrgID, w.b1.InstallationID)

	a := w.lb(w.a1, "", reader)
	if fmt.Sprint(lbNames(a)) != "[Alpha Stats]" || a["items"].([]any)[0].(map[string]any)["value"].(float64) != 1 {
		t.Fatalf("org A sees only its own installation: %v", a)
	}
	b := w.lb(w.b1, "", reader)
	if fmt.Sprint(lbNames(b)) != "[Beta Stats]" || b["items"].([]any)[0].(map[string]any)["value"].(float64) != 5 {
		t.Fatalf("org B sees only its own installation: %v", b)
	}
	if got := w.lb(w.a1, "?q=Beta", reader); got["total"].(float64) != 0 {
		t.Fatalf("no leakage through search: %v", got)
	}
	// Mismatched pairs are 404 - also with a warm cache.
	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID}
	for _, scope := range []installationFixture{crossOrg, crossInst} {
		for i := 0; i < 2; i++ {
			w.expect(w.do(http.MethodGet, w.path(scope, "/leaderboard"), reader, nil), http.StatusNotFound, "mismatched organization/installation")
		}
	}
	// Authentication: no acting user, or one that never synced.
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard"), "", nil), http.StatusUnauthorized, "no acting user")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard"), "never-synced", nil), http.StatusUnauthorized, "unsynced user")
	// The literal route is not captured by /{factionID}.
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/leaderboard"), reader, nil), http.StatusOK, "routing")
	// Read-only: no other methods.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if r := w.do(m, w.path(w.a1, "/leaderboard"), leaderA, nil); r.Status == http.StatusOK || r.Status == http.StatusCreated {
			t.Errorf("%s on the leaderboard must not succeed: %d", m, r.Status)
		}
	}
}

func TestFactionLeaderboardInvalidationThroughHandlers(t *testing.T) {
	w := newFactionWorld(t)
	leader, joiner, reader := w.players[0], w.players[1], w.players[2]
	lp := w.linkPlayer(w.a1, leader, "Boss")
	w.linkPlayer(w.a1, joiner, "Joiner")
	f := w.createFaction(w.a1, leader, "Living Board", "LVB", "OPEN")
	fid := idOf(f)
	entry := func() map[string]any {
		for _, it := range w.lb(w.a1, "?limit=100", reader)["items"].([]any) {
			if int64(it.(map[string]any)["factionId"].(float64)) == fid {
				return it.(map[string]any)
			}
		}
		t.Fatal("faction missing from the leaderboard")
		return nil
	}
	if entry()["memberCount"].(float64) != 1 {
		t.Fatal("baseline")
	}
	// A membership change through the API shows immediately although the leaderboard is cached.
	w.joined(leader, fid, joiner)
	if got := entry()["memberCount"].(float64); got != 2 {
		t.Fatalf("joining must invalidate the leaderboard: %v", got)
	}
	// A faction created through the API is listed immediately.
	w.createFaction(w.a1, w.players[3], "Late Arrivals", "LAT", "OPEN")
	if got := w.lb(w.a1, "?q=late", reader); got["total"].(float64) != 1 {
		t.Fatalf("a new faction appears immediately: %v", got)
	}
	// A branding change through the API shows immediately.
	w.expect(w.do(http.MethodPut, w.path(w.a1, fmt.Sprintf("/%d", fid)), leader, map[string]any{"name": "Living Board II", "flagKey": "GREEN"}), http.StatusOK, "rename")
	if e := entry(); e["name"] != "Living Board II" || e["flagKey"] != "GREEN" {
		t.Fatalf("editing the faction must invalidate the leaderboard: %v", e)
	}
	// A kill announced by the killfeed hook shows immediately.
	guild, server := w.gameContext(w.a1)
	var joinedAt time.Time
	if err := w.a.DB.Pool.QueryRow(t.Context(), `SELECT MIN(joined_at) FROM hub_faction_membership_history WHERE faction_id=$1 AND left_at IS NULL`, fid).Scan(&joinedAt); err != nil {
		t.Fatal(err)
	}
	if entry()["value"].(float64) != 0 {
		t.Fatal("baseline kills")
	}
	w.insertKill(w.a1, lp, joinedAt.Add(time.Minute), false)
	if entry()["value"].(float64) != 0 {
		t.Fatal("the warm cache is served until the killfeed notifies (bounded by the TTL)")
	}
	w.a.FactionHubStats.NotifyCombat(guild, server, lp)
	if got := entry()["value"].(float64); got != 1 {
		t.Fatalf("a persisted kill must invalidate the leaderboard: %v", got)
	}
	// Leaving changes the member count.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("/%d/leave", fid)), joiner, nil), http.StatusOK, "leave")
	if got := entry()["memberCount"].(float64); got != 1 {
		t.Fatalf("leaving must invalidate the leaderboard: %v", got)
	}
}
