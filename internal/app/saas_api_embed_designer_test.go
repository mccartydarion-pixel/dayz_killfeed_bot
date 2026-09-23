package app

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

var designerAt = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func designerTemplate() embedtemplates.Config {
	return embedtemplates.Config{
		Enabled: true, Color: "#D4AF37", Timestamp: true,
		Title:       embedtemplates.Text{Enabled: true, Template: "{{killer}} eliminated {{victim}}"},
		Description: embedtemplates.Text{Enabled: true, Template: "A {{weapon}} kill from {{distance}} away."},
		Author:      embedtemplates.Author{Enabled: true, Name: "Champion", IconURL: "https://cdn.example.com/a.png"},
		Thumbnail:   embedtemplates.Media{Enabled: true, URL: "https://cdn.example.com/t.png"},
		Image:       embedtemplates.Media{Enabled: true, URL: "http://cdn.example.com/i.png"},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "Champion on {{server_name}}", IconURL: "https://cdn.example.com/f.png"},
		Fields: []embedtemplates.Field{
			{Key: "weapon", Label: "Weapon", Enabled: true, Template: "{{weapon}}", Inline: true, Order: 0},
			{Key: "distance", Label: "Distance", Enabled: true, Template: "{{distance}}", Inline: true, Order: 1},
			{Key: "streak", Label: "Streak", Enabled: true, Template: "{{streak}}", Inline: false, Order: 2},
		},
	}
}

func designerVars() map[string]string {
	return map[string]string{"killer": "ChampionPlayer", "victim": "RivalPlayer", "weapon": "M4-A1", "distance": "127.4m", "server_name": "Northstar"}
}

// --- fakes ------------------------------------------------------------------

type designerRoutesFake struct {
	routes map[string]string
	err    error
}

func (f *designerRoutesFake) ListForInstallation(context.Context, int64, int64) ([]repository.ChannelRoute, error) {
	var out []repository.ChannelRoute
	for k, v := range f.routes {
		out = append(out, repository.ChannelRoute{RouteKey: k, ChannelID: v, ManagedByChampion: true})
	}
	return out, f.err
}

type designerDiscordFake struct {
	channels []discord.RawGuildChannel
	missing  []string
	notFound bool
	sendErr  error
	sent     []fakeSent
}

type fakeSent struct {
	channel string
	msg     *discordgo.MessageSend
}

func (f *designerDiscordFake) ListAllGuildChannels(string) ([]discord.RawGuildChannel, error) {
	return f.channels, nil
}
func (f *designerDiscordFake) Verify(_, channelID string) discord.Verification {
	return discord.Verification{GuildFound: true, ChannelFound: !f.notFound, Missing: f.missing}
}
func (f *designerDiscordFake) SendMessage(channelID string, msg *discordgo.MessageSend) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, fakeSent{channelID, msg})
	return "msg-" + strconv.Itoa(len(f.sent)), nil
}

func designerGuild() *designerDiscordFake {
	return &designerDiscordFake{channels: []discord.RawGuildChannel{{ID: "combat", Name: "🔫・combat-feed", Type: discordgo.ChannelTypeGuildText}}}
}

func draft(cfg embedtemplates.Config, vars map[string]string) EmbedDraftRequest {
	return EmbedDraftRequest{Template: cfg, Variables: vars}
}

// --- preview ----------------------------------------------------------------

func TestPreviewRendersUnsavedDraftWithProductionRenderer(t *testing.T) {
	resp, emb, derr := renderEmbedDraft("KILLFEED", draft(designerTemplate(), designerVars()), runtimeRenderingEnabled, designerAt)
	if derr != nil || !resp.Renderable || emb == nil {
		t.Fatalf("valid draft must render: %+v %v", resp, derr)
	}
	// The preview is exactly what the live path (RenderEvent) produces.
	cfg, _ := embedtemplates.Validate(designerTemplate(), "KILLFEED")
	live, err := embedrender.RenderEvent(cfg, "KILLFEED", designerVars(), designerAt)
	if err != nil || !reflect.DeepEqual(emb, live) {
		t.Fatalf("preview must equal the production render:\n%+v\n%+v", emb, live)
	}
	d := resp.Embed
	if d.Title != "ChampionPlayer eliminated RivalPlayer" || d.Color != 0xD4AF37 || d.Timestamp != "2026-09-23T12:00:00Z" || d.Author.IconURL == "" || d.Image.URL != "http://cdn.example.com/i.png" {
		t.Fatalf("dto: %+v", d)
	}
	if resp.Metrics.FieldCount != 2 || resp.Metrics.TotalText != embedrender.TotalText(emb) {
		t.Fatalf("metrics: %+v", resp.Metrics)
	}
	// {{streak}} is absent: its field is omitted, exactly as live events omit it, with a warning.
	for _, f := range d.Fields {
		if f.Name == "Streak" {
			t.Fatal("a field whose value is missing must be omitted")
		}
	}
	if !strings.Contains(strings.Join(resp.Warnings, " "), "1 field(s) are hidden") {
		t.Fatalf("warnings: %v", resp.Warnings)
	}
}

