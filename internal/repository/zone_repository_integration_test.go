//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Champion Phase 4 (docs/ZONES_UAV_RADAR.md) integration tests: zone CRUD/tenant isolation,
// ignore/authorized/ban lists, and the persisted presence/intrusion tables the intrusion engine
// (internal/killfeed) reads and writes - all against a real PostgreSQL. Reuses saas_repository_
// integration_test.go's saasIntegrationDB/newSaaSFixture (same package) rather than a second
// organization/installation fixture builder.

func newZoneTestWorld(t *testing.T) (*ZoneRepository, saasFixture, func(name string) int64) {
	t.Helper()
	db := saasIntegrationDB(t)
	fixture := newSaaSFixture(t, db)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	seedPlayer := func(name string) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
			fixture.GuildRowID, fmt.Sprintf("zone-%d-%d", suffix, time.Now().UnixNano()), name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	return NewZoneRepository(db.Pool), fixture, seedPlayer
}

func TestZoneCRUDLifecycle(t *testing.T) {
	repo, fx, _ := newZoneTestWorld(t)
	ctx := context.Background()

	zone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "North Base", ZoneTypeRestricted, 100, 200, 50, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	if zone.Name != "North Base" || zone.Radius != 50 || zone.Enabled != true {
		t.Fatalf("unexpected created zone: %+v", zone)
	}

	got, err := repo.GetZone(ctx, fx.InstallationID, zone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != zone.ID {
		t.Fatalf("get mismatch")
	}

	list, err := repo.ListZones(ctx, fx.InstallationID)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 listed zone, got %d (err=%v)", len(list), err)
	}

	newName := "North Base Renamed"
	enabled := false
	updated, err := repo.UpdateZone(ctx, fx.InstallationID, zone.ID, ZoneUpdate{Name: &newName, Enabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != newName || updated.Enabled {
		t.Fatalf("update did not apply: %+v", updated)
	}

	deleted, err := repo.DeleteZone(ctx, fx.InstallationID, zone.ID)
	if err != nil || !deleted {
		t.Fatalf("expected delete to succeed, err=%v deleted=%v", err, deleted)
	}
	if _, err := repo.GetZone(ctx, fx.InstallationID, zone.ID); err != ErrZoneNotFound {
		t.Fatalf("expected ErrZoneNotFound after delete, got %v", err)
	}
}

func TestZoneCrossTenantIsolation(t *testing.T) {
	repoA, fxA, _ := newZoneTestWorld(t)
	_, fxB, _ := newZoneTestWorld(t)
	ctx := context.Background()

	zone, err := repoA.CreateZone(ctx, fxA.InstallationID, fxA.GuildRowID, fxA.ServerRowID, "A Zone", ZoneTypeSafezone, 0, 0, 10, nil, 300, fxA.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := repoA.GetZone(ctx, fxB.InstallationID, zone.ID); err != ErrZoneNotFound {
		t.Fatalf("organization B must never resolve organization A's zone, got %v", err)
	}
	if deleted, _ := repoA.DeleteZone(ctx, fxB.InstallationID, zone.ID); deleted {
		t.Fatal("organization B must never be able to delete organization A's zone")
	}
	if updated, err := repoA.UpdateZone(ctx, fxB.InstallationID, zone.ID, ZoneUpdate{}); err == nil && updated != nil {
		t.Fatal("organization B must never be able to update organization A's zone")
	}
}

func TestZoneIgnoreAndAuthorizedEntries(t *testing.T) {
	repo, fx, seedPlayer := newZoneTestWorld(t)
	ctx := context.Background()
	zone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Zone", ZoneTypeUAV, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	playerID := seedPlayer("Alice")

	if ignored, err := repo.IsIgnored(ctx, zone.ID, playerID, nil); err != nil || ignored {
		t.Fatalf("expected not ignored before any entry, got %v err=%v", ignored, err)
	}
	if _, err := repo.AddIgnoreEntry(ctx, zone.ID, ZoneEntryPlayer, fmt.Sprint(playerID), fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	if ignored, err := repo.IsIgnored(ctx, zone.ID, playerID, nil); err != nil || !ignored {
		t.Fatalf("expected ignored after adding a PLAYER entry, got %v err=%v", ignored, err)
	}

	if authorized, err := repo.IsAuthorized(ctx, zone.ID, playerID, nil); err != nil || authorized {
		t.Fatalf("expected not authorized before any entry, got %v err=%v", authorized, err)
	}
	entry, err := repo.AddAuthorizedEntry(ctx, zone.ID, ZoneEntryPlayer, fmt.Sprint(playerID), fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	if authorized, err := repo.IsAuthorized(ctx, zone.ID, playerID, nil); err != nil || !authorized {
		t.Fatalf("expected authorized after adding a PLAYER entry, got %v err=%v", authorized, err)
	}

	deleted, err := repo.DeleteAuthorizedEntry(ctx, fx.InstallationID, entry.ID)
	if err != nil || !deleted {
		t.Fatalf("expected authorized entry deletion to succeed, err=%v", err)
	}
	if authorized, _ := repo.IsAuthorized(ctx, zone.ID, playerID, nil); authorized {
		t.Fatal("expected not authorized after deletion")
	}

	roles, err := repo.DiscordRoleIgnoreEntries(ctx, zone.ID)
	if err != nil || len(roles) != 0 {
		t.Fatalf("expected no DISCORD_ROLE ignore entries yet, got %v", roles)
	}
	if _, err := repo.AddIgnoreEntry(ctx, zone.ID, ZoneEntryDiscordRole, "role-123", fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	roles, err = repo.DiscordRoleIgnoreEntries(ctx, zone.ID)
	if err != nil || len(roles) != 1 || roles[0] != "role-123" {
		t.Fatalf("expected [role-123], got %v (err=%v)", roles, err)
	}
}

func TestZoneBanLifecycle(t *testing.T) {
	repo, fx, seedPlayer := newZoneTestWorld(t)
	ctx := context.Background()
	zone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Zone", ZoneTypeRestricted, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	playerID := seedPlayer("Bad Actor")

	if ban, err := repo.ActiveZoneBan(ctx, zone.ID, playerID); err != nil || ban != nil {
		t.Fatalf("expected no active ban yet, got %+v err=%v", ban, err)
	}
	ban, err := repo.AddZoneBan(ctx, zone.ID, playerID, "raiding", fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !ban.Active {
		t.Fatalf("expected new ban to be active: %+v", ban)
	}
	if active, err := repo.ActiveZoneBan(ctx, zone.ID, playerID); err != nil || active == nil {
		t.Fatalf("expected an active ban, got %+v err=%v", active, err)
	}

	lifted, err := repo.LiftZoneBan(ctx, fx.InstallationID, ban.ID, fx.OwnerUserID)
	if err != nil || !lifted {
		t.Fatalf("expected lift to succeed, err=%v", err)
	}
	if active, err := repo.ActiveZoneBan(ctx, zone.ID, playerID); err != nil || active != nil {
		t.Fatalf("expected no active ban after lifting, got %+v err=%v", active, err)
	}

	// A ban can be re-added after a lift - history is preserved (never overwritten/deleted).
	if _, err := repo.AddZoneBan(ctx, zone.ID, playerID, "raiding again", fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListZoneBans(ctx, zone.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("expected 2 ban rows (history preserved), got %d (err=%v)", len(rows), err)
	}
}

func TestZonePresenceAndIntrusionLifecycle(t *testing.T) {
	repo, fx, seedPlayer := newZoneTestWorld(t)
	ctx := context.Background()
	zone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Zone", ZoneTypeRestricted, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	playerID := seedPlayer("Intruder")
	now := time.Now().UTC().Truncate(time.Millisecond)

	if p, err := repo.GetPresence(ctx, zone.ID, playerID); err != nil || p != nil {
		t.Fatalf("expected no presence row yet, got %+v err=%v", p, err)
	}
	if err := repo.UpsertPresence(ctx, zone.ID, playerID, PresenceInside, &now, now, 0, &now); err != nil {
		t.Fatal(err)
	}
	p, err := repo.GetPresence(ctx, zone.ID, playerID)
	if err != nil || p == nil || p.Status != PresenceInside {
		t.Fatalf("expected persisted INSIDE presence, got %+v err=%v", p, err)
	}
	if p.LastAlertAt == nil || !p.LastAlertAt.Equal(now) {
		t.Fatalf("expected last_alert_at to be set, got %+v", p.LastAlertAt)
	}

	// A subsequent write with lastAlertAt=nil must NOT clear the cooldown anchor.
	later := now.Add(10 * time.Second)
	if err := repo.UpsertPresence(ctx, zone.ID, playerID, PresenceInside, &now, later, 0, nil); err != nil {
		t.Fatal(err)
	}
	p2, err := repo.GetPresence(ctx, zone.ID, playerID)
	if err != nil || p2.LastAlertAt == nil || !p2.LastAlertAt.Equal(now) {
		t.Fatalf("expected last_alert_at to survive a nil update, got %+v", p2)
	}

	if open, err := repo.GetOpenIntrusion(ctx, zone.ID, playerID); err != nil || open != nil {
		t.Fatalf("expected no open intrusion yet, got %+v err=%v", open, err)
	}
	it, err := repo.CreateIntrusion(ctx, zone.ID, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, playerID, "Intruder", false, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != IntrusionActive || it.AlertCount != 1 {
		t.Fatalf("unexpected created intrusion: %+v", it)
	}

	// The partial unique index (zone_id,player_id) WHERE status<>'EXITED' must reject a second
	// simultaneously-open intrusion for the same pair.
	if _, err := repo.CreateIntrusion(ctx, zone.ID, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, playerID, "Intruder", false, now, true); err == nil {
		t.Fatal("expected a unique-violation creating a second open intrusion for the same zone+player")
	}

	if open, err := repo.GetOpenIntrusion(ctx, zone.ID, playerID); err != nil || open == nil || open.ID != it.ID {
		t.Fatalf("expected to find the open intrusion, got %+v err=%v", open, err)
	}

	if err := repo.MarkExited(ctx, it.ID, later); err != nil {
		t.Fatal(err)
	}
	if open, err := repo.GetOpenIntrusion(ctx, zone.ID, playerID); err != nil || open != nil {
		t.Fatalf("expected no open intrusion after exit, got %+v err=%v", open, err)
	}
	closed, err := repo.GetIntrusion(ctx, fx.InstallationID, it.ID)
	if err != nil || closed.Status != IntrusionExited || closed.ExitedAt == nil {
		t.Fatalf("expected the intrusion to be EXITED with exited_at set, got %+v err=%v", closed, err)
	}
}

func TestActiveZonesForServerOnlyReturnsEnabled(t *testing.T) {
	repo, fx, _ := newZoneTestWorld(t)
	ctx := context.Background()
	enabledZone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Enabled", ZoneTypeSafezone, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	disabledZone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Disabled", ZoneTypeSafezone, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	if _, err := repo.UpdateZone(ctx, fx.InstallationID, disabledZone.ID, ZoneUpdate{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}

	zones, err := repo.ActiveZonesForServer(ctx, fx.ServerRowID)
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones[0].ID != enabledZone.ID {
		t.Fatalf("expected only the enabled zone, got %+v", zones)
	}
}

func TestListActiveForInstallationAndAcknowledgeAndHistory(t *testing.T) {
	repo, fx, seedPlayer := newZoneTestWorld(t)
	ctx := context.Background()
	zone, err := repo.CreateZone(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, "Zone", ZoneTypeRestricted, 0, 0, 10, nil, 300, fx.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	playerID := seedPlayer("Watched")
	now := time.Now().UTC().Truncate(time.Millisecond)

	it, err := repo.CreateIntrusion(ctx, zone.ID, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, playerID, "Watched", false, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertPresence(ctx, zone.ID, playerID, PresenceInside, &now, now, 0, &now); err != nil {
		t.Fatal(err)
	}

	active, err := repo.ListActiveForInstallation(ctx, fx.InstallationID, ActiveIntrusionFilter{})
	if err != nil || len(active) != 1 || active[0].ID != it.ID {
		t.Fatalf("expected 1 active intrusion, got %+v (err=%v)", active, err)
	}
	if active[0].ZoneName != "Zone" {
		t.Fatalf("expected zone name to be joined in, got %+v", active[0])
	}

	acked, err := repo.AcknowledgeIntrusion(ctx, fx.InstallationID, it.ID, fx.OwnerUserID)
	if err != nil || acked == nil || acked.Status != IntrusionAcknowledged {
		t.Fatalf("expected acknowledge to succeed, got %+v err=%v", acked, err)
	}
	// Acknowledging twice must not find a still-ACTIVE row (already ACKNOWLEDGED).
	if again, err := repo.AcknowledgeIntrusion(ctx, fx.InstallationID, it.ID, fx.OwnerUserID); err != nil || again != nil {
		t.Fatalf("expected a second acknowledge to find nothing, got %+v err=%v", again, err)
	}

	// An ACKNOWLEDGED intrusion is still "open" (not EXITED) - it must still appear in the active listing.
	stillActive, err := repo.ListActiveForInstallation(ctx, fx.InstallationID, ActiveIntrusionFilter{Acknowledged: boolPtr(true)})
	if err != nil || len(stillActive) != 1 {
		t.Fatalf("expected the acknowledged intrusion to remain in the active listing, got %+v (err=%v)", stillActive, err)
	}

	if err := repo.MarkExited(ctx, it.ID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	afterExit, err := repo.ListActiveForInstallation(ctx, fx.InstallationID, ActiveIntrusionFilter{})
	if err != nil || len(afterExit) != 0 {
		t.Fatalf("expected the exited intrusion to leave the active listing, got %+v", afterExit)
	}

	history, err := repo.ListHistory(ctx, fx.InstallationID, IntrusionHistoryFilter{})
	if err != nil || len(history) != 1 || history[0].Status != IntrusionExited {
		t.Fatalf("expected 1 EXITED row in history, got %+v (err=%v)", history, err)
	}
}

func boolPtr(b bool) *bool { return &b }
