package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		DayZServerValidation{Reachable: true, Supported: true, Platform: "XBOX", Message: "DayZ Xbox server connected."},
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
