package app

import (
	"encoding/json"
	"testing"
)

func TestParseSameVictimCooldownMinutes(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
		ok   bool
	}{
		{`{"rpPerKill":100}`, 5, true}, // omitted: an older website keeps the five-minute rule
		{`{"sameVictimCooldownMinutes":null}`, 5, true},
		{`{"sameVictimCooldownMinutes":0}`, 0, true},
		{`{"sameVictimCooldownMinutes":5}`, 5, true},
		{`{"sameVictimCooldownMinutes":30}`, 30, true},
		{`{"sameVictimCooldownMinutes":120}`, 120, true},
		{`{"sameVictimCooldownMinutes":-1}`, 0, false},
		{`{"sameVictimCooldownMinutes":121}`, 0, false},
		{`{"sameVictimCooldownMinutes":5.5}`, 0, false},
		{`{"sameVictimCooldownMinutes":"5"}`, 0, false},
		{`{"sameVictimCooldownMinutes":true}`, 0, false},
		{`{"sameVictimCooldownMinutes":[5]}`, 0, false},
	} {
		var req serverRankedSeasonRequest
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		got, ok := parseSameVictimCooldownMinutes(req.SameVictimCooldownMinutes)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}
