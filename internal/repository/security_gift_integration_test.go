//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestSecurityGifts(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	player := seedPlayer("Lucky")
	ledgerRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, fx.GuildRowID, player).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, bad := range []struct {
		svc  string
		days int
		key  string
	}{{"OFFLINE_PROTECTION", 7, "gift-key-01"}, {ServiceBaseRaidAlarm, 0, "gift-key-01"}, {ServiceBaseRaidAlarm, 91, "gift-key-01"}, {ServiceBaseRaidAlarm, 7, "short"}} {
		if _, _, err := sales.Gift(ctx, scope, player, bad.svc, bad.days, "", fx.OwnerUserID, bad.key); err == nil {
			t.Fatalf("bad gift accepted: %+v", bad)
		}
	}
	if _, _, err := sales.Gift(ctx, scope, player+99999, ServiceBaseRaidAlarm, 7, "", fx.OwnerUserID, "gift-key-02"); err == nil {
		t.Fatal("gift to a player outside the guild accepted")
	}
	first, replay, err := sales.Gift(ctx, scope, player, ServiceSentinelPro, 7, " Thanks for the event ", fx.OwnerUserID, "gift-key-03")
	if err != nil || replay || !first.Gift || first.PricePoints != 0 || first.EndsAt.Sub(first.StartsAt) != 7*24*time.Hour {
		t.Fatalf("gift: %+v %v %v", first, replay, err)
	}
	if again, replay, err := sales.Gift(ctx, scope, player, ServiceSentinelPro, 7, "", fx.OwnerUserID, "gift-key-03"); err != nil || !replay || again.ID != first.ID {
		t.Fatalf("replay: %+v %v %v", again, replay, err)
	}
	second, _, err := sales.Gift(ctx, scope, player, ServiceSentinelPro, 3, "", fx.OwnerUserID, "gift-key-04")
	if err != nil || !second.StartsAt.Equal(first.EndsAt) {
		t.Fatalf("gifts must stack: %+v %v", second, err)
	}
	if ledgerRows() != 0 {
		t.Fatal("a gift moved Champion Points")
	}
	// The bundle gift counts as paid time for a covered service.
	if until, err := sales.ActiveUntil(ctx, fx.InstallationID, player, ServiceBaseBlackBox); err != nil || until == nil || !until.Equal(second.EndsAt) {
		t.Fatalf("gifted paid time: %v %v", until, err)
	}
	gifts, err := sales.RecentGifts(ctx, scope, 10)
	if err != nil || len(gifts) != 2 || gifts[1].Note != "Thanks for the event" || gifts[0].PlayerName != "Lucky" {
		t.Fatalf("recent gifts: %+v %v", gifts, err)
	}
	if name, discordID, err := sales.GiftRecipient(ctx, fx.GuildRowID, player); err != nil || name != "Lucky" || discordID != "" {
		t.Fatalf("recipient: %q %q %v", name, discordID, err)
	}
	// Gifts are not sales.
	sum, err := sales.SalesSummary(ctx, scope, time.Now().AddDate(0, 0, -30), 10)
	if err != nil {
		t.Fatal(err)
	}
	bundle := sum.Services[4]
	if bundle.Sales != 0 || bundle.PointsEarned != 0 || bundle.Gifts != 2 || bundle.ActivePlayers != 1 || sum.TotalSales != 0 || !sum.Recent[0].Gift {
		t.Fatalf("summary: %+v %+v", bundle, sum)
	}
	// The schema refuses a half-gift.
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET price_points=5 WHERE id=$1`, first.ID); err == nil {
		t.Fatal("a gift with a price was accepted")
	}
	// Clean up so global expiry checks in other tests aren't affected.
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET expiry_notified_at=NOW() WHERE installation_id=$1`, fx.InstallationID); err != nil {
		t.Fatal(err)
	}
}
