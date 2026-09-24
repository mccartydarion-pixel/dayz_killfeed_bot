package dayzmap

import (
	"math"
	"testing"
)

func TestLookupOnlyVerifiedMaps(t *testing.T) {
	for _, key := range []string{"chernarusplus", "ChernarusPlus", " enoch "} {
		if _, ok := Lookup(key); !ok {
			t.Errorf("%q should resolve", key)
		}
	}
	for _, key := range []string{"", "sakhal", "deerisle", "namalsk", "chernarus", "livonia"} {
		if _, ok := Lookup(key); ok {
			t.Errorf("%q must be unsupported (unverified bounds or not a world name)", key)
		}
	}
	if len(Supported()) != 2 || Supported()[0].Key != "chernarusplus" {
		t.Fatalf("%+v", Supported())
	}
}

func TestContains(t *testing.T) {
	ch, _ := Lookup("chernarusplus")
	lv, _ := Lookup("enoch")
	for name, c := range map[string]struct {
		m    Map
		x, z float64
		want bool
	}{
		"inside":         {ch, 7500.25, 8300.5, true},
		"corner":         {ch, 0, 0, true},
		"far edge":       {ch, 15360, 15360, true},
		"negative":       {ch, -0.01, 10, false},
		"beyond":         {ch, 15360.01, 10, false},
		"livonia inside": {lv, 12000, 12800, true},
		"livonia beyond": {lv, 13000, 100, false},
		"nan":            {ch, math.NaN(), 1, false},
		"inf":            {ch, 1, math.Inf(1), false},
		"negative inf":   {ch, math.Inf(-1), 1, false},
	} {
		if got := c.m.Contains(c.x, c.z); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
