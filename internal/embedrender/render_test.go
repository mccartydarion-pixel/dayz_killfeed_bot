package embedrender

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

func killTemplate() embedtemplates.Config {
	return embedtemplates.Config{
		Enabled: true, RouteKey: "KILLFEED", Color: "#D4AF37", Timestamp: true,
		Title:       embedtemplates.Text{Enabled: true, Template: "{{killer}} eliminated {{victim}}"},
		Description: embedtemplates.Text{Enabled: true, Template: "A {{weapon}} kill from {{distance}} away."},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "Champion Killfeed on {{server_name}}", IconURL: "https://cdn.example.com/f.png"},
		Author:      embedtemplates.Author{Enabled: true, Name: "Champion", IconURL: "https://cdn.example.com/a.png"},
		Thumbnail:   embedtemplates.Media{Enabled: true, URL: "https://cdn.example.com/t.png"},
		Image:       embedtemplates.Media{Enabled: true, URL: "https://cdn.example.com/i.png"},
		Fields: []embedtemplates.Field{
			{Key: "weapon", Label: "Weapon", Enabled: true, Template: "{{weapon}}", Inline: true, Order: 1},
			{Key: "killer", Label: "Killer", Enabled: true, Template: "{{killer}}", Inline: false, Order: 0},
			{Key: "distance", Label: "Distance", Enabled: true, Template: "{{distance}}", Inline: true, Order: 2},
		},
	}
}

var at = time.Date(2026, 9, 19, 12, 30, 0, 0, time.UTC)

func full() map[string]string {
	return map[string]string{"killer": "Alice", "victim": "Bob", "weapon": "M4-A1", "distance": "87m", "server_name": "Northstar"}
}

