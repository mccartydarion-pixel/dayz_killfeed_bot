//go:build integration

package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// A customer's /server select (ConnectServer) in their own guild selects THAT guild's public
// counter server. It never moves the primary guild's public counter onto the customer's server,
// which is what the primary guild's voice channel shows.
func TestConnectServerSelectsThePublicServerOfItsOwnGuild(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	ctx := context.Background()
	a.Config.DiscordGuildID = fmt.Sprintf("primary-guild-%d", time.Now().UnixNano())
	a.Servers = repository.NewServerRepository(a.DB.Pool)
	primaryRowID, err := a.Guilds.UpsertGuild(ctx, repository.GuildRecord{DiscordGuildID: a.Config.DiscordGuildID})
	if err != nil || primaryRowID == 0 {
		t.Fatalf("primary guild row: id=%d err=%v", primaryRowID, err)
	}
	var started []int64
	a.WorkerManager = servers.NewWorkerManager(func(context.Context, int64) error { return nil })
	_ = started

	fixture := buildInstallationFixture(t, a, verifier)
	customerRowID := mustGuildRowID(t, a, fixture.DiscordGuildID)
	var customerServer, primaryServer int64
	for _, row := range []struct {
		guild int64
		dst   *int64
	}{{customerRowID, &customerServer}, {primaryRowID, &primaryServer}} {
		if err := a.DB.Pool.QueryRow(ctx, `
INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, display_name, status, active)
VALUES($1,'nitrado',$2,'dayz','playstation','Counter Test','CONNECTED',TRUE) RETURNING id`,
			row.guild, fmt.Sprintf("svc-%d-%d", row.guild, time.Now().UnixNano())).Scan(row.dst); err != nil {
			t.Fatal(err)
		}
	}
	a.counterOwnerMu.Lock()
	a.publicCounterServerID = primaryServer
	a.counterOwnerMu.Unlock()
	if err := a.Guilds.SetSelectedPublicServer(ctx, a.Config.DiscordGuildID, primaryServer); err != nil {
		t.Fatal(err)
	}

	if err := a.ConnectServer(ctx, customerServer); err != nil {
		t.Fatalf("ConnectServer: %v", err)
	}
	a.counterOwnerMu.RLock()
	public := a.publicCounterServerID
	a.counterOwnerMu.RUnlock()
	if public != primaryServer {
		t.Fatalf("the primary guild's public counter moved to the customer's server %d", public)
	}
	primaryRec, _, _ := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID)
	if primaryRec == nil || primaryRec.SelectedPublicServerID != primaryServer {
		t.Fatalf("primary guild selection changed: %+v", primaryRec)
	}
	customerRec, err := a.Guilds.GetGuildByID(ctx, customerRowID)
	if err != nil || customerRec == nil || customerRec.SelectedPublicServerID != customerServer {
		t.Fatalf("the customer's guild must select its own server: %+v err=%v", customerRec, err)
	}

	// The primary guild's own server still owns the public counter.
	if err := a.ConnectServer(ctx, primaryServer); err != nil {
		t.Fatalf("ConnectServer(primary): %v", err)
	}
	a.counterOwnerMu.RLock()
	public = a.publicCounterServerID
	a.counterOwnerMu.RUnlock()
	if public != primaryServer {
		t.Fatalf("public counter = %d, want %d", public, primaryServer)
	}
}
