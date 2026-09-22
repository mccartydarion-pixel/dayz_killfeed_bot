//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// End-to-end Champion Player API tests (Champion Access Model Phase 2, Part A/B) over the real
// routes and a real PostgreSQL: server association, multi-server players, stats isolation,
// unverified/cross-server denial. w.players are synced Champion users with NO organization
// membership - exactly a "Player" identity.

// insertDeath mirrors insertKill (saas_api_faction_stats_integration_test.go) for the death side.
func (w *factionWorld) insertDeath(f installationFixture, victim int64, at time.Time) {
	w.t.Helper()
	guild, server := w.gameContext(f)
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO deaths(guild_id, server_id, session_id, event_fingerprint, player_id, death_type, event_time)
VALUES($1,$2,'s',$3,$4,'ENVIRONMENT',$5)`, guild, server, fmt.Sprintf("api-death-%d-%d", time.Now().UnixNano(), statsTestSeq.Add(1)), victim, at); err != nil {
		w.t.Fatal(err)
	}
}

// claimBounty inserts an already-CLAIMED bounty row, bypassing the real claim workflow (this test
// only needs the aggregate data present, matching insertKill/insertDeath's own shortcut style).
// serverID = 0 means a guild-wide bounty (bounties.server_id NULL), matching ClaimForKill's own
// nullable scoping.
func (w *factionWorld) claimBounty(f installationFixture, claimedBy int64, rewardPoints int, serverID int64) {
	w.t.Helper()
	guild, _ := w.gameContext(f)
	var server any
	if serverID > 0 {
		server = serverID
	}
	now := time.Now()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO bounties(guild_id, server_id, target_player_id, created_by_type, status, reward_points, starts_at, claimed_by_player_id, claimed_at)
VALUES($1,$2,$3,'SYSTEM','CLAIMED',$4,$5,$3,$5)`, guild, server, claimedBy, rewardPoints, now); err != nil {
		w.t.Fatal(err)
	}
}

func (w *factionWorld) playerServersPath() string { return "/api/saas/player/servers" }
func (w *factionWorld) playerStatsPath(installationID int64) string {
	return fmt.Sprintf("/api/saas/player/servers/%d/stats", installationID)
}

func TestPlayerServersVerifiedPlayerOnOneServer(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[0]
	player := w.linkPlayer(w.a1, actor, "Survivor1")
	w.insertKill(w.a1, player, time.Now(), false)

	resp := w.getJSON(w.playerServersPath(), actor)
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("expected exactly 1 server, got %v", resp)
	}
	item := items[0].(map[string]any)
	if int64(item["installationId"].(float64)) != w.a1.InstallationID {
		t.Fatalf("%v", item)
	}
	if int64(item["linkedPlayerId"].(float64)) != player {
		t.Fatalf("%v", item)
	}
	if item["organizationId"] != nil {
		t.Fatalf("organizationId must not be exposed to a Player: %v", item)
	}
	if resp["defaultInstallationId"] == nil || int64(resp["defaultInstallationId"].(float64)) != w.a1.InstallationID {
		t.Fatalf("expected the default installation set, got %v", resp)
	}
}

func TestPlayerServersMultiServerPlayerOrderedByRecentActivity(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[1]
	second := w.secondInstallation(w.a1)
	player := w.linkPlayer(w.a1, actor, "Survivor2") // one player_links row for the whole guild

	// Active on the second server MORE recently than the first.
	w.insertKill(w.a1, player, time.Now().Add(-time.Hour), false)
	w.insertKill(second, player, time.Now(), false)
	guild, server1 := w.gameContext(w.a1)
	_, server2 := w.gameContext(second)
	if err := w.a.ActivityRepository.Connect(context.Background(), guild, server1, player, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.a.ActivityRepository.Connect(context.Background(), guild, server2, player, time.Now()); err != nil {
		t.Fatal(err)
	}

	resp := w.getJSON(w.playerServersPath(), actor)
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected both servers of the guild, got %v", resp)
	}
	first := items[0].(map[string]any)
	if int64(first["installationId"].(float64)) != second.InstallationID {
		t.Fatalf("expected the more recently active server first, got %v", resp)
	}
	if int64(resp["defaultInstallationId"].(float64)) != second.InstallationID {
		t.Fatalf("expected the default to be the most recently active installation, got %v", resp)
	}
}

func TestPlayerServersUnverifiedPlayerGetsEmptyList(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[2] // synced, never linked to any player

	resp := w.getJSON(w.playerServersPath(), actor)
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("expected an empty list for an unverified user, got %v", resp)
	}
	if resp["defaultInstallationId"] != nil {
		t.Fatalf("expected no default installation, got %v", resp)
	}
}

// A player verified+active on org A's guild must never see org B's installation, even though both
// exist in the same database (tenant isolation for the player-facing surface).
func TestPlayerServersCrossTenantIsolation(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[3]
	player := w.linkPlayer(w.a1, actor, "OnlyOnA")
	w.insertKill(w.a1, player, time.Now(), false)

	resp := w.getJSON(w.playerServersPath(), actor)
	for _, raw := range resp["items"].([]any) {
		item := raw.(map[string]any)
		if int64(item["installationId"].(float64)) == w.b1.InstallationID {
			t.Fatalf("org B's installation must never appear for a player only ever linked/active on org A: %v", resp)
		}
	}
}

