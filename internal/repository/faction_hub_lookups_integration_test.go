//go:build integration

package repository

import (
	"context"
	"testing"
)

// Zone faction rules and the staff player directory read a player's faction from the Faction
// Hub (through their verified gamertag link), scoped to the installation.
func TestZoneAndDirectoryUseHubFactions(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	ctx := context.Background()
	member, outsider := seedPlayer("Hub Member"), seedPlayer("Outsider")
	faction := seedHubFaction(t, zones.pool, fx.GuildRowID, "ZHF", hubSeedMember{Player: member, Role: "LEADER"})

	if got, err := zones.PlayerFactionID(ctx, fx.InstallationID, member); err != nil || got == nil || *got != faction {
		t.Fatalf("member's hub faction: %v %v", got, err)
	}
	if got, err := zones.PlayerFactionID(ctx, fx.InstallationID, outsider); err != nil || got != nil {
		t.Fatalf("a player in no faction: %v %v", got, err)
	}
	if got, err := zones.PlayerFactionID(ctx, fx.InstallationID+100000, member); err != nil || got != nil {
		t.Fatalf("another installation sees no faction: %v %v", got, err)
	}
	if ok, err := zones.IsInstallationHubFaction(ctx, fx.InstallationID, faction); err != nil || !ok {
		t.Fatalf("own faction: %v %v", ok, err)
	}
	if ok, err := zones.IsInstallationHubFaction(ctx, fx.InstallationID+100000, faction); err != nil || ok {
		t.Fatalf("another installation's faction must not be accepted: %v %v", ok, err)
	}

	rows, err := NewLocationRepository(zones.pool).ListPlayerDirectory(ctx, fx.GuildRowID, fx.ServerRowID, PlayerDirectoryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range rows {
		switch row.PlayerID {
		case member:
			seen++
			if row.FactionID == nil || *row.FactionID != faction || row.FactionName == nil || *row.FactionName != "ZHF" {
				t.Fatalf("directory faction for the member: %+v", row)
			}
		case outsider:
			seen++
			if row.FactionID != nil {
				t.Fatalf("directory faction for an outsider: %+v", row)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("both players listed once: %d", seen)
	}
}
