package progression

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Battle pass tracks and reward kinds.
const (
	TrackFree    = "FREE"
	TrackPremium = "PREMIUM"

	RewardPoints = "POINTS" // Champion Points paid to the player's balance
	RewardTitle  = "TITLE"  // a title the player can show on their profile
	RewardBadge  = "BADGE"  // an emoji badge the player can show next to their name
)

// Limits of a battle pass season.
const (
	MinLevels, MaxLevels           = 5, 100
	MinXPPerLevel, MaxXPPerLevel   = 100, 100000
	MaxTitleRunes, MaxBadgeRunes   = 32, 8
	MaxRewardPoints                = 1000000
	MaxPremiumPrice                = 10000000
	MaxXPPerSource                 = 100000
	MaxDailyKillCap, MaxDailyHours = 200, 24
)

// Reward is one level's reward on one track.
type Reward struct {
	Level  int    `json:"level"`
	Track  string `json:"track"`
	Kind   string `json:"kind"`
	Amount int64  `json:"amount,omitempty"` // POINTS
	Text   string `json:"text,omitempty"`   // TITLE: the title; BADGE: the emoji
	Label  string `json:"label,omitempty"`  // BADGE: what the badge is called
}

var ErrRewardInvalid = errors.New("a reward is invalid")

// ValidateRewards checks a reward track against the season's level count: a level and track appear
// at most once and every reward is complete.
func ValidateRewards(rewards []Reward, levels int) error {
	seen := map[[2]any]bool{}
	for _, r := range rewards {
		key := [2]any{r.Level, r.Track}
		if r.Level < 1 || r.Level > levels || (r.Track != TrackFree && r.Track != TrackPremium) || seen[key] {
			return ErrRewardInvalid
		}
		seen[key] = true
		switch r.Kind {
		case RewardPoints:
			if r.Amount < 1 || r.Amount > MaxRewardPoints {
				return ErrRewardInvalid
			}
		case RewardTitle:
			if t := strings.TrimSpace(r.Text); t == "" || utf8.RuneCountInString(t) > MaxTitleRunes {
				return ErrRewardInvalid
			}
		case RewardBadge:
			if t := strings.TrimSpace(r.Text); t == "" || utf8.RuneCountInString(t) > MaxBadgeRunes || utf8.RuneCountInString(strings.TrimSpace(r.Label)) > MaxTitleRunes {
				return ErrRewardInvalid
			}
		default:
			return ErrRewardInvalid
		}
	}
	return nil
}

// DefaultRewards is a ready-made track for a season of `levels` levels: points on the free track
// every other level and titles at the milestones; points every level, badges and better titles on
// the premium track.
func DefaultRewards(levels int) []Reward {
	out := []Reward{}
	titles := map[int]string{10: "Survivor", 20: "Veteran", 30: "Legend", 40: "Warlord", 50: "Immortal"}
	premiumTitles := map[int]string{15: "Hunter", 25: "Apex Predator", 35: "Reaper", 45: "Overlord"}
	badges := map[int][2]string{5: {"🎖️", "Recruit"}, 12: {"🔥", "On Fire"}, 18: {"⚔️", "Duelist"}, 24: {"💀", "Headhunter"}, 30: {"👑", "Champion"}}
	for lvl := 1; lvl <= levels; lvl++ {
		switch {
		case lvl == levels && titles[lvl] == "":
			out = append(out, Reward{Level: lvl, Track: TrackFree, Kind: RewardTitle, Text: "Season Finisher"})
		case titles[lvl] != "":
			out = append(out, Reward{Level: lvl, Track: TrackFree, Kind: RewardTitle, Text: titles[lvl]})
		case lvl%2 == 0:
			out = append(out, Reward{Level: lvl, Track: TrackFree, Kind: RewardPoints, Amount: int64(25 * lvl)})
		}
		switch {
		case lvl == levels && badges[lvl][0] == "" && premiumTitles[lvl] == "":
			out = append(out, Reward{Level: lvl, Track: TrackPremium, Kind: RewardBadge, Text: "👑", Label: "Season Champion"})
		case badges[lvl][0] != "":
			out = append(out, Reward{Level: lvl, Track: TrackPremium, Kind: RewardBadge, Text: badges[lvl][0], Label: badges[lvl][1]})
		case premiumTitles[lvl] != "":
			out = append(out, Reward{Level: lvl, Track: TrackPremium, Kind: RewardTitle, Text: premiumTitles[lvl]})
		default:
			out = append(out, Reward{Level: lvl, Track: TrackPremium, Kind: RewardPoints, Amount: int64(40 * lvl)})
		}
	}
	return out
}

// Level is the level reached with xp (0 before the first), capped at the season's last level.
func Level(xp, xpPerLevel int64, levels int) int {
	if xp <= 0 || xpPerLevel <= 0 {
		return 0
	}
	if l := xp / xpPerLevel; l < int64(levels) {
		return int(l)
	}
	return levels
}
