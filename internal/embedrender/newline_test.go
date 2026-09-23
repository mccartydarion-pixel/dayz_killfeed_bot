package embedrender

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

func TestExpandNewlines(t *testing.T) {
	cases := map[string]string{
		`a\nb`:        "a\nb",
		"a\nb":        "a\nb",
		`a\n\nb`:      "a\n\nb",
		`a\\nb`:       `a\nb`,  // escaped: the two literal characters
		`a\\\nb`:      `a\\nb`, // left to right: one kept backslash, then the escaped pair
		"a\r\nb\rc":   "a\nb\nc",
		`C:\path\x`:   `C:\path\x`, // other backslashes are kept
		`end\`:        `end\`,
		`{{killer}}`:  `{{killer}}`,
		`\n{{a}}\n`:   "\n{{a}}\n",
		`no escapes!`: `no escapes!`,
	}
	for in, want := range cases {
		if got := ExpandNewlines(in); got != want {
			t.Errorf("ExpandNewlines(%q) = %q, want %q", in, got, want)
		}
	}
}

func newlineTemplate() embedtemplates.Config {
	return embedtemplates.Config{
		Enabled: true, Color: "#D4AF37",
		Title:       embedtemplates.Text{Enabled: true, Template: `{{killer}}\neliminated`},
		Description: embedtemplates.Text{Enabled: true, Template: `Weapon: {{weapon}}\nDistance: {{distance}}\n\nStreak: {{streak}}`},
		Footer:      embedtemplates.Footer{Enabled: true, Text: "Line one\nLine two"},
		Fields: []embedtemplates.Field{
			{Key: "stats", Label: "KILLER\nSTATS", Enabled: true, Template: `Kills: {{killer_kills}}\nDeaths: {{killer_deaths}}\nK/D: {{killer_kd}}\nStreak: {{killer_streak}}`, Order: 0},
			{Key: "lit", Label: "Literal", Enabled: true, Template: `path\\n{{weapon}}`, Order: 1},
		},
	}
}

func TestNewlinesRenderInEverySection(t *testing.T) {
	vars := map[string]string{"killer": "Alice", "weapon": "M4-A1", "distance": "11.7m", "streak": "1", "killer_kills": "9", "killer_deaths": "0", "killer_kd": "9.00", "killer_streak": "1"}
	e, err := RenderEvent(newlineTemplate(), "KILLFEED", vars, at)
	if err != nil {
		t.Fatal(err)
	}
	if e.Description != "Weapon: M4-A1\nDistance: 11.7m\n\nStreak: 1" {
		t.Fatalf("description: %q", e.Description)
	}
	if e.Footer.Text != "Line one\nLine two" {
		t.Fatalf("real Enter newlines stay: %q", e.Footer.Text)
	}
	if e.Fields[0].Value != "Kills: 9\nDeaths: 0\nK/D: 9.00\nStreak: 1" {
		t.Fatalf("field value: %q", e.Fields[0].Value)
	}
	if e.Fields[1].Value != `path\nM4-A1` {
		t.Fatalf(`an escaped \\n stays literal: %q`, e.Fields[1].Value)
	}
	if e.Title != "Alice eliminated" || e.Fields[0].Name != "KILLER STATS" {
		t.Fatalf("single-line sections fold breaks: title %q, name %q", e.Title, e.Fields[0].Name)
	}
}

func TestNewlineWithMissingOptionalValue(t *testing.T) {
	vars := map[string]string{"killer": "Alice", "weapon": "M4-A1", "distance": "11.7m"}
	e, err := RenderEvent(newlineTemplate(), "KILLFEED", vars, at)
	if err != nil {
		t.Fatal(err)
	}
	// The description keeps its other lines; the stats field is omitted entirely.
	if e.Description != "Weapon: M4-A1\nDistance: 11.7m\n\nStreak:" {
		t.Fatalf("description: %q", e.Description)
	}
	for _, f := range e.Fields {
		if strings.HasPrefix(f.Name, "KILLER") {
			t.Fatal("a field whose values are missing is omitted, never shown with blanks or zeros")
		}
	}
}

func TestHostileValueCannotInjectALineBreak(t *testing.T) {
	cfg := embedtemplates.Config{Enabled: true, Color: "#000000", Description: embedtemplates.Text{Enabled: true, Template: "Killer: {{killer}}\nVictim: {{victim}}"}}
	vars := map[string]string{"killer": `Bad\nPlayer`, "victim": "Real\nBreak\r\nPlayer"}
	e, err := RenderEvent(cfg, "KILLFEED", vars, at)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(e.Description, "\n") != 1 {
		t.Fatalf("only the template's own break may exist: %q", e.Description)
	}
	if !strings.Contains(e.Description, `Bad\\nPlayer`) || !strings.Contains(e.Description, "Real Break Player") {
		t.Fatalf("values stay sanitized player data: %q", e.Description)
	}
}

func TestLimitsApplyAfterNewlineExpansion(t *testing.T) {
	// 500 `\n` escapes are 1000 template characters but 500 rendered line breaks.
	breaks := strings.Repeat(`\n`, 500)
	cfg := embedtemplates.Config{Enabled: true, Color: "#000000",
		Description: embedtemplates.Text{Enabled: true, Template: "x" + breaks + "y"},
		Fields:      []embedtemplates.Field{{Key: "f", Label: "F", Enabled: true, Template: "a" + strings.Repeat(`\n`, 400) + "b"}},
	}
	e, err := Render(cfg, "KILLFEED", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(e.Description); n != 502 {
		t.Fatalf(`each \n counts as one rendered character: got %d`, n)
	}
	if n := utf8.RuneCountInString(e.Fields[0].Value); n != 402 {
		t.Fatalf("field value length after expansion: %d", n)
	}

	// Over the limits once expanded: truncated like any other text.
	cfg.Description.Template = strings.Repeat(`ab\n`, 1500) // 4500 rendered > 4096
	cfg.Fields[0].Template = strings.Repeat(`ab\n`, 400)    // 1200 rendered > 1024
	e, err = Render(cfg, "KILLFEED", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(e.Description) > MaxDescription || utf8.RuneCountInString(e.Fields[0].Value) > MaxFieldValue || TotalText(e) > MaxTotal {
		t.Fatalf("limits after expansion: desc %d value %d total %d", utf8.RuneCountInString(e.Description), utf8.RuneCountInString(e.Fields[0].Value), TotalText(e))
	}
}
