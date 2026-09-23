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

	"github.com/bwmarrin/discordgo"
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

	// channels backs ListGuildChannels/ListAllGuildChannels/Create* - an
	// in-memory per-guild channel store standing in for a real Discord
	// guild's channel list (Step 5 one-click auto-setup + customization).
	channels     map[string][]fakeGuildChannel
	channelIDSeq int
	// permissions overrides GuildPermissions per guild; a guild absent from
	// this map defaults to full permissions (discordgo.PermissionAll) so
	// most tests need no extra setup - only the explicit "missing
	// permission" test cases opt out.
	permissions map[string]int64
	// memberRoles backs MemberRoles: guildID -> discordUserID -> role IDs. A user absent from
	// this map resolves to an empty role list (never an error), matching "this person is in the
	// guild but holds no mapped role" rather than "lookup failed".
	memberRoles map[string]map[string][]string
}

type fakeGuildChannel struct {
	ID       string
	Name     string
	Type     discordgo.ChannelType
	ParentID string
	Position int
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

// ListGuildChannels mirrors discord.Client.ListGuildChannels's customer-
// facing filter: only text/announcement channels, always "viewable" in this
// fake (permission filtering itself is covered at the discord package unit
// level, not here).
func (f *fakeDiscordVerifier) ListGuildChannels(guildID string) ([]discord.GuildChannelInfo, error) {
	out := make([]discord.GuildChannelInfo, 0, len(f.channels[guildID]))
	for _, ch := range f.channels[guildID] {
		if ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews {
			continue
		}
		out = append(out, discord.GuildChannelInfo{ID: ch.ID, Name: ch.Name, Type: ch.Type, Position: ch.Position, CanSend: true})
	}
	return out, nil
}

// ListAllGuildChannels mirrors discord.Client.ListAllGuildChannels: every
// channel, including categories, unfiltered.
func (f *fakeDiscordVerifier) ListAllGuildChannels(guildID string) ([]discord.RawGuildChannel, error) {
	out := make([]discord.RawGuildChannel, 0, len(f.channels[guildID]))
	for _, ch := range f.channels[guildID] {
		out = append(out, discord.RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID})
	}
	return out, nil
}

// GuildPermissions returns the configured override for guildID, or full
// permissions by default (see the permissions field doc comment).
func (f *fakeDiscordVerifier) GuildPermissions(guildID string) (int64, error) {
	if perms, ok := f.permissions[guildID]; ok {
		return perms, nil
	}
	return discordgo.PermissionAll, nil
}

// MemberRoles returns the configured role list for (guildID, userID), or an empty (non-nil) list
// if unconfigured.
func (f *fakeDiscordVerifier) MemberRoles(guildID, userID string) ([]string, error) {
	return f.memberRoles[guildID][userID], nil
}

// CreateGuildCategory and CreateGuildTextChannel append to the fake's
// per-guild channel store and hand back a freshly minted, unique ID -
// mirroring a real Discord channel create.
func (f *fakeDiscordVerifier) CreateGuildCategory(guildID, name string) (*discord.RawGuildChannel, error) {
	return f.createFakeChannel(guildID, name, discordgo.ChannelTypeGuildCategory, "")
}

func (f *fakeDiscordVerifier) CreateGuildTextChannel(guildID, name, parentCategoryID string) (*discord.RawGuildChannel, error) {
	return f.createFakeChannel(guildID, name, discordgo.ChannelTypeGuildText, parentCategoryID)
}

func (f *fakeDiscordVerifier) createFakeChannel(guildID, name string, ctype discordgo.ChannelType, parentID string) (*discord.RawGuildChannel, error) {
	if f.channels == nil {
		f.channels = map[string][]fakeGuildChannel{}
	}
	f.channelIDSeq++
	id := fmt.Sprintf("fake-channel-%d", f.channelIDSeq)
	ch := fakeGuildChannel{ID: id, Name: name, Type: ctype, ParentID: parentID, Position: len(f.channels[guildID])}
	f.channels[guildID] = append(f.channels[guildID], ch)
	return &discord.RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID}, nil
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
		SaaSChannelRoutes:    repository.NewChannelRouteRepository(db.Pool),
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
	completeDiscordStep(t, a, fixture)
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
	completeDiscordStep(t, a, fixture)
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
	completeDiscordStep(t, a, fixture)
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

// --- Step 5: Discord channel configuration ----------------------------------

// seedGuildChannels registers channels directly in the fake's per-guild
// store, standing in for channels a customer's Discord server already has -
// used to exercise listing/validation without going through Create*.
func seedGuildChannels(verifier *fakeDiscordVerifier, guildID string, channels ...fakeGuildChannel) {
	if verifier.channels == nil {
		verifier.channels = map[string][]fakeGuildChannel{}
	}
	verifier.channels[guildID] = append(verifier.channels[guildID], channels...)
}

func listDiscordChannels(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleListDiscordChannels(rr, req)
	return rr
}

func getChannelSettings(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleGetChannelSettings(rr, req)
	return rr
}

func saveChannelSettings(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string, settings InstallationChannelSettings) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPut, "/x", settings), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleSaveChannelSettings(rr, req)
	return rr
}

// TestListDiscordChannelsReturnsOnlyTextAndAnnouncement is cases A/B: only
// the installation's own guild's text/announcement channels come back -
// voice/category channels are never included (enforced by
// discord.Client.ListGuildChannels itself; the fake mirrors that same
// filter so this test also proves the handler wires it through correctly).
func TestListDiscordChannelsReturnsOnlyTextAndAnnouncement(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "c-text", Name: "general", Type: discordgo.ChannelTypeGuildText, Position: 1},
		fakeGuildChannel{ID: "c-news", Name: "announcements", Type: discordgo.ChannelTypeGuildNews, Position: 2},
		fakeGuildChannel{ID: "c-voice", Name: "Lobby", Type: discordgo.ChannelTypeGuildVoice, Position: 0},
		fakeGuildChannel{ID: "c-cat", Name: "Category", Type: discordgo.ChannelTypeGuildCategory, Position: 0},
	)

	rr := listDiscordChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	channels := decodeBody[[]DiscordChannelSummary](t, rr)
	if len(channels) != 2 {
		t.Fatalf("expected exactly the 2 text/announcement channels, got %+v", channels)
	}
	ids := map[string]bool{}
	for _, c := range channels {
		ids[c.ID] = true
	}
	if !ids["c-text"] || !ids["c-news"] {
		t.Fatalf("expected c-text and c-news, got %+v", channels)
	}
}

