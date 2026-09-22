package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/routing"
	"github.com/yourname/dayz-killfeed/internal/server"
)

const (
	adminTestSecret = "test-service-secret"
	adminTestAdmin  = "900000000000000001"
)

type fakeAdminReader struct {
	mu    sync.Mutex
	calls int
	org   adminrepo.OrganizationFilter
	subs  adminrepo.SubscriptionFilter
	inst  adminrepo.InstallationFilter

	orgs      []adminrepo.Organization
	orgNext   int64
	guildName string // injected into installation rows (secret-leak test)
	err       error
}

func (f *fakeAdminReader) hit()                       { f.mu.Lock(); f.calls++; f.mu.Unlock() }
func (f *fakeAdminReader) Ping(context.Context) error { f.hit(); return nil }
func (f *fakeAdminReader) Overview(context.Context) (adminrepo.Overview, error) {
	f.hit()
	return adminrepo.Overview{Organizations: 3, Users: 5, Installations: adminrepo.InstallationCounts{Total: 4, Ready: 2, Configuring: 1, Disconnected: 1}, Subscriptions: adminrepo.SubscriptionCounts{Total: 3, Trial: 2, Active: 1}}, f.err
}
func (f *fakeAdminReader) ListOrganizations(_ context.Context, fl adminrepo.OrganizationFilter) ([]adminrepo.Organization, int64, error) {
	f.hit()
	f.org = fl
	return f.orgs, f.orgNext, f.err
}
func (f *fakeAdminReader) GetOrganization(_ context.Context, id int64) (*adminrepo.Organization, error) {
	f.hit()
	if id == 404 {
		return nil, nil
	}
	return &adminrepo.Organization{ID: id, Name: "Org", Members: []adminrepo.MemberRow{}, Installations: []adminrepo.InstallationSummary{}}, f.err
}
func (f *fakeAdminReader) ListSubscriptions(_ context.Context, fl adminrepo.SubscriptionFilter) ([]adminrepo.SubscriptionRow, int64, error) {
	f.hit()
	f.subs = fl
	return nil, 0, f.err
}
func (f *fakeAdminReader) ListInstallations(_ context.Context, fl adminrepo.InstallationFilter) ([]adminrepo.InstallationSummary, int64, error) {
	f.hit()
	f.inst = fl
	if f.guildName != "" {
		return []adminrepo.InstallationSummary{{ID: 1, InstallationID: 1, Discord: adminrepo.DiscordInfo{GuildName: f.guildName}}}, 0, f.err
	}
	return nil, 0, f.err
}
func (f *fakeAdminReader) GetInstallation(_ context.Context, id int64) (*adminrepo.InstallationDetail, error) {
	f.hit()
	if id == 404 {
		return nil, nil
	}
	return &adminrepo.InstallationDetail{ChannelRoutes: []adminrepo.ChannelRoute{{RouteKey: "KILLFEED", ChannelID: "c-1"}}}, f.err
}
func (f *fakeAdminReader) InstallationHealth(context.Context) (adminrepo.HealthSummary, []adminrepo.HealthInstallation, error) {
	f.hit()
	return adminrepo.HealthSummary{ByStatus: map[string]int{}, ByHealth: map[string]int{}, ServersByStatus: map[string]int{}}, []adminrepo.HealthInstallation{}, f.err
}

func newAdminTestApp(admins ...string) (*App, *fakeAdminReader) {
	fake := &fakeAdminReader{}
	return &App{Config: &config.Config{WebsiteAPISecret: adminTestSecret, AdminDiscordIDs: admins}, adminSaaS: fake}, fake
}

func adminGet(a *App, h adminHandler, target, bearer, acting string, pathValues map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if acting != "" {
		req.Header.Set(actingUserHeader, acting)
	}
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rr := httptest.NewRecorder()
	a.adminRoute(h)(rr, req)
	return rr
}

// --- A-F: authentication and authorization ------------------------------------------------------------

