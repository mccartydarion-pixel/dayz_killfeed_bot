// Package ranked contains the rules for CHAMPIONS Ranked Points. Persistence
// and kill ingestion are separate so server seasons can award persisted kills.
package ranked

import (
	"fmt"
	"time"
)

// SameVictimCooldown is the default wait before the same attacker earns RP from
// the same victim again. A season freezes its own value (0 to 120 minutes) when
// it starts; this default applies when the owner does not choose one.
const SameVictimCooldown = DefaultSameVictimCooldownMinutes * time.Minute

const (
	DefaultSameVictimCooldownMinutes = 5
	MaxSameVictimCooldownMinutes     = 120
)

// ValidSameVictimCooldownMinutes reports whether a season may freeze this wait.
// 0 means no wait: every eligible kill of the same victim counts.
func ValidSameVictimCooldownMinutes(minutes int) bool {
	return minutes >= 0 && minutes <= MaxSameVictimCooldownMinutes
}

// DescribeSameVictimWait is the player-facing wording for a season's wait.
func DescribeSameVictimWait(minutes int) string {
	switch {
	case minutes <= 0:
		return "every kill of the same player counts"
	case minutes == 1:
		return "the same player counts again after 1 minute"
	default:
		return fmt.Sprintf("the same player counts again after %d minutes", minutes)
	}
}

type Tier string

const (
	Unranked Tier = "UNRANKED"
	Rookie   Tier = "ROOKIE"
	Bronze   Tier = "BRONZE"
	Silver   Tier = "SILVER"
	Gold     Tier = "GOLD"
	Platinum Tier = "PLATINUM"
	Diamond  Tier = "DIAMOND"
	Master   Tier = "MASTER"
)

var orderedTiers = [...]Tier{Rookie, Bronze, Silver, Gold, Platinum, Diamond, Master}

// Thresholds are the cumulative seasonal RP required to enter each tier.
// The values are supplied by the season's frozen ruleset; changing a future
// season must never reinterpret an archived season's standings.
type Thresholds [7]int64

func (t Thresholds) Validate() error {
	previous := int64(0)
	for i, value := range t {
		if value <= previous {
			return fmt.Errorf("%s threshold must exceed the previous threshold", orderedTiers[i])
		}
		previous = value
	}
	return nil
}

// Progress computes a tier and the remaining RP for the next tier.
func (t Thresholds) Progress(rp int64) (tier Tier, next Tier, remaining int64, err error) {
	if err = t.Validate(); err != nil {
		return "", "", 0, err
	}
	if rp < 0 {
		return "", "", 0, fmt.Errorf("RP cannot be negative")
	}
	tier = Unranked
	for i, threshold := range t {
		if rp < threshold {
			return tier, orderedTiers[i], threshold - rp, nil
		}
		tier = orderedTiers[i]
	}
	return Master, "", 0, nil
}

// Level orders tiers: 0 for Unranked, 1 for Rookie, up to 7 for Master (-1 when unknown).
func (t Tier) Level() int {
	if t == Unranked {
		return 0
	}
	for i, tier := range orderedTiers {
		if tier == t {
			return i + 1
		}
	}
	return -1
}

// EligibleRepeat reports whether the same attacker can earn RP from the same
// victim again. The caller must use the server's player IDs, event time,
// a durable local award ledger and the kill's season's frozen wait. A kill
// exactly at the end of the wait is eligible; a wait of zero (or less) makes
// every kill eligible. Kills remain ordinary combat events when this returns
// false.
func EligibleRepeat(killAt, previousAwardAt time.Time, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return true
	}
	return !killAt.Before(previousAwardAt.Add(cooldown))
}
