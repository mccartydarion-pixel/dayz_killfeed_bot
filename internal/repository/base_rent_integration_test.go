//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBaseRentLifecycle(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	rent := NewBaseRentRepository(pool)
	s := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	renter, other, stranger := seedPlayer("Renter"), seedPlayer("Other"), seedPlayer("Stranger")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,'rent-renter','VERIFIED',NOW())`, fx.GuildRowID, renter); err != nil {
		t.Fatal(err)
	}
	// A player-requested (rented) base and an owner-made (free) base.
	req, err := NewCaseBaseRequestRepository(pool).Create(ctx, BaseRequestScope(s), BaseRequestInput{PlayerID: renter, Name: "Rented", CenterX: 1000, CenterZ: 2000, Radius: 50, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := NewCaseBaseRequestRepository(pool).Approve(ctx, BaseRequestScope(s), req.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	rentedID := *approved.Request.BaseID
	var freeID int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Free',5000,2000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, renter).Scan(&freeID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPerimeterWatchRepository(pool).SetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID, true, 100, 1800, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBaseRaidAlarmRepository(pool).SetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID, true, 600, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBaseBlackBoxRepository(pool).SetSettings(ctx, s.InstallationID, s.GuildID, s.ServerID, true, 14, nil); err != nil {
		t.Fatal(err)
	}
	var strangerADM string
	if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, stranger).Scan(&strangerADM); err != nil {
		t.Fatal(err)
	}
	served := func(x float64) (perimeter, raid, blackBox int) {
		t.Helper()
		p, err := NewPerimeterWatchRepository(pool).MatchPerimeter(ctx, s.GuildID, s.ServerID, stranger, x, 2000)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewBaseRaidAlarmRepository(pool).MatchRaid(ctx, s.GuildID, s.ServerID, strangerADM, x, 2000)
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewBaseBlackBoxRepository(pool).MatchVisit(ctx, s.GuildID, s.ServerID, stranger, x, 2000)
		if err != nil {
			t.Fatal(err)
		}
		return len(p), len(r), len(b)
	}
	if p, r, b := served(1000); p != 1 || r != 1 || b != 1 {
		t.Fatalf("rent off: services must run: %d %d %d", p, r, b)
	}
	if bases, err := rent.PlayerBases(ctx, s, renter); err != nil || len(bases) != 0 {
		t.Fatalf("rent off: nothing to pay: %+v %v", bases, err)
	}
	if _, err := rent.Pay(ctx, s, renter, rentedID, "rent-key-0001"); !errors.Is(err, ErrBaseRentOff) {
		t.Fatalf("paid while off: %v", err)
	}
	// Rent on: the clock starts now; nothing is paused yet.
	if _, err := rent.SetSettings(ctx, s, true, 0, 7, nil); err == nil {
		t.Fatal("zero rent accepted")
	}
	if _, err := rent.SetSettings(ctx, s, true, 400, 31, nil); err == nil {
		t.Fatal("31-day period accepted")
	}
	st, err := rent.SetSettings(ctx, s, true, 400, 7, nil)
	if err != nil || !st.Enabled || st.EnabledSince == nil {
		t.Fatalf("enable: %+v %v", st, err)
	}
	bases, err := rent.PlayerBases(ctx, s, renter)
	if err != nil || len(bases) != 1 || bases[0].BaseID != rentedID || bases[0].Paused || !bases[0].Overdue {
		t.Fatalf("renter bases (only the requested base, due now, in grace): %+v %v", bases, err)
	}
	if p, r, b := served(1000); p != 1 || r != 1 || b != 1 {
		t.Fatalf("in grace: services must run: %d %d %d", p, r, b)
	}
	// Past the 3-day grace: paused. The owner-made base keeps working.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_settings SET enabled_since=NOW()-INTERVAL '4 days' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_registered_bases SET created_at=NOW()-INTERVAL '5 days' WHERE id=$1`, rentedID); err != nil {
		t.Fatal(err)
	}
	if p, r, b := served(1000); p+r+b != 0 {
		t.Fatalf("paused base still served: %d %d %d", p, r, b)
	}
	if p, r, b := served(5000); p != 1 || r != 1 || b != 1 {
		t.Fatalf("owner-made base must stay free: %d %d %d", p, r, b)
	}
	all, err := rent.AllBases(ctx, s, 10)
	if err != nil || len(all) != 1 || !all[0].Paused || all[0].OwnerName != "Renter" {
		t.Fatalf("owner view: %+v %v", all, err)
	}
	notices, err := rent.DueNotices(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var paused *RentNotice
	for i := range notices {
		if notices[i].BaseID == rentedID && notices[i].Kind == BaseRentNoticePaused {
			paused = &notices[i]
		}
	}
	if paused == nil || paused.DiscordUserID != "rent-renter" || paused.PricePoints != 400 {
		t.Fatalf("paused notice: %+v", notices)
	}
	if err := rent.MarkNotice(ctx, *paused); err != nil {
		t.Fatal(err)
	}
	if again, _ := rent.DueNotices(ctx, 50); containsNotice(again, rentedID, BaseRentNoticePaused) {
		t.Fatal("paused notice sent twice")
	}

	// Paying: the owner's player only, only rented bases, and enough points.
	if _, err := rent.Pay(ctx, s, renter, rentedID, "rent-key-0002"); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("paid without points: %v", err)
	}
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: s.GuildID, PlayerID: renter, Type: TxAdminCredit, Amount: 1000, ReferenceID: "rent-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rent.Pay(ctx, s, other, rentedID, "rent-key-0003"); !errors.Is(err, ErrBaseRentNotOwned) {
		t.Fatalf("someone else paid: %v", err)
	}
	if _, err := rent.Pay(ctx, s, renter, freeID, "rent-key-0004"); !errors.Is(err, ErrBaseRentNotOwned) {
		t.Fatalf("paid rent on a free base: %v", err)
	}
	paid, err := rent.Pay(ctx, s, renter, rentedID, "rent-key-0005")
	if err != nil || paid.Duplicate || paid.BalanceAfter != 600 || paid.Payment.PeriodDays != 7 {
		t.Fatalf("pay: %+v %v", paid, err)
	}
	if p, r, b := served(1000); p != 1 || r != 1 || b != 1 {
		t.Fatalf("paid base must be served again: %d %d %d", p, r, b)
	}
	if replay, err := rent.Pay(ctx, s, renter, rentedID, "rent-key-0005"); err != nil || !replay.Duplicate || replay.Payment.ID != paid.Payment.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	next, err := rent.Pay(ctx, s, renter, rentedID, "rent-key-0006")
	if err != nil || !next.Payment.StartsAt.Equal(paid.Payment.EndsAt) || next.BalanceAfter != 200 {
		t.Fatalf("payments must stack: %+v %v", next, err)
	}
	var ledgerType string
	if err := pool.QueryRow(ctx, `SELECT reason_type FROM point_transactions WHERE id=(SELECT ledger_entry_id FROM base_rent_payments WHERE id=$1)`, paid.Payment.ID).Scan(&ledgerType); err != nil || ledgerType != TxBaseRent {
		t.Fatalf("ledger row: %q %v", ledgerType, err)
	}
	if pays, err := rent.RecentPayments(ctx, s, 10); err != nil || len(pays) != 2 || pays[0].BaseName != "Rented" {
		t.Fatalf("recent payments: %+v %v", pays, err)
	}
	// Due soon: a reminder a day before.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_payments SET starts_at=NOW()-INTERVAL '6 days',ends_at=NOW()+INTERVAL '12 hours' WHERE base_id=$1 AND id=$2`, rentedID, next.Payment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE base_rent_payments SET starts_at=NOW()-INTERVAL '13 days',ends_at=NOW()-INTERVAL '6 days' WHERE id=$1`, paid.Payment.ID); err != nil {
		t.Fatal(err)
	}
	if soon, _ := rent.DueNotices(ctx, 50); !containsNotice(soon, rentedID, BaseRentNoticeDueSoon) {
		t.Fatalf("due-soon notice: %+v", soon)
	}
	// Turning rent off stops it all; turning it back on restarts the clock.
	if _, err := rent.SetSettings(ctx, s, false, 400, 7, nil); err != nil {
		t.Fatal(err)
	}
	if bases, _ := rent.PlayerBases(ctx, s, renter); len(bases) != 0 {
		t.Fatalf("rent off: %+v", bases)
	}
	before := time.Now()
	if st, err := rent.SetSettings(ctx, s, true, 400, 7, nil); err != nil || st.EnabledSince.Before(before.Add(-time.Second)) {
		t.Fatalf("re-enable must restart the clock: %+v %v", st, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO base_rent_notices(base_id,kind,due_at) SELECT $1,'DUE_SOON',base_rent_due_at($1) ON CONFLICT DO NOTHING`, rentedID); err != nil {
		t.Fatal(err)
	}
	if _, err := rent.SetSettings(ctx, s, false, 400, 7, nil); err != nil {
		t.Fatal(err)
	}
}

func containsNotice(ns []RentNotice, baseID int64, kind string) bool {
	for _, n := range ns {
		if n.BaseID == baseID && n.Kind == kind {
			return true
		}
	}
	return false
}