func mustRender(t *testing.T, cfg embedtemplates.Config, vars map[string]string) *discordgo.MessageEmbed {
	t.Helper()
	e, err := Render(cfg, "KILLFEED", vars, at)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRenderSubstitutesEverySection(t *testing.T) {
	e := mustRender(t, killTemplate(), full())
	if e.Title != "Alice eliminated Bob" || e.Description != "A M4-A1 kill from 87m away." {
		t.Fatalf("title/description: %q / %q", e.Title, e.Description)
	}
	if e.Footer == nil || e.Footer.Text != "Champion Killfeed on Northstar" || e.Footer.IconURL != "https://cdn.example.com/f.png" {
		t.Fatalf("footer: %+v", e.Footer)
	}
	if e.Author == nil || e.Author.Name != "Champion" || e.Author.IconURL != "https://cdn.example.com/a.png" {
		t.Fatalf("author: %+v", e.Author)
	}
	if e.Thumbnail == nil || e.Thumbnail.URL != "https://cdn.example.com/t.png" || e.Image == nil || e.Image.URL != "https://cdn.example.com/i.png" {
		t.Fatalf("thumbnail/image: %+v %+v", e.Thumbnail, e.Image)
	}
	if e.Color != 0xD4AF37 {
		t.Fatalf("color: %x", e.Color)
	}
	if e.Timestamp != "2026-09-19T12:30:00Z" {
		t.Fatalf("timestamp: %q", e.Timestamp)
	}
}

func TestFieldOrderAndInline(t *testing.T) {
	e := mustRender(t, killTemplate(), full())
	if len(e.Fields) != 3 || e.Fields[0].Name != "Killer" || e.Fields[1].Name != "Weapon" || e.Fields[2].Name != "Distance" {
		t.Fatalf("fields must follow their order: %+v", e.Fields)
	}
	if e.Fields[0].Inline || !e.Fields[1].Inline || !e.Fields[2].Inline {
		t.Fatalf("inline must be carried per field: %+v", e.Fields)
	}
	if e.Fields[0].Value != "Alice" || e.Fields[1].Value != "M4-A1" {
		t.Fatalf("field values: %+v", e.Fields)
	}
}

func TestMissingOptionalDataOmitsTheFieldEntirely(t *testing.T) {
	v := full()
	delete(v, "distance")
	e := mustRender(t, killTemplate(), v)
	for _, f := range e.Fields {
		if f.Name == "Distance" || strings.Contains(f.Value, "N/A") || f.Value == "" {
			t.Fatalf("an absent optional value must omit its field, got %+v", f)
		}
	}
	if len(e.Fields) != 2 {
		t.Fatalf("expected Killer and Weapon only: %+v", e.Fields)
	}
	// An empty string counts as absent, exactly like a missing key.
	v["distance"] = ""
	if got := mustRender(t, killTemplate(), v); len(got.Fields) != 2 {
		t.Fatalf("an empty value is absent: %+v", got.Fields)
	}
	// In running text the gap is closed instead of leaving a hole.
	if e.Description != "A M4-A1 kill from away." {
		t.Fatalf("description gap: %q", e.Description)
	}
	// A field whose LABEL references the absent value is omitted too.
	cfg := killTemplate()
	cfg.Fields = []embedtemplates.Field{{Key: "x", Label: "{{distance}}", Enabled: true, Template: "v", Order: 0}, {Key: "y", Label: "L", Enabled: true, Template: "v", Order: 1}}
	if got := mustRender(t, cfg, v); len(got.Fields) != 1 || got.Fields[0].Name != "L" {
		t.Fatalf("label-referenced absence: %+v", got.Fields)
	}
	// A label or value that mixes text with an absent variable is omitted too - the
	// remaining text alone ("Range:") would misrepresent the field.
	cfg.Fields = []embedtemplates.Field{{Key: "x", Label: "Range: {{distance}}", Enabled: true, Template: "v", Order: 0}, {Key: "y", Label: "L", Enabled: true, Template: "hit at {{distance}}", Order: 1}, {Key: "z", Label: "Z", Enabled: true, Template: "ok", Order: 2}}
	if got := mustRender(t, cfg, v); len(got.Fields) != 1 || got.Fields[0].Name != "Z" {
		t.Fatalf("mixed text with an absent variable must omit the field: %+v", got.Fields)
	}
}

func TestDisabledSectionsAreOmitted(t *testing.T) {
	cfg := killTemplate()
	cfg.Title.Enabled, cfg.Author.Enabled, cfg.Footer.Enabled = false, false, false
	cfg.Thumbnail.Enabled, cfg.Image.Enabled, cfg.Timestamp = false, false, false
	cfg.Fields[0].Enabled = false
	e := mustRender(t, cfg, full())
	if e.Title != "" || e.Author != nil || e.Footer != nil || e.Thumbnail != nil || e.Image != nil || e.Timestamp != "" {
		t.Fatalf("disabled sections must not appear: %+v", e)
	}
	if len(e.Fields) != 2 {
		t.Fatalf("a disabled field must not appear: %+v", e.Fields)
	}
}

func TestDisabledOrEmptyTemplateIsNotRenderable(t *testing.T) {
	cfg := killTemplate()
	cfg.Enabled = false
	if _, err := Render(cfg, "KILLFEED", full(), at); err != ErrNotRenderable {
		t.Fatalf("a disabled template means 'use the default': %v", err)
	}
	empty := embedtemplates.Config{Enabled: true, Color: "#000000", Description: embedtemplates.Text{Enabled: true, Template: "{{distance}}"}}
	v := full()
	delete(v, "distance")
	if _, err := Render(empty, "KILLFEED", v, at); err != ErrNotRenderable {
		t.Fatalf("a template that renders to nothing must not become an empty embed: %v", err)
	}
}

func TestInvalidColorAndUnapprovedVariableFailSafely(t *testing.T) {
	for _, c := range []string{"", "red", "#12345", "#GGGGGG", "16777216", "0xFFFFFF"} {
		cfg := killTemplate()
		cfg.Color = c
		if _, err := Render(cfg, "KILLFEED", full(), at); err == nil {
			t.Errorf("color %q must not render", c)
		}
	}
	cfg := killTemplate()
	cfg.Title.Template = "{{amount}}" // another route's variable, impossible through validated config
	if _, err := Render(cfg, "KILLFEED", full(), at); err == nil {
		t.Error("an unapproved variable must fail the render, never be substituted")
	}
	if _, err := Render(killTemplate(), "NOPE", full(), at); err == nil {
		t.Error("an unknown route must not render")
	}
}

func TestBadURLsAreDroppedNotSent(t *testing.T) {
	cfg := killTemplate()
	cfg.Thumbnail.URL = "javascript:alert(1)"
	cfg.Image.URL = "data:image/png;base64,AAAA"
	cfg.Author.IconURL = "file:///etc/passwd"
	cfg.Footer.IconURL = "https://user:pw@example.com/x.png"
	e := mustRender(t, cfg, full())
	if e.Thumbnail != nil || e.Image != nil {
		t.Fatalf("unsafe URLs must be dropped: %+v %+v", e.Thumbnail, e.Image)
	}
	if e.Author == nil || e.Author.IconURL != "" || e.Footer == nil || e.Footer.IconURL != "" {
		t.Fatalf("an unsafe icon is dropped but the text stays: %+v %+v", e.Author, e.Footer)
	}
}

func TestSectionTruncationAtRenderTime(t *testing.T) {
	long := strings.Repeat("Z", 5000)
	cfg := embedtemplates.Config{Enabled: true, Color: "#123456",
		Title:       embedtemplates.Text{Enabled: true, Template: "{{killer}}"},
		Description: embedtemplates.Text{Enabled: true, Template: "{{weapon}}"},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "{{server_name}}"},
		Fields:      []embedtemplates.Field{{Key: "a", Label: "{{killer}}", Enabled: true, Template: "{{weapon}}", Order: 0}},
	}
	// Values are not capped here (Render is pure); the section limits are.
	e := mustRender(t, cfg, map[string]string{"killer": long, "weapon": strings.Repeat("W", 6000), "server_name": long})
	check := func(name string, s string, max int) {
		if n := utf8.RuneCountInString(s); n > max || !strings.HasSuffix(s, "…") {
			t.Errorf("%s: %d chars (max %d) ends %q", name, n, max, s[len(s)-4:])
		}
	}
	check("title", e.Title, MaxTitle)
	check("footer", e.Footer.Text, MaxFooter)
	check("field name", e.Fields[0].Name, MaxFieldName)
	check("field value", e.Fields[0].Value, MaxFieldValue)
	if utf8.RuneCountInString(e.Description) > MaxDescription {
		t.Errorf("description: %d", utf8.RuneCountInString(e.Description))
	}
	if TotalText(e) > MaxTotal {
		t.Errorf("combined text %d exceeds %d", TotalText(e), MaxTotal)
	}
}

