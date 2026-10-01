//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSecurityServiceSalesAndRaidAlarmGate(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	sales := NewSecurityServiceRepository(pool)
	alarm := NewBaseRaidAlarmRepository(pool)
	econ := NewEconomyRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	buyer, poor, raider := seedPlayer("Buyer"), seedPlayer("Poor"), seedPlayer("Raider")
	balance := func(player int64) int64 {
		t.Helper()
		var b int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE((SELECT balance FROM player_points WHERE guild_id=$1 AND player_id=$2),0)`, fx.GuildRowID, player).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := econ.Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: buyer, Type: TxAdminCredit, Amount: 5000, ReferenceID: "seed-buyer", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := econ.Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: poor, Type: TxAdminCredit, Amount: 100, ReferenceID: "seed-poor", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}

	// Not for sale until the owner offers it.
	if offer, err := sales.GetOffer(ctx, scope, ServiceBaseRaidAlarm); err != nil || offer.Enabled || offer.Configured {
		t.Fatalf("default offer: %+v %v", offer, err)
	}
	if _, err := sales.Purchase(ctx, scope, buyer, ServiceBaseRaidAlarm, "request-0001"); !errors.Is(err, ErrSecurityOfferUnavailable) {
		t.Fatalf("bought without an offer: %v", err)
	}
	if _, err := sales.SetOffer(ctx, scope, ServiceBaseRaidAlarm, true, 0, 7, nil); err == nil {
		t.Fatal("zero price accepted")
	}
	if _, err := sales.SetOffer(ctx, SecurityScope{scope.InstallationID, scope.GuildID + 99999, scope.ServerID}, ServiceBaseRaidAlarm, true, 1000, 7, nil); err == nil {
		t.Fatal("offer accepted a mismatched guild")
	}
	if _, err := sales.SetOffer(ctx, scope, ServiceBaseRaidAlarm, true, 1000, 7, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}

	// Purchase charges once, and a replay of the same key charges nothing.
	first, err := sales.Purchase(ctx, scope, buyer, ServiceBaseRaidAlarm, "request-0001")
	if err != nil || first.Duplicate || first.Purchase.PricePoints != 1000 || first.BalanceAfter != 4000 || balance(buyer) != 4000 {
		t.Fatalf("first purchase: %+v %v balance=%d", first, err, balance(buyer))
	}
	if d := first.Purchase.EndsAt.Sub(first.Purchase.StartsAt); d != 7*24*time.Hour {
		t.Fatalf("paid time: %v", d)
	}
	replay, err := sales.Purchase(ctx, scope, buyer, ServiceBaseRaidAlarm, "request-0001")
	if err != nil || !replay.Duplicate || replay.Purchase.ID != first.Purchase.ID || balance(buyer) != 4000 {
		t.Fatalf("replay charged again: %+v %v balance=%d", replay, err, balance(buyer))
	}
	// Buying again while active adds the time on the end.
	second, err := sales.Purchase(ctx, scope, buyer, ServiceBaseRaidAlarm, "request-0002")
	if err != nil || !second.Purchase.StartsAt.Equal(first.Purchase.EndsAt) || balance(buyer) != 3000 {
		t.Fatalf("second purchase did not stack: %+v %v", second, err)
	}
	var ledgerType string
	if err := pool.QueryRow(ctx, `SELECT reason_type FROM point_transactions WHERE id=(SELECT ledger_entry_id FROM security_service_purchases WHERE id=$1)`, first.Purchase.ID).Scan(&ledgerType); err != nil || ledgerType != TxSecurityPurchase {
		t.Fatalf("ledger row: %q %v", ledgerType, err)
	}
	// Not enough points: nothing charged, nothing recorded.
	if _, err := sales.Purchase(ctx, scope, poor, ServiceBaseRaidAlarm, "request-poor1"); !errors.Is(err, ErrInsufficientFunds) || balance(poor) != 100 {
		t.Fatalf("poor purchase: %v balance=%d", err, balance(poor))
	}
	if until, err := sales.ActiveUntil(ctx, scope.InstallationID, poor, ServiceBaseRaidAlarm); err != nil || until != nil {
		t.Fatalf("poor player has paid time: %v %v", until, err)
	}
	if until, err := sales.ActiveUntil(ctx, scope.InstallationID, buyer, ServiceBaseRaidAlarm); err != nil || until == nil || !until.Equal(second.Purchase.EndsAt) {
		t.Fatalf("buyer paid time: %v %v", until, err)
	}
	sold, active, err := sales.RecentSales(ctx, scope, ServiceBaseRaidAlarm, 10)
	if err != nil || len(sold) != 2 || active != 1 || sold[0].PlayerName != "Buyer" {
		t.Fatalf("recent sales: %+v %d %v", sold, active, err)
	}

	// The alarm only fires for paying owners while the offer is on.
	var paidBase, unpaidBase int64
	for owner, target := range map[int64]*int64{buyer: &paidBase, poor: &unpaidBase} {
		x := 1000.0
		if owner == poor {
			x = 5000
		}
		if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Base',$5,1000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner, x).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := alarm.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	var raiderADM string
	if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, raider).Scan(&raiderADM); err != nil {
		t.Fatal(err)
	}
	matchAt := func(x float64) int {
		t.Helper()
		m, err := alarm.MatchRaid(ctx, fx.GuildRowID, fx.ServerRowID, raiderADM, x, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}
	if matchAt(1000) != 1 || matchAt(5000) != 0 {
		t.Fatalf("while selling: paid base must alarm, unpaid must not (paid=%d unpaid=%d)", matchAt(1000), matchAt(5000))
	}
	if _, err := sales.SetOffer(ctx, scope, ServiceBaseRaidAlarm, false, 1000, 7, nil); err != nil {
		t.Fatal(err)
	}
	if matchAt(1000) != 1 || matchAt(5000) != 1 {
		t.Fatal("with selling off the alarm must be free for every base")
	}

	// Ended paid time gets exactly one expiry, and only when nothing follows it.
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET starts_at=NOW()-INTERVAL '20 days',ends_at=NOW()-INTERVAL '13 days' WHERE id=$1`, first.Purchase.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET starts_at=NOW()-INTERVAL '13 days',ends_at=NOW()-INTERVAL '6 days' WHERE id=$1`, second.Purchase.ID); err != nil {
		t.Fatal(err)
	}
	due, err := sales.DueExpiries(ctx, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("due expiries: %+v %v", due, err)
	}
	for _, e := range due {
		if err := sales.MarkExpiryNotified(ctx, e.PurchaseID); err != nil {
			t.Fatal(err)
		}
	}
	if again, err := sales.DueExpiries(ctx, 10); err != nil || len(again) != 0 {
		t.Fatalf("expiry announced twice: %+v %v", again, err)
	}
	third, err := sales.Purchase(ctx, scope, buyer, ServiceBaseRaidAlarm, "request-0003")
	if err == nil {
		t.Fatalf("bought while the offer is off: %+v", third)
	}
}
