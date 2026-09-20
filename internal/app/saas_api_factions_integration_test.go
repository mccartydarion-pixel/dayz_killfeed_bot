//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/assetstore"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/factionassets"
	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/server"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// End-to-end Faction Hub API tests: the real routes on a real HTTP listener (so route
// precedence such as /factions/me vs /factions/{factionID} is exercised), against a real
// PostgreSQL. See docs/FACTIONS.md.

type factionWorld struct {
	t     *testing.T
	a     *App
	base  string
	store *assetstore.MemoryStore

	a1, b1 installationFixture // two independent organizations
	admin  string              // ADMIN of org A
	member string              // MEMBER of org A
	// Players: synced Champion users who belong to NO organization.
	players []string
}

func newFactionWorld(t *testing.T) *factionWorld {
	t.Helper()
	a, verifier := saasIntegrationApp(t)
	a.FactionHub = repository.NewFactionHubRepository(a.DB.Pool)
	a.saasFactionCreateLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.saasFactionCreateDayLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.saasFactionApplyLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.saasFactionApplyDayLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.saasFactionLogoLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.saasFactionLogoDayLimiter = newSaaSRateLimiter(time.Hour, 1000)
	a.FactionHubStats = factionstats.NewService(repository.NewHubStatsRepository(a.DB.Pool), factionstats.Options{})
	store := assetstore.NewMemoryStore()
	a.FactionAssets = factionassets.NewService(store, a.FactionHub)
	a.Config.PublicBaseURL = "https://champion.example"

	// One listener per test, on a free local port, serving only the faction routes.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	srv, err := server.New(&config.Config{Port: port, WebsiteAPISecret: "test-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.HTTPServer = srv
	a.registerFactionHubRoutes()
	a.EconomyService = economy.NewService(repository.NewEconomyRepository(a.DB.Pool), nil)
	a.EconomyAccounts = economy.NewAccounts(a.EconomyService, repository.NewEconomyRepository(a.DB.Pool))
	a.saasEconomyAdjustLimiter = newSaaSRateLimiter(time.Hour, 100000)
	a.saasEconomyHistoryLimiter = newSaaSRateLimiter(time.Hour, 100000)
	a.registerEconomyRoutes()
	a.Shop = shop.NewService(repository.NewShopRepository(a.DB.Pool), a.EconomyAccounts, a.EconomyService)
	a.saasShopPurchaseLimiter = newSaaSRateLimiter(time.Hour, 100000)
	a.saasShopAdminLimiter = newSaaSRateLimiter(time.Hour, 100000)
	a.registerShopRoutes()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		shutdown, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(shutdown)
	})

	w := &factionWorld{t: t, a: a, base: "http://127.0.0.1:" + port, store: store}
	w.a1 = buildInstallationFixture(t, a, verifier)
	w.b1 = buildInstallationFixture(t, a, verifier)
	for _, f := range []installationFixture{w.a1, w.b1} {
		w.attachServer(f)
	}
	suffix := time.Now().UnixNano()
	adm := syncUser(t, a, fmt.Sprintf("fh-admin-%d", suffix), "Org Admin")
	mem := syncUser(t, a, fmt.Sprintf("fh-member-%d", suffix), "Org Member")
	for user, role := range map[string]string{adm.DiscordUserID: "ADMIN", mem.DiscordUserID: "MEMBER"} {
		if _, err := a.DB.Pool.Exec(context.Background(), `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, w.a1.OrgID, mustAppUserID(t, a, user), role); err != nil {
			t.Fatal(err)
		}
	}
	w.admin, w.member = adm.DiscordUserID, mem.DiscordUserID
	for i := 0; i < 6; i++ {
		w.players = append(w.players, syncUser(t, a, fmt.Sprintf("fh-player-%d-%d", suffix, i), fmt.Sprintf("Player %d", i)).DiscordUserID)
	}
	// The wait for the listener to accept connections.
	for i := 0; i < 50; i++ {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return w
}

// attachServer gives the fixture's installation a selected DayZ server (a faction needs one).
func (w *factionWorld) attachServer(f installationFixture) {
	w.t.Helper()
	ctx := context.Background()
	var guildID, serverID int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT c.guild_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, f.InstallationID).Scan(&guildID); err != nil {
		w.t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		guildID, fmt.Sprintf("fh-%d", time.Now().UnixNano())).Scan(&serverID); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$1 WHERE id=$2`, serverID, f.InstallationID); err != nil {
		w.t.Fatal(err)
	}
}

func (w *factionWorld) path(f installationFixture, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/factions%s", f.OrgID, f.InstallationID, suffix)
}

type apiResult struct {
	Status      int
	ContentType string
	Body        []byte
	m           map[string]any
}

