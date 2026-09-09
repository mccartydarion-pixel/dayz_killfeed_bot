package bounties

import "testing"

func TestRewardForStreakTiers(t *testing.T) {
	cases := []struct{ streak, reward int }{{9, 0}, {10, 500}, {15, 750}, {20, 1000}, {25, 1500}, {40, 1500}}
	for _, tc := range cases {
		if got := RewardForStreak(tc.streak); got != tc.reward {
			t.Fatalf("streak %d: got %d want %d", tc.streak, got, tc.reward)
		}
	}
}
func TestShouldUpgrade(t *testing.T) {
	if !ShouldUpgrade(500, 750) {
		t.Fatal("higher tier should upgrade")
	}
	if ShouldUpgrade(750, 500) {
		t.Fatal("lower tier should not downgrade")
	}
}
