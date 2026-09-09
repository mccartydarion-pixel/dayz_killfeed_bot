package bounties

const AllowTeamKillClaims = false

type Tier struct{ MinimumStreak, RewardPoints int }

var Tiers = []Tier{{10, 500}, {15, 750}, {20, 1000}, {25, 1500}}

func RewardForStreak(streak int) int {
	reward := 0
	for _, tier := range Tiers {
		if streak >= tier.MinimumStreak {
			reward = tier.RewardPoints
		}
	}
	return reward
}
func ShouldUpgrade(current, recommended int64) bool { return recommended > current }
