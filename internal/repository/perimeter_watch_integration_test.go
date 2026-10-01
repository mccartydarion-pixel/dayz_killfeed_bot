//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestPerimeterWatchMatchCooldownAndSale(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	repo := NewPerimeterWatchRepository(pool)
	owner, friend, mate, stranger, other := seedPlayer("Owner"), seedPlayer("Friend"), seedPlayer("Mate"), seedPlayer("Stranger"), seedPlayer("Other")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,'pw-owner','VERIFIED',NOW())`, fx.GuildRowID, owner); err != nil {
		t.Fatal(err)
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'PW','PW',$2) RETURNING id`, fx.GuildRowID, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{owner, mate} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`, fx.GuildRowID, faction, p); err != nil {
			t.Fatal(err)
		}
	}
	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','North Base',1000,2000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_base_authorizations(installation_id,guild_id,server_id,base_id,player_id,valid_from) VALUES($1,$2,$3,$4,$5,$6)`,
		fx.InstallationID, fx.GuildRowID, fx.ServerRowID, baseID, friend, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	match := func(player int64, x float64) []PerimeterMatch {
		t.Helper()
		m, err := repo.MatchPerimeter(ctx, fx.GuildRowID, fx.ServerRowID, player, x, 2000)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if len(match(stranger, 1120)) != 0 {
		t.Fatal("matched while Perimeter Watch is off")
	}
	if s, err := repo.GetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID); err != nil || s.Enabled || s.MarginMeters != 100 {
		t.Fatalf("default settings: %+v %v", s, err)
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 10, 1800, nil); err == nil {
		t.Fatal("margin below minimum accepted")
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 100, 1800, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	// Base radius 50 + margin 100 = 150 m watched.
	for name, p := range map[string]int64{"owner": owner, "friend": friend, "faction mate": mate} {
		if len(match(p, 1120)) != 0 {
			t.Fatalf("%s triggered Perimeter Watch", name)
		}
	}
	if len(match(stranger, 1160)) != 0 {
		t.Fatal("outside the watched area matched")
	}
	got := match(stranger, 1120)
	if len(got) != 1 || got[0].BaseID != baseID || got[0].DistanceMeters != 120 || got[0].OwnerDiscordUserID != "pw-owner" || got[0].CooldownSeconds != 1800 {
		t.Fatalf("stranger match: %+v", got)
	}
	first, err := repo.RecordAlert(ctx, got[0], stranger, "Stranger")
	if err != nil || first == 0 {
		t.Fatalf("first alert: %d %v", first, err)
	}
	if again, err := repo.RecordAlert(ctx, got[0], stranger, "Stranger"); err != nil || again != 0 {
		t.Fatalf("same visitor alerted twice: %d %v", again, err)
	}
	if burst, err := repo.RecordAlert(ctx, got[0], other, "Other"); err != nil || burst != 0 {
		t.Fatalf("base alerted twice inside two minutes: %d %v", burst, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE perimeter_watch_alerts SET created_at=NOW()-INTERVAL '3 minutes' WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if later, err := repo.RecordAlert(ctx, got[0], other, "Other"); err != nil || later == 0 {
		t.Fatalf("a different visitor after two minutes should alert: %d %v", later, err)
	}
	if err := repo.MarkDelivery(ctx, first, BaseRaidDeliverySent); err != nil {
		t.Fatal(err)
	}
	if recent, err := repo.RecentAlerts(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, 10); err != nil || len(recent) != 2 || recent[1].Delivery != BaseRaidDeliverySent || recent[1].BaseName != "North Base" {
		t.Fatalf("recent: %+v %v", recent, err)
	}

	// While selling, only owners with paid Perimeter Watch time get alerts.
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	if _, err := sales.SetOffer(ctx, scope, ServicePerimeterWatch, true, 300, 7, nil); err != nil {
		t.Fatal(err)
	}
	if len(match(stranger, 1120)) != 0 {
		t.Fatal("unpaid owner got Perimeter Watch while it is on sale")
	}
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: owner, Type: TxAdminCredit, Amount: 500, ReferenceID: "pw-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	p, err := sales.Purchase(ctx, scope, owner, ServicePerimeterWatch, "pw-request-1")
	if err != nil || p.Purchase.ServiceID != ServicePerimeterWatch || p.BalanceAfter != 200 {
		t.Fatalf("perimeter purchase: %+v %v", p, err)
	}
	if len(match(stranger, 1120)) != 1 {
		t.Fatal("paying owner did not get Perimeter Watch")
	}
	// A Base Raid Alarm purchase is a different service.
	if until, err := sales.ActiveUntil(ctx, fx.InstallationID, owner, ServiceBaseRaidAlarm); err != nil || until != nil {
		t.Fatalf("perimeter purchase counted as raid alarm time: %v %v", until, err)
	}
}
