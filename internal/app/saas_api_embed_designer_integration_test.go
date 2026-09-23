//go:build integration

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Embed Designer V2 over the real handlers and a disposable PostgreSQL. Discord is
// always the in-memory fakeDiscordVerifier: no test can reach a real guild.

func designerDraftBody(title string) map[string]any {
	return map[string]any{
		"template":  goodTemplate(title),
		"variables": map[string]string{"killer": "ChampionPlayer", "victim": "RivalPlayer", "weapon": "M4-A1", "distance": "127.4m"},
	}
}

// routeKillfeed gives installation fx a routed, fully permitted KILLFEED channel.
func (w *embedWorld) routeKillfeed(fx installationFixture, channelID string) {
	w.t.Helper()
	verifier := w.a.saasDiscordVerifier.(*fakeDiscordVerifier)
	seedGuildChannels(verifier, fx.DiscordGuildID, fakeGuildChannel{ID: channelID, Name: "combat-feed", Type: discordgo.ChannelTypeGuildText})
	verifier.channelFound[fx.DiscordGuildID+"|"+channelID] = true
	if err := w.a.SaaSChannelRoutes.UpsertRoute(context.Background(), fx.OrgID, fx.InstallationID, "KILLFEED", channelID, true); err != nil {
		w.t.Fatal(err)
	}
}

func (w *embedWorld) preview(org, inst int64, route, actor string, body any) *httptest.ResponseRecorder {
	return w.call(w.a.handlePreviewEmbedTemplate, http.MethodPost, org, inst, route, actor, body)
}

func (w *embedWorld) test(org, inst int64, route, actor string, body any) *httptest.ResponseRecorder {
	return w.call(w.a.handleTestEmbedTemplate, http.MethodPost, org, inst, route, actor, body)
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &env)
	return env.Error.Code
}