func TestPreviewRejectsInvalidDraftsSafely(t *testing.T) {
	cases := map[string]EmbedDraftRequest{
		"bad color":        draft(func() embedtemplates.Config { c := designerTemplate(); c.Color = "red"; return c }(), nil),
		"unknown variable": draft(designerTemplate(), map[string]string{"password": "x"}),
		"foreign variable": draft(designerTemplate(), map[string]string{"item": "rations"}),
		"javascript image": draft(func() embedtemplates.Config { c := designerTemplate(); c.Image.URL = "javascript:alert(1)"; return c }(), nil),
		"data thumbnail": draft(func() embedtemplates.Config {
			c := designerTemplate()
			c.Thumbnail.URL = "data:image/png;base64,AA"
			return c
		}(), nil),
		"file author icon": draft(func() embedtemplates.Config {
			c := designerTemplate()
			c.Author.IconURL = "file:///etc/passwd"
			return c
		}(), nil),
		"ftp footer icon": draft(func() embedtemplates.Config { c := designerTemplate(); c.Footer.IconURL = "ftp://x/y.png"; return c }(), nil),
		"userinfo image": draft(func() embedtemplates.Config {
			c := designerTemplate()
			c.Image.URL = "https://user:pw@evil.example/x.png"
			return c
		}(), nil),
		"control char image": draft(func() embedtemplates.Config {
			c := designerTemplate()
			c.Image.URL = "https://cdn.example.com/\x00.png"
			return c
		}(), nil),
		"template expression": draft(func() embedtemplates.Config {
			c := designerTemplate()
			c.Title.Template = "{{killer | upper}}"
			return c
		}(), nil),
		"oversized value": draft(designerTemplate(), map[string]string{"killer": strings.Repeat("a", maxDraftVariableValue+1)}),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			resp, _, derr := renderEmbedDraft("KILLFEED", req, runtimeRenderingEnabled, designerAt)
			if derr == nil {
				t.Fatalf("expected a validation error, got %+v", resp)
			}
			if derr.code != codeEmbedTemplateInvalid || strings.Contains(strings.ToLower(derr.message), "pq:") || strings.Contains(derr.message, "embedrender") {
				t.Fatalf("customer-safe EMBED_TEMPLATE_INVALID expected, got %s: %s", derr.code, derr.message)
			}
		})
	}
}

func TestPreviewSanitizesHostileSampleValuesLikeLiveEvents(t *testing.T) {
	hostile := map[string]string{
		"killer": "@everyone <@123> <@&456> <#789> @here", "victim": "Bob‮evil​\x07", "weapon": "**bold** [x](https://evil) `code`", "distance": "87m",
	}
	_, emb, derr := renderEmbedDraft("KILLFEED", draft(designerTemplate(), hostile), runtimeRenderingEnabled, designerAt)
	if derr != nil || emb == nil {
		t.Fatal(derr)
	}
	text := emb.Title + " " + emb.Description
	for _, bad := range []string{"@everyone", "@here", "<@123>", "<@&456>", "<#789>", "‮", "​", "\x07", "**bold**", "[x](", "`code`"} {
		if strings.Contains(text, bad) {
			t.Fatalf("%q survived sanitization: %q", bad, text)
		}
	}
}