func (r *apiResult) JSON(t *testing.T) map[string]any {
	t.Helper()
	if r.m == nil {
		if err := json.Unmarshal(r.Body, &r.m); err != nil {
			t.Fatalf("response is not a JSON object (%d): %s", r.Status, r.Body)
		}
	}
	return r.m
}

func (r *apiResult) errCode(t *testing.T) string {
	t.Helper()
	e, _ := r.JSON(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// do sends a request as actor (a Discord user id; "" sends no acting-user header).
func (w *factionWorld) do(method, path, actor string, body any) *apiResult {
	w.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			w.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.base+path, rd)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-secret")
	if actor != "" {
		req.Header.Set(actingUserHeader, actor)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return &apiResult{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: raw}
}

func (w *factionWorld) expect(r *apiResult, status int, what string) *apiResult {
	w.t.Helper()
	if r.Status != status {
		w.t.Fatalf("%s: want HTTP %d, got %d: %s", what, status, r.Status, r.Body)
	}
	return r
}

func (w *factionWorld) createFaction(f installationFixture, actor, name, tag, recruit string) map[string]any {
	w.t.Helper()
	r := w.expect(w.do(http.MethodPost, w.path(f, ""), actor, map[string]any{"name": name, "tag": tag, "description": "We hold " + name, "recruitmentStatus": recruit}), http.StatusCreated, "create faction "+name)
	return r.JSON(w.t)
}

func idOf(m map[string]any) int64 { return int64(m["id"].(float64)) }

// --- authentication, routing and the player flow -------------------------------------------------------------

func TestFactionAPIAuthenticationAndRouting(t *testing.T) {
	w := newFactionWorld(t)
	// Service auth first, then the acting user.
	req, _ := http.NewRequest(http.MethodGet, w.base+w.path(w.a1, ""), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no service auth must be 401, got %d", resp.StatusCode)
	}
	w.expect(w.do(http.MethodGet, w.path(w.a1, ""), "", nil), http.StatusUnauthorized, "no acting user")
	w.expect(w.do(http.MethodGet, w.path(w.a1, ""), "never-synced-user", nil), http.StatusUnauthorized, "unsynced acting user")

	p := w.players[0]
	// /factions/me is its own route, not a faction id.
	me := w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), p, nil), http.StatusOK, "me").JSON(t)
	if _, ok := me["pendingApplications"]; !ok || me["faction"] != nil {
		t.Fatalf("me must be the my-faction shape: %v", me)
	}
	// A non-numeric id is a 400, an unknown one a 404.
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/abc"), p, nil), http.StatusBadRequest, "non-numeric faction id")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "/999999999"), p, nil), http.StatusNotFound, "unknown faction id")
	// Unsupported methods on a known route are 405, not a silent success.
	w.expect(w.do(http.MethodDelete, w.path(w.a1, "/1"), p, nil), http.StatusMethodNotAllowed, "DELETE on a faction")
}

