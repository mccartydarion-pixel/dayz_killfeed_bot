//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeDiscordVerifier lets these tests exercise the full SaaS Discord
// handlers without a live Discord session - see the discordGuildVerifier
// interface in saas_api_discord.go, which exists specifically so this is
// possible.
type fakeDiscordVerifier struct {
	guildFound   map[string]bool
	channelFound map[string]bool
	missing      map[string][]string
}

func (f *fakeDiscordVerifier) Verify(guildID, channelID string) discord.Verification {
	v := discord.Verification{GuildFound: f.guildFound[guildID]}
	if channelID == "" {
		return v
	}
	key := guildID + "|" + channelID
	v.ChannelFound = f.channelFound[key]
	if v.ChannelFound {
		v.Missing = f.missing[key]
	}
	return v
}

func saasIntegrationApp(t *testing.T) (*App, *fakeDiscordVerifier) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	verifier := &fakeDiscordVerifier{guildFound: map[string]bool{}, channelFound: map[string]bool{}, missing: map[string][]string{}}
	a := &App{
		Config:               &config.Config{WebsiteAPISecret: "test-secret"},
		DB:                   db,
		Guilds:               repository.NewGuildRepository(db.Pool),
		SaaSUsers:            repository.NewUserRepository(db.Pool),
		SaaSOrganizations:    repository.NewOrganizationRepository(db.Pool),
		SaaSGuildConnections: repository.NewGuildConnectionRepository(db.Pool),
		SaaSServers:          repository.NewSaaSServerRepository(db.Pool),
		SaaSInstallations:    repository.NewInstallationRepository(db.Pool),
		SaaSSubscriptions:    repository.NewSubscriptionRepository(db.Pool),
		saasDiscordVerifier:  verifier,
	}
	return a, verifier
}

// --- request helpers -------------------------------------------------------

func saasRequest(method, path string, body any) *http.Request {
	var reader strings.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = *strings.NewReader(string(data))
	}
	req := httptest.NewRequest(method, path, &reader)
	req.Header.Set("Authorization", "Bearer test-secret")
	return req
}

func withActingUser(req *http.Request, discordUserID string) *http.Request {
	req.Header.Set(actingUserHeader, discordUserID)
	return req
}

func withPathValues(req *http.Request, kv map[string]string) *http.Request {
	for k, v := range kv {
		req.SetPathValue(k, v)
	}
	return req
}

func decodeBody[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %s: %v", rr.Body.String(), err)
	}
	return out
}