func TestPreviewEnforcesDiscordLimitsAfterSubstitution(t *testing.T) {
	// Within every save-time limit, but far over Discord's limits once 100-character
	// values are substituted: title 300 > 256, description 5000 > 4096, field names
	// 300 > 256, field values 1500 > 1024, footer 2500 > 2048, total >> 6000, and
	// the 25-field cap.
	cfg := designerTemplate()
	cfg.Title.Template = "{{killer}}{{victim}}{{killer}}"
	cfg.Description.Template = strings.Repeat("{{weapon}} ", 50)
	cfg.Footer.Text = strings.Repeat("{{server_name}}", 25)
	cfg.Author.Enabled = false
	cfg.Fields = nil
	for i := 0; i < embedtemplates.MaxFields; i++ {
		cfg.Fields = append(cfg.Fields, embedtemplates.Field{Key: "f" + strconv.Itoa(i), Label: "{{killer}}{{victim}}{{killer}}", Enabled: true, Template: strings.Repeat("{{victim}} ", 15), Order: i})
	}
	vars := map[string]string{"killer": strings.Repeat("K", 100), "victim": strings.Repeat("V", 100), "weapon": strings.Repeat("W", 100), "server_name": strings.Repeat("S", 100), "distance": "1m"}
	resp, emb, derr := renderEmbedDraft("KILLFEED", draft(cfg, vars), runtimeRenderingEnabled, designerAt)
	if derr != nil {
		t.Fatalf("the draft must pass save-time validation: %v", derr)
	}
	if emb == nil || !resp.Renderable {
		t.Fatal("expected a rendered embed")
	}
	runes := utf8.RuneCountInString
	if runes(emb.Title) > embedrender.MaxTitle || runes(emb.Description) > embedrender.MaxDescription || len(emb.Fields) > embedrender.MaxFields {
		t.Fatalf("section limit exceeded: title %d desc %d fields %d", runes(emb.Title), runes(emb.Description), len(emb.Fields))
	}
	for _, f := range emb.Fields {
		if runes(f.Name) > embedrender.MaxFieldName || runes(f.Value) > embedrender.MaxFieldValue {
			t.Fatalf("field limit exceeded: %d / %d", runes(f.Name), runes(f.Value))
		}
	}
	if emb.Footer != nil && runes(emb.Footer.Text) > embedrender.MaxFooter {
		t.Fatal("footer limit exceeded")
	}
	if embedrender.TotalText(emb) > embedrender.MaxTotal || resp.Metrics.TotalText > embedrender.MaxTotal {
		t.Fatalf("total %d exceeds %d", embedrender.TotalText(emb), embedrender.MaxTotal)
	}
}

func TestPreviewDisabledAndUnsupportedRoutes(t *testing.T) {
	cfg := designerTemplate()
	cfg.Enabled = false
	resp, emb, derr := renderEmbedDraft("KILLFEED", draft(cfg, designerVars()), runtimeRenderingEnabled, designerAt)
	if derr != nil || resp.Renderable || emb != nil || resp.Reason != reasonTemplateDisabled || resp.Embed != nil {
		t.Fatalf("a disabled template is not renderable: %+v %v", resp, derr)
	}

	// A route without runtime custom rendering previews the design with a clear warning.
	shop := embedtemplates.Config{Enabled: true, Color: "#4C7FA0", Title: embedtemplates.Text{Enabled: true, Template: "{{player}} bought {{item}}"}}
	resp, _, derr = renderEmbedDraft("SHOP", draft(shop, map[string]string{"player": "A", "item": "Rations"}), runtimeRenderingOff, designerAt)
	if derr != nil || !resp.Renderable || resp.CustomRenderingSupported || resp.RuntimeRendering != runtimeRenderingOff || len(resp.Warnings) == 0 {
		t.Fatalf("unsupported route preview: %+v %v", resp, derr)
	}

	// Flag off on a supported route: renders, and says NOT_ENABLED.
	resp, _, _ = renderEmbedDraft("KILLFEED", draft(designerTemplate(), designerVars()), runtimeRenderingOff, designerAt)
	if resp.RuntimeRendering != runtimeRenderingOff || !resp.CustomRenderingSupported || !strings.Contains(strings.Join(resp.Warnings, " "), "NOT_ENABLED") {
		t.Fatalf("flag-off preview must say so: %+v", resp)
	}
}

// --- test send --------------------------------------------------------------

