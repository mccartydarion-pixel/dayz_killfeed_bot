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

// A guild with no saved Nitrado connection gets no worker.
func TestServerHasNitradoFalseWithoutConnection(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	guild, serverID := w.gameContext(w.a1)
	w.a.Servers = repository.NewServerRepository(w.a.DB.Pool)
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM nitrado_connections WHERE guild_id=$1`, guild); err != nil {
		t.Fatal(err)
	}
	ok, err := w.a.serverHasNitrado(ctx, repository.GameServer{ID: serverID, GuildID: guild})
	if err != nil || ok {
		t.Fatalf("serverHasNitrado = %v, %v; want false, nil", ok, err)
	}
}

// Website onboarding stores the Nitrado credential on the organization (guild_id NULL); a server
// that organization owns must be able to use it.
func TestServerUsesOrganizationNitradoCredential(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	guild, serverID := w.gameContext(w.a1)
	w.a.Servers = repository.NewServerRepository(w.a.DB.Pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := w.a.DB.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DELETE FROM nitrado_connections WHERE guild_id=$1 OR organization_id=$2`, guild, w.a1.OrgID)
	exec(`UPDATE game_servers SET organization_id=$1 WHERE id=$2`, w.a1.OrgID, serverID)
	row := repository.GameServer{ID: serverID, GuildID: guild}

	if ok, err := w.a.serverHasNitrado(ctx, row); err != nil || ok {
		t.Fatalf("before connecting: serverHasNitrado = %v, %v", ok, err)
	}
	ct, nonce, version, err := w.a.CredentialCipher.Encrypt([]byte("org-token"))
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO nitrado_connections(guild_id, organization_id, credential_ciphertext, credential_nonce, credential_key_version, status) VALUES(NULL,$1,$2,$3,$4,'ACTIVE')`,
		w.a1.OrgID, ct, nonce, version)

	if ok, err := w.a.serverHasNitrado(ctx, row); err != nil || !ok {
		t.Fatalf("after connecting: serverHasNitrado = %v, %v", ok, err)
	}
	if client, err := w.a.nitradoClientForServer(ctx, row); err != nil || client == nil {
		t.Fatalf("nitradoClientForServer = %v, %v", client, err)
	}
}
