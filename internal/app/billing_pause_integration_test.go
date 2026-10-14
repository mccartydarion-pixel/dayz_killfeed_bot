//go:build integration

package app

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// serverBillingPaused follows game server -> organization -> subscription.
func TestServerBillingPausedFollowsSubscription(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	_, serverID := w.gameContext(w.a1)
	w.a.Servers = repository.NewServerRepository(w.a.DB.Pool)
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE game_servers SET organization_id=$1 WHERE id=$2`, w.a1.OrgID, serverID); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := w.a.DB.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	paused := func() bool {
		t.Helper()
		p, err := w.a.serverBillingPaused(ctx, serverID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	setOrgPlan(t, w.a, w.a1.OrgID, "CHAMPION")
	if paused() {
		t.Fatal("an active subscription must not pause the server")
	}

	exec(`UPDATE subscriptions SET status='TRIAL', provider_subscription_id=NULL, trial_ends_at=NOW() - INTERVAL '1 hour' WHERE organization_id=$1`, w.a1.OrgID)
	if !paused() {
		t.Fatal("an expired trial must pause the server")
	}

	exec(`UPDATE game_servers SET organization_id=NULL WHERE id=$1`, serverID)
	if paused() {
		t.Fatal("a server no organization owns must never be paused")
	}
}
