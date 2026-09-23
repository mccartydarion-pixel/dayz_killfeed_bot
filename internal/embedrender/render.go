// Package embedrender turns an installation's saved custom embed template (Phase 2,
// internal/embedtemplates) plus one event's variables into a Discord embed, safely.
//
// It is PRESENTATION ONLY and pure once the template is in hand: no network calls,
// no image fetches, no database access in Render. Publishers keep building their
// existing Champion default embed and hand it to Renderer.Customize, which returns
// either that same default (no custom template, feature off, or ANY problem) or the
// custom rendering - so a template can never cost an event its card.
package embedrender

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/embedtemplates"
)

// Discord embed limits enforced on the RENDERED output (the template text was already
// validated at save time, but substituted values make it longer).
const (
	MaxTitle       = 256
	MaxDescription = 4096
	MaxFields      = 25
	MaxFieldName   = 256
	MaxFieldValue  = 1024
	MaxFooter      = 2048
	MaxAuthorName  = 256
	MaxTotal       = 6000
	// MaxValue caps one substituted variable value (a name, a weapon ...).
	MaxValue = 100
	ellipsis = "…"
)

var (
	tokenRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)
	spaces  = regexp.MustCompile(`[ \t]{2,}`)
	colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

	// ErrNotRenderable: the template produced nothing Discord would accept (an empty
	// embed) or is not usable. Callers fall back to the Champion default.
	ErrNotRenderable = errors.New("embedrender: template produced no renderable content")
)