func TestAdminAPIAuthMatrix(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	routes := map[string]struct {
		h  adminHandler
		pv map[string]string
	}{
		"/api/admin/overview":        {a.handleAdminOverview, nil},
		"/api/admin/organizations":   {a.handleAdminListOrganizations, nil},
		"/api/admin/organizations/1": {a.handleAdminGetOrganization, map[string]string{"organizationID": "1"}},
		"/api/admin/subscriptions":   {a.handleAdminListSubscriptions, nil},
		"/api/admin/installations":   {a.handleAdminListInstallations, nil},
		"/api/admin/installations/1": {a.handleAdminGetInstallation, map[string]string{"installationID": "1"}},
		"/api/admin/health":          {a.handleAdminHealth, nil},
	}
	for path, rt := range routes {
		cases := []struct {
			name           string
			bearer, acting string
			want           int
		}{
			{"A no authorization", "", adminTestAdmin, http.StatusUnauthorized},
			{"A wrong secret", "nope", adminTestAdmin, http.StatusUnauthorized},
			{"B service auth but no acting user", adminTestSecret, "", http.StatusUnauthorized},
			{"C ordinary customer", adminTestSecret, "111222333444555666", http.StatusForbidden},
			{"D organization owner / E guild admin are not on the allowlist", adminTestSecret, "owner-of-an-org", http.StatusForbidden},
			{"F platform admin", adminTestSecret, adminTestAdmin, http.StatusOK},
		}
		for _, c := range cases {
			fake.calls = 0
			rr := adminGet(a, rt.h, path, c.bearer, c.acting, rt.pv)
			if rr.Code != c.want {
				t.Errorf("%s / %s: expected %d, got %d (%s)", path, c.name, c.want, rr.Code, rr.Body.String())
			}
			if c.want != http.StatusOK && fake.calls != 0 {
				t.Errorf("%s / %s: a rejected request must never reach the data layer", path, c.name)
			}
		}
	}
}

// An empty allowlist fails closed: nobody is a platform admin, whatever they claim.
func TestAdminAPIFailsClosedWithoutAllowlist(t *testing.T) {
	a, _ := newAdminTestApp()
	for _, id := range []string{adminTestAdmin, "1", "admin", "*"} {
		if rr := adminGet(a, a.handleAdminOverview, "/api/admin/overview", adminTestSecret, id, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("id %q with no allowlist must be forbidden, got %d", id, rr.Code)
		}
	}
	// A missing/empty service secret configuration never authenticates anyone.
	b := &App{Config: &config.Config{AdminDiscordIDs: []string{adminTestAdmin}}, adminSaaS: &fakeAdminReader{}}
	if rr := adminGet(b, b.handleAdminOverview, "/x", "", adminTestAdmin, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no configured service secret must be 401, got %d", rr.Code)
	}
}

func TestPlatformAdminAllowlistParsing(t *testing.T) {
	got := config.ParseAdminDiscordIDs(" 111111111111111111 , ,222222222222222222,111111111111111111,abc,12,3333x33333,  ")
	if len(got) != 2 || got[0] != "111111111111111111" || got[1] != "222222222222222222" {
		t.Fatalf("expected two clean unique numeric ids, got %v", got)
	}
	cfg := &config.Config{AdminDiscordIDs: got}
	if !cfg.IsPlatformAdmin(" 222222222222222222 ") || cfg.IsPlatformAdmin("222222222222222223") || cfg.IsPlatformAdmin("") {
		t.Fatal("allowlist membership must be exact")
	}
	var nilCfg *config.Config
	if nilCfg.IsPlatformAdmin("111111111111111111") {
		t.Fatal("a nil config has no admins")
	}
}

// --- N: pagination bounds, filters, cursor --------------------------------------------------------------

func TestAdminAPIPaginationBounds(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	get := func(q string) *httptest.ResponseRecorder {
		return adminGet(a, a.handleAdminListOrganizations, "/api/admin/organizations"+q, adminTestSecret, adminTestAdmin, nil)
	}
	if rr := get(""); rr.Code != 200 || fake.org.Limit != 50 {
		t.Fatalf("default limit must be 50, got %d (%d)", fake.org.Limit, rr.Code)
	}
	if rr := get("?limit=100"); rr.Code != 200 || fake.org.Limit != 100 {
		t.Fatalf("limit 100 allowed, got %d", fake.org.Limit)
	}
	if rr := get("?limit=100000"); rr.Code != 200 || fake.org.Limit != 100 {
		t.Fatalf("an oversized limit must be clamped to 100, got %d", fake.org.Limit)
	}
	for _, bad := range []string{"0", "-1", "abc", "1.5"} {
		if rr := get("?limit=" + bad); rr.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s must be rejected (never unlimited), got %d", bad, rr.Code)
		}
	}
	if rr := get("?cursor=%21%21bad"); rr.Code != http.StatusBadRequest {
		t.Fatalf("a bad cursor must be rejected, got %d", rr.Code)
	}
	if rr := get("?search=" + strings.Repeat("x", 101)); rr.Code != http.StatusBadRequest {
		t.Fatalf("an over-long search must be rejected, got %d", rr.Code)
	}
}

