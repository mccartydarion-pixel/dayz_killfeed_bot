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
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/security"
)

// fakeDiscordVerifier lets these tests exercise the full SaaS Discord
// handlers without a live Discord session - see the discordGuildVerifier
// interface in saas_api_discord.go, which exists specifically so this is
// possible.
type fakeDiscordVerifier struct {
	guildFound   map[string]bool
	channelFound map[string]bool
	missing      map[string][]string

	// verifyCalls counts live Verify calls, so tests can assert the
	// zero-network-call eligible-guilds path never invokes it (section 7).
	verifyCalls int
}

// BotID returns a fixed fake bot user ID - only used for diagnostic log
// fields in production, never for test assertions.
func (f *fakeDiscordVerifier) BotID() string { return "fake-bot-id" }

// HasGuildCached mirrors the production cache-only lookup, backed by the
// same guildFound map Verify uses (a real cache would agree with a live
// check for a guild the bot is actually in) - never increments verifyCalls.
func (f *fakeDiscordVerifier) HasGuildCached(guildID string) bool {
	return f.guildFound[guildID]
}

func (f *fakeDiscordVerifier) Verify(guildID, channelID string) discord.Verification {
	f.verifyCalls++
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
		SaaSCredentials:      repository.NewCredentialRepository(db.Pool),
		saasDiscordVerifier:  verifier,
	}
	cipher, err := security.NewAESGCM("01234567890123456789012345678901", 1)
	if err != nil {
		t.Fatal(err)
	}
	a.CredentialCipher = cipher
	return a, verifier
}