// syncUser is a small fixture helper wrapping the real handleUserSync
// handler, so every test that needs a synced user exercises case B's own
// code path rather than reaching into the repository directly.
func syncUser(t *testing.T, a *App, discordUserID, username string) UserSummary {
	t.Helper()
	req := saasRequest(http.MethodPost, "/api/saas/users/sync", userSyncRequest{
		DiscordUserID:   discordUserID,
		DiscordUsername: username,
	})
	rr := httptest.NewRecorder()
	a.handleUserSync(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("sync user: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	return decodeBody[UserSummary](t, rr)
}

// --- case B: Discord user sync ---------------------------------------------

func TestUserSyncUpsertsAndNeverDuplicates(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	discordID := fmt.Sprintf("api-user-%d", suffix)

	first := syncUser(t, a, discordID, "Original")
	if first.DiscordUserID != discordID || first.DiscordUsername != "Original" {
		t.Fatalf("unexpected first sync result: %+v", first)
	}
	second := syncUser(t, a, discordID, "Renamed")
	if second.ID != first.ID {
		t.Fatalf("expected the same user row across syncs, got %d and %d", first.ID, second.ID)
	}
	if second.DiscordUsername != "Renamed" {
		t.Fatalf("expected sync to update the username, got %q", second.DiscordUsername)
	}
}

// --- case C: organization create + owner membership -------------------------

func TestCreateOrganizationCreatesOwnerMembership(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("api-owner-%d", suffix), "Owner")

	req := withActingUser(saasRequest(http.MethodPost, "/api/saas/organizations", createOrganizationRequest{
		Name: "API Test Org", Slug: fmt.Sprintf("api-test-org-%d", suffix),
	}), owner.DiscordUserID)
	rr := httptest.NewRecorder()
	a.handleCreateOrganization(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	org := decodeBody[OrganizationSummary](t, rr)
	if org.Role != repository.RoleOwner {
		t.Fatalf("expected OWNER role in the create response, got %q", org.Role)
	}

	role, ok, err := a.SaaSOrganizations.VerifyMembership(context.Background(), org.ID, mustAppUserID(t, a, owner.DiscordUserID))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || role != repository.RoleOwner {
		t.Fatalf("expected the creator's OWNER membership to exist, got ok=%v role=%q", ok, role)
	}
}

func mustAppUserID(t *testing.T, a *App, discordUserID string) int64 {
	t.Helper()
	u, err := a.SaaSUsers.GetByDiscordID(context.Background(), discordUserID)
	if err != nil || u == nil {
		t.Fatalf("expected a synced user for %s: %v", discordUserID, err)
	}
	return u.ID
}

func mustCreateOrg(t *testing.T, a *App, ownerDiscordID, name, slug string) OrganizationSummary {
	t.Helper()
	req := withActingUser(saasRequest(http.MethodPost, "/api/saas/organizations", createOrganizationRequest{Name: name, Slug: slug}), ownerDiscordID)
	rr := httptest.NewRecorder()
	a.handleCreateOrganization(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create organization: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	return decodeBody[OrganizationSummary](t, rr)
}

// --- case D: list organizations scoped to user -------------------------------

func TestListOrganizationsScopedToActingUser(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	userA := syncUser(t, a, fmt.Sprintf("api-list-a-%d", suffix), "A")
	userB := syncUser(t, a, fmt.Sprintf("api-list-b-%d", suffix), "B")
	org := mustCreateOrg(t, a, userA.DiscordUserID, "List Org", fmt.Sprintf("list-org-%d", suffix))

	reqA := withActingUser(saasRequest(http.MethodGet, "/api/saas/organizations", nil), userA.DiscordUserID)
	rrA := httptest.NewRecorder()
	a.handleListOrganizations(rrA, reqA)
	listA := decodeBody[[]OrganizationSummary](t, rrA)
	if !containsOrgID(listA, org.ID) {
		t.Fatalf("expected user A's list to include their own organization, got %+v", listA)
	}

	reqB := withActingUser(saasRequest(http.MethodGet, "/api/saas/organizations", nil), userB.DiscordUserID)
	rrB := httptest.NewRecorder()
	a.handleListOrganizations(rrB, reqB)
	listB := decodeBody[[]OrganizationSummary](t, rrB)
	if containsOrgID(listB, org.ID) {
		t.Fatalf("expected user B's list to NOT include user A's organization, got %+v", listB)
	}
}

func containsOrgID(list []OrganizationSummary, id int64) bool {
	for _, o := range list {
		if o.ID == id {
			return true
		}
	}
	return false
}

// --- case E: unauthorized organization access blocked ------------------------

func TestUnauthorizedOrganizationAccessBlocked(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("api-e-owner-%d", suffix), "Owner")
	outsider := syncUser(t, a, fmt.Sprintf("api-e-outsider-%d", suffix), "Outsider")
	org := mustCreateOrg(t, a, owner.DiscordUserID, "Private Org", fmt.Sprintf("private-org-%d", suffix))

	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/api/saas/organizations/"+strconv.FormatInt(org.ID, 10), nil), outsider.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	rr := httptest.NewRecorder()
	a.handleGetOrganization(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-member, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeForbidden)

	// The dashboard must refuse the same way.
	req2 := withPathValues(withActingUser(saasRequest(http.MethodGet, "/api/saas/organizations/"+strconv.FormatInt(org.ID, 10)+"/dashboard", nil), outsider.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	rr2 := httptest.NewRecorder()
	a.handleDashboard(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("expected dashboard 403 for a non-member, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// --- fixture: a fully wired installation ------------------------------------

type installationFixture struct {
	OwnerDiscordID string
	OrgID          int64
	DiscordGuildID string
	ConnectionID   int64
	InstallationID int64
}

// buildInstallationFixture drives the real Discord-connection and
// installation-creation handlers (not a repository shortcut) to reach a
// realistic pre-verification state: an org, a connected+eligible guild, and
// an installation in NOT_STARTED.
func buildInstallationFixture(t *testing.T, a *App, verifier *fakeDiscordVerifier) installationFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("api-fixture-owner-%d", suffix), "Owner")
	org := mustCreateOrg(t, a, owner.DiscordUserID, "Fixture Org", fmt.Sprintf("fixture-org-%d", suffix))

	discordGuildID := fmt.Sprintf("guild-%d", suffix)
	verifier.guildFound[discordGuildID] = true

	connReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/api/saas/organizations/"+strconv.FormatInt(org.ID, 10)+"/discord/connection", connectGuildRequest{
		DiscordGuildID: discordGuildID,
		GuildName:      "Fixture Guild",
		Permissions:    strconv.FormatInt(int64(requiredGuildPermissions), 10),
	}), owner.DiscordUserID), map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	connRR := httptest.NewRecorder()
	a.handleConnectDiscordGuild(connRR, connReq)
	if connRR.Code != http.StatusOK {
		t.Fatalf("connect guild: expected 200, got %d: %s", connRR.Code, connRR.Body.String())
	}
	conn := decodeBody[DiscordGuildConnectionSummary](t, connRR)

	instReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/api/saas/organizations/"+strconv.FormatInt(org.ID, 10)+"/installations", createInstallationRequest{
		DiscordGuildConnectionID: conn.ID,
	}), owner.DiscordUserID), map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	instRR := httptest.NewRecorder()
	a.handleCreateInstallation(instRR, instReq)
	if instRR.Code != http.StatusCreated {
		t.Fatalf("create installation: expected 201, got %d: %s", instRR.Code, instRR.Body.String())
	}
	inst := decodeBody[InstallationSummary](t, instRR)

	return installationFixture{OwnerDiscordID: owner.DiscordUserID, OrgID: org.ID, DiscordGuildID: discordGuildID, ConnectionID: conn.ID, InstallationID: inst.ID}
}

