package presentation

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// The wording and layout rules of a built-in card, as code. docs/DISCORD_DESIGN.md is the
// written form; CheckEmbed is what the tests run over every card in the design preview.
//
//	colour   one of the six palette colours, chosen by meaning
//	author   "Champions® <Product>" - the only place the brand appears
//	title    sentence case, no shouting, at most one emoji and only at the start
//	fields   sentence-case labels, at most one emoji and only at the start
//	footer   "Server name · short context", plain text, one line, no shouting
//
// Cards an owner designs (Embed Designer templates, the welcome card's own text and colour, a
// faction's recruitment colours) are theirs and are never checked or rewritten.

// keepUpper are short forms that stay in capitals: they are names, not shouting.
var keepUpper = map[string]bool{
	"RP": true, "XP": true, "ADM": true, "DM": true, "VIP": true, "ID": true, "UAV": true, "API": true,
	"UTC": true, "NWAF": true, "OK": true, "HP": true, "PSN": true, "URL": true, "PC": true, "UI": true,
	"K": true, "D": true, "KD": true, "CASE": true, "QA": true, "TCP": true, "FPS": true, "X": true, "Z": true,
}

// properCase are names with their own spelling.
var properCase = map[string]string{
	"PVP": "PvP", "PVE": "PvE", "DAYZ": "DayZ", "NITRADO": "Nitrado", "CHAMPION": "Champion", "CHAMPIONS": "Champions",
	"DISCORD": "Discord", "PLAYSTATION": "PlayStation", "XBOX": "Xbox", "DMS": "DMs",
}

// isShout reports whether word (letters only) is written in capitals without being a known
// short form: "KILLER" shouts, "RP" and "K" do not.
func isShout(word string) bool {
	if utf8.RuneCountInString(word) < 3 || keepUpper[word] {
		return false
	}
	for _, r := range word {
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return true
}

// shoutWords lists the shouted words of s.
func shoutWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if isShout(w) {
			out = append(out, w)
		}
	}
	return out
}

// SentenceCase rewrites a label written in capitals as a calm sentence: "PLAYER ELIMINATED"
// becomes "Player eliminated", "🎯 HEADSHOT" becomes "🎯 Headshot", "ADM STALE" becomes
// "ADM stale", "NEW #1" becomes "New #1". Words that are not in capitals (a player's name, a
// weapon like "M4-A1") are left exactly as they are, so it is safe on mixed text. Classifier
// labels stay in capitals where they are data ({{kill_type}}, {{range}} in owner templates);
// this is only how a built-in card displays them.
func SentenceCase(s string) string {
	allCaps := true
	for _, r := range s {
		if unicode.IsLower(r) {
			allCaps = false
			break
		}
	}
	var b strings.Builder
	first := true
	runes := []rune(s)
	for i := 0; i < len(runes); {
		if !unicode.IsLetter(runes[i]) {
			b.WriteRune(runes[i])
			i++
			continue
		}
		j := i
		for j < len(runes) && unicode.IsLetter(runes[j]) {
			j++
		}
		word := string(runes[i:j])
		// A word glued to a digit ("M4", "A1", "5th") is a code, not a word.
		code := (i > 0 && unicode.IsDigit(runes[i-1])) || (j < len(runes) && unicode.IsDigit(runes[j]))
		switch {
		case code:
			// unchanged
		case properCase[word] != "":
			word = properCase[word]
		case isShout(word) || (allCaps && !keepUpper[word] && word == strings.ToUpper(word)):
			word = strings.ToLower(word)
			if first {
				r := []rune(word)
				r[0] = unicode.ToUpper(r[0])
				word = string(r)
			}
		}
		first = false
		b.WriteString(word)
		i = j
	}
	return b.String()
}

// EnumLabel shows a stored code as words: "BASE_RADAR" becomes "Base radar", "RESTRICTED"
// becomes "Restricted", "selection_stale" becomes "Selection stale", "PVP" becomes "PvP".
// The stored value itself is never changed.
func EnumLabel(code string) string {
	return SentenceCase(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "_", " ")))
}

// isEmoji reports whether r is a pictograph. Arrows, dashes, the middle dot and other plain
// typography are not emoji.
func isEmoji(r rune) bool {
	if r >= 0x2798 && r <= 0x27AF && r != 0x27A1 {
		return false // dingbat arrows (➜ ➔ ➤): typography, not pictographs
	}
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, transport, flags' regional indicators
		r >= 0x2600 && r <= 0x27BF, // misc symbols and dingbats (☠ ⚔ ✅ ⚠ ✨ ...)
		r >= 0x2B00 && r <= 0x2BFF, // stars and squares (⭐ ⬆ ...)
		r >= 0x231A && r <= 0x23FF, // ⌛ ⏸ ...
		r == 0x2194, r == 0x21A9:
		return true
	}
	return false
}

// emojiJoin are the runes that glue pictographs into one emoji (variation selector, ZWJ, skin
// tones are inside the pictograph range already).
func emojiJoin(r rune) bool { return r == 0xFE0F || r == 0x200D }

