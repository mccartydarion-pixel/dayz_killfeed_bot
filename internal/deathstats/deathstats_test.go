package deathstats

import "testing"

func TestClassification(t *testing.T) {
	cases := []struct {
		deathType string
		pvp       bool
	}{
		{"PVP", true},
		{"SUICIDE", false},
		{"UNKNOWN", false},
		{"INFECTED", false},    // not written today; PvE if it ever is
		{"ANIMAL", false},      // not written today
		{"ENVIRONMENT", false}, // not written today
		{"", false},
		{"pvp", false}, // the stored value is exact; SQL compares it exactly too
		{"PVP ", false},
	}
	for _, c := range cases {
		if got := IsPvP(c.deathType); got != c.pvp {
			t.Errorf("IsPvP(%q) = %v, want %v", c.deathType, got, c.pvp)
		}
		if got := IsPvE(c.deathType); got == c.pvp {
			t.Errorf("IsPvE(%q) = %v, want %v", c.deathType, got, !c.pvp)
		}
	}
}

func TestSQLFragments(t *testing.T) {
	if PvPPredicate != "death_type = 'PVP'" {
		t.Errorf("PvPPredicate = %q", PvPPredicate)
	}
	if PvPPredicateD != "d.death_type = 'PVP'" {
		t.Errorf("PvPPredicateD = %q", PvPPredicateD)
	}
	if PvPCount != "COUNT(*) FILTER (WHERE death_type = 'PVP')" {
		t.Errorf("PvPCount = %q", PvPCount)
	}
	if PvPCountD != "COUNT(*) FILTER (WHERE d.death_type = 'PVP')" {
		t.Errorf("PvPCountD = %q", PvPCountD)
	}
}

func TestPvE(t *testing.T) {
	cases := []struct{ deaths, pvp, want int64 }{
		{0, 0, 0}, {10, 0, 10}, {10, 10, 0}, {10, 4, 6},
		{3, 5, 0},  // never negative
		{3, -1, 3}, // never more than the total
	}
	for _, c := range cases {
		if got := PvE(c.deaths, c.pvp); got != c.want {
			t.Errorf("PvE(%d,%d) = %d, want %d", c.deaths, c.pvp, got, c.want)
		}
	}
}

func TestKD(t *testing.T) {
	cases := []struct {
		name                       string
		kills, deaths, pvp         int64
		overall, pvpKD             float64
		overallRounded, pvpRounded float64
	}{
		{"no kills, no deaths", 0, 0, 0, 0, 0, 0, 0},
		{"kills, no deaths: the kill count", 9, 0, 0, 9, 9, 9, 9},
		{"only PvE deaths: PvP K/D is the kill count", 9, 3, 0, 3, 9, 3, 9},
		{"only PvP deaths: both the same", 9, 3, 3, 3, 3, 3, 3},
		{"a mix", 10, 8, 3, 1.25, 10.0 / 3, 1.25, 3.33},
		{"no kills, only PvE deaths", 0, 5, 0, 0, 0, 0, 0},
		{"no kills, PvP deaths", 0, 5, 2, 0, 0, 0, 0},
		{"rounds half up", 1, 8, 8, 0.125, 0.125, 0.13, 0.13},
	}
	for _, c := range cases {
		if got := KD(c.kills, c.deaths); got != c.overall {
			t.Errorf("%s: KD(overall) = %v, want %v", c.name, got, c.overall)
		}
		if got := KD(c.kills, c.pvp); got != c.pvpKD {
			t.Errorf("%s: KD(pvp) = %v, want %v", c.name, got, c.pvpKD)
		}
		if got := KDRounded(c.kills, c.deaths); got != c.overallRounded {
			t.Errorf("%s: KDRounded(overall) = %v, want %v", c.name, got, c.overallRounded)
		}
		if got := KDRounded(c.kills, c.pvp); got != c.pvpRounded {
			t.Errorf("%s: KDRounded(pvp) = %v, want %v", c.name, got, c.pvpRounded)
		}
	}
	if got := KD(4, -2); got != 4 {
		t.Errorf("KD with negative deaths = %v, want 4", got)
	}
}
