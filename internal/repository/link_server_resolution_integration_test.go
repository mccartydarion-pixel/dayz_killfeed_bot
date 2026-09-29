//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/linking"
)

// linkResolutionFixture is one guild with one observed player and the real
// repositories wired into the real LinkVerificationService, exactly as app.go
// does (linking.NewService(links, activity, servers, links)).
type linkResolutionFixture struct {
	ctx      context.Context
	guildID  int64
	playerID int64
	name     string
	servers  *ServerRepository
	activity *ActivityRepository
	links    *LinkRepository
	service  *linking.LinkVerificationService
	suffix   int64
}

func newLinkResolutionFixture(t *testing.T) *linkResolutionFixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f := &linkResolutionFixture{
		ctx:      ctx,
		servers:  NewServerRepository(db.Pool),
		activity: NewActivityRepository(db.Pool),
		links:    NewLinkRepository(db.Pool),
		suffix:   time.Now().UnixNano(),
	}
	f.service = linking.NewService(f.links, f.activity, f.servers, f.links)
	f.guildID, err = NewGuildRepository(db.Pool).UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-link-resolve-%d", f.suffix)})
	if err != nil {
		t.Fatal(err)
	}
	f.name = fmt.Sprintf("LinkPlayer%d", f.suffix)
	f.playerID, err = NewPlayerRepository(db.Pool).UpsertPlayer(ctx, f.guildID, fmt.Sprintf("dayz-link-%d", f.suffix), f.name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *linkResolutionFixture) server(t *testing.T, key, status string, active bool) int64 {
	t.Helper()
	s, err := f.servers.UpsertGameServer(f.ctx, GameServer{GuildID: f.guildID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("svc-%s-%d", key, f.suffix), Game: "DayZ", DisplayName: key, Status: status, Active: active})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// observe records a completed session of d on serverID through the same
// Connect/Disconnect calls the ADM persistence queue makes.
func (f *linkResolutionFixture) observe(t *testing.T, serverID int64, d time.Duration) {
	t.Helper()
	start := time.Now().Add(-time.Hour)
	if err := f.activity.Connect(f.ctx, f.guildID, serverID, f.playerID, start); err != nil {
		t.Fatal(err)
	}
	if err := f.activity.Disconnect(f.ctx, f.guildID, serverID, f.playerID, start.Add(d)); err != nil {
		t.Fatal(err)
	}
}

// TestLinkResolvesServerConnectedThroughDashboard is the LINK CHECK UNAVAILABLE
// regression: the SaaS dashboard connects a server with status ONLINE/OFFLINE
// (nitradoServiceStatus), the ADM worker ingests activity for it because it is
// active, and linking must read that same server's activity.
func TestLinkResolvesServerConnectedThroughDashboard(t *testing.T) {
	for _, status := range []string{"ONLINE", "OFFLINE"} {
		t.Run(status, func(t *testing.T) {
			f := newLinkResolutionFixture(t)
			serverID := f.server(t, "dash", status, true)
			f.observe(t, serverID, 6*time.Minute)

			link, err := f.service.Request(f.ctx, f.guildID, "discord-"+status, f.name)
			if err != nil {
				t.Fatalf("Request = %v; want a pending link for an observed player on an active %s server", err, status)
			}
			if link.PlayerID != f.playerID || link.Status != linking.StatusPending {
				t.Fatalf("link = %+v", link)
			}
		})
	}
}

// TestLinkWithMultipleActiveServersUsesObservedServer: a guild with more than
// one active server (the multi-server runtime) must not fail every link.
// Playtime is judged per server, never summed across servers.
func TestLinkWithMultipleActiveServersUsesObservedServer(t *testing.T) {
	f := newLinkResolutionFixture(t)
	a := f.server(t, "a", "CONNECTED", true)
	b := f.server(t, "b", "CONNECTED", true)
	f.observe(t, b, 6*time.Minute)
	_ = a
	if _, err := f.service.Request(f.ctx, f.guildID, "discord-multi", f.name); err != nil {
		t.Fatalf("Request = %v; want a pending link from server b's observed activity", err)
	}

	g := newLinkResolutionFixture(t)
	a2 := g.server(t, "a", "CONNECTED", true)
	b2 := g.server(t, "b", "CONNECTED", true)
	g.observe(t, a2, 3*time.Minute)
	g.observe(t, b2, 3*time.Minute)
	if _, err := g.service.Request(g.ctx, g.guildID, "discord-split", g.name); !errors.Is(err, linking.ErrPlaytimeRequired) {
		t.Fatalf("Request = %v; want ErrPlaytimeRequired (3+3 minutes on two servers is not 5 minutes on one)", err)
	}
}

// TestLinkWithoutActiveServerIsNotConfiguredNotUnavailable: no active server is
// a configuration state, not a backend outage, and an inactive server's old
// activity never counts.
func TestLinkWithoutActiveServerIsNotConfiguredNotUnavailable(t *testing.T) {
	f := newLinkResolutionFixture(t)
	if _, err := f.service.Request(f.ctx, f.guildID, "discord-none", f.name); !errors.Is(err, linking.ErrNoConnectedServer) {
		t.Fatalf("no server: Request = %v; want ErrNoConnectedServer", err)
	}
	inactive := f.server(t, "gone", "DISCONNECTED", false)
	f.observe(t, inactive, time.Hour)
	if _, err := f.service.Request(f.ctx, f.guildID, "discord-none", f.name); !errors.Is(err, linking.ErrNoConnectedServer) {
		t.Fatalf("inactive server: Request = %v; want ErrNoConnectedServer", err)
	}
}
func linkIntegrationDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestLinkRequestSucceedsForSaaSOnboardedServer is the LINK CHECK UNAVAILABLE
// regression, end to end against real SQL: the SaaS installation flow stores
// game_servers.status as ONLINE/OFFLINE (Nitrado's service status), which the
// old ConnectedServerID status filter ('connected','ready','active') rejected,
// so every /link failed before activity was even read.
func TestLinkRequestSucceedsForSaaSOnboardedServer(t *testing.T) {
	db := linkIntegrationDB(t)
	ctx := context.Background()
	guilds, players, servers := NewGuildRepository(db.Pool), NewPlayerRepository(db.Pool), NewServerRepository(db.Pool)
	activity, links := NewActivityRepository(db.Pool), NewLinkRepository(db.Pool)
	service := linking.NewService(links, activity, servers, links)

	suffix := time.Now().UnixNano()
	discordGuild := fmt.Sprintf("integration-link-saas-%d", suffix)
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: discordGuild})
	if err != nil {
		t.Fatal(err)
	}

	playerID, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-link-%d", suffix), "ChampionTCP", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// No server at all: installation not configured (not a generic outage).
	if _, err := service.Request(ctx, guildID, "discord-user", "ChampionTCP"); !errors.Is(err, linking.ErrInstallationNotConfigured) {
		t.Fatalf("expected installation-not-configured with no server, got %v", err)
	}

	// Exactly what UpsertForInstallation writes for a started Nitrado service.
	srv, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guildID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("svc-%d", suffix), Game: "DayZ", Platform: "PLAYSTATION", DisplayName: "Champions", Status: "ONLINE", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := servers.ConnectedServerID(ctx, guildID); err != nil || got != srv.ID {
		t.Fatalf("expected the ONLINE SaaS server to resolve, got %d, %v", got, err)
	}

	// Player not observed on the server yet.
	if _, err := service.Request(ctx, guildID, "discord-user", "championtcp"); !errors.Is(err, linking.ErrPlayerNotObserved) {
		t.Fatalf("expected player-not-observed, got %v", err)
	}

	// Two minutes observed: insufficient, with the observed amount reported.
	now := time.Now()
	if err := activity.Connect(ctx, guildID, srv.ID, playerID, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var shortfall *linking.PlaytimeShortfallError
	if _, err := service.Request(ctx, guildID, "discord-user", "ChampionTCP"); !errors.As(err, &shortfall) || shortfall.Observed < time.Minute || shortfall.Observed >= 5*time.Minute {
		t.Fatalf("expected a playtime shortfall of ~2m, got %v", err)
	}

	// Six minutes of checkpointed activity: the pending link is created.
	if _, err := db.Pool.Exec(ctx, `UPDATE player_server_activity SET total_observed_seconds=360, last_observed_at=$4 WHERE guild_id=$1 AND server_id=$2 AND player_id=$3`, guildID, srv.ID, playerID, now); err != nil {
		t.Fatal(err)
	}
	link, err := service.Request(ctx, guildID, "discord-user", "ChampionTCP")
	if err != nil {
		t.Fatalf("expected a pending link after 6 observed minutes, got %v", err)
	}
	if link.PlayerID != playerID || link.Status != linking.StatusPending {
		t.Fatalf("unexpected link %+v", link)
	}

	// Server restart closes the phantom session but keeps accrued playtime.
	if err := activity.ResetConnectedForRestart(ctx, guildID, srv.ID); err != nil {
		t.Fatal(err)
	}
	row, err := activity.Get(ctx, guildID, srv.ID, playerID)
	if err != nil || row == nil || row.CurrentlyConnected || row.TotalObservedSeconds != 360 {
		t.Fatalf("expected closed session with 360s preserved, got %+v, %v", row, err)
	}
}

