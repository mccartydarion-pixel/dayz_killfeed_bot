package embedtemplates

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func valid() Config {
	return Config{
		Enabled: true, Color: "#d4af37",
		Title:       Text{Enabled: true, Template: "{{killer}} eliminated {{victim}}"},
		Description: Text{Enabled: true, Template: "A {{ weapon }} kill from {{distance}} away."},
		Author:      Author{Enabled: false, Name: "Champion"},
		Footer:      Footer{Enabled: true, Text: "Champion Killfeed"},
		Timestamp:   true,
		Fields: []Field{
			{Key: "weapon", Label: "Weapon", Enabled: true, Template: "{{weapon}}", Inline: true, Order: 2},
			{Key: "killer", Label: "Killer", Enabled: true, Template: "{{killer}}", Inline: true, Order: 0},
			{Key: "victim", Label: "Victim", Enabled: true, Template: "{{victim}}", Inline: true, Order: 1},
		},
	}
}

func issuesOf(t *testing.T, cfg Config, route string) []string {
	t.Helper()
	_, err := Validate(cfg, route)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a validation error, got %v", err)
	}
	return ve.Issues
}

func mustFail(t *testing.T, name string, mutate func(*Config), route, wantSubstring string) {
	t.Helper()
	cfg := valid()
	mutate(&cfg)
	joined := strings.Join(issuesOf(t, cfg, route), " | ")
	if !strings.Contains(joined, wantSubstring) {
		t.Errorf("%s: expected an issue containing %q, got %q", name, wantSubstring, joined)
	}
}

func TestValidConfigIsNormalized(t *testing.T) {
	out, err := Validate(valid(), "KILLFEED")
	if err != nil {
		t.Fatal(err)
	}
	if out.RouteKey != "KILLFEED" || out.Version != SchemaVersion || out.Color != "#D4AF37" {
		t.Fatalf("route/version/color must be normalized: %+v", out)
	}
	if out.Description.Template != "A {{weapon}} kill from {{distance}} away." {
		t.Fatalf("placeholders are canonicalized (no inner spaces): %q", out.Description.Template)
	}
	// Fields sorted by order and renumbered 0..n-1.
	if len(out.Fields) != 3 || out.Fields[0].Key != "killer" || out.Fields[1].Key != "victim" || out.Fields[2].Key != "weapon" {
		t.Fatalf("fields must follow their order: %+v", out.Fields)
	}
	for i, f := range out.Fields {
		if f.Order != i {
			t.Fatalf("orders must be renumbered 0..n-1: %+v", out.Fields)
		}
	}
}

func TestRepeatedOrdersKeepTheSubmittedSequence(t *testing.T) {
	cfg := valid()
	for i := range cfg.Fields {
		cfg.Fields[i].Order = 0
	}
	out, err := Validate(cfg, "KILLFEED")
	if err != nil || out.Fields[0].Key != "weapon" || out.Fields[2].Key != "victim" {
		t.Fatalf("a stable sort keeps the submitted order on ties: %+v %v", out.Fields, err)
	}
}

func TestEveryRouteAcceptsAMinimalTemplateAndItsOwnVariables(t *testing.T) {
	for _, route := range RouteKeys() {
		cfg := Config{Enabled: true, Color: "#123456", Title: Text{Enabled: true, Template: "hi"}}
		for _, v := range Variables(route) {
			cfg.Description.Template += "{{" + v + "}} "
		}
		cfg.Description.Enabled = true
		if _, err := Validate(cfg, route); err != nil {
			t.Errorf("%s: %v", route, err)
		}
	}
}

