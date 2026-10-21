package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeOptins struct {
	ids []int64
	err error
}

func (f fakeOptins) EnabledServerIDs(context.Context) ([]int64, error) { return f.ids, f.err }

func TestCaseEvidenceSelfServe(t *testing.T) {
	t.Setenv("CASE_EVIDENCE_ENABLED", "true")
	t.Setenv("CASE_EVIDENCE_SERVER_IDS", "1")
	t.Setenv("CASE_EVIDENCE_SELF_SERVE", "")
	loadCaseEvidenceOptins(context.Background(), fakeOptins{ids: []int64{7}})
	if caseEvidenceEnvForServer(7) {
		t.Fatal("self-serve off: an owner's choice must not count")
	}
	if !caseEvidenceEnvForServer(1) {
		t.Fatal("allowlist still counts")
	}
	t.Setenv("CASE_EVIDENCE_SELF_SERVE", "true")
	loadCaseEvidenceOptins(context.Background(), fakeOptins{ids: []int64{7}})
	if !caseEvidenceEnvForServer(7) || caseEvidenceEnvForServer(8) {
		t.Fatal("self-serve on: only opted-in servers")
	}
	loadCaseEvidenceOptins(context.Background(), fakeOptins{err: errors.New("down")})
	if !caseEvidenceEnvForServer(7) {
		t.Fatal("a failed reload keeps the last snapshot")
	}
	t.Setenv("CASE_EVIDENCE_ENABLED", "false")
	if caseEvidenceEnvForServer(7) || caseEvidenceSelfServe() {
		t.Fatal("the master switch still wins")
	}
	caseOptinMu.Lock()
	caseOptinServers = map[int64]bool{}
	caseOptinMu.Unlock()
}

func TestCaseSetupWords(t *testing.T) {
	for state, want := range map[string]string{killfeed.ADMHealthy: setupOK, killfeed.ADMQuiet: setupWait, killfeed.ADMTransportError: setupAction,
		killfeed.ADMSourceLagging: setupWait, killfeed.ADMWorkerStalled: setupWait, "UNKNOWN": setupWait} {
		if got, detail := caseFeedStep(state, ""); got != want || detail == "" {
			t.Fatalf("%s: %s %q", state, got, detail)
		}
	}
	cases := []struct {
		ev     caseSetupEvidence
		state  string
		action string
	}{
		{caseSetupEvidence{}, setupLocked, ""},
		{caseSetupEvidence{SelfServe: true}, setupAction, "TOGGLE_EVIDENCE"},
		{caseSetupEvidence{SelfServe: true, Configured: true, OwnerChoice: true}, setupWait, "TOGGLE_EVIDENCE"},
		{caseSetupEvidence{SelfServe: true, Configured: true, Running: true, OwnerChoice: true}, setupOK, "TOGGLE_EVIDENCE"},
		{caseSetupEvidence{Running: true}, setupWait, ""},
		{caseSetupEvidence{SetByChampions: true, Configured: true, Running: true, SelfServe: true}, setupOK, ""},
		{caseSetupEvidence{SetByChampions: true, SelfServe: true}, setupLocked, ""},
		{caseSetupEvidence{Configured: true, Running: true}, setupOK, ""},
	}
	for _, c := range cases {
		if s := caseEvidenceStep(c.ev); s.State != c.state || s.Action != c.action || s.Detail == "" {
			t.Fatalf("%+v: %+v", c.ev, s)
		}
	}
}

func TestCaseBaseBoostSteps(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	states := func(s []caseSetupStep) []string {
		out := make([]string, len(s))
		for i, x := range s {
			if x.Detail == "" {
				t.Fatalf("empty detail: %+v", x)
			}
			out[i] = x.State
		}
		return out
	}
	cases := []struct {
		name                string
		configured, running bool
		ready               repository.BaseBoostReadiness
		review              repository.CaseVerdictSummary
		released            bool
		want                []string
	}{
		{"nothing set up", false, false, repository.BaseBoostReadiness{}, repository.CaseVerdictSummary{}, false,
			[]string{setupLocked, setupWait, setupAction, setupTesting}},
		{"waiting for restart", true, false, repository.BaseBoostReadiness{Bases: 2}, repository.CaseVerdictSummary{}, false,
			[]string{setupWait, setupWait, setupOK, setupTesting}},
		{"running, no build logging", true, true, repository.BaseBoostReadiness{Bases: 1}, repository.CaseVerdictSummary{}, false,
			[]string{setupOK, setupAction, setupOK, setupTesting}},
		{"running, logging stopped", true, true, repository.BaseBoostReadiness{LastBuildAt: &old, Bases: 1}, repository.CaseVerdictSummary{Reviewed: 3, FalseAlarms: 1, DaysReviewing: 2}, false,
			[]string{setupOK, setupAction, setupOK, setupTesting}},
		{"all ready", true, true, repository.BaseBoostReadiness{BuildLines: 40, Bases: 3}, repository.CaseVerdictSummary{}, false,
			[]string{setupOK, setupOK, setupOK, setupTesting}},
		{"released", true, true, repository.BaseBoostReadiness{BuildLines: 1, Bases: 1}, repository.CaseVerdictSummary{}, true,
			[]string{setupOK, setupOK, setupOK, setupOK}},
	}
	for _, c := range cases {
		got := caseBaseBoostSteps(c.configured, c.running, c.ready, c.review, c.released, now)
		if s := states(got); strings.Join(s, ",") != strings.Join(c.want, ",") {
			t.Fatalf("%s: %v want %v", c.name, s, c.want)
		}
	}
	stopped := caseBaseBoostSteps(true, true, repository.BaseBoostReadiness{LastBuildAt: &old}, repository.CaseVerdictSummary{}, false, now)
	if !strings.Contains(stopped[1].Detail, "10 days ago") {
		t.Fatalf("last build age: %q", stopped[1].Detail)
	}
	scored := caseBaseBoostSteps(true, true, repository.BaseBoostReadiness{BuildLines: 1}, repository.CaseVerdictSummary{Reviewed: 1, FalseAlarms: 1, DaysReviewing: 1}, false, now)
	if !strings.Contains(scored[3].Detail, "1 reviewed, 1 false alarm, 1 day") || !strings.Contains(scored[1].Detail, "1 build or placement line ") {
		t.Fatalf("singular wording: %q / %q", scored[3].Detail, scored[1].Detail)
	}
}
