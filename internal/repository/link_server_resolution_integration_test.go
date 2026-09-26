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
