// Package deathstats is the one definition of a "PvP death" and a "PvE death", and of the K/D
// figures built on them. Every query, DTO and card that splits deaths or shows a PvP K/D uses it,
// so the split cannot differ from one screen to the next (docs/DEATH_COUNTS.md).
//
//   - A PvP death is a death caused by another player: a deaths row whose death_type is PVP (the
//     row written with the kill, for its victim).
//   - A PvE death is every other death: suicide, a "died" line with no cause (bleeding out,
//     starvation, a fall, drowning), and any type added later that is not PVP.
//   - Overall K/D = player kills / all deaths. PvP K/D = player kills / PvP deaths.
//
// DayZ's console logs do not record infected or animal kills, so there are no "PvE kills" and no
// PvE K/D.
package deathstats

import "math"

// TypePvP is the death_type of a death caused by another player. It is the only value that means
// PvP; repository.DeathTypePVP is this constant.
const TypePvP = "PVP"

// IsPvP reports whether a death_type is a death caused by another player.
func IsPvP(deathType string) bool { return deathType == TypePvP }

// IsPvE reports whether a death_type is any death not caused by another player.
func IsPvE(deathType string) bool { return !IsPvP(deathType) }

// PvPPredicate is the SQL condition for a PvP death on an unaliased deaths row.
const PvPPredicate = "death_type = '" + TypePvP + "'"

// PvPCount is the SQL aggregate counting the PvP deaths among the deaths rows being aggregated.
// PvE deaths are COUNT(*) minus this, so the two always add up to the total.
const PvPCount = "COUNT(*) FILTER (WHERE " + PvPPredicate + ")"

// PvPPredicateD and PvPCountD are PvPPredicate and PvPCount for a deaths row aliased "d" (constants,
// so they can be part of a constant query).
const (
	PvPPredicateD = "d." + PvPPredicate
	PvPCountD     = "COUNT(*) FILTER (WHERE " + PvPPredicateD + ")"
)

// PvE returns the PvE deaths given all deaths and the PvP deaths among them. It never goes below zero.
func PvE(deaths, pvpDeaths int64) int64 {
	if pvpDeaths >= deaths {
		return 0
	}
	if pvpDeaths < 0 {
		return deaths
	}
	return deaths - pvpDeaths
}

// KD is kills per death by the house rule every K/D in the project follows: with no deaths the
// ratio is the kill count itself - never a division by zero, never infinity. It is the overall
// K/D when given all deaths and the PvP K/D when given PvP deaths.
func KD(kills, deaths int64) float64 {
	if deaths <= 0 {
		return float64(kills)
	}
	return float64(kills) / float64(deaths)
}

// KDRounded is KD rounded to two decimals, the precision every K/D is shown and ranked at.
func KDRounded(kills, deaths int64) float64 {
	return math.Round(KD(kills, deaths)*100) / 100
}
