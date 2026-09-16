package presentation

import "strings"

// StoryHeader is the immutable visual decision used by the Discord renderer.
type StoryHeader struct {
	Title    string
	Subtitle string
	Accent   int
	Hero     string
	Icon     string
}

func BuildStoryHeader(story StoryType) StoryHeader {
	header := buildStoryHeader(story)
	header.Title = header.Icon + " " + header.Title
	return header
}

func buildStoryHeader(story StoryType) StoryHeader {
	const brand = "CHAMPION • "
	switch story {
	case StoryMelee:
		return StoryHeader{Title: brand + "CLOSE QUARTERS", Accent: FactionGold, Hero: "CLOSE QUARTERS", Icon: "🥊"}
	case StoryHeadshot:
		return StoryHeader{Title: brand + "HEADSHOT", Accent: CombatRed, Subtitle: "PRECISION ELIMINATION", Hero: "HEADSHOT CONFIRMED", Icon: "🎯"}
	case StoryLongRange:
		return StoryHeader{Title: brand + "LONG RANGE ELIMINATION", Accent: InfoSteel, Hero: "LONG RANGE SHOT", Icon: "🎯"}
	case StoryExtremeRange:
		return StoryHeader{Title: brand + "EXTREME RANGE", Accent: EventGold, Hero: "EXTREME RANGE ELIMINATION", Icon: "🏆"}
	case StoryPersonalRecord:
		return StoryHeader{Title: brand + "NEW PERSONAL RECORD", Accent: ChampionGold, Hero: "PERSONAL RECORD BROKEN", Icon: "📈"}
	case StoryServerRecord:
		return StoryHeader{Title: brand + "NEW SERVER RECORD", Accent: EventGold, Hero: "NEW SERVER RECORD", Icon: "👑"}
	case StoryStreakMilestone:
		return StoryHeader{Title: brand + "KILL STREAK", Accent: CombatRed, Hero: "STREAK MILESTONE", Icon: "🔥"}
	case StoryStreakEnded:
		return StoryHeader{Title: brand + "STREAK ENDED", Accent: CombatRed, Hero: "STREAK ENDED", Icon: "☠️"}
	case StoryRevenge:
		return StoryHeader{Title: brand + "REVENGE", Accent: CombatRed, Hero: "REVENGE SECURED", Icon: "⚔️"}
	case StoryNemesis:
		return StoryHeader{Title: brand + "NEMESIS ELIMINATED", Accent: CombatRed, Hero: "NEMESIS DOWN", Icon: "⚔️"}
	case StoryBountyClaimed:
		return StoryHeader{Title: brand + "BOUNTY CLAIMED", Accent: EventGold, Hero: "BOUNTY CLAIMED", Icon: "🎯"}
	case StoryWarKill:
		return StoryHeader{Title: brand + "WAR KILL", Accent: CombatRed, Hero: "FACTION WAR", Icon: "⚔️"}
	case StoryWarLeadChange:
		return StoryHeader{Title: brand + "WAR LEAD CHANGE", Accent: EventGold, Hero: "WAR LEAD CHANGE", Icon: "⚔️"}
	case StoryWarTie:
		return StoryHeader{Title: brand + "WAR TIED", Accent: CombatRed, Hero: "ALL SQUARE", Icon: "⚔️"}
	case StoryFirstBlood:
		return StoryHeader{Title: brand + "FIRST BLOOD", Accent: CombatRed, Hero: "FIRST BLOOD", Icon: "🩸"}
	case StoryEventKill:
		return StoryHeader{Title: brand + "EVENT KILL", Accent: EventGold, Hero: "COMPETITIVE EVENT", Icon: "🏆"}
	case StoryEventLeadChange:
		return StoryHeader{Title: brand + "EVENT LEAD CHANGE", Accent: EventGold, Hero: "NEW EVENT LEADER", Icon: "🏆"}
	case StoryRankPromotion:
		return StoryHeader{Title: brand + "RANK PROMOTION", Accent: ChampionGold, Hero: "PROMOTED", Icon: "🎖️"}
	case StoryLeaderboardTakeover:
		return StoryHeader{Title: brand + "NEW #1", Accent: EventGold, Hero: "LEADERBOARD TAKEOVER", Icon: "👑"}
	case StoryRapidKill:
		return StoryHeader{Title: brand + "MULTI-KILL", Accent: CombatRed, Hero: "MULTI-KILL", Icon: "💀"}
	default:
		return StoryHeader{Title: brand + "PLAYER ELIMINATED", Accent: NeutralGraphite, Hero: "COMBAT REPORT", Icon: "💀"}
	}
}

