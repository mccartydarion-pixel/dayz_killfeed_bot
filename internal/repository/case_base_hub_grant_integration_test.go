//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A base faction grant names a Faction Hub faction of the base's installation; its members are
// then exempt from the base's alerts, and the Base Boosting review counts the grant.
func TestBaseGrantNamesAHubFaction(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	owner, raider := seedPlayer("Base Owner"), seedPlayer("Ally")
	var raiderADM string
	if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, raider).Scan(&raiderADM); err != nil {
		t.Fatal(err)
	}
	bases := NewCaseBaseRegistrationRepository(pool)
	base, err := bases.CreateDraft(ctx, CaseBaseDraftInput{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID,
		OwnerPlayerID: owner, MapKey: "chernarusplus", Name: "Hub Grant Fort", CenterX: 500, CenterZ: 500, Radius: 40})
	if err != nil {
		t.Fatal(err)
	}
	alarm := NewBaseRaidAlarmRepository(pool)
	if _, err := alarm.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	raids := func() int {
		t.Helper()
		m, err := alarm.MatchRaid(ctx, fx.GuildRowID, fx.ServerRowID, raiderADM, 500, 500)
		if err != nil {
			t.Fatal(err)
		}
		return len(m)
	}
	if raids() != 1 {
		t.Fatal("an outsider at the base sets the alarm off")
	}

	faction := seedHubFaction(t, pool, fx.GuildRowID, "GRT", hubSeedMember{Player: raider, Role: "LEADER"})
	grant, err := bases.AddDraftGrant(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, base.ID, nil, &faction, nil)
	if err != nil || grant.FactionID == nil || *grant.FactionID != faction || grant.LegacyFactionID != nil {
		t.Fatalf("hub faction grant: %+v %v", grant, err)
	}
	if raids() != 0 {
		t.Fatal("a member of the granted hub faction must not set the alarm off")
	}
	if _, err := bases.AddDraftGrant(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, base.ID, nil, &faction, nil); err == nil {
		t.Fatal("an overlapping grant for the same hub faction was accepted")
	}
	list, err := bases.ListAuthorizations(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, base.ID)
	if err != nil || len(list) != 1 || list[0].FactionID == nil || *list[0].FactionID != faction {
		t.Fatalf("grant list: %+v %v", list, err)
	}
	shadow, err := bases.ListBaseShadowBases(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID)
	if err != nil || len(shadow) != 1 || len(shadow[0].AuthorizedFactions) != 1 || shadow[0].AuthorizedFactions[0] != faction {
		t.Fatalf("Base Boosting review sees the hub grant: %+v %v", shadow, err)
	}

	// Another installation's faction cannot be granted here.
	other := newSaaSFixture(t, saasIntegrationDB(t))
	stranger := seedHubFaction(t, pool, other.GuildRowID, "OTH", hubSeedMember{Player: seedPlayerIn(t, pool, other.GuildRowID, "Stranger"), Role: "LEADER"})
	if _, err := bases.AddDraftGrant(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, base.ID, nil, &stranger, nil); err != ErrCaseBaseNotFound {
		t.Fatalf("another installation's faction: %v", err)
	}
}

func seedPlayerIn(t *testing.T, pool *pgxpool.Pool, guildRowID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		guildRowID, fmt.Sprintf("seed-%d-%d", guildRowID, time.Now().UnixNano()), name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
