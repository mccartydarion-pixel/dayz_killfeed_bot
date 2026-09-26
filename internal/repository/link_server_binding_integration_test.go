//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

// linkBindingFixture builds the full Guild -> Organization -> Installation ->
// game_servers chain the way the SaaS dashboard connect flow does
// (UpsertForInstallation with Nitrado power state as status), then records
// observed activity for one player on that server.
type linkBindingFixture struct {
	GuildRowID, ServerID, PlayerID int64
	PlayerName                     string
}

func newLinkBindingFixture(t *testing.T, repoStatus string, observed time.Duration) linkBindingFixture {
	t.Helper()
	db := saasIntegrationDB(t)
	ctx := context.Background()
	base := newSaaSFixture(t, db)
	suffix := time.Now().UnixNano()

	// The legacy fixture server is CONNECTED; retire it so the guild has
	// exactly one bound server - the dashboard-connected one.
	servers := NewServerRepository(db.Pool)
	if err := servers.Deactivate(ctx, base.ServerRowID); err != nil {
		t.Fatal(err)
	}
	dash, err := NewSaaSServerRepository(db.Pool).UpsertForInstallation(ctx, base.OrgID, base.GuildRowID, GameServer{
		Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("dash-svc-%d", suffix), Game: "DayZ",
		Platform: "PLAYSTATION", DisplayName: "Dashboard Server", Status: repoStatus, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("PsnPlayer%d", suffix%100000)
	playerID, err := NewPlayerRepository(db.Pool).UpsertPlayer(ctx, base.GuildRowID, fmt.Sprintf("dayz-%d", suffix), name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	activity := NewActivityRepository(db.Pool)
	start := time.Now().Add(-observed)
	if err := activity.Connect(ctx, base.GuildRowID, dash.ID, playerID, start); err != nil {
		t.Fatal(err)
	}
	// Checkpoint every 60s like the live worker so observed time accrues.
	for at := start.Add(time.Minute); at.Before(time.Now()); at = at.Add(time.Minute) {
		if err := activity.CheckpointConnected(ctx, base.GuildRowID, dash.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	return linkBindingFixture{GuildRowID: base.GuildRowID, ServerID: dash.ID, PlayerID: playerID, PlayerName: name}
}

// TestConnectedServerIDResolvesDashboardConnectedServer reproduces the
// production NO_CONNECTED_SERVER failure: the SaaS dashboard writes Nitrado
// power state (ONLINE/OFFLINE) into game_servers.status, which the old
// status allow-list ('connected','ready','active') never matched, while the
// activity worker (which keys on active) kept recording events for it.
func TestConnectedServerIDResolvesDashboardConnectedServer(t *testing.T) {
	for _, status := range []string{"ONLINE", "OFFLINE", "CONNECTED"} {
		t.Run(status, func(t *testing.T) {
			f := newLinkBindingFixture(t, status, 6*time.Minute)
			db := saasIntegrationDB(t)
			got, err := NewServerRepository(db.Pool).ConnectedServerID(context.Background(), f.GuildRowID)
			if err != nil {
				t.Fatalf("expected the active dashboard server to resolve, got %v", err)
			}
			if got != f.ServerID {
				t.Fatalf("resolved server %d, want %d", got, f.ServerID)
			}
		})
	}
}

// TestConnectedServerIDExcludesDisconnectedAndOtherGuilds keeps teardown and
// tenant isolation intact: a deactivated server never resolves, and one
// guild's server is never visible to another guild.
func TestConnectedServerIDExcludesDisconnectedAndOtherGuilds(t *testing.T) {
	f := newLinkBindingFixture(t, "ONLINE", time.Minute)
	other := newLinkBindingFixture(t, "ONLINE", time.Minute)
	db := saasIntegrationDB(t)
	ctx := context.Background()
	servers := NewServerRepository(db.Pool)

	if got, err := servers.ConnectedServerID(ctx, other.GuildRowID); err != nil || got != other.ServerID {
		t.Fatalf("other guild resolved (%d, %v), want its own server %d", got, err, other.ServerID)
	}
	if err := servers.Deactivate(ctx, f.ServerID); err != nil {
		t.Fatal(err)
	}
	_, err := servers.ConnectedServerID(ctx, f.GuildRowID)
	if err == nil || !strings.Contains(err.Error(), "no connected server") {
		t.Fatalf("expected no connected server after teardown, got %v", err)
	}
	// Active but explicitly DISCONNECTED status is still not a binding.
	if _, err := db.Pool.Exec(ctx, `UPDATE game_servers SET active=TRUE, status='DISCONNECTED' WHERE id=$1`, f.ServerID); err != nil {
		t.Fatal(err)
	}
	if _, err := servers.ConnectedServerID(ctx, f.GuildRowID); err == nil {
		t.Fatal("expected a DISCONNECTED row to stay unbound")
	}
}

// TestPSNLinkRequestEndToEnd drives the real link service over the real
// repositories: guild -> dashboard server -> activity -> five-minute rule.
func TestPSNLinkRequestEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("five minutes observed creates pending link", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		link, err := svc.Request(ctx, f.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), strings.ToLower(f.PlayerName))
		if err != nil {
			t.Fatalf("expected a pending link, got %v", err)
		}
		if link.Status != linking.StatusPending || link.PlayerID != f.PlayerID {
			t.Fatalf("unexpected link %+v", link)
		}
	})

	t.Run("under five minutes is playtime required not unavailable", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 2*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		_, err := svc.Request(ctx, f.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), f.PlayerName)
		if !errors.Is(err, linking.ErrPlaytimeRequired) {
			t.Fatalf("expected ErrPlaytimeRequired, got %v", err)
		}
	})

	t.Run("challenge completes, verifies and assigns the role", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &recordingRoles{}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		link, err := svc.Status(ctx, f.GuildRowID, discordID)
		if err != nil || link == nil || link.Status != linking.StatusVerified {
			t.Fatalf("expected VERIFIED link, got %+v %v", link, err)
		}
		if len(roles.assigned) != 1 || roles.assigned[0] != discordID {
			t.Fatalf("expected the verified role assigned once to %s, got %v", discordID, roles.assigned)
		}
	})

	t.Run("activity on another guild's server never counts", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		other := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		// f's player name is unknown in other's guild.
		_, err := svc.Request(ctx, other.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), f.PlayerName)
		if !errors.Is(err, linking.ErrPlayerNotFound) {
			t.Fatalf("expected cross-guild lookup to be not found, got %v", err)
		}
	})
}

type recordingRoles struct{ assigned []string }

func (r *recordingRoles) AssignVerifiedRole(_ context.Context, discordUserID string) error {
	r.assigned = append(r.assigned, discordUserID)
	return nil
}
