// Package ranked contains the rules for CHAMPIONS Ranked Points. Persistence
// and kill ingestion are deliberately separate so ranks cannot be awarded
// before a verified cross-server console identity is available.
package ranked

import (
	"fmt"
	"time"
)

const SameVictimCooldown = 5 * time.Minute

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

// Progress computes a tier and the remaining RP for the next tier. Elite Top
// 250 is a leaderboard position among Master players, never an RP threshold.
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

// EligibleRepeat reports whether the same attacker can earn RP from the same
// victim again. The caller must use verified platform identities, event time,
// and a durable, cross-server ledger. Kills remain ordinary combat events when
// this returns false.
func EligibleRepeat(killAt, previousAwardAt time.Time) bool {
	return !killAt.Before(previousAwardAt.Add(SameVictimCooldown))
}
