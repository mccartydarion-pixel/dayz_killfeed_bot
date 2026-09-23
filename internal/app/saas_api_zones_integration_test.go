//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Phase 4 (docs/ZONES_UAV_RADAR.md) API integration tests: capability gating (ZONE_VIEW/
// ZONE_MANAGE/ZONE_IGNORE_MANAGE/UAV_MANAGE/INTRUSION_ACK), the UAV/Base Radar extra-Owner-gate,
// and acknowledge+audit. Reuses clientAdminWorld exactly like saas_api_player_intelligence_
// integration_test.go - no second harness.

func newZoneWorld(t *testing.T) *clientAdminWorld {
	t.Helper()
	w := newClientAdminWorld(t)
	w.a.Zones = repository.NewZoneRepository(w.a.DB.Pool)
	return w
}

// zoneActor syncs a fresh, uniquely-named app_user (POST /api/saas/users/sync, matching how a real
// website session bootstraps before any admin call - resolveActingUser 401s otherwise) and returns
// its Discord user id, ready to be passed to w.mapRole or used as a w.call actor directly.
func zoneActor(t *testing.T, w *clientAdminWorld, name string) string {
	t.Helper()
	user := syncUser(t, w.a, fmt.Sprintf("zone-%s-%d", name, time.Now().UnixNano()), name)
	return user.DiscordUserID
}

func TestZoneListRequiresCapability(t *testing.T) {
	w := newZoneWorld(t)
	// An actor who has synced but has no mapped Discord role at all.
	unmapped := zoneActor(t, w, "unmapped")
	rr := w.call(w.a.handleListZones, http.MethodGet, w.path("/zones"), unmapped, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor with no mapped level, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = w.call(w.a.handleListZones, http.MethodGet, w.path("/zones"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the organization owner, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestZoneCreateRequiresZoneManageNotJustView(t *testing.T) {
	w := newZoneWorld(t)
	mod := zoneActor(t, w, "moderator")
	w.mapRole(mod, "role-mod", "MODERATOR")

	body := createZoneRequest{Name: "Safe Zone", ZoneType: repository.ZoneTypeSafezone, CenterX: 0, CenterZ: 0, Radius: 50}
	rr := w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), mod, body, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a Moderator (ZONE_VIEW only) must not be able to create a zone, got %d: %s", rr.Code, rr.Body.String())
	}

	admin := zoneActor(t, w, "admin")
	w.mapRole(admin, "role-admin", "ADMINISTRATOR")
	rr = w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), admin, body, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("an Administrator must be able to create an ordinary zone, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestUAVZoneCreationRequiresOwnerEvenWithZoneManage(t *testing.T) {
	w := newZoneWorld(t)
	admin := zoneActor(t, w, "admin")
	w.mapRole(admin, "role-admin", "ADMINISTRATOR")

	body := createZoneRequest{Name: "UAV Watch", ZoneType: repository.ZoneTypeUAV, CenterX: 0, CenterZ: 0, Radius: 500}
	rr := w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), admin, body, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("an Administrator without UAV_MANAGE must not be able to create a UAV zone, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), w.f.OwnerDiscordID, body, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("the organization owner must be able to create a UAV zone, got %d: %s", rr.Code, rr.Body.String())
	}
	zone := decodeBody[zoneDTO](t, rr)
	if zone.ZoneType != repository.ZoneTypeUAV {
		t.Fatalf("unexpected zone type %q", zone.ZoneType)
	}

	// The same UAV zone must also require UAV_MANAGE to DELETE, not just ZONE_MANAGE.
	del := w.call(w.a.handleDeleteZone, http.MethodDelete, w.path("/zones/x"), admin, nil, map[string]string{"zoneID": strconv.FormatInt(zone.ID, 10)})
	if del.Code != http.StatusForbidden {
		t.Fatalf("an Administrator without UAV_MANAGE must not be able to delete a UAV zone, got %d: %s", del.Code, del.Body.String())
	}
}

func TestZoneInvalidRadiusAndTypeRejected(t *testing.T) {
	w := newZoneWorld(t)

	rr := w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), w.f.OwnerDiscordID,
		createZoneRequest{Name: "Bad", ZoneType: repository.ZoneTypeSafezone, Radius: 0}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for radius<=0, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), w.f.OwnerDiscordID,
		createZoneRequest{Name: "Bad", ZoneType: "NOT_A_TYPE", Radius: 10}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid zoneType, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestZoneIgnoreManageRequiresItsOwnCapability(t *testing.T) {
	w := newZoneWorld(t)
	mod := zoneActor(t, w, "mod")
	w.mapRole(mod, "role-mod", "MODERATOR")
	rr := w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), w.f.OwnerDiscordID,
		createZoneRequest{Name: "Zone", ZoneType: repository.ZoneTypeRestricted, Radius: 10}, nil)
	zone := decodeBody[zoneDTO](t, rr)

	ignore := w.call(w.a.handleAddIgnoreEntry, http.MethodPost, w.path("/zones/x/ignore"), mod,
		addEntryRequest{EntryType: repository.ZoneEntryPlayer, EntryValue: "1"}, map[string]string{"zoneID": strconv.FormatInt(zone.ID, 10)})
	if ignore.Code != http.StatusForbidden {
		t.Fatalf("a Moderator must not have ZONE_IGNORE_MANAGE, got %d: %s", ignore.Code, ignore.Body.String())
	}

	ignore = w.call(w.a.handleAddIgnoreEntry, http.MethodPost, w.path("/zones/x/ignore"), w.f.OwnerDiscordID,
		addEntryRequest{EntryType: repository.ZoneEntryPlayer, EntryValue: "1"}, map[string]string{"zoneID": strconv.FormatInt(zone.ID, 10)})
	if ignore.Code != http.StatusCreated {
		t.Fatalf("the owner must be able to add an ignore entry, got %d: %s", ignore.Code, ignore.Body.String())
	}
}

