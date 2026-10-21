//go:build integration

package repository

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// Plan tiers (internal/entitlements) against a real database: the runtime route lookup,
// the custom-embed lookup and the faction cap.

func enforcePlans(t *testing.T, on bool) {
	t.Helper()
	prev := entitlements.Enforced()
	entitlements.SetEnforced(on)
	t.Cleanup(func() { entitlements.SetEnforced(prev) })
}

func (w *hubWorld) setPlan(org int64, plan string) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(w.ctx, `
INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,'ACTIVE')
ON CONFLICT (organization_id) DO UPDATE SET plan = EXCLUDED.plan`, org, plan); err != nil {
		w.t.Fatal(err)
	}
}

func (w *hubWorld) guildAndServer(inst int64) (guildRow, server int64) {
	w.t.Helper()
	if err := w.db.Pool.QueryRow(w.ctx, `
SELECT c.guild_id, i.game_server_id FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id WHERE i.id=$1`, inst).Scan(&guildRow, &server); err != nil {
		w.t.Fatal(err)
	}
	return guildRow, server
}

func TestPlanGatingChannelRoutes(t *testing.T) {
	w := newHubWorld(t)
	repo := NewChannelRouteRepository(w.db.Pool)
	for route, ch := range map[string]string{"BOUNTY": "ch-bounty", "HEATMAPS": "ch-heat", "ECONOMY": "ch-eco", "SERVER_RANKS": "ch-ranks", "KILLFEED": "ch-kill"} {
		if _, err := w.db.Pool.Exec(w.ctx, `INSERT INTO installation_channel_routes(installation_id, route_key, channel_id) VALUES($1,$2,$3)`, w.inst1, route, ch); err != nil {
			t.Fatal(err)
		}
	}
	guildRow, server := w.guildAndServer(w.inst1)
	resolve := func(route string) bool {
		t.Helper()
		_, found, err := repo.ResolveChannel(w.ctx, guildRow, server, route)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	premiumRoutes := []string{"BOUNTY", "HEATMAPS", "ECONOMY", "SERVER_RANKS"}

	enforcePlans(t, true)
	w.setPlan(w.org1, "NORMAL")
	for _, route := range premiumRoutes {
		if resolve(route) {
			t.Fatalf("Survivor must not resolve the %s route", route)
		}
	}
	if !resolve("KILLFEED") {
		t.Fatal("Survivor keeps the killfeed route")
	}

	w.setPlan(w.org1, "PREMIUM")
	for _, route := range append(premiumRoutes, "KILLFEED") {
		if !resolve(route) {
			t.Fatalf("Champion must resolve the %s route", route)
		}
	}

	// No subscription row: nothing is taken away.
	if _, err := w.db.Pool.Exec(w.ctx, `DELETE FROM subscriptions WHERE organization_id=$1`, w.org1); err != nil {
		t.Fatal(err)
	}
	if !resolve("BOUNTY") {
		t.Fatal("an organization without a subscription row keeps every route")
	}

	// Gating off: Survivor gets everything, exactly as before plan tiers.
	w.setPlan(w.org1, "NORMAL")
	entitlements.SetEnforced(false)
	if !resolve("BOUNTY") {
		t.Fatal("with gating off Survivor resolves every route")
	}
}

func TestPlanGatingFactionLimit(t *testing.T) {
	w := newHubWorld(t)
	in := func(n int) HubFactionInput {
		return HubFactionInput{Name: fmt.Sprintf("Capped %d", n), Tag: fmt.Sprintf("CP%d", n), RecruitmentStatus: "OPEN"}
	}
	users := w.newUsers(4)
	var created []*HubFaction
	for i := 0; i < 2; i++ {
		f, err := w.repo.CreateFactionWithLimit(w.ctx, w.org1, w.inst1, users[i], in(i), 2)
		if err != nil {
			t.Fatalf("faction %d under the cap: %v", i, err)
		}
		created = append(created, f)
	}
	_, err := w.repo.CreateFactionWithLimit(w.ctx, w.org1, w.inst1, users[2], in(2), 2)
	if !errors.Is(err, factionhub.ErrFactionLimitReached) {
		t.Fatalf("third faction over a cap of 2: want ErrFactionLimitReached, got %v", err)
	}
	// The cap is per installation: the organization's other server is unaffected.
	if _, err := w.repo.CreateFactionWithLimit(w.ctx, w.org1, w.inst1b, users[2], in(2), 2); err != nil {
		t.Fatalf("another installation has its own cap: %v", err)
	}
	// Unlimited (0) ignores the count; CreateFaction is unlimited.
	if _, err := w.repo.CreateFaction(w.ctx, w.org1, w.inst1, users[3], in(3)); err != nil {
		t.Fatalf("CreateFaction is uncapped: %v", err)
	}
	// A dissolved faction frees its slot.
	if _, err := w.db.Pool.Exec(w.ctx, `DELETE FROM hub_factions WHERE id = ANY($1)`, []int64{created[0].ID, created[1].ID}); err != nil {
		t.Fatal(err)
	}
	fresh := w.newUser()
	if _, err := w.repo.CreateFactionWithLimit(w.ctx, w.org1, w.inst1, fresh, in(4), 2); err != nil {
		t.Fatalf("after dissolving, a slot is free again: %v", err)
	}
}

// Concurrent creators cannot both slip past the cap.
func TestPlanGatingFactionLimitConcurrent(t *testing.T) {
	w := newHubWorld(t)
	const limit, racers = 3, 8
	users := w.newUsers(racers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, capped := 0, 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := w.repo.CreateFactionWithLimit(w.ctx, w.org1, w.inst1b, users[i], HubFactionInput{Name: fmt.Sprintf("Race %d", i), Tag: fmt.Sprintf("RC%d", i), RecruitmentStatus: "OPEN"}, limit)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, factionhub.ErrFactionLimitReached):
				capped++
			default:
				t.Errorf("racer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if ok != limit || capped != racers-limit {
		t.Fatalf("created %d, capped %d; want exactly %d created", ok, capped, limit)
	}
	if n := w.count(`SELECT COUNT(*) FROM hub_factions WHERE installation_id=$1`, w.inst1b); n != limit {
		t.Fatalf("installation has %d factions, want %d", n, limit)
	}
}

func TestPlanGatingCustomEmbedTemplates(t *testing.T) {
	w := newEmbedWorld(t)
	org, first := w.installation()
	inst := w.secondInstallation(org, first) // has a DayZ server, like the runtime lookup needs
	var guildRow, server int64
	w.must(w.pool.QueryRow(w.ctx, `SELECT c.guild_id, i.game_server_id FROM installations i JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id WHERE i.id=$1`, inst).Scan(&guildRow, &server))
	if _, err := w.repo.Upsert(w.ctx, org, inst, sampleConfig("KILLFEED", "custom-card")); err != nil {
		t.Fatal(err)
	}
	w.must(w.repo.SetActivation(w.ctx, org, inst, "KILLFEED", EmbedModeCustom, 0))
	setPlan := func(plan string) {
		_, err := w.pool.Exec(w.ctx, `INSERT INTO subscriptions(organization_id, plan, status) VALUES($1,$2,'ACTIVE')
ON CONFLICT (organization_id) DO UPDATE SET plan = EXCLUDED.plan`, org, plan)
		w.must(err)
	}

	enforcePlans(t, true)
	setPlan("NORMAL")
	gotInst, cfg, err := w.repo.ResolveTemplate(w.ctx, guildRow, server, "KILLFEED")
	w.must(err)
	if gotInst != inst || cfg != nil {
		t.Fatalf("Survivor keeps the default card (installation still reported): %d %+v", gotInst, cfg)
	}
	setPlan("PREMIUM")
	gotInst, cfg, err = w.repo.ResolveTemplate(w.ctx, guildRow, server, "KILLFEED")
	w.must(err)
	if gotInst != inst || cfg == nil || cfg.Title.Template != "custom-card" {
		t.Fatalf("Champion renders the saved template: %d %+v", gotInst, cfg)
	}
}
