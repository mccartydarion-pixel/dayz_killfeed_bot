//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBaseRentFreeAndClock(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	rent := NewBaseRentRepository(pool)
	s := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	owner := seedPlayer("Freebie")
	reqs := NewCaseBaseRequestRepository(pool)
	req, err := reqs.Create(ctx, BaseRequestScope(s), BaseRequestInput{PlayerID: owner, Name: "Event Base", CenterX: 10, CenterZ: 10, Radius: 50, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := reqs.Approve(ctx, BaseRequestScope(s), req.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *approved.Request.BaseID
	var ownerMade int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Staff',9000,9000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&ownerMade); err != nil {
		t.Fatal(err)
	}
	if _, err := rent.SetSettings(ctx, s, true, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
	// Long overdue: paused.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_settings SET enabled_since=NOW()-INTERVAL '10 days' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_registered_bases SET created_at=NOW()-INTERVAL '20 days' WHERE id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	if bases, _ := rent.PlayerBases(ctx, s, owner); len(bases) != 1 || !bases[0].Paused {
		t.Fatalf("paused before: %+v", bases)
	}
	if err := rent.SetRentFree(ctx, s, ownerMade, true, "", nil); !errors.Is(err, ErrBaseRentNotOwned) {
		t.Fatalf("owner-made base can't be made rent-free (it already is): %v", err)
	}
	if err := rent.SetRentFree(ctx, s, baseID, true, string(make([]rune, 201)), nil); !errors.Is(err, ErrInvalidBaseRent) {
		t.Fatalf("long note: %v", err)
	}
	if err := rent.SetRentFree(ctx, s, baseID, true, "Event winners", nil); err != nil {
		t.Fatal(err)
	}
	if bases, _ := rent.PlayerBases(ctx, s, owner); len(bases) != 0 {
		t.Fatalf("rent-free base pays nothing: %+v", bases)
	}
	var paused bool
	if err := pool.QueryRow(ctx, `SELECT base_rent_paused($1)`, baseID).Scan(&paused); err != nil || paused {
		t.Fatalf("rent-free base is never paused: %v %v", paused, err)
	}
	if _, _, _, err := rent.Quote(ctx, s, owner, baseID); !errors.Is(err, ErrBaseRentNotOwned) {
		t.Fatalf("no rent to pay on a rent-free base: %v", err)
	}
	free, err := rent.RentFreeBases(ctx, s)
	if err != nil || len(free) != 1 || free[0].BaseID != baseID || free[0].Note != "Event winners" || free[0].OwnerName != "Freebie" {
		t.Fatalf("rent-free list: %+v %v", free, err)
	}
	// Charged again: the clock restarts now, so it's due now (in grace), not paused.
	before := time.Now().Add(-time.Second)
	if err := rent.SetRentFree(ctx, s, baseID, false, "", nil); err != nil {
		t.Fatal(err)
	}
	bases, _ := rent.PlayerBases(ctx, s, owner)
	if len(bases) != 1 || bases[0].Paused || bases[0].DueAt.Before(before) {
		t.Fatalf("resumed rent restarts the clock: %+v", bases)
	}
	if free, _ := rent.RentFreeBases(ctx, s); len(free) != 0 {
		t.Fatalf("no longer rent-free: %+v", free)
	}
	// Switching rent off and on with an old payment doesn't leave it overdue straight away.
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: s.GuildID, PlayerID: owner, Type: TxAdminCredit, Amount: 500, ReferenceID: "rent-free-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rent.Pay(ctx, s, owner, baseID, "rent-free-pay-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE base_rent_payments SET starts_at=NOW()-INTERVAL '30 days',ends_at=NOW()-INTERVAL '23 days' WHERE base_id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE base_rent_exemptions SET changed_at=NOW()-INTERVAL '40 days' WHERE base_id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	if bases, _ := rent.PlayerBases(ctx, s, owner); len(bases) != 1 || !bases[0].Paused {
		t.Fatalf("old payment, rent on all along: paused: %+v", bases)
	}
	if _, err := rent.SetSettings(ctx, s, false, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rent.SetSettings(ctx, s, true, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
	if bases, _ := rent.PlayerBases(ctx, s, owner); len(bases) != 1 || bases[0].Paused || bases[0].DueAt.Before(before) {
		t.Fatalf("switching rent back on restarts the clock: %+v", bases)
	}
	if _, err := rent.SetSettings(ctx, s, false, 100, 7, nil); err != nil {
		t.Fatal(err)
	}
}
