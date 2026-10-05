package app

import (
	"encoding/json"
	"strings"
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

func TestParseSameVictimCooldownChange(t *testing.T) {
	for _, tc := range []struct {
		body    string
		want    int
		problem string // substring of the refusal; "" = accepted
	}{
		{`{"sameVictimCooldownMinutes":0}`, 0, ""},
		{`{"sameVictimCooldownMinutes":2}`, 2, ""},
		{`{"sameVictimCooldownMinutes":120}`, 120, ""},
		{`{"sameVictimCooldownMinutes":2,"confirm":"x","somethingElse":1}`, 2, ""}, // unknown fields are ignored, as everywhere
		{`{}`, 0, "whole number of minutes from 0 to 120"},                         // required here: no silent default
		{`{"sameVictimCooldownMinutes":null}`, 0, "whole number of minutes from 0 to 120"},
		{`{"sameVictimCooldownMinutes":-1}`, 0, "whole number of minutes from 0 to 120"},
		{`{"sameVictimCooldownMinutes":121}`, 0, "whole number of minutes from 0 to 120"},
		{`{"sameVictimCooldownMinutes":5.5}`, 0, "whole number of minutes from 0 to 120"},
		{`{"sameVictimCooldownMinutes":"5"}`, 0, "whole number of minutes from 0 to 120"},
		{`{"sameVictimCooldownMinutes":5,"rpPerKill":200}`, 0, "frozen"},
		{`{"sameVictimCooldownMinutes":5,"thresholds":[1,2,3,4,5,6,7]}`, 0, "frozen"},
	} {
		var req serverRankedSeasonWaitRequest
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		got, problem := parseSameVictimCooldownChange(req)
		if tc.problem == "" && (problem != "" || got != tc.want) {
			t.Errorf("%s: got (%d, %q), want %d", tc.body, got, problem, tc.want)
		}
		if tc.problem != "" && !strings.Contains(problem, tc.problem) {
			t.Errorf("%s: got problem %q, want one containing %q", tc.body, problem, tc.problem)
		}
	}
}
