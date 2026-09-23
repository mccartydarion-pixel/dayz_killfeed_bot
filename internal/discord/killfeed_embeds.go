package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/streaks"
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
	ColorLongRange       = presentation.Steel
	ColorExtremeRange    = presentation.EventGold
	ColorCloseRange      = presentation.CombatRed
	ColorNeutralGraphite = presentation.NeutralGraphite
)

// Range thresholds (meters) for style selection. longRangeMin defers to the
// shared presentation.LongshotDistanceMeters constant so the Discord embed,
// the story engine, and the persisted per-kill classification never drift.
const (
	closeRangeMax   = 15.0  // 0–15m
	extremeRangeMin = 200.0 // 200m+
)

const longRangeMin = presentation.LongshotDistanceMeters // 100–199.9m

// Badge strings (derived only from confirmed data). Badges are SECONDARY: the
// primary story is the card's title and is never repeated as a badge.
const (
	badgeHeadshot      = "🎯 Headshot"
	badgeCloseQuarters = "🔥 Close Range"
	badgeLongShot      = "🎯 Long Shot"
	badgeExtremeRange  = "👑 Extreme Range"
	badgeMostWanted    = "🎯 Most Wanted"
	badgeKillingSpree  = "🔥 Killing Spree"
	badgeStreakEnded   = "💀 Streak Ended"
)

// maxVisibleBadges caps the secondary badge row; more would be an emoji wall.
const maxVisibleBadges = 3

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

func isMelee(ev *killfeed.Event) bool {
	w := strings.ToLower(ev.Weapon)
	return strings.Contains(w, "fist") || strings.Contains(w, "melee")
}