func TestFactionAPIPlayerFlowEndToEnd(t *testing.T) {
	w := newFactionWorld(t)
	leader, applicant, other := w.players[0], w.players[1], w.players[2]

	// Any synced user - no organization membership - founds a faction and becomes its LEADER.
	f := w.createFaction(w.a1, leader, "Unit Zero", "uz", "OPEN")
	fid := idOf(f)
	if f["slug"] != "unit-zero" || f["tag"] != "UZ" || f["memberCount"].(float64) != 1 || f["recruitmentStatus"] != "OPEN" {
		t.Fatalf("created profile: %v", f)
	}
	if v := f["viewer"].(map[string]any); v["role"] != "LEADER" || v["canEdit"] != true || v["canManageApplications"] != true || v["moderationView"] != false {
		t.Fatalf("leader viewer block: %v", v)
	}
	if l := f["leader"].(map[string]any); l["role"] != "LEADER" || l["discordUserId"] != leader {
		t.Fatalf("profile leader: %v", l)
	}
	fp := fmt.Sprintf("/%d", fid)

	// Directory: public within the authenticated experience (a non-member reads it).
	dir := w.expect(w.do(http.MethodGet, w.path(w.a1, ""), applicant, nil), http.StatusOK, "directory").JSON(t)
	items := dir["items"].([]any)
	if len(items) != 1 || dir["nextCursor"] != nil || dir["limit"].(float64) != 25 {
		t.Fatalf("directory: %v", dir)
	}
	row := items[0].(map[string]any)
	for _, k := range []string{"id", "name", "tag", "slug", "descriptionPreview", "recruitmentStatus", "memberCount", "logoKey", "flagKey", "armbandKey", "primaryColor", "secondaryColor"} {
		if _, ok := row[k]; !ok {
			t.Errorf("directory row is missing %q: %v", k, row)
		}
	}
	// The directory never leaks application data or members.
	for _, k := range []string{"applications", "members", "message"} {
		if _, ok := row[k]; ok {
			t.Errorf("directory row must not include %q", k)
		}
	}

	// Apply (empty body is a valid message-less application) -> pending; duplicate -> 409.
	app := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": "I run 8h a day and have a mic"}), http.StatusCreated, "apply").JSON(t)
	appID := idOf(app)
	if app["status"] != "PENDING" || app["message"] != "I run 8h a day and have a mic" {
		t.Fatalf("application: %v", app)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, nil), http.StatusConflict, "duplicate application")
	// My faction shows the pending application and no faction yet.
	me := w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), applicant, nil), http.StatusOK, "me pending").JSON(t)
	pend := me["pendingApplications"].([]any)
	if me["faction"] != nil || len(pend) != 1 || pend[0].(map[string]any)["faction"].(map[string]any)["slug"] != "unit-zero" {
		t.Fatalf("me pending: %v", me)
	}
	// The applicant cannot see applications, accept, or deny.
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications"), applicant, nil), http.StatusForbidden, "applicant listing applications")
	appPath := fmt.Sprintf("%s/applications/%d", fp, appID)
	w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/accept"), applicant, nil), http.StatusForbidden, "applicant accepting own application")
	w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/deny"), other, nil), http.StatusForbidden, "outsider denying")
	// Someone else cannot withdraw it (it looks like it does not exist).
	w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/withdraw"), other, nil), http.StatusNotFound, "withdrawing someone else's application")

	// The leader sees it (with the applicant's message) and accepts it.
	list := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications?status=pending"), leader, nil), http.StatusOK, "leader listing").JSON(t)
	if li := list["items"].([]any); len(li) != 1 || li[0].(map[string]any)["applicant"].(map[string]any)["discordUserId"] != applicant {
		t.Fatalf("applications list: %v", list)
	}
	acc := w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/accept"), leader, nil), http.StatusOK, "accept").JSON(t)
	if acc["application"].(map[string]any)["status"] != "ACCEPTED" || acc["member"].(map[string]any)["role"] != "MEMBER" {
		t.Fatalf("accept result: %v", acc)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/accept"), leader, nil), http.StatusConflict, "accepting twice")
	w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/withdraw"), applicant, nil), http.StatusConflict, "withdrawing an accepted application")
	me = w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), applicant, nil), http.StatusOK, "me member").JSON(t)
	if me["role"] != "MEMBER" || me["faction"].(map[string]any)["id"].(float64) != float64(fid) || len(me["pendingApplications"].([]any)) != 0 {
		t.Fatalf("me as member: %v", me)
	}
	// A member cannot apply elsewhere / found another faction on this installation.
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), applicant, map[string]any{"name": "Second Chance", "tag": "SC"}), http.StatusConflict, "member founding a faction")

	// Profile and member list are public; member count and roles are right.
	prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), other, nil), http.StatusOK, "profile").JSON(t)
	if prof["memberCount"].(float64) != 2 || len(prof["members"].([]any)) != 2 || prof["viewer"].(map[string]any)["role"] != nil {
		t.Fatalf("profile: %v", prof)
	}
	for _, k := range []string{"applications", "pendingApplications"} {
		if _, ok := prof[k]; ok {
			t.Errorf("the public profile must not include %q", k)
		}
	}
	mem := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/members"), other, nil), http.StatusOK, "members").JSON(t)
	if mem["total"].(float64) != 2 {
		t.Fatalf("members: %v", mem)
	}
	memberID := int64(mem["items"].([]any)[1].(map[string]any)["id"].(float64))
	mp := fmt.Sprintf("%s/members/%d", fp, memberID)

	// Promote / demote / remove: only the LEADER promotes; the member itself cannot.
	w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/promote"), applicant, nil), http.StatusForbidden, "member promoting themselves")
	pr := w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/promote"), leader, nil), http.StatusOK, "promote").JSON(t)
	if pr["role"] != "OFFICER" {
		t.Fatalf("promote: %v", pr)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/promote"), leader, nil), http.StatusConflict, "promoting an officer")
	// The officer may now review applications but not edit the faction.
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications"), applicant, nil), http.StatusOK, "officer listing applications")
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), applicant, map[string]any{"description": "officer edit"}), http.StatusForbidden, "officer editing the faction")
	dm := w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/demote"), leader, nil), http.StatusOK, "demote").JSON(t)
	if dm["role"] != "MEMBER" {
		t.Fatalf("demote: %v", dm)
	}
	leaderMemberID := int64(mem["items"].([]any)[0].(map[string]any)["id"].(float64))
	w.expect(w.do(http.MethodDelete, w.path(w.a1, fmt.Sprintf("%s/members/%d", fp, leaderMemberID)), leader, nil), http.StatusForbidden, "removing the leader")
	rm := w.expect(w.do(http.MethodDelete, w.path(w.a1, mp), leader, nil), http.StatusOK, "remove").JSON(t)
	if rm["removed"] != true {
		t.Fatalf("remove: %v", rm)
	}
	me = w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), applicant, nil), http.StatusOK, "me after removal").JSON(t)
	if me["faction"] != nil || me["role"] != nil {
		t.Fatalf("after removal the user has no faction: %v", me)
	}

	// Withdraw and deny paths.
	app2 := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": "again"}), http.StatusCreated, "re-apply").JSON(t)
	wd := w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/withdraw", fp, idOf(app2))), applicant, nil), http.StatusOK, "withdraw").JSON(t)
	if wd["status"] != "WITHDRAWN" {
		t.Fatalf("withdraw: %v", wd)
	}
	app3 := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, nil), http.StatusCreated, "apply 3").JSON(t)
	dn := w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/deny", fp, idOf(app3))), leader, nil), http.StatusOK, "deny").JSON(t)
	if dn["status"] != "DENIED" || dn["reviewedByUserId"] == nil || dn["reviewedAt"] == nil {
		t.Fatalf("deny: %v", dn)
	}
	// History is retained and filterable.
	all := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications?limit=10"), leader, nil), http.StatusOK, "history").JSON(t)
	if n := len(all["items"].([]any)); n != 3 {
		t.Fatalf("application history must be kept, got %d rows", n)
	}
}

