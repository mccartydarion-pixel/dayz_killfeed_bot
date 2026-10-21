package caseintel

import "testing"

func TestOnlySuspiciousLoginsIsReleased(t *testing.T) {
	if !ModuleReleased("CASE-LOGIN-001") {
		t.Fatal("Suspicious Logins should be released to staff alerts")
	}
	got := ReleasedThresholds("CASE-LOGIN-001")
	if got == nil || got.Strict != 2 || got.Balanced != 3 || got.Relaxed != 4 || got.MinimumEvidence != 2 {
		t.Fatalf("Suspicious Logins thresholds = %+v", got)
	}
	for _, d := range ClientCatalog() {
		if d.ID == "CASE-LOGIN-001" {
			continue
		}
		if ModuleReleased(d.ID) || ReleasedThresholds(d.ID) != nil {
			t.Fatalf("%s is released to staff alerts without a recorded review", d.ID)
		}
	}
	if ModuleReleased("CASE-UNKNOWN-001") {
		t.Fatal("unknown module released")
	}
}

func TestReleasedThresholdsNeedApprovalAndCatalogMode(t *testing.T) {
	saved := releasedThresholds
	defer func() { releasedThresholds = saved }()
	releasedThresholds = map[string]ValidatedThresholds{
		"CASE-LOGIN-001":    {Approved: false, MinimumEvidence: 1, Relaxed: 3, Balanced: 2, Strict: 1},
		"CASE-TELEPORT-001": {Approved: true, MinimumEvidence: 1, Relaxed: 3, Balanced: 2, Strict: 1},
	}
	if ReleasedThresholds("CASE-LOGIN-001") != nil {
		t.Fatal("unapproved thresholds returned")
	}
	if ReleasedThresholds("CASE-TELEPORT-001") == nil {
		t.Fatal("approved thresholds not returned")
	}
	// Thresholds alone do not release a module still BLOCKED in the catalog.
	if ModuleReleased("CASE-TELEPORT-001") {
		t.Fatal("module released while BLOCKED in the catalog")
	}
}