func TestTestSendSendsTheExactPreviewEmbedToTheRoutedChannel(t *testing.T) {
	routes := &designerRoutesFake{routes: map[string]string{"KILLFEED": "combat"}}
	d := designerGuild()
	req := draft(designerTemplate(), designerVars())
	resp, derr := sendEmbedTest(context.Background(), routes, d, 1, 2, "g", "KILLFEED", req, runtimeRenderingOff, designerAt)
	if derr != nil || !resp.Sent || resp.MessageID != "msg-1" || resp.Channel.ID != "combat" || resp.Channel.Name != "🔫・combat-feed" || resp.RuntimeRendering != runtimeRenderingOff {
		t.Fatalf("send: %+v %v", resp, derr)
	}
	if len(d.sent) != 1 || d.sent[0].channel != "combat" {
		t.Fatalf("exactly one send to the routed channel, got %+v", d.sent)
	}
	msg := d.sent[0].msg
	// Parity: the sent embed is structurally identical to the canonical preview.
	preview, previewEmb, _ := renderEmbedDraft("KILLFEED", req, runtimeRenderingOff, designerAt)
	if len(msg.Embeds) != 1 || !reflect.DeepEqual(msg.Embeds[0], previewEmb) || !reflect.DeepEqual(embedToDTO(msg.Embeds[0]), preview.Embed) {
		t.Fatalf("test embed must equal the preview:\n%+v\n%+v", msg.Embeds[0], previewEmb)
	}
	if !strings.HasPrefix(msg.Content, "🧪 Champion Embed Test • Killfeed\nThis is a design preview — not a live server event.") {
		t.Fatalf("notice: %q", msg.Content)
	}
	am := msg.AllowedMentions
	if am == nil || am.Parse == nil || len(am.Parse) != 0 || len(am.Users) != 0 || len(am.Roles) != 0 || am.RepliedUser {
		t.Fatalf("mentions must be fully disabled: %+v", am)
	}
}

func TestTestSendRefusesWhatItCannotHonestlySend(t *testing.T) {
	ok := func() (*designerRoutesFake, *designerDiscordFake) {
		return &designerRoutesFake{routes: map[string]string{"KILLFEED": "combat"}}, designerGuild()
	}
	disabled := designerTemplate()
	disabled.Enabled = false
	cases := []struct {
		name  string
		route string
		setup func(*designerRoutesFake, *designerDiscordFake)
		req   EmbedDraftRequest
		code  string
	}{
		{"unsupported route", "SHOP", nil, draft(embedtemplates.Config{Enabled: true, Color: "#000000", Title: embedtemplates.Text{Enabled: true, Template: "x"}}, nil), codeEmbedCustomNotSupported},
		{"disabled template", "KILLFEED", nil, draft(disabled, designerVars()), codeEmbedTemplateNotRenderable},
		{"invalid template", "KILLFEED", nil, draft(embedtemplates.Config{Enabled: true, Color: "nope"}, nil), codeEmbedTemplateInvalid},
		{"no route", "KILLFEED", func(r *designerRoutesFake, _ *designerDiscordFake) { r.routes = map[string]string{} }, draft(designerTemplate(), designerVars()), codeEmbedRouteNotConfigured},
		{"channel deleted", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.channels = nil }, draft(designerTemplate(), designerVars()), codeEmbedChannelUnavailable},
		{"channel in another guild", "KILLFEED", func(r *designerRoutesFake, _ *designerDiscordFake) { r.routes["KILLFEED"] = "foreign" }, draft(designerTemplate(), designerVars()), codeEmbedChannelUnavailable},
		{"inaccessible channel", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.notFound = true }, draft(designerTemplate(), designerVars()), codeEmbedChannelUnavailable},
		{"no view", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.missing = []string{"View Channel"} }, draft(designerTemplate(), designerVars()), codeEmbedSendForbidden},
		{"no send", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.missing = []string{"Send Messages"} }, draft(designerTemplate(), designerVars()), codeEmbedSendForbidden},
		{"no embed links", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.missing = []string{"Embed Links"} }, draft(designerTemplate(), designerVars()), codeEmbedLinksRequired},
		{"discord rejects", "KILLFEED", func(_ *designerRoutesFake, d *designerDiscordFake) { d.sendErr = errors.New("403 Missing Access") }, draft(designerTemplate(), designerVars()), codeEmbedSendFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			routes, d := ok()
			if c.setup != nil {
				c.setup(routes, d)
			}
			resp, derr := sendEmbedTest(context.Background(), routes, d, 1, 2, "g", c.route, c.req, runtimeRenderingEnabled, designerAt)
			if derr == nil || derr.code != c.code {
				t.Fatalf("want %s, got %+v / %+v", c.code, resp, derr)
			}
			if resp.Sent || len(d.sent) != 0 {
				t.Fatal("nothing may be sent on failure")
			}
			if strings.Contains(derr.message, "403") || strings.Contains(derr.message, "Missing Access") {
				t.Fatalf("raw Discord errors must not leak: %s", derr.message)
			}
		})
	}
	// Read Message History is reported but does not block a send.
	routes, d := ok()
	d.missing = []string{"Read Message History"}
	if resp, derr := sendEmbedTest(context.Background(), routes, d, 1, 2, "g", "KILLFEED", draft(designerTemplate(), designerVars()), runtimeRenderingEnabled, designerAt); derr != nil || !resp.Sent {
		t.Fatalf("history is not needed to post: %+v %v", resp, derr)
	}
}