func TestFactionAPIRecruitmentAndUpdate(t *testing.T) {
	w := newFactionWorld(t)
	leader, p1, p2 := w.players[0], w.players[1], w.players[2]
	f := w.createFaction(w.a1, leader, "Gate Keepers", "GK", "CLOSED")
	fp := fmt.Sprintf("/%d", idOf(f))
	// CLOSED: cannot apply.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), p1, nil), http.StatusConflict, "apply to CLOSED")

	// Only the leader edits; unknown fields (image URLs, ids) are rejected, not stored.
	upd := map[string]any{"recruitmentStatus": "invite_only", "description": "Now by invitation", "primaryColor": "#c0ffee", "secondaryColor": "#000000",
		"requirements": map[string]any{"minimumHours": 100, "minimumAge": 18, "pvpRequired": true, "builderNeeded": false, "micRequired": true, "customRequirements": "Be active"}}
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), p1, upd), http.StatusForbidden, "non-member editing")
	got := w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, upd), http.StatusOK, "leader editing").JSON(t)
	if got["recruitmentStatus"] != "INVITE_ONLY" || got["primaryColor"] != "#C0FFEE" || got["slug"] != "gate-keepers" {
		t.Fatalf("edited profile: %v", got)
	}
	req := got["requirements"].(map[string]any)
	if req["minimumHours"].(float64) != 100 || req["minimumAge"].(float64) != 18 || req["pvpRequired"] != true || req["micRequired"] != true || req["builderNeeded"] != false || req["customRequirements"] != "Be active" {
		t.Fatalf("requirements: %v", req)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), p1, nil), http.StatusConflict, "apply to INVITE_ONLY")
	for _, body := range []map[string]any{
		{"logoKey": "wolf"}, {"logoUrl": "https://evil.example/x.png"}, {"logo": "https://evil.example/x.png"}, {"logoAssetId": 1},
		{"installationId": 999}, {"organizationId": 1}, {"gameServerId": 1}, {"slug": "hijack"}, {"answers": map[string]any{"q": "a"}},
	} {
		w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, body), http.StatusBadRequest, fmt.Sprintf("unknown/immutable field %v", body))
	}
	// Bad values.
	for name, body := range map[string]map[string]any{
		"bad status":     {"recruitmentStatus": "PUBLIC"},
		"bad color":      {"primaryColor": "red"},
		"bad name":       {"name": "@everyone"},
		"bad tag":        {"tag": "toolong"},
		"underage req":   {"requirements": map[string]any{"minimumAge": 5}},
		"negative hours": {"requirements": map[string]any{"minimumHours": -3}},
	} {
		w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, body), http.StatusBadRequest, name)
	}
	// Reopen recruiting; applying works and the directory filter finds it.
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"recruitmentStatus": "OPEN"}), http.StatusOK, "reopen")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), p1, nil), http.StatusCreated, "apply to OPEN")
	w.createFaction(w.a1, p2, "Shut Door", "SD", "CLOSED")
	rec := w.expect(w.do(http.MethodGet, w.path(w.a1, "?recruiting=true"), p1, nil), http.StatusOK, "recruiting filter").JSON(t)
	if it := rec["items"].([]any); len(it) != 1 || it[0].(map[string]any)["name"] != "Gate Keepers" {
		t.Fatalf("recruiting filter: %v", rec)
	}
	// Name / tag conflicts.
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"name": "shut door"}), http.StatusConflict, "renaming onto an existing name")
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), w.players[3], map[string]any{"name": "GATE KEEPERS", "tag": "GG"}), http.StatusConflict, "duplicate name on create")
}

