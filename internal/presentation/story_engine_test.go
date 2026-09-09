package presentation

import "testing"

func TestPriority(t *testing.T) {
	d := 324.8
	c := Context{Distance: &d, Headshot: true, StreakMilestone: 10, BountyClaimed: true}
	if SelectPrimary(c) != StoryBountyClaimed {
		t.Fatal("bounty should win")
	}
	c.BountyClaimed = false
	if SelectPrimary(c) != StoryStreakMilestone {
		t.Fatal("streak should win over range/headshot")
	}
	c.StreakMilestone = 0
	if SelectPrimary(c) != StoryExtremeRange {
		t.Fatal("extreme should win")
	}
}
func TestSecondaryBounded(t *testing.T) {
	d := 300.0
	c := Context{Distance: &d, Headshot: true, Melee: true, BountyClaimed: true, WarKill: true, StreakMilestone: 10}
	if len(Secondary(c, StoryStandard)) > 5 {
		t.Fatal("badges unbounded")
	}
}
