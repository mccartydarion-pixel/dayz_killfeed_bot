//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Client Admin Control Plane Phase 1 end-to-end tests (docs/CLIENT_ADMIN.md): permission
// resolution (organization-owner bootstrap, Discord-role mapping, escalation safety), the audit
// log, and a representative slice of capability endpoints exercised through the real handlers
// against a real PostgreSQL. Discord and Nitrado are both faked - no live external call is ever
// made from this suite (task's explicit "FAKE PROVIDERS" requirement).

// clientAdminWorld wires a fresh *App with the Client Admin Control Plane repositories and routes
// on top of the shared saasIntegrationApp harness.
type clientAdminWorld struct {
	t                 *testing.T
	a                 *App
	verifier          *fakeDiscordVerifier
	f                 installationFixture
	guildID           int64  // internal guilds.id for f
	serverID          int64  // internal game_servers.id attached to f
	providerServiceID string // game_servers.provider_service_id - the Nitrado-side service id
}

func newClientAdminWorld(t *testing.T) *clientAdminWorld {
	t.Helper()
	a, verifier := saasIntegrationApp(t)
	a.Permissions = repository.NewPermissionsRepository(a.DB.Pool)
	a.AdminAudit = repository.NewAuditRepository(a.DB.Pool)
	a.ClientAdmin = repository.NewClientAdminRepository(a.DB.Pool)
	a.Servers = repository.NewServerRepository(a.DB.Pool)
	a.Bounties = repository.NewBountyRepository(a.DB.Pool)
	a.Factions = repository.NewFactionRepository(a.DB.Pool)
	a.Streaks = repository.NewStreakRepository(a.DB.Pool)
	a.Seasons = repository.NewSeasonRepository(a.DB.Pool)
	a.registerClientAdminRoutes()

	f := buildInstallationFixture(t, a, verifier)
	ctx := context.Background()
	var guildID, serverID int64
	if err := a.DB.Pool.QueryRow(ctx, `SELECT c.guild_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, f.InstallationID).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	providerServiceID := fmt.Sprintf("ca-%d", time.Now().UnixNano())
	if err := a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status, organization_id) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3) RETURNING id`,
		guildID, providerServiceID, f.OrgID).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$1 WHERE id=$2`, serverID, f.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, `INSERT INTO server_configs(server_id) VALUES($1) ON CONFLICT DO NOTHING`, serverID); err != nil {
		t.Fatal(err)
	}
	return &clientAdminWorld{t: t, a: a, verifier: verifier, f: f, guildID: guildID, serverID: serverID, providerServiceID: providerServiceID}
}

func (w *clientAdminWorld) path(suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/admin%s", w.f.OrgID, w.f.InstallationID, suffix)
}

func (w *clientAdminWorld) pathValues() map[string]string {
	return map[string]string{"organizationID": strconv.FormatInt(w.f.OrgID, 10), "installationID": strconv.FormatInt(w.f.InstallationID, 10)}
}

// call invokes handler directly (Go 1.22 mux path values injected manually, matching this
// package's existing convention - see connectNitrado/syncUser) with extra path values merged in.
func (w *clientAdminWorld) call(handler http.HandlerFunc, method, path, actor string, body any, extra map[string]string) *httptest.ResponseRecorder {
	w.t.Helper()
	pv := w.pathValues()
	for k, v := range extra {
		pv[k] = v
	}
	req := withPathValues(withActingUser(saasRequest(method, path, body), actor), pv)
	rr := httptest.NewRecorder()
	handler(rr, req)
	return rr
}

func (w *clientAdminWorld) seedPlayer(name string) int64 {
	w.t.Helper()
	var id int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		w.guildID, fmt.Sprintf("dayz-%d", time.Now().UnixNano()), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// withFakeNitradoActions points the org's Nitrado client factory at a local server handling both
// the connect-time services listing AND the action endpoints (restart/stop/whitelist/banlist)
// this suite's handlers call - never a live Nitrado service.
func withFakeNitradoActions(t *testing.T, a *App) *[]string {
	t.Helper()
	calls := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/services" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(psServiceJSON))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client {
		return nitrado.NewClient(srv.URL, token, nil)
	}
	return calls
}

// --- permission resolution ------------------------------------------------------------------------

func TestOrgOwnerBootstrapsToLevelOwner(t *testing.T) {
	w := newClientAdminWorld(t)
	rr := w.call(w.a.handleListPermissions, http.MethodGet, w.path("/permissions"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the organization owner, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[map[string]any](t, rr)
	if body["actorLevel"] != "OWNER" {
		t.Fatalf("expected actorLevel OWNER, got %v", body["actorLevel"])
	}
}

func TestNonMemberWithNoRoleMappingIsForbidden(t *testing.T) {
	w := newClientAdminWorld(t)
	stranger := syncUser(t, w.a, fmt.Sprintf("ca-stranger-%d", time.Now().UnixNano()), "Stranger")
	rr := w.call(w.a.handleListPermissions, http.MethodGet, w.path("/permissions"), stranger.DiscordUserID, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor with no org ownership and no Discord role mapping, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestDiscordRoleMappingGrantsCapabilityAtItsLevelOnly(t *testing.T) {
	w := newClientAdminWorld(t)
	modUser := syncUser(t, w.a, fmt.Sprintf("ca-mod-%d", time.Now().UnixNano()), "Mod")
	roleID := "discord-role-moderator"
	if w.verifier.memberRoles == nil {
		w.verifier.memberRoles = map[string]map[string][]string{}
	}
	if w.verifier.memberRoles[w.f.DiscordGuildID] == nil {
		w.verifier.memberRoles[w.f.DiscordGuildID] = map[string][]string{}
	}
	w.verifier.memberRoles[w.f.DiscordGuildID][modUser.DiscordUserID] = []string{roleID}

	// Map the role to MODERATOR as the owner.
	setRR := w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/"+roleID), w.f.OwnerDiscordID, setPermissionRequest{Level: "MODERATOR"}, map[string]string{"discordRoleID": roleID})
	if setRR.Code != http.StatusOK {
		t.Fatalf("owner setting a MODERATOR mapping should succeed: %d %s", setRR.Code, setRR.Body.String())
	}

	// The MODERATOR-mapped user can list warnings (WARNINGS_VIEW = Moderator)...
	playerID := w.seedPlayer("Target")
	listRR := w.call(w.a.handleListWarnings, http.MethodGet, w.path(fmt.Sprintf("/warnings/%d", playerID)), modUser.DiscordUserID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if listRR.Code != http.StatusOK {
		t.Fatalf("moderator-level actor should be able to list warnings: %d %s", listRR.Code, listRR.Body.String())
	}

	// ...but cannot clear one (WARNINGS_CLEAR = Administrator).
	clearRR := w.call(w.a.handleClearWarning, http.MethodPost, w.path("/warnings/x/1/clear"), modUser.DiscordUserID, nil, map[string]string{"warningID": "1"})
	if clearRR.Code != http.StatusForbidden {
		t.Fatalf("moderator-level actor must NOT be able to clear warnings (Administrator-only), got %d: %s", clearRR.Code, clearRR.Body.String())
	}
}

func TestModeratorCannotGrantOwnerEscalationDenied(t *testing.T) {
	w := newClientAdminWorld(t)
	modUser := syncUser(t, w.a, fmt.Sprintf("ca-esc-%d", time.Now().UnixNano()), "Mod")
	roleID := "discord-role-moderator-esc"
	w.verifier.memberRoles = map[string]map[string][]string{w.f.DiscordGuildID: {modUser.DiscordUserID: {roleID}}}
	if setRR := w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/"+roleID), w.f.OwnerDiscordID, setPermissionRequest{Level: "MODERATOR"}, map[string]string{"discordRoleID": roleID}); setRR.Code != http.StatusOK {
		t.Fatalf("owner setup failed: %d %s", setRR.Code, setRR.Body.String())
	}

	// The task's own explicit example: a Moderator can never grant Owner.
	escRR := w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/some-other-role"), modUser.DiscordUserID, setPermissionRequest{Level: "OWNER"}, map[string]string{"discordRoleID": "some-other-role"})
	if escRR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 ADMIN_ESCALATION_DENIED for a Moderator granting Owner, got %d: %s", escRR.Code, escRR.Body.String())
	}
}

// --- audit log ---------------------------------------------------------------------------------

func TestSuccessfulActionWritesAuditLogEntry(t *testing.T) {
	w := newClientAdminWorld(t)
	playerID := w.seedPlayer("AuditTarget")
	issueRR := w.call(w.a.handleIssueWarning, http.MethodPost, w.path(fmt.Sprintf("/warnings/%d", playerID)), w.f.OwnerDiscordID, issueWarningRequest{Reason: "combat logging"}, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if issueRR.Code != http.StatusCreated {
		t.Fatalf("issue warning failed: %d %s", issueRR.Code, issueRR.Body.String())
	}

	auditRR := w.call(w.a.handleListAuditLog, http.MethodGet, w.path("/audit-log"), w.f.OwnerDiscordID, nil, nil)
	if auditRR.Code != http.StatusOK {
		t.Fatalf("list audit log failed: %d %s", auditRR.Code, auditRR.Body.String())
	}
	body := decodeBody[map[string]any](t, auditRR)
	items, _ := body["items"].([]any)
	found := false
	for _, raw := range items {
		e, _ := raw.(map[string]any)
		if e["action"] == "WARNING_ISSUED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WARNING_ISSUED audit entry, got %v", items)
	}
}

// --- warnings preserve the audit trail on clear --------------------------------------------------

func TestClearWarningPreservesRowNeverDeletes(t *testing.T) {
	w := newClientAdminWorld(t)
	playerID := w.seedPlayer("WarnTarget")
	issueRR := w.call(w.a.handleIssueWarning, http.MethodPost, w.path(fmt.Sprintf("/warnings/%d", playerID)), w.f.OwnerDiscordID, issueWarningRequest{Reason: "team killing"}, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	issued := decodeBody[warningDTO](t, issueRR)

	clearRR := w.call(w.a.handleClearWarning, http.MethodPost, w.path(fmt.Sprintf("/warnings/%d/%d/clear", playerID, issued.ID)), w.f.OwnerDiscordID, nil, map[string]string{"warningID": strconv.FormatInt(issued.ID, 10)})
	if clearRR.Code != http.StatusOK {
		t.Fatalf("clear warning failed: %d %s", clearRR.Code, clearRR.Body.String())
	}
	cleared := decodeBody[warningDTO](t, clearRR)
	if !cleared.Cleared || cleared.ClearedAt == nil {
		t.Fatalf("expected cleared=true and a clearedAt timestamp, got %+v", cleared)
	}
	if cleared.Reason != issued.Reason {
		t.Fatalf("clearing must never change the original reason: got %q, want %q", cleared.Reason, issued.Reason)
	}

	listRR := w.call(w.a.handleListWarnings, http.MethodGet, w.path(fmt.Sprintf("/warnings/%d", playerID)), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	list := decodeBody[map[string]any](t, listRR)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("clearing a warning must never delete it - expected 1 row, got %d", len(items))
	}
}

// --- current-actor permission introspection (Client Admin Control Plane Phase 1 Part 2,
// docs/CLIENT_ADMIN.md "Current Actor Client Admin Permissions") -----------------------------------

// mapRole maps discordUserID's roleID to level in w's installation, as the organization owner.
func (w *clientAdminWorld) mapRole(discordUserID, roleID, level string) {
	w.t.Helper()
	if w.verifier.memberRoles == nil {
		w.verifier.memberRoles = map[string]map[string][]string{}
	}
	if w.verifier.memberRoles[w.f.DiscordGuildID] == nil {
		w.verifier.memberRoles[w.f.DiscordGuildID] = map[string][]string{}
	}
	w.verifier.memberRoles[w.f.DiscordGuildID][discordUserID] = append(w.verifier.memberRoles[w.f.DiscordGuildID][discordUserID], roleID)
	rr := w.call(w.a.handleSetPermission, http.MethodPut, w.path("/permissions/"+roleID), w.f.OwnerDiscordID, setPermissionRequest{Level: level}, map[string]string{"discordRoleID": roleID})
	if rr.Code != http.StatusOK {
		w.t.Fatalf("owner mapping role %s to %s failed: %d %s", roleID, level, rr.Code, rr.Body.String())
	}
}

func TestClientAdminMeOwnerBootstrapFullCapabilitySet(t *testing.T) {
	w := newClientAdminWorld(t)
	rr := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the organization owner, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[clientAdminMeResponse](t, rr)
	if body.Level != "OWNER" {
		t.Fatalf("expected level OWNER, got %q", body.Level)
	}
	want := permissions.CapabilitiesForLevel(permissions.LevelOwner)
	if len(body.Capabilities) != len(want) {
		t.Fatalf("expected the full OWNER capability set (%d), got %d: %v", len(want), len(body.Capabilities), body.Capabilities)
	}
	for i := range want {
		if body.Capabilities[i] != want[i] {
			t.Fatalf("capability set/order mismatch at %d: got %s, want %s", i, body.Capabilities[i], want[i])
		}
	}
}

func TestClientAdminMeResolvesEachMappedLevel(t *testing.T) {
	for _, level := range []string{"ADMINISTRATOR", "MODERATOR", "GATEKEEPER"} {
		t.Run(level, func(t *testing.T) {
			w := newClientAdminWorld(t)
			user := syncUser(t, w.a, fmt.Sprintf("ca-me-%s-%d", level, time.Now().UnixNano()), "Actor")
			w.mapRole(user.DiscordUserID, "role-"+level, level)

			rr := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), user.DiscordUserID, nil, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
			}
			body := decodeBody[clientAdminMeResponse](t, rr)
			if body.Level != level {
				t.Fatalf("expected level %s, got %q", level, body.Level)
			}
			l, _ := permissions.ParseLevel(level)
			want := permissions.CapabilitiesForLevel(l)
			if len(body.Capabilities) != len(want) {
				t.Fatalf("%s: expected %d capabilities, got %d: %v", level, len(want), len(body.Capabilities), body.Capabilities)
			}
			for i := range want {
				if body.Capabilities[i] != want[i] {
					t.Fatalf("%s: capability mismatch at %d: got %s, want %s", level, i, body.Capabilities[i], want[i])
				}
			}
			foundRole := false
			for _, r := range body.DiscordRoleIDs {
				if r == "role-"+level {
					foundRole = true
				}
			}
			if !foundRole {
				t.Fatalf("expected discordRoleIds to include the mapped role, got %v", body.DiscordRoleIDs)
			}
		})
	}
}

func TestClientAdminMeHighestMappedRoleWins(t *testing.T) {
	w := newClientAdminWorld(t)
	user := syncUser(t, w.a, fmt.Sprintf("ca-me-multi-%d", time.Now().UnixNano()), "Actor")
	w.mapRole(user.DiscordUserID, "role-gatekeeper-x", "GATEKEEPER")
	w.mapRole(user.DiscordUserID, "role-admin-x", "ADMINISTRATOR")
	w.mapRole(user.DiscordUserID, "role-moderator-x", "MODERATOR")

	rr := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), user.DiscordUserID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[clientAdminMeResponse](t, rr)
	if body.Level != "ADMINISTRATOR" {
		t.Fatalf("expected the highest mapped level (ADMINISTRATOR) to win, got %q", body.Level)
	}
}

func TestClientAdminMeNoMappedRoleIsForbidden(t *testing.T) {
	w := newClientAdminWorld(t)
	stranger := syncUser(t, w.a, fmt.Sprintf("ca-me-stranger-%d", time.Now().UnixNano()), "Stranger")
	rr := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), stranger.DiscordUserID, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 ADMIN_FORBIDDEN for an actor with no mapped level, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestClientAdminMeDifferentOrganizationIsForbidden(t *testing.T) {
	w1 := newClientAdminWorld(t)
	w2 := newClientAdminWorld(t)
	// w2's owner has no role/ownership in w1's organization/installation at all.
	rr := w1.call(w1.a.handleClientAdminMe, http.MethodGet, w1.path("/me"), w2.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor from an unrelated organization, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestClientAdminMeDiscordUnavailableFailsClosed(t *testing.T) {
	w := newClientAdminWorld(t)
	user := syncUser(t, w.a, fmt.Sprintf("ca-me-noDiscord-%d", time.Now().UnixNano()), "Actor")
	// Not the org owner, so resolution must fall through to a live Discord role lookup - which
	// fails closed to ADMIN_DISCORD_UNAVAILABLE when the verifier is unavailable, never silently
	// elevating or downgrading.
	saved := w.a.saasDiscordVerifier
	w.a.saasDiscordVerifier = nil
	defer func() { w.a.saasDiscordVerifier = saved }()

	rr := w.call(w.a.handleClientAdminMe, http.MethodGet, w.path("/me"), user.DiscordUserID, nil, nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 ADMIN_DISCORD_UNAVAILABLE, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[map[string]any](t, rr)
	errObj, _ := body["error"].(map[string]any)
	if errObj["code"] != "ADMIN_DISCORD_UNAVAILABLE" {
		t.Fatalf("expected error code ADMIN_DISCORD_UNAVAILABLE, got %v", errObj["code"])
	}
}

// --- restart / stop: confirmation + fake Nitrado enforcement ---------------------------------------

func TestRestartRequiresTypedConfirmation(t *testing.T) {
	w := newClientAdminWorld(t)
	withFakeNitradoActions(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)

	rr := w.call(w.a.handleServerRestart, http.MethodPost, w.path("/server/restart"), w.f.OwnerDiscordID, serverActionRequest{Reason: "patch"}, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 ADMIN_CONFIRMATION_REQUIRED without a typed confirmation, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRestartSucceedsWithConfirmationAndCallsNitrado(t *testing.T) {
	w := newClientAdminWorld(t)
	calls := withFakeNitradoActions(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)
	*calls = nil // drop the connect-time /services call from the assertion below

	rr := w.call(w.a.handleServerRestart, http.MethodPost, w.path("/server/restart"), w.f.OwnerDiscordID, serverActionRequest{Reason: "patch", Confirm: "RESTART"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restart with correct confirmation should succeed: %d %s", rr.Code, rr.Body.String())
	}
	found := false
	for _, c := range *calls {
		if c == "POST /services/"+w.providerServiceID+"/gameservers/restart" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the fake Nitrado server to see a restart call, got %v", *calls)
	}
}

// --- whitelist: Champion record + Nitrado enforcement roundtrip ------------------------------------

func TestWhitelistAddListRemoveRoundTrip(t *testing.T) {
	w := newClientAdminWorld(t)
	calls := withFakeNitradoActions(t, w.a)
	connectNitrado(t, w.a, w.f.OrgID, w.f.OwnerDiscordID)
	*calls = nil

	addRR := w.call(w.a.handleAddWhitelist, http.MethodPost, w.path("/whitelist"), w.f.OwnerDiscordID, accessEntryRequest{Identifier: "player-123", Reason: "vetted"}, nil)
	if addRR.Code != http.StatusCreated {
		t.Fatalf("add whitelist failed: %d %s", addRR.Code, addRR.Body.String())
	}
	sawAdd := false
	for _, c := range *calls {
		if c == "POST /services/"+w.providerServiceID+"/gameservers/games/whitelist" {
			sawAdd = true
		}
	}
	if !sawAdd {
		t.Fatalf("expected a Nitrado whitelist add call, got %v", *calls)
	}

	listRR := w.call(w.a.handleListWhitelist, http.MethodGet, w.path("/whitelist"), w.f.OwnerDiscordID, nil, nil)
	list := decodeBody[map[string]any](t, listRR)
	items, _ := list["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 active whitelist entry, got %d", len(items))
	}

	*calls = nil
	remRR := w.call(w.a.handleRemoveWhitelist, http.MethodDelete, w.path("/whitelist/player-123"), w.f.OwnerDiscordID, nil, map[string]string{"identifier": "player-123"})
	if remRR.Code != http.StatusOK {
		t.Fatalf("remove whitelist failed: %d %s", remRR.Code, remRR.Body.String())
	}
	sawRemove := false
	for _, c := range *calls {
		if c == "DELETE /services/"+w.providerServiceID+"/gameservers/games/whitelist" {
			sawRemove = true
		}
	}
	if !sawRemove {
		t.Fatalf("expected a Nitrado whitelist remove call, got %v", *calls)
	}

	listAfterRR := w.call(w.a.handleListWhitelist, http.MethodGet, w.path("/whitelist"), w.f.OwnerDiscordID, nil, nil)
	listAfter := decodeBody[map[string]any](t, listAfterRR)
	itemsAfter, _ := listAfter["items"].([]any)
	if len(itemsAfter) != 0 {
		t.Fatalf("expected 0 active whitelist entries after removal, got %d", len(itemsAfter))
	}
}

// --- season reset (resetEveryoneStats) requires typed confirmation and preserves history -----------

func TestResetEveryoneStatsRequiresConfirmationAndStartsNewSeason(t *testing.T) {
	w := newClientAdminWorld(t)
	if _, err := w.a.Seasons.EnsureDefaultSeason(context.Background(), w.guildID, time.Now()); err != nil {
		t.Fatal(err)
	}
	missingConfirm := w.call(w.a.handleResetEveryoneStats, http.MethodPost, w.path("/stats/reset-season"), w.f.OwnerDiscordID, resetSeasonRequest{Name: "Season 2"}, nil)
	if missingConfirm.Code != http.StatusConflict {
		t.Fatalf("expected 409 without typed confirmation, got %d: %s", missingConfirm.Code, missingConfirm.Body.String())
	}

	rr := w.call(w.a.handleResetEveryoneStats, http.MethodPost, w.path("/stats/reset-season"), w.f.OwnerDiscordID, resetSeasonRequest{Name: "Season 2", Confirm: "RESET EVERYONE"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reset with correct confirmation should succeed: %d %s", rr.Code, rr.Body.String())
	}
	history, err := w.a.Seasons.GetSeasonHistory(context.Background(), w.guildID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) < 2 {
		t.Fatalf("expected the old season to be preserved alongside the new one, got %d seasons", len(history))
	}
}
