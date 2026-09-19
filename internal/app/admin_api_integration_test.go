//go:build integration

package app

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
)

const adminFounderID = "900000000000000777"

// adminWorld: two unrelated customer organizations built through the real SaaS
// handlers, a founder on the allowlist, and the real cross-tenant read model.
type adminWorld struct {
	t      *testing.T
	a      *App
	a1, b1 installationFixture
}

func newAdminWorld(t *testing.T) *adminWorld {
	t.Helper()
	a, verifier := saasIntegrationApp(t)
	a.adminSaaS = adminrepo.New(a.DB.Pool)
	a.Config.AdminDiscordIDs = []string{adminFounderID}
	w := &adminWorld{t: t, a: a}
	w.a1 = buildInstallationFixture(t, a, verifier)
	w.b1 = buildInstallationFixture(t, a, verifier)
	return w
}

func (w *adminWorld) get(h adminHandler, target, acting string, pv map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	if acting != "" {
		req.Header.Set(actingUserHeader, acting)
	}
	for k, v := range pv {
		req.SetPathValue(k, v)
	}
	rr := httptest.NewRecorder()
	w.a.adminRoute(h)(rr, req)
	return rr
}

func (w *adminWorld) allRoutes() map[string]func(acting string) *httptest.ResponseRecorder {
	a := w.a
	org := strconv.FormatInt(w.a1.OrgID, 10)
	inst := strconv.FormatInt(w.a1.InstallationID, 10)
	return map[string]func(string) *httptest.ResponseRecorder{
		"overview": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminOverview, "/api/admin/overview", u, nil)
		},
		"organizations": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminListOrganizations, "/api/admin/organizations", u, nil)
		},
		"organization": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminGetOrganization, "/api/admin/organizations/"+org, u, map[string]string{"organizationID": org})
		},
		"subscriptions": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminListSubscriptions, "/api/admin/subscriptions", u, nil)
		},
		"installations": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminListInstallations, "/api/admin/installations", u, nil)
		},
		"installation": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminGetInstallation, "/api/admin/installations/"+inst, u, map[string]string{"installationID": inst})
		},
		"health": func(u string) *httptest.ResponseRecorder {
			return w.get(a.handleAdminHealth, "/api/admin/health", u, nil)
		},
	}
}

// Platform admin: allowed. Organization OWNER, ADMIN and MEMBER of the very
// organization being read, and the other tenant's owner: all 403.
func TestAdminAPIOnlyThePlatformAdminCrossesTenants(t *testing.T) {
	w := newAdminWorld(t)
	suffix := time.Now().UnixNano()
	adminMember := syncUser(t, w.a, fmt.Sprintf("api-adm-%d", suffix), "Org Admin")
	plainMember := syncUser(t, w.a, fmt.Sprintf("api-mem-%d", suffix), "Org Member")
	for user, role := range map[string]string{adminMember.DiscordUserID: "ADMIN", plainMember.DiscordUserID: "MEMBER"} {
		if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, w.a1.OrgID, mustAppUserID(t, w.a, user), role); err != nil {
			t.Fatal(err)
		}
	}
	for name, call := range w.allRoutes() {
		for who, id := range map[string]string{
			"organization OWNER":       w.a1.OwnerDiscordID,
			"organization ADMIN":       adminMember.DiscordUserID,
			"organization MEMBER":      plainMember.DiscordUserID,
			"the other tenant's OWNER": w.b1.OwnerDiscordID,
			"an unknown user":          "555000111222333",
		} {
			if rr := call(id); rr.Code != http.StatusForbidden {
				t.Errorf("%s as %s: expected 403, got %d", name, who, rr.Code)
			}
		}
		if rr := call(adminFounderID); rr.Code != http.StatusOK {
			t.Errorf("%s as the platform admin: expected 200, got %d (%s)", name, rr.Code, rr.Body.String())
		}
	}
}

