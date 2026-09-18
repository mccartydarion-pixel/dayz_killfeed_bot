package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// TestSaaSHandlersRejectMissingServiceAuth is case A: every SaaS route must
// reject a request that carries no (or the wrong) WEBSITE_API_SECRET bearer
// token, before touching anything else.
func TestSaaSHandlersRejectMissingServiceAuth(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "secret"}}

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"user sync", a.handleUserSync},
		{"list organizations", a.handleListOrganizations},
		{"create organization", a.handleCreateOrganization},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/saas/x", strings.NewReader("{}"))
			rr := httptest.NewRecorder()
			c.handler(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no auth header, got %d", rr.Code)
			}
			assertErrorCode(t, rr, codeUnauthorized)

			req = httptest.NewRequest(http.MethodPost, "/api/saas/x", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer wrong-secret")
			rr = httptest.NewRecorder()
			c.handler(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with wrong secret, got %d", rr.Code)
			}
			assertErrorCode(t, rr, codeUnauthorized)
		})
	}
}

// TestResolveActingUserRequiresSyncedUser proves a request that passes
// service auth but has never synced (no acting-user header, or a Discord ID
// with no app_users row) is still rejected.
func TestResolveActingUserRequiresHeader(t *testing.T) {
	a := &App{Config: &config.Config{WebsiteAPISecret: "secret"}, SaaSUsers: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/saas/organizations", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()

	a.handleListOrganizations(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a missing acting-user header, got %d", rr.Code)
	}
	assertErrorCode(t, rr, codeUnauthorized)
}

func assertErrorCode(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env apiErrorEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("expected a JSON error envelope, got %q: %v", rr.Body.String(), err)
	}
	if env.Error.Code != want {
		t.Fatalf("expected error code %q, got %q", want, env.Error.Code)
	}
}

// --- setup progress transition validation --------------------------------

func TestValidateSetupProgressOrderRejectsSkippedSteps(t *testing.T) {
	cases := []struct {
		name  string
		state saasProgressState
		want  bool
	}{
		{"all false is valid", saasProgressState{}, true},
		{"discord only is valid", saasProgressState{DiscordCompleted: true}, true},
		{"nitrado without discord is invalid", saasProgressState{NitradoCompleted: true}, false},
		{"server without nitrado is invalid", saasProgressState{DiscordCompleted: true, ServerSelected: true}, false},
		{"channels without server is invalid", saasProgressState{DiscordCompleted: true, NitradoCompleted: true, ChannelsCompleted: true}, false},
		{"validation without channels is invalid", saasProgressState{DiscordCompleted: true, NitradoCompleted: true, ServerSelected: true, ValidationCompleted: true}, false},
		{"full chain is valid", saasProgressState{DiscordCompleted: true, NitradoCompleted: true, ServerSelected: true, ChannelsCompleted: true, ValidationCompleted: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateSetupProgressOrder(c.state.toRepo()); got != c.want {
				t.Fatalf("expected %v, got %v", c.want, got)
			}
		})
	}
}

// saasProgressState is a small test-local convenience for building
// repository.InstallationSetupProgress values by name in table tests.
type saasProgressState struct {
	DiscordCompleted, NitradoCompleted, ServerSelected, ChannelsCompleted, ValidationCompleted bool
}

func (s saasProgressState) toRepo() repository.InstallationSetupProgress {
	return repository.InstallationSetupProgress{
		DiscordCompleted:    s.DiscordCompleted,
		NitradoCompleted:    s.NitradoCompleted,
		ServerSelected:      s.ServerSelected,
		ChannelsCompleted:   s.ChannelsCompleted,
		ValidationCompleted: s.ValidationCompleted,
	}
}

// --- permission verification mapping (case K) -----------------------------

