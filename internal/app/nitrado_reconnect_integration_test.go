//go:build integration

package app

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/servers"
)

// Reconnecting Nitrado restarts the organization's running workers so they read the new token: a
// worker builds its Nitrado client once, at start. Another organization's worker is untouched.
func TestNitradoReconnectRestartsTheOrganizationsRunningWorkers(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	fixture := buildInstallationFixture(t, a, verifier)
	completeDiscordStep(t, a, fixture)
	withFakeNitradoServer(t, a, 200, psServiceJSON)
	a.Servers = repository.NewServerRepository(a.DB.Pool)
	ctx := context.Background()

	var guildID int64
	if err := a.DB.Pool.QueryRow(ctx, `SELECT c.guild_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, fixture.InstallationID).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	newServer := func(orgID *int64) int64 {
		var id int64
		if err := a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status, active, organization_id) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',TRUE,$3) RETURNING id`,
			guildID, fmt.Sprintf("rc-%d", time.Now().UnixNano()), orgID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ours := newServer(&fixture.OrgID)
	unowned := newServer(nil)

	starts := map[int64]*atomic.Int32{ours: {}, unowned: {}}
	a.WorkerManager = servers.NewWorkerManager(func(ctx context.Context, id int64) error {
		starts[id].Add(1)
		<-ctx.Done()
		return nil
	})
	t.Cleanup(a.WorkerManager.StopAll)
	for _, id := range []int64{ours, unowned} {
		if err := a.WorkerManager.Start(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(id int64, n int32) bool {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if starts[id].Load() >= n && a.WorkerManager.Running(id) {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !waitFor(ours, 1) || !waitFor(unowned, 1) {
		t.Fatal("workers did not start")
	}

	a.restartOrganizationWorkers(ctx, fixture.OrgID)

	if !waitFor(ours, 2) {
		t.Fatalf("the organization's worker must be restarted on the new token, starts=%d running=%t", starts[ours].Load(), a.WorkerManager.Running(ours))
	}
	time.Sleep(50 * time.Millisecond)
	if got := starts[unowned].Load(); got != 1 {
		t.Fatalf("a server the organization does not own must not be restarted, starts=%d", got)
	}
}