// The founder sees both tenants; each tenant still sees only itself through the
// customer API, which is unchanged.
func TestAdminReadsBothTenantsWhileCustomerIsolationHolds(t *testing.T) {
	w := newAdminWorld(t)
	page := func(q string) struct {
		Items []adminrepo.Organization `json:"items"`
	} {
		rr := w.get(w.a.handleAdminListOrganizations, "/api/admin/organizations"+q, adminFounderID, nil)
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		return decodeBody[struct {
			Items []adminrepo.Organization `json:"items"`
		}](t, rr)
	}
	ids := map[int64]bool{}
	for _, o := range page("?limit=100").Items {
		ids[o.ID] = true
	}
	if !ids[w.a1.OrgID] || !ids[w.b1.OrgID] {
		t.Fatalf("the platform admin must see both tenants, got %v", ids)
	}
	// Search finds the right tenant by its Discord guild name.
	found := page("?limit=100&search=Fixture%20Guild").Items
	if len(found) < 2 {
		t.Fatalf("both fixture guilds share the name 'Fixture Guild', got %d", len(found))
	}

	// Customer API: tenant A's owner cannot read tenant B, through any /api/saas read.
	orgB := strconv.FormatInt(w.b1.OrgID, 10)
	instB := strconv.FormatInt(w.b1.InstallationID, 10)
	for name, h := range map[string]http.HandlerFunc{"organization": w.a.handleGetOrganization, "dashboard": w.a.handleDashboard} {
		req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), w.a1.OwnerDiscordID), map[string]string{"organizationID": orgB})
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("customer %s: tenant A reading tenant B must stay 403, got %d", name, rr.Code)
		}
	}
	req := withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), w.a1.OwnerDiscordID), map[string]string{"organizationID": orgB, "installationID": instB})
	rr := httptest.NewRecorder()
	w.a.handleGetInstallation(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("customer installation read across tenants must stay 403, got %d", rr.Code)
	}
	// ...and the founder is not a customer: the platform allowlist grants no /api/saas access.
	req = withPathValues(withActingUser(saasRequest(http.MethodGet, "/x", nil), adminFounderID), map[string]string{"organizationID": orgB})
	rr = httptest.NewRecorder()
	w.a.handleGetOrganization(rr, req)
	if rr.Code == http.StatusOK {
		t.Errorf("the platform admin allowlist must not open the customer API, got %d", rr.Code)
	}
}

