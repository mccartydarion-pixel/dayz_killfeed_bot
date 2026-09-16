package streaks

import "sort"

// Tier is a centralized streak milestone definition.
type Tier struct {
	Threshold int
	Name      string
	Emoji     string
}

var DefaultTiers = []Tier{
	{3, "HOT STREAK", "🔥"},
	{5, "KILLING SPREE", "🔥"},
	{10, "DOMINATING", "⚔️"},
	{15, "UNSTOPPABLE", "👑"},
	{20, "CHAMPION", "🏆"},
	{25, "LEGENDARY", "👑"},
}

// MeaningfulStreakThreshold is the minimum kill streak worth publicly
// announcing as ended. It is the single source of truth for EndedAnnouncement
// and for the durable STREAK_ENDED kill classification persisted at kill time
// (see internal/killfeed/persistence.go) - both must stay in sync.
const MeaningfulStreakThreshold = 5

// KillingSpreeThreshold is the authoritative "killing spree" milestone kill
// count, reused by the durable KILLING_SPREE kill classification. It reuses
// this package's own "KILLING SPREE" tier rather than introducing a second,
// competing threshold constant.
func KillingSpreeThreshold() int {
	for _, t := range DefaultTiers {
		if t.Name == "KILLING SPREE" {
			return t.Threshold
		}
	}
	return MeaningfulStreakThreshold
}

// Result is the deterministic effect of one authoritative kill/death.
type Result struct {
	CurrentStreak int
	BestStreak    int
	Reached       *Tier
	EndedStreak   int
}

func TierAt(streak int) *Tier {
	var result *Tier
	for i := range DefaultTiers {
		if streak >= DefaultTiers[i].Threshold {
			result = &DefaultTiers[i]
		}
	}
	return result
}

func IsMajor(streak int) bool {
	for _, t := range DefaultTiers {
		if t.Threshold >= MeaningfulStreakThreshold && t.Threshold == streak {
			return true
		}
	}
	return false
}

func EndedAnnouncement(streak int) bool { return streak >= MeaningfulStreakThreshold }

// SortTiers returns a copy ordered by threshold for presentation/configuration.
func SortTiers() []Tier {
	out := append([]Tier(nil), DefaultTiers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Threshold < out[j].Threshold })
	return out
}
