package presentation

import "testing"

func TestSignatureStoryHeadersHaveDistinctAccents(t *testing.T) {
	stories := []StoryType{StoryStandard, StoryMelee, StoryHeadshot, StoryLongRange, StoryExtremeRange, StoryBountyClaimed, StoryWarKill, StoryWarLeadChange, StoryStreakEnded}
	seen := map[string]bool{}
	for _, story := range stories {
		header := BuildStoryHeader(story)
		if header.Title == "" || header.Hero == "" || header.Icon == "" {
			t.Fatalf("incomplete header for %s: %#v", story, header)
		}
		seen[header.Title] = true
	}
	if len(seen) != len(stories) {
		t.Fatal("signature stories should not collapse into one visual header")
	}
}

func TestRangeClassesAndWeaponFlavorAreCentralized(t *testing.T) {
	distance := 287.4
	if got := RangeClass(&distance, false); got != "EXTREME RANGE" && got != "LONG RANGE" {
		t.Fatalf("unexpected range class: %q", got)
	}
	if got := RangeClass(nil, true); got != "CLOSE QUARTERS" {
		t.Fatalf("unexpected melee range class: %q", got)
	}
	if got := WeaponStory("M70 Tundra", false); got != "Dead Eye" {
		t.Fatalf("unexpected sniper flavor: %q", got)
	}
}

func TestPriorityIncludesSignatureStories(t *testing.T) {
	if SelectPrimary(Context{FirstBlood: true, RapidKill: true}) != StoryFirstBlood {
		t.Fatal("first blood should outrank rapid kill")
	}
	if SelectPrimary(Context{ServerRecord: true, BountyClaimed: true, WarLeadChange: true}) != StoryServerRecord {
		t.Fatal("server record should have highest priority")
	}
}
