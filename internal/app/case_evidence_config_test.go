package app

import "testing"

func TestCASEEvidenceOptInRequiresExplicitServerAllowlist(t *testing.T) {
	t.Setenv("CASE_EVIDENCE_ENABLED", "false")
	t.Setenv("CASE_EVIDENCE_SERVER_IDS", "1,2")
	if caseEvidenceEnabledForServer(1) {
		t.Fatal("disabled collector must stay off")
	}
	t.Setenv("CASE_EVIDENCE_ENABLED", "true")
	for _, id := range []int64{1, 2} {
		if !caseEvidenceEnabledForServer(id) {
			t.Fatalf("allowlisted server %d not enabled", id)
		}
	}
	for _, id := range []int64{0, 3, 100} {
		if caseEvidenceEnabledForServer(id) {
			t.Fatalf("unlisted server %d must remain disabled", id)
		}
	}
	t.Setenv("CASE_EVIDENCE_SERVER_IDS", "")
	if caseEvidenceEnabledForServer(1) {
		t.Fatal("empty allowlist must fail closed")
	}
	t.Setenv("CASE_EVIDENCE_SERVER_IDS", "not-an-id, -1")
	if caseEvidenceEnabledForServer(1) {
		t.Fatal("malformed allowlist must fail closed")
	}
}

func TestCASEBuildEvidenceRequiresBothAllowlistLayers(t *testing.T) {
	t.Setenv("CASE_EVIDENCE_ENABLED", "true")
	t.Setenv("CASE_EVIDENCE_SERVER_IDS", "11")
	t.Setenv("CASE_BUILD_EVIDENCE_ENABLED", "false")
	t.Setenv("CASE_BUILD_EVIDENCE_SERVER_IDS", "11")
	if caseBuildEvidenceEnabledForServer(11) {
		t.Fatal("build evidence off by default")
	}
	t.Setenv("CASE_BUILD_EVIDENCE_ENABLED", "true")
	for _, id := range []int64{0, 12} {
		if caseBuildEvidenceEnabledForServer(id) {
			t.Fatalf("other server enabled: %d", id)
		}
	}
	if !caseBuildEvidenceEnabledForServer(11) {
		t.Fatal("both allowlists should enable selected server")
	}
	t.Setenv("CASE_BUILD_EVIDENCE_SERVER_IDS", "")
	if caseBuildEvidenceEnabledForServer(11) {
		t.Fatal("empty build allowlist accepted")
	}
	t.Setenv("CASE_BUILD_EVIDENCE_SERVER_IDS", "11")
	t.Setenv("CASE_EVIDENCE_ENABLED", "false")
	if caseBuildEvidenceEnabledForServer(11) {
		t.Fatal("base collector disabled")
	}
}