func TestAdminAPICursorRoundTripAndListEnvelope(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	fake.orgs = []adminrepo.Organization{{ID: 9, Name: "Nine"}}
	fake.orgNext = 9
	rr := adminGet(a, a.handleAdminListOrganizations, "/api/admin/organizations?limit=1&search=%20Nine%20", adminTestSecret, adminTestAdmin, nil)
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"nextCursor"`
		Limit      int              `json:"limit"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.NextCursor == nil || body.Limit != 1 {
		t.Fatalf("unexpected envelope %s (%v)", rr.Body.String(), err)
	}
	if fake.org.Search != "Nine" {
		t.Fatalf("search must be trimmed and passed through, got %q", fake.org.Search)
	}
	// The cursor handed out decodes back to the id, and is accepted on the next call.
	id, ok := decodeAdminCursor(*body.NextCursor)
	if !ok || id != 9 {
		t.Fatalf("cursor must round-trip, got %d %v", id, ok)
	}
	adminGet(a, a.handleAdminListOrganizations, "/api/admin/organizations?cursor="+*body.NextCursor, adminTestSecret, adminTestAdmin, nil)
	if fake.org.Cursor != 9 {
		t.Fatalf("the cursor must reach the repository, got %d", fake.org.Cursor)
	}
	// An empty result serialises as [] and a null cursor, never null items.
	fake.orgs, fake.orgNext = nil, 0
	rr = adminGet(a, a.handleAdminListOrganizations, "/api/admin/organizations", adminTestSecret, adminTestAdmin, nil)
	if !strings.Contains(rr.Body.String(), `"items":[]`) || !strings.Contains(rr.Body.String(), `"nextCursor":null`) {
		t.Fatalf("empty page must be items:[] nextCursor:null, got %s", rr.Body.String())
	}
}

