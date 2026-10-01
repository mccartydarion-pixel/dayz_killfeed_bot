//go:build integration

package app

import (
	"context"
	"strings"
	"testing"
	"time"

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

func TestBaseCommandsPayRent(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	owner := w.linkPlayer(w.a1, w.players[0], "Renter")
	other := w.linkPlayer(w.a1, w.players[1], "Other")
	guild, server := w.gameContext(w.a1)
	scope, err := w.a.baseCommandScope(ctx, guild, server)
	if err != nil {
		t.Fatal(err)
	}
	reqs := repository.NewCaseBaseRequestRepository(pool)
	req, err := reqs.Create(ctx, scope, repository.BaseRequestInput{PlayerID: owner, Name: "Hut", CenterX: 100, CenterZ: 200, Radius: 50, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := reqs.Approve(ctx, scope, req.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *approved.Request.BaseID
	if _, err := w.a.baseCommandRentQuote(ctx, guild, server, owner, baseID); err == nil || !strings.Contains(err.Error(), "doesn't charge") {
		t.Fatalf("rent off: %v", err)
	}
	rs := repository.SecurityScope{InstallationID: scope.InstallationID, GuildID: guild, ServerID: server}
	if _, err := repository.NewBaseRentRepository(pool).SetSettings(ctx, rs, true, 300, 5, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.NewBaseRentRepository(pool).SetSettings(context.Background(), rs, false, 300, 5, nil)
	})
	sum, err := w.a.baseCommandSummary(ctx, guild, server, owner)
	if err != nil || sum.RentPrice != 300 || sum.RentDays != 5 || len(sum.Rent) != 1 || sum.Rent[0].BaseID != baseID {
		t.Fatalf("summary rent: %+v %v", sum, err)
	}
	q, err := w.a.baseCommandRentQuote(ctx, guild, server, owner, baseID)
	if err != nil || q.BaseName != "Hut" || q.PricePoints != 300 || q.PeriodDays != 5 {
		t.Fatalf("quote: %+v %v", q, err)
	}
	if _, err := w.a.baseCommandRentQuote(ctx, guild, server, other, baseID); err == nil || !strings.Contains(err.Error(), "can't pay") {
		t.Fatalf("stranger quote: %v", err)
	}
	if _, err := w.a.baseCommandRentPay(ctx, guild, server, owner, baseID, "discord-123456789012345678"); err == nil || !strings.Contains(err.Error(), "enough") {
		t.Fatalf("no points: %v", err)
	}
	if _, err := repository.NewEconomyRepository(pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: owner, Type: repository.TxAdminCredit, Amount: 500, ReferenceID: "discord-rent-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	paid, err := w.a.baseCommandRentPay(ctx, guild, server, owner, baseID, "discord-123456789012345679")
	if err != nil || paid.BaseName != "Hut" || paid.Balance != 200 || paid.Duplicate {
		t.Fatalf("pay: %+v %v", paid, err)
	}
	again, err := w.a.baseCommandRentPay(ctx, guild, server, owner, baseID, "discord-123456789012345679")
	if err != nil || !again.Duplicate || again.BaseName != "Hut" || again.Balance != 200 {
		t.Fatalf("double click must charge once: %+v %v", again, err)
	}
}