// TestSaveChannelSettingsPersistsAndPreservesOtherFields covers D/E/F/I/J:
// the required killfeed channel plus optional channels persist, the SAME
// channel can serve multiple purposes, unrelated settings fields survive
// untouched, and setup advances CHANNELS -> VALIDATION.
func TestSaveChannelSettingsPersistsAndPreservesOtherFields(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	completeDiscordStep(t, a, fixture)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "c-killfeed", Name: "champion-killfeed", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "c-admin", Name: "champion-admin", Type: discordgo.ChannelTypeGuildText},
	)

	// Move server selection along first so ChannelsCompleted is a valid
	// transition (validateSetupProgressOrder requires ServerSelected).
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	if rr := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("select dayz server: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Record a distinctive Timezone/DistanceUnit combination to prove they
	// survive the channel save untouched (section 12/I).
	custom := repository.InstallationSettings{
		InstallationID: fixture.InstallationID, Timezone: "Europe/Berlin", DistanceUnit: "FEET",
		OnlineDisplayEnabled: false, LeaderboardEnabled: false,
	}
	if err := a.SaaSInstallations.UpdateSettings(context.Background(), fixture.OrgID, fixture.InstallationID, custom); err != nil {
		t.Fatal(err)
	}

	// Same channel (c-killfeed) reused for both killfeed and admin log purposes (F).
	rr := saveChannelSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationChannelSettings{
		KillfeedChannelID: "c-killfeed",
		AdminLogChannelID: "c-killfeed",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("save channel settings: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	saved := decodeBody[InstallationChannelSettings](t, rr)
	if saved.KillfeedChannelID != "c-killfeed" || saved.AdminLogChannelID != "c-killfeed" {
		t.Fatalf("expected the same channel to serve both purposes, got %+v", saved)
	}
	if saved.LeaderboardChannelID != "" || saved.PlayerStatusChannelID != "" {
		t.Fatalf("expected the unset optional channels to remain empty, got %+v", saved)
	}

	// Refresh restoration (section 16): GET must return exactly what was saved.
	getRR := getChannelSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get channel settings: expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	got := decodeBody[InstallationChannelSettings](t, getRR)
	if got != saved {
		t.Fatalf("expected GET to restore exactly what was saved, got %+v want %+v", got, saved)
	}

	persisted, err := a.SaaSInstallations.GetSettings(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || persisted == nil {
		t.Fatalf("expected persisted settings, err=%v", err)
	}
	if persisted.Timezone != "Europe/Berlin" || persisted.DistanceUnit != "FEET" || persisted.OnlineDisplayEnabled || persisted.LeaderboardEnabled {
		t.Fatalf("expected unrelated settings preserved untouched, got %+v", persisted)
	}
	if persisted.ChannelSetupSource != "MANUAL" {
		t.Fatalf("expected channel_setup_source=MANUAL after an explicit save, got %q", persisted.ChannelSetupSource)
	}

	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ChannelsCompleted || progress.CurrentStep != "VALIDATION" {
		t.Fatalf("expected channelsCompleted=true currentStep=VALIDATION, got %+v err=%v", progress, err)
	}

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationConfiguring {
		t.Fatalf("expected installation status CONFIGURING, got %+v err=%v", inst, err)
	}
}

// TestSaveChannelSettingsRejectsMissingKillfeedChannel covers section 10:
// killfeedChannelId is the only hard requirement.
func TestSaveChannelSettingsRejectsMissingKillfeedChannel(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "c-1", Name: "general", Type: discordgo.ChannelTypeGuildText})

	rr := saveChannelSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationChannelSettings{LeaderboardChannelID: "c-1"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a killfeed channel, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)
}

// TestSaveChannelSettingsRejectsForeignGuildChannel is case C: a channel ID
// that exists but belongs to a DIFFERENT Discord guild must never be
// accepted, even though it's a syntactically valid Discord snowflake.
func TestSaveChannelSettingsRejectsForeignGuildChannel(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: "foreign-channel", Name: "general", Type: discordgo.ChannelTypeGuildText})

	rr := saveChannelSettings(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, InstallationChannelSettings{KillfeedChannelID: "foreign-channel"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a foreign-guild channel, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)
}

// TestChannelSettingsCrossTenantRejected covers cases G/H: organization B
// can never read or write organization A's channel settings, even by
// guessing A's installation ID within its own org-scoped request.
func TestChannelSettingsCrossTenantRejected(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: "c-1", Name: "champion-killfeed", Type: discordgo.ChannelTypeGuildText})

	// Seed A's settings so there's something real for B to fail to read.
	if rr := saveChannelSettings(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, InstallationChannelSettings{KillfeedChannelID: "c-1"}); rr.Code != http.StatusOK {
		t.Fatalf("seed A's settings: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// G: B reading A's installation (within B's own org scope) must 404, never leak A's data.
	readRR := getChannelSettings(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID)
	if readRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant read, got %d: %s", readRR.Code, readRR.Body.String())
	}

	// H: B writing to A's installation ID must 404, never modify A's settings.
	writeRR := saveChannelSettings(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID, InstallationChannelSettings{KillfeedChannelID: "c-1"})
	if writeRR.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant write, got %d: %s", writeRR.Code, writeRR.Body.String())
	}

	stillA, err := a.SaaSInstallations.GetSettings(context.Background(), fixtureA.OrgID, fixtureA.InstallationID)
	if err != nil || stillA == nil || stillA.KillfeedChannelID != "c-1" {
		t.Fatalf("expected A's settings untouched by B's attempts, got %+v err=%v", stillA, err)
	}
}

// --- Step 5: one-click channel auto-setup -----------------------------------

func autoSetupChannels(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string, force bool) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", autoSetupChannelsRequest{Force: force}), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleAutoSetupChannels(rr, req)
	return rr
}

// championAllRouteKeys is every stable route key in the blueprint, used by
// tests that need to assert completeness without hand-maintaining a second
// copy of the list.
var championAllRouteKeys = []string{
	"KILLFEED", "PVE_FEED", "LINK_GAMERTAG", "STATS_LEADERBOARDS", "AUTO_LEADERBOARD",
	"HITFEED", "BOUNTY", "BOUNTY_TRACKING", "HEATMAPS", "ECONOMY", "CASINO", "SHOP",
	"CONNECTIONS", "BUILD_FEED", "ADMIN_ALERTS", "ADMIN_LOGS",
}

