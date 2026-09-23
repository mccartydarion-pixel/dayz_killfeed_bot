package presentation

import (
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Champion's restrained semantic palette. Keep color selection attached to
// meaning rather than to individual commands or message authors.
const (
	ChampionGold    = 0xC9A227 // brand accent: standard kills, leaderboards
	CombatRed       = 0x8B0000 // decisive combat: headshots, streak events
	SuccessGreen    = 0x3F8F68
	WarningAmber    = 0xB7791F // suicides, cautionary economy/bounty states
	InfoSteel       = 0x4682B4 // steel: long range, operational panels
	NeutralGraphite = 0x2B2F33 // quiet cards: ordinary deaths
	ErrorRed        = 0xB83232
	FactionGold     = 0xA9822B
	EventGold       = 0xD4AF37 // rare moments: extreme range, bounty claims
)

// Steel is the design-system name for InfoSteel.
const Steel = InfoSteel

// Brand hierarchy: the author line carries the brand exactly once, the title
// is the event, and the footer is the slogan. Nothing else repeats the brand.
const (
	AuthorName = "CHAMPIONS® KILLFEED"

	ChampionSlogan    = "EVERY KILL TELLS A STORY"
	FooterAutoRefresh = "CHAMPION • AUTO-REFRESH"
	FooterLiveIntel   = "CHAMPION • LIVE SERVER INTELLIGENCE"
)

// ChampionAuthor is the author block every branded card uses.
func ChampionAuthor() *discordgo.MessageEmbedAuthor {
	return &discordgo.MessageEmbedAuthor{Name: AuthorName}
}

// SeasonFooterText is the slogan footer, prefixed with the season when one is
// known: "CHAMPION • Season 3 • EVERY KILL TELLS A STORY". The season name is
// stored data, so it is cleaned (no pings, no control characters).
func SeasonFooterText(season string) string {
	season = strings.TrimSpace(season)
	if season == "" {
		return ChampionSlogan
	}
	return "CHAMPION • " + CleanName(season, 60) + " • " + ChampionSlogan
}

// NewFeedEmbed starts a feed card: brand author, event title, slogan footer.
func NewFeedEmbed(title string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Author: ChampionAuthor(),
		Title:  title,
		Color:  color,
		Footer: ChampionFooter(),
	}
}

// NewChampionEmbed establishes the common hierarchy for panels and command
// responses: the brand in the author line, the section as the title.
func NewChampionEmbed(section string, color int) *discordgo.MessageEmbed {
	return NewFeedEmbed(section, color)
}

func ChampionFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: ChampionSlogan}
}

// UpdatedFooter is the operational-panel footer. The refresh time belongs in
// the embed timestamp (see StampEmbed): Discord does not render <t:...>
// markup inside footers, so it is never put there.
func UpdatedFooter(time.Time) *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: FooterLiveIntel}
}

func AutoRefreshFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: FooterAutoRefresh}
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