func TestPlayerStatsFieldsComputeCorrectly(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[4]
	player := w.linkPlayer(w.a1, actor, "StatsPlayer")

	w.insertKill(w.a1, player, time.Now(), true)  // headshot
	w.insertKill(w.a1, player, time.Now(), false) // not a headshot
	w.insertDeath(w.a1, player, time.Now())
	w.claimBounty(w.a1, player, 250, 0) // guild-wide claim

	resp := w.getJSON(w.playerStatsPath(w.a1.InstallationID), actor)
	if int(resp["kills"].(float64)) != 2 {
		t.Fatalf("kills: %v", resp)
	}
	if int(resp["deaths"].(float64)) != 1 {
		t.Fatalf("deaths: %v", resp)
	}
	if resp["kd"].(float64) != 2.0 {
		t.Fatalf("kd: %v", resp)
	}
	if int(resp["headshots"].(float64)) != 1 {
		t.Fatalf("headshots: %v", resp)
	}
	if int(resp["bountiesClaimed"].(float64)) != 1 || int64(resp["bountyValue"].(float64)) != 250 {
		t.Fatalf("bounties: %v", resp)
	}
	if resp["installationId"] == nil || int64(resp["installationId"].(float64)) != w.a1.InstallationID {
		t.Fatalf("%v", resp)
	}
}

// The core installation-isolation guarantee (task section 8): a player active on BOTH servers of
// one guild must see each server's stats independently - server A's kills must never bleed into
// server B's response, and vice versa.
func TestPlayerStatsInstallationIsolation(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[5]
	second := w.secondInstallation(w.a1)
	player := w.linkPlayer(w.a1, actor, "TwoServerPlayer")

	w.insertKill(w.a1, player, time.Now(), false)
	w.insertKill(w.a1, player, time.Now(), false)
	w.insertKill(second, player, time.Now(), false) // only one kill on the second server

	statsA := w.getJSON(w.playerStatsPath(w.a1.InstallationID), actor)
	statsB := w.getJSON(w.playerStatsPath(second.InstallationID), actor)
	if int(statsA["kills"].(float64)) != 2 {
		t.Fatalf("server A must show only its own 2 kills: %v", statsA)
	}
	if int(statsB["kills"].(float64)) != 1 {
		t.Fatalf("server B must show only its own 1 kill, never A's, got %v", statsB)
	}
}

func TestPlayerStatsUnlinkedPlayerReturnsIdentityRequired(t *testing.T) {
	w := newFactionWorld(t)
	actor := syncUser(t, w.a, fmt.Sprintf("unlinked-%d", time.Now().UnixNano()), "Unlinked").DiscordUserID

	r := w.do(http.MethodGet, w.playerStatsPath(w.a1.InstallationID), actor, nil)
	if r.Status != http.StatusConflict || r.errCode(t) != "PLAYER_IDENTITY_REQUIRED" {
		t.Fatalf("expected 409 PLAYER_IDENTITY_REQUIRED, got %d %s", r.Status, r.Body)
	}
}

func TestPlayerStatsUnknownInstallationReturns404(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[0]
	r := w.do(http.MethodGet, w.playerStatsPath(999999999), actor, nil)
	if r.Status != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown installation, got %d %s", r.Status, r.Body)
	}
}

// A player verified in the guild (via a link) but who has NEVER played on this SPECIFIC server of
// a multi-server guild must be denied, non-enumerating (404, not a distinct "not associated" code) -
// task section 10. A verified link to the guild alone is not proof for this one server.
func TestPlayerStatsNoObservedActivityOnThisServerReturns404(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[1]
	second := w.secondInstallation(w.a1)
	w.linkPlayer(w.a1, actor, "OnlyOnFirstServer")
	// No kill/death/activity ever recorded on `second`.

	r := w.do(http.MethodGet, w.playerStatsPath(second.InstallationID), actor, nil)
	if r.Status != http.StatusNotFound {
		t.Fatalf("expected 404 (non-enumerating) for a server the player never played on, got %d %s", r.Status, r.Body)
	}
}

// Cross-tenant: org B's owner (a real, verified organization role) has no player association with
// org A's installation and must be denied exactly like any other unassociated caller.
func TestPlayerStatsCrossTenantOwnerIsNotImplicitlyAssociated(t *testing.T) {
	w := newFactionWorld(t)
	r := w.do(http.MethodGet, w.playerStatsPath(w.a1.InstallationID), w.b1.OwnerDiscordID, nil)
	if r.Status != http.StatusConflict && r.Status != http.StatusNotFound {
		t.Fatalf("org membership must never substitute for player identity, got %d %s", r.Status, r.Body)
	}
}

func TestPlayerServersUnauthenticated(t *testing.T) {
	w := newFactionWorld(t)
	w.expect(w.do(http.MethodGet, w.playerServersPath(), "", nil), http.StatusUnauthorized, "no acting user")
	w.expect(w.do(http.MethodGet, w.playerStatsPath(w.a1.InstallationID), "", nil), http.StatusUnauthorized, "no acting user")
}
