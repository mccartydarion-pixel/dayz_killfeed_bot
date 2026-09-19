package embedrender

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

func allText(e *discordgo.MessageEmbed) string {
	var b strings.Builder
	b.WriteString(e.Title + "\n" + e.Description)
	for _, f := range e.Fields {
		b.WriteString("\n" + f.Name + "\n" + f.Value)
	}
	if e.Footer != nil {
		b.WriteString("\n" + e.Footer.Text)
	}
	if e.Author != nil {
		b.WriteString("\n" + e.Author.Name)
	}
	return b.String()
}

var hostileNames = []string{
	"@everyone", "@here", "<@123456789012345678>", "<@!123456789012345678>", "<@&987654321098765432>", "<#123456789012345678>",
	"Bob @everyone pls", "**bold**", "__under__", "~~strike~~", "||spoiler||", "`code`", "```block```", "> quote", "[click](https://evil.example)",
	"<a:emoji:123>", "<t:1700000000:R>", "{{victim}}", "{{ killer }}", "{{{{x}}}}", "}}{{", "${killer}", "{% if x %}", "a\nb\r\nc", "a\x00b\x1bc",
	"‮evil", "​​​", "########", "@@@@", strings.Repeat("A", 5000), strings.Repeat("🙂", 3000), "\\", "\\\\\\", "back\\slash*",
}

// Player-controlled values must never become an active mention, break a field's
// structure, inject a template, or push the embed past a Discord limit.
func TestHostilePlayerValuesAreNeutralized(t *testing.T) {
	cfg := killTemplate()
	src := &fakeSource{inst: 7, cfg: &cfg}
	r, _ := newR(src)
	for _, hostile := range hostileNames {
		vars := full()
		vars["killer"], vars["victim"], vars["weapon"], vars["server_name"], vars["distance"] = hostile, hostile, hostile, hostile, hostile
		e := r.Customize(context.Background(), 1, 10, "KILLFEED", vars, at, defEmbed())
		if e == nil || e.Description == "DEFAULT CARD" {
			// The only acceptable fallback is the default card; a hostile value alone must
			// still render (it is sanitized, not rejected).
			t.Fatalf("hostile value %q must still render a card", clip(hostile))
		}
		text := allText(e)
		if strings.ContainsAny(text, "@") {
			t.Errorf("%q: an '@' survived (mention risk): %q", clip(hostile), clip(text))
		}
		// No unescaped mention/channel syntax: every '<' from a value is escaped.
		for i, ch := range text {
			if ch == '<' && (i == 0 || text[i-1] != '\\') {
				t.Errorf("%q: unescaped '<' (mention/emoji/timestamp syntax): %q", clip(hostile), clip(text))
				break
			}
		}
		if strings.Contains(text, "evil.example") && !strings.Contains(text, "\\[") {
			t.Errorf("%q: a masked link survived: %q", clip(hostile), clip(text))
		}
		// Template injection: a value's braces never expand a second time.
		if strings.Contains(hostile, "{{victim}}") && strings.Count(text, "Bob") > 0 {
			t.Errorf("%q: the value was re-interpreted as a template: %q", clip(hostile), clip(text))
		}
		// Structure: no control characters or line breaks come from a value.
		for _, f := range e.Fields {
			if strings.ContainsAny(f.Name+f.Value, "\n\r\x00\x1b") {
				t.Errorf("%q: control characters in a field: %+v", clip(hostile), f)
			}
		}
		// Discord limits hold whatever the values were.
		assertLimits(t, e, clip(hostile))
	}
}

