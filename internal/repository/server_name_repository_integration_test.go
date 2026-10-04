//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
)

type nameRow struct {
	display, provider string
	custom            bool
}

func readNameRow(t *testing.T, repo *ServerNameRepository, serverID int64) nameRow {
	t.Helper()
	var r nameRow
	if err := repo.pool.QueryRow(context.Background(), `SELECT COALESCE(display_name,''), COALESCE(provider_display_name,''), display_name_custom FROM game_servers WHERE id=$1`, serverID).
		Scan(&r.display, &r.provider, &r.custom); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestServerNameSyncStorageAndCustomRule(t *testing.T) {
	db := saasIntegrationDB(t)
	fx := newSaaSFixture(t, db)
	ctx := context.Background()
	names := NewServerNameRepository(db.Pool)
	admin := NewClientAdminRepository(db.Pool)
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET display_name='Connected name', display_name_custom=FALSE, provider_display_name=NULL, provider='NITRADO', active=TRUE WHERE id=$1`, fx.ServerRowID); err != nil {
		t.Fatal(err)
	}

	// The Nitrado name changed: the display name follows.
	changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Nitrado v2")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := readNameRow(t, names, fx.ServerRowID); got != (nameRow{"Nitrado v2", "Nitrado v2", false}) {
		t.Fatalf("after sync: %+v", got)
	}
	// The same name again is not a change.
	if changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Nitrado v2"); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	// An empty name is refused and nothing is blanked.
	if _, err := names.RecordProviderName(ctx, fx.ServerRowID, ""); err == nil {
		t.Fatal("an empty provider name must be refused")
	}
	if got := readNameRow(t, names, fx.ServerRowID); got.display != "Nitrado v2" {
		t.Fatalf("name blanked: %+v", got)
	}

	// The owner types a name: custom, and the sync no longer overwrites it but still records Nitrado's.
	st, err := admin.SetServerDisplayName(ctx, fx.GuildRowID, fx.ServerRowID, "Owner's pick")
	if err != nil || !st.Custom || st.DisplayName != "Owner's pick" || st.ProviderName != "Nitrado v2" {
		t.Fatalf("rename: %+v err %v", st, err)
	}
	if changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Nitrado v3"); err != nil || changed {
		t.Fatalf("a custom name must win: changed=%v err=%v", changed, err)
	}
	if got := readNameRow(t, names, fx.ServerRowID); got != (nameRow{"Owner's pick", "Nitrado v3", true}) {
		t.Fatalf("custom wins: %+v", got)
	}

	// A re-selection of the same service (both upserts) keeps the custom name.
	if _, err := NewSaaSServerRepository(db.Pool).UpsertForInstallation(ctx, fx.OrgID, fx.GuildRowID, GameServer{Provider: "NITRADO", ProviderServiceID: providerServiceIDOf(t, names, fx.ServerRowID),
		Game: "DayZ", Platform: "PLAYSTATION", DisplayName: "Nitrado v4", ProviderName: "Nitrado v4", Status: "ACTIVE", Active: true}); err != nil {
		t.Fatal(err)
	}
	if got := readNameRow(t, names, fx.ServerRowID); got != (nameRow{"Owner's pick", "Nitrado v4", true}) {
		t.Fatalf("reconnect must keep a custom name: %+v", got)
	}

	// Typing exactly the current Nitrado name is not custom; the sync follows again.
	st, err = admin.SetServerDisplayName(ctx, fx.GuildRowID, fx.ServerRowID, "Nitrado v4")
	if err != nil || st.Custom {
		t.Fatalf("the Nitrado name is not a custom name: %+v err %v", st, err)
	}
	if changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Nitrado v5"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	// Clearing a custom name goes back to the Nitrado name.
	if _, err := admin.SetServerDisplayName(ctx, fx.GuildRowID, fx.ServerRowID, "Custom again"); err != nil {
		t.Fatal(err)
	}
	st, err = admin.SetServerDisplayName(ctx, fx.GuildRowID, fx.ServerRowID, "")
	if err != nil || st.Custom || st.DisplayName != "Nitrado v5" {
		t.Fatalf("clear: %+v err %v", st, err)
	}

	// Clearing while no Nitrado name is known is refused and changes nothing.
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET provider_display_name=NULL, display_name='Typed', display_name_custom=TRUE WHERE id=$1`, fx.ServerRowID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.SetServerDisplayName(ctx, fx.GuildRowID, fx.ServerRowID, ""); !errors.Is(err, ErrNoProviderName) {
		t.Fatalf("expected ErrNoProviderName, got %v", err)
	}
	if _, err := admin.SetServerDisplayName(ctx, fx.GuildRowID+987654, fx.ServerRowID, "x"); !errors.Is(err, ErrInstallationScopeNotFound) {
		t.Fatalf("another guild's server must not be renamed, got %v", err)
	}
	// First sight of the Nitrado name: a custom name that is exactly that name stops being custom.
	if changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Typed"); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := readNameRow(t, names, fx.ServerRowID); got != (nameRow{"Typed", "Typed", false}) {
		t.Fatalf("first sight: %+v", got)
	}
}

func providerServiceIDOf(t *testing.T, repo *ServerNameRepository, serverID int64) string {
	t.Helper()
	var id string
	if err := repo.pool.QueryRow(context.Background(), `SELECT provider_service_id FROM game_servers WHERE id=$1`, serverID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestServerNameSyncTargetsSkipSuspendedAndInactive(t *testing.T) {
	db := saasIntegrationDB(t)
	fx := newSaaSFixture(t, db)
	ctx := context.Background()
	names := NewServerNameRepository(db.Pool)
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET provider='NITRADO', active=TRUE WHERE id=$1`, fx.ServerRowID); err != nil {
		t.Fatal(err)
	}
	listed := func() bool {
		t.Helper()
		targets, err := names.ListSyncTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if target.ServerID == fx.ServerRowID {
				if target.GuildID != fx.GuildRowID || target.OrganizationID == nil || *target.OrganizationID != fx.OrgID || target.ProviderServiceID == "" {
					t.Fatalf("unexpected target %+v", target)
				}
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("an active Nitrado server must be a sync target")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE installations SET suspended_at=NOW() WHERE id=$1`, fx.InstallationID); err != nil {
		t.Fatal(err)
	}
	if listed() {
		t.Fatal("a suspended installation's server must be skipped")
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE installations SET suspended_at=NULL WHERE id=$1`, fx.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET active=FALSE WHERE id=$1`, fx.ServerRowID); err != nil {
		t.Fatal(err)
	}
	if listed() {
		t.Fatal("an inactive server must be skipped")
	}
	// An inactive server is not written either.
	if changed, err := names.RecordProviderName(ctx, fx.ServerRowID, "Ignored"); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}