func TestMapPermissionVerificationUnreachableFailsEverything(t *testing.T) {
	result := mapPermissionVerification(discord.Verification{GuildFound: true, ChannelFound: false})
	if result.OverallPass {
		t.Fatal("expected an unreachable channel to fail overall")
	}
	if len(result.Capabilities) != 4 {
		t.Fatalf("expected 4 capability results, got %d", len(result.Capabilities))
	}
	for _, c := range result.Capabilities {
		if c.Result != resultFail {
			t.Fatalf("expected every capability to FAIL when the channel is unreachable, got %s=%s", c.Capability, c.Result)
		}
	}
}

func TestMapPermissionVerificationBlockingVsWarning(t *testing.T) {
	v := discord.Verification{
		GuildFound:   true,
		ChannelFound: true,
		Missing:      []string{"Send Messages", "Embed Links"},
	}
	result := mapPermissionVerification(v)
	if result.OverallPass {
		t.Fatal("expected a missing blocking capability (Send Messages) to fail overall")
	}
	got := map[string]string{}
	for _, c := range result.Capabilities {
		got[c.Capability] = c.Result
	}
	if got["View Channel"] != resultPass {
		t.Fatalf("expected View Channel PASS, got %s", got["View Channel"])
	}
	if got["Send Messages"] != resultFail {
		t.Fatalf("expected Send Messages FAIL (blocking), got %s", got["Send Messages"])
	}
	if got["Embed Links"] != resultWarning {
		t.Fatalf("expected Embed Links WARNING (non-blocking), got %s", got["Embed Links"])
	}
	if got["Read Message History"] != resultPass {
		t.Fatalf("expected Read Message History PASS, got %s", got["Read Message History"])
	}
}

func TestMapPermissionVerificationAllPresent(t *testing.T) {
	result := mapPermissionVerification(discord.Verification{GuildFound: true, ChannelFound: true})
	if !result.OverallPass {
		t.Fatal("expected everything present to overall pass")
	}
	for _, c := range result.Capabilities {
		if c.Result != resultPass {
			t.Fatalf("expected every capability PASS, got %s=%s", c.Capability, c.Result)
		}
	}
}

// --- no sensitive fields ever serialize (case L) --------------------------