func RangeClass(distance *float64, melee bool) string {
	if melee {
		return "CLOSE QUARTERS"
	}
	if distance == nil {
		return ""
	}
	switch {
	case *distance >= 200:
		return "EXTREME RANGE"
	case *distance >= LongshotDistanceMeters:
		return "LONG RANGE"
	case *distance < 15:
		return "CLOSE QUARTERS"
	default:
		return "MID RANGE"
	}
}

func WeaponStory(weapon string, melee bool) string {
	if melee {
		return "Straight One-Two"
	}
	lower := strings.ToLower(weapon)
	switch {
	case strings.Contains(lower, "shotgun"), strings.Contains(lower, "bk-43"):
		return "Point Blank"
	case strings.Contains(lower, "mosin"), strings.Contains(lower, "m70"), strings.Contains(lower, "svd"), strings.Contains(lower, "tundra"):
		return "Dead Eye"
	case strings.Contains(lower, "m4"), strings.Contains(lower, "ak-"), strings.Contains(lower, "ka-m"):
		return "Controlled Burst"
	case strings.Contains(lower, "glock"), strings.Contains(lower, "cz75"), strings.Contains(lower, "deagle"), strings.Contains(lower, "pistol"):
		return "Sidearm Finish"
	default:
		return ""
	}
}

// WeaponStoryIcon pairs an icon with WeaponStory's flavor text, using the
// same classification. Empty when WeaponStory has no flavor text either.
func WeaponStoryIcon(weapon string, melee bool) string {
	if melee {
		return "🥊"
	}
	lower := strings.ToLower(weapon)
	switch {
	case strings.Contains(lower, "shotgun"), strings.Contains(lower, "bk-43"):
		return "💥"
	case strings.Contains(lower, "mosin"), strings.Contains(lower, "m70"), strings.Contains(lower, "svd"), strings.Contains(lower, "tundra"):
		return "🎯"
	case strings.Contains(lower, "m4"), strings.Contains(lower, "ak-"), strings.Contains(lower, "ka-m"):
		return "🔫"
	case strings.Contains(lower, "glock"), strings.Contains(lower, "cz75"), strings.Contains(lower, "deagle"), strings.Contains(lower, "pistol"):
		return "🔫"
	default:
		return ""
	}
}

// WeaponCategory labels the weapon's class using the same classification as
// WeaponStory, for display as "{category} • {weapon}". Empty when the
// weapon doesn't match a known bucket - never fabricate a category.
func WeaponCategory(weapon string, melee bool) string {
	if melee {
		return "Fists"
	}
	lower := strings.ToLower(weapon)
	switch {
	case strings.Contains(lower, "shotgun"), strings.Contains(lower, "bk-43"):
		return "Shotgun"
	case strings.Contains(lower, "mosin"), strings.Contains(lower, "m70"), strings.Contains(lower, "svd"), strings.Contains(lower, "tundra"):
		return "Sniper Rifle"
	case strings.Contains(lower, "m4"), strings.Contains(lower, "ak-"), strings.Contains(lower, "ka-m"):
		return "Assault Rifle"
	case strings.Contains(lower, "glock"), strings.Contains(lower, "cz75"), strings.Contains(lower, "deagle"), strings.Contains(lower, "pistol"):
		return "Sidearm"
	default:
		return ""
	}
}
