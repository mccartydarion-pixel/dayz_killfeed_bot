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
	if EligibleRepeat(previous.Add(4*time.Minute+59*time.Second), previous) {
		t.Fatal("repeat kill inside five minutes awarded")
	}
	if !EligibleRepeat(previous.Add(5*time.Minute), previous) {
		t.Fatal("repeat kill at five minutes denied")
	}
}