func listChannelRoutes(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleListChannelRoutes(rr, req)
	return rr
}

func saveChannelRoutes(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string, routes map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPut, "/x", saveChannelRoutesRequest{Routes: routes}), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleSaveChannelRoutes(rr, req)
	return rr
}

// TestAutoSetupChannelsCreatesCompleteBlueprint is case A/section 21 "all 16
// default route keys": a single call creates the Champion category and all
// sixteen default channels, every route populated, KILLFEED marked
// managed=true.
func TestAutoSetupChannelsCreatesCompleteBlueprint(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	advanceFixtureToServerSelected(t, a, fixture)

	rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[AutoSetupChannelsResponse](t, rr)
	if !resp.Configured {
		t.Fatalf("expected configured=true, got %+v", resp)
	}
	if resp.Category == nil || resp.Category.Name != championManagedCategoryName {
		t.Fatalf("expected the Champion default category, got %+v", resp.Category)
	}
	if len(resp.Routes) != len(championAllRouteKeys) {
		t.Fatalf("expected all %d default routes, got %d: %+v", len(championAllRouteKeys), len(resp.Routes), resp.Routes)
	}
	ids := map[string]bool{}
	for _, key := range championAllRouteKeys {
		info, ok := resp.Routes[key]
		if !ok || info.ChannelID == "" {
			t.Fatalf("expected route %s to be configured, got %+v", key, resp.Routes)
		}
		if !info.ManagedByChampion {
			t.Fatalf("expected route %s to be managed by Champion, got %+v", key, info)
		}
		ids[info.ChannelID] = true
	}
	if len(ids) != len(championAllRouteKeys) {
		t.Fatalf("expected %d distinct channels, got %d", len(championAllRouteKeys), len(ids))
	}
	for _, ch := range verifier.channels[fixture.DiscordGuildID] {
		if ch.Type == discordgo.ChannelTypeGuildText && ids[ch.ID] && ch.ParentID != resp.Category.ID {
			t.Fatalf("expected channel %q to sit under the Champion category, got parent %q", ch.ID, ch.ParentID)
		}
	}

	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ChannelsCompleted || progress.CurrentStep != "VALIDATION" || progress.ValidationCompleted {
		t.Fatalf("expected channelsCompleted=true currentStep=VALIDATION validationCompleted=false, got %+v err=%v", progress, err)
	}
	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationConfiguring {
		t.Fatalf("expected installation status CONFIGURING, got %+v err=%v", inst, err)
	}
}

// TestAutoSetupChannelsIsIdempotent is case B: calling auto-setup twice must
// reuse the same category and channels, never create "-1"/"-2" duplicates.
func TestAutoSetupChannelsIsIdempotent(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	first := decodeBody[AutoSetupChannelsResponse](t, autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false))
	second := decodeBody[AutoSetupChannelsResponse](t, autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false))

	if second.Category.ID != first.Category.ID {
		t.Fatalf("expected the same category to be reused, got %q then %q", first.Category.ID, second.Category.ID)
	}
	for _, key := range championAllRouteKeys {
		if second.Routes[key].ChannelID != first.Routes[key].ChannelID {
			t.Fatalf("expected route %s to reuse the same channel ID, got %q then %q", key, first.Routes[key].ChannelID, second.Routes[key].ChannelID)
		}
	}

	total := 0
	for _, ch := range verifier.channels[fixture.DiscordGuildID] {
		if ch.Type == discordgo.ChannelTypeGuildText {
			total++
		}
	}
	if total != len(championAllRouteKeys) {
		t.Fatalf("expected exactly %d text channels after two auto-setup calls, got %d", len(championAllRouteKeys), total)
	}
}

// TestAutoSetupChannelsReusesExistingChampionChannelsByName is case C: if
// the Champion category/channels already exist in the guild (e.g. created
// manually, or Champion never persisted the IDs), auto-setup recognizes and
// reuses them by name rather than creating duplicates (section 4 recovery
// path).
func TestAutoSetupChannelsReusesExistingChampionChannelsByName(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "existing-category", Name: "champion killfeed", Type: discordgo.ChannelTypeGuildCategory})
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "existing-killfeed", Name: "killfeed", Type: discordgo.ChannelTypeGuildText, ParentID: "existing-category"},
	)

	rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[AutoSetupChannelsResponse](t, rr)
	if resp.Category.ID != "existing-category" {
		t.Fatalf("expected the pre-existing category to be reused, got %+v", resp.Category)
	}
	if resp.Routes["KILLFEED"].ChannelID != "existing-killfeed" {
		t.Fatalf("expected the pre-existing killfeed channel to be reused, got %q", resp.Routes["KILLFEED"].ChannelID)
	}

	killfeedCount := 0
	for _, ch := range verifier.channels[fixture.DiscordGuildID] {
		if ch.Name == "killfeed" {
			killfeedCount++
		}
	}
	if killfeedCount != 1 {
		t.Fatalf("expected no duplicate killfeed channel, found %d", killfeedCount)
	}
}

// TestAutoSetupChannelsMissingManageChannelsReturnsSafeFailure is case D: a
// bot lacking Manage Channels gets a safe, structured result - never a raw
// Discord error, never a 5xx.
func TestAutoSetupChannelsMissingManageChannelsReturnsSafeFailure(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	if verifier.permissions == nil {
		verifier.permissions = map[string]int64{}
	}
	verifier.permissions[fixture.DiscordGuildID] = discordgo.PermissionViewChannel // no Manage Channels

	rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (a safe result, not an error), got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[AutoSetupChannelsResponse](t, rr)
	if resp.Configured || resp.Reason != "MISSING_MANAGE_CHANNELS" {
		t.Fatalf("expected configured=false reason=MISSING_MANAGE_CHANNELS, got %+v", resp)
	}
	if len(verifier.channels[fixture.DiscordGuildID]) != 0 {
		t.Fatal("expected no channels to have been created")
	}
}

