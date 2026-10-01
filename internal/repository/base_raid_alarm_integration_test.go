//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestBaseRaidAlarmMatchAndCooldown(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	repo := NewBaseRaidAlarmRepository(pool)
	ctx := context.Background()
	admID := func(playerID int64) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, playerID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner, friend, factionFriend, ownerMate, stranger := seedPlayer("Owner"), seedPlayer("Friend"),
		seedPlayer("FactionFriend"), seedPlayer("OwnerMate"), seedPlayer("Stranger")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at)
 VALUES($1,$2,'raid-owner-discord','VERIFIED',NOW())`, fx.GuildRowID, owner); err != nil {
		t.Fatal(err)
	}
	faction := func(name string, leader int64, members ...int64) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,$2,$2,$3) RETURNING id`,
			fx.GuildRowID, name, leader).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, m := range append([]int64{leader}, members...) {
			if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`,
				fx.GuildRowID, id, m); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	faction("OWNR", owner, ownerMate)
	allies := faction("ALLY", factionFriend)

	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','North Base',1000,2000,50) RETURNING id`,
		fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	from := time.Now().Add(-time.Minute)
	for _, grant := range []struct{ player, faction *int64 }{{player: &friend}, {faction: &allies}} {
		if _, err := pool.Exec(ctx, `INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,faction_id,valid_from) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			fx.InstallationID, fx.GuildRowID, fx.ServerRowID, baseID, grant.player, grant.faction, from); err != nil {
			t.Fatal(err)
		}
	}

	match := func(player int64, x, z float64) []BaseRaidMatch {
		t.Helper()
		m, err := repo.MatchRaid(ctx, fx.GuildRowID, fx.ServerRowID, admID(player), x, z)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if got := match(stranger, 1010, 2010); len(got) != 0 {
		t.Fatalf("alarm off by default, got %+v", got)
	}
	settings, err := repo.GetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID)
	if err != nil || settings.Enabled || settings.CooldownSeconds != BaseRaidDefaultCooldownSeconds {
		t.Fatalf("default settings: %+v %v", settings, err)
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 30, nil); err == nil {
		t.Fatal("cooldown below minimum accepted")
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 600, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}

	for name, player := range map[string]int64{"owner": owner, "friend grant": friend, "faction grant": factionFriend, "owner's faction": ownerMate} {
		if got := match(player, 1010, 2010); len(got) != 0 {
			t.Fatalf("%s raised an alarm: %+v", name, got)
		}
	}
	if got := match(stranger, 1100, 2000); len(got) != 0 {
		t.Fatalf("outside the base circle raised an alarm: %+v", got)
	}
	got := match(stranger, 1030, 2030)
	if len(got) != 1 || got[0].BaseID != baseID || got[0].OwnerDiscordUserID != "raid-owner-discord" ||
		got[0].RaiderPlayerID == nil || *got[0].RaiderPlayerID != stranger || got[0].CooldownSeconds != 600 {
		t.Fatalf("stranger match: %+v", got)
	}
	unknown, err := repo.MatchRaid(ctx, fx.GuildRowID, fx.ServerRowID, "never-seen-player", 1000, 2000)
	if err != nil || len(unknown) != 1 || unknown[0].RaiderPlayerID != nil {
		t.Fatalf("unknown player should alarm with no player id: %+v %v", unknown, err)
	}

	ev := BaseRaidEvent{RaiderName: "Stranger", Part: "Wall", Target: "Fence", Tool: "Hatchet", TimeOfDay: "14:32:10"}
	first, err := repo.RecordAlert(ctx, got[0], ev)
	if err != nil || first == 0 {
		t.Fatalf("first alert: %d %v", first, err)
	}
	if again, err := repo.RecordAlert(ctx, got[0], ev); err != nil || again != 0 {
		t.Fatalf("cooldown not applied: %d %v", again, err)
	}
	if err := repo.MarkDelivery(ctx, first, BaseRaidDeliverySent); err != nil {
		t.Fatal(err)
	}
	recent, err := repo.RecentAlerts(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, 10)
	if err != nil || len(recent) != 1 || recent[0].Delivery != BaseRaidDeliverySent || recent[0].BaseName != "North Base" || recent[0].Part != "Wall" {
		t.Fatalf("recent alerts: %+v %v", recent, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE case_registered_bases SET state='REVOKED',revoked_at=NOW() WHERE id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	if got := match(stranger, 1030, 2030); len(got) != 0 {
		t.Fatalf("withdrawn base raised an alarm: %+v", got)
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID+999999, fx.ServerRowID, true, 600, nil); err == nil {
		t.Fatal("settings accepted a mismatched guild")
	}
}
