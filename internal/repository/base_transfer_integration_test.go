//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBaseTransferLifecycle(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	s := BaseRequestScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	repo := NewBaseTransferRepository(pool)
	giver, mate, unlinked, outsider, full := seedPlayer("Giver"), seedPlayer("Mate"), seedPlayer("Unlinked"), seedPlayer("Outsider"), seedPlayer("Full")
	for i, p := range []int64{giver, mate, outsider, full} {
		if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`,
			fx.GuildRowID, p, "xfer-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'Movers','MOV',$2) RETURNING id`, fx.GuildRowID, giver).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{giver, mate, unlinked, full} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`, fx.GuildRowID, faction, p); err != nil {
			t.Fatal(err)
		}
	}
	addBase := func(owner int64, name string, x float64) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus',$5,$6,100,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner, name, x).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	hall := addBase(giver, "Hall", 100)
	for i := 0; i < BaseRequestMaxPerPlayer; i++ {
		addBase(full, "Full base", 2000+float64(i)*500)
	}
	bases, mates, err := repo.Options(ctx, s, giver)
	if err != nil || len(bases) != 1 || bases[0].BaseID != hall || len(mates) != 2 {
		t.Fatalf("options (own base; linked mates only): %+v %+v %v", bases, mates, err)
	}
	for _, bad := range []struct {
		from, base, to int64
		want           error
	}{
		{giver, hall, giver, ErrInvalidBaseTransfer},
		{giver, hall, unlinked, ErrBaseTransferNotMate},
		{giver, hall, outsider, ErrBaseTransferNotMate},
		{mate, hall, giver, ErrBaseTransferNotMate},
		{giver, hall, full, ErrBaseTransferLimit},
	} {
		if _, err := repo.Create(ctx, s, bad.from, bad.base, bad.to); !errors.Is(err, bad.want) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	tr, err := repo.Create(ctx, s, giver, hall, mate)
	if err != nil || tr.Status != BaseTransferPending || tr.BaseName != "Hall" || tr.ToName != "Mate" {
		t.Fatalf("create: %+v %v", tr, err)
	}
	if _, err := repo.Create(ctx, s, giver, hall, mate); !errors.Is(err, ErrBaseTransferWaiting) {
		t.Fatalf("second waiting transfer: %v", err)
	}
	if mine, err := repo.Mine(ctx, s, mate, 10); err != nil || len(mine) != 1 {
		t.Fatalf("receiver sees it: %+v %v", mine, err)
	}
	// Cancel and ask again.
	if err := repo.Cancel(ctx, s, mate, tr.ID); !errors.Is(err, ErrBaseTransferNotFound) {
		t.Fatalf("only the giver cancels: %v", err)
	}
	if err := repo.Cancel(ctx, s, giver, tr.ID); err != nil {
		t.Fatal(err)
	}
	tr, err = repo.Create(ctx, s, giver, hall, mate)
	if err != nil {
		t.Fatal(err)
	}
	// Decline.
	d, err := repo.Decline(ctx, s, tr.ID, "Not yet", nil)
	if err != nil || d.Transfer.Status != BaseTransferDeclined || d.Transfer.DeclineReason != "Not yet" || d.FromDiscord == "" || d.ToDiscord == "" {
		t.Fatalf("decline: %+v %v", d, err)
	}
	if _, err := repo.Approve(ctx, s, tr.ID, nil); !errors.Is(err, ErrBaseTransferDecided) {
		t.Fatalf("approve after decline: %v", err)
	}
	// The mate leaves the faction before approval: refused.
	tr, err = repo.Create(ctx, s, giver, hall, mate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE faction_members SET active=FALSE WHERE player_id=$1`, mate); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Approve(ctx, s, tr.ID, nil); !errors.Is(err, ErrBaseTransferNotMate) {
		t.Fatalf("approve after leaving: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE faction_members SET active=TRUE WHERE player_id=$1`, mate); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	d, err = repo.Approve(ctx, s, tr.ID, nil)
	if err != nil || d.Transfer.Status != BaseTransferApproved || d.Transfer.DecidedAt == nil || d.Transfer.DecidedAt.Before(before) {
		t.Fatalf("approve: %+v %v", d, err)
	}
	var owner int64
	if err := pool.QueryRow(ctx, `SELECT owner_player_id FROM case_registered_bases WHERE id=$1`, hall).Scan(&owner); err != nil || owner != mate {
		t.Fatalf("base changed owner: %d %v", owner, err)
	}
	if list, err := repo.ForOwner(ctx, s, 10); err != nil || len(list) != 3 || list[0].ID != tr.ID {
		t.Fatalf("owner list (newest first, no waiting ones): %+v %v", list, err)
	}
}