// withFakeNitradoServer points a's Nitrado client factory at a local test
// server serving GET /services with the given raw JSON body (any shape
// nitrado.DecodeServices accepts - see internal/nitrado/models.go), so
// Nitrado-dependent handlers can be exercised without a live account. If
// status is not http.StatusOK, AuthenticationCheck fails exactly like an
// invalid token would.
func withFakeNitradoServer(t *testing.T, a *App, status int, servicesJSON string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(servicesJSON))
		}
	}))
	t.Cleanup(srv.Close)
	a.saasNitradoClientFactory = func(token string) *nitrado.Client {
		return nitrado.NewClient(srv.URL, token, nil)
	}
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

	// Case E: bot_installed on the guild connection row itself must also be
	// updated by a successful verification, not just the installation status.
	conn, err := a.SaaSGuildConnections.GetScoped(context.Background(), fixture.OrgID, fixture.ConnectionID)
	if err != nil || conn == nil || !conn.BotInstalled {
		t.Fatalf("expected bot_installed=true on the guild connection after verification, got %+v err=%v", conn, err)
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

// --- section 7: eligible-guilds performance -------------------------------

// TestEligibleGuildsUsesZeroLiveDiscordCalls is the fix's core regression
// guard: at least 50 candidate guilds, none of which may trigger a live
// Verify call (the original bug - one sequential Discord REST round trip
// per candidate - was exactly what blew the website's 12s timeout).
// Eligibility must still come only from each candidate's own OAuth
// permission bitfield, and botInstalled only from the fake's cache map.
func TestEligibleGuildsUsesZeroLiveDiscordCalls(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	suffix := time.Now().UnixNano()
	owner := syncUser(t, a, fmt.Sprintf("api-perf-owner-%d", suffix), "Owner")
	org := mustCreateOrg(t, a, owner.DiscordUserID, "Perf Org", fmt.Sprintf("perf-org-%d", suffix))

	const candidateCount = 60
	type expectation struct {
		eligible     bool
		botInstalled bool
	}
	expected := make(map[string]expectation, candidateCount)
	guilds := make([]discordGuildCandidate, 0, candidateCount)
	for i := 0; i < candidateCount; i++ {
		guildID := fmt.Sprintf("perf-guild-%d-%d", suffix, i)
		var perms string
		var eligible bool
		switch i % 4 {
		case 0:
			perms, eligible = "8", true // ADMINISTRATOR
		case 1:
			perms, eligible = "32", true // MANAGE_GUILD
		case 2:
			perms, eligible = "0", false
		case 3:
			perms, eligible = "not-a-number", false // malformed
		}
		// Every other candidate is present in the bot's fake cache.
		cached := i%2 == 0
		if cached {
			verifier.guildFound[guildID] = true
		}
		expected[guildID] = expectation{eligible: eligible, botInstalled: cached}
		guilds = append(guilds, discordGuildCandidate{DiscordGuildID: guildID, GuildName: "Guild " + guildID, Permissions: perms})
	}

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", eligibleGuildsRequest{Guilds: guilds}), owner.DiscordUserID),
		map[string]string{"organizationID": strconv.FormatInt(org.ID, 10)})
	rr := httptest.NewRecorder()
	a.handleEligibleGuilds(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if verifier.verifyCalls != 0 {
		t.Fatalf("expected 0 live Verify calls for %d candidates, got %d", candidateCount, verifier.verifyCalls)
	}

	got := decodeBody[[]DiscordGuildSummary](t, rr)
	if len(got) != candidateCount {
		t.Fatalf("expected %d results, got %d", candidateCount, len(got))
	}
	for _, g := range got {
		want, ok := expected[g.DiscordGuildID]
		if !ok {
			t.Fatalf("unexpected guild in response: %q", g.DiscordGuildID)
		}
		if g.Eligible != want.eligible {
			t.Fatalf("guild %q: expected eligible=%v, got %v", g.DiscordGuildID, want.eligible, g.Eligible)
		}
		if g.BotInstalled != want.botInstalled {
			t.Fatalf("guild %q: expected botInstalled=%v, got %v", g.DiscordGuildID, want.botInstalled, g.BotInstalled)
		}
	}
}

// --- console DayZ backend (Nitrado connect/discover/select) ----------------

const psServiceJSON = `[{"id":"111111","status":"active","game":"DayZ (PS4)","details":{"name":"Champions PS","folder_short":"dayzps"}}]`

const mixedServicesJSON = `[
  {"id":"111111","status":"active","game":"DayZ (PS4)","details":{"name":"Champions PS","folder_short":"dayzps"}},
  {"id":"222222","status":"active","game":"DayZ (Xbox)","details":{"name":"Champions Xbox"}},
  {"id":"333333","status":"active","game":"DayZ","details":{"name":"PC Server"}},
  {"id":"444444","status":"active","game":"Minecraft","details":{"name":"MC Server"}}
]`

func connectNitrado(t *testing.T, a *App, orgID int64, ownerDiscordID string) NitradoConnectResponse {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nitradoConnectRequest{Token: "fake-nitrado-token"}), ownerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10)})
	rr := httptest.NewRecorder()
	a.handleNitradoConnect(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("connect nitrado: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	return decodeBody[NitradoConnectResponse](t, rr)
}

// TestNitradoConnectPersistsCredentialAndAdvancesSetup is section 4 + 11:
// a valid token is validated live, the encrypted envelope is persisted, the
// raw token never appears in the response, and setup progress advances
// (nitradoCompleted=true, currentStep=SERVER) for every installation under
// the organization.
func TestNitradoConnectPersistsCredentialAndAdvancesSetup(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)

	resp := connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	if !resp.Connected || resp.ServicesFound != 1 {
		t.Fatalf("expected connected=true servicesFound=1, got %+v", resp)
	}

	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(context.Background(), fixture.OrgID)
	if err != nil || envelope == nil {
		t.Fatalf("expected a persisted credential envelope: %v", err)
	}
	if len(envelope.Ciphertext) == 0 || len(envelope.Nonce) == 0 {
		t.Fatal("expected a non-empty encrypted envelope")
	}
	if strings.Contains(string(envelope.Ciphertext), "fake-nitrado-token") {
		t.Fatal("expected the token to be encrypted, not stored in plaintext")
	}

	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.NitradoCompleted {
		t.Fatalf("expected nitradoCompleted=true, got %+v err=%v", progress, err)
	}
	if progress.CurrentStep != "SERVER" {
		t.Fatalf("expected currentStep=SERVER after connect, got %q", progress.CurrentStep)
	}
}

