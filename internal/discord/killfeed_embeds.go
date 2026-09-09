package discord

import (
	"fmt"
	"math"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

// KillEmbedStyle selects the visual presentation for a kill.
type KillEmbedStyle string

const (
	KillEmbedStandard     KillEmbedStyle = "STANDARD"
	KillEmbedHeadshot     KillEmbedStyle = "HEADSHOT"
	KillEmbedLongRange    KillEmbedStyle = "LONG_RANGE"
	KillEmbedExtremeRange KillEmbedStyle = "EXTREME_RANGE"
	KillEmbedCloseRange   KillEmbedStyle = "CLOSE_RANGE"
)

// Champion brand color palette. Named constants keep branding consistent; no
// random per-message colors.
const (
	ColorChampionGold = 0xC9A227 // STANDARD: dark gold / amber
	ColorHeadshotRed  = 0x8B0000 // HEADSHOT: deep red
	ColorLongRange    = 0x4682B4 // LONG_RANGE: steel blue
	ColorExtremeRange = 0xB8860B // EXTREME_RANGE: royal gold accent
	ColorCloseRange   = 0xE67E22 // CLOSE_RANGE: aggressive orange
)

// Range thresholds (meters) for style selection.
const (
	closeRangeMax   = 15.0  // 0–15m
	longRangeMin    = 100.0 // 100–199.9m
	extremeRangeMin = 200.0 // 200m+
)

// Badge strings (derived only from confirmed data).
const (
	badgeHeadshot      = "🎯 Headshot"
	badgeCloseQuarters = "🔥 Close Range"
	badgeLongShot      = "🎯 Long Shot"
	badgeExtremeRange  = "👑 Extreme Range"
)

// KillPresentation is the style decision, computed before rendering. Keeping it
// separate makes the model extensible for future badges (streaks, revenge, etc.).
type KillPresentation struct {
	Style       KillEmbedStyle
	Badges      []string
	Title       string
	AccentColor int
	Footer      string
}

// isHeadshot reports whether confirmed data shows a head hit. Headshot is only
// claimed when the hit-zone data actually says Head; never inferred.
func isHeadshot(ev *killfeed.Event) bool {
	return ev != nil && strings.EqualFold(strings.TrimSpace(ev.HitZone), "Head")
}

// distance returns the kill distance in meters, or -1 if unknown.
func distanceOf(ev *killfeed.Event) float64 {
	if ev != nil && ev.Distance != nil {
		return *ev.Distance
	}
	return -1
}

// BuildPresentation chooses the embed style with deterministic priority:
// EXTREME_RANGE > HEADSHOT > LONG_RANGE > CLOSE_RANGE > STANDARD.
func BuildPresentation(ev *killfeed.Event) KillPresentation {
	d := distanceOf(ev)
	headshot := isHeadshot(ev)

	badges := []string{}
	if headshot {
		badges = append(badges, badgeHeadshot)
	}

	switch {
	case d >= extremeRangeMin:
		badges = append(badges, badgeExtremeRange)
		return KillPresentation{
			Style:       KillEmbedExtremeRange,
			Badges:      badges,
			Title:       "👑 CHAMPION • EXTREME RANGE",
			AccentColor: ColorExtremeRange,
			Footer:      "DISTANCE DOMINANCE • CHAMPION",
		}
	case headshot:
		if d >= 0 && d <= closeRangeMax {
			badges = append(badges, badgeCloseQuarters)
		}
		return KillPresentation{
			Style:       KillEmbedHeadshot,
			Badges:      badges,
			Title:       "🎯 CHAMPION • HEADSHOT",
			AccentColor: ColorHeadshotRed,
			Footer:      "PRECISION ELIMINATION • CHAMPION KILLFEED",
		}
	case d >= longRangeMin && d < extremeRangeMin:
		badges = append(badges, badgeLongShot)
		return KillPresentation{
			Style:       KillEmbedLongRange,
			Badges:      badges,
			Title:       "🎯 CHAMPION • LONG SHOT",
			AccentColor: ColorLongRange,
			Footer:      "LONG RANGE ELIMINATION",
		}
	case d >= 0 && d <= closeRangeMax:
		badges = append(badges, badgeCloseQuarters)
		return KillPresentation{
			Style:       KillEmbedCloseRange,
			Badges:      badges,
			Title:       "🔥 CHAMPION • CLOSE QUARTERS",
			AccentColor: ColorCloseRange,
			Footer:      "POINT-BLANK ELIMINATION",
		}
	default:
		return KillPresentation{
			Style:       KillEmbedStandard,
			Badges:      badges,
			Title:       "🏆 CHAMPION KILLFEED",
			AccentColor: ColorChampionGold,
			Footer:      "CHAMPION • EVERY KILL TELLS A STORY",
		}
	}
}

// Discord embed limits we guard against.
const (
	maxNameLen   = 80
	maxWeaponLen = 80
	maxDescLen   = 900
	maxFooterLen = 200
)

// safeTrunc truncates a string to n runes without splitting multibyte runes.
func safeTrunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// sanitizeName neutralizes mention/markdown abuse while keeping the name readable.
// Removes @ and # so names can't ping users/roles, and trims control chars.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "@", "") // block @everyone/@here/user pings
	s = strings.ReplaceAll(s, "#", "") // block channel mentions
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 && r != ' ' { // strip control characters
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		out = "Unknown"
	}
	return safeTrunc(out, maxNameLen)
}

// BuildKillEmbed renders one embed for an authoritative kill. Style is chosen by
// BuildPresentation; all publishing logic stays in the caller. One kill = one embed.
func BuildKillEmbed(ev *killfeed.Event) *discordgo.MessageEmbed {
	if ev == nil {
		return nil
	}
	p := BuildPresentation(ev)

	killer := "Unknown"
	if ev.Killer != nil && ev.Killer.Name != "" {
		killer = sanitizeName(ev.Killer.Name)
	}
	victim := "Unknown"
	if ev.Victim != nil && ev.Victim.Name != "" {
		victim = sanitizeName(ev.Victim.Name)
	}

	// Killer dominates; victim secondary. Badge line is optional and compact.
	var desc strings.Builder
	fmt.Fprintf(&desc, "⚔️ **KILLER**  %s\n\n💀 **VICTIM**  %s", killer, victim)
	if len(p.Badges) > 0 {
		fmt.Fprintf(&desc, "\n\n%s", strings.Join(p.Badges, "  "))
	}

	fields := []*discordgo.MessageEmbedField{}
	if ev.Weapon != "" {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "🔫 Weapon",
			Value:  safeTrunc(ev.Weapon, maxWeaponLen),
			Inline: true,
		})
	}
	if ev.Distance != nil {
		rounded := math.Round(*ev.Distance*10) / 10
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   "📏 Distance",
			Value:  fmt.Sprintf("%.1fm", rounded),
			Inline: true,
		})
	}

	embed := &discordgo.MessageEmbed{
		Title:       p.Title,
		Description: safeTrunc(desc.String(), maxDescLen),
		Color:       p.AccentColor,
		Fields:      fields,
		Author: &discordgo.MessageEmbedAuthor{
			Name: "🏆 CHAMPION KILLFEED",
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: safeTrunc(p.Footer, maxFooterLen),
		},
	}

	// Only set the embed timestamp when a valid absolute event time exists.
	if !ev.Timestamp.IsZero() {
		embed.Timestamp = ev.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return embed
}
