package discord

import (
	"fmt"
	"math"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// KillEmbedStyle selects the visual presentation for a kill.
type KillEmbedStyle string

const (
	KillEmbedStandard     KillEmbedStyle = "STANDARD"
	KillEmbedHeadshot     KillEmbedStyle = "HEADSHOT"
	KillEmbedLongRange    KillEmbedStyle = "LONG_RANGE"
	KillEmbedExtremeRange KillEmbedStyle = "EXTREME_RANGE"
	KillEmbedCloseRange   KillEmbedStyle = "CLOSE_RANGE"
	KillEmbedBountyClaim  KillEmbedStyle = "BOUNTY_CLAIM"
)

// Champion brand color palette. Named constants keep branding consistent; no
// random per-message colors.
const (
	ColorChampionGold    = presentation.ChampionGold
	ColorDangerRed       = presentation.ErrorRed
	ColorSuccessGreen    = presentation.SuccessGreen
	ColorInfoBlue        = presentation.InfoSteel
	ColorWarningOrange   = presentation.WarningAmber
	ColorHeadshotRed     = presentation.CombatRed
	ColorLongRange       = presentation.InfoSteel
	ColorExtremeRange    = presentation.EventGold
	ColorCloseRange      = presentation.CombatRed
	ColorNeutralGraphite = presentation.NeutralGraphite
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
	badgeMostWanted    = "🎯 Most Wanted"
)

// KillPresentation is the style decision, computed before rendering. Keeping it
// separate makes the model extensible for future badges (streaks, revenge, etc.).
type KillPresentation struct {
	Style       KillEmbedStyle
	Story       presentation.StoryType
	Badges      []string
	Title       string
	Subtitle    string
	Hero        string
	Icon        string
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

// BuildPresentation delegates story priority to the shared presentation
// engine, then adds the Discord-specific style mapping and header.
func BuildPresentation(ev *killfeed.Event) KillPresentation {
	if ev == nil {
		header := presentation.BuildStoryHeader(presentation.StoryStandard)
		return KillPresentation{Style: KillEmbedStandard, Story: presentation.StoryStandard, Title: header.Title, Hero: header.Hero, Icon: header.Icon, AccentColor: header.Accent, Footer: presentation.ChampionSlogan}
	}
	d := distanceOf(ev)
	headshot := isHeadshot(ev)
	melee := strings.Contains(strings.ToLower(ev.Weapon), "fist") || strings.Contains(strings.ToLower(ev.Weapon), "melee")
	story := presentation.SelectPrimary(presentation.Context{Distance: ev.Distance, Headshot: headshot, Melee: melee, BountyClaimed: ev.BountyClaimed, WarKill: ev.WarBadge != "", EventBadges: ev.ActiveEventBadges})
	header := presentation.BuildStoryHeader(story)

	badges := []string{}
	if headshot {
		badges = append(badges, badgeHeadshot)
	}
	if ev != nil {
		if ev.BountyTarget {
			badges = append(badges, badgeMostWanted)
		}
		for _, badge := range ev.ActiveEventBadges {
			if len(badges) >= 3 {
				break
			}
			badges = append(badges, badge)
		}
		if ev.WarBadge != "" && len(badges) < 3 {
			badges = append(badges, ev.WarBadge)
		}
	}
	if d >= extremeRangeMin && story != presentation.StoryExtremeRange {
		badges = append(badges, badgeExtremeRange)
	} else if headshot && story != presentation.StoryHeadshot {
		badges = append(badges, badgeHeadshot)
	} else if d >= longRangeMin && story != presentation.StoryLongRange {
		badges = append(badges, badgeLongShot)
	} else if melee && story != presentation.StoryMelee {
		badges = append(badges, badgeCloseQuarters)
	}
	if headshot && d >= 0 && d <= closeRangeMax && story == presentation.StoryHeadshot {
		badges = append(badges, badgeCloseQuarters)
	}
	if len(badges) > 5 {
		badges = badges[:5]
	}
	style := KillEmbedStandard
	switch story {
	case presentation.StoryBountyClaimed:
		style = KillEmbedBountyClaim
	case presentation.StoryMelee:
		style = KillEmbedCloseRange
	case presentation.StoryHeadshot:
		style = KillEmbedHeadshot
	case presentation.StoryLongRange:
		style = KillEmbedLongRange
	case presentation.StoryExtremeRange:
		style = KillEmbedExtremeRange
	}
	return KillPresentation{Style: style, Story: story, Badges: badges, Title: header.Title, Subtitle: header.Subtitle, Hero: header.Hero, Icon: header.Icon, AccentColor: header.Accent, Footer: presentation.ChampionSlogan}
}

// Discord embed limits we guard against.
const (
	maxNameLen   = 80
	maxWeaponLen = 80
	maxDescLen   = 1800
	maxFooterLen = 200
)

type LocationMode string

const (
	LocationOff         LocationMode = "OFF"
	LocationZoneOnly    LocationMode = "ZONE_ONLY"
	LocationCoordinates LocationMode = "COORDINATES"
)

type KillEmbedOptions struct {
	LocationMode LocationMode
}

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
	return BuildKillEmbedWithOptions(ev, KillEmbedOptions{LocationMode: LocationOff})
}

func BuildKillEmbedWithOptions(ev *killfeed.Event, options KillEmbedOptions) *discordgo.MessageEmbed {
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

	melee := strings.Contains(strings.ToLower(ev.Weapon), "fist") || strings.Contains(strings.ToLower(ev.Weapon), "melee")
	// The description is intentionally the stable top half of every kill card.
	var desc strings.Builder
	fmt.Fprintf(&desc, "%s  ➜  %s", killer, victim)
	if p.Hero != "" && p.Story != presentation.StoryStandard {
		fmt.Fprintf(&desc, "\n\n%s **%s**", p.Icon, p.Hero)
		if p.Subtitle != "" {
			fmt.Fprintf(&desc, "\n%s", p.Subtitle)
		}
	}
	if ev.Weapon != "" {
		if story := presentation.WeaponStory(ev.Weapon, melee); story != "" {
			fmt.Fprintf(&desc, "\n\n%s\n%s", story, safeTrunc(ev.Weapon, maxWeaponLen))
		} else {
			fmt.Fprintf(&desc, "\n\n%s", safeTrunc(ev.Weapon, maxWeaponLen))
		}
	}
	if len(p.Badges) > 0 {
		fmt.Fprintf(&desc, "\n\n%s", strings.Join(p.Badges, "  "))
	}

	fields := []*discordgo.MessageEmbedField{}
	if p.Story != presentation.StoryStandard && p.Hero != "" {
		if hero := heroMetric(ev, p.Story); hero != "" {
			fields = append(fields, &discordgo.MessageEmbedField{Name: p.Hero, Value: hero, Inline: false})
		}
	}
	details := make([]string, 0, 6)
	if ev.Distance != nil {
		details = append(details, fmt.Sprintf("**Distance**  %.1fm", math.Round(*ev.Distance*10)/10))
	}
	if rangeClass := presentation.RangeClass(ev.Distance, melee); rangeClass != "" {
		details = append(details, "**Range**  "+rangeClass)
	}
	if ev.HitZone != "" {
		details = append(details, "**Final Hit**  "+safeTrunc(strings.ToUpper(ev.HitZone), 40))
	}
	if ev.Damage != nil {
		details = append(details, fmt.Sprintf("**Final Hit Damage**  %.1f", *ev.Damage))
	}
	if ev.Ammo != "" {
		details = append(details, "**Ammo**  "+safeTrunc(ev.Ammo, maxWeaponLen))
	}
	if options.LocationMode == LocationCoordinates && ev.Killer != nil && ev.Killer.Position != nil {
		pos := ev.Killer.Position
		details = append(details, fmt.Sprintf("**Location**  %.1f • %.1f • %.1f", pos.X, pos.Y, pos.Z))
	}
	if len(details) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "KILL DETAILS", Value: safeTrunc(strings.Join(details, "\n"), 1000), Inline: false})
	}

	footer := p.Footer
	if ev.SeasonName != "" {
		footer = fmt.Sprintf("CHAMPION KILLFEED • %s", sanitizeName(ev.SeasonName))
	}
	embed := &discordgo.MessageEmbed{
		Title:       p.Title,
		Description: safeTrunc(desc.String(), maxDescLen),
		Color:       p.AccentColor,
		Fields:      fields,
		Author: &discordgo.MessageEmbedAuthor{
			Name: "CHAMPION KILLFEED",
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: safeTrunc(footer, maxFooterLen),
		},
	}

	// Only set the embed timestamp when a valid absolute event time exists.
	if !ev.Timestamp.IsZero() {
		embed.Timestamp = ev.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return embed
}

func heroMetric(ev *killfeed.Event, story presentation.StoryType) string {
	if ev == nil || ev.Distance == nil {
		return ""
	}
	distance := fmt.Sprintf("%.1fm", math.Round(*ev.Distance*10)/10)
	switch story {
	case presentation.StoryLongRange, presentation.StoryExtremeRange:
		return distance
	default:
		return ""
	}
}