// TestNoSensitiveFieldsInAPIResponses marshals one instance of every
// response DTO this API returns and asserts the combined JSON never
// contains a credential-shaped field name (section 18). This is a
// regression guard, not exhaustive - it fails loudly if a future field
// named like a secret is ever added to one of these types.
func TestNoSensitiveFieldsInAPIResponses(t *testing.T) {
	now := time.Now()
	nowStr := &[]string{now.Format(time.RFC3339)}[0]

	samples := []any{
		UserSummary{ID: 1, DiscordUserID: "1", DiscordUsername: "x"},
		OrganizationSummary{ID: 1, Name: "x", Slug: "x", Role: "OWNER"},
		DashboardSummary{
			Organization: OrganizationSummary{ID: 1},
			Subscription: &SubscriptionSummary{Plan: "TRIAL", Status: "TRIAL", TrialEndsAt: nowStr},
			Installations: []InstallationSummary{{
				ID:                1,
				Status:            "READY",
				SetupProgress:     &SetupProgressSummary{CurrentStep: "COMPLETE"},
				DiscordConnection: &DiscordGuildConnectionSummary{ID: 1, GuildName: "x", ConnectedAt: now.Format(time.RFC3339)},
				DayZServer:        &DayZServerSummary{ID: 1, Game: "DayZ", Platform: "PLAYSTATION", Status: "CONNECTED"},
			}},
		},
		InstallationSummary{ID: 1, Status: "READY"},
		SetupProgressSummary{CurrentStep: "DISCORD"},
		DiscordGuildConnectionSummary{ID: 1, GuildName: "x", GuildIcon: "y", ConnectedAt: now.Format(time.RFC3339)},
		DiscordGuildSummary{DiscordGuildID: "1", GuildName: "x", Eligible: true, BotInstalled: true},
		DiscordVerificationResult{Installed: true, GuildReachable: true, VerifiedAt: now.Format(time.RFC3339)},
		PermissionVerificationResult{Capabilities: []CapabilityCheck{{Capability: "View Channel", Result: "PASS"}}, OverallPass: true},
		apiErrorEnvelope{Error: apiError{Code: codeInternalError, Message: "x"}},
		NitradoConnectResponse{Connected: true, ServicesFound: 3},
		NitradoServiceSummary{ServiceID: 123456, Name: "x", Game: "DayZ", Platform: "PLAYSTATION", Status: "ONLINE"},
		[]NitradoServiceSummary{{ServiceID: 987654, Name: "y", Game: "DayZ", Platform: "XBOX", Status: "ONLINE"}},
		DayZServerSelection{ID: 42, ServiceID: 123456, DisplayName: "x", Game: "DayZ", Platform: "PLAYSTATION", Status: "ONLINE"},
		SelectDayZServerResponse{
			Server:             DayZServerSelection{ID: 42, ServiceID: 123456, DisplayName: "x", Game: "DayZ", Platform: "PLAYSTATION", Status: "ONLINE"},
			InstallationID:     4,
			ReusedInstallation: true,
		},
		DayZServerValidation{Reachable: true, Supported: true, Platform: "XBOX", Message: "DayZ Xbox server connected."},
		DiscordChannelSummary{ID: "1", Name: "champion-killfeed", Type: "TEXT", Position: 1, CanSend: true},
		[]DiscordChannelSummary{{ID: "1", Name: "champion-killfeed", Type: "TEXT"}},
		InstallationChannelSettings{KillfeedChannelID: "1", LeaderboardChannelID: "2", PlayerStatusChannelID: "3", AdminLogChannelID: "4"},
		ChannelCategorySummary{ID: "1", Name: "CHAMPION KILLFEED"},
		AutoSetupChannelsResponse{
			Configured: true,
			Category:   &ChannelCategorySummary{ID: "1", Name: "CHAMPION KILLFEED"},
			Channels:   &InstallationChannelSettings{KillfeedChannelID: "1", LeaderboardChannelID: "2", PlayerStatusChannelID: "3", AdminLogChannelID: "4"},
		},
		AutoSetupChannelsResponse{Configured: false, Reason: "MISSING_MANAGE_CHANNELS"},
		AutoSetupChannelsResponse{Configured: false, Reason: "CUSTOM_CONFIGURATION_EXISTS"},
	}

	forbidden := []string{
		"ciphertext", "credential_nonce", "authtag", "auth_tag", "bottoken", "bot_token",
		"oauthtoken", "oauth_token", "websiteapisecret", "website_api_secret", "database_url", "databaseurl",
		"\"token\"", "nitrado_token", "nitradotoken",
	}

	for _, sample := range samples {
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatalf("marshal %T: %v", sample, err)
		}
		lower := strings.ToLower(string(data))
		for _, bad := range forbidden {
			if strings.Contains(lower, strings.ToLower(bad)) {
				t.Fatalf("response type %T serialized a forbidden field %q: %s", sample, bad, data)
			}
		}
	}
}

// --- rate limiting ----------------------------------------------------------

func TestSaaSRateLimiterBlocksAfterMax(t *testing.T) {
	l := newSaaSRateLimiter(time.Minute, 2)
	if !l.Allow("user-1") || !l.Allow("user-1") {
		t.Fatal("expected the first two attempts to be allowed")
	}
	if l.Allow("user-1") {
		t.Fatal("expected the third attempt within the window to be blocked")
	}
	if !l.Allow("user-2") {
		t.Fatal("expected a different key to have its own independent budget")
	}
}

// --- console DayZ service filtering (section 14) ---------------------------