func TestAdminAPIFilterValidationAndNormalization(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	do := func(h adminHandler, path string) *httptest.ResponseRecorder {
		return adminGet(a, h, path, adminTestSecret, adminTestAdmin, nil)
	}
	if rr := do(a.handleAdminListOrganizations, "/x?subscriptionStatus=active&installationStatus=ready&plan=Trial"); rr.Code != 200 ||
		fake.org.SubscriptionStatus != "ACTIVE" || fake.org.InstallationStatus != "READY" || fake.org.Plan != "Trial" {
		t.Fatalf("filters must be normalised and forwarded: %+v (%d)", fake.org, rr.Code)
	}
	// The website's customer filter sends `status` for the installation status.
	if rr := do(a.handleAdminListOrganizations, "/x?status=degraded"); rr.Code != 200 || fake.org.InstallationStatus != "DEGRADED" || fake.org.SubscriptionStatus != "" {
		t.Fatalf("`status` must mean the installation status on organizations: %+v (%d)", fake.org, rr.Code)
	}
	if rr := do(a.handleAdminListOrganizations, "/x?status=degraded&installationStatus=ready"); rr.Code != 200 || fake.org.InstallationStatus != "READY" {
		t.Fatalf("an explicit installationStatus wins over the alias: %+v", fake.org)
	}
	if rr := do(a.handleAdminListSubscriptions, "/x?status=past_due&plan=PRO"); rr.Code != 200 || fake.subs.Status != "PAST_DUE" || fake.subs.Plan != "PRO" {
		t.Fatalf("subscription filters: %+v (%d)", fake.subs, rr.Code)
	}
	if rr := do(a.handleAdminListInstallations, "/x?status=degraded&health=offline&organizationId=7"); rr.Code != 200 ||
		fake.inst.Status != "DEGRADED" || fake.inst.Health != "OFFLINE" || fake.inst.OrganizationID != 7 {
		t.Fatalf("installation filters: %+v (%d)", fake.inst, rr.Code)
	}
	// A value outside the real vocabulary (there is no "offline" installation status
	// or "trialing"/"expired" subscription status) can match nothing: the answer is a
	// well-formed EMPTY page, and the database is never asked. It is not a 400, so a
	// typo in the website's free-text filter box cannot break the whole page.
	for _, empty := range []struct {
		h    adminHandler
		path string
	}{
		{a.handleAdminListInstallations, "/x?status=offline"},
		{a.handleAdminListInstallations, "/x?health=fantastic"},
		{a.handleAdminListSubscriptions, "/x?status=trialing"},
		{a.handleAdminListSubscriptions, "/x?status=expired"},
		{a.handleAdminListSubscriptions, "/x?plan=" + "a%27%3Bdrop"},
		{a.handleAdminListOrganizations, "/x?subscriptionStatus=nope"},
		{a.handleAdminListOrganizations, "/x?installationStatus=nope"},
		{a.handleAdminListOrganizations, "/x?status=nope"},
	} {
		fake.calls = 0
		rr := do(empty.h, empty.path)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"items":[]`) || !strings.Contains(rr.Body.String(), `"nextCursor":null`) || fake.calls != 0 {
			t.Errorf("%s must be an empty page without a query, got %d %s (calls=%d)", empty.path, rr.Code, rr.Body.String(), fake.calls)
		}
	}
	// A malformed identifier, unlike an unknown filter value, is a client error.
	for _, bad := range []string{"/x?organizationId=-3", "/x?organizationId=abc"} {
		if rr := do(a.handleAdminListInstallations, bad); rr.Code != http.StatusBadRequest {
			t.Errorf("%s must be a 400, got %d", bad, rr.Code)
		}
	}
}

func TestAdminAPINotFoundAndBadIDs(t *testing.T) {
	a, _ := newAdminTestApp(adminTestAdmin)
	if rr := adminGet(a, a.handleAdminGetOrganization, "/x", adminTestSecret, adminTestAdmin, map[string]string{"organizationID": "404"}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown organization must be 404, got %d", rr.Code)
	}
	if rr := adminGet(a, a.handleAdminGetInstallation, "/x", adminTestSecret, adminTestAdmin, map[string]string{"installationID": "404"}); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown installation must be 404, got %d", rr.Code)
	}
	for _, id := range []string{"0", "-1", "abc", "1;drop"} {
		if rr := adminGet(a, a.handleAdminGetOrganization, "/x", adminTestSecret, adminTestAdmin, map[string]string{"organizationID": id}); rr.Code != http.StatusBadRequest {
			t.Fatalf("organizationID %q must be 400, got %d", id, rr.Code)
		}
	}
}

func TestAdminAPIRepositoryErrorsAreNotLeaked(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	fake.err = errors.New(`pq: relation "secret_table" does not exist at postgres://user:hunter2@db`)
	rr := adminGet(a, a.handleAdminOverview, "/x", adminTestSecret, adminTestAdmin, nil)
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "hunter2") || strings.Contains(rr.Body.String(), "secret_table") {
		t.Fatalf("a repository error must map to a fixed 500, got %d %s", rr.Code, rr.Body.String())
	}
}

// --- contract shapes ------------------------------------------------------------------------------------------

