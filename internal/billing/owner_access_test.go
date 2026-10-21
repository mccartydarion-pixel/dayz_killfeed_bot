package billing

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func withOwnerOrganization(t *testing.T, id int64) {
	t.Helper()
	entitlements.SetOwnerOrganizations(func(org int64) bool { return org == id })
	t.Cleanup(func() { entitlements.SetOwnerOrganizations(nil) })
}

func lockedOutSubscriptions(now time.Time) map[string]*repository.Subscription {
	past := now.Add(-48 * time.Hour)
	return map[string]*repository.Subscription{
		"no row":            nil,
		"trial expired":     {Plan: repository.SubscriptionTrial, Status: repository.SubscriptionTrial, TrialEndsAt: &past},
		"trial used":        {Plan: repository.PlanNone, Status: repository.SubscriptionInactive},
		"stripe canceled":   {Plan: "PREMIUM", Status: repository.SubscriptionCanceled, ProviderSubscriptionID: "sub_1"},
		"stripe suspended":  {Plan: "PREMIUM", Status: repository.SubscriptionSuspended, ProviderSubscriptionID: "sub_1"},
		"grant lapsed":      {Plan: "PREMIUM", Status: repository.SubscriptionActive, OwnerGrantUntil: &past},
		"revoked by status": {Plan: "PREMIUM", Status: repository.SubscriptionCanceled},
	}
}

// A platform owner's own organization is never told "billing required", whatever its
// subscription row says; the row itself is reported unchanged. Every other organization gets
// exactly StateOf.
func TestStateForNeverLocksOutAPlatformOwnerOrganization(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	withOwnerOrganization(t, 7)
	for name, sub := range lockedOutSubscriptions(now) {
		plain := StateOf(sub, now)
		if !plain.BillingRequired {
			t.Fatalf("%s: the fixture must be a locked-out state", name)
		}
		customer := StateFor(8, sub, now)
		if !reflect.DeepEqual(customer, plain) {
			t.Fatalf("%s: a customer organization must get exactly StateOf: %+v vs %+v", name, customer, plain)
		}
		owner := StateFor(7, sub, now)
		if owner.BillingRequired || !owner.PlatformOwnerAccess {
			t.Fatalf("%s: owner organization is locked out: %+v", name, owner)
		}
		owner.BillingRequired, owner.PlatformOwnerAccess = plain.BillingRequired, false
		if !reflect.DeepEqual(owner, plain) {
			t.Fatalf("%s: everything but the lock-out must be reported as it is", name)
		}
	}
}

func TestInstallationLimitForPlatformOwnerOrganization(t *testing.T) {
	withOwnerOrganization(t, 7)
	c := mustCatalog(t)
	for name, sub := range lockedOutSubscriptions(time.Now()) {
		if got, want := InstallationLimitFor(8, sub, c), InstallationLimit(sub, c); got != want {
			t.Fatalf("%s: customer limit %d, want %d", name, got, want)
		}
		if got := InstallationLimitFor(7, sub, c); got != OwnerInstallations {
			t.Fatalf("%s: owner limit %d", name, got)
		}
	}
	if InstallationLimitFor(7, nil, nil) != OwnerInstallations || InstallationLimitFor(8, nil, nil) != TrialInstallations {
		t.Fatal("limits without a catalog")
	}
}

// The subscription summary the website reads: an owner organization on a restricted, ended plan
// still shows that plan and status, with every entitlement and no billing wall.
func TestSummaryForPlatformOwnerOrganization(t *testing.T) {
	prev := entitlements.Enforced()
	entitlements.SetEnforced(true)
	t.Cleanup(func() { entitlements.SetEnforced(prev) })
	withOwnerOrganization(t, 7)
	store := newFakeStore()
	for _, org := range []int64{7, 8} {
		store.byOrg[org] = &repository.Subscription{OrganizationID: org, Plan: entitlements.PlanSurvivor, Status: repository.SubscriptionCanceled, ProviderSubscriptionID: "sub_x"}
	}
	svc := NewService(store, mustCatalog(t), nil, Options{})
	all := len(entitlements.Resolve(entitlements.ForOrganization(7, "")))

	owner, err := svc.Summary(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if owner.Plan != entitlements.PlanSurvivor || owner.Status != repository.SubscriptionCanceled {
		t.Fatalf("the row must be reported as it is: %+v", owner)
	}
	if !owner.PlatformOwnerAccess || owner.BillingRequired || len(owner.Entitlements) != all {
		t.Fatalf("owner summary: access=%v billingRequired=%v entitlements=%d/%d", owner.PlatformOwnerAccess, owner.BillingRequired, len(owner.Entitlements), all)
	}
	customer, err := svc.Summary(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if customer.PlatformOwnerAccess || !customer.BillingRequired || len(customer.Entitlements) >= all {
		t.Fatalf("customer summary changed: access=%v billingRequired=%v entitlements=%d", customer.PlatformOwnerAccess, customer.BillingRequired, len(customer.Entitlements))
	}
	// No subscription row at all.
	store.byOrg = map[int64]*repository.Subscription{}
	if s, _ := svc.Summary(context.Background(), 7); s == nil || s.BillingRequired || !s.PlatformOwnerAccess || len(s.Entitlements) != all {
		t.Fatalf("owner organization with no row: %+v", s)
	}
	if s, _ := svc.Summary(context.Background(), 8); s == nil || !s.BillingRequired || s.PlatformOwnerAccess {
		t.Fatalf("customer organization with no row: %+v", s)
	}
}
