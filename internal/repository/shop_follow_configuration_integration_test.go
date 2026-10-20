//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
)

// Champion's own cfggameplay.json rewrite (map switch, stadium build) moves the digest Shop
// auto-delivery accepted; a digest that no longer matches Champion's "before" stays, so any other
// change still pauses delivery.
func TestShopAutoDeliveryFollowsChampionsOwnConfigWrite(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	fx := newSaaSFixture(t, db)
	repo := NewShopAutoDeliveryRepository(db.Pool)
	shaOld, shaNew, shaOther := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	if err := repo.SetEnabled(ctx, fx.OrgID, fx.InstallationID, true, fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	if err := repo.AcceptConfiguration(ctx, fx.OrgID, fx.InstallationID, shaOld, "dayzOffline.chernarusplus"); err != nil {
		t.Fatal(err)
	}
	accepted := func() string {
		t.Helper()
		var sha string
		if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(config_sha256,'') FROM shop_auto_delivery_settings WHERE installation_id=$1`, fx.InstallationID).Scan(&sha); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	if err := repo.FollowConfiguration(ctx, fx.InstallationID, shaOther, shaNew); err != nil || accepted() != shaOld {
		t.Fatalf("a different before must not move the digest: %q %v", accepted(), err)
	}
	if err := repo.FollowConfiguration(ctx, fx.InstallationID, shaOld, shaNew); err != nil || accepted() != shaNew {
		t.Fatalf("Champion's own write moves the digest: %q %v", accepted(), err)
	}
}
