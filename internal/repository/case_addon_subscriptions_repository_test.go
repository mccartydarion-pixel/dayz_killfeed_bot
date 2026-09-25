package repository

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/casebilling"
)

func TestCaseAddonAccessInputUsesIndependentTenantScope(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	end := now.Add(time.Hour)
	selected := int64(300)
	record := CaseAddonSubscription{
		OrganizationID: 100, InstallationID: 200, GameServerID: 300,
		Tier: string(casebilling.Pro), Status: "ACTIVE", Provider: "stripe",
		ProviderSubscriptionID: "sub_123", ProviderPriceID: "price_123",
		CurrentPeriodEnd: &end, PaidThrough: &end, FounderTrialGranted: true, SelectedGameServerID: &selected,
	}
	input := record.AccessInput(100, 200, "ACTIVE", true, casebilling.Pro)
	if !casebilling.Has(input, casebilling.CapPro, now) {
		t.Fatal("matching verified source should grant Pro")
	}
	for _, scope := range [][2]int64{{101, 200}, {100, 201}, {101, 201}} {
		mismatch := record.AccessInput(scope[0], scope[1], "ACTIVE", true, casebilling.Pro)
		if got := casebilling.Resolve(mismatch, now); len(got) != 0 {
			t.Fatalf("foreign authenticated scope %v granted %v", scope, got)
		}
	}
	selected = 301
	moved := record.AccessInput(100, 200, "ACTIVE", true, casebilling.Pro)
	if got := casebilling.Resolve(moved, now); len(got) != 0 {
		t.Fatalf("moved game server inherited add-on: %v", got)
	}
}