// ConnectedServerID (single-server views such as admin diagnostics): the
// guild's selected public server decides; without a selection several active
// servers are ambiguous. Linking itself does not use this rule.
func TestConnectedServerIDUsesSelectedPublicServer(t *testing.T) {
	db := linkIntegrationDB(t)
	ctx := context.Background()
	guilds, servers := NewGuildRepository(db.Pool), NewServerRepository(db.Pool)
	suffix := time.Now().UnixNano()
	discordGuild := fmt.Sprintf("integration-link-multi-%d", suffix)
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: discordGuild})
	if err != nil {
		t.Fatal(err)
	}
	a, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guildID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("a-%d", suffix), Game: "DayZ", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guildID, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("b-%d", suffix), Game: "DayZ", Status: "OFFLINE", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := servers.ConnectedServerID(ctx, guildID); !errors.Is(err, linking.ErrMultipleConnectedServers) {
		t.Fatalf("expected multiple-servers without a selection, got %v", err)
	}
	if err := guilds.SetSelectedPublicServer(ctx, discordGuild, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := servers.ConnectedServerID(ctx, guildID); err != nil || got != b.ID {
		t.Fatalf("expected selected server %d, got %d, %v", b.ID, got, err)
	}
	// A selected server that was deactivated no longer qualifies.
	if err := servers.Deactivate(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := servers.ConnectedServerID(ctx, guildID); err != nil || got != a.ID {
		t.Fatalf("expected fallback to the only active server %d, got %d, %v", a.ID, got, err)
	}
}