func TestFactionAPIValidationAndPlainTextStorage(t *testing.T) {
	w := newFactionWorld(t)
	p := w.players[0]
	bad := map[string]any{
		"empty":        map[string]any{},
		"short name":   map[string]any{"name": "ab", "tag": "AB"},
		"mention name": map[string]any{"name": "@everyone", "tag": "EV"},
		"html name":    map[string]any{"name": "<b>Bold</b>", "tag": "BB"},
		"bad tag":      map[string]any{"name": "Fine Name", "tag": "A-B"},
		"long tag":     map[string]any{"name": "Fine Name", "tag": "ABCDEF"},
		"bad status":   map[string]any{"name": "Fine Name", "tag": "FN", "recruitmentStatus": "MAYBE"},
		"long desc":    map[string]any{"name": "Fine Name", "tag": "FN", "description": strings.Repeat("x", 501)},
		"unknown key":  map[string]any{"name": "Fine Name", "tag": "FN", "imageUrl": "https://x"},
	}
	for name, body := range bad {
		w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, body), http.StatusBadRequest, name)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, `{"name":"Fine Name","tag":"FN"} {"x":1}`), http.StatusBadRequest, "trailing JSON value")
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, `not json`), http.StatusBadRequest, "malformed JSON")
	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, `{"name":"`+strings.Repeat("a", 40<<10)+`","tag":"FN"}`), http.StatusRequestEntityTooLarge, "oversized body")
	// Nothing was created by any of the rejected requests.
	if n := w.expect(w.do(http.MethodGet, w.path(w.a1, ""), p, nil), http.StatusOK, "directory").JSON(t)["items"].([]any); len(n) != 0 {
		t.Fatalf("rejected creates must leave nothing behind, got %d factions", len(n))
	}

	// Text is stored as plain content: markup is kept verbatim (never executed or stripped) and
	// only JSON-escaped on the wire; the recruitment status defaults to CLOSED.
	evil := "<script>alert(1)</script> @everyone <@123> & \"quotes\""
	got := w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, map[string]any{"name": "Plain Text", "tag": "pt", "description": evil}), http.StatusCreated, "create").JSON(t)
	if got["description"] != evil || got["recruitmentStatus"] != "CLOSED" {
		t.Fatalf("plain-text storage: %v", got)
	}
	if ct := w.do(http.MethodGet, w.path(w.a1, fmt.Sprintf("/%d", idOf(got))), p, nil).ContentType; !strings.Contains(ct, "application/json") {
		t.Fatal("responses must be application/json")
	}
	// The directory preview is single-line and length-limited.
	long := w.createFaction(w.a1, w.players[1], "Long Winded", "LW", "OPEN")
	_ = long
	w.expect(w.do(http.MethodPut, w.path(w.a1, fmt.Sprintf("/%d", idOf(long))), w.players[1], map[string]any{"description": strings.Repeat("word ", 90) + "\nsecond line"}), http.StatusOK, "long description")
	dir := w.expect(w.do(http.MethodGet, w.path(w.a1, "?q=long"), p, nil), http.StatusOK, "search").JSON(t)["items"].([]any)
	prev := dir[0].(map[string]any)["descriptionPreview"].(string)
	if len([]rune(prev)) > 140 || strings.Contains(prev, "\n") {
		t.Fatalf("preview must be one line of at most 140 runes: %q", prev)
	}

	// Directory parameters.
	w.expect(w.do(http.MethodGet, w.path(w.a1, "?limit=0"), p, nil), http.StatusBadRequest, "limit=0")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "?limit=abc"), p, nil), http.StatusBadRequest, "limit=abc")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "?cursor=bogus"), p, nil), http.StatusBadRequest, "bad cursor")
	w.expect(w.do(http.MethodGet, w.path(w.a1, "?q="+strings.Repeat("a", 60)), p, nil), http.StatusBadRequest, "search too long")
	if r := w.expect(w.do(http.MethodGet, w.path(w.a1, "?limit=5000"), p, nil), http.StatusOK, "limit clamps"); r.JSON(t)["limit"].(float64) != 100 {
		t.Fatalf("limit must clamp to 100: %v", r.JSON(t))
	}
}