// TestAutoSetupChannelsDoesNotOverwriteManualConfiguration is case E: an
// existing MANUAL configuration (via the legacy .../channels endpoint)
// blocks auto-setup unless force=true, and is left completely untouched
// when it does.
func TestAutoSetupChannelsDoesNotOverwriteManualConfiguration(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "custom-killfeed", Name: "my-custom-feed", Type: discordgo.ChannelTypeGuildText})

	if rr := saveChannelSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationChannelSettings{KillfeedChannelID: "custom-killfeed"}); rr.Code != http.StatusOK {
		t.Fatalf("manual save: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	blockedRR := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if blockedRR.Code != http.StatusOK {
		t.Fatalf("expected 200 (a safe result, not an error), got %d: %s", blockedRR.Code, blockedRR.Body.String())
	}
	blocked := decodeBody[AutoSetupChannelsResponse](t, blockedRR)
	if blocked.Configured || blocked.Reason != "CUSTOM_CONFIGURATION_EXISTS" {
		t.Fatalf("expected configured=false reason=CUSTOM_CONFIGURATION_EXISTS, got %+v", blocked)
	}

	stillCustom, err := a.SaaSInstallations.GetSettings(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || stillCustom == nil || stillCustom.KillfeedChannelID != "custom-killfeed" {
		t.Fatalf("expected the manual configuration to remain untouched, got %+v err=%v", stillCustom, err)
	}

	// force=true is an explicit, deliberate override (section 9 "restore defaults").
	forcedRR := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, true)
	if forcedRR.Code != http.StatusOK {
		t.Fatalf("expected 200 with force=true, got %d: %s", forcedRR.Code, forcedRR.Body.String())
	}
	forced := decodeBody[AutoSetupChannelsResponse](t, forcedRR)
	if !forced.Configured || forced.Routes["KILLFEED"].ChannelID == "custom-killfeed" {
		t.Fatalf("expected force=true to actually run auto-setup, got %+v", forced)
	}
}

// TestAutoSetupChannelsProtectsCustomerOwnedRoute covers section 14/15 via
// the NEW routes endpoint: a route the customer explicitly pointed at an
// existing channel via PUT .../channel-routes also blocks auto-setup unless
// force=true - not just the legacy four-field endpoint.
func TestAutoSetupChannelsProtectsCustomerOwnedRoute(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "my-killfeed", Name: "my-killfeed", Type: discordgo.ChannelTypeGuildText})

	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "my-killfeed"}); rr.Code != http.StatusOK {
		t.Fatalf("save channel routes: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr := autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (a safe result, not an error), got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[AutoSetupChannelsResponse](t, rr)
	if resp.Configured || resp.Reason != "CUSTOM_CONFIGURATION_EXISTS" {
		t.Fatalf("expected configured=false reason=CUSTOM_CONFIGURATION_EXISTS, got %+v", resp)
	}

	routes, err := a.SaaSChannelRoutes.ListForInstallation(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || len(routes) != 1 || routes[0].ChannelID != "my-killfeed" {
		t.Fatalf("expected the customer's route to remain untouched, got %+v err=%v", routes, err)
	}
}

// TestAutoSetupChannelsCrossTenantRejected is case I: organization B can
// never trigger auto-setup against organization A's installation.
func TestAutoSetupChannelsCrossTenantRejected(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)

	rr := autoSetupChannels(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID, false)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant auto-setup, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(verifier.channels[fixtureA.DiscordGuildID]) != 0 {
		t.Fatal("expected no channels to have been created in A's guild by B's attempt")
	}
}

// TestAutoSetupChannelsMirrorsLegacySettings covers section 21 "legacy
// four-field settings remain compatible": after auto-setup, GET
// .../channels (the OLD endpoint) still reflects the resolved KILLFEED/
// STATS_LEADERBOARDS/ADMIN_LOGS routes, not stale/empty legacy columns.
func TestAutoSetupChannelsMirrorsLegacySettings(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	resp := decodeBody[AutoSetupChannelsResponse](t, autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false))

	legacyRR := getChannelSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if legacyRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", legacyRR.Code, legacyRR.Body.String())
	}
	legacy := decodeBody[InstallationChannelSettings](t, legacyRR)
	if legacy.KillfeedChannelID != resp.Routes["KILLFEED"].ChannelID {
		t.Fatalf("expected legacy killfeedChannelId to mirror the KILLFEED route, got %q want %q", legacy.KillfeedChannelID, resp.Routes["KILLFEED"].ChannelID)
	}
	if legacy.LeaderboardChannelID != resp.Routes["STATS_LEADERBOARDS"].ChannelID {
		t.Fatalf("expected legacy leaderboardChannelId to mirror STATS_LEADERBOARDS, got %q want %q", legacy.LeaderboardChannelID, resp.Routes["STATS_LEADERBOARDS"].ChannelID)
	}
	if legacy.AdminLogChannelID != resp.Routes["ADMIN_LOGS"].ChannelID {
		t.Fatalf("expected legacy adminLogChannelId to mirror ADMIN_LOGS, got %q want %q", legacy.AdminLogChannelID, resp.Routes["ADMIN_LOGS"].ChannelID)
	}
}

// TestChannelRoutesPersistenceSurvivesRefresh covers section 21 "route
// persistence survives refresh" and "same channel can serve multiple
// routes": saving two routes to the SAME channel, then GET-ing, must return
// exactly what was saved.
func TestChannelRoutesPersistenceSurvivesRefresh(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID,
		fakeGuildChannel{ID: "c-shared", Name: "general", Type: discordgo.ChannelTypeGuildText},
		fakeGuildChannel{ID: "c-admin", Name: "admin", Type: discordgo.ChannelTypeGuildText},
	)

	saveRR := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{
		"KILLFEED":   "c-shared",
		"BOUNTY":     "c-shared", // same channel, two routes (section 9)
		"ADMIN_LOGS": "c-admin",
		"PVE_FEED":   "", // explicitly disabled/omitted - never required
	})
	if saveRR.Code != http.StatusOK {
		t.Fatalf("save channel routes: expected 200, got %d: %s", saveRR.Code, saveRR.Body.String())
	}
	saved := decodeBody[ChannelRoutesResponse](t, saveRR)
	if saved.Routes["KILLFEED"].ChannelID != "c-shared" || saved.Routes["BOUNTY"].ChannelID != "c-shared" {
		t.Fatalf("expected KILLFEED and BOUNTY to share c-shared, got %+v", saved.Routes)
	}
	if _, ok := saved.Routes["PVE_FEED"]; ok {
		t.Fatalf("expected PVE_FEED to be absent (empty string means disabled), got %+v", saved.Routes)
	}
	for key, info := range saved.Routes {
		if info.ManagedByChampion {
			t.Fatalf("expected every manually-saved route to be managedByChampion=false, route %s was true", key)
		}
	}

	getRR := listChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	got := decodeBody[ChannelRoutesResponse](t, getRR)
	if len(got.Routes) != 3 || got.Routes["KILLFEED"].ChannelID != "c-shared" || got.Routes["BOUNTY"].ChannelID != "c-shared" || got.Routes["ADMIN_LOGS"].ChannelID != "c-admin" {
		t.Fatalf("expected GET to restore exactly what was saved, got %+v", got.Routes)
	}
}