func TestAcknowledgeIntrusionRequiresCapabilityAndAudits(t *testing.T) {
	w := newZoneWorld(t)
	rr := w.call(w.a.handleCreateZone, http.MethodPost, w.path("/zones"), w.f.OwnerDiscordID,
		createZoneRequest{Name: "Zone", ZoneType: repository.ZoneTypeRestricted, Radius: 10}, nil)
	zone := decodeBody[zoneDTO](t, rr)

	playerID := w.seedPlayer("Intruder")
	ctx := context.Background()
	it, err := w.a.Zones.CreateIntrusion(ctx, zone.ID, w.f.InstallationID, w.guildID, w.serverID, playerID, "Intruder", false, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}

	// Gatekeeper (below Moderator) must not be able to acknowledge.
	gatekeeper := zoneActor(t, w, "gatekeeper")
	w.mapRole(gatekeeper, "role-gk", "GATEKEEPER")
	ack := w.call(w.a.handleAcknowledgeIntrusion, http.MethodPost, w.path("/intrusions/x/acknowledge"), gatekeeper, nil, map[string]string{"intrusionID": strconv.FormatInt(it.ID, 10)})
	if ack.Code != http.StatusForbidden {
		t.Fatalf("a Gatekeeper must not have INTRUSION_ACK, got %d: %s", ack.Code, ack.Body.String())
	}

	mod := zoneActor(t, w, "mod")
	w.mapRole(mod, "role-mod", "MODERATOR")
	ack = w.call(w.a.handleAcknowledgeIntrusion, http.MethodPost, w.path("/intrusions/x/acknowledge"), mod, nil, map[string]string{"intrusionID": strconv.FormatInt(it.ID, 10)})
	if ack.Code != http.StatusOK {
		t.Fatalf("a Moderator must be able to acknowledge, got %d: %s", ack.Code, ack.Body.String())
	}
	dto := decodeBody[zoneIntrusionDTO](t, ack)
	if dto.Status != repository.IntrusionAcknowledged {
		t.Fatalf("expected ACKNOWLEDGED status, got %q", dto.Status)
	}

	entries, err := w.a.AdminAudit.List(ctx, w.f.OrgID, &w.f.InstallationID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "INTRUSION_ACKNOWLEDGE" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected an INTRUSION_ACKNOWLEDGE audit entry")
	}
}