func TestInvalidRoute(t *testing.T) {
	for _, bad := range []string{"", "killfeed", "KILLFEED ", "NOPE", "KILLFEED;DROP", "../KILLFEED"} {
		if ValidRoute(bad) {
			t.Errorf("%q must not be a valid route", bad)
		}
		if _, err := Validate(valid(), bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	mustFail(t, "route in body differs from URL", func(c *Config) { c.RouteKey = "ECONOMY" }, "KILLFEED", "does not match")
}

func TestColorValidation(t *testing.T) {
	for _, bad := range []string{"", "D4AF37", "#D4AF3", "#D4AF377", "#GGGGGG", "0xD4AF37", "red", "#FFFFFFFF", "16777216", "-1", "#D4AF37 ", "rgb(1,2,3)"} {
		mustFail(t, "color "+bad, func(c *Config) { c.Color = bad }, "KILLFEED", "color: must be")
	}
	for _, good := range []string{"#000000", "#FFFFFF", "#abcdef"} {
		cfg := valid()
		cfg.Color = good
		out, err := Validate(cfg, "KILLFEED")
		if err != nil || out.Color != strings.ToUpper(good) {
			t.Errorf("%s: %+v %v", good, out.Color, err)
		}
	}
}

func TestURLValidation(t *testing.T) {
	bad := []string{"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:image/png;base64,AAAA", "file:///etc/passwd", "ftp://example.com/a.png",
		"//example.com/a.png", "example.com/a.png", "http://", "https://", "https:///nohost", "http://exa mple.com", "https://user:pw@example.com/a.png",
		"https://example.com/{{killer}}.png", "mailto:a@b.c", "http:example.com", "vbscript:x", "https://example.com/a\nb", strings.Repeat("a", 2100)}
	for _, u := range bad {
		for _, target := range []struct {
			name string
			set  func(*Config, string)
		}{
			{"author.iconUrl", func(c *Config, v string) { c.Author.IconURL = v }},
			{"thumbnail.url", func(c *Config, v string) { c.Thumbnail = Media{Enabled: true, URL: v} }},
			{"image.url", func(c *Config, v string) { c.Image = Media{Enabled: true, URL: v} }},
			{"footer.iconUrl", func(c *Config, v string) { c.Footer.IconURL = v }},
		} {
			cfg := valid()
			target.set(&cfg, u)
			if !strings.Contains(strings.Join(issuesOf(t, cfg, "KILLFEED"), "|"), target.name) {
				t.Errorf("%s = %q must be rejected", target.name, clip(u, 40))
			}
		}
	}
	for _, good := range []string{"http://example.com/a.png", "https://cdn.example.com/a/b.png?x=1#f", "HTTPS://EXAMPLE.COM/A.PNG", "https://example.com:8443/a.png"} {
		cfg := valid()
		cfg.Thumbnail = Media{Enabled: true, URL: good}
		cfg.Footer.IconURL = good
		if _, err := Validate(cfg, "KILLFEED"); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	// A disabled section may keep an empty URL; an enabled one must have it.
	mustFail(t, "enabled thumbnail without a URL", func(c *Config) { c.Thumbnail = Media{Enabled: true} }, "KILLFEED", "thumbnail.url: is required")
	cfg := valid()
	cfg.Image = Media{Enabled: false}
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("a disabled image without a URL is fine: %v", err)
	}
}

func TestDiscordLimits(t *testing.T) {
	mustFail(t, "title", func(c *Config) { c.Title.Template = strings.Repeat("x", MaxTitle+1) }, "KILLFEED", "title.template: too long")
	mustFail(t, "description", func(c *Config) { c.Description.Template = strings.Repeat("x", MaxDescription+1) }, "KILLFEED", "description.template: too long")
	mustFail(t, "author name", func(c *Config) { c.Author = Author{Enabled: true, Name: strings.Repeat("x", MaxAuthorName+1)} }, "KILLFEED", "author.name: too long")
	mustFail(t, "footer", func(c *Config) { c.Footer.Text = strings.Repeat("x", MaxFooterText+1) }, "KILLFEED", "footer.text: too long")
	mustFail(t, "field label", func(c *Config) { c.Fields[0].Label = strings.Repeat("x", MaxFieldLabel+1) }, "KILLFEED", "fields[0].label: too long")
	mustFail(t, "field value", func(c *Config) { c.Fields[0].Template = strings.Repeat("x", MaxFieldValue+1) }, "KILLFEED", "fields[0].template: too long")
	mustFail(t, "too many fields", func(c *Config) {
		c.Fields = nil
		for i := 0; i <= MaxFields; i++ {
			c.Fields = append(c.Fields, Field{Key: "f" + strings.Repeat("x", i), Label: "L", Template: "T", Enabled: true, Order: i})
		}
	}, "KILLFEED", "too many fields")

	// Exactly at the limits is fine (counted in characters, not bytes).
	cfg := valid()
	cfg.Title.Template = strings.Repeat("é", MaxTitle)
	cfg.Description.Template = strings.Repeat("🙂", 100)
	cfg.Fields = nil
	for i := 0; i < MaxFields; i++ {
		cfg.Fields = append(cfg.Fields, Field{Key: "f" + strings.Repeat("y", i), Label: "L", Template: "T", Enabled: true, Order: i})
	}
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("limits are inclusive and counted in characters: %v", err)
	}

	// Combined text budget: each part alone is within its own limit.
	mustFail(t, "total budget", func(c *Config) {
		c.Description.Template = strings.Repeat("d", MaxDescription)
		c.Footer.Text = strings.Repeat("f", MaxFooterText)
		c.Fields = []Field{{Key: "a", Label: "L", Template: strings.Repeat("v", MaxFieldValue), Enabled: true}}
	}, "KILLFEED", "combined embed text too long")
	// Disabled parts do not count towards the budget.
	cfg = valid()
	cfg.Description = Text{Enabled: false, Template: strings.Repeat("d", MaxDescription)}
	cfg.Footer = Footer{Enabled: false, Text: strings.Repeat("f", MaxFooterText)}
	cfg.Fields = []Field{{Key: "a", Label: "L", Template: strings.Repeat("v", MaxFieldValue), Enabled: true}}
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("disabled sections are not counted: %v", err)
	}
	// ...but each still respects its own limit.
	mustFail(t, "disabled section limit", func(c *Config) { c.Description = Text{Enabled: false, Template: strings.Repeat("d", MaxDescription+1)} }, "KILLFEED", "description.template: too long")
}