func TestTestSendRequestCannotChooseTheChannel(t *testing.T) {
	// The request type has no destination fields at all; decoding rejects them.
	for _, field := range []string{"channelId", "guildId", "token"} {
		if _, ok := reflect.TypeOf(EmbedDraftRequest{}).FieldByNameFunc(func(n string) bool { return strings.EqualFold(n, field) }); ok {
			t.Fatalf("EmbedDraftRequest must not have a %s field", field)
		}
	}
}

func TestEmbedTestRateLimits(t *testing.T) {
	l := newEmbedTestLimiter()
	if !l.allow(7, 2) {
		t.Fatal("first send is allowed")
	}
	if l.allow(7, 2) {
		t.Fatal("a second send by the same actor within 3 s is refused")
	}
	for user := int64(100); user < 109; user++ {
		if !l.allow(user, 2) {
			t.Fatalf("send %d of 10 per minute must be allowed", user-98)
		}
	}
	if l.allow(200, 2) {
		t.Fatal("the 11th send per minute per installation is refused")
	}
	if !l.allow(300, 3) {
		t.Fatal("other installations are unaffected")
	}
	if httpStatusForCode[codeEmbedRateLimited] != 429 {
		t.Fatal("rate limiting answers HTTP 429")
	}
}

// The designer performs validation, rendering, route lookup and one Discord send -
// never a gameplay write or event injection. Its imports prove it cannot reach the
// kill pipeline, economy, bounties, stats, presence or the ADM parser.
func TestEmbedDesignerHasNoGameplayDependencies(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "saas_api_embed_designer.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"/internal/killfeed", "/internal/economy", "/internal/bounties", "/internal/stats", "/internal/streaks", "/internal/achievements", "/internal/factionstats", "/internal/nitrado", "/internal/events", "/internal/shop", "/internal/linking"}
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbidden {
			if strings.HasSuffix(path, bad) {
				t.Fatalf("embed designer must not import %s", path)
			}
		}
	}
}

// Live publishing, preview and test send all go through embedrender.RenderEvent.
func TestLiveCustomizerUsesTheSharedRenderPath(t *testing.T) {
	store := fakeTemplateStore{cfg: func() *embedtemplates.Config {
		c, _ := embedtemplates.Validate(designerTemplate(), "KILLFEED")
		return &c
	}()}
	r := embedrender.New(embedrender.Options{Enabled: true, Source: store})
	def := &discordgo.MessageEmbed{Title: "default"}
	live := r.Customize(context.Background(), 1, 1, "KILLFEED", designerVars(), designerAt, def)
	_, preview, _ := renderEmbedDraft("KILLFEED", draft(designerTemplate(), designerVars()), runtimeRenderingEnabled, designerAt)
	if !reflect.DeepEqual(live, preview) {
		t.Fatalf("live event render must equal the preview:\n%+v\n%+v", live, preview)
	}
}

type fakeTemplateStore struct{ cfg *embedtemplates.Config }

func (f fakeTemplateStore) ResolveTemplate(context.Context, int64, int64, string) (int64, *embedtemplates.Config, error) {
	return 2, f.cfg, nil
}
