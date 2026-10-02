// Package rewards holds the rules for automatic Champion Point rewards (docs/CLIENT_HUB_GROWTH.md):
// reaching a Ranked tier, a weekly activity streak, and finishing in a stats season's top N.
// Persistence and payout live in internal/repository; this package is pure so every rule is
// unit tested.
package rewards

import (
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
)

const (
	KindRankReached  = "RANK_REACHED"
	KindWeeklyActive = "WEEKLY_ACTIVE"
	KindSeasonTop    = "SEASON_TOP"

	MaxPoints = 1_000_000
)

// Rule is one owner-configured reward. Tier is used by RANK_REACHED, MinHours/MinDays by
// WEEKLY_ACTIVE and Places by SEASON_TOP.
type Rule struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Tier      string    `json:"tier,omitempty"`
	Points    int64     `json:"points"`
	MinHours  int       `json:"minHours,omitempty"`
	MinDays   int       `json:"minDays,omitempty"`
	Places    int       `json:"places,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Tiers is the order a player climbs (UNRANKED earns nothing).
var Tiers = []ranked.Tier{ranked.Rookie, ranked.Bronze, ranked.Silver, ranked.Gold, ranked.Platinum, ranked.Diamond, ranked.Master}

func tierIndex(t ranked.Tier) int {
	for i, x := range Tiers {
		if x == t {
			return i
		}
	}
	return -1
}

// AtLeast reports whether a player's tier is the rule's tier or above.
func AtLeast(have, want ranked.Tier) bool {
	w := tierIndex(want)
	return w >= 0 && tierIndex(have) >= w
}

// Normalize validates a rule and clears the fields its kind does not use.
func Normalize(r Rule) (Rule, error) {
	if r.Points < 1 || r.Points > MaxPoints {
		return r, fmt.Errorf("points must be between 1 and %d", MaxPoints)
	}
	out := Rule{ID: r.ID, Kind: r.Kind, Points: r.Points, Enabled: r.Enabled}
	switch r.Kind {
	case KindRankReached:
		if tierIndex(ranked.Tier(r.Tier)) < 0 {
			return r, fmt.Errorf("pick a Ranked tier")
		}
		out.Tier = r.Tier
	case KindWeeklyActive:
		if r.MinHours < 1 || r.MinHours > 100 {
			return r, fmt.Errorf("weekly hours must be between 1 and 100")
		}
		if r.MinDays < 1 || r.MinDays > 7 {
			return r, fmt.Errorf("days played must be between 1 and 7")
		}
		out.MinHours, out.MinDays = r.MinHours, r.MinDays
	case KindSeasonTop:
		if r.Places < 1 || r.Places > 25 {
			return r, fmt.Errorf("places must be between 1 and 25")
		}
		out.Places = r.Places
	default:
		return r, fmt.Errorf("unknown reward kind %q", r.Kind)
	}
	return out, nil
}

// LastFullWeek is the most recent completed ISO week (Monday 00:00 UTC to the next Monday) and
// its label, e.g. "2026-W39".
func LastFullWeek(now time.Time) (start, end time.Time, label string) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7 // days since Monday
	end = day.AddDate(0, 0, -offset)
	start = end.AddDate(0, 0, -7)
	y, w := start.ISOWeek()
	return start, end, fmt.Sprintf("%d-W%02d", y, w)
}

// Reference is the ledger idempotency key for one payout: a player is paid at most once per key.
func RankReference(seasonID int64, tier string) string {
	return fmt.Sprintf("reward:rank:%d:%s", seasonID, tier)
}
func WeeklyReference(week string) string    { return "reward:weekly:" + week }
func SeasonReference(seasonID int64) string { return fmt.Sprintf("reward:season:%d", seasonID) }

// Describe is the one-line ledger description a player sees for a payout.
func Describe(r Rule, place int) string {
	switch r.Kind {
	case KindRankReached:
		return "Reward: reached " + r.Tier + " in Ranked"
	case KindWeeklyActive:
		return fmt.Sprintf("Reward: played %d+ hours on %d+ days last week", r.MinHours, r.MinDays)
	case KindSeasonTop:
		return fmt.Sprintf("Reward: #%d in the stats season", place)
	}
	return "Reward"
}
