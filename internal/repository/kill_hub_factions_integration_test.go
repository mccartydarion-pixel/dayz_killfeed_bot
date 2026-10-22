//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Kills record the killer's and victim's Faction Hub factions on the kill's server, and a season's
// top faction is the Hub faction with the most kills, named as it was when the season ended.
func TestKillsAndSeasonsUseHubFactions(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	killer, mate, victim := seedPlayer("Killer"), seedPlayer("Mate"), seedPlayer("Victim")
	faction := seedHubFaction(t, pool, fx.GuildRowID, "KHF", hubSeedMember{Player: killer, Role: "LEADER"}, hubSeedMember{Player: mate})

	got, err := NewFactionHubMembershipRepository(pool).FactionsOnServer(ctx, fx.GuildRowID, fx.ServerRowID, []int64{killer, victim})
	if err != nil || len(got) != 1 || got[killer] != faction {
		t.Fatalf("hub factions on the server: %v %v", got, err)
	}
	if other, err := NewFactionHubMembershipRepository(pool).FactionsOnServer(ctx, fx.GuildRowID, fx.ServerRowID+100000, []int64{killer}); err != nil || len(other) != 0 {
		t.Fatalf("another server sees no faction: %v %v", other, err)
	}

	start := time.Now().Add(-time.Hour)
	season, err := NewSeasonRepository(pool).Start(ctx, fx.GuildRowID, "Hub Season", start)
	if err != nil {
		t.Fatal(err)
	}
	kills := NewKillRepository(pool)
	for i, k := range []int64{killer, killer, mate} {
		f, at := faction, start.Add(time.Duration(i+1)*time.Minute)
		if err := kills.InsertKill(ctx, KillRecord{GuildID: fx.GuildRowID, ServerID: fx.ServerRowID, SessionID: "s", Fingerprint: fmt.Sprintf("hubkill-%d-%d", i, time.Now().UnixNano()),
			KillerPlayerID: k, VictimPlayerID: victim, KillerFactionID: &f, SeasonID: &season.ID, WeaponRaw: "M4", WeaponDisplay: "M4", EventTime: &at}); err != nil {
			t.Fatal(err)
		}
	}
	var hub, legacy int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE killer_hub_faction_id=$2), COUNT(*) FILTER (WHERE killer_faction_id IS NOT NULL) FROM kills WHERE guild_id=$1`, fx.GuildRowID, faction).Scan(&hub, &legacy); err != nil || hub != 3 || legacy != 0 {
		t.Fatalf("kills store the hub faction only: hub=%d legacy=%d %v", hub, legacy, err)
	}

	result, err := NewSeasonRepository(pool).FinalizeSeason(ctx, fx.GuildRowID, season.ID, time.Now())
	if err != nil || result.TopFactionLabel != "[KHF] KHF" || result.TopFactionKills != 3 || result.TopFactionID != 0 {
		t.Fatalf("season top faction: %+v %v", result, err)
	}
	// Dissolving the faction later keeps the season's label.
	if _, err := pool.Exec(ctx, `DELETE FROM hub_factions WHERE id=$1`, faction); err != nil {
		t.Fatal(err)
	}
	again, err := NewSeasonRepository(pool).GetSeasonResults(ctx, fx.GuildRowID, season.ID)
	if err != nil || again == nil || again.TopFactionLabel != "[KHF] KHF" {
		t.Fatalf("season label after dissolve: %+v %v", again, err)
	}
}