func TestVariableValidation(t *testing.T) {
	// Approved for the route: fine, including repeats.
	cfg := valid()
	cfg.Title.Template = "{{killer}} {{killer}} {{ammo}} {{streak}} {{server_name}}"
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("approved variables: %v", err)
	}
	mustFail(t, "unknown variable", func(c *Config) { c.Title.Template = "{{nope}}" }, "KILLFEED", "unknown variable {{nope}}")
	mustFail(t, "another route's variable", func(c *Config) { c.Title.Template = "{{amount}}" }, "KILLFEED", "{{amount}} is not available for KILLFEED")
	mustFail(t, "wrong-route variable on economy", func(c *Config) { c.Title.Template = "{{killer}}" }, "ECONOMY", "{{killer}} is not available for ECONOMY")
	mustFail(t, "uppercase name", func(c *Config) { c.Title.Template = "{{Killer}}" }, "KILLFEED", "unknown variable")
	mustFail(t, "variable in a field label", func(c *Config) { c.Fields[0].Label = "{{secret}}" }, "KILLFEED", "fields[0].label: unknown variable")
	mustFail(t, "variable in the footer", func(c *Config) { c.Footer.Text = "{{amount}}" }, "KILLFEED", "footer.text")
	mustFail(t, "variable in the author", func(c *Config) { c.Author = Author{Enabled: true, Name: "{{nope}}"} }, "KILLFEED", "author.name")
	for _, ok := range Variables("ECONOMY") {
		c := valid()
		c.Title.Template = "{{" + ok + "}}"
		c.Description.Template, c.Fields = "x", nil
		if _, err := Validate(c, "ECONOMY"); err != nil {
			t.Errorf("ECONOMY variable %s: %v", ok, err)
		}
	}
	if !strings.Contains(strings.Join(Variables("ECONOMY"), ","), "transaction_type") {
		t.Error("ECONOMY must offer transaction_type")
	}
}

