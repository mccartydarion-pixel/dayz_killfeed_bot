// Package vip holds the rules for supporter / VIP tiers (docs/CLIENT_HUB_GROWTH.md): a named tier
// with a killfeed badge, a display colour, an optional Discord role and a Champion Point reward
// multiplier. Persistence and Discord calls live elsewhere; this package is pure.
package vip

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxMultiplier = 3.0
	MaxBadgeRunes = 24
	MaxNameRunes  = 40
	MaxDays       = 3650
)

type Tier struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Badge            string  `json:"badge"`
	Color            string  `json:"color"`
	DiscordRoleID    string  `json:"discordRoleId,omitempty"`
	RewardMultiplier float64 `json:"rewardMultiplier"`
	SortOrder        int     `json:"sortOrder"`
	ActiveMembers    int64   `json:"activeMembers"`
}

var (
	colorRe     = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	snowflakeRe = regexp.MustCompile(`^[0-9]{17,20}$`)
)

// Normalize validates a tier as an owner submitted it.
func Normalize(t Tier) (Tier, error) {
	t.Name = strings.TrimSpace(t.Name)
	t.Badge = strings.TrimSpace(strings.NewReplacer("\n", " ", "\r", " ", "@", "").Replace(t.Badge))
	t.Color = strings.TrimSpace(t.Color)
	t.DiscordRoleID = strings.TrimSpace(t.DiscordRoleID)
	if t.Name == "" || utf8.RuneCountInString(t.Name) > MaxNameRunes {
		return t, fmt.Errorf("name the tier (up to %d characters)", MaxNameRunes)
	}
	if t.Badge == "" || utf8.RuneCountInString(t.Badge) > MaxBadgeRunes {
		return t, fmt.Errorf("the killfeed badge is 1 to %d characters", MaxBadgeRunes)
	}
	if t.Color == "" {
		t.Color = "#E7B94A"
	}
	if !colorRe.MatchString(t.Color) {
		return t, fmt.Errorf("colour must look like #E7B94A")
	}
	t.Color = strings.ToUpper(t.Color)
	if t.DiscordRoleID != "" && !snowflakeRe.MatchString(t.DiscordRoleID) {
		return t, fmt.Errorf("the Discord role must be a role ID (17 to 20 digits)")
	}
	if t.RewardMultiplier == 0 {
		t.RewardMultiplier = 1
	}
	if math.IsNaN(t.RewardMultiplier) || t.RewardMultiplier < 1 || t.RewardMultiplier > MaxMultiplier {
		return t, fmt.Errorf("reward multiplier is between 1.0 and %.1f", MaxMultiplier)
	}
	t.RewardMultiplier = math.Round(t.RewardMultiplier*100) / 100
	if t.SortOrder < 0 || t.SortOrder > 1000 {
		return t, fmt.Errorf("sort order is between 0 and 1000")
	}
	return t, nil
}

// Multiply applies a tier multiplier to a reward, rounding down; a missing or invalid multiplier
// leaves the amount unchanged.
func Multiply(amount int64, multiplier float64) int64 {
	if multiplier <= 1 || multiplier > MaxMultiplier || math.IsNaN(multiplier) {
		return amount
	}
	return int64(math.Floor(float64(amount) * multiplier))
}
