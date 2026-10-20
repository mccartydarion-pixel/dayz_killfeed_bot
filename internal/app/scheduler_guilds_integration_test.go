//go:build integration

package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Scheduled per-guild work (rewards, season planner, VIP expiry, event finalization, hot zones)
// runs for the primary guild and every guild with an active server, each once; a guild whose
// only server is inactive is left out.
func TestSchedulerGuildsCoverEveryGuildWithAnActiveServer(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	a.Servers = repository.NewServerRepository(a.DB.Pool)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	guild := func(tag string) int64 {
		var id int64
		if err := a.DB.Pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("sg-%s-%d", tag, suffix)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	n := 0
	server := func(guildID int64, active bool) {
		n++
		if _, err := a.DB.Pool.Exec(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status, active) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE',$3)`,
			guildID, fmt.Sprintf("sg-%d-%d-%d", guildID, suffix, n), active); err != nil {
			t.Fatal(err)
		}
	}
	primary, customer, idle := guild("primary"), guild("customer"), guild("idle")
	server(primary, true)
	server(customer, true)
	server(customer, true)
	server(idle, false)

	got := a.schedulerGuilds(ctx, primary)
	count := map[int64]int{}
	for _, id := range got {
		count[id]++
	}
	if len(got) == 0 || got[0] != primary {
		t.Fatalf("the primary guild comes first, got %v", got)
	}
	if count[primary] != 1 || count[customer] != 1 {
		t.Fatalf("each guild with an active server runs exactly once, got %v", got)
	}
	if count[idle] != 0 {
		t.Fatalf("a guild with no active server is skipped, got %v", got)
	}
}
