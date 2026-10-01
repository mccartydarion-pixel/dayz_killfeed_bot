package caseintel

import "testing"

func TestNoModuleIsReleasedWithoutReview(t *testing.T) {
	for _, d := range ClientCatalog() {
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