func TestMalformedAndCodeLikeTemplatesAreRejected(t *testing.T) {
	bad := []string{
		"{{killer}", "{killer}}", "{{killer", "killer}}", "{{}}", "{{ }}", "{{{killer}}}", "{killer}", "{{ killer victim }}",
		"{{killer|upper}}", "{{ killer | upper }}", "{{.Killer}}", "{{ .Killer }}", "{{killer()}}", "{{killer.name}}", "{{killer-x}}", "{{ if killer }}", "{{end}}",
		"{{range .}}x{{end}}", "{{template \"a\"}}", "{{printf \"%s\" killer}}", "{{ 1+1 }}", "{{killer}}{{", "${killer}", "{% if x %}", "{%- x -%}",
		"{# c #}", "{{__proto__}}", "{{constructor}}", "{{`ls`}}", "{{killer;rm -rf /}}", "$(whoami) {{", "{{killer}}}",
	}
	for _, s := range bad {
		cfg := valid()
		cfg.Title.Template = s
		// "${killer}" style text without any brace pair is plain text; the ones with braces are reserved.
		if _, err := Validate(cfg, "KILLFEED"); err == nil {
			t.Errorf("template %q must be rejected", s)
		}
	}
	// Plain text that merely looks technical is fine: only braces are reserved.
	for _, s := range []string{"$(whoami)", "`ls`", "<%= killer %>", "$" + "{{killer}}", "<script>alert(1)</script>", "rm -rf /", "100% {{killer}}!", "50$ bounty", "a | b", "x = 1+1", "@everyone <t:1700000000:R>"} {
		cfg := valid()
		cfg.Title.Template = s
		if _, err := Validate(cfg, "KILLFEED"); err != nil {
			t.Errorf("plain text %q must be accepted (it is never evaluated): %v", s, err)
		}
	}
	mustFail(t, "control character", func(c *Config) { c.Description.Template = "a\x00b" }, "KILLFEED", "control characters")
	mustFail(t, "escape character", func(c *Config) { c.Description.Template = "a\x1bb" }, "KILLFEED", "control characters")
	cfg := valid()
	cfg.Description.Template = "line one\nline two\ttabbed"
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("newlines and tabs are fine: %v", err)
	}
}

func TestFieldKeyAndEmptinessRules(t *testing.T) {
	mustFail(t, "duplicate key", func(c *Config) { c.Fields[2].Key = "WEAPON" }, "KILLFEED", "duplicate field key")
	for _, k := range []string{"", "has space", "a.b", "a/b", strings.Repeat("k", 65), "é"} {
		mustFail(t, "key "+k, func(c *Config) { c.Fields[0].Key = k }, "KILLFEED", "key: must be")
	}
	mustFail(t, "negative order", func(c *Config) { c.Fields[0].Order = -1 }, "KILLFEED", "order")
	mustFail(t, "huge order", func(c *Config) { c.Fields[0].Order = 1 << 40 }, "KILLFEED", "order")
	mustFail(t, "empty enabled title", func(c *Config) { c.Title.Template = "  " }, "KILLFEED", "title.template: must not be empty")
	mustFail(t, "empty enabled field label", func(c *Config) { c.Fields[0].Label = "" }, "KILLFEED", "fields[0].label: must not be empty")
	mustFail(t, "empty enabled field value", func(c *Config) { c.Fields[0].Template = "" }, "KILLFEED", "fields[0].template: must not be empty")
	mustFail(t, "nothing to send", func(c *Config) {
		c.Title.Enabled, c.Description.Enabled, c.Footer.Enabled = false, false, false
		for i := range c.Fields {
			c.Fields[i].Enabled = false
		}
	}, "KILLFEED", "at least one enabled")
	// A disabled template (route uses the default) may be empty.
	cfg := valid()
	cfg.Enabled = false
	cfg.Title.Enabled, cfg.Description.Enabled, cfg.Footer.Enabled = false, false, false
	cfg.Fields = nil
	if _, err := Validate(cfg, "KILLFEED"); err != nil {
		t.Errorf("a disabled template may be empty: %v", err)
	}
}

func TestIssuesNeverEchoFreeFormInput(t *testing.T) {
	secret := "SUPER-SECRET-VALUE-12345"
	cfg := valid()
	cfg.Title.Template = "{{" + secret + "}} " + secret
	cfg.Thumbnail = Media{Enabled: true, URL: "javascript:" + secret}
	cfg.Color = secret
	for _, issue := range issuesOf(t, cfg, "KILLFEED") {
		if strings.Contains(issue, "javascript") || strings.Contains(issue, "SUPER-SECRET-VALUE-12345") && !strings.Contains(issue, "unknown variable") {
			t.Errorf("issue leaks input: %q", issue)
		}
	}
	// The one allowed echo is a matched variable name, clipped.
	long := "{{" + strings.Repeat("a", 300) + "}}"
	cfg = valid()
	cfg.Title.Template = long
	for _, issue := range issuesOf(t, cfg, "KILLFEED") {
		if len(issue) > 200 {
			t.Errorf("echoed variable names are clipped: %d chars", len(issue))
		}
	}
}