func clip(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

func assertLimits(t *testing.T, e *discordgo.MessageEmbed, label string) {
	t.Helper()
	if n := utf8.RuneCountInString(e.Title); n > MaxTitle {
		t.Errorf("%s: title %d", label, n)
	}
	if n := utf8.RuneCountInString(e.Description); n > MaxDescription {
		t.Errorf("%s: description %d", label, n)
	}
	if len(e.Fields) > MaxFields {
		t.Errorf("%s: %d fields", label, len(e.Fields))
	}
	for _, f := range e.Fields {
		if n := utf8.RuneCountInString(f.Name); n > MaxFieldName || n == 0 {
			t.Errorf("%s: field name %d", label, n)
		}
		if n := utf8.RuneCountInString(f.Value); n > MaxFieldValue || n == 0 {
			t.Errorf("%s: field value %d", label, n)
		}
	}
	if e.Footer != nil && utf8.RuneCountInString(e.Footer.Text) > MaxFooter {
		t.Errorf("%s: footer", label)
	}
	if e.Author != nil && utf8.RuneCountInString(e.Author.Name) > MaxAuthorName {
		t.Errorf("%s: author", label)
	}
	if n := TotalText(e); n > MaxTotal {
		t.Errorf("%s: combined %d", label, n)
	}
}

func TestVeryLongNamesAreCappedBeforeSubstitution(t *testing.T) {
	if got := SanitizeValue(strings.Repeat("N", 5000)); utf8.RuneCountInString(got) > MaxValue+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("a value is capped at %d characters with an ellipsis: %d", MaxValue, utf8.RuneCountInString(got))
	}
	if SanitizeValue("Short Name") != "Short Name" || SanitizeValue("  padded  \t name ") != "padded name" {
		t.Fatal("ordinary names stay readable")
	}
	if SanitizeValue("@everyone") != "everyone" || SanitizeValue("#general") != "general" {
		t.Fatalf("@ and # are removed like the existing card sanitizers: %q %q", SanitizeValue("@everyone"), SanitizeValue("#general"))
	}
	if SanitizeValue("@@@") != "" || SanitizeValue("\x00\x1b") != "" {
		t.Fatal("nothing usable left = empty (the value is then absent)")
	}
	if got := SanitizeValue("a**b_c"); got != `a\*\*b\_c` {
		t.Fatalf("markdown is escaped: %q", got)
	}
	if SanitizeValue("<@123>") != `\<123\>` {
		t.Fatalf("mention syntax is defused: %q", SanitizeValue("<@123>"))
	}
	if got := SanitizeValue("é🙂 名前"); got != "é🙂 名前" {
		t.Fatalf("unicode names are preserved: %q", got)
	}
}

// Administrator-authored template text keeps its own markdown (only VARIABLE values are
// escaped), and a template containing "@everyone" is harmless text because every send
// site sets AllowedMentions to parse nothing.
func TestAdminAuthoredMarkdownIsPreserved(t *testing.T) {
	cfg := killTemplate()
	cfg.Title.Template = "**{{killer}}** ➜ *{{victim}}*"
	cfg.Description.Template = "> {{weapon}}\n`{{distance}}`"
	src := &fakeSource{inst: 7, cfg: &cfg}
	r, _ := newR(src)
	e := r.Customize(context.Background(), 1, 10, "KILLFEED", full(), at, defEmbed())
	if e.Title != "**Alice** ➜ *Bob*" || e.Description != "> M4-A1\n`87m`" {
		t.Fatalf("admin markdown must survive: %q / %q", e.Title, e.Description)
	}
	// ...while a hostile VALUE in the same template cannot add markup of its own.
	e = r.Customize(context.Background(), 1, 10, "KILLFEED", map[string]string{"killer": "**pwn**", "victim": "x", "weapon": "w", "distance": "d"}, at, defEmbed())
	if !strings.HasPrefix(e.Title, `**\*\*pwn\*\***`) {
		t.Fatalf("a value's markup is escaped inside admin markup: %q", e.Title)
	}
}

// A template can still name variables freely across sections; values stay separate.
func TestPlaceholdersInsideValuesAreNotExpandedTwice(t *testing.T) {
	cfg := embedtemplates.Config{Enabled: true, Color: "#123456", Title: embedtemplates.Text{Enabled: true, Template: "{{killer}} / {{victim}}"}}
	src := &fakeSource{inst: 7, cfg: &cfg}
	r, _ := newR(src)
	e := r.Customize(context.Background(), 1, 10, "KILLFEED", map[string]string{"killer": "{{victim}}", "victim": "REAL"}, at, defEmbed())
	if e.Title != "{{victim}} / REAL" {
		t.Fatalf("single-pass substitution: %q", e.Title)
	}
}
