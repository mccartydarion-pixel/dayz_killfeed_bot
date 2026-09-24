//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type embedWorld struct {
	t       *testing.T
	a       *App
	a1, b1  installationFixture
	admin   string // ADMIN member of org A
	member  string // MEMBER of org A
	outside string // synced user with no organization
}

func newEmbedWorld(t *testing.T) *embedWorld {
	t.Helper()
	a, verifier := saasIntegrationApp(t)
	a.EmbedTemplates = embedtemplates.NewService(repository.NewEmbedTemplateRepository(a.DB.Pool))
	a.EmbedActivations = repository.NewEmbedTemplateRepository(a.DB.Pool)
	w := &embedWorld{t: t, a: a}
	w.a1 = buildInstallationFixture(t, a, verifier)
	w.b1 = buildInstallationFixture(t, a, verifier)
	suffix := time.Now().UnixNano()
	adm := syncUser(t, a, fmt.Sprintf("emb-admin-%d", suffix), "Org Admin")
	mem := syncUser(t, a, fmt.Sprintf("emb-member-%d", suffix), "Org Member")
	out := syncUser(t, a, fmt.Sprintf("emb-outside-%d", suffix), "Outside")
	for user, role := range map[string]string{adm.DiscordUserID: "ADMIN", mem.DiscordUserID: "MEMBER"} {
		if _, err := a.DB.Pool.Exec(context.Background(), `INSERT INTO organization_members(organization_id, user_id, role) VALUES($1,$2,$3)`, w.a1.OrgID, mustAppUserID(t, a, user), role); err != nil {
			t.Fatal(err)
		}
	}
	w.admin, w.member, w.outside = adm.DiscordUserID, mem.DiscordUserID, out.DiscordUserID
	return w
}

func goodTemplate(title string) map[string]any {
	return map[string]any{
		"routeKey": "KILLFEED", "enabled": true, "color": "#d4af37",
		"title":       map[string]any{"enabled": true, "template": title},
		"description": map[string]any{"enabled": true, "template": "A {{ weapon }} kill from {{distance}} away."},
		"author":      map[string]any{"enabled": false, "name": "Champion", "iconUrl": ""},
		"thumbnail":   map[string]any{"enabled": false, "url": ""},
		"image":       map[string]any{"enabled": false, "url": ""},
		"footer":      map[string]any{"enabled": true, "text": "Champion Killfeed", "iconUrl": ""},
		"timestamp":   true,
		"fields": []map[string]any{
			{"key": "weapon", "label": "Weapon", "enabled": true, "template": "{{weapon}}", "inline": true, "order": 1},
			{"key": "killer", "label": "Killer", "enabled": true, "template": "{{killer}}", "inline": true, "order": 0},
		},
	}
}