func TestRouteVariableTablesAreSelfConsistent(t *testing.T) {
	for route, vars := range routeVariables {
		if len(vars) == 0 {
			t.Errorf("%s has no variables", route)
		}
		seen := map[string]bool{}
		for _, v := range vars {
			if seen[v] || !regexpName(v) {
				t.Errorf("%s: bad or duplicate variable %q", route, v)
			}
			seen[v] = true
		}
	}
	// Callers cannot mutate the table through the returned slices.
	v := Variables("KILLFEED")
	v[0] = "hacked"
	if Variables("KILLFEED")[0] == "hacked" {
		t.Fatal("Variables must return a copy")
	}
	all := AllVariables()
	all["KILLFEED"][0] = "hacked"
	if Variables("KILLFEED")[0] == "hacked" {
		t.Fatal("AllVariables must return copies")
	}
}

func regexpName(s string) bool {
	if s == "" {
		return false
	}
	// Lowercase snake case: a letter first, then letters, digits or underscores
	// (the {{name}} grammar accepts digits, e.g. h2h_score).
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r == '_' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s[0] >= 'a' && s[0] <= 'z'
}

// --- service with a fake store ---------------------------------------------------------------

type fakeStore struct {
	mu    sync.Mutex
	rows  map[string]Stored
	calls int
}

func (f *fakeStore) key(o, i int64, r string) string {
	return string(rune(o)) + "/" + string(rune(i)) + "/" + r
}
func (f *fakeStore) List(context.Context, int64, int64) ([]Stored, error) {
	f.calls++
	return nil, nil
}
func (f *fakeStore) Get(_ context.Context, o, i int64, r string) (*Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if s, ok := f.rows[f.key(o, i, r)]; ok {
		return &s, nil
	}
	return nil, nil
}
func (f *fakeStore) Upsert(_ context.Context, o, i int64, cfg Config) (Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.rows == nil {
		f.rows = map[string]Stored{}
	}
	s := Stored{Config: cfg, InstallationID: i, UpdatedAt: time.Now()}
	f.rows[f.key(o, i, cfg.RouteKey)] = s
	return s, nil
}
func (f *fakeStore) Delete(_ context.Context, o, i int64, r string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	_, ok := f.rows[f.key(o, i, r)]
	delete(f.rows, f.key(o, i, r))
	return ok, nil
}

func TestServiceValidatesBeforeTouchingTheStore(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	ctx := context.Background()
	if _, err := svc.Save(ctx, 1, 1, "NOPE", valid()); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("expected ErrInvalidRoute, got %v", err)
	}
	bad := valid()
	bad.Color = "nope"
	if _, err := svc.Save(ctx, 1, 1, "KILLFEED", bad); err == nil {
		t.Fatal("an invalid template must be rejected")
	}
	if _, err := svc.Get(ctx, 1, 1, "NOPE"); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("Get must validate the route")
	}
	if _, err := svc.Delete(ctx, 1, 1, "NOPE"); !errors.Is(err, ErrInvalidRoute) {
		t.Fatal("Delete must validate the route")
	}
	if store.calls != 0 {
		t.Fatalf("nothing invalid may reach the store, got %d calls", store.calls)
	}
	saved, err := svc.Save(ctx, 1, 1, "KILLFEED", valid())
	if err != nil || saved.Config.Color != "#D4AF37" {
		t.Fatalf("save must store the NORMALIZED config: %+v %v", saved.Config.Color, err)
	}
	if got, _ := svc.Get(ctx, 1, 1, "KILLFEED"); got == nil {
		t.Fatal("expected the saved template")
	}
	if existed, err := svc.Delete(ctx, 1, 1, "KILLFEED"); err != nil || !existed {
		t.Fatalf("delete: %v %v", existed, err)
	}
	if existed, err := svc.Delete(ctx, 1, 1, "KILLFEED"); err != nil || existed {
		t.Fatalf("a second delete is idempotent: %v %v", existed, err)
	}
}
