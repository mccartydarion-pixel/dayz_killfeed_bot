package rewards

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
)

func TestAtLeast(t *testing.T) {
	if !AtLeast(ranked.Gold, ranked.Gold) || !AtLeast(ranked.Master, ranked.Bronze) || AtLeast(ranked.Silver, ranked.Gold) || AtLeast(ranked.Unranked, ranked.Rookie) || AtLeast(ranked.Gold, ranked.Unranked) {
		t.Fatal("tier order")
	}
}

func TestNormalize(t *testing.T) {
	ok := []Rule{{Kind: KindRankReached, Tier: "GOLD", Points: 500, MinHours: 9}, {Kind: KindWeeklyActive, Points: 100, MinHours: 5, MinDays: 3}, {Kind: KindSeasonTop, Points: 1000, Places: 10}}
	for _, r := range ok {
		n, err := Normalize(r)
		if err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		if r.Kind == KindRankReached && n.MinHours != 0 {
			t.Fatal("unused fields must be cleared")
		}
	}
	bad := []Rule{{Kind: KindRankReached, Tier: "UNRANKED", Points: 1}, {Kind: KindWeeklyActive, Points: 1, MinHours: 0, MinDays: 1}, {Kind: KindSeasonTop, Points: 1, Places: 26}, {Kind: "X", Points: 1}, {Kind: KindSeasonTop, Places: 3, Points: 0}, {Kind: KindSeasonTop, Places: 3, Points: MaxPoints + 1}}
	for _, r := range bad {
		if _, err := Normalize(r); err == nil {
			t.Errorf("expected rejection: %+v", r)
		}
	}
}

func TestLastFullWeek(t *testing.T) {
	// Thursday 1 Oct 2026 -> the week of Mon 21 Sep .. Mon 28 Sep.
	start, end, label := LastFullWeek(time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) || label != "2026-W39" {
		t.Fatalf("got %s %s %s", start, end, label)
	}
	// On a Monday the week that just ended is the last full one.
	start, _, _ = LastFullWeek(time.Date(2026, 9, 28, 0, 30, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monday: %s", start)
	}
}