// TestNitradoConnectRejectsInvalidToken is section 4: an invalid token must
// never be persisted.
func TestNitradoConnectRejectsInvalidToken(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusUnauthorized, "")

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nitradoConnectRequest{Token: "bad-token"}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10)})
	rr := httptest.NewRecorder()
	a.handleNitradoConnect(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a rejected token, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeNitradoUnavailable)

	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(context.Background(), fixture.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if envelope != nil {
		t.Fatal("expected no credential to be persisted for a rejected token")
	}
}

// TestNitradoServicesListsOnlySupportedPlatforms is section 5/14: a mixed
// PlayStation + Xbox + PC + unrelated-game list is filtered down to just
// the two supported console services.
func TestNitradoServicesListsOnlySupportedPlatforms(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10)})
	rr := httptest.NewRecorder()
	a.handleNitradoServices(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	services := decodeBody[[]NitradoServiceSummary](t, rr)
	if len(services) != 2 {
		t.Fatalf("expected exactly 2 supported services, got %d: %+v", len(services), services)
	}
	platforms := map[string]bool{}
	for _, s := range services {
		platforms[s.Platform] = true
		if s.Game != "DayZ" {
			t.Fatalf("expected game=DayZ, got %q", s.Game)
		}
	}
	if !platforms["PLAYSTATION"] || !platforms["XBOX"] {
		t.Fatalf("expected both PLAYSTATION and XBOX represented, got %+v", services)
	}
}

// TestSelectDayZServerPersistsPlatformAndAdvancesSetup is sections 7/9/11/14:
// selecting a valid PlayStation service persists the exact platform on the
// game_servers row, associates it with the installation, and advances
// setup progress (serverSelected=true, currentStep=CHANNELS).
func TestSelectDayZServerPersistsPlatformAndAdvancesSetup(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", selectDayZServerRequest{ServiceID: 111111}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleSelectDayZServer(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[SelectDayZServerResponse](t, rr)
	if resp.Server.Platform != "PLAYSTATION" || resp.Server.ServiceID != 111111 {
		t.Fatalf("expected PLAYSTATION/111111, got %+v", resp.Server)
	}
	if resp.ReusedInstallation || resp.InstallationID != fixture.InstallationID {
		t.Fatalf("expected a fresh, non-reused association with installation %d, got %+v", fixture.InstallationID, resp)
	}

	server, err := a.SaaSServers.GetScoped(context.Background(), fixture.OrgID, resp.Server.ID)
	if err != nil || server == nil || server.Platform != "PLAYSTATION" || server.ProviderServiceID != "111111" {
		t.Fatalf("expected a persisted PLAYSTATION game_servers row, got %+v err=%v", server, err)
	}

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.GameServerID == nil || *inst.GameServerID != resp.Server.ID {
		t.Fatalf("expected the installation's game_server_id to be set to %d, got %+v err=%v", resp.Server.ID, inst, err)
	}

	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ServerSelected || progress.CurrentStep != "CHANNELS" {
		t.Fatalf("expected serverSelected=true currentStep=CHANNELS, got %+v err=%v", progress, err)
	}
}

// TestSelectDayZServerRejectsPCAndCrossTenant covers sections 7/13/14: a PC
// DayZ service is rejected, and organization B can never select or even see
// organization A's connected Nitrado services/servers.
func TestSelectDayZServerRejectsPCAndCrossTenant(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)

	// PC (service 333333) must be rejected even though it's a real service
	// on the connected account.
	pcReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", selectDayZServerRequest{ServiceID: 333333}), fixtureA.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureA.OrgID, 10), "installationID": strconv.FormatInt(fixtureA.InstallationID, 10)})
	pcRR := httptest.NewRecorder()
	a.handleSelectDayZServer(pcRR, pcReq)
	if pcRR.Code != http.StatusBadRequest {
		t.Fatalf("expected DayZ PC to be rejected, got %d: %s", pcRR.Code, pcRR.Body.String())
	}
	assertErrorCode(t, pcRR, codeInvalidRequest)

	// Organization B has its own installation but never connected Nitrado -
	// listing services must fail cleanly (not see A's credential), and B's
	// installation must never resolve A's guild connection.
	fixtureB := buildInstallationFixture(t, a, verifier)
	servicesReq := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), fixtureB.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureB.OrgID, 10)})
	servicesRR := httptest.NewRecorder()
	a.handleNitradoServices(servicesRR, servicesReq)
	if servicesRR.Code != http.StatusNotFound {
		t.Fatalf("expected organization B (no Nitrado connection) to get 404, got %d: %s", servicesRR.Code, servicesRR.Body.String())
	}

	// B guessing A's installation ID within B's own organization context
	// must not leak A's data (tenant isolation - section 13).
	crossReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", selectDayZServerRequest{ServiceID: 111111}), fixtureB.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureB.OrgID, 10), "installationID": strconv.FormatInt(fixtureA.InstallationID, 10)})
	crossRR := httptest.NewRecorder()
	a.handleSelectDayZServer(crossRR, crossReq)
	if crossRR.Code != http.StatusNotFound {
		t.Fatalf("expected organization B selecting into organization A's installation to 404, got %d: %s", crossRR.Code, crossRR.Body.String())
	}
}