func TestCombinedLimitIsEnforcedDeterministically(t *testing.T) {
	cfg := embedtemplates.Config{Enabled: true, Color: "#123456",
		Title:       embedtemplates.Text{Enabled: true, Template: "T {{killer}}"},
		Description: embedtemplates.Text{Enabled: true, Template: "{{weapon}}"},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "{{server_name}}"},
		Author:      embedtemplates.Author{Enabled: true, Name: "{{victim}}"},
	}
	for i := 0; i < 25; i++ {
		cfg.Fields = append(cfg.Fields, embedtemplates.Field{Key: "f" + strings.Repeat("x", i), Label: "N{{killer}}", Enabled: true, Template: "{{weapon}}", Order: i})
	}
	v := map[string]string{"killer": strings.Repeat("k", 200), "weapon": strings.Repeat("w", 5000), "server_name": strings.Repeat("s", 3000), "victim": strings.Repeat("v", 300)}
	e1 := mustRender(t, cfg, v)
	e2 := mustRender(t, cfg, v)
	if TotalText(e1) > MaxTotal {
		t.Fatalf("combined text %d exceeds %d", TotalText(e1), MaxTotal)
	}
	if len(e1.Fields) > MaxFields {
		t.Fatalf("more than %d fields", MaxFields)
	}
	if e1.Title != e2.Title || e1.Description != e2.Description || len(e1.Fields) != len(e2.Fields) {
		t.Fatal("truncation must be deterministic")
	}
	if e1.Title == "" || !strings.HasPrefix(e1.Title, "T ") {
		t.Fatalf("the title keeps its structure: %q", e1.Title)
	}
	for _, f := range e1.Fields {
		if f.Name == "" || f.Value == "" {
			t.Fatalf("a field must never end up empty (Discord rejects it): %+v", f)
		}
	}
}

func TestFieldCountIsCappedAt25(t *testing.T) {
	cfg := killTemplate()
	cfg.Fields = nil
	for i := 0; i < 40; i++ { // more than validation would allow, defensively
		cfg.Fields = append(cfg.Fields, embedtemplates.Field{Key: "f" + strings.Repeat("x", i), Label: "L", Enabled: true, Template: "v", Order: i})
	}
	if e := mustRender(t, cfg, full()); len(e.Fields) != 25 {
		t.Fatalf("expected 25 fields, got %d", len(e.Fields))
	}
}

func TestTruncateNeverLeavesADanglingEscape(t *testing.T) {
	if got := truncate(`abc\*def`, 5); strings.Contains(got, `\…`) {
		t.Fatalf("a cut escape must not swallow the ellipsis: %q", got)
	}
	if truncate("hello", 0) != "" || truncate("hello", 1) != "…" || truncate("hi", 5) != "hi" {
		t.Fatal("truncate edge cases")
	}
	if got := truncate("héllo wörld 🙂🙂🙂", 8); utf8.RuneCountInString(got) != 8 || !utf8.ValidString(got) {
		t.Fatalf("truncation is by characters and stays valid UTF-8: %q", got)
	}
}