func TestFactionAPIDirectoryPagination(t *testing.T) {
	w := newFactionWorld(t)
	for i := 0; i < 5; i++ {
		w.createFaction(w.a1, w.players[i], fmt.Sprintf("Squad %c", 'A'+i), fmt.Sprintf("S%c", 'A'+i), "OPEN")
	}
	seen := map[float64]bool{}
	cursor, pages := "", 0
	for {
		path := "?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := w.expect(w.do(http.MethodGet, w.path(w.a1, path), w.players[5], nil), http.StatusOK, "page").JSON(t)
		for _, it := range page["items"].([]any) {
			id := it.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatalf("faction %v appeared on two pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := page["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("expected 5 factions over 3 pages, got %d over %d", len(seen), pages)
	}
	// Search: case-insensitive on name and tag.
	if it := w.expect(w.do(http.MethodGet, w.path(w.a1, "?q=SQUAD%20c"), w.players[5], nil), http.StatusOK, "search").JSON(t)["items"].([]any); len(it) != 1 {
		t.Fatalf("name search: %v", it)
	}
	if it := w.expect(w.do(http.MethodGet, w.path(w.a1, "?q=sd"), w.players[5], nil), http.StatusOK, "tag search").JSON(t)["items"].([]any); len(it) != 1 {
		t.Fatalf("tag search: %v", it)
	}
}

// --- authorization: organization role is not faction role ---------------------------------------------

func TestFactionAPIOrganizationRolesAreNotFactionRoles(t *testing.T) {
	w := newFactionWorld(t)
	leader, applicant := w.players[0], w.players[1]
	f := w.createFaction(w.a1, leader, "Separate Powers", "SP", "OPEN")
	fp := fmt.Sprintf("/%d", idOf(f))
	app := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": "hi"}), http.StatusCreated, "apply").JSON(t)
	appPath := fmt.Sprintf("%s/applications/%d", fp, idOf(app))
	memberID := func() string {
		w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/accept"), leader, nil), http.StatusOK, "accept")
		mem := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/members"), leader, nil), http.StatusOK, "members").JSON(t)
		return fmt.Sprint(int64(mem["items"].([]any)[1].(map[string]any)["id"].(float64)))
	}
	// Before accepting: the org owner / admin / member vs the pending application.
	for who, actor := range map[string]string{"org OWNER": w.a1.OwnerDiscordID, "org ADMIN": w.admin} {
		lst := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications"), actor, nil), http.StatusOK, who+" read-only moderation view").JSON(t)
		if len(lst["items"].([]any)) != 1 {
			t.Fatalf("%s should see the application", who)
		}
		prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), actor, nil), http.StatusOK, who+" profile").JSON(t)
		if v := prof["viewer"].(map[string]any); v["moderationView"] != true || v["role"] != nil || v["canEdit"] != false || v["canManageApplications"] != false {
			t.Fatalf("%s must get a moderation view but no faction rights: %v", who, v)
		}
		w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/accept"), actor, nil), http.StatusForbidden, who+" accepting")
		w.expect(w.do(http.MethodPost, w.path(w.a1, appPath+"/deny"), actor, nil), http.StatusForbidden, who+" denying")
		w.expect(w.do(http.MethodPut, w.path(w.a1, fp), actor, map[string]any{"description": "hijack"}), http.StatusForbidden, who+" editing")
	}
	// An ordinary org MEMBER (not a faction officer) gets no moderation view.
	w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/applications"), w.member, nil), http.StatusForbidden, "org MEMBER listing")
	mid := memberID()
	for who, actor := range map[string]string{"org OWNER": w.a1.OwnerDiscordID, "org ADMIN": w.admin} {
		w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/members/"+mid+"/promote"), actor, nil), http.StatusForbidden, who+" promoting")
		w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/members/"+mid), actor, nil), http.StatusForbidden, who+" removing")
	}
	// An org admin can still found or join a faction as a player - through explicit faction membership.
	adminFaction := w.createFaction(w.a1, w.admin, "Admin Squad", "AS", "OPEN")
	if adminFaction["viewer"].(map[string]any)["role"] != "LEADER" {
		t.Fatal("an org admin who founds a faction is its LEADER through explicit membership")
	}
}