// --- duplicate installation reuse (live SQLSTATE 23505 fix) -----------------

// createSecondInstallation creates another installation under the same
// Discord guild connection as fixture - exactly how the live duplicate
// installations that caused this bug arose (a customer restarting the
// wizard for a guild that already has an installation).
func createSecondInstallation(t *testing.T, a *App, fixture installationFixture) int64 {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", createInstallationRequest{DiscordGuildConnectionID: fixture.ConnectionID}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10)})
	rr := httptest.NewRecorder()
	a.handleCreateInstallation(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create second installation: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	return decodeBody[InstallationSummary](t, rr).ID
}

func selectDayZServer(t *testing.T, a *App, orgID, installationID, serviceID int64, ownerDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", selectDayZServerRequest{ServiceID: serviceID}), ownerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleSelectDayZServer(rr, req)
	return rr
}

// TestSelectDayZServerReusesExistingInstallation is the exact live scenario
// (section 11): installation #4-equivalent already has the server selected;
// a newer, still-empty installation under the SAME guild connection selects
// the SAME server. Must reuse #4, not attempt to write a second
// (discord_guild_connection_id, game_server_id) row - the previously live
// SQLSTATE 23505 / HTTP 500.
func TestSelectDayZServerReusesExistingInstallation(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	firstRR := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)
	if firstRR.Code != http.StatusOK {
		t.Fatalf("first select: expected 200, got %d: %s", firstRR.Code, firstRR.Body.String())
	}
	first := decodeBody[SelectDayZServerResponse](t, firstRR)
	if first.ReusedInstallation || first.InstallationID != fixture.InstallationID {
		t.Fatalf("expected the first selection to be fresh on %d, got %+v", fixture.InstallationID, first)
	}

	newInstallationID := createSecondInstallation(t, a, fixture)

	secondRR := selectDayZServer(t, a, fixture.OrgID, newInstallationID, 111111, fixture.OwnerDiscordID)
	if secondRR.Code != http.StatusOK {
		t.Fatalf("expected 200 (idempotent reuse, never a 500), got %d: %s", secondRR.Code, secondRR.Body.String())
	}
	second := decodeBody[SelectDayZServerResponse](t, secondRR)
	if !second.ReusedInstallation {
		t.Fatal("expected reusedInstallation=true")
	}
	if second.InstallationID != fixture.InstallationID {
		t.Fatalf("expected the resolved installation to be the original %d, got %d", fixture.InstallationID, second.InstallationID)
	}
	if second.Server.ID != first.Server.ID {
		t.Fatalf("expected the same game_servers row %d, got %d (no duplicate)", first.Server.ID, second.Server.ID)
	}

	// The original installation - not the redundant new one - must be the
	// one whose setup progress advanced.
	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ServerSelected || progress.CurrentStep != "CHANNELS" {
		t.Fatalf("expected the original installation to show serverSelected=true currentStep=CHANNELS, got %+v err=%v", progress, err)
	}

	// The redundant, still-empty installation must have been safely deleted
	// (section 7) - it had no progress beyond Discord and no settings.
	gone, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, newInstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if gone != nil {
		t.Fatalf("expected the redundant empty installation %d to be deleted, still found: %+v", newInstallationID, gone)
	}

	var count int
	if err := a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM installations WHERE discord_guild_connection_id=$1 AND game_server_id=$2`, fixture.ConnectionID, first.Server.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one installation to own this (guild connection, server) pair, got %d", count)
	}
}

// TestSelectDayZServerSameInstallationIsIdempotent is section 12: selecting
// the same server twice on the same installation is a no-op success, never
// a conflict.
func TestSelectDayZServerSameInstallationIsIdempotent(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	first := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)
	if first.Code != http.StatusOK {
		t.Fatalf("first select: expected 200, got %d: %s", first.Code, first.Body.String())
	}
	second := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)
	if second.Code != http.StatusOK {
		t.Fatalf("re-select: expected 200, got %d: %s", second.Code, second.Body.String())
	}
	resp := decodeBody[SelectDayZServerResponse](t, second)
	if resp.ReusedInstallation {
		t.Fatal("expected reusedInstallation=false when re-selecting on the SAME installation that already owns it")
	}
	if resp.InstallationID != fixture.InstallationID {
		t.Fatalf("expected installationId=%d, got %d", fixture.InstallationID, resp.InstallationID)
	}

	// Still installed and functioning - not deleted, since it's not
	// "redundant and empty," it's the one actually holding the server.
	still, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || still == nil || still.GameServerID == nil {
		t.Fatalf("expected the installation to still exist with its game server set, got %+v err=%v", still, err)
	}
}

// TestSelectDayZServerDifferentServerSameGuildAllowed is section 13: a
// second installation under the same guild connection selecting a
// DIFFERENT DayZ server is a legitimate new pairing, never blocked -
// multi-server customers must keep working.
func TestSelectDayZServerDifferentServerSameGuildAllowed(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	first := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)
	if first.Code != http.StatusOK {
		t.Fatalf("first select (PS): expected 200, got %d: %s", first.Code, first.Body.String())
	}

	secondInstallationID := createSecondInstallation(t, a, fixture)
	second := selectDayZServer(t, a, fixture.OrgID, secondInstallationID, 222222, fixture.OwnerDiscordID)
	if second.Code != http.StatusOK {
		t.Fatalf("second select (Xbox, different server): expected 200, got %d: %s", second.Code, second.Body.String())
	}
	resp := decodeBody[SelectDayZServerResponse](t, second)
	if resp.ReusedInstallation {
		t.Fatal("expected a genuinely new (guild connection, server) pair to never be treated as reuse")
	}
	if resp.InstallationID != secondInstallationID {
		t.Fatalf("expected installationId=%d, got %d", secondInstallationID, resp.InstallationID)
	}
	if resp.Server.Platform != "XBOX" {
		t.Fatalf("expected XBOX, got %q", resp.Server.Platform)
	}

	// The first installation must be untouched by the second selection.
	firstInst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || firstInst == nil || firstInst.GameServerID == nil {
		t.Fatalf("expected the first installation's server to remain set, got %+v err=%v", firstInst, err)
	}
	firstServer, err := a.SaaSServers.GetScoped(context.Background(), fixture.OrgID, *firstInst.GameServerID)
	if err != nil || firstServer == nil || firstServer.Platform != "PLAYSTATION" {
		t.Fatalf("expected the first installation to still point at the PLAYSTATION server, got %+v err=%v", firstServer, err)
	}
}

// TestValidateDayZServerReportsReachability is section 12: a safe live
// check of the selected server's continued reachability/support.
func TestValidateDayZServerReportsReachability(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	selectReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", selectDayZServerRequest{ServiceID: 111111}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	selectRR := httptest.NewRecorder()
	a.handleSelectDayZServer(selectRR, selectReq)
	if selectRR.Code != http.StatusOK {
		t.Fatalf("select: expected 200, got %d: %s", selectRR.Code, selectRR.Body.String())
	}

	validateReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	validateRR := httptest.NewRecorder()
	a.handleValidateDayZServer(validateRR, validateReq)
	if validateRR.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", validateRR.Code, validateRR.Body.String())
	}
	result := decodeBody[DayZServerValidation](t, validateRR)
	if !result.Reachable || !result.Supported || result.Platform != "PLAYSTATION" {
		t.Fatalf("expected reachable/supported PLAYSTATION, got %+v", result)
	}
}