// SanitizeValue makes one player- or server-controlled value safe to place in an
// embed: control characters and line breaks become spaces, '@' and '#' are removed
// (same policy as the existing card sanitizers, so a name can never form a mention,
// @everyone/@here or channel link), Discord markdown/mention syntax characters are
// escaped so a name cannot break structure (`<@1>`, `[x](y)`, `**`, backticks ...),
// and the result is capped at MaxValue characters. It is deterministic.
func SanitizeValue(s string) string {
	var b strings.Builder
	pendingSpace := false
	n := 0
	for _, r := range s {
		switch {
		case r == '@' || r == '#':
			continue
		case r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' || r == '\u200b' || r == '\u200e' || r == '\u200f' || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			// control, line/paragraph separators, zero-width and bidi override characters
			pendingSpace = b.Len() > 0
			continue
		case r == ' ' || r == '\t':
			pendingSpace = b.Len() > 0
			continue
		}
		if n >= MaxValue {
			b.WriteString(ellipsis)
			break
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		switch r {
		case '\\', '*', '_', '~', '|', '`', '>', '<', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// truncate cuts s to at most max characters ending in an ellipsis, without leaving a
// dangling escape backslash. max < 1 yields "".
func truncate(s string, max int) string {
	if max < 1 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return ellipsis
	}
	cut := string([]rune(s)[:max-1])
	// An odd number of trailing backslashes would escape the ellipsis: drop one.
	trail := len(cut) - len(strings.TrimRight(cut, `\`))
	if trail%2 == 1 {
		cut = cut[:len(cut)-1]
	}
	return cut + ellipsis
}

// ExpandNewlines turns the template's newline syntax into real line breaks. It runs
// on TEMPLATE text only, before any variable is substituted, so a value can never be
// read as syntax (a player named `Bad\nPlayer` stays that name). Rules, left to
// right: `\\n` (backslash, backslash, n) -> the two literal characters `\n`;
// `\n` -> a line break; CRLF and a lone CR -> a line break; any other backslash is
// kept as written. Real newlines typed in a textarea already are line breaks.
func ExpandNewlines(s string) string {
	if !strings.ContainsAny(s, "\\\r") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+2 < len(s) && s[i+1] == '\\' && s[i+2] == 'n':
			b.WriteString(`\n`)
			i += 2
		case c == '\\' && i+1 < len(s) && s[i+1] == 'n':
			b.WriteByte('\n')
			i++
		case c == '\r':
			b.WriteByte('\n')
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// singleLine folds line breaks into spaces for sections Discord shows on one line
// (title, author name, field name), so they stay consistent however the template
// was typed.
func singleLine(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// substitute replaces every {{name}} in one pass (values are never re-scanned, so a
// value containing "{{x}}" stays literal text). It reports whether any referenced
// variable was absent, and an error for a variable outside the route's approved set.
func substitute(tmpl string, vars map[string]string, approved map[string]bool) (string, bool, error) {
	missing := false
	var badVar string
	out := tokenRe.ReplaceAllStringFunc(ExpandNewlines(tmpl), func(tok string) string {
		name := tokenRe.FindStringSubmatch(tok)[1]
		if !approved[name] {
			badVar = name
			return ""
		}
		v := vars[name]
		if v == "" {
			missing = true
			return ""
		}
		return v
	})
	if badVar != "" {
		return "", false, fmt.Errorf("embedrender: variable not approved for the route")
	}
	if missing {
		out = spaces.ReplaceAllString(out, " ")
	}
	return strings.TrimSpace(out), missing, nil
}

func validURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > embedtemplates.MaxURLLength || strings.ContainsFunc(s, func(r rune) bool { return r <= 0x20 || r == 0x7f }) {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil {
		return ""
	}
	if sc := strings.ToLower(u.Scheme); sc != "http" && sc != "https" {
		return ""
	}
	return s
}

// Render builds the custom embed for one event. routeKey selects the approved
// variable set; vars holds this event's values (an empty or missing entry means
// "absent"); at stamps the embed when the template asks for a timestamp. It returns
// ErrNotRenderable (or another error) when the caller should use the default.
//
// Behavior:
//   - disabled template -> ErrNotRenderable (the route uses its default);
//   - a field whose label or value references an absent variable is omitted entirely
//     (never "Distance: N/A"); an absent variable in the title/description/footer is
//     replaced by nothing and the gap closed;
//   - every section is truncated to Discord's limit with an ellipsis, then the
//     combined text is trimmed to MaxTotal deterministically;
//   - URLs are re-checked (http/https only); a bad one drops that element.
func Render(cfg embedtemplates.Config, routeKey string, vars map[string]string, at time.Time) (*discordgo.MessageEmbed, error) {
	if !cfg.Enabled {
		return nil, ErrNotRenderable
	}
	approved := map[string]bool{}
	for _, v := range embedtemplates.Variables(routeKey) {
		approved[v] = true
	}
	if len(approved) == 0 {
		return nil, ErrNotRenderable
	}
	if !colorRe.MatchString(cfg.Color) {
		return nil, fmt.Errorf("embedrender: invalid color")
	}
	color, err := strconv.ParseUint(cfg.Color[1:], 16, 32)
	if err != nil || color > 0xFFFFFF {
		return nil, fmt.Errorf("embedrender: invalid color")
	}
	emb := &discordgo.MessageEmbed{Color: int(color)}

	if cfg.Title.Enabled {
		s, _, err := substitute(cfg.Title.Template, vars, approved)
		if err != nil {
			return nil, err
		}
		emb.Title = truncate(singleLine(s), MaxTitle)
	}
	if cfg.Description.Enabled {
		s, _, err := substitute(cfg.Description.Template, vars, approved)
		if err != nil {
			return nil, err
		}
		emb.Description = truncate(s, MaxDescription)
	}

	fields := append([]embedtemplates.Field(nil), cfg.Fields...)
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].Order < fields[j].Order })
	for _, f := range fields {
		if !f.Enabled || len(emb.Fields) >= MaxFields {
			continue
		}
		name, m1, err := substitute(f.Label, vars, approved)
		if err != nil {
			return nil, err
		}
		val, m2, err := substitute(f.Template, vars, approved)
		if err != nil {
			return nil, err
		}
		if m1 || m2 || name == "" || val == "" {
			continue // optional data absent: omit the field entirely
		}
		emb.Fields = append(emb.Fields, &discordgo.MessageEmbedField{Name: truncate(singleLine(name), MaxFieldName), Value: truncate(val, MaxFieldValue), Inline: f.Inline})
	}

	if cfg.Author.Enabled {
		name, _, err := substitute(cfg.Author.Name, vars, approved)
		if err != nil {
			return nil, err
		}
		if name != "" {
			emb.Author = &discordgo.MessageEmbedAuthor{Name: truncate(singleLine(name), MaxAuthorName), IconURL: validURL(cfg.Author.IconURL)}
		}
	}
	if cfg.Footer.Enabled {
		text, _, err := substitute(cfg.Footer.Text, vars, approved)
		if err != nil {
			return nil, err
		}
		if text != "" {
			emb.Footer = &discordgo.MessageEmbedFooter{Text: truncate(text, MaxFooter), IconURL: validURL(cfg.Footer.IconURL)}
		}
	}
	if cfg.Thumbnail.Enabled {
		if u := validURL(cfg.Thumbnail.URL); u != "" {
			emb.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: u}
		}
	}
	if cfg.Image.Enabled {
		if u := validURL(cfg.Image.URL); u != "" {
			emb.Image = &discordgo.MessageEmbedImage{URL: u}
		}
	}
	if cfg.Timestamp {
		if at.IsZero() {
			at = time.Now()
		}
		emb.Timestamp = at.UTC().Format(time.RFC3339)
	}

	fitTotal(emb)
	if !renderable(emb) {
		return nil, ErrNotRenderable
	}
	return emb, nil
}

func renderable(e *discordgo.MessageEmbed) bool {
	return e.Title != "" || e.Description != "" || len(e.Fields) > 0 || e.Author != nil || e.Footer != nil || e.Image != nil || e.Thumbnail != nil
}

// TotalText is Discord's combined-size measure for an embed: title, description,
// field names and values, footer text and author name, in characters.
func TotalText(e *discordgo.MessageEmbed) int {
	n := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		n += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
	}
	if e.Footer != nil {
		n += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		n += utf8.RuneCountInString(e.Author.Name)
	}
	return n
}

// fitTotal trims the combined text to MaxTotal, deterministically and in a fixed
// order that keeps the template's structure: description first, then field values
// from the last field back, then footer, author name, and finally the title. Each cut
// ends with an ellipsis; field values keep at least one character and the title
// keeps at least one.
func fitTotal(e *discordgo.MessageEmbed) {
	excess := TotalText(e) - MaxTotal
	if excess <= 0 {
		return
	}
	cut := func(s *string, min int) {
		if excess <= 0 {
			return
		}
		have := utf8.RuneCountInString(*s)
		if have <= min {
			return
		}
		want := have - excess
		if want < min {
			want = min
		}
		*s = truncate(*s, want)
		excess -= have - utf8.RuneCountInString(*s)
	}
	cut(&e.Description, 0)
	for i := len(e.Fields) - 1; i >= 0 && excess > 0; i-- {
		cut(&e.Fields[i].Value, 1)
	}
	if e.Footer != nil {
		cut(&e.Footer.Text, 1)
	}
	if e.Author != nil {
		cut(&e.Author.Name, 1)
	}
	cut(&e.Title, 1)
	// Still over: drop trailing fields (their names count too) until it fits.
	for excess > 0 && len(e.Fields) > 0 {
		last := e.Fields[len(e.Fields)-1]
		excess -= utf8.RuneCountInString(last.Name) + utf8.RuneCountInString(last.Value)
		e.Fields = e.Fields[:len(e.Fields)-1]
	}
}

// supportedRoutes are the routes whose runtime publishers render custom templates.
// Everything else either has no publisher yet (SHOP, BUILD_FEED, ADMIN_ALERTS), is a
// persistent panel/board rather than a single-event card (BOUNTY, HEATMAPS, SERVER_STATUS,
// LINK_GAMERTAG, STATS_LEADERBOARDS, AUTO_LEADERBOARD) or is a live diagnostic monitor
// (ADMIN_LOGS): see docs/EMBED_RUNTIME.md. A saved template for such a route is stored
// but never rendered.
var supportedRoutes = []string{"KILLFEED", "HITFEED", "PVE_FEED", "BOUNTY_TRACKING", "CONNECTIONS", "ECONOMY"}

// SupportedRoutes returns the routes with runtime custom rendering (a copy).
func SupportedRoutes() []string { return append([]string(nil), supportedRoutes...) }

// RouteSupported reports whether routeKey has a runtime publisher that renders templates.
func RouteSupported(routeKey string) bool {
	for _, r := range supportedRoutes {
		if r == routeKey {
			return true
		}
	}
	return false
}