// --- cases F/G: setup progress role gating ----------------------------------

func TestMemberCannotModifySetupProgressButOwnerCan(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	suffix := time.Now().UnixNano()
	member := syncUser(t, a, fmt.Sprintf("api-member-%d", suffix), "Member")
	if _, err := a.DB.Pool.Exec(context.Background(), `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,'MEMBER')`, fixture.OrgID, mustAppUserID(t, a, member.DiscordUserID)); err != nil {
		t.Fatal(err)
	}

	patch := setupProgressPatchRequest{CurrentStep: strPtr("NITRADO")}
	orgIDStr := strconv.FormatInt(fixture.OrgID, 10)
	instIDStr := strconv.FormatInt(fixture.InstallationID, 10)

	// F: MEMBER is blocked.
	memberReq := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", patch), member.DiscordUserID),
		map[string]string{"organizationID": orgIDStr, "installationID": instIDStr})
	memberRR := httptest.NewRecorder()
	a.handleUpdateSetupProgress(memberRR, memberReq)
	if memberRR.Code != http.StatusForbidden {
		t.Fatalf("expected MEMBER setup mutation to be forbidden, got %d: %s", memberRR.Code, memberRR.Body.String())
	}

	// G: OWNER is allowed.
	ownerReq := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", patch), fixture.OwnerDiscordID),
		map[string]string{"organizationID": orgIDStr, "installationID": instIDStr})
	ownerRR := httptest.NewRecorder()
	a.handleUpdateSetupProgress(ownerRR, ownerReq)
	if ownerRR.Code != http.StatusOK {
		t.Fatalf("expected OWNER setup mutation to succeed, got %d: %s", ownerRR.Code, ownerRR.Body.String())
	}
	progress := decodeBody[SetupProgressSummary](t, ownerRR)
	if progress.CurrentStep != "NITRADO" {
		t.Fatalf("expected currentStep to update to NITRADO, got %q", progress.CurrentStep)
	}
}

func strPtr(s string) *string { return &s }

// --- case H: installation cross-tenant access blocked -------------------------

func TestInstallationCrossTenantAccessBlocked(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)

	// A guesses B's installation ID inside A's own organization context.
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), fixtureA.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureA.OrgID, 10), "installationID": strconv.FormatInt(fixtureB.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleGetInstallation(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected cross-tenant installation lookup to 404, got %d: %s", rr.Code, rr.Body.String())
	}

	// A is not even a member of B's organization at all.
	req2 := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), fixtureA.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureB.OrgID, 10), "installationID": strconv.FormatInt(fixtureB.InstallationID, 10)})
	rr2 := httptest.NewRecorder()
	a.handleGetInstallation(rr2, req2)
	if rr2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a non-member reaching into another org, got %d: %s", rr2.Code, rr2.Body.String())
	}
}

// --- case I: verified Discord guild connection persists -----------------------

func TestVerifiedDiscordGuildConnectionPersists(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("api-i-owner-%d", suffix), "Owner")
	org := mustCreateOrg(t, a, owner.DiscordUserID, "Connect Org", fmt.Sprintf("connect-org-%d", suffix))

	discordGuildID := fmt.Sprintf("guild-i-%d", suffix)
	verifier.guildFound[discordGuildID] = true

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{
		DiscordGuildID: discordGuildID,
		GuildName:      "Connect Guild",
		Permissions:    strconv.FormatInt(int64(requiredGuildPermissions), 10),
	}), owner.DiscordUserID), map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	rr := httptest.NewRecorder()
	a.handleConnectDiscordGuild(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	conn := decodeBody[DiscordGuildConnectionSummary](t, rr)
	if !conn.BotInstalled || !conn.PermissionsVerified {
		t.Fatalf("expected botInstalled and permissionsVerified true, got %+v", conn)
	}

	persisted, err := a.SaaSGuildConnections.GetScoped(context.Background(), org.ID, conn.ID)
	if err != nil || persisted == nil {
		t.Fatalf("expected the connection to be persisted: %v", err)
	}

	// An unverified (insufficient permission) selection must be rejected.
	badReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{
		DiscordGuildID: fmt.Sprintf("guild-bad-%d", suffix),
		Permissions:    "0",
	}), owner.DiscordUserID), map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	badRR := httptest.NewRecorder()
	a.handleConnectDiscordGuild(badRR, badReq)
	if badRR.Code != http.StatusForbidden {
		t.Fatalf("expected an unverified guild selection to be rejected, got %d: %s", badRR.Code, badRR.Body.String())
	}
}

