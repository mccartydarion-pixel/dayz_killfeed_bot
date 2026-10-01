//go:build integration

package repository

import (
	"context"
	"testing"
)

func TestSentinelProCoversEveryBaseService(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	owner, mate, stranger := seedPlayer("Owner"), seedPlayer("Mate"), seedPlayer("Stranger")
	for p, id := range map[int64]string{owner: "sp-owner", mate: "sp-mate"} {
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, fx.GuildRowID, p, id); err != nil {
			t.Fatal(err)
		}
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'SP','SP',$2) RETURNING id`, fx.GuildRowID, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for p, role := range map[int64]string{owner: "OWNER", mate: "MEMBER"} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, fx.GuildRowID, faction, p, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Fort',1000,2000,50)`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner); err != nil {
		t.Fatal(err)
	}
	var strangerADM string
	if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, stranger).Scan(&strangerADM); err != nil {
		t.Fatal(err)
	}
	// Every service switched on, nothing sold: free for everyone.
	if _, err := NewBaseRaidAlarmRepository(pool).SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPerimeterWatchRepository(pool).SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 100, 1800, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBaseBlackBoxRepository(pool).SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 14, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFactionSecurityRepository(pool).SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, nil); err != nil {
		t.Fatal(err)
	}
	counts := func() (raid, perimeter, blackBox, faction int) {
		t.Helper()
		r, err := NewBaseRaidAlarmRepository(pool).MatchRaid(ctx, fx.GuildRowID, fx.ServerRowID, strangerADM, 1000, 2000)
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewPerimeterWatchRepository(pool).MatchPerimeter(ctx, fx.GuildRowID, fx.ServerRowID, stranger, 1000, 2000)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewBaseBlackBoxRepository(pool).MatchVisit(ctx, fx.GuildRowID, fx.ServerRowID, stranger, 1000, 2000)
		if err != nil {
			t.Fatal(err)
		}
		f, err := NewFactionSecurityRepository(pool).Recipients(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner)
		if err != nil {
			t.Fatal(err)
		}
		return len(r), len(p), len(b), len(f)
	}
	if r, p, b, f := counts(); r != 1 || p != 1 || b != 1 || f != 1 {
		t.Fatalf("free while nothing is sold: %d %d %d %d", r, p, b, f)
	}
	// Selling only the bundle makes every covered service paid-only.
	if _, err := sales.SetOffer(ctx, scope, ServiceSentinelPro, true, 2000, 30, nil); err != nil {
		t.Fatal(err)
	}
	for _, svc := range SentinelProCovers {
		if on, err := sales.OnSale(ctx, scope, svc); err != nil || !on {
			t.Fatalf("%s should count as on sale through the bundle: %v %v", svc, on, err)
		}
	}
	if r, p, b, f := counts(); r+p+b+f != 0 {
		t.Fatalf("unpaid owner got services while the bundle is sold: %d %d %d %d", r, p, b, f)
	}
	// Buying the bundle turns them all on for this owner.
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: owner, Type: TxAdminCredit, Amount: 2500, ReferenceID: "sp-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := sales.Purchase(ctx, scope, owner, ServiceSentinelPro, "sp-request-1")
	if err != nil || bundle.BalanceAfter != 500 || bundle.Purchase.ServiceID != ServiceSentinelPro {
		t.Fatalf("bundle purchase: %+v %v", bundle, err)
	}
	if r, p, b, f := counts(); r != 1 || p != 1 || b != 1 || f != 1 {
		t.Fatalf("bundle didn't cover every service: %d %d %d %d", r, p, b, f)
	}
	for _, svc := range SentinelProCovers {
		if until, err := sales.ActiveUntil(ctx, fx.InstallationID, owner, svc); err != nil || until == nil || !until.Equal(bundle.Purchase.EndsAt) {
			t.Fatalf("%s paid time through the bundle: %v %v", svc, until, err)
		}
	}
	if until, err := sales.ActiveUntil(ctx, fx.InstallationID, stranger, ServiceBaseRaidAlarm); err != nil || until != nil {
		t.Fatalf("stranger has paid time: %v %v", until, err)
	}

	// An ended single-service purchase is silenced while the bundle still runs.
	if _, err := pool.Exec(ctx, `INSERT INTO security_service_purchases
 (installation_id,guild_id,server_id,player_id,service_id,price_points,duration_days,starts_at,ends_at,request_key,ledger_entry_id)
 SELECT installation_id,guild_id,server_id,player_id,'BASE_RAID_ALARM',1,1,NOW()-INTERVAL '2 days',NOW()-INTERVAL '1 day','sp-old-raid',ledger_entry_id
 FROM security_service_purchases WHERE id=$1`, bundle.Purchase.ID); err != nil {
		t.Fatal(err)
	}
	mine := func() []SecurityExpiry {
		t.Helper()
		due, err := sales.DueExpiries(ctx, 200)
		if err != nil {
			t.Fatal(err)
		}
		var out []SecurityExpiry
		for _, e := range due {
			if e.InstallationID == fx.InstallationID {
				out = append(out, e)
			}
		}
		return out
	}
	if due := mine(); len(due) != 0 {
		t.Fatalf("expiry DM while the bundle still covers it: %+v", due)
	}
	if _, err := pool.Exec(ctx, `UPDATE security_service_purchases SET starts_at=NOW()-INTERVAL '40 days',ends_at=NOW()-INTERVAL '10 days' WHERE id=$1`, bundle.Purchase.ID); err != nil {
		t.Fatal(err)
	}
	due := mine()
	if len(due) != 1 || due[0].ServiceID != ServiceSentinelPro {
		t.Fatalf("bundle expiry: %+v", due)
	}
	if err := sales.MarkExpiryNotified(ctx, due[0].PurchaseID); err != nil {
		t.Fatal(err)
	}
}