// BuildPresentation delegates story priority to the shared presentation
// engine, then adds the Discord-specific style mapping and header. Title is
// the icon-prefixed event ("🎯 HEADSHOT") with no brand prefix.
func BuildPresentation(ev *killfeed.Event) KillPresentation {
	if ev == nil {
		header := presentation.BuildStoryHeader(presentation.StoryStandard)
		return KillPresentation{Style: KillEmbedStandard, Story: presentation.StoryStandard, Title: header.Title, Subtitle: header.Subtitle, Hero: header.Hero, Icon: header.Icon, AccentColor: header.Accent, Footer: presentation.ChampionSlogan}
	}
	d := distanceOf(ev)
	headshot := isHeadshot(ev)
	melee := isMelee(ev)

	// KILLING_SPREE/STREAK_ENDED are read from the persisted classification
	// (ev.KillingSpree/ev.StreakEnded, copied from the durable KillRecord) -
	// never recomputed from current player_combat_stats - and fed into the
	// existing generic milestone/streak-ended slots the story engine already
	// prioritizes, rather than a second competing story path.
	streakMilestone := 0
	if ev.KillingSpree && ev.KillerStreak != nil {
		streakMilestone = *ev.KillerStreak
	}
	victimEndedStreak, streakEndedThreshold := 0, 0
	if ev.StreakEnded && ev.EndedStreakCount != nil {
		victimEndedStreak = *ev.EndedStreakCount
		streakEndedThreshold = streaks.MeaningfulStreakThreshold
	}
	story := presentation.SelectPrimary(presentation.Context{
		Distance: ev.Distance, Headshot: headshot, Melee: melee, BountyClaimed: ev.BountyClaimed, WarKill: ev.WarBadge != "", EventBadges: ev.ActiveEventBadges,
		StreakMilestone:      streakMilestone,
		VictimEndedStreak:    victimEndedStreak,
		StreakEndedThreshold: streakEndedThreshold,
	})
	header := presentation.BuildStoryHeader(story)
	badges := secondaryBadges(ev, story, d, headshot)

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

// secondaryBadges lists confirmed secondary distinctions in priority order,
// excluding whatever the primary story already says. Presentation only: the
// story itself was selected by the shared engine.
func secondaryBadges(ev *killfeed.Event, story presentation.StoryType, d float64, headshot bool) []string {
	var badges []string
	add := func(b string) {
		for _, have := range badges {
			if have == b {
				return
			}
		}
		badges = append(badges, b)
	}
	if ev.BountyTarget && story != presentation.StoryBountyClaimed {
		add(badgeMostWanted)
	}
	if headshot && story != presentation.StoryHeadshot {
		add(badgeHeadshot)
	}
	switch {
	case d >= extremeRangeMin && story != presentation.StoryExtremeRange:
		add(badgeExtremeRange)
	case d >= longRangeMin && d < extremeRangeMin && story != presentation.StoryLongRange:
		add(badgeLongShot)
	}
	if headshot && d >= 0 && d <= closeRangeMax && story == presentation.StoryHeadshot {
		add(badgeCloseQuarters)
	}
	if ev.KillingSpree && story != presentation.StoryStreakMilestone {
		add(badgeKillingSpree)
	}
	if ev.StreakEnded && story != presentation.StoryStreakEnded {
		add(badgeStreakEnded)
	}
	for _, badge := range ev.ActiveEventBadges {
		if strings.TrimSpace(badge) != "" {
			add(badge)
		}
	}
	if ev.WarBadge != "" && story != presentation.StoryWarKill {
		add(ev.WarBadge)
	}
	if len(badges) > maxVisibleBadges {
		badges = badges[:maxVisibleBadges]
	}
	return badges
}

// Name/weapon caps used by the feed cards (runes). Discord's own limits are
// enforced by presentation.FitEmbed on every finished card.
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
func safeTrunc(s string, n int) string { return presentation.Truncate(s, n) }

// sanitizeName neutralizes mention abuse while keeping the name readable:
// @ and # are removed so names can't ping users/roles/channels, and control
// and zero-width/bidi characters are dropped. Plain text (no markdown
// escaping); cardName is the form used inside markdown-formatted cards.
func sanitizeName(s string) string { return presentation.CleanName(s, maxNameLen) }

// cardName is a player name ready for a markdown card body: cleaned, capped
// and markdown-escaped so "Semillita-azul-_" renders literally.
func cardName(p *killfeed.PlayerRef) string {
	if p == nil || strings.TrimSpace(p.Name) == "" {
		return "Unknown"
	}
	return presentation.SafeName(p.Name, presentation.MaxCardNameRunes)
}

// BuildKillEmbed renders one embed for an authoritative kill. Style is chosen by
// BuildPresentation; all publishing logic stays in the caller. One kill = one embed.
func BuildKillEmbed(ev *killfeed.Event) *discordgo.MessageEmbed {
	return BuildKillEmbedWithOptions(ev, KillEmbedOptions{LocationMode: LocationOff})
}

// BuildKillEmbedWithOptions renders the Champion V2 kill card:
//
//	author  CHAMPIONS® KILLFEED
//	title   <icon> <STORY>              ☠️ PLAYER ELIMINATED, 🎯 HEADSHOT, ...
//	desc    **Killer** → **Victim**
//	        <story line>                special stories only
//	        <weapon flavor>
//	        `WEAPON` • distance • range
//	        <up to 3 secondary badges>
//	fields  [hero metric]               DISTANCE / STREAK / REWARD (special only)
//	        KILLER | VICTIM | H2H       inline, each only when data exists
//	        FINAL HIT | LOCATION        inline, each only when data exists
//	footer  EVERY KILL TELLS A STORY    season-prefixed when known
//
// What happened (title), who (matchup), how (weapon block), why it is notable
// (story line, badges, hero), then the stats. Nothing absent is shown.
func BuildKillEmbedWithOptions(ev *killfeed.Event, options KillEmbedOptions) *discordgo.MessageEmbed {
	if ev == nil {
		return nil
	}
	p := BuildPresentation(ev)
	melee := isMelee(ev)
	killer, victim := cardName(ev.Killer), cardName(ev.Victim)
	hero := heroField(ev, p.Story)

	var desc strings.Builder
	fmt.Fprintf(&desc, "**%s** → **%s**", killer, victim)
	if line := storyLine(ev, p); line != "" {
		desc.WriteString("\n" + line)
	}
	if block := weaponBlock(ev, melee, hero != nil && hero.Name == "DISTANCE"); block != "" {
		desc.WriteString("\n\n" + block)
	}
	if len(p.Badges) > 0 {
		desc.WriteString("\n\n" + strings.Join(p.Badges, " • "))
	}

	embed := presentation.NewFeedEmbed(p.Title, p.AccentColor)
	embed.Description = safeTrunc(desc.String(), maxDescLen)
	embed.Footer.Text = safeTrunc(presentation.SeasonFooterText(ev.SeasonName), maxFooterLen)
	presentation.AppendFields(embed, hero)
	presentation.AppendFields(embed, combatStatFields(ev, p.Story, killer, victim)...)
	// A head hit is already the title or a badge; FINAL HIT would repeat it.
	if ev.HitZone != "" && !isHeadshot(ev) {
		value := presentation.EscapeMarkdown(presentation.TitleCase(presentation.CleanName(ev.HitZone, 40)))
		if ev.Damage != nil {
			value += fmt.Sprintf(" • %.1f dmg", *ev.Damage)
		}
		presentation.AppendFields(embed, presentation.MetricField("FINAL HIT", value, true))
	}
	if options.LocationMode == LocationCoordinates && ev.Killer != nil && ev.Killer.Position != nil {
		pos := ev.Killer.Position
		presentation.AppendFields(embed, presentation.MetricField("LOCATION", fmt.Sprintf("%.1f • %.1f • %.1f", pos.X, pos.Y, pos.Z), true))
	}
	presentation.StampEmbed(embed, ev.Timestamp)
	return presentation.FitEmbed(embed)
}

// storyLine is the one-line context under the matchup for special stories.
// The matchup line already names both players (the victim owned the ended
// streak), so a streak-ended card states the persisted count only when it
// exists; otherwise it falls back to the generic subtitle.
func storyLine(ev *killfeed.Event, p KillPresentation) string {
	if p.Story == presentation.StoryStandard {
		return ""
	}
	if p.Story == presentation.StoryStreakEnded && ev.EndedStreakCount != nil && *ev.EndedStreakCount > 0 {
		return fmt.Sprintf("Ended a **%d-kill streak**", *ev.EndedStreakCount)
	}
	if p.Subtitle == "" {
		return ""
	}
	return "_" + p.Subtitle + "_"
}

// weaponBlock is the "how": the weapon's flavor headline (when the shared
// classifier knows the weapon) over a compact `WEAPON` • distance • range
// strip. Distance is left out when the hero field already shows it.
func weaponBlock(ev *killfeed.Event, melee, distanceIsHero bool) string {
	var parts []string
	if ev.Weapon != "" {
		parts = append(parts, "`"+strings.ReplaceAll(safeTrunc(ev.Weapon, maxWeaponLen), "`", "'")+"`")
	}
	if ev.Distance != nil && !distanceIsHero {
		parts = append(parts, presentation.FormatDistance(*ev.Distance))
	}
	if rc := presentation.RangeClass(ev.Distance, melee); rc != "" {
		parts = append(parts, presentation.TitleCase(rc))
	}
	strip := strings.Join(parts, " • ")
	story := ""
	if ev.Weapon != "" {
		story = presentation.WeaponStory(ev.Weapon, melee)
	}
	if story == "" {
		return strip
	}
	head := "**" + story + "**"
	if icon := presentation.WeaponStoryIcon(ev.Weapon, melee); icon != "" {
		head = icon + " " + head
	}
	return head + "\n" + strip
}

// heroField is the single stand-out metric of a special story: the distance of
// a longshot, the streak of a spree, the reward of a claimed bounty.
func heroField(ev *killfeed.Event, story presentation.StoryType) *discordgo.MessageEmbedField {
	switch story {
	case presentation.StoryLongRange, presentation.StoryExtremeRange:
		if ev.Distance != nil {
			return presentation.MetricField("DISTANCE", "**"+presentation.FormatDistance(*ev.Distance)+"**", false)
		}
	case presentation.StoryStreakMilestone:
		if ev.KillerStreak != nil && *ev.KillerStreak > 0 {
			return presentation.MetricField("STREAK", fmt.Sprintf("**%d**", *ev.KillerStreak), false)
		}
	case presentation.StoryBountyClaimed:
		if ev.BountyPoints > 0 {
			return presentation.MetricField("REWARD", "**"+presentation.FormatPoints(ev.BountyPoints)+"**", false)
		}
	}
	return nil
}

// combatStatFields renders KILLER / VICTIM / H2H side by side (inline), each
// only when its source data is present - a guild without the stats/analytics
// repositories wired still gets a working card, just without these fields.
// On mobile they stack, and each still reads on its own.
func combatStatFields(ev *killfeed.Event, story presentation.StoryType, killer, victim string) []*discordgo.MessageEmbedField {
	var fields []*discordgo.MessageEmbedField
	if ev.KillerStats != nil {
		value := presentation.CompactStats(ev.KillerStats.Kills, ev.KillerStats.Deaths, ev.KillerStats.KD())
		// The spree card already shows the streak as its hero metric.
		if ev.KillerStreak != nil && *ev.KillerStreak > 0 && story != presentation.StoryStreakMilestone {
			value += fmt.Sprintf("\n🔥 Streak **%d**", *ev.KillerStreak)
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "KILLER", Value: value, Inline: true})
	}
	if ev.VictimStats != nil {
		value := presentation.CompactStats(ev.VictimStats.Kills, ev.VictimStats.Deaths, ev.VictimStats.KD())
		fields = append(fields, &discordgo.MessageEmbedField{Name: "VICTIM", Value: value, Inline: true})
	}
	if ev.Encounters != nil {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "H2H", Value: h2hValue(ev.Encounters, killer, victim), Inline: true})
	}
	return fields
}

// h2hValue: "**4–0**" plus who leads, names capped so the inline column holds.
func h2hValue(h *killfeed.HeadToHead, killer, victim string) string {
	score := fmt.Sprintf("**%d–%d**", h.KillerWins, h.VictimWins)
	switch {
	case h.KillerWins > h.VictimWins:
		return score + "\n" + shortName(killer) + " leads"
	case h.VictimWins > h.KillerWins:
		return score + "\n" + shortName(victim) + " leads"
	case h.KillerWins > 0:
		return score + "\nAll square"
	default:
		return score
	}
}

// shortName caps an already-escaped card name for narrow inline columns
// without leaving a dangling escape backslash.
func shortName(escaped string) string {
	const max = 20
	r := []rune(escaped)
	if len(r) <= max {
		return escaped
	}
	r = r[:max-1]
	if r[len(r)-1] == '\\' {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