// TestGuildConnectionConflictAcrossOrganizations is section 10's explicit
// cross-tenant safety requirement: a guild already claimed by organization A
// must never be silently reassigned to organization B.
func TestGuildConnectionConflictAcrossOrganizations(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	ownerA := syncUser(t, a, fmt.Sprintf("api-conflict-a-%d", suffix), "OwnerA")
	ownerB := syncUser(t, a, fmt.Sprintf("api-conflict-b-%d", suffix), "OwnerB")
	orgA := mustCreateOrg(t, a, ownerA.DiscordUserID, "Conflict Org A", fmt.Sprintf("conflict-org-a-%d", suffix))
	orgB := mustCreateOrg(t, a, ownerB.DiscordUserID, "Conflict Org B", fmt.Sprintf("conflict-org-b-%d", suffix))

	discordGuildID := fmt.Sprintf("guild-conflict-%d", suffix)
	verifier.guildFound[discordGuildID] = true
	permStr := strconv.FormatInt(int64(requiredGuildPermissions), 10)

	reqA := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{DiscordGuildID: discordGuildID, Permissions: permStr}), ownerA.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(orgA.ID, 10)})
	rrA := httptest.NewRecorder()
	a.handleConnectDiscordGuild(rrA, reqA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("expected org A to claim the guild, got %d: %s", rrA.Code, rrA.Body.String())
	}

	reqB := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{DiscordGuildID: discordGuildID, Permissions: permStr}), ownerB.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(orgB.ID, 10)})
	rrB := httptest.NewRecorder()
	a.handleConnectDiscordGuild(rrB, reqB)
	if rrB.Code != http.StatusConflict {
		t.Fatalf("expected org B's claim on the same guild to CONFLICT, got %d: %s", rrB.Code, rrB.Body.String())
	}
	assertErrorCode(t, rrB, codeConflict)

	// Org A reconnecting the same guild it already owns must still succeed
	// (an idempotent re-verify, not a conflict with itself).
	rrAAgain := httptest.NewRecorder()
	a.handleConnectDiscordGuild(rrAAgain, withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", connectGuildRequest{DiscordGuildID: discordGuildID, Permissions: permStr}), ownerA.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(orgA.ID, 10)}))
	if rrAAgain.Code != http.StatusOK {
		t.Fatalf("expected org A to re-verify its own guild without conflict, got %d: %s", rrAAgain.Code, rrAAgain.Body.String())
	}
}

// --- case J: bot installation verification -------------------------------------

func TestVerifyInstallationTransitionsStatus(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	before, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || before == nil || before.Status != repository.InstallationNotStarted {
		t.Fatalf("expected a fresh installation to start NOT_STARTED, got %+v err=%v", before, err)
	}

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleVerifyInstallation(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	result := decodeBody[DiscordVerificationResult](t, rr)
	if !result.Installed || !result.GuildReachable {
		t.Fatalf("expected installed and guildReachable true, got %+v", result)
	}

	after, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || after == nil || after.Status != repository.InstallationDiscordConnected {
		t.Fatalf("expected status to transition to DISCORD_CONNECTED, got %+v err=%v", after, err)
	}

	// A second verification of an already-DISCORD_CONNECTED installation
	// must not regress or otherwise misbehave.
	rr2 := httptest.NewRecorder()
	a.handleVerifyInstallation(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("expected re-verification to still succeed, got %d", rr2.Code)
	}
	still, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || still == nil || still.Status != repository.InstallationDiscordConnected {
		t.Fatalf("expected status to remain DISCORD_CONNECTED, got %+v err=%v", still, err)
	}
}

// --- bonus: permission verification end to end --------------------------------

func TestVerifyPermissionsRequiresPriorDiscordConnection(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", verifyPermissionsRequest{ChannelID: "chan-1"}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleVerifyPermissions(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected INSTALLATION_NOT_VERIFIED before Discord is connected, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)
}
