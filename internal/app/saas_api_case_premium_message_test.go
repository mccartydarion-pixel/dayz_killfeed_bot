package app

import (
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

// A Watch-only server calling a Pro-only route must be told Pro is required, not that it has no
// C.A.S.E. access (staging QA, Phase 6.21: the Pro export denial implied Watch was not active).
func TestCasePremiumRequiredMessageNamesTheRequiredTier(t *testing.T) {
	for cap, want := range map[casebilling.Capability]string{
		casebilling.CapWatch:   "C.A.S.E. Watch access is required for the selected server",
		casebilling.CapPro:     "C.A.S.E. Pro access is required for the selected server",
		casebilling.CapCommand: "C.A.S.E. Command access is required for the selected server",
	} {
		if got := casePremiumRequiredMessage(cap); got != want {
			t.Errorf("%s: %q, want %q", cap, got, want)
		}
	}
	if got := casePremiumRequiredMessage("case.unknown"); strings.Contains(got, "Watch") || strings.Contains(got, "Pro ") {
		t.Errorf("unknown capability must not name a tier: %q", got)
	}
	if strings.Contains(casePremiumRequiredMessage(casebilling.CapPro), "does not have") {
		t.Error("Pro denial must not imply the server lacks C.A.S.E. access entirely")
	}
}
