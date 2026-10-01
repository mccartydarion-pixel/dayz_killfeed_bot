package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEveryTemplateIsCreatable(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tpl := range Templates() {
		cfg := tpl.Config
		if tpl.NeedsWeapons {
			cfg = json.RawMessage(`{"weapon_names":["M4-A1"]}`)
		}
		p, err := ValidatePlan(tpl.Type, tpl.Name, tpl.Description, cfg, nil, now.Add(time.Duration(tpl.DurationHours)*time.Hour), [3]int{tpl.FirstPoints, tpl.SecondPoints, tpl.ThirdPoints}, now)
		if err != nil || !p.Immediate {
			t.Fatalf("%s: %v %+v", tpl.Key, err, p)
		}
	}
	if _, ok := TemplateByKey("sniper_weekend"); !ok {
		t.Fatal("template lookup is case-insensitive")
	}
}

func TestValidatePlanRules(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	later := now.Add(48 * time.Hour)
	ok := func(start *time.Time, end time.Time) error {
		_, err := ValidatePlan(TypeMostKills, "Frenzy", "", nil, start, end, [3]int{1000, 500, 250}, now)
		return err
	}
	if err := ok(&later, later.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if p, _ := ValidatePlan(TypeMostKills, "F", "", nil, &later, later.Add(time.Hour), [3]int{}, now); p.Immediate || !p.StartsAt.Equal(later) {
		t.Fatal("a future start schedules the event")
	}
	cases := map[string]error{
		"too short":      ok(nil, now.Add(10*time.Minute)),
		"too long":       ok(nil, now.Add(15*24*time.Hour)),
		"too far ahead":  ok(ptr(now.Add(61*24*time.Hour)), now.Add(62*24*time.Hour)),
		"end before now": ok(nil, now.Add(-time.Hour)),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
	bad := []struct {
		typ, name, cfg string
		prizes         [3]int
	}{
		{TypeBounty, "x", "{}", [3]int{}},
		{TypeHotZone, "x", "{}", [3]int{}},
		{TypeMostKills, "", "{}", [3]int{}},
		{TypeMostKills, strings.Repeat("x", 81), "{}", [3]int{}},
		{TypeMostKills, "x", `{"minimum_distanc":1}`, [3]int{}},
		{TypeWeaponChallenge, "x", `{"weapon_names":[]}`, [3]int{}},
		{TypeLongestKill, "x", `{"minimum_distance":9000}`, [3]int{}},
		{TypeMostKills, "x", "{}", [3]int{-1, 0, 0}},
		{TypeMostKills, "x", "{}", [3]int{MaxPrizePoints + 1, 0, 0}},
	}
	for _, b := range bad {
		if _, err := ValidatePlan(b.typ, b.name, "", json.RawMessage(b.cfg), nil, now.Add(2*time.Hour), b.prizes, now); err == nil {
			t.Errorf("%+v must be rejected", b)
		}
	}
	p, err := ValidatePlan(TypeWeaponChallenge, "W", "", json.RawMessage(`{"weapon_names":["  M4-A1 ", ""]}`), nil, now.Add(time.Hour), [3]int{}, now)
	if err != nil || p.Config.(WeaponChallengeConfig).WeaponNames[0] != "M4-A1" || len(p.Config.(WeaponChallengeConfig).WeaponNames) != 1 {
		t.Fatalf("weapon names are trimmed: %+v %v", p.Config, err)
	}
}

func ptr(t time.Time) *time.Time { return &t }