// TestToDayZServerSummaryIncludesServiceID is the dayz-server-refresh fix:
// InstallationSummary.dayzServer must include the persisted Nitrado
// serviceId, parsed from repository.GameServer.ProviderServiceID (stored as
// a string), so the website can hydrate the selected DayZ server from a
// dashboard/installation response after a refresh without re-selecting it.
func TestToDayZServerSummaryIncludesServiceID(t *testing.T) {
	server := repository.GameServer{
		ID:                7,
		ProviderServiceID: "123456",
		DisplayName:       "Champions",
		Game:              "DayZ",
		Platform:          "PLAYSTATION",
		Status:            "ONLINE",
	}

	got := toDayZServerSummary(server)

	want := DayZServerSummary{
		ID:          7,
		ServiceID:   123456,
		DisplayName: "Champions",
		Game:        "DayZ",
		Platform:    "PLAYSTATION",
		Status:      "ONLINE",
	}
	if got != want {
		t.Fatalf("expected %+v, got %+v", want, got)
	}

	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"serviceId":123456`) {
		t.Fatalf("expected serialized serviceId=123456, got %s", data)
	}
}

// TestToDayZServerSummaryDoesNotPanicOnMalformedServiceID proves malformed
// persisted data (should not happen for a Nitrado-backed row, but this is
// stored data, not a fresh API response) never panics - ServiceID just
// stays at its zero value.
func TestToDayZServerSummaryDoesNotPanicOnMalformedServiceID(t *testing.T) {
	got := toDayZServerSummary(repository.GameServer{ID: 1, ProviderServiceID: "not-a-number"})
	if got.ServiceID != 0 {
		t.Fatalf("expected ServiceID to stay 0 for malformed data, got %d", got.ServiceID)
	}
}

// TestSupportedNitradoDayZServicesFiltersAndClassifies is the mixed-list
// case from section 14: PlayStation and Xbox DayZ services are listed
// (multiple of each), PC DayZ and an unrelated game are dropped entirely -
// never exposed in a setup response (section 5).
func TestSupportedNitradoDayZServicesFiltersAndClassifies(t *testing.T) {
	services := []nitrado.Service{
		{ID: "1", Game: "DayZ (PS4)", Status: "active", Details: nitrado.ServiceDetails{Name: "PS Server One"}},
		{ID: "2", Game: "DayZ (PS4)", Status: "suspended", Details: nitrado.ServiceDetails{Name: "PS Server Two"}},
		{ID: "3", Game: "DayZ (Xbox)", Status: "active", Details: nitrado.ServiceDetails{Name: "Xbox Server One"}},
		{ID: "4", Game: "DayZ", Status: "active", Details: nitrado.ServiceDetails{Name: "PC Server"}},
		{ID: "5", Game: "Minecraft", Status: "active", Details: nitrado.ServiceDetails{Name: "MC Server"}},
	}

	got := supportedNitradoDayZServices(services)
	if len(got) != 3 {
		t.Fatalf("expected 3 supported services (2 PS + 1 Xbox), got %d: %+v", len(got), got)
	}

	byID := map[int64]NitradoServiceSummary{}
	for _, s := range got {
		byID[s.ServiceID] = s
	}
	if s, ok := byID[1]; !ok || s.Platform != "PLAYSTATION" || s.Status != "ONLINE" || s.Name != "PS Server One" {
		t.Fatalf("expected PS server 1 online and correctly named, got %+v (ok=%v)", s, ok)
	}
	if s, ok := byID[2]; !ok || s.Platform != "PLAYSTATION" || s.Status != "OFFLINE" {
		t.Fatalf("expected PS server 2 offline (suspended), got %+v (ok=%v)", s, ok)
	}
	if s, ok := byID[3]; !ok || s.Platform != "XBOX" {
		t.Fatalf("expected Xbox server 3, got %+v (ok=%v)", s, ok)
	}
	if _, ok := byID[4]; ok {
		t.Fatal("expected DayZ PC (service 4) to be excluded entirely")
	}
	if _, ok := byID[5]; ok {
		t.Fatal("expected the unrelated game (service 5) to be excluded entirely")
	}
}

// --- channel auto-setup pure functions (Step 5) -----------------------------

func TestNormalizeChannelName(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantOK   bool
	}{
		{"Kill Feed", "kill-feed", true},
		{"  pvp-feed  ", "pvp-feed", true},
		{"Kill Feed!!", "kill-feed", true},
		{"already-lower_case", "already-lower_case", true},
		{"multiple   spaces", "multiple-spaces", true},
		{"---leading-and-trailing---", "leading-and-trailing", true},
		{"", "", false},
		{"   ", "", false},
		{"!!!", "", false},
		{"emoji😀name", "emojiname", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := normalizeChannelName(c.in)
			if ok != c.wantOK {
				t.Fatalf("normalizeChannelName(%q): expected ok=%v, got ok=%v (name=%q)", c.in, c.wantOK, ok, got)
			}
			if ok && got != c.wantName {
				t.Fatalf("normalizeChannelName(%q): expected %q, got %q", c.in, c.wantName, got)
			}
		})
	}
}

func TestNormalizeChannelNameRejectsOverlyLongNames(t *testing.T) {
	_, ok := normalizeChannelName(strings.Repeat("a", 101))
	if ok {
		t.Fatal("expected a 101-character name to be rejected")
	}
	got, ok := normalizeChannelName(strings.Repeat("a", 100))
	if !ok || len(got) != 100 {
		t.Fatalf("expected a 100-character name to be accepted as-is, got %q ok=%v", got, ok)
	}
}

func TestChannelTypeLabel(t *testing.T) {
	if got := channelTypeLabel(discordgo.ChannelTypeGuildNews); got != "ANNOUNCEMENT" {
		t.Fatalf("expected ANNOUNCEMENT for a news channel, got %q", got)
	}
	if got := channelTypeLabel(discordgo.ChannelTypeGuildText); got != "TEXT" {
		t.Fatalf("expected TEXT for a text channel, got %q", got)
	}
}

func TestHasCustomChannelConfiguration(t *testing.T) {
	cases := []struct {
		name string
		s    repository.InstallationSettings
		want bool
	}{
		{"never configured", repository.InstallationSettings{}, false},
		{"manual with killfeed set", repository.InstallationSettings{KillfeedChannelID: "1"}, true},
		{"legacy data with empty source", repository.InstallationSettings{KillfeedChannelID: "1", ChannelSetupSource: ""}, true},
		{"auto-configured is not custom", repository.InstallationSettings{KillfeedChannelID: "1", ChannelSetupSource: "AUTO"}, false},
		{"explicitly manual", repository.InstallationSettings{KillfeedChannelID: "1", ChannelSetupSource: "MANUAL"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasCustomChannelConfiguration(c.s); got != c.want {
				t.Fatalf("hasCustomChannelConfiguration(%+v): expected %v, got %v", c.s, c.want, got)
			}
		})
	}
}

func TestEnsureManagedCategoryPrefersPersistedIDThenName(t *testing.T) {
	a := &App{}
	channels := []discord.RawGuildChannel{
		{ID: "cat-old", Name: "CHAMPION KILLFEED", Type: discordgo.ChannelTypeGuildCategory},
		{ID: "cat-new", Name: "champion killfeed", Type: discordgo.ChannelTypeGuildCategory},
	}

	// Persisted ID wins even though a differently-cased name match also exists.
	got, err := a.ensureManagedCategory("guild-1", channels, "cat-old")
	if err != nil || got.ID != "cat-old" {
		t.Fatalf("expected the persisted category ID to win, got %+v err=%v", got, err)
	}

	// No persisted ID (or a stale one) falls back to the case-insensitive name match.
	got, err = a.ensureManagedCategory("guild-1", channels, "does-not-exist")
	if err != nil || got.ID != "cat-old" {
		t.Fatalf("expected the first name match to be reused, got %+v err=%v", got, err)
	}
}
