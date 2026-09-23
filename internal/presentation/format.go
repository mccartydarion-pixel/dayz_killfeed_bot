package presentation

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Shared value formatting for every Champion card. Pure string work: no I/O, no
// state, cheap enough for the high-frequency feeds. Raw values are never
// modified - only their display.

// FormatThousands renders n with comma separators (125000 -> "125,000").
func FormatThousands(n int64) string {
	neg := n < 0
	u := uint64(n)
	if neg {
		u = uint64(-(n + 1)) + 1 // safe for math.MinInt64
	}
	s := strconv.FormatUint(u, 10)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Plural returns "1 Kill" / "2 Kills" style counts with separators.
func Plural(n int64, singular, plural string) string {
	if n == 1 || n == -1 {
		return FormatThousands(n) + " " + singular
	}
	return FormatThousands(n) + " " + plural
}

// FormatPoints renders Champion Points: "12,500 pts", "1 pt".
func FormatPoints(n int64) string { return Plural(n, "pt", "pts") }

// FormatDistance renders meters at one decimal: "11.7m".
func FormatDistance(meters float64) string {
	return fmt.Sprintf("%.1fm", math.Round(meters*10)/10)
}

// FormatKD renders a ratio at fixed two-decimal precision. The ratio itself is
// computed by the caller (CombatRecord.KD) - only its display lives here.
func FormatKD(kd float64) string { return fmt.Sprintf("%.2f", kd) }

// CompactStats is the one-line stat strip used by kill and death cards:
// "**9 K** • **0 D** • **9.00 K/D**".
func CompactStats(kills, deaths int64, kd float64) string {
	return fmt.Sprintf("**%s K** • **%s D** • **%s K/D**", FormatThousands(kills), FormatThousands(deaths), FormatKD(kd))
}

// TitleCase turns a classifier label like "CLOSE QUARTERS" into "Close Quarters".
func TitleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// Truncate cuts s to at most n runes, ending with "…" when shortened. It never
// splits a multibyte rune.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// invisible reports zero-width and bidi-control runes that can hide or reorder
// text in a Discord client.
func invisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, // zero-width space/joiners, LRM/RLM
		r >= 0x202A && r <= 0x202E, // bidi embeddings/overrides
		r >= 0x2060 && r <= 0x2069, // word joiner, bidi isolates
		r == 0xFEFF, r == 0x00AD:
		return true
	}
	return false
}

// CleanName strips mention triggers (@, #), control characters and
// zero-width/bidi runes, collapses whitespace and caps the length. It does not
// escape markdown; use SafeName for text that is rendered inside markdown.
func CleanName(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '@' || r == '#':
			continue
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r) || invisible(r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if out == "" {
		return "Unknown"
	}
	return Truncate(out, max)
}

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`, `>`, `\>`, `[`, `\[`, `]`, `\]`,
)

// EscapeMarkdown neutralizes Discord markdown so a player name renders
// literally ("Semillita-azul-_" keeps its underscore, "**x**" is not bold).
func EscapeMarkdown(s string) string { return markdownEscaper.Replace(s) }

// SafeName is the display form of a player-controlled string inside a card:
// cleaned (no pings, no invisible runes), capped at max runes, then markdown
// escaped. Valid gamer tags render unchanged.
func SafeName(s string, max int) string { return EscapeMarkdown(CleanName(s, max)) }

// Name length caps. PSN IDs top out at 16 characters and Xbox gamertags at 15
// (+ a suffix); Steam names reach 32. Cards allow a little more; leaderboard
// rows cap at 32 so one pathological name cannot wreck a scoreboard.
const (
	MaxCardNameRunes = 48
	MaxRankNameRunes = 32
)