func TestEmbedDesignerRolesAndUnsavedDraft(t *testing.T) {
	w := newEmbedWorld(t)
	w.a.AdminAudit = repository.NewAuditRepository(w.a.DB.Pool)
	w.routeKillfeed(w.a1, "chan-a1-combat")
	verifier := w.a.saasDiscordVerifier.(*fakeDiscordVerifier)

	// Every member may preview; the preview reports the routed destination.
	for _, actor := range []string{w.a1.OwnerDiscordID, w.admin, w.member} {
		rr := w.preview(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", actor, designerDraftBody("{{killer}} eliminated {{victim}}"))
		if rr.Code != http.StatusOK {
			t.Fatalf("preview as %s: %d %s", actor, rr.Code, rr.Body.String())
		}
		var p EmbedPreviewResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &p)
		if !p.Renderable || p.Embed == nil || p.Embed.Title != "ChampionPlayer eliminated RivalPlayer" || p.Destination == nil || p.Destination.ChannelID != "chan-a1-combat" {
			t.Fatalf("preview: %s", rr.Body.String())
		}
	}
	if rr := w.preview(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.outside, designerDraftBody("x")); rr.Code == http.StatusOK {
		t.Fatal("a non-member cannot preview")
	}

	// OWNER and ADMIN may send a test; MEMBER may not.
	w.a.embedTestLimiter = nil
	for _, actor := range []string{w.a1.OwnerDiscordID, w.admin} {
		rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", actor, designerDraftBody("Draft by "+actor[:3]+" {{killer}}"))
		if rr.Code != http.StatusOK {
			t.Fatalf("test as %s: %d %s", actor, rr.Code, rr.Body.String())
		}
		var tr EmbedTestResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &tr)
		if !tr.Sent || tr.Channel.ID != "chan-a1-combat" || tr.MessageID == "" {
			t.Fatalf("test response: %s", rr.Body.String())
		}
	}
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.member, designerDraftBody("x")); rr.Code != http.StatusForbidden {
		t.Fatalf("a MEMBER cannot send tests, got %d", rr.Code)
	}
	if len(verifier.sentMessages) != 2 {
		t.Fatalf("exactly two sends, got %d", len(verifier.sentMessages))
	}
	sent := verifier.sentMessages[len(verifier.sentMessages)-1]
	if sent.channelID != "chan-a1-combat" || sent.msg.AllowedMentions == nil || len(sent.msg.AllowedMentions.Parse) != 0 {
		t.Fatalf("send went to %s with mentions %+v", sent.channelID, sent.msg.AllowedMentions)
	}
	if !strings.HasPrefix(sent.msg.Embeds[0].Title, "Draft by") {
		t.Fatalf("the UNSAVED draft is what gets sent, got %q", sent.msg.Embeds[0].Title)
	}

	// Neither preview nor test persisted anything.
	if rr := w.get(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID); !strings.Contains(rr.Body.String(), `"customized":false`) {
		t.Fatalf("a draft must never be saved: %s", rr.Body.String())
	}

	// Safe audit rows: ids and route only, never template text or sample names.
	var n int
	var after string
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*), COALESCE(MAX(after_state::text),'') FROM admin_audit_log WHERE organization_id=$1 AND action='embed_test_sent'`, w.a1.OrgID).Scan(&n, &after); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !strings.Contains(after, "chan-a1-combat") || strings.Contains(after, "ChampionPlayer") || strings.Contains(after, "Draft by") {
		t.Fatalf("audit rows: %d %s", n, after)
	}
}

func TestEmbedDesignerTenantIsolationAndNoChannelChoice(t *testing.T) {
	w := newEmbedWorld(t)
	w.routeKillfeed(w.a1, "chan-a1-combat")
	w.routeKillfeed(w.b1, "chan-b1-combat")
	w.a.embedTestLimiter = nil
	verifier := w.a.saasDiscordVerifier.(*fakeDiscordVerifier)

	// Organization B cannot preview or test installation A, in any combination.
	if rr := w.preview(w.b1.OrgID, w.a1.InstallationID, "KILLFEED", w.b1.OwnerDiscordID, designerDraftBody("x")); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant preview: %d", rr.Code)
	}
	if rr := w.test(w.b1.OrgID, w.a1.InstallationID, "KILLFEED", w.b1.OwnerDiscordID, designerDraftBody("x")); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant test: %d", rr.Code)
	}
	if rr := w.test(w.a1.OrgID, w.b1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("x")); rr.Code != http.StatusNotFound {
		t.Fatalf("installation of another org: %d", rr.Code)
	}
	// A request naming a channel is rejected outright.
	body := designerDraftBody("x")
	body["channelId"] = "chan-b1-combat"
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, body); rr.Code != http.StatusBadRequest || errorCode(t, rr) != codeEmbedTemplateInvalid {
		t.Fatalf("a channelId in the body must be refused: %d %s", rr.Code, rr.Body.String())
	}
	if len(verifier.sentMessages) != 0 {
		t.Fatal("nothing may have been sent")
	}
	// Each installation only ever reaches its own routed channel.
	if rr := w.test(w.b1.OrgID, w.b1.InstallationID, "KILLFEED", w.b1.OwnerDiscordID, designerDraftBody("{{killer}}")); rr.Code != http.StatusOK || verifier.sentMessages[0].channelID != "chan-b1-combat" {
		t.Fatalf("B's own test: %d %+v", rr.Code, verifier.sentMessages)
	}
}

func TestEmbedDesignerTestFailuresAndRateLimit(t *testing.T) {
	w := newEmbedWorld(t)
	w.a.embedTestLimiter = nil
	// No KILLFEED route yet.
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("x")); errorCode(t, rr) != codeEmbedRouteNotConfigured {
		t.Fatalf("no route: %d %s", rr.Code, rr.Body.String())
	}
	// Unsupported runtime route.
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "SHOP", w.a1.OwnerDiscordID, map[string]any{"template": map[string]any{"enabled": true, "color": "#000000", "title": map[string]any{"enabled": true, "template": "x"}}}); errorCode(t, rr) != codeEmbedCustomNotSupported {
		t.Fatalf("unsupported: %d %s", rr.Code, rr.Body.String())
	}
	w.routeKillfeed(w.a1, "chan-a1-combat")
	verifier := w.a.saasDiscordVerifier.(*fakeDiscordVerifier)
	verifier.missing[w.a1.DiscordGuildID+"|chan-a1-combat"] = []string{"Embed Links"}
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("x")); errorCode(t, rr) != codeEmbedLinksRequired {
		t.Fatalf("embed links: %d %s", rr.Code, rr.Body.String())
	}
	verifier.missing[w.a1.DiscordGuildID+"|chan-a1-combat"] = nil

	w.a.embedTestLimiter = newEmbedTestLimiter()
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("{{killer}}")); rr.Code != http.StatusOK {
		t.Fatalf("first send: %d %s", rr.Code, rr.Body.String())
	}
	rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("{{killer}}"))
	if rr.Code != http.StatusTooManyRequests || errorCode(t, rr) != codeEmbedRateLimited {
		t.Fatalf("an immediate second send is rate limited: %d %s", rr.Code, rr.Body.String())
	}
}

// Preview and test send leave every gameplay table exactly as it was.
func TestEmbedDesignerHasNoGameplaySideEffects(t *testing.T) {
	w := newEmbedWorld(t)
	w.routeKillfeed(w.a1, "chan-a1-combat")
	w.a.embedTestLimiter = nil
	tables := []string{"kills", "deaths", "player_points", "point_transactions", "bounties", "player_location_events", "players", "installation_embed_templates"}
	count := func() map[string]int64 {
		out := map[string]int64{}
		for _, tbl := range tables {
			var n int64
			if err := w.a.DB.Pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM "+tbl).Scan(&n); err != nil {
				out[tbl] = -1 // table absent in this schema: compared as-is
				continue
			}
			out[tbl] = n
		}
		return out
	}
	before := count()
	for i := 0; i < 3; i++ {
		w.preview(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("{{killer}} eliminated {{victim}}"))
	}
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, designerDraftBody("{{killer}} eliminated {{victim}}")); rr.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rr.Code, rr.Body.String())
	}
	after := count()
	for _, tbl := range tables {
		if before[tbl] != after[tbl] {
			t.Fatalf("%s changed: %d -> %d", tbl, before[tbl], after[tbl])
		}
	}
}

// V2.1: the API serves the backend-owned variable metadata beside the old name
// lists, and preview/test still persist nothing.
func TestEmbedDesignerVariableMetadataAndNoPersistence(t *testing.T) {
	w := newEmbedWorld(t)
	w.routeKillfeed(w.a1, "chan-a1-combat")
	w.a.embedTestLimiter = nil

	var one struct {
		Variables           []string `json:"variables"`
		VariableDefinitions []struct {
			Name, Label, Category, Example, Availability, Format string
			Optional                                             bool
		} `json:"variableDefinitions"`
	}
	rr := w.get(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.member)
	if err := json.Unmarshal(rr.Body.Bytes(), &one); err != nil || len(one.VariableDefinitions) != len(one.Variables) || len(one.Variables) == 0 {
		t.Fatalf("GET must carry both variables and variableDefinitions: %s", rr.Body.String())
	}
	found := false
	for _, d := range one.VariableDefinitions {
		if d.Name == "killer_kd" {
			found = d.Label == "Killer K/D" && d.Category == "Killer Stats" && d.Example == "9.00" && d.Optional && d.Format == "decimal"
		}
	}
	if !found {
		t.Fatalf("killer_kd metadata missing or wrong: %+v", one.VariableDefinitions)
	}
	var list struct {
		Variables           map[string][]string          `json:"variables"`
		VariableDefinitions map[string][]json.RawMessage `json:"variableDefinitions"`
	}
	rr = w.list(w.a1.OrgID, w.a1.InstallationID, w.member)
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil || len(list.VariableDefinitions["HITFEED"]) != len(list.Variables["HITFEED"]) {
		t.Fatalf("list must carry per-route metadata: %s", rr.Body.String())
	}

	body := map[string]any{"template": goodTemplate(`{{killer}}\neliminated {{victim}}`), "variables": map[string]string{"killer": "A", "victim": "B", "killer_kd": "9.00"}}
	if rr := w.preview(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, body); rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.test(w.a1.OrgID, w.a1.InstallationID, "KILLFEED", w.a1.OwnerDiscordID, body); rr.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rr.Code, rr.Body.String())
	}
	var n int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM installation_embed_templates WHERE installation_id=$1`, w.a1.InstallationID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("drafts must never be saved: %d %v", n, err)
	}
}
