package app

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
)

type memFlagStore struct{ rows []featureflags.Override }

func (m *memFlagStore) ListOverrides(context.Context) ([]featureflags.Override, error) {
	return m.rows, nil
}
func (m *memFlagStore) ServerInstallations(context.Context) (map[int64]int64, error) {
	return map[int64]int64{}, nil
}

// ownerAccessFixture makes organization 70 / installation 7 a platform owner's own, for one test.
func ownerAccessFixture(t *testing.T, overrides ...featureflags.Override) *App {
	t.Helper()
	entitlements.SetOwnerOrganizations(func(org int64) bool { return org == 70 })
	prev := entitlements.Enforced()
	entitlements.SetEnforced(true)
	t.Cleanup(func() { entitlements.SetOwnerOrganizations(nil); entitlements.SetEnforced(prev) })
	flags := featureflags.New(&memFlagStore{rows: overrides}, time.Hour)
	flags.SetOwnerInstallations(func(inst int64) bool { return inst == 7 })
	if err := flags.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &App{Config: &config.Config{}, FeatureFlags: flags}
}

// Map rotation is available for a platform owner's own installation with the environment switch
// off, no override and plan gating enforced. A customer's installation is exactly as before.
func TestMapRotationIsAvailableForAPlatformOwnerInstallation(t *testing.T) {
	a := ownerAccessFixture(t)
	if a.Config.MapRotationEnabled {
		t.Fatal("the environment default must be off for this test")
	}
	reason, planBlocked, err := a.mapRotationAvailable(context.Background(), 70, 7)
	if err != nil || reason != "" || planBlocked {
		t.Fatalf("owner installation: reason=%q planBlocked=%v err=%v", reason, planBlocked, err)
	}
	reason, planBlocked, err = a.mapRotationAvailable(context.Background(), 80, 8)
	if err != nil || reason != mapRotationReasonFlag || planBlocked {
		t.Fatalf("customer installation: reason=%q planBlocked=%v err=%v", reason, planBlocked, err)
	}
	// The owner's organization, but another organization's installation id: not unlocked.
	if a.mapRotationFlag(8) {
		t.Fatal("only the owner's own installations get the flag")
	}
}

// The owner can still switch a feature off for their own server: the override wins.
func TestPlatformOwnerCanSwitchAFeatureOffForTheirOwnServer(t *testing.T) {
	a := ownerAccessFixture(t, featureflags.Override{InstallationID: 7, Flag: featureflags.MapRotation, Enabled: false})
	reason, _, err := a.mapRotationAvailable(context.Background(), 70, 7)
	if err != nil || reason != mapRotationReasonFlag {
		t.Fatalf("an OFF override must win: reason=%q err=%v", reason, err)
	}
	a.Config.MapRotationEnabled = true
	if a.mapRotationFlag(7) {
		t.Fatal("an OFF override wins over the environment default too")
	}
}

// organizationPlan is the seam for the app's plan gates: with no subscription store it still
// marks a platform owner's organization as unrestricted and nobody else.
func TestOrganizationPlanMarksPlatformOwnerOrganizations(t *testing.T) {
	a := ownerAccessFixture(t)
	owner, err := a.organizationPlan(context.Background(), 70)
	if err != nil || !owner.OwnerAccess() {
		t.Fatalf("owner plan: %+v %v", owner, err)
	}
	for _, k := range []entitlements.Key{entitlements.MapRotation, entitlements.FightReplay, entitlements.Economy, entitlements.Retention, entitlements.PerkStore} {
		if !entitlements.Has(owner, k) {
			t.Fatalf("owner organization is missing %s", k)
		}
	}
	if entitlements.FactionLimit(owner) != 0 {
		t.Fatal("owner organization has a faction limit")
	}
	customer, err := a.organizationPlan(context.Background(), 80)
	if err != nil || customer.OwnerAccess() {
		t.Fatalf("customer plan: %+v %v", customer, err)
	}
}

// Custom embeds follow the same rule, but owner access cannot conjure a renderer.
func TestCustomEmbedsOwnerAccessNeedsARenderer(t *testing.T) {
	a := ownerAccessFixture(t)
	if a.customEmbedsFor(7) || a.flagOwnerAccess(7, featureflags.CustomEmbeds) {
		t.Fatal("with no renderer custom embeds stay off, owner or not")
	}
	if !a.flagOwnerAccess(7, featureflags.MapRotation) || a.flagOwnerAccess(8, featureflags.MapRotation) || a.flagOwnerAccess(7, featureflags.ShopCanary) {
		t.Fatal("flagOwnerAccess answers are wrong")
	}
}