// TestChannelRoutesRequireKillfeed covers section 10: KILLFEED remains
// required even in the new routes model - disabling it is rejected.
func TestChannelRoutesRequireKillfeed(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "c-1", Name: "general", Type: discordgo.ChannelTypeGuildText})

	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "c-1"}); rr.Code != http.StatusOK {
		t.Fatalf("initial save: expected 200, got %d", rr.Code)
	}

	rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": ""})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 clearing the required killfeed route, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)

	still, err := a.SaaSChannelRoutes.ListForInstallation(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || len(still) != 1 || still[0].ChannelID != "c-1" {
		t.Fatalf("expected KILLFEED to remain configured, got %+v err=%v", still, err)
	}
}

// TestChannelRoutesRejectsForeignGuildChannel covers section 12/21 "cross-
// guild channel rejected".
func TestChannelRoutesRejectsForeignGuildChannel(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: "foreign-channel", Name: "general", Type: discordgo.ChannelTypeGuildText})

	rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "foreign-channel"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a foreign-guild channel, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)
}

// TestChannelRoutesCrossTenantRejected covers section 21 "cross-tenant
// access rejected" for the new routes endpoints.
func TestChannelRoutesCrossTenantRejected(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: "c-1", Name: "killfeed", Type: discordgo.ChannelTypeGuildText})

	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "c-1"}); rr.Code != http.StatusOK {
		t.Fatalf("seed A's routes: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if rr := listChannelRoutes(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant read, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID, map[string]string{"KILLFEED": "c-1"}); rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant write, got %d: %s", rr.Code, rr.Body.String())
	}

	stillA, err := a.SaaSChannelRoutes.ListForInstallation(context.Background(), fixtureA.OrgID, fixtureA.InstallationID)
	if err != nil || len(stillA) != 1 || stillA[0].ChannelID != "c-1" {
		t.Fatalf("expected A's routes untouched by B's attempts, got %+v err=%v", stillA, err)
	}
}

// TestChannelRoutesManagedOwnershipDistinguishesSource covers section 21
// "Champion-managed ownership preserved" / "customer-owned channels never
// marked managed": an auto-created route is managed=true; a manually saved
// route pointing at the SAME channel ID is managed=false.
func TestChannelRoutesManagedOwnershipDistinguishesSource(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	auto := decodeBody[AutoSetupChannelsResponse](t, autoSetupChannels(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, false))
	killfeedChannelID := auto.Routes["KILLFEED"].ChannelID

	routes, err := a.SaaSChannelRoutes.ListForInstallation(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if !r.ManagedByChampion {
			t.Fatalf("expected every auto-setup route to be managed=true, route %s was false", r.RouteKey)
		}
	}

	// A manual save pointing PVE_FEED at the SAME Champion-created channel
	// must still be recorded as customer-owned for THAT route (section 15) -
	// managed ownership is never implied just because the channel ID matches
	// one Champion happened to create for a different route.
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"PVE_FEED": killfeedChannelID}); rr.Code != http.StatusOK {
		t.Fatalf("manual save: expected 200, got %d", rr.Code)
	}
	updated, err := a.SaaSChannelRoutes.ListForInstallation(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]repository.ChannelRoute{}
	for _, r := range updated {
		byKey[r.RouteKey] = r
	}
	if byKey["PVE_FEED"].ManagedByChampion {
		t.Fatal("expected the manually-set PVE_FEED route to be managed=false")
	}
	if !byKey["KILLFEED"].ManagedByChampion {
		t.Fatal("expected KILLFEED to remain managed=true - untouched by the PVE_FEED save")
	}
}

// TestCreateDiscordChannelRejectsForeignGuildCategory is case H for the
// optional custom-channel-creation endpoint (section 10/19): a categoryId
// belonging to a different guild must be rejected, never used as a parent.
func TestCreateDiscordChannelRejectsForeignGuildCategory(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA := buildInstallationFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)
	seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: "foreign-category", Name: "Other Guild Category", Type: discordgo.ChannelTypeGuildCategory})

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", createDiscordChannelRequest{Name: "pvp-feed", CategoryID: "foreign-category"}), fixtureA.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixtureA.OrgID, 10), "installationID": strconv.FormatInt(fixtureA.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleCreateDiscordChannel(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a foreign-guild categoryId, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)
	if len(verifier.channels[fixtureA.DiscordGuildID]) != 0 {
		t.Fatal("expected no channel to have been created in A's guild")
	}
}

// TestCreateDiscordChannelNormalizesName covers section 12: a human-friendly
// name is safely normalized before being sent to Discord.
func TestCreateDiscordChannelNormalizesName(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", createDiscordChannelRequest{Name: "  Kill Feed!! "}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleCreateDiscordChannel(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	created := decodeBody[DiscordChannelSummary](t, rr)
	if created.Name != "kill-feed" {
		t.Fatalf("expected the name to normalize to %q, got %q", "kill-feed", created.Name)
	}
}

// --- setup finalization + customer hub --------------------------------------

func boolPtr(b bool) *bool { return &b }

