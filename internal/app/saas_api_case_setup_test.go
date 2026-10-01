package app

import (
	"context"
	"errors"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
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
