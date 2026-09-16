package presentation

import "strings"

type StoryType string

const (
	StoryStandard            StoryType = "STANDARD"
	StoryMelee               StoryType = "MELEE"
	StoryHeadshot            StoryType = "HEADSHOT"
	StoryLongRange           StoryType = "LONG_RANGE"
	StoryExtremeRange        StoryType = "EXTREME_RANGE"
	StoryStreakMilestone     StoryType = "STREAK_MILESTONE"
	StoryStreakEnded         StoryType = "STREAK_ENDED"
	StoryWarKill             StoryType = "WAR_KILL"
	StoryBountyClaimed       StoryType = "BOUNTY_CLAIMED"
	StoryEventKill           StoryType = "EVENT_KILL"
	StoryTeamKill            StoryType = "TEAM_KILL"
	StoryFactionKill         StoryType = "FACTION_KILL"
	StoryPersonalRecord      StoryType = "PERSONAL_RECORD"
	StoryServerRecord        StoryType = "SERVER_RECORD"
	StoryRevenge             StoryType = "REVENGE"
	StoryNemesis             StoryType = "NEMESIS"
	StoryWarLeadChange       StoryType = "WAR_LEAD_CHANGE"
	StoryWarTie              StoryType = "WAR_TIE"
	StoryFirstBlood          StoryType = "FIRST_BLOOD"
	StoryEventLeadChange     StoryType = "EVENT_LEAD_CHANGE"
	StoryRankPromotion       StoryType = "RANK_PROMOTION"
	StoryLeaderboardTakeover StoryType = "LEADERBOARD_TAKEOVER"
	StoryRapidKill           StoryType = "RAPID_KILL"
)

type Context struct {
	Distance                                                            *float64
	Headshot, Melee, TeamKill, EnemyFactionKill, WarKill, BountyClaimed bool
	EventBadges                                                         []string
	StreakMilestone, VictimEndedStreak                                  int
	StreakEndedThreshold                                                int
	PersonalRecord, ServerRecord, Revenge, Nemesis                      bool
	WarLeadChange, WarTie, FirstBlood, EventLeadChange                  bool
	RankPromotion, LeaderboardTakeover, RapidKill                       bool
}

func SelectPrimary(c Context) StoryType {
	if c.ServerRecord {
		return StoryServerRecord
	}
	if c.BountyClaimed {
		return StoryBountyClaimed
	}
	if c.WarLeadChange {
		return StoryWarLeadChange
	}
	if c.WarTie {
		return StoryWarTie
	}
	if c.FirstBlood {
		return StoryFirstBlood
	}
	if c.EventLeadChange {
		return StoryEventLeadChange
	}
	if c.LeaderboardTakeover {
		return StoryLeaderboardTakeover
	}
	if c.RankPromotion {
		return StoryRankPromotion
	}
	if c.StreakEndedThreshold > 0 && c.VictimEndedStreak >= c.StreakEndedThreshold {
		return StoryStreakEnded
	}
	if c.StreakMilestone > 0 {
		return StoryStreakMilestone
	}
	if c.PersonalRecord {
		return StoryPersonalRecord
	}
	if c.Distance != nil && *c.Distance >= 200 {
		return StoryExtremeRange
	}
	if c.Headshot {
		return StoryHeadshot
	}
	if c.Revenge {
		return StoryRevenge
	}
	if c.Nemesis {
		return StoryNemesis
	}
	if c.WarKill && len(c.EventBadges) > 0 {
		return StoryWarKill
	}
	if c.TeamKill {
		return StoryTeamKill
	}
	if c.EnemyFactionKill {
		return StoryFactionKill
	}
	if c.WarKill {
		return StoryWarKill
	}
	if len(c.EventBadges) > 0 {
		return StoryEventKill
	}
	if c.RapidKill {
		return StoryRapidKill
	}
	if c.Distance != nil && *c.Distance >= 100 {
		return StoryLongRange
	}
	return StoryStandard
}
func Secondary(c Context, primary StoryType) []string {
	out := []string{}
	add := func(v string) {
		if len(out) < 5 {
			out = append(out, v)
		}
	}
	if c.Headshot && primary != StoryHeadshot {
		add("🎯 HEADSHOT")
	}
	if c.Distance != nil && *c.Distance >= 200 && primary != StoryExtremeRange {
		add("🚀 EXTREME RANGE")
	}
	if c.BountyClaimed && primary != StoryBountyClaimed {
		add("💰 BOUNTY")
	}
	if c.WarKill && primary != StoryWarKill {
		add("⚔️ WAR KILL")
	}
	if c.StreakMilestone > 0 && primary != StoryStreakMilestone {
		add("🔥 " + itoa(c.StreakMilestone) + " STREAK")
	}
	return out
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return strings.TrimSpace(digits)
}