// completeDiscordStep marks the wizard's Discord step done exactly as the
// website does (PATCH .../setup, discordCompleted=true). Later steps'
// progress flags are only persisted when the earlier ones are already true
// (validateSetupProgressOrder), so any test that asserts nitradoCompleted /
// serverSelected / channelsCompleted must do this first - buildInstallation
// Fixture deliberately stops at a fresh, pre-Discord-step installation.
func completeDiscordStep(t *testing.T, a *App, fixture installationFixture) {
	t.Helper()
	// The Discord step also runs verify-installation, which is what moves a
	// real installation NOT_STARTED -> DISCORD_CONNECTED.
	verifyReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	verifyRR := httptest.NewRecorder()
	a.handleVerifyInstallation(verifyRR, verifyReq)
	if verifyRR.Code != http.StatusOK {
		t.Fatalf("verify installation: expected 200, got %d: %s", verifyRR.Code, verifyRR.Body.String())
	}
	req := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", setupProgressPatchRequest{DiscordCompleted: boolPtr(true), CurrentStep: strPtr("NITRADO")}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	rr := httptest.NewRecorder()
	a.handleUpdateSetupProgress(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("complete discord step: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// advanceFixtureToServerSelected walks a fresh fixture through Discord,
// Nitrado connect, and DayZ server selection (no channels yet).
func advanceFixtureToServerSelected(t *testing.T, a *App, fixture installationFixture) {
	t.Helper()
	completeDiscordStep(t, a, fixture)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	if rr := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("select dayz server: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func finalizeSetup(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleFinalizeSetup(rr, req)
	return rr
}

func getHub(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleGetInstallationHub(rr, req)
	return rr
}

func getInstallationSettings(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleGetInstallationSettings(rr, req)
	return rr
}

func saveInstallationSettings(t *testing.T, a *App, orgID, installationID int64, actingDiscordID string, settings InstallationGeneralSettings) *httptest.ResponseRecorder {
	t.Helper()
	req := withPathValues(withActingUser(saasRequest(http.MethodPut, "/x", settings), actingDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(orgID, 10), "installationID": strconv.FormatInt(installationID, 10)})
	rr := httptest.NewRecorder()
	a.handleSaveInstallationSettings(rr, req)
	return rr
}

// buildFinalizableFixture drives the full onboarding chain (Discord verify +
// discordCompleted, Nitrado connect, DayZ server select, KILLFEED route with
// a fully-passing live permission check) so finalize tests can call
// finalizeSetup and expect success without repeating this setup in every
// test. killfeedChannelID is returned so tests can flip its permission
// result to exercise the "cannot finalize before permissions pass" case.
func buildFinalizableFixture(t *testing.T, a *App, verifier *fakeDiscordVerifier) (fixture installationFixture, killfeedChannelID string) {
	t.Helper()
	fixture = buildInstallationFixture(t, a, verifier)

	verifyReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	verifyRR := httptest.NewRecorder()
	a.handleVerifyInstallation(verifyRR, verifyReq)
	if verifyRR.Code != http.StatusOK {
		t.Fatalf("verify installation: expected 200, got %d: %s", verifyRR.Code, verifyRR.Body.String())
	}
	// discordCompleted is the website wizard's own explicit "Discord step
	// done" signal (PATCH .../setup, section 8) - verify-installation only
	// confirms live bot presence, it never sets this flag itself.
	patchReq := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", setupProgressPatchRequest{DiscordCompleted: boolPtr(true), CurrentStep: strPtr("NITRADO")}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	patchRR := httptest.NewRecorder()
	a.handleUpdateSetupProgress(patchRR, patchReq)
	if patchRR.Code != http.StatusOK {
		t.Fatalf("patch discordCompleted: expected 200, got %d: %s", patchRR.Code, patchRR.Body.String())
	}

	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	if rr := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("select dayz server: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	killfeedChannelID = "c-killfeed"
	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: killfeedChannelID, Name: "killfeed", Type: discordgo.ChannelTypeGuildText})
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": killfeedChannelID}); rr.Code != http.StatusOK {
		t.Fatalf("save channel routes: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Full live permission pass: channel found, nothing missing.
	verifier.channelFound[fixture.DiscordGuildID+"|"+killfeedChannelID] = true

	return fixture, killfeedChannelID
}

// TestSetupProgressSurvivesReloadAtExactStep covers cases A/B: partial setup
// (Discord connected, Nitrado not yet) persists and GET .../setup returns
// exactly the saved currentStep on a fresh read - never restarting at step 1.
func TestSetupProgressSurvivesReloadAtExactStep(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	patch := setupProgressPatchRequest{DiscordCompleted: boolPtr(true), CurrentStep: strPtr("NITRADO")}
	patchReq := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", patch), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	patchRR := httptest.NewRecorder()
	a.handleUpdateSetupProgress(patchRR, patchReq)
	if patchRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", patchRR.Code, patchRR.Body.String())
	}

	// A fresh GET (simulating a reload) must return exactly what was saved.
	getReq := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	getRR := httptest.NewRecorder()
	a.handleGetSetupProgress(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	got := decodeBody[SetupProgressSummary](t, getRR)
	if !got.DiscordCompleted || got.NitradoCompleted || got.CurrentStep != "NITRADO" {
		t.Fatalf("expected discordCompleted=true nitradoCompleted=false currentStep=NITRADO exactly as saved, got %+v", got)
	}
}

// TestFinalizeSetupRequiresEachPrerequisite covers cases C/D/E/F: finalize
// must fail cleanly at each missing prerequisite, in the persisted-state
// order the backend actually checks them, never trusting a client claim.
func TestFinalizeSetupRequiresEachPrerequisite(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	// C: before Discord is verified/completed at all.
	rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code == http.StatusOK {
		t.Fatal("expected finalize to fail before Discord verification")
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)

	verifyReq := withPathValues(withActingUser(saasRequest(http.MethodPost, "/x", nil), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	a.handleVerifyInstallation(httptest.NewRecorder(), verifyReq)
	patchReq := withPathValues(withActingUser(saasRequest(http.MethodPatch, "/x", setupProgressPatchRequest{DiscordCompleted: boolPtr(true), CurrentStep: strPtr("NITRADO")}), fixture.OwnerDiscordID),
		map[string]string{"organizationID": strconv.FormatInt(fixture.OrgID, 10), "installationID": strconv.FormatInt(fixture.InstallationID, 10)})
	a.handleUpdateSetupProgress(httptest.NewRecorder(), patchReq)

	// D: Discord done, Nitrado not connected yet.
	rr = finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code == http.StatusOK {
		t.Fatal("expected finalize to fail before Nitrado is connected")
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)

	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)

	// E: Nitrado connected, no DayZ server selected yet.
	rr = finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code == http.StatusOK {
		t.Fatal("expected finalize to fail before a DayZ server is selected")
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)

	if rr := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("select dayz server: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// F: server selected, no KILLFEED route configured yet.
	rr = finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code == http.StatusOK {
		t.Fatal("expected finalize to fail without a KILLFEED route")
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status == repository.InstallationReady {
		t.Fatalf("expected the installation to never reach READY through any of these failed attempts, got %+v err=%v", inst, err)
	}
}

// TestFinalizeSetupRequiresPassingPermissions is case G: even with every
// other prerequisite met, a configured channel missing a blocking Discord
// permission (View Channel/Send Messages) must prevent finalization.
func TestFinalizeSetupRequiresPassingPermissions(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, killfeedChannelID := buildFinalizableFixture(t, a, verifier)

	// Override the full pass buildFinalizableFixture set up: this channel is
	// found, but missing a BLOCKING capability.
	verifier.channelFound[fixture.DiscordGuildID+"|"+killfeedChannelID] = true
	verifier.missing[fixture.DiscordGuildID+"|"+killfeedChannelID] = []string{"Send Messages"}

	rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code == http.StatusOK {
		t.Fatal("expected finalize to fail when a configured channel is missing a blocking permission")
	}
	assertErrorCode(t, rr, codeInstallationNotVerified)

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status == repository.InstallationReady {
		t.Fatalf("expected the installation to remain non-READY, got %+v err=%v", inst, err)
	}
}

// TestFinalizeSetupNonBlockingWarningsStillPass covers section 4: a missing
// WARNING-only capability (Embed Links/Read Message History) must NOT block
// finalization - only View Channel/Send Messages do.
func TestFinalizeSetupNonBlockingWarningsStillPass(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, killfeedChannelID := buildFinalizableFixture(t, a, verifier)
	verifier.missing[fixture.DiscordGuildID+"|"+killfeedChannelID] = []string{"Embed Links", "Read Message History"}

	rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 despite non-blocking warnings, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestFinalizeSetupSucceeds covers cases H/I/J/K: a fully-configured,
// passing-permissions installation finalizes into READY, currentStep
// COMPLETE, validationCompleted true, with both completion timestamps
// stamped exactly once.
func TestFinalizeSetupSucceeds(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, _ := buildFinalizableFixture(t, a, verifier)

	rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBody[FinalizeSetupResponse](t, rr)
	if !resp.Completed || resp.Installation.Status != repository.InstallationReady {
		t.Fatalf("expected completed=true status=READY, got %+v", resp)
	}

	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ValidationCompleted || progress.CurrentStep != "COMPLETE" || progress.CompletedAt == nil {
		t.Fatalf("expected validationCompleted=true currentStep=COMPLETE completedAt stamped, got %+v err=%v", progress, err)
	}
	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationReady || inst.SetupCompletedAt == nil {
		t.Fatalf("expected status=READY setupCompletedAt stamped, got %+v err=%v", inst, err)
	}
	firstCompletedAt := *progress.CompletedAt
	firstSetupCompletedAt := *inst.SetupCompletedAt

	// K/L: a repeated finalize call is idempotent - success, never resets
	// either timestamp.
	secondRR := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if secondRR.Code != http.StatusOK {
		t.Fatalf("expected 200 on repeat finalize, got %d: %s", secondRR.Code, secondRR.Body.String())
	}
	secondResp := decodeBody[FinalizeSetupResponse](t, secondRR)
	if !secondResp.Completed {
		t.Fatal("expected completed=true on repeat finalize")
	}
	progress2, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress2 == nil || progress2.CompletedAt == nil || !progress2.CompletedAt.Equal(firstCompletedAt) {
		t.Fatalf("expected completedAt unchanged across repeat finalize, first=%v second=%+v err=%v", firstCompletedAt, progress2, err)
	}
	inst2, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst2 == nil || inst2.SetupCompletedAt == nil || !inst2.SetupCompletedAt.Equal(firstSetupCompletedAt) {
		t.Fatalf("expected setupCompletedAt unchanged across repeat finalize, first=%v second=%+v err=%v", firstSetupCompletedAt, inst2, err)
	}
}

// TestFinalizeSetupCrossTenantRejected covers tenant isolation for the
// finalize endpoint.
func TestFinalizeSetupCrossTenantRejected(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA, _ := buildFinalizableFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)

	rr := finalizeSetup(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant finalize, got %d: %s", rr.Code, rr.Body.String())
	}
	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixtureA.OrgID, fixtureA.InstallationID)
	if err != nil || inst == nil || inst.Status == repository.InstallationReady {
		t.Fatalf("expected A's installation untouched by B's cross-tenant attempt, got %+v err=%v", inst, err)
	}
}

// TestHubSummaryReturnsSavedState is case M: the hub reflects the saved
// Discord connection, DayZ server, channel routes, and general settings.
func TestHubSummaryReturnsSavedState(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, killfeedChannelID := buildFinalizableFixture(t, a, verifier)
	if rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("finalize: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr := saveInstallationSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationGeneralSettings{
		Timezone: "America/New_York", DistanceUnit: "FEET", OnlineDisplayEnabled: true, LeaderboardEnabled: false,
	}); rr.Code != http.StatusOK {
		t.Fatalf("save settings: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr := getHub(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	hub := decodeBody[HubSummary](t, rr)
	if hub.Organization.ID != fixture.OrgID {
		t.Fatalf("expected the organization summary, got %+v", hub.Organization)
	}
	if hub.Installation.Status != repository.InstallationReady {
		t.Fatalf("expected installation.status=READY, got %+v", hub.Installation)
	}
	if hub.Discord == nil || !hub.Discord.BotInstalled {
		t.Fatalf("expected discord.botInstalled=true, got %+v", hub.Discord)
	}
	if hub.DayZServer == nil || hub.DayZServer.Platform != "PLAYSTATION" {
		t.Fatalf("expected the selected PLAYSTATION dayzServer, got %+v", hub.DayZServer)
	}
	if hub.ChannelRoutes["KILLFEED"].ChannelID != killfeedChannelID {
		t.Fatalf("expected channelRoutes.KILLFEED=%q, got %+v", killfeedChannelID, hub.ChannelRoutes)
	}
	if hub.Settings.Timezone != "America/New_York" || hub.Settings.DistanceUnit != "FEET" || !hub.Settings.OnlineDisplayEnabled || hub.Settings.LeaderboardEnabled {
		t.Fatalf("expected the saved general settings, got %+v", hub.Settings)
	}
}

// TestHubSummaryNoSecrets is case N: marshal the hub response and assert it
// never contains anything credential-shaped, using the same forbidden-token
// list TestNoSensitiveFieldsInAPIResponses uses.
func TestHubSummaryNoSecrets(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, _ := buildFinalizableFixture(t, a, verifier)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)

	rr := getHub(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := strings.ToLower(rr.Body.String())
	forbidden := []string{
		"ciphertext", "credential_nonce", "authtag", "auth_tag", "bottoken", "bot_token",
		"oauthtoken", "oauth_token", "websiteapisecret", "website_api_secret", "database_url", "databaseurl",
		"\"token\"", "nitrado_token", "nitradotoken", "fake-nitrado-token",
	}
	for _, bad := range forbidden {
		if strings.Contains(body, strings.ToLower(bad)) {
			t.Fatalf("hub response contained forbidden field %q: %s", bad, rr.Body.String())
		}
	}
}

// TestHubCrossTenantRejected is case Q: organization B can never read
// organization A's hub.
func TestHubCrossTenantRejected(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixtureA, _ := buildFinalizableFixture(t, a, verifier)
	fixtureB := buildInstallationFixture(t, a, verifier)

	rr := getHub(t, a, fixtureB.OrgID, fixtureA.InstallationID, fixtureB.OwnerDiscordID)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-tenant hub access, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestInstallationSettingsPersistAndAreCustomerEditable covers the settings
// GET/PUT contract (section 10/11).
func TestInstallationSettingsPersistAndAreCustomerEditable(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	saveRR := saveInstallationSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationGeneralSettings{
		Timezone: "Europe/Berlin", DistanceUnit: "meters", OnlineDisplayEnabled: false, LeaderboardEnabled: true,
	})
	if saveRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", saveRR.Code, saveRR.Body.String())
	}
	saved := decodeBody[InstallationGeneralSettings](t, saveRR)
	if saved.Timezone != "Europe/Berlin" || saved.DistanceUnit != "METERS" || saved.OnlineDisplayEnabled || !saved.LeaderboardEnabled {
		t.Fatalf("expected the saved settings (distanceUnit normalized to uppercase), got %+v", saved)
	}

	getRR := getInstallationSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID)
	if getRR.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", getRR.Code, getRR.Body.String())
	}
	if got := decodeBody[InstallationGeneralSettings](t, getRR); got != saved {
		t.Fatalf("expected GET to restore exactly what was saved, got %+v want %+v", got, saved)
	}
}

// TestInstallationSettingsRejectsInvalidDistanceUnit covers input
// validation for the new settings endpoint.
func TestInstallationSettingsRejectsInvalidDistanceUnit(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)

	rr := saveInstallationSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationGeneralSettings{
		Timezone: "UTC", DistanceUnit: "LIGHTYEARS",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid distanceUnit, got %d: %s", rr.Code, rr.Body.String())
	}
	assertErrorCode(t, rr, codeInvalidRequest)
}

// TestNonCriticalSettingsChangeKeepsReady is case O: changing timezone/
// distance unit/display toggles on a READY installation must never
// invalidate its validation or move it out of READY.
func TestNonCriticalSettingsChangeKeepsReady(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, _ := buildFinalizableFixture(t, a, verifier)
	if rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("finalize: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if rr := saveInstallationSettings(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, InstallationGeneralSettings{
		Timezone: "Asia/Tokyo", DistanceUnit: "FEET", OnlineDisplayEnabled: true, LeaderboardEnabled: true,
	}); rr.Code != http.StatusOK {
		t.Fatalf("save settings: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationReady {
		t.Fatalf("expected status to remain READY after a non-critical settings change, got %+v err=%v", inst, err)
	}
	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || !progress.ValidationCompleted {
		t.Fatalf("expected validationCompleted to remain true, got %+v err=%v", progress, err)
	}
}

// TestCriticalKillfeedRouteChangeInvalidatesValidation is case P: changing
// the KILLFEED route's channel on a READY installation must downgrade it
// back to CONFIGURING/VALIDATION - a stale permission-verification result
// for the OLD channel can never be trusted for a different one.
func TestCriticalKillfeedRouteChangeInvalidatesValidation(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, _ := buildFinalizableFixture(t, a, verifier)
	if rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("finalize: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: "c-new-killfeed", Name: "new-killfeed", Type: discordgo.ChannelTypeGuildText})
	if rr := saveChannelRoutes(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID, map[string]string{"KILLFEED": "c-new-killfeed"}); rr.Code != http.StatusOK {
		t.Fatalf("save channel routes: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationConfiguring {
		t.Fatalf("expected status=CONFIGURING after a critical KILLFEED route change, got %+v err=%v", inst, err)
	}
	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || progress.ValidationCompleted || progress.CurrentStep != "VALIDATION" {
		t.Fatalf("expected validationCompleted=false currentStep=VALIDATION, got %+v err=%v", progress, err)
	}

	// Re-finalizing against the NEW channel (also passing) must succeed
	// again - this is a re-validation, not a one-way lockout.
	verifier.channelFound[fixture.DiscordGuildID+"|c-new-killfeed"] = true
	if rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("re-finalize: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestCriticalDayZServerChangeInvalidatesValidation covers the DayZ-server
// half of section 13's critical-change list: selecting a genuinely
// different server on a READY installation must also downgrade it.
func TestCriticalDayZServerChangeInvalidatesValidation(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture, _ := buildFinalizableFixture(t, a, verifier)
	// buildFinalizableFixture installs a PlayStation-only fake Nitrado
	// account; switch to the mixed one so a second server can be selected.
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	if rr := finalizeSetup(t, a, fixture.OrgID, fixture.InstallationID, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("finalize: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Select a DIFFERENT server (222222, Xbox) on the same, already-READY installation.
	if rr := selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 222222, fixture.OwnerDiscordID); rr.Code != http.StatusOK {
		t.Fatalf("select different dayz server: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	inst, err := a.SaaSInstallations.GetScoped(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || inst == nil || inst.Status != repository.InstallationConfiguring {
		t.Fatalf("expected status=CONFIGURING after a critical DayZ server change, got %+v err=%v", inst, err)
	}
	progress, err := a.SaaSInstallations.GetSetupProgress(context.Background(), fixture.OrgID, fixture.InstallationID)
	if err != nil || progress == nil || progress.ValidationCompleted {
		t.Fatalf("expected validationCompleted=false, got %+v err=%v", progress, err)
	}
}
