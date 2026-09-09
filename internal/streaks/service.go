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
		if t.Threshold >= 5 && t.Threshold == streak {
			return true
		}
	}
	return false
}

func EndedAnnouncement(streak int) bool { return streak >= 5 }

// SortTiers returns a copy ordered by threshold for presentation/configuration.
func SortTiers() []Tier {
	out := append([]Tier(nil), DefaultTiers...)
	sort.Slice(out, func(i, j int) bool { return out[i].Threshold < out[j].Threshold })
	return out
}
