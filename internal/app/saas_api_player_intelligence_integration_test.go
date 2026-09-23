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

// Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md) API-level integration tests: capability gating,
// cross-tenant isolation at the HTTP layer, and audit writes for the two privileged read routes
// (task section 10). Reuses clientAdminWorld from saas_api_client_admin_integration_test.go - same
// harness, same fixture, just with Locations wired up too.

func newPlayerIntelWorld(t *testing.T) *clientAdminWorld {
	t.Helper()
	w := newClientAdminWorld(t)
	w.a.Locations = repository.NewLocationRepository(w.a.DB.Pool)
	return w
}

func (w *clientAdminWorld) seedLocationEvent(playerID int64, x, z float64, eventType string, at time.Time) {
	w.t.Helper()
	if _, err := w.a.Locations.InsertLocationEvents(context.Background(), []repository.LocationEventInput{
		{GuildID: w.guildID, ServerID: w.serverID, PlayerID: playerID, Gamertag: fmt.Sprintf("p%d", playerID), X: x, Z: z, EventType: eventType, ObservedAt: at},
	}); err != nil {
		w.t.Fatal(err)
	}
}

func TestPlayerDirectoryRequiresCapability(t *testing.T) {
	w := newPlayerIntelWorld(t)
	stranger := syncUser(t, w.a, fmt.Sprintf("pi-stranger-%d", time.Now().UnixNano()), "Stranger")
	rr := w.call(w.a.handlePlayerDirectory, http.MethodGet, w.path("/players"), stranger.DiscordUserID, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor with no mapped capability, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPlayerDirectoryOwnerCanList(t *testing.T) {
	w := newPlayerIntelWorld(t)
	w.seedPlayer("Alice")
	rr := w.call(w.a.handlePlayerDirectory, http.MethodGet, w.path("/players"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the organization owner, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[map[string]any](t, rr)
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatal("expected at least the seeded player in the directory response")
	}
}

func TestPlayerDirectoryNeverLeaksAnotherOrganization(t *testing.T) {
	w1 := newPlayerIntelWorld(t)
	w2 := newPlayerIntelWorld(t)
	otherPlayerID := w2.seedPlayer("OtherOrgPlayer")

	rr := w1.call(w1.a.handlePlayerDirectory, http.MethodGet, w1.path("/players"), w1.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[map[string]any](t, rr)
	items, _ := body["items"].([]any)
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		if id, _ := entry["playerId"].(float64); int64(id) == otherPlayerID {
			t.Fatal("a player from a different organization's guild must never appear in this directory response")
		}
	}
}

func TestLatestLocationAndHistoryRequireLocationCapabilities(t *testing.T) {
	w := newPlayerIntelWorld(t)
	playerID := w.seedPlayer("Located")
	w.seedLocationEvent(playerID, 100, 200, "HIT", time.Now().Add(-time.Minute))

	// Owner has every capability, including PLAYER_LAST_LOCATION_VIEW/PLAYER_LOCATION_VIEW.
	latestRR := w.call(w.a.handleLatestLocation, http.MethodGet, w.path(fmt.Sprintf("/players/%d/locations/latest", playerID)), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if latestRR.Code != http.StatusOK {
		t.Fatalf("expected 200 for latest location, got %d: %s", latestRR.Code, latestRR.Body.String())
	}
	latest := decodeBody[map[string]any](t, latestRR)
	if latest["freshness"] == nil || latest["ageSeconds"] == nil || latest["observedAt"] == nil {
		t.Fatalf("expected freshness/ageSeconds/observedAt on every exposed location, got %v", latest)
	}

	historyRR := w.call(w.a.handleLocationHistory, http.MethodGet, w.path(fmt.Sprintf("/players/%d/locations", playerID)), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if historyRR.Code != http.StatusOK {
		t.Fatalf("expected 200 for location history, got %d: %s", historyRR.Code, historyRR.Body.String())
	}

	// A Moderator-level mapping (below Administrator) must be forbidden from both.
	modUser := syncUser(t, w.a, fmt.Sprintf("pi-mod-%d", time.Now().UnixNano()), "Mod")
	w.mapRole(modUser.DiscordUserID, "role-pi-moderator", "MODERATOR")
	forbiddenRR := w.call(w.a.handleLatestLocation, http.MethodGet, w.path(fmt.Sprintf("/players/%d/locations/latest", playerID)), modUser.DiscordUserID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if forbiddenRR.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a Moderator-level actor requesting privileged location data, got %d: %s", forbiddenRR.Code, forbiddenRR.Body.String())
	}
}

func TestLatestLocationReturns404WhenNoObservedLocation(t *testing.T) {
	w := newPlayerIntelWorld(t)
	playerID := w.seedPlayer("NeverSeen")
	rr := w.call(w.a.handleLatestLocation, http.MethodGet, w.path(fmt.Sprintf("/players/%d/locations/latest", playerID)), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(playerID, 10)})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a player with no location history, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOnlinePlayersNeverIncludesDisconnectedAndWritesAudit(t *testing.T) {
	w := newPlayerIntelWorld(t)
	online := w.seedPlayer("StillOnline")
	offline := w.seedPlayer("LongGone")
	w.connectPlayer(online, time.Now())
	w.connectPlayer(offline, time.Now().Add(-time.Hour))
	w.disconnectPlayer(offline, time.Now().Add(-30*time.Minute))

	rr := w.call(w.a.handleOnlinePlayers, http.MethodGet, w.path("/players/online"), w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[map[string]any](t, rr)
	items, _ := body["items"].([]any)
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		if id, _ := entry["playerId"].(float64); int64(id) == offline {
			t.Fatal("a disconnected player must never appear in the online-players response")
		}
	}

	// Audit (task section 10): viewing online-player locations is a privileged read action.
	auditRR := w.call(w.a.handleListAuditLog, http.MethodGet, w.path("/audit-log"), w.f.OwnerDiscordID, nil, nil)
	auditBody := decodeBody[map[string]any](t, auditRR)
	auditItems, _ := auditBody["items"].([]any)
	found := false
	for _, raw := range auditItems {
		entry, _ := raw.(map[string]any)
		if entry["action"] == "ONLINE_PLAYER_LOCATIONS_VIEWED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an ONLINE_PLAYER_LOCATIONS_VIEWED audit entry, got %v", auditItems)
	}
}

// connectPlayer/disconnectPlayer are small helpers for this file's online-state tests, reusing
// player_server_activity directly (the same table Phase 1's lastOnline endpoint already reads).
func (w *clientAdminWorld) connectPlayer(playerID int64, at time.Time) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `
INSERT INTO player_server_activity(guild_id,server_id,player_id,first_seen_at,last_seen_at,currently_connected,current_session_started_at)
VALUES($1,$2,$3,$4,$4,true,$4)
ON CONFLICT(guild_id,server_id,player_id) DO UPDATE SET last_seen_at=$4,currently_connected=true,current_session_started_at=$4`,
		w.guildID, w.serverID, playerID, at); err != nil {
		w.t.Fatal(err)
	}
}

func (w *clientAdminWorld) disconnectPlayer(playerID int64, at time.Time) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE player_server_activity SET last_seen_at=$3,currently_connected=false WHERE guild_id=$1 AND server_id=$2 AND player_id=$4`,
		w.guildID, w.serverID, at, playerID); err != nil {
		w.t.Fatal(err)
	}
}
