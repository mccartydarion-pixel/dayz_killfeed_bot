//go:build integration

package app

import (
	"context"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseCommandsSummaryAndRequest(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.Locations = repository.NewLocationRepository(pool)
	player := w.linkPlayer(w.a1, w.players[0], "Builder")
	guild, server := w.gameContext(w.a1)

	sum, err := w.a.baseCommandSummary(ctx, guild, server, player)
	if err != nil || len(sum.Bases) != 0 || sum.Pending != "" || len(sum.PaidUntil) != 0 {
		t.Fatalf("empty summary: %+v %v", sum, err)
	}
	if _, err := w.a.baseCommandRequest(ctx, guild, server, player, "Hilltop", 50, ""); err == nil || !strings.Contains(err.Error(), "30 minutes") {
		t.Fatalf("no position must be refused: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at)
 VALUES($1,$2,$3,'Builder',100,200,'PLAYER_LIST',NOW())`, guild, server, player); err != nil {
		t.Fatal(err)
	}
	msg, err := w.a.baseCommandRequest(ctx, guild, server, player, "Hilltop", 50, "by the tower")
	if err != nil || !strings.Contains(msg, "Hilltop") {
		t.Fatalf("request: %q %v", msg, err)
	}
	if _, err := w.a.baseCommandRequest(ctx, guild, server, player, "Again", 50, ""); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("second request: %v", err)
	}
	if _, err := w.a.baseCommandRequest(ctx, guild, server, player, "Huge", 500, ""); err == nil {
		t.Fatal("oversized base accepted")
	}
	sum, err = w.a.baseCommandSummary(ctx, guild, server, player)
	if err != nil || sum.Pending != "Hilltop" {
		t.Fatalf("pending summary: %+v %v", sum, err)
	}
	if _, err := w.a.baseCommandSummary(ctx, guild, server+999999, player); err == nil {
		t.Fatal("a server without an installation must fail")
	}
}