func TestFactionAPITenantIsolation(t *testing.T) {
	w := newFactionWorld(t)
	leaderA, leaderB, outsider := w.players[0], w.players[1], w.players[2]
	fa := w.createFaction(w.a1, leaderA, "Alpha Tenant", "AT", "OPEN")
	fb := w.createFaction(w.b1, leaderB, "Alpha Tenant", "AT", "OPEN") // the same name/tag in another organization is fine
	if idOf(fa) == idOf(fb) || fa["slug"] != fb["slug"] {
		t.Fatalf("same name on two tenants must produce two factions with the same slug: %v %v", fa["slug"], fb["slug"])
	}
	appA := w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("/%d/applications", idOf(fa))), outsider, nil), http.StatusCreated, "apply in A").JSON(t)

	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}  // org B's id on installation A
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID} // installation B under org A
	fidA := fmt.Sprintf("/%d", idOf(fa))
	appPath := fmt.Sprintf("%s/applications/%d", fidA, idOf(appA))
	memberPath := fidA + "/members/1"
	probes := []struct {
		method, suffix string
		body           any
	}{
		{http.MethodGet, "", nil}, {http.MethodGet, "/me", nil}, {http.MethodPost, "", map[string]any{"name": "Probe Faction", "tag": "PF"}},
		{http.MethodGet, fidA, nil}, {http.MethodPut, fidA, map[string]any{"description": "x"}},
		{http.MethodGet, fidA + "/members", nil}, {http.MethodGet, fidA + "/applications", nil}, {http.MethodPost, fidA + "/applications", nil},
		{http.MethodPost, appPath + "/accept", nil}, {http.MethodPost, appPath + "/deny", nil}, {http.MethodPost, appPath + "/withdraw", nil},
		{http.MethodPost, memberPath + "/promote", nil}, {http.MethodPost, memberPath + "/demote", nil}, {http.MethodDelete, memberPath, nil},
	}
	for _, scope := range []struct {
		label string
		f     installationFixture
	}{{"org B id on installation A", crossOrg}, {"installation B under org A", crossInst}} {
		for _, p := range probes {
			for _, actor := range []string{leaderA, leaderB, w.a1.OwnerDiscordID} {
				r := w.do(p.method, w.path(scope.f, p.suffix), actor, p.body)
				if r.Status != http.StatusNotFound {
					t.Errorf("%s %s (%s) as %s: want 404, got %d %s", p.method, p.suffix, scope.label, actor, r.Status, r.Body)
				}
			}
		}
	}
	// Tenant B's faction id under tenant A's path is also a 404 (no leakage of another tenant's factions).
	w.expect(w.do(http.MethodGet, w.path(w.a1, fmt.Sprintf("/%d", idOf(fb))), leaderA, nil), http.StatusNotFound, "B's faction under A")
	w.expect(w.do(http.MethodPut, w.path(w.a1, fmt.Sprintf("/%d", idOf(fb))), leaderA, map[string]any{"description": "steal"}), http.StatusNotFound, "editing B's faction via A")
	w.expect(w.do(http.MethodGet, w.path(w.b1, fidA), leaderB, nil), http.StatusNotFound, "A's faction under B")
	// Directories never mix tenants.
	for f, want := range map[installationFixture]float64{w.a1: float64(idOf(fa)), w.b1: float64(idOf(fb))} {
		dir := w.expect(w.do(http.MethodGet, w.path(f, ""), outsider, nil), http.StatusOK, "directory").JSON(t)["items"].([]any)
		if len(dir) != 1 || dir[0].(map[string]any)["id"].(float64) != want {
			t.Fatalf("directory leaked across tenants: %v", dir)
		}
	}
	// One user, two tenants, two independent standings.
	w.expect(w.do(http.MethodPost, w.path(w.b1, fmt.Sprintf("/%d/applications", idOf(fb))), outsider, nil), http.StatusCreated, "same user applies in B")
	// The probes changed nothing in A.
	prof := w.expect(w.do(http.MethodGet, w.path(w.a1, fidA), leaderA, nil), http.StatusOK, "A profile").JSON(t)
	if prof["description"] != "We hold Alpha Tenant" || prof["memberCount"].(float64) != 1 {
		t.Fatalf("probes must not change tenant A: %v", prof)
	}
}

func TestFactionAPIRateLimits(t *testing.T) {
	w := newFactionWorld(t)
	w.a.saasFactionCreateLimiter = newSaaSRateLimiter(time.Hour, 1)
	w.a.saasFactionApplyLimiter = newSaaSRateLimiter(time.Hour, 2)
	p, q := w.players[0], w.players[1]

	w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, map[string]any{"name": "First Try", "tag": "FT"}), http.StatusCreated, "first create")
	r := w.expect(w.do(http.MethodPost, w.path(w.a1, ""), p, map[string]any{"name": "Second Try", "tag": "ST"}), http.StatusTooManyRequests, "second create inside the cooldown")
	if r.errCode(t) != "RATE_LIMITED" {
		t.Fatalf("rate limit error code: %s", r.Body)
	}
	// The limit is per user: another player is unaffected.
	target := w.createFaction(w.a1, w.players[4], "Target Practice", "TP", "OPEN")
	tp := fmt.Sprintf("/%d/applications", idOf(target))
	w.expect(w.do(http.MethodPost, w.path(w.a1, tp), q, nil), http.StatusCreated, "apply 1")
	w.expect(w.do(http.MethodPost, w.path(w.a1, tp), q, nil), http.StatusConflict, "apply 2 (duplicate, but it counts as an attempt)")
	w.expect(w.do(http.MethodPost, w.path(w.a1, tp), q, nil), http.StatusTooManyRequests, "application spam")
	w.expect(w.do(http.MethodPost, w.path(w.a1, tp), w.players[5], nil), http.StatusCreated, "another applicant is unaffected")
}