// Real rows from real handlers: the detail endpoints carry the right tenant's data.
func TestAdminDetailEndpointsReturnTheRightTenant(t *testing.T) {
	w := newAdminWorld(t)
	rr := w.get(w.a.handleAdminGetOrganization, "/x", adminFounderID, map[string]string{"organizationID": strconv.FormatInt(w.a1.OrgID, 10)})
	org := decodeBody[adminrepo.Organization](t, rr)
	if org.ID != w.a1.OrgID || org.OwnerUser == nil || org.OwnerUser.DiscordID != w.a1.OwnerDiscordID || len(org.Members) != 1 || org.Members[0].Role != "OWNER" ||
		len(org.Installations) != 1 || org.Installations[0].ID != w.a1.InstallationID || org.Installations[0].Discord.GuildID != w.a1.DiscordGuildID {
		t.Fatalf("organization A detail: %+v", org)
	}
	if org.Subscription == nil || org.Subscription.Status == "" {
		t.Fatalf("a new organization has a trial subscription: %+v", org.Subscription)
	}
	rr = w.get(w.a.handleAdminGetInstallation, "/x", adminFounderID, map[string]string{"installationID": strconv.FormatInt(w.b1.InstallationID, 10)})
	inst := decodeBody[adminrepo.InstallationDetail](t, rr)
	if inst.OrganizationID != w.b1.OrgID || inst.Discord.GuildID != w.b1.DiscordGuildID || inst.Status != "NOT_STARTED" || inst.Health != "SETTING_UP" {
		t.Fatalf("installation B detail: %+v", inst)
	}
	rr = w.get(w.a.handleAdminGetInstallation, "/x", adminFounderID, map[string]string{"installationID": "999999999"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown installation: expected 404, got %d", rr.Code)
	}
}

// Every admin endpoint, against rows that hold secret-shaped values, never
// serialises them: Nitrado credential envelope, configured tokens, DB URL.
func TestAdminResponsesNeverContainSecrets(t *testing.T) {
	w := newAdminWorld(t)
	ctx := context.Background()
	cfg := w.a.Config
	cfg.DiscordToken = "DISCORD-BOT-TOKEN-abcdef123456"
	cfg.NitradoToken = "NITRADO-ACCESS-TOKEN-abcdef123456"
	cfg.CredentialEncryptionKey = "ENCRYPTION-KEY-abcdef1234567890"
	cfg.DatabaseURL = "postgres://champion:S3cretDbPassword@db.internal:5432/champion"

	cipher := []byte("NITRADO-ENCRYPTED-CREDENTIAL-BLOB-" + strconv.FormatInt(time.Now().UnixNano(), 10))
	nonce := []byte("NONCE-NONCE-NONCE")
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO nitrado_connections(guild_id, organization_id, credential_ciphertext, credential_nonce, credential_key_version, status) VALUES(NULL,$1,$2,$3,42,'ACTIVE')`, w.a1.OrgID, cipher, nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE subscriptions SET provider='stripe', provider_customer_id='cus_SECRET_CUSTOMER_ID', provider_subscription_id='sub_SECRET_SUB_ID' WHERE organization_id=$1`, w.a1.OrgID); err != nil {
		t.Fatal(err)
	}

	forbidden := []string{
		string(cipher), string(nonce), base64.StdEncoding.EncodeToString(cipher), hex.EncodeToString(cipher), "\\x" + hex.EncodeToString(cipher),
		"credential_ciphertext", "credential_nonce", "keyVersion", "key_version",
		cfg.DiscordToken, cfg.NitradoToken, cfg.CredentialEncryptionKey, cfg.DatabaseURL, "S3cretDbPassword", "DATABASE_URL",
		"test-secret", "cus_SECRET_CUSTOMER_ID", "sub_SECRET_SUB_ID", "stripe",
	}
	for name, call := range w.allRoutes() {
		rr := call(adminFounderID)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s leaked %q", name, bad)
			}
		}
		if !json.Valid(rr.Body.Bytes()) {
			t.Errorf("%s: invalid JSON", name)
		}
	}
	// The Nitrado link is reported as connected (status only) in the installation detail.
	rr := w.allRoutes()["installation"](adminFounderID)
	d := decodeBody[adminrepo.InstallationDetail](t, rr)
	if d.NitradoConnection == nil || !d.NitradoConnection.Connected || d.NitradoConnection.Status != "ACTIVE" {
		t.Fatalf("expected the non-secret Nitrado status, got %+v", d.NitradoConnection)
	}

	// And if a customer manages to put a configured secret into a field the API
	// returns (a guild name), the response is withheld rather than sent.
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE discord_guild_connections SET guild_name=$2 WHERE id=$1`, w.a1.ConnectionID, "Guild "+cfg.NitradoToken); err != nil {
		t.Fatal(err)
	}
	rr = w.allRoutes()["installations"](adminFounderID)
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), cfg.NitradoToken) {
		t.Fatalf("a response containing a configured secret must be withheld, got %d", rr.Code)
	}
}

// The API's pagination against the real database: limit is bounded and a cursor walk
// covers every row exactly once.
func TestAdminAPIPaginationOverRealRows(t *testing.T) {
	w := newAdminWorld(t)
	// A third and fourth tenant so there are several rows.
	a, verifier := w.a, saasVerifierOf(t, w)
	_ = buildInstallationFixture(t, a, verifier)
	_ = buildInstallationFixture(t, a, verifier)

	type envelope struct {
		Items      []adminrepo.InstallationSummary `json:"items"`
		NextCursor *string                         `json:"nextCursor"`
		Limit      int                             `json:"limit"`
	}
	seen := map[int64]bool{}
	cursor := ""
	pages := 0
	for {
		q := "?limit=2&search=Fixture%20Guild"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rr := w.get(w.a.handleAdminListInstallations, "/api/admin/installations"+q, adminFounderID, nil)
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body.String())
		}
		env := decodeBody[envelope](t, rr)
		if env.Limit != 2 || len(env.Items) > 2 {
			t.Fatalf("page size must be honoured: %+v", env)
		}
		for _, it := range env.Items {
			if seen[it.ID] {
				t.Fatalf("installation %d appeared twice", it.InstallationID)
			}
			seen[it.ID] = true
		}
		pages++
		if env.NextCursor == nil {
			break
		}
		cursor = *env.NextCursor
		if pages > 200 {
			t.Fatal("pagination did not terminate")
		}
	}
	for _, id := range []int64{w.a1.InstallationID, w.b1.InstallationID} {
		if !seen[id] {
			t.Fatalf("installation %d was skipped by pagination", id)
		}
	}
	// limit=100000 is clamped to 100.
	rr := w.get(w.a.handleAdminListInstallations, "/x?limit=100000", adminFounderID, nil)
	if env := decodeBody[envelope](t, rr); env.Limit != 100 || len(env.Items) > 100 {
		t.Fatalf("limit must be capped at 100: %d items, limit %d", len(env.Items), env.Limit)
	}
}

func saasVerifierOf(t *testing.T, w *adminWorld) *fakeDiscordVerifier {
	t.Helper()
	v, ok := w.a.saasDiscordVerifier.(*fakeDiscordVerifier)
	if !ok {
		t.Fatal("expected the fake Discord verifier")
	}
	return v
}

// The same website-contract check as the unit test, but against REAL rows produced by
// the real SaaS handlers and PostgreSQL (nullable columns really are null here).
func TestAdminRealResponsesMatchTheFounderHubWebsiteContract(t *testing.T) {
	w := newAdminWorld(t)
	get := func(name string) map[string]any {
		rr := w.allRoutes()[name](adminFounderID)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
		return decodeJSON(t, rr.Body.Bytes())
	}
	checkShape(t, "overview", get("overview"), shapeOverview)

	org := get("organization")
	checkShape(t, "organization detail", org, shapeOrganization)
	checkShape(t, "organization detail.subscription", org["subscription"], shapeSubscription)
	checkShape(t, "organization detail.members[0]", first(t, org, "members"), shapeMember)
	checkShape(t, "organization detail.installations[0]", first(t, org, "installations"), shapeInstallation)

	orgs := get("organizations")
	checkShape(t, "organizations page", orgs, shape{"items": "arr", "nextCursor": "str?"})
	checkShape(t, "organizations[0]", first(t, orgs, "items"), shapeOrganization)

	subs := get("subscriptions")
	sub := shape{"organization": "str", "installationCount": "num"}
	for k, v := range shapeSubscription {
		sub[k] = v
	}
	checkShape(t, "subscriptions[0]", first(t, subs, "items"), sub)

	insts := get("installations")
	li := shape{"organization": "str"}
	for k, v := range shapeInstallation {
		li[k] = v
	}
	checkShape(t, "installations[0]", first(t, insts, "items"), li)

	inst := get("installation")
	di := shape{"organization": "str", "subscription": "obj?", "channelRoutes": "arr", "settings": "obj?"}
	for k, v := range shapeInstallation {
		di[k] = v
	}
	checkShape(t, "installation detail", inst, di)

	h := get("health")
	checkShape(t, "health", h, shape{"backendStatus": "str", "installations": "arr"})
}

// Channel routes shown for an installation are exactly its own, with the ownership
// visible through the API (a route on tenant B's installation never shows on A's).
func TestAdminInstallationDetailShowsOnlyItsOwnRoutes(t *testing.T) {
	w := newAdminWorld(t)
	ctx := context.Background()
	for _, r := range []struct {
		inst    int64
		key, ch string
	}{{w.a1.InstallationID, "KILLFEED", "chan-a-kill"}, {w.a1.InstallationID, "BOUNTY", "chan-a-bounty"}, {w.b1.InstallationID, "KILLFEED", "chan-b-kill"}} {
		if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id, managed_by_champion) VALUES($1,$2,$3,FALSE)`, r.inst, r.key, r.ch); err != nil {
			t.Fatal(err)
		}
	}
	w.a.adminChannelNames = func(id string) string {
		if id == "chan-a-kill" {
			return "killfeed"
		}
		return ""
	}
	rr := w.allRoutes()["installation"](adminFounderID)
	d := decodeBody[adminrepo.InstallationDetail](t, rr)
	got := map[string]string{}
	for _, r := range d.ChannelRoutes {
		got[r.RouteKey] = r.ChannelID
	}
	if len(got) != 2 || got["KILLFEED"] != "chan-a-kill" || got["BOUNTY"] != "chan-a-bounty" || strings.Contains(rr.Body.String(), "chan-b-kill") {
		t.Fatalf("installation A must show exactly its own routes: %v (%s)", got, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"channelName":"killfeed"`) || !strings.Contains(rr.Body.String(), `"channelName":null`) {
		t.Fatalf("a cached name is included; an unknown one is null: %s", rr.Body.String())
	}
}
