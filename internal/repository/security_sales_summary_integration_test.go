//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestSecuritySalesSummary(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	a, b := seedPlayer("Alpha"), seedPlayer("Bravo")
	econ := NewEconomyRepository(pool)
	for _, p := range []int64{a, b} {
		if _, err := econ.Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: p, Type: TxAdminCredit, Amount: 10000, ReferenceID: "sum-seed-" + time.Now().Format("150405.000000"), CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := sales.SalesSummary(ctx, scope, time.Now().AddDate(0, 0, -30), 10)
	if err != nil || len(empty.Services) != 5 || empty.TotalSales != 0 || empty.Services[0].ServiceID != ServiceBaseRaidAlarm || empty.Services[4].ServiceID != ServiceSentinelPro {
		t.Fatalf("empty summary: %+v %v", empty, err)
	}
	for _, o := range []struct {
		svc   string
		price int64
	}{{ServiceBaseRaidAlarm, 500}, {ServicePerimeterWatch, 300}, {ServiceSentinelPro, 2000}} {
		if _, err := sales.SetOffer(ctx, scope, o.svc, true, o.price, 7, nil); err != nil {
			t.Fatal(err)
		}
	}
	buy := func(player int64, svc, key string) SecurityPurchase {
		t.Helper()
		res, err := sales.Purchase(ctx, scope, player, svc, key)
		if err != nil {
			t.Fatal(err)
		}
		return res.Purchase
	}
	buy(a, ServiceBaseRaidAlarm, "sum-key-001")
	buy(a, ServiceBaseRaidAlarm, "sum-key-002") // stacked: same buyer
	buy(b, ServiceBaseRaidAlarm, "sum-key-003")
	old := buy(b, ServicePerimeterWatch, "sum-key-004")
	buy(a, ServiceSentinelPro, "sum-key-005")
	// The perimeter sale was long ago and has ended.
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET created_at=NOW()-INTERVAL '60 days',starts_at=NOW()-INTERVAL '60 days',ends_at=NOW()-INTERVAL '53 days',expiry_notified_at=NOW() WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}
	sum, err := sales.SalesSummary(ctx, scope, time.Now().AddDate(0, 0, -30), 10)
	if err != nil {
		t.Fatal(err)
	}
	raid, perimeter, bundle := sum.Services[0], sum.Services[1], sum.Services[4]
	if raid.Sales != 3 || raid.PointsEarned != 1500 || raid.Buyers != 2 || raid.ActivePlayers != 2 {
		t.Fatalf("raid row: %+v", raid)
	}
	if perimeter.Sales != 0 || perimeter.PointsEarned != 0 || perimeter.ActivePlayers != 0 {
		t.Fatalf("perimeter row (old sale outside the window): %+v", perimeter)
	}
	if bundle.Sales != 1 || bundle.PointsEarned != 2000 || bundle.ActivePlayers != 1 {
		t.Fatalf("bundle row: %+v", bundle)
	}
	if sum.TotalSales != 4 || sum.TotalPoints != 3500 || sum.PlayersWithAny != 2 || len(sum.Recent) != 5 || sum.Recent[0].ServiceID != ServiceSentinelPro {
		t.Fatalf("totals: %+v", sum)
	}
	long, err := sales.SalesSummary(ctx, scope, time.Now().AddDate(0, 0, -90), 10)
	if err != nil || long.Services[1].Sales != 1 || long.TotalPoints != 3800 {
		t.Fatalf("90-day window: %+v %v", long, err)
	}
}