func TestFactionAPIAuditLogsCarryOnlySafeIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := newFactionWorld(t)
	leader, applicant := w.players[0], w.players[1]
	secretName, secretMsg, secretDesc := "Confidential Unit Name", "my private application message", "private faction description text"
	f := w.expect(w.do(http.MethodPost, w.path(w.a1, ""), leader, map[string]any{"name": secretName, "tag": "CU", "description": secretDesc, "recruitmentStatus": "OPEN"}), http.StatusCreated, "create").JSON(t)
	fp := fmt.Sprintf("/%d", idOf(f))
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"description": secretDesc + " v2"}), http.StatusOK, "update")
	a1 := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": secretMsg}), http.StatusCreated, "apply").JSON(t)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/accept", fp, idOf(a1))), leader, nil), http.StatusOK, "accept")
	mem := w.expect(w.do(http.MethodGet, w.path(w.a1, fp+"/members"), leader, nil), http.StatusOK, "members").JSON(t)
	mp := fmt.Sprintf("%s/members/%d", fp, int64(mem["items"].([]any)[1].(map[string]any)["id"].(float64)))
	w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/promote"), leader, nil), http.StatusOK, "promote")
	w.expect(w.do(http.MethodPost, w.path(w.a1, mp+"/demote"), leader, nil), http.StatusOK, "demote")
	w.expect(w.do(http.MethodDelete, w.path(w.a1, mp), leader, nil), http.StatusOK, "remove")
	a2 := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": secretMsg + " 2"}), http.StatusCreated, "apply 2").JSON(t)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/withdraw", fp, idOf(a2))), applicant, nil), http.StatusOK, "withdraw")
	a3 := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), applicant, map[string]any{"message": secretMsg + " 3"}), http.StatusCreated, "apply 3").JSON(t)
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("%s/applications/%d/deny", fp, idOf(a3))), leader, nil), http.StatusOK, "deny")

	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	for _, ev := range []string{"faction_created", "faction_updated", "faction_application_created", "faction_application_accepted", "faction_application_denied",
		"faction_application_withdrawn", "faction_member_promoted", "faction_member_demoted", "faction_member_removed"} {
		if !strings.Contains(logs, "event="+ev) {
			t.Errorf("missing audit event %s", ev)
		}
	}
	for _, secret := range []string{secretName, secretMsg, secretDesc, "test-secret"} {
		if strings.Contains(logs, secret) {
			t.Errorf("audit logs must not contain user-written text or secrets: found %q", secret)
		}
	}
	for _, id := range []string{"acting_user_id=", "installation_id=", "organization_id=", "faction_id="} {
		if !strings.Contains(logs, id) {
			t.Errorf("audit events must carry %s", id)
		}
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestFactionAPINoServerAndSuspendedInstallation(t *testing.T) {
	w := newFactionWorld(t)
	p := w.players[0]
	f := w.createFaction(w.a1, p, "Before Suspension", "BS", "OPEN")
	// Reads keep working, writes are refused with a fixed message, on a suspended installation.
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE installations SET status='SUSPENDED' WHERE id=$1`, w.a1.InstallationID); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodGet, w.path(w.a1, ""), w.players[1], nil), http.StatusOK, "directory while suspended")
	r := w.expect(w.do(http.MethodPost, w.path(w.a1, ""), w.players[1], map[string]any{"name": "Late Founder", "tag": "LF"}), http.StatusConflict, "create while suspended")
	if !strings.Contains(string(r.Body), "suspended") {
		t.Fatalf("message: %s", r.Body)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("/%d/applications", idOf(f))), w.players[1], nil), http.StatusConflict, "apply while suspended")
	// An installation without a DayZ server cannot host factions (they belong to a server context).
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE installations SET status='READY' WHERE id=$1`, w.b1.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE installations SET game_server_id=NULL WHERE id=$1`, w.b1.InstallationID); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, w.path(w.b1, ""), p, map[string]any{"name": "No Server", "tag": "NS"}), http.StatusConflict, "create without a server")
	w.expect(w.do(http.MethodGet, w.path(w.b1, ""), p, nil), http.StatusOK, "empty directory without a server")
}