func TestAdminAPIOverviewAndHealthShapes(t *testing.T) {
	a, _ := newAdminTestApp(adminTestAdmin)
	rr := adminGet(a, a.handleAdminOverview, "/x", adminTestSecret, adminTestAdmin, nil)
	var ov map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &ov)
	inst, _ := ov["installations"].(map[string]any)
	subs, _ := ov["subscriptions"].(map[string]any)
	for _, k := range []string{"total", "ready", "configuring", "degraded", "disconnected", "suspended"} {
		if _, ok := inst[k]; !ok {
			t.Errorf("installations.%s missing in %s", k, rr.Body.String())
		}
	}
	for _, k := range []string{"total", "trial", "active", "pastDue", "canceled", "suspended"} {
		if _, ok := subs[k]; !ok {
			t.Errorf("subscriptions.%s missing in %s", k, rr.Body.String())
		}
	}
	for _, fake := range []string{"offline", "trialing", "expired"} {
		if _, ok := inst[fake]; ok {
			t.Errorf("installations.%s is not a real status and must not be reported", fake)
		}
		if _, ok := subs[fake]; ok {
			t.Errorf("subscriptions.%s is not a real status and must not be reported", fake)
		}
	}
	if ov["organizations"].(float64) != 3 || ov["users"].(float64) != 5 {
		t.Fatalf("unexpected overview %s", rr.Body.String())
	}

	rr = adminGet(a, a.handleAdminHealth, "/x", adminTestSecret, adminTestAdmin, nil)
	var h map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil || rr.Code != 200 {
		t.Fatalf("health: %d %s", rr.Code, rr.Body.String())
	}
	for _, k := range []string{"backendStatus", "installations", "generatedAt", "backend", "database", "discord", "summary"} {
		if _, ok := h[k]; !ok {
			t.Errorf("health.%s missing", k)
		}
	}
	if strings.Contains(strings.ToLower(rr.Body.String()), "uptimepercent") {
		t.Fatal("no invented uptime percentages")
	}
}

// The backend status the overview and health report is the runtime registry's own
// overall state, or UNKNOWN when there is none - never a made-up value.
func TestAdminBackendStatusComesFromTheRuntimeRegistry(t *testing.T) {
	a, _ := newAdminTestApp(adminTestAdmin)
	if got := a.backendStatus(); got != "UNKNOWN" {
		t.Fatalf("no registry must be UNKNOWN, got %q", got)
	}
	a.HealthRegistry = health.NewRegistry()
	if got := a.backendStatus(); got != "HEALTHY" {
		t.Fatalf("an empty registry is HEALTHY, got %q", got)
	}
	a.HealthRegistry.Set(health.Component{Name: "db", State: health.Unhealthy, Critical: true})
	rr := adminGet(a, a.handleAdminOverview, "/x", adminTestSecret, adminTestAdmin, nil)
	if !strings.Contains(rr.Body.String(), `"backendStatus":"UNHEALTHY"`) {
		t.Fatalf("overview must carry the runtime status: %s", rr.Body.String())
	}
	rr = adminGet(a, a.handleAdminHealth, "/x", adminTestSecret, adminTestAdmin, nil)
	if !strings.Contains(rr.Body.String(), `"backendStatus":"UNHEALTHY"`) || strings.Contains(rr.Body.String(), "message") {
		t.Fatalf("health must carry the runtime status and no free-text messages: %s", rr.Body.String())
	}
}

func TestInstallationHealthMappingMatchesCustomerAPI(t *testing.T) {
	for _, s := range adminrepo.InstallationStatuses {
		if got, want := adminrepo.HealthForStatus(s), installationHealth(s); got != want {
			t.Errorf("%s: admin health %q != customer health %q", s, got, want)
		}
	}
	// Every health value is reachable, and the inverse mapping partitions the statuses.
	seen := 0
	for _, h := range adminrepo.HealthValues {
		seen += len(adminrepo.StatusesForHealth(h))
	}
	if seen != len(adminrepo.InstallationStatuses) {
		t.Fatalf("StatusesForHealth must partition the statuses: %d != %d", seen, len(adminrepo.InstallationStatuses))
	}
}

func TestAdminAPIChannelNameFromCacheOnly(t *testing.T) {
	a, _ := newAdminTestApp(adminTestAdmin)
	a.adminChannelNames = func(id string) string {
		if id == "c-1" {
			return "killfeed"
		}
		return ""
	}
	rr := adminGet(a, a.handleAdminGetInstallation, "/x", adminTestSecret, adminTestAdmin, map[string]string{"installationID": "1"})
	if !strings.Contains(rr.Body.String(), `"channelName":"killfeed"`) {
		t.Fatalf("a cached channel name must be included: %s", rr.Body.String())
	}
	a.adminChannelNames = func(string) string { return "" }
	rr = adminGet(a, a.handleAdminGetInstallation, "/x", adminTestSecret, adminTestAdmin, map[string]string{"installationID": "1"})
	if !strings.Contains(rr.Body.String(), `"channelName":null`) {
		t.Fatalf("an unknown channel name is null (the website falls back to the id), never invented: %s", rr.Body.String())
	}
}