// EmojiCount counts whole emoji in s (a flag or a joined sequence counts once).
func EmojiCount(s string) int {
	n, in := 0, false
	for _, r := range s {
		switch {
		case isEmoji(r):
			if !in {
				n++
			}
			in = true
		case emojiJoin(r):
			// still inside the current emoji
		default:
			in = false
		}
	}
	return n
}

// startsWithEmoji reports whether the first rune of s is a pictograph.
func startsWithEmoji(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return r != utf8.RuneError && isEmoji(r)
}

// CheckLabel returns what is wrong with a title or a field label ("" = nothing): it must not
// shout and may carry at most one emoji, at the start.
func CheckLabel(label string) string {
	if w := shoutWords(label); len(w) > 0 {
		return fmt.Sprintf("shouts %q (use sentence case)", strings.Join(w, " "))
	}
	switch n := EmojiCount(label); {
	case n > 1:
		return "has more than one emoji"
	case n == 1 && !startsWithEmoji(label):
		return "has an emoji that is not at the start"
	}
	return ""
}

// CheckFooter returns what is wrong with a footer text ("" = nothing).
func CheckFooter(text string) string {
	switch {
	case strings.TrimSpace(text) == "":
		return "is empty (leave the footer out instead)"
	case strings.Contains(text, "\n"):
		return "has more than one line"
	case strings.Contains(text, " • ") || strings.Contains(text, " | ") || strings.Contains(text, " - "):
		return `separates its parts with something other than " · "`
	case strings.Contains(text, "<t:"):
		return "has a Discord timestamp, which footers do not render (use StampEmbed)"
	case EmojiCount(text) > 0:
		return "has an emoji"
	}
	if w := shoutWords(text); len(w) > 0 {
		return fmt.Sprintf("shouts %q (use sentence case)", strings.Join(w, " "))
	}
	return ""
}

// CheckEmbed returns every way a built-in card breaks the design rules (nil = it follows them).
func CheckEmbed(e *discordgo.MessageEmbed) []string {
	if e == nil {
		return nil
	}
	var out []string
	if !InPalette(e.Color) {
		out = append(out, fmt.Sprintf("colour #%06X is not one of the six palette colours", e.Color))
	}
	if e.Author != nil {
		if !strings.HasPrefix(e.Author.Name, BrandName) {
			out = append(out, fmt.Sprintf("author %q does not start with %s", e.Author.Name, BrandName))
		}
		if w := shoutWords(strings.ReplaceAll(e.Author.Name, "C.A.S.E.", "")); len(w) > 0 {
			out = append(out, fmt.Sprintf("author %q shouts", e.Author.Name))
		}
	}
	if e.Title != "" {
		if why := CheckLabel(e.Title); why != "" {
			out = append(out, fmt.Sprintf("title %q %s", e.Title, why))
		}
	}
	for _, f := range e.Fields {
		if f == nil {
			continue
		}
		if why := CheckLabel(f.Name); why != "" {
			out = append(out, fmt.Sprintf("field label %q %s", f.Name, why))
		}
	}
	if e.Footer != nil {
		if why := CheckFooter(e.Footer.Text); why != "" {
			out = append(out, fmt.Sprintf("footer %q %s", e.Footer.Text, why))
		}
	}
	return out
}

// properNouns keep their capitals inside a sentence-case label: product and feature names,
// and names with their own spelling.
var properNouns = []string{
	"Champions®", "Champion Points", "Champion Card", "Champion", "Player Hub", "Security Store", "Base Raid Alarm", "Raid Alarm",
	"Perimeter Watch", "Base Black Box", "Black Box", "Faction Security", "Sentinel Pro", "Server of the Week", "Client Hub",
	"Nitrado", "PlayStation", "Xbox", "Discord", "DayZ", "Ranked", "PvP", "PvE", "Unranked", "Killfeed",
}

// TitleCaseWords lists the words of a label that are capitalised without being its first word
// or a known name - the mark of Title Case ("Channel Summary") where sentence case ("Channel
// summary") is the rule. also names further words that are data in this label (a player, a
// server, an event), so a test can pass its sample names.
func TitleCaseWords(label string, also ...string) []string {
	label = strings.ReplaceAll(label, `\`, "") // markdown escapes inside a safe name
	for _, name := range append(append([]string{}, also...), properNouns...) {
		if name != "" {
			label = strings.ReplaceAll(label, name, " ")
		}
	}
	var out []string
	first := true
	for _, word := range strings.FieldsFunc(label, func(r rune) bool { return unicode.IsSpace(r) || r == '/' }) {
		letters := strings.TrimFunc(word, func(r rune) bool { return !unicode.IsLetter(r) })
		if letters == "" {
			continue
		}
		// A new sentence or clause may start with a capital.
		if first {
			first = false
			continue
		}
		r := []rune(letters)
		if len(r) > 1 && unicode.IsUpper(r[0]) && unicode.IsLower(r[1]) {
			out = append(out, letters)
		}
		if strings.HasSuffix(word, ":") || strings.HasSuffix(word, ".") || strings.HasSuffix(word, "?") {
			first = true
		}
	}
	return out
}
