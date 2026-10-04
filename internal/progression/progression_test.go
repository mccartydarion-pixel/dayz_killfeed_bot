package progression

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestWeaponClass(t *testing.T) {
	cases := map[string]string{
		"Mosin 9130": ClassSniper, "SVD": ClassSniper, "M70 Tundra": ClassSniper, "CR-527": ClassSniper,
		"M4-A1": ClassRifle, "AKM": ClassRifle, "KA-M": ClassRifle, "LAR": ClassRifle, "AUG": ClassRifle,
		"MP5-K": ClassSMG, "Bizon": ClassSMG, "CR-61 Skorpion": ClassSMG,
		"BK-43": ClassShotgun, "Vaiga": ClassShotgun, "Sawed-off BK-133": ClassShotgun,
		"Glock 19": ClassPistol, "Deagle": ClassPistol, "FX-45": ClassPistol, "Longhorn": ClassPistol,
		"Hunting Knife": "", "Fists": "", "": "",
	}
	for weapon, want := range cases {
		if got := WeaponClass(weapon); got != want {
			t.Errorf("%q: got %q, want %q", weapon, got, want)
		}
	}
	// Every pattern compiles in Go once the PostgreSQL word boundaries are translated.
	for class := range weaponPatterns {
		if goPattern(class) == nil || WeaponPattern(class) == "" {
			t.Errorf("%s has no pattern", class)
		}
		if _, err := regexp.Compile(strings.NewReplacer(`\m`, `\b`, `\M`, `\b`).Replace(WeaponPattern(class))); err != nil {
			t.Errorf("%s: %v", class, err)
		}
	}
}

func TestGenerateIsStableAndDistinct(t *testing.T) {
	day := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	a := Generate(7, PeriodDay, day, 3, Features{})
	b := Generate(7, PeriodDay, day.Add(5*time.Hour), 3, Features{})
	if len(a) != 3 {
		t.Fatalf("3 challenges: %+v", a)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("the same day draws the same set: %+v vs %+v", a, b)
		}
	}
	keys, weapons := map[string]bool{}, 0
	for _, c := range a {
		if keys[c.Key] {
			t.Fatalf("duplicate %s", c.Key)
		}
		keys[c.Key] = true
		if c.Kind == KindWeapon {
			weapons++
		}
		if c.Kind == KindHotZone || c.Kind == KindTerritory {
			t.Fatalf("hot zone and territory challenges need those features: %+v", c)
		}
		if c.Title == "" || c.Target < 1 {
			t.Fatalf("incomplete: %+v", c)
		}
	}
	if weapons > 1 {
		t.Fatalf("at most one weapon challenge: %+v", a)
	}
	differs := false
	for d := 1; d < 10 && !differs; d++ {
		next := Generate(7, PeriodDay, day.AddDate(0, 0, d), 3, Features{})
		for i := range next {
			if next[i].Key != a[i].Key {
				differs = true
			}
		}
	}
	if !differs {
		t.Fatal("other days draw other challenges")
	}
	all := Generate(7, PeriodWeek, PeriodStart(PeriodWeek, day), 50, Features{HotZones: true, Territory: true})
	if len(all) != len(templates)-4 { // four of the five weapon templates are skipped
		t.Fatalf("everything else is drawn: %d", len(all))
	}
	for _, c := range all {
		if c.Kind == KindLongRange && c.Param != 300 {
			t.Fatalf("weekly long range is 300 m: %+v", c)
		}
	}
}

func TestPeriods(t *testing.T) {
	sun := time.Date(2026, 10, 4, 22, 30, 0, 0, time.UTC) // a Sunday
	if got := PeriodStart(PeriodWeek, sun); !got.Equal(time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week starts Monday: %v", got)
	}
	if got := PeriodStart(PeriodDay, sun); !got.Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day: %v", got)
	}
	if got := PeriodEnd(PeriodWeek, PeriodStart(PeriodWeek, sun)); !got.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("week end: %v", got)
	}
	if minutesText(90) != "1h 30m" || minutesText(120) != "2 hours" || minutesText(60) != "1 hour" || minutesText(45) != "45 minutes" {
		t.Fatal("minutes text")
	}
}

