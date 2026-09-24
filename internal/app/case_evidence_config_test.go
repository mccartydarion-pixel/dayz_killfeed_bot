package app

import "testing"

func TestCASEEvidenceOptInRequiresExplicitServerAllowlist(t *testing.T) {
	t.Setenv("CASE_EVIDENCE_ENABLED","false")
	t.Setenv("CASE_EVIDENCE_SERVER_IDS","1,2")
	if caseEvidenceEnabledForServer(1) {t.Fatal("disabled collector must stay off")}
	t.Setenv("CASE_EVIDENCE_ENABLED","true")
	for _,id:=range []int64{1,2} {
		if !caseEvidenceEnabledForServer(id){t.Fatalf("allowlisted server %d not enabled",id)}
	}
	for _,id:=range []int64{0,3,100} {
		if caseEvidenceEnabledForServer(id){t.Fatalf("unlisted server %d must remain disabled",id)}
	}
	t.Setenv("CASE_EVIDENCE_SERVER_IDS","")
	if caseEvidenceEnabledForServer(1){t.Fatal("empty allowlist must fail closed")}
	t.Setenv("CASE_EVIDENCE_SERVER_IDS","not-an-id, -1")
	if caseEvidenceEnabledForServer(1){t.Fatal("malformed allowlist must fail closed")}
}
