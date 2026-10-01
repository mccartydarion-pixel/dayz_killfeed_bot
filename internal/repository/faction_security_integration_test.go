//go:build integration

package repository

import (
	"context"
	"sort"
	"testing"
)

func TestFactionSecurityRecipientsPreferenceAndSale(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	repo := NewFactionSecurityRepository(pool)
	owner, officer, member, unlinked, outsider, gone := seedPlayer("Owner"), seedPlayer("Officer"), seedPlayer("Member"), seedPlayer("Unlinked"), seedPlayer("Outsider"), seedPlayer("Gone")
	for p, discordID := range map[int64]string{owner: "fs-owner", officer: "fs-officer", member: "fs-member", outsider: "fs-outsider", gone: "fs-gone"} {
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, fx.GuildRowID, p, discordID); err != nil {
			t.Fatal(err)
		}
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'Wolves','WLF',$2) RETURNING id`, fx.GuildRowID, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for p, role := range map[int64]string{owner: "OWNER", officer: "OFFICER", member: "MEMBER", unlinked: "MEMBER"} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,$4)`, fx.GuildRowID, faction, p, role); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role,active) VALUES($1,$2,$3,'MEMBER',FALSE)`, fx.GuildRowID, faction, gone); err != nil {
		t.Fatal(err)
	}
	recipients := func() []string {
		t.Helper()
		ids, err := repo.Recipients(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(ids)
		return ids
	}

	if s, err := repo.GetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID); err != nil || s.Enabled {
		t.Fatalf("default settings: %+v %v", s, err)
	}
	if ids := recipients(); len(ids) != 0 {
		t.Fatalf("shared while off: %v", ids)
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	// Everyone: linked, active faction mates only; never the owner, a former member or an outsider.
	if ids := recipients(); len(ids) != 2 || ids[0] != "fs-member" || ids[1] != "fs-officer" {
		t.Fatalf("everyone: %v", ids)
	}
	if who, err := repo.GetPreference(ctx, fx.InstallationID, owner); err != nil || who != FactionRecipientsAll {
		t.Fatalf("default preference: %q %v", who, err)
	}
	if err := repo.SetPreference(ctx, fx.InstallationID, owner, "SOME"); err == nil {
		t.Fatal("invalid preference accepted")
	}
	if err := repo.SetPreference(ctx, fx.InstallationID, owner, FactionRecipientsLeaders); err != nil {
		t.Fatal(err)
	}
	if ids := recipients(); len(ids) != 1 || ids[0] != "fs-officer" {
		t.Fatalf("leaders only: %v", ids)
	}
	sum, err := repo.Summary(ctx, fx.GuildRowID, owner)
	if err != nil || !sum.InFaction || sum.FactionName != "Wolves" || sum.LinkedMembers != 2 || sum.LinkedLeaders != 1 {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	if sum, err := repo.Summary(ctx, fx.GuildRowID, outsider); err != nil || sum.InFaction {
		t.Fatalf("outsider summary: %+v %v", sum, err)
	}
	if ids, err := repo.Recipients(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, outsider); err != nil || len(ids) != 0 {
		t.Fatalf("owner without a faction shared: %v %v", ids, err)
	}

	// While selling, only owners with paid time share.
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	if _, err := sales.SetOffer(ctx, scope, ServiceFactionSecurity, true, 250, 7, nil); err != nil {
		t.Fatal(err)
	}
	if ids := recipients(); len(ids) != 0 {
		t.Fatalf("unpaid owner shared while on sale: %v", ids)
	}
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: owner, Type: TxAdminCredit, Amount: 300, ReferenceID: "fs-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if p, err := sales.Purchase(ctx, scope, owner, ServiceFactionSecurity, "fs-request-1"); err != nil || p.BalanceAfter != 50 {
		t.Fatalf("faction security purchase: %+v %v", p, err)
	}
	if ids := recipients(); len(ids) != 1 {
		t.Fatalf("paying owner did not share: %v", ids)
	}

	// Share log.
	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Den',1000,2000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	if err := repo.LogShare(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, baseID, "OTHER", 1, 0); err == nil {
		t.Fatal("unknown source accepted")
	}
	if err := repo.LogShare(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, baseID, FactionShareRaidAlarm, 2, 1); err != nil {
		t.Fatal(err)
	}
	if recent, err := repo.RecentShares(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, 10); err != nil || len(recent) != 1 || recent[0].BaseName != "Den" || recent[0].Sent != 2 || recent[0].Failed != 1 {
		t.Fatalf("recent shares: %+v %v", recent, err)
	}
}