// call invokes a handler exactly as the router would (path values set), as `actor`.
func (w *embedWorld) call(h http.HandlerFunc, method string, org, inst int64, route string, actor string, body any) *httptest.ResponseRecorder {
	var raw []byte
	switch b := body.(type) {
	case nil:
	case []byte:
		raw = b
	default:
		raw, _ = json.Marshal(b)
	}
	req := httptest.NewRequest(method, "/x", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer test-secret")
	if actor != "" {
		req.Header.Set(actingUserHeader, actor)
	}
	req.SetPathValue("organizationID", strconv.FormatInt(org, 10))
	req.SetPathValue("installationID", strconv.FormatInt(inst, 10))
	if route != "" {
		req.SetPathValue("routeKey", route)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func (w *embedWorld) put(org, inst int64, route, actor string, body any) *httptest.ResponseRecorder {
	return w.call(w.a.handlePutEmbedTemplate, http.MethodPut, org, inst, route, actor, body)
}
func (w *embedWorld) get(org, inst int64, route, actor string) *httptest.ResponseRecorder {
	return w.call(w.a.handleGetEmbedTemplate, http.MethodGet, org, inst, route, actor, nil)
}
func (w *embedWorld) list(org, inst int64, actor string) *httptest.ResponseRecorder {
	return w.call(w.a.handleListEmbedTemplates, http.MethodGet, org, inst, "", actor, nil)
}
func (w *embedWorld) del(org, inst int64, route, actor string) *httptest.ResponseRecorder {
	return w.call(w.a.handleDeleteEmbedTemplate, http.MethodDelete, org, inst, route, actor, nil)
}

// activate selects Champion Default ("DEFAULT") or the saved Custom Embed ("CUSTOM") for a route
// through the real activation endpoint.
func (w *embedWorld) activate(org, inst int64, route, actor, mode string) *httptest.ResponseRecorder {
	return w.call(w.a.handlePutEmbedActivation, http.MethodPut, org, inst, route, actor, map[string]string{"mode": mode})
}

// --- authorization ---------------------------------------------------------------------------------

func TestEmbedTemplateAuthentication(t *testing.T) {
	w := newEmbedWorld(t)
	// Service auth is checked first.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.SetPathValue("organizationID", "1")
	req.SetPathValue("installationID", "1")
	rr := httptest.NewRecorder()
	w.a.handleListEmbedTemplates(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no service auth must be 401, got %d", rr.Code)
	}
	// An acting user is required, and must have synced.
	if rr := w.list(w.a1.OrgID, w.a1.InstallationID, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no acting user must be 401, got %d", rr.Code)
	}
	if rr := w.list(w.a1.OrgID, w.a1.InstallationID, "never-synced-user"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("an unsynced user must be 401, got %d", rr.Code)
	}
	if rr := w.call(w.a.handleListEmbedTemplates, http.MethodGet, 0, 0, "", w.a1.OwnerDiscordID, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("a bad organization id must be 400, got %d", rr.Code)
	}
}

func TestEmbedTemplateRolePolicy(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst := w.a1.OrgID, w.a1.InstallationID

	// OWNER and ADMIN can write.
	if rr := w.put(org, inst, "KILLFEED", w.a1.OwnerDiscordID, goodTemplate("owner {{killer}}")); rr.Code != http.StatusOK {
		t.Fatalf("OWNER must be able to save: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.put(org, inst, "HITFEED", w.admin, map[string]any{"enabled": true, "color": "#123456", "title": map[string]any{"enabled": true, "template": "hit {{victim}}"}}); rr.Code != http.StatusOK {
		t.Fatalf("ADMIN must be able to save: %d %s", rr.Code, rr.Body.String())
	}
	// MEMBER: read-only, matching the other installation read endpoints.
	if rr := w.get(org, inst, "KILLFEED", w.member); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"customized":true`) {
		t.Fatalf("MEMBER may read: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.list(org, inst, w.member); rr.Code != http.StatusOK {
		t.Fatalf("MEMBER may list: %d", rr.Code)
	}
	if rr := w.put(org, inst, "KILLFEED", w.member, goodTemplate("member edit")); rr.Code != http.StatusForbidden {
		t.Fatalf("MEMBER must not save, got %d", rr.Code)
	}
	if rr := w.del(org, inst, "KILLFEED", w.member); rr.Code != http.StatusForbidden {
		t.Fatalf("MEMBER must not reset, got %d", rr.Code)
	}
	// A forbidden write changed nothing.
	if rr := w.get(org, inst, "KILLFEED", w.a1.OwnerDiscordID); !strings.Contains(rr.Body.String(), "owner {{killer}}") {
		t.Fatalf("a rejected write must not change the template: %s", rr.Body.String())
	}
	// A user with no membership at all: nothing.
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"list": w.list(org, inst, w.outside), "get": w.get(org, inst, "KILLFEED", w.outside),
		"put": w.put(org, inst, "KILLFEED", w.outside, goodTemplate("x")), "delete": w.del(org, inst, "KILLFEED", w.outside),
	} {
		if rr.Code != http.StatusForbidden {
			t.Errorf("a non-member must get 403 on %s, got %d", name, rr.Code)
		}
	}
}

// --- tenant isolation ----------------------------------------------------------------------------------

func TestEmbedTemplateTenantIsolation(t *testing.T) {
	w := newEmbedWorld(t)
	orgA, instA := w.a1.OrgID, w.a1.InstallationID
	orgB, instB := w.b1.OrgID, w.b1.InstallationID
	if rr := w.put(orgA, instA, "KILLFEED", w.a1.OwnerDiscordID, goodTemplate("A-private-title")); rr.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.put(orgB, instB, "KILLFEED", w.b1.OwnerDiscordID, goodTemplate("B-title")); rr.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rr.Code, rr.Body.String())
	}

	// B's owner using org A's path: not a member of org A.
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"list": w.list(orgA, instA, w.b1.OwnerDiscordID), "get": w.get(orgA, instA, "KILLFEED", w.b1.OwnerDiscordID),
		"put": w.put(orgA, instA, "KILLFEED", w.b1.OwnerDiscordID, goodTemplate("HIJACK")), "delete": w.del(orgA, instA, "KILLFEED", w.b1.OwnerDiscordID),
	} {
		if rr.Code != http.StatusForbidden || strings.Contains(rr.Body.String(), "A-private-title") {
			t.Errorf("org B on org A's path (%s): expected 403 without data, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// B's owner using THEIR OWN org path but org A's installation id: the installation is
	// not theirs - the same safe 404 as an unknown one, and nothing leaks or changes.
	for name, rr := range map[string]*httptest.ResponseRecorder{
		"list": w.list(orgB, instA, w.b1.OwnerDiscordID), "get": w.get(orgB, instA, "KILLFEED", w.b1.OwnerDiscordID),
		"put": w.put(orgB, instA, "KILLFEED", w.b1.OwnerDiscordID, goodTemplate("HIJACK")), "delete": w.del(orgB, instA, "KILLFEED", w.b1.OwnerDiscordID),
	} {
		if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "A-private-title") {
			t.Errorf("foreign installation id (%s): expected 404 without data, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if rr := w.get(orgB, 987654321, "KILLFEED", w.b1.OwnerDiscordID); rr.Code != http.StatusNotFound {
		t.Errorf("an unknown installation id must be the same 404, got %d", rr.Code)
	}
	// Org A's template is intact, and B's is B's.
	if rr := w.get(orgA, instA, "KILLFEED", w.a1.OwnerDiscordID); !strings.Contains(rr.Body.String(), "A-private-title") {
		t.Fatalf("org A's template must be untouched: %s", rr.Body.String())
	}
	if rr := w.get(orgB, instB, "KILLFEED", w.b1.OwnerDiscordID); !strings.Contains(rr.Body.String(), "B-title") || strings.Contains(rr.Body.String(), "A-private") {
		t.Fatalf("org B sees its own template only: %s", rr.Body.String())
	}
}

// Two installations of one organization (and guild): styling one leaves the other alone.
func TestEmbedTemplateMultiInstallationIsolation(t *testing.T) {
	w := newEmbedWorld(t)
	org := w.a1.OrgID
	var conn, guild int64
	ctx := context.Background()
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT discord_guild_connection_id FROM installations WHERE id=$1`, w.a1.InstallationID).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT guild_id FROM discord_guild_connections WHERE id=$1`, conn).Scan(&guild); err != nil {
		t.Fatal(err)
	}
	var server, inst2 int64
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, organization_id) VALUES($1,'NITRADO',$2,'DAYZ','PLAYSTATION','S2','ACTIVE',$3) RETURNING id`,
		guild, fmt.Sprintf("svc-%d", time.Now().UnixNano()), org).Scan(&server); err != nil {
		t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`, org, conn, server).Scan(&inst2); err != nil {
		t.Fatal(err)
	}
	owner := w.a1.OwnerDiscordID
	w.put(org, w.a1.InstallationID, "KILLFEED", owner, goodTemplate("server-one"))
	w.put(org, inst2, "KILLFEED", owner, goodTemplate("server-two"))
	w.put(org, w.a1.InstallationID, "KILLFEED", owner, goodTemplate("server-one-v2"))
	if body := w.get(org, inst2, "KILLFEED", owner).Body.String(); !strings.Contains(body, "server-two") || strings.Contains(body, "server-one") {
		t.Fatalf("installation 2 must be unaffected: %s", body)
	}
	if rr := w.del(org, w.a1.InstallationID, "KILLFEED", owner); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if body := w.get(org, inst2, "KILLFEED", owner).Body.String(); !strings.Contains(body, `"customized":true`) || !strings.Contains(body, "server-two") {
		t.Fatalf("resetting installation 1 must not reset installation 2: %s", body)
	}
}

// --- behavior ---------------------------------------------------------------------------------------------

type embedResp struct {
	RouteKey         string                 `json:"routeKey"`
	Customized       bool                   `json:"customized"`
	Template         *embedtemplates.Config `json:"template"`
	Variables        []string               `json:"variables"`
	CreatedAt        *string                `json:"createdAt"`
	UpdatedAt        *string                `json:"updatedAt"`
	RuntimeRendering string                 `json:"runtimeRendering"`
}

func TestEmbedTemplateSaveGetListResetLifecycle(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst, owner := w.a1.OrgID, w.a1.InstallationID, w.a1.OwnerDiscordID

	// Not customized: the backend does not fabricate a default.
	rr := w.get(org, inst, "KILLFEED", owner)
	got := decodeBody[embedResp](t, rr)
	if rr.Code != 200 || got.Customized || got.Template != nil || got.RouteKey != "KILLFEED" || got.RuntimeRendering != "NOT_ENABLED" || len(got.Variables) == 0 {
		t.Fatalf("expected the not-customized state: %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"template":null`) {
		t.Fatalf("template must be an explicit null: %s", rr.Body.String())
	}
	var empty struct {
		Templates        []json.RawMessage   `json:"templates"`
		CustomizedRoutes []string            `json:"customizedRoutes"`
		Variables        map[string][]string `json:"variables"`
		Limits           map[string]int      `json:"limits"`
		RuntimeRendering string              `json:"runtimeRendering"`
	}
	rr = w.list(org, inst, owner)
	empty = decodeBody[struct {
		Templates        []json.RawMessage   `json:"templates"`
		CustomizedRoutes []string            `json:"customizedRoutes"`
		Variables        map[string][]string `json:"variables"`
		Limits           map[string]int      `json:"limits"`
		RuntimeRendering string              `json:"runtimeRendering"`
	}](t, rr)
	if empty.Templates == nil || len(empty.Templates) != 0 || len(empty.CustomizedRoutes) != 0 || len(empty.Variables) == 0 || empty.Limits["title"] != 256 || empty.RuntimeRendering != "NOT_ENABLED" {
		t.Fatalf("an empty list is empty arrays plus the variables and limits: %s", rr.Body.String())
	}

	// Save: the response is the NORMALIZED stored config.
	rr = w.put(org, inst, "KILLFEED", owner, goodTemplate("v1 {{killer}}"))
	saved := decodeBody[embedResp](t, rr)
	if rr.Code != 200 || !saved.Customized || saved.Template == nil {
		t.Fatalf("save: %d %s", rr.Code, rr.Body.String())
	}
	c := saved.Template
	if c.Color != "#D4AF37" || c.RouteKey != "KILLFEED" || c.Version != 1 || c.Description.Template != "A {{weapon}} kill from {{distance}} away." ||
		len(c.Fields) != 2 || c.Fields[0].Key != "killer" || c.Fields[0].Order != 0 || c.Fields[1].Key != "weapon" || c.Fields[1].Order != 1 {
		t.Fatalf("the response must be the normalized config: %+v", c)
	}
	if saved.CreatedAt == nil || saved.UpdatedAt == nil {
		t.Fatal("timestamps must be reported")
	}

	// Update: same row, created_at kept, updated_at advanced.
	time.Sleep(20 * time.Millisecond)
	updated := decodeBody[embedResp](t, w.put(org, inst, "KILLFEED", owner, goodTemplate("v2 {{victim}}")))
	if updated.Template.Title.Template != "v2 {{victim}}" || *updated.CreatedAt != *saved.CreatedAt || *updated.UpdatedAt == *saved.UpdatedAt {
		t.Fatalf("update must keep created_at and refresh updated_at: %+v vs %+v", updated, saved)
	}

	// Get / list show it; only customized routes are listed.
	if g := decodeBody[embedResp](t, w.get(org, inst, "KILLFEED", owner)); !g.Customized || g.Template.Title.Template != "v2 {{victim}}" {
		t.Fatalf("get after save: %+v", g)
	}
	w.put(org, inst, "ECONOMY", owner, map[string]any{"enabled": true, "color": "#4C9A72", "title": map[string]any{"enabled": true, "template": "{{player}} {{transaction_type}} {{amount}}"}})
	list := decodeBody[struct {
		Templates        []embedResp `json:"templates"`
		CustomizedRoutes []string    `json:"customizedRoutes"`
	}](t, w.list(org, inst, owner))
	if len(list.Templates) != 2 || len(list.CustomizedRoutes) != 2 || list.CustomizedRoutes[0] != "ECONOMY" || list.CustomizedRoutes[1] != "KILLFEED" {
		t.Fatalf("the list holds exactly the customized routes: %+v", list)
	}
	// A route that was never saved is still not customized.
	if g := decodeBody[embedResp](t, w.get(org, inst, "HITFEED", owner)); g.Customized {
		t.Fatal("HITFEED was never saved")
	}

	// Reset: idempotent, and only that route.
	for i := 0; i < 2; i++ {
		rr = w.del(org, inst, "KILLFEED", owner)
		d := decodeBody[embedResp](t, rr)
		if rr.Code != 200 || d.Customized || d.Template != nil {
			t.Fatalf("delete #%d must report the not-customized state: %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	if g := decodeBody[embedResp](t, w.get(org, inst, "ECONOMY", owner)); !g.Customized {
		t.Fatal("resetting KILLFEED must not touch ECONOMY")
	}
}

func TestEmbedTemplateHTTPValidation(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst, owner := w.a1.OrgID, w.a1.InstallationID, w.a1.OwnerDiscordID
	mutate := func(f func(map[string]any)) map[string]any {
		m := goodTemplate("t {{killer}}")
		f(m)
		return m
	}
	cases := []struct {
		name   string
		route  string
		body   any
		want   int
		expect string
	}{
		{"invalid route", "NOPE", goodTemplate("x"), 400, "invalid routeKey"},
		{"lowercase route", "killfeed", goodTemplate("x"), 400, "invalid routeKey"},
		{"invalid color", "KILLFEED", mutate(func(m map[string]any) { m["color"] = "red" }), 400, "color"},
		{"overflowing color", "KILLFEED", mutate(func(m map[string]any) { m["color"] = "16777216" }), 400, "color"},
		{"javascript url", "KILLFEED", mutate(func(m map[string]any) { m["thumbnail"] = map[string]any{"enabled": true, "url": "javascript:alert(1)"} }), 400, "thumbnail.url"},
		{"data url", "KILLFEED", mutate(func(m map[string]any) {
			m["image"] = map[string]any{"enabled": true, "url": "data:image/png;base64,AAAA"}
		}), 400, "image.url"},
		{"file url", "KILLFEED", mutate(func(m map[string]any) {
			m["author"] = map[string]any{"enabled": true, "name": "a", "iconUrl": "file:///etc/passwd"}
		}), 400, "author.iconUrl"},
		{"title too long", "KILLFEED", mutate(func(m map[string]any) {
			m["title"] = map[string]any{"enabled": true, "template": strings.Repeat("x", 257)}
		}), 400, "title.template: too long"},
		{"unknown variable", "KILLFEED", mutate(func(m map[string]any) { m["title"] = map[string]any{"enabled": true, "template": "{{nope}}"} }), 400, "unknown variable"},
		{"wrong route variable", "ECONOMY", mutate(func(m map[string]any) {}), 400, "not available for ECONOMY"},
		{"malformed placeholder", "KILLFEED", mutate(func(m map[string]any) { m["title"] = map[string]any{"enabled": true, "template": "{{killer}"} }), 400, "braces are reserved"},
		{"code-like template", "KILLFEED", mutate(func(m map[string]any) {
			m["title"] = map[string]any{"enabled": true, "template": "{{ .Killer | printf \"%s\" }}"}
		}), 400, "braces are reserved"},
		{"route mismatch", "HITFEED", goodTemplate("x"), 400, "does not match"},
		{"unknown field is rejected, not stored", "KILLFEED", mutate(func(m map[string]any) { m["webhookUrl"] = "https://evil.example" }), 400, "invalid embed template payload"},
		{"unknown nested key", "KILLFEED", mutate(func(m map[string]any) {
			m["footer"] = map[string]any{"enabled": true, "text": "x", "iconUrl": "", "extra": 1}
		}), 400, "invalid embed template payload"},
		{"wrong type", "KILLFEED", mutate(func(m map[string]any) { m["timestamp"] = "yes" }), 400, "invalid embed template payload"},
		{"not json", "KILLFEED", []byte("{nope"), 400, "invalid embed template payload"},
		{"two json values", "KILLFEED", []byte(`{} {}`), 400, "invalid embed template payload"},
		{"empty body", "KILLFEED", []byte(``), 400, "invalid embed template payload"},
		{"json array", "KILLFEED", []byte(`[]`), 400, "invalid embed template payload"},
	}
	for _, c := range cases {
		rr := w.put(org, inst, c.route, owner, c.body)
		if rr.Code != c.want || !strings.Contains(rr.Body.String(), c.expect) {
			t.Errorf("%s: expected %d containing %q, got %d %s", c.name, c.want, c.expect, rr.Code, rr.Body.String())
		}
	}
	// Nothing invalid was stored.
	if g := decodeBody[embedResp](t, w.get(org, inst, "KILLFEED", owner)); g.Customized {
		t.Fatal("no rejected request may create a template")
	}

	// Fields: 26 fields, and an over-long label/value.
	many := goodTemplate("t")
	var fields []map[string]any
	for i := 0; i < 26; i++ {
		fields = append(fields, map[string]any{"key": fmt.Sprintf("f%d", i), "label": "L", "enabled": true, "template": "T", "inline": false, "order": i})
	}
	many["fields"] = fields
	if rr := w.put(org, inst, "KILLFEED", owner, many); rr.Code != 400 || !strings.Contains(rr.Body.String(), "too many fields") {
		t.Errorf("26 fields: %d %s", rr.Code, rr.Body.String())
	}
	long := goodTemplate("t")
	long["fields"] = []map[string]any{{"key": "a", "label": strings.Repeat("l", 257), "enabled": true, "template": strings.Repeat("v", 1025), "inline": false, "order": 0}}
	if rr := w.put(org, inst, "KILLFEED", owner, long); rr.Code != 400 || !strings.Contains(rr.Body.String(), "fields[0].label: too long") || !strings.Contains(rr.Body.String(), "fields[0].template: too long") {
		t.Errorf("long label/value: %d %s", rr.Code, rr.Body.String())
	}
	tooLongFooter := goodTemplate("t")
	tooLongFooter["footer"] = map[string]any{"enabled": true, "text": strings.Repeat("f", 2049), "iconUrl": ""}
	if rr := w.put(org, inst, "KILLFEED", owner, tooLongFooter); rr.Code != 400 || !strings.Contains(rr.Body.String(), "footer.text: too long") {
		t.Errorf("long footer: %d %s", rr.Code, rr.Body.String())
	}

	// The error message never reflects free-form input.
	leak := goodTemplate("t")
	leak["color"] = "SUPER-SECRET-INPUT-9876"
	leak["thumbnail"] = map[string]any{"enabled": true, "url": "javascript:SUPER-SECRET-INPUT-9876"}
	if rr := w.put(org, inst, "KILLFEED", owner, leak); strings.Contains(rr.Body.String(), "SUPER-SECRET-INPUT-9876") {
		t.Errorf("errors must not echo the input: %s", rr.Body.String())
	}
}

// The request body is bounded: a multi-megabyte payload is refused without being read
// into memory or stored.
func TestEmbedTemplateRequestSizeBound(t *testing.T) {
	w := newEmbedWorld(t)
	org, inst, owner := w.a1.OrgID, w.a1.InstallationID, w.a1.OwnerDiscordID
	huge := goodTemplate("t")
	huge["description"] = map[string]any{"enabled": true, "template": strings.Repeat("x", 4<<20)}
	rr := w.put(org, inst, "KILLFEED", owner, huge)
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "PAYLOAD_TOO_LARGE") {
		t.Fatalf("a 4 MiB body must be refused with 413, got %d", rr.Code)
	}
	// Just over the bound with valid JSON is also refused; a normal maximal template is fine.
	pad := []byte(`{"pad":"` + strings.Repeat("x", maxEmbedTemplateBody) + `"}`)
	if rr := w.put(org, inst, "KILLFEED", owner, pad); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the bound must be 413, got %d", rr.Code)
	}
	max := goodTemplate("t")
	max["description"] = map[string]any{"enabled": true, "template": strings.Repeat("d", 3000)}
	max["footer"] = map[string]any{"enabled": true, "text": strings.Repeat("f", 1000), "iconUrl": ""}
	var fields []map[string]any
	for i := 0; i < 25; i++ {
		fields = append(fields, map[string]any{"key": fmt.Sprintf("k%d", i), "label": strings.Repeat("l", 20), "enabled": true, "template": strings.Repeat("v", 40), "inline": true, "order": i})
	}
	max["fields"] = fields
	if rr := w.put(org, inst, "KILLFEED", owner, max); rr.Code != http.StatusOK {
		t.Fatalf("a maximal valid template must fit the bound: %d %s", rr.Code, rr.Body.String())
	}
	if g := decodeBody[embedResp](t, w.get(org, inst, "KILLFEED", owner)); !g.Customized || g.Template.Description.Template != strings.Repeat("d", 3000) {
		t.Fatal("the last valid template must be the stored one (the oversized ones stored nothing)")
	}
}

// Audit events carry ids, never template contents, secrets or headers.
func TestEmbedTemplateAuditLoggingIsSafe(t *testing.T) {
	var buf logCapture
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	w := newEmbedWorld(t)
	org, inst, owner := w.a1.OrgID, w.a1.InstallationID, w.a1.OwnerDiscordID
	w.put(org, inst, "KILLFEED", owner, goodTemplate("a-very-private-title-xyz"))
	w.del(org, inst, "KILLFEED", owner)
	w.put(org, inst, "KILLFEED", w.member, goodTemplate("denied-write")) // forbidden: must not log a save

	out := buf.String()
	uid := strconv.FormatInt(mustAppUserID(t, w.a, owner), 10)
	for _, want := range []string{"event=embed_template_saved", "event=embed_template_deleted", "organization_id=" + strconv.FormatInt(org, 10),
		"installation_id=" + strconv.FormatInt(inst, 10), "route_key=KILLFEED", "acting_user_id=" + uid} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log is missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "event=embed_template_saved") != 1 {
		t.Errorf("only the successful save is logged:\n%s", out)
	}
	for _, leak := range []string{"a-very-private-title-xyz", "denied-write", "test-secret", "Bearer", "Authorization", "description"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log must not contain %q:\n%s", leak, out)
		}
	}
}