// --- M: secrets ---------------------------------------------------------------------------------------------------

var forbiddenTagWords = []string{"token", "secret", "cipher", "nonce", "password", "credential", "encryption", "databaseurl", "apikey", "keyversion", "providercustomer", "providersubscription"}

// Every field of every admin DTO: none may be named like a secret. (The DTOs are
// built from explicit columns, so a secret cannot be added without adding a field.)
func TestAdminDTOsHaveNoSecretShapedFields(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type, path string)
	walk = func(ty reflect.Type, path string) {
		for ty.Kind() == reflect.Ptr || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name := strings.ToLower(strings.Split(f.Tag.Get("json"), ",")[0] + " " + f.Name)
			for _, bad := range forbiddenTagWords {
				if strings.Contains(name, bad) {
					t.Errorf("%s.%s looks secret-bearing (%q)", path, f.Name, bad)
				}
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	for _, v := range []any{adminrepo.Overview{}, adminrepo.Organization{}, adminrepo.SubscriptionRow{}, adminrepo.SubscriptionInfo{},
		adminrepo.InstallationSummary{}, adminrepo.InstallationDetail{}, adminrepo.HealthSummary{}, adminrepo.HealthInstallation{}, adminHealthResponse{}} {
		walk(reflect.TypeOf(v), reflect.TypeOf(v).Name())
	}
}

// Backstop: if a configured secret ever reaches a response body (here via a
// customer-controlled guild name), the response is withheld.
func TestAdminResponseWithheldWhenItContainsAConfiguredSecret(t *testing.T) {
	a, fake := newAdminTestApp(adminTestAdmin)
	a.Config.DiscordToken = "MTIzNDU2.bot-token-value.abcdef"
	a.Config.NitradoToken = "nitrado-access-token-123456"
	a.Config.CredentialEncryptionKey = "01234567890123456789012345678901"
	a.Config.DatabaseURL = "postgres://champion:dbpassword99@db.internal:5432/champion"
	for _, secret := range []string{a.Config.DiscordToken, a.Config.NitradoToken, a.Config.CredentialEncryptionKey, a.Config.DatabaseURL, "dbpassword99"} {
		fake.guildName = "Guild " + secret
		rr := adminGet(a, a.handleAdminListInstallations, "/x", adminTestSecret, adminTestAdmin, nil)
		if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("a response containing %q must be withheld, got %d %s", secret, rr.Code, rr.Body.String())
		}
	}
	// A short, ordinary value is not treated as a secret.
	fake.guildName = "Guild abc"
	if rr := adminGet(a, a.handleAdminListInstallations, "/x", adminTestSecret, adminTestAdmin, nil); rr.Code != http.StatusOK {
		t.Fatalf("an ordinary name must pass, got %d", rr.Code)
	}
	// The service secret itself.
	fake.guildName = "G " + adminTestSecret
	if rr := adminGet(a, a.handleAdminListInstallations, "/x", adminTestSecret, adminTestAdmin, nil); rr.Code != http.StatusInternalServerError {
		t.Fatalf("the service secret must never be echoed, got %d", rr.Code)
	}
}

// --- audit logging ---------------------------------------------------------------------------------------------------

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}
func (l *logCapture) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

func TestAdminReadsAreLoggedSafely(t *testing.T) {
	var cap logCapture
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&cap, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a, fake := newAdminTestApp(adminTestAdmin)
	fake.guildName = "Some Guild"
	adminGet(a, a.handleAdminGetOrganization, "/api/admin/organizations/12?search=needle", adminTestSecret, adminTestAdmin, map[string]string{"organizationID": "12"})
	adminGet(a, a.handleAdminGetInstallation, "/api/admin/installations/34", adminTestSecret, adminTestAdmin, map[string]string{"installationID": "34"})
	adminGet(a, a.handleAdminListInstallations, "/api/admin/installations?search=needle", adminTestSecret, adminTestAdmin, nil)
	adminGet(a, a.handleAdminOverview, "/x", adminTestSecret, "777000111222333", nil) // denied

	out := cap.String()
	for _, want := range []string{"event=admin_read", "acting_admin_discord_id=" + adminTestAdmin, "organization_id=12", "installation_id=34", "event=admin_denied"} {
		if !strings.Contains(out, want) {
			t.Errorf("log is missing %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{adminTestSecret, "Bearer", "Authorization", "needle", "Some Guild", "items"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log must never contain %q:\n%s", leak, out)
		}
	}
}

