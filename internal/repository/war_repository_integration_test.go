//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestHubFactionWarLifecycle(t *testing.T) {
	db := saasIntegrationDB(t)
	home, other := newSaaSFixture(t, db), newSaaSFixture(t, db)
	ctx := context.Background()
	pa, pb := seedPlayerIn(t, db.Pool, home.GuildRowID, "Red Boss"), seedPlayerIn(t, db.Pool, home.GuildRowID, "Blue Boss")
	px := seedPlayerIn(t, db.Pool, other.GuildRowID, "Xen Boss")
	red := seedHubFaction(t, db.Pool, home.GuildRowID, "RED", hubSeedMember{Player: pa, Role: "LEADER"})
	blu := seedHubFaction(t, db.Pool, home.GuildRowID, "BLU", hubSeedMember{Player: pb, Role: "LEADER"})
	xen := seedHubFaction(t, db.Pool, other.GuildRowID, "XEN", hubSeedMember{Player: px, Role: "LEADER"})
	wars := NewPostgresWarRepository(db.Pool)
	hub := NewFactionHubMembershipRepository(db.Pool)

	if _, err := wars.Challenge(ctx, home.InstallationID, 0, red, xen, 0); !errors.Is(err, ErrWarFactionNotInGuild) {
		t.Fatalf("a war against another server's faction: %v", err)
	}
	if _, err := wars.Challenge(ctx, other.InstallationID, 0, red, blu, 0); !errors.Is(err, ErrWarFactionNotInGuild) {
		t.Fatalf("a war between factions of another installation: %v", err)
	}
	// The challenger is side A whatever the ids' order.
	w, err := wars.Challenge(ctx, home.InstallationID, 0, blu, red, pb)
	if err != nil || w.GuildID != home.GuildRowID || w.InstallationID != home.InstallationID || w.Status != WarPending ||
		w.FactionAID != blu || w.FactionBID != red || w.FactionALabel != "[BLU] BLU" || w.FactionBLabel != "[RED] RED" {
		t.Fatalf("challenge: %+v %v", w, err)
	}
	if _, err := wars.Challenge(ctx, home.InstallationID, 0, red, blu, 0); !errors.Is(err, ErrWarAlreadyOpen) {
		t.Fatalf("a second open war for the pair: %v", err)
	}
	if id, err := wars.ActiveWarOnServer(ctx, home.ServerRowID, red, blu); err != nil || id != 0 {
		t.Fatalf("a pending war takes no kills: %d %v", id, err)
	}
	if err := wars.EndWar(ctx, home.GuildRowID, w.ID); !errors.Is(err, ErrWarNotChanged) {
		t.Fatalf("ending a pending war: %v", err)
	}
	if err := wars.AcceptWar(ctx, other.GuildRowID, w.ID); !errors.Is(err, ErrWarNotChanged) {
		t.Fatalf("accepting from another guild: %v", err)
	}
	if err := wars.AcceptWar(ctx, home.GuildRowID, w.ID); err != nil {
		t.Fatal(err)
	}
	if id, err := wars.ActiveWarOnServer(ctx, home.ServerRowID, red, blu); err != nil || id != w.ID {
		t.Fatalf("active war on the server: %d %v", id, err)
	}
	if id, _ := wars.ActiveWarOnServer(ctx, other.ServerRowID, red, blu); id != 0 {
		t.Fatalf("the war is found on another server: %d", id)
	}

	// Blue scores two kills on Red, Red one on Blue.
	for n, k := range []struct{ killer, victim, kf, vf int64 }{{pb, pa, blu, red}, {pb, pa, blu, red}, {pa, pb, red, blu}} {
		if _, err := db.Pool.Exec(ctx, `INSERT INTO kills(guild_id,session_id,event_fingerprint,killer_player_id,victim_player_id,war_id,killer_hub_faction_id,victim_hub_faction_id,distance,created_at)
VALUES($1,'war',$2,$3,$4,$5,$6,$7,$8,$9)`, home.GuildRowID, fmt.Sprintf("war-%d-%d", w.ID, n), k.killer, k.victim, w.ID, k.kf, k.vf, 100+float64(n), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if a, b, err := wars.GetWarScore(ctx, home.GuildRowID, w.ID); err != nil || a != 2 || b != 1 {
		t.Fatalf("score = %d-%d %v", a, b, err)
	}
	if err := wars.EndWar(ctx, home.GuildRowID, w.ID); err != nil {
		t.Fatal(err)
	}
	ended, err := wars.GetWar(ctx, home.GuildRowID, w.ID)
	if err != nil || ended.Status != WarEnded || ended.ScoreA != 2 || ended.ScoreB != 1 || ended.WinnerSide != "A" || ended.WinnerLabel() != "[BLU] BLU" {
		t.Fatalf("ended war: %+v %v", ended, err)
	}
	if list, err := wars.GetEndedWars(ctx, home.GuildRowID, 5); err != nil || len(list) != 1 || list[0].ID != w.ID {
		t.Fatalf("ended wars: %+v %v", list, err)
	}

	rivalry, err := hub.Rivalry(ctx, home.GuildRowID, red, blu)
	if err != nil || rivalry.AKills != 1 || rivalry.BKills != 2 || rivalry.Wars != 1 || rivalry.TopPlayerID != pb || rivalry.TopKills != 2 || rivalry.LongestPlayerID != pa {
		t.Fatalf("rivalry: %+v %v", rivalry, err)
	}
	if refs, err := hub.MemberFactions(ctx, home.GuildRowID, pa); err != nil || len(refs) != 1 || refs[0].ID != red || refs[0].Role != "LEADER" || refs[0].InstallationID != home.InstallationID {
		t.Fatalf("member factions: %+v %v", refs, err)
	}
	if refs, err := hub.FactionsByTag(ctx, home.GuildRowID, " [red] "); err != nil || len(refs) != 1 || refs[0].ID != red || refs[0].Label() != "[RED] RED" {
		t.Fatalf("faction by tag: %+v %v", refs, err)
	}
	if refs, err := hub.FactionsByTag(ctx, home.GuildRowID, "XEN"); err != nil || len(refs) != 0 {
		t.Fatalf("another server's tag: %+v %v", refs, err)
	}
	if ins, err := hub.GuildInstallations(ctx, home.GuildRowID); err != nil || len(ins) != 1 || ins[0].ID != home.InstallationID || ins[0].OrganizationID != home.OrgID {
		t.Fatalf("installations: %+v %v", ins, err)
	}

	// A dissolved faction keeps its name in the war's history.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM hub_factions WHERE id=$1`, blu); err != nil {
		t.Fatal(err)
	}
	after, err := wars.GetWar(ctx, home.GuildRowID, w.ID)
	if err != nil || after.FactionAID != 0 || after.FactionALabel != "[BLU] BLU" || after.WinnerLabel() != "[BLU] BLU" {
		t.Fatalf("war after the winner was dissolved: %+v %v", after, err)
	}
}
