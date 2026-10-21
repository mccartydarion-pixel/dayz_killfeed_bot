package presentation

import (
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The Champion palette: six colours, each with one meaning. Colour is chosen by what a
// message MEANS, never by which feature or command sent it. The values match the website's
// design tokens (crimson button, gold highlight, success green, danger red, graphite).
// docs/DISCORD_DESIGN.md is the written form of these rules; TestDesignRules enforces them.
const (
	Crimson = 0xD60F1D // brand: kills and combat, primary announcements, calls to action
	Gold    = 0xF0C65E // highlights: records, rewards, winners, leaderboards
	Green   = 0x38C987 // success and healthy: confirmed, recovered, online, joined
	Amber   = 0xE08A1E // warnings: needs attention soon, degraded, expiring
	Red     = 0xFF3344 // errors and danger: failed, critical, under attack, declined
	Neutral = 0x8D8D96 // information: routine status, diagnostics, quiet events
)

// The names older builders use. They are kept so call sites read by intent ("EventGold",
// "CombatRed") but every one resolves to a palette colour above - there is no seventh colour.
const (
	ChampionGold    = Gold
	EventGold       = Gold
	FactionGold     = Gold
	CombatRed       = Crimson
	SuccessGreen    = Green
	WarningAmber    = Amber
	ErrorRed        = Red
	InfoSteel       = Neutral
	NeutralGraphite = Neutral
)

// Steel is the design-system name for InfoSteel.
const Steel = InfoSteel

// Palette returns the six colours in a fixed order.
func Palette() []int { return []int{Crimson, Gold, Green, Amber, Red, Neutral} }

// PaletteName names a palette colour ("" when c is not one).
func PaletteName(c int) string {
	switch c {
	case Crimson:
		return "crimson"
	case Gold:
		return "gold"
	case Green:
		return "green"
	case Amber:
		return "amber"
	case Red:
		return "red"
	case Neutral:
		return "neutral"
	}
	return ""
}

// InPalette reports whether c is one of the six colours.
func InPalette(c int) bool { return PaletteName(c) != "" }

// Brand hierarchy: the author line carries the brand exactly once, the title is the event,
// and the footer says where it happened. Nothing else repeats the brand, and nothing shouts.
const (
	// BrandName starts every author line.
	BrandName  = "Champions®"
	AuthorName = BrandName + " Killfeed"

	// ChampionSlogan is the footer of a kill or death card when there is no season to name.
	ChampionSlogan = "Every kill tells a story"

	// Footer contexts shared by more than one card.
	FooterAutoRefresh = "Updates automatically"
	FooterLiveIntel   = "Live server status"
	FooterStaffOnly   = "Staff only"

	// footerSeparator joins the parts of a footer and of an author line.
	footerSeparator = " · "
)

// ChampionAuthor is the author block every branded card uses.
func ChampionAuthor() *discordgo.MessageEmbedAuthor {
	return &discordgo.MessageEmbedAuthor{Name: AuthorName}
}

// BrandAuthor is the author line of a card that belongs to one named part of the product:
// "Champions® Battle Pass", "Champions® Ranked". An empty product gives the plain brand line.
func BrandAuthor(product string) *discordgo.MessageEmbedAuthor {
	product = strings.TrimSpace(product)
	if product == "" {
		return ChampionAuthor()
	}
	return &discordgo.MessageEmbedAuthor{Name: BrandName + " " + product}
}

// FooterText is the one footer format: "Server name · short context". Either part may be
// empty; both empty gives "". The server name is stored data, so it is cleaned (no pings, no
// control characters). Discord shows footers as plain text: no markdown, no <t:...> times.
func FooterText(server, context string) string {
	server, context = strings.TrimSpace(server), strings.TrimSpace(context)
	if server != "" {
		server = CleanName(server, 60)
	}
	switch {
	case server != "" && context != "":
		return server + footerSeparator + context
	case server != "":
		return server
	default:
		return context
	}
}

// Footer is FooterText as an embed footer, or nil when there is nothing to say.
func Footer(server, context string) *discordgo.MessageEmbedFooter {
	text := FooterText(server, context)
	if text == "" {
		return nil
	}
	return &discordgo.MessageEmbedFooter{Text: text}
}

// SeasonFooterText is the footer context of a kill, death or result card: the season when one
// is known, otherwise the slogan. The season name is stored data, so it is cleaned.
func SeasonFooterText(season string) string {
	if strings.TrimSpace(season) == "" {
		return ChampionSlogan
	}
	return CleanName(season, 60)
}

// SeasonFooter is SeasonFooterText as an embed footer.
func SeasonFooter(season string) *discordgo.MessageEmbedFooter {
	return Footer("", SeasonFooterText(season))
}

// NewFeedEmbed starts a card: brand author, event title, palette colour. It has no footer:
// a builder adds one with Footer when it has a server or a context to name.
func NewFeedEmbed(title string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Author: ChampionAuthor(),
		Title:  title,
		Color:  color,
	}
}

// NewChampionEmbed establishes the common hierarchy for panels and command
// responses: the brand in the author line, the section as the title.
func NewChampionEmbed(section string, color int) *discordgo.MessageEmbed {
	return NewFeedEmbed(section, color)
}

// ChampionFooter is the slogan footer.
func ChampionFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: ChampionSlogan}
}

// UpdatedFooter is the footer of a live panel. The refresh time belongs in
// the embed timestamp (see StampEmbed): Discord does not render <t:...>
// markup inside footers, so it is never put there.
func UpdatedFooter(time.Time) *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: FooterLiveIntel}
}

// AutoRefreshFooter is the footer of a board that is edited on a schedule.
func AutoRefreshFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: FooterAutoRefresh}
}

// Timestamp is Discord's own time markup for text that Discord renders (descriptions and
// field values, not footers or field names). Use it wherever a builder has a time: every
// reader then sees it in their own time zone. style is one of Discord's letters:
// 'R' relative ("in 2 hours"), 'f' date and time, 'F' long date and time, 'D' date, 't' time.
func Timestamp(at time.Time, style byte) string {
	return "<t:" + strconv.FormatInt(at.Unix(), 10) + ":" + string(style) + ">"
}

// TimestampWithRelative is "long date and time (relative)": how a scheduled moment is named
// (an event start, a season change), weekday included.
func TimestampWithRelative(at time.Time) string {
	return Timestamp(at, 'F') + " (" + Timestamp(at, 'R') + ")"
}

// StampEmbed sets Discord's native embed timestamp from an authoritative time.
// A zero time leaves the timestamp unset.
func StampEmbed(e *discordgo.MessageEmbed, at time.Time) {
	if e == nil || at.IsZero() {
		return
	}
	e.Timestamp = at.UTC().Format("2006-01-02T15:04:05.000Z")
}

func StatusField(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
}