// --- routing: read-only, separate from /api/saas ------------------------------------------------------------------

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

func TestAdminAPIRoutesAreReadOnlyAndSeparateFromSaaS(t *testing.T) {
	port := freePort(t)
	srv, err := server.New(&config.Config{Port: port}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := newAdminTestApp(adminTestAdmin)
	a.HTTPServer = srv
	a.registerAdminAPI()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.ListenAndServe(ctx) }()

	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 3 * time.Second}
	waitUp := func() {
		for i := 0; i < 50; i++ {
			if resp, err := client.Get(base + "/live"); err == nil {
				resp.Body.Close()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("server did not start")
	}
	waitUp()
	do := func(method, path string, headers map[string]string) int {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(""))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	authed := map[string]string{"Authorization": "Bearer " + adminTestSecret, actingUserHeader: adminTestAdmin}

	for _, p := range []string{"/api/admin/overview", "/api/admin/organizations", "/api/admin/organizations/1", "/api/admin/subscriptions", "/api/admin/installations", "/api/admin/installations/1", "/api/admin/health"} {
		if code := do(http.MethodGet, p, authed); code != http.StatusOK {
			t.Errorf("GET %s: expected 200, got %d", p, code)
		}
		if code := do(http.MethodGet, p, nil); code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s: expected 401, got %d", p, code)
		}
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if code := do(m, p, authed); code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: Phase 1 is read-only, expected 405, got %d", m, p, code)
			}
		}
	}
	// The admin surface is not reachable through the tenant prefix.
	if code := do(http.MethodGet, "/api/saas/admin/overview", authed); code != http.StatusNotFound {
		t.Errorf("/api/saas must not serve admin data, got %d", code)
	}
}

// fakeRouteStore is a minimal routing.RouteStore for performanceSnapshot's
// route-cache-stats test - it always resolves, so a lookup is guaranteed to
// populate the resolver's hit/miss counters deterministically.
type fakeRouteStore struct{}

func (fakeRouteStore) ResolveChannel(context.Context, int64, int64, string) (string, bool, error) {
	return "chan-1", true, nil
}

// TestPerformanceSnapshotNilSafe proves the Champion Performance Phase 1
// admin snapshot never panics when DB/ChannelRoutes are unset (e.g. a
// partially-initialized App in a test harness, or a route not yet reached
// during startup) - it must degrade to zero values, never crash the whole
// /api/admin/health response.
func TestPerformanceSnapshotNilSafe(t *testing.T) {
	a := &App{}
	got := a.performanceSnapshot()
	want := adminPerformance{}
	if got != want {
		t.Fatalf("expected a zero-value snapshot with no DB/ChannelRoutes, got %+v", got)
	}
}

// TestPerformanceSnapshotReflectsRouteCacheCounters proves a real resolver's
// hit/miss counters flow through to the snapshot correctly.
func TestPerformanceSnapshotReflectsRouteCacheCounters(t *testing.T) {
	a := &App{ChannelRoutes: routing.NewResolver(fakeRouteStore{}, time.Minute)}
	ctx := context.Background()
	if _, _, err := a.ChannelRoutes.Resolve(ctx, 1, 1, routing.RouteKillfeed); err != nil { // miss
		t.Fatal(err)
	}
	if _, _, err := a.ChannelRoutes.Resolve(ctx, 1, 1, routing.RouteKillfeed); err != nil { // hit
		t.Fatal(err)
	}
	got := a.performanceSnapshot()
	if got.Routing.CacheHits != 1 || got.Routing.CacheMisses != 1 || got.Routing.CacheEntries != 1 {
		t.Fatalf("got %+v", got.Routing)
	}
	if got.Routing.CacheHitRate != 0.5 {
		t.Fatalf("expected a 50%% hit rate, got %v", got.Routing.CacheHitRate)
	}
}