func TestZones(t *testing.T) {
	zs := Zones("")
	if len(zs) == 0 || len(Zones("enoch")) == 0 || Zones("CHERNARUSPLUS")[0].Key != "chernogorsk" {
		t.Fatal("zones")
	}
	keys := map[string]bool{}
	for _, m := range mapZones {
		for _, z := range m {
			if keys[z.Key] && z.Key != "polana" {
				t.Fatalf("duplicate key %s", z.Key)
			}
			keys[z.Key] = true
		}
	}
	if z, ok := ZoneAt(zs, 1650, 14050); !ok || z.Name != "Tisy" {
		t.Fatalf("Tisy: %+v %v", z, ok)
	}
	if _, ok := ZoneAt(zs, 7500, 500); ok {
		t.Fatal("open country is in no zone")
	}
	// Overlapping circles: the nearest centre wins.
	if z, _ := ZoneAt(zs, 7750, 14900); z.Key != "kamensk-military" {
		t.Fatalf("nearest: %+v", z)
	}
	if Zones("enoch")[0].Key != "nadbor" {
		t.Fatalf("accents are dropped from keys: %s", Zones("enoch")[0].Key)
	}
}

func TestDecideHolder(t *testing.T) {
	s := func(pairs ...int) []FactionScore {
		out := []FactionScore{}
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, FactionScore{FactionID: int64(pairs[i]), Points: pairs[i+1]})
		}
		return out
	}
	cases := []struct {
		name   string
		holder int64
		scores []FactionScore
		want   int64
	}{
		{"nobody scores", 0, nil, 0},
		{"below the minimum", 0, s(1, 2), 0},
		{"first capture", 0, s(1, 3, 2, 1), 1},
		{"tie between challengers takes nothing", 0, s(1, 4, 2, 4), 0},
		{"holder defends a tie", 1, s(1, 5, 2, 5), 1},
		{"challenger with more takes it", 1, s(1, 5, 2, 6), 2},
		{"challenger needs the minimum even against nothing", 1, s(1, 1, 2, 2), 1},
		{"idle holder goes neutral", 1, s(2, 2), 0},
		{"idle holder loses to a challenger", 1, s(2, 3), 2},
		{"holder leads", 1, s(1, 9, 2, 3), 1},
	}
	for _, c := range cases {
		if got := DecideHolder(c.holder, c.scores, 3); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if IncomeShares(100, 3) != 33 || IncomeShares(2, 5) != 1 || IncomeShares(0, 4) != 0 || IncomeShares(100, 0) != 0 {
		t.Fatal("shares")
	}
}

func TestBattlePassLevelsAndRewards(t *testing.T) {
	if Level(0, 1000, 30) != 0 || Level(999, 1000, 30) != 0 || Level(1000, 1000, 30) != 1 || Level(10_000_000, 1000, 30) != 30 {
		t.Fatal("levels")
	}
	for _, levels := range []int{MinLevels, 30, 37, MaxLevels} {
		rw := DefaultRewards(levels)
		if err := ValidateRewards(rw, levels); err != nil {
			t.Fatalf("%d levels: default track is valid: %v", levels, err)
		}
		premium := 0
		for _, r := range rw {
			if r.Track == TrackPremium {
				premium++
			}
		}
		if premium != levels {
			t.Fatalf("every level has a premium reward: %d/%d", premium, levels)
		}
		last := rw[len(rw)-1]
		if last.Level != levels {
			t.Fatalf("the last level has a reward: %+v", last)
		}
	}
	bad := [][]Reward{
		{{Level: 0, Track: TrackFree, Kind: RewardPoints, Amount: 5}},
		{{Level: 31, Track: TrackFree, Kind: RewardPoints, Amount: 5}},
		{{Level: 1, Track: "GOLD", Kind: RewardPoints, Amount: 5}},
		{{Level: 1, Track: TrackFree, Kind: RewardPoints}},
		{{Level: 1, Track: TrackFree, Kind: RewardTitle, Text: "  "}},
		{{Level: 1, Track: TrackFree, Kind: RewardTitle, Text: strings.Repeat("x", 33)}},
		{{Level: 1, Track: TrackFree, Kind: RewardBadge, Text: "abcdefghi"}},
		{{Level: 1, Track: TrackFree, Kind: "SKIN"}},
		{{Level: 1, Track: TrackFree, Kind: RewardPoints, Amount: 1}, {Level: 1, Track: TrackFree, Kind: RewardPoints, Amount: 2}},
	}
	for i, b := range bad {
		if ValidateRewards(b, 30) == nil {
			t.Errorf("case %d should be refused", i)
		}
	}
}
