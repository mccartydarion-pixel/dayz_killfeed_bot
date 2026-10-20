//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

// An organization that subscribed again (S2) must not lose access when its older subscription
// (S1) ends: S1's terminal event leaves the row on S2. Events for S2, and a new live subscription
// replacing an ended one, still apply.
func TestStaleSubscriptionEndDoesNotOverwriteTheLiveOne(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fixture := newSaaSFixture(t, db)
	subs := NewSubscriptionRepository(db.Pool)
	if _, err := subs.EnsureTrial(ctx, fixture.OrgID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	apply := func(subID, status string) *Subscription {
		t.Helper()
		out, err := subs.ApplyProviderState(ctx, fixture.OrgID, ProviderState{Provider: ProviderStripe, ProviderCustomerID: "cus_1", ProviderSubscriptionID: subID, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	apply("sub_S1", SubscriptionActive)
	apply("sub_S2", SubscriptionActive)
	if got := apply("sub_S1", SubscriptionCanceled); got.ProviderSubscriptionID != "sub_S2" || got.Status != SubscriptionActive {
		t.Fatalf("S1 ending must leave the live S2 in place, got %s %s", got.ProviderSubscriptionID, got.Status)
	}
	if got := apply("sub_S2", SubscriptionPastDue); got.Status != SubscriptionPastDue {
		t.Fatalf("S2's own events still apply, got %s", got.Status)
	}
	if got := apply("sub_S2", SubscriptionCanceled); got.Status != SubscriptionCanceled {
		t.Fatalf("S2's own end applies, got %s", got.Status)
	}
	if got := apply("sub_S3", SubscriptionActive); got.ProviderSubscriptionID != "sub_S3" || got.Status != SubscriptionActive {
		t.Fatalf("a new subscription after the end applies, got %s %s", got.ProviderSubscriptionID, got.Status)
	}
}
