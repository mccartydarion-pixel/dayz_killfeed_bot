package ranked

import (
	"testing"
	"time"
)

func TestProgressBoundaries(t *testing.T) {
	thresholds := Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}
	for _, tc := range []struct {
		rp        int64
		tier      Tier
		next      Tier
		remaining int64
	}{
		{0, Unranked, Rookie, 100},
		{99, Unranked, Rookie, 1},
		{100, Rookie, Bronze, 200},
		{2099, Platinum, Diamond, 1},
		{2100, Diamond, Master, 700},
		{2800, Master, "", 0},
		{100000, Master, "", 0},
	} {
		tier, next, remaining, err := thresholds.Progress(tc.rp)
		if err != nil || tier != tc.tier || next != tc.next || remaining != tc.remaining {
			t.Errorf("Progress(%d) = (%s, %s, %d, %v), want (%s, %s, %d)", tc.rp, tier, next, remaining, err, tc.tier, tc.next, tc.remaining)
		}
	}
}

func TestInvalidThresholdsAndRP(t *testing.T) {
	for _, thresholds := range []Thresholds{{}, {100, 100, 200, 300, 400, 500, 600}} {
		if _, _, _, err := thresholds.Progress(0); err == nil {
			t.Errorf("expected invalid thresholds to fail: %v", thresholds)
		}
	}
	if _, _, _, err := (Thresholds{100, 200, 300, 400, 500, 600, 700}).Progress(-1); err == nil {
		t.Fatal("negative RP accepted")
	}
}

func TestSameVictimCooldownBoundary(t *testing.T) {
	previous := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	if SameVictimCooldown != 5*time.Minute || DefaultSameVictimCooldownMinutes != 5 {
		t.Fatalf("default wait changed: %s", SameVictimCooldown)
	}
	for _, minutes := range []int{5, 30, 120} {
		wait := time.Duration(minutes) * time.Minute
		if EligibleRepeat(previous.Add(wait-time.Second), previous, wait) {
			t.Errorf("%d min: repeat kill one second early awarded", minutes)
		}
		if !EligibleRepeat(previous.Add(wait), previous, wait) {
			t.Errorf("%d min: repeat kill exactly at the wait denied", minutes)
		}
		if !EligibleRepeat(previous.Add(wait+time.Second), previous, wait) {
			t.Errorf("%d min: repeat kill after the wait denied", minutes)
		}
	}
	// The default is the old hard-coded rule.
	if EligibleRepeat(previous.Add(4*time.Minute+59*time.Second), previous, SameVictimCooldown) {
		t.Fatal("repeat kill inside five minutes awarded")
	}
	if !EligibleRepeat(previous.Add(5*time.Minute), previous, SameVictimCooldown) {
		t.Fatal("repeat kill at five minutes denied")
	}
}

func TestZeroSameVictimCooldownAlwaysEligible(t *testing.T) {
	previous := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, after := range []time.Duration{0, time.Second, time.Minute, time.Hour} {
		if !EligibleRepeat(previous.Add(after), previous, 0) {
			t.Errorf("no wait: repeat kill %s later denied", after)
		}
	}
}

func TestValidSameVictimCooldownMinutes(t *testing.T) {
	for minutes, want := range map[int]bool{-1: false, 0: true, 5: true, 30: true, 120: true, 121: false} {
		if got := ValidSameVictimCooldownMinutes(minutes); got != want {
			t.Errorf("ValidSameVictimCooldownMinutes(%d) = %v, want %v", minutes, got, want)
		}
	}
}

func TestDescribeSameVictimWait(t *testing.T) {
	for minutes, want := range map[int]string{
		0: "every kill of the same player counts", 1: "the same player counts again after 1 minute", 30: "the same player counts again after 30 minutes",
	} {
		if got := DescribeSameVictimWait(minutes); got != want {
			t.Errorf("DescribeSameVictimWait(%d) = %q, want %q", minutes, got, want)
		}
	}
}
