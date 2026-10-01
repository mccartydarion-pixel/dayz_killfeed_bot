//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBaseRentFactionPays(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	rent := NewBaseRentRepository(pool)
	s := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	owner, mate, outsider, former := seedPlayer("Owner"), seedPlayer("Mate"), seedPlayer("Outsider"), seedPlayer("Former")
	if _, err := pool.Exec(ctx, `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,'rent-owner-d','VERIFIED',NOW())`, fx.GuildRowID, owner); err != nil {
		t.Fatal(err)
	}
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'Renters','RNT',$2) RETURNING id`, fx.GuildRowID, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		id     int64
		active bool
	}{{owner, true}, {mate, true}, {former, false}} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role,active) VALUES($1,$2,$3,'MEMBER',$4)`, fx.GuildRowID, faction, m.id, m.active); err != nil {
			t.Fatal(err)
		}
	}
	reqs := NewCaseBaseRequestRepository(pool)
	req, err := reqs.Create(ctx, BaseRequestScope(s), BaseRequestInput{PlayerID: owner, Name: "Clan Hall", CenterX: 100, CenterZ: 100, Radius: 50, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := reqs.Approve(ctx, BaseRequestScope(s), req.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *approved.Request.BaseID
	if _, err := rent.SetSettings(ctx, s, true, 250, 7, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{mate, outsider, former} {
		if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: s.GuildID, PlayerID: p, Type: TxAdminCredit, Amount: 1000, ReferenceID: "rent-faction-seed-" + time.Now().Format("150405.000000") + string(rune('a'+p%26)), CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := rent.PlayerBases(ctx, s, mate)
	if err != nil || len(bases) != 1 || !bases[0].Faction || bases[0].OwnerName != "Owner" {
		t.Fatalf("mate sees the faction base: %+v %v", bases, err)
	}
	if own, err := rent.PlayerBases(ctx, s, owner); err != nil || len(own) != 1 || own[0].Faction {
		t.Fatalf("owner's own base isn't marked faction: %+v %v", own, err)
	}
	if out, err := rent.PlayerBases(ctx, s, outsider); err != nil || len(out) != 0 {
		t.Fatalf("outsider sees nothing: %+v %v", out, err)
	}
	for _, p := range []int64{outsider, former} {
		if _, err := rent.Pay(ctx, s, p, baseID, "rent-faction-bad"); !errors.Is(err, ErrBaseRentNotOwned) {
			t.Fatalf("player %d paid: %v", p, err)
		}
		if _, _, _, err := rent.Quote(ctx, s, p, baseID); !errors.Is(err, ErrBaseRentNotOwned) {
			t.Fatalf("player %d quoted: %v", p, err)
		}
	}
	if name, price, _, err := rent.Quote(ctx, s, mate, baseID); err != nil || name != "Clan Hall" || price != 250 {
		t.Fatalf("mate quote: %q %d %v", name, price, err)
	}
	paid, err := rent.Pay(ctx, s, mate, baseID, "rent-faction-0001")
	if err != nil || paid.BalanceAfter != 750 || paid.OwnerID != owner || paid.OwnerDiscord != "rent-owner-d" || paid.PayerName != "Mate" || paid.Payment.PlayerID != mate {
		t.Fatalf("mate pays from their own points: %+v %v", paid, err)
	}
	if replay, err := rent.Pay(ctx, s, mate, baseID, "rent-faction-0001"); err != nil || !replay.Duplicate || replay.OwnerID != 0 {
		t.Fatalf("replay doesn't notify again: %+v %v", replay, err)
	}
	if own, _ := rent.PlayerBases(ctx, s, owner); len(own) != 1 || own[0].Overdue {
		t.Fatalf("payment counts for the owner's base: %+v", own)
	}
	if hist, err := rent.PlayerPayments(ctx, s, owner, 10); err != nil || len(hist) != 1 || hist[0].PlayerName != "Mate" || hist[0].BaseName != "Clan Hall" || hist[0].Gift {
		t.Fatalf("owner's history shows who paid: %+v %v", hist, err)
	}
	if hist, err := rent.PlayerPayments(ctx, s, mate, 10); err != nil || len(hist) != 1 {
		t.Fatalf("mate's history: %+v %v", hist, err)
	}
	if hist, err := rent.PlayerPayments(ctx, s, outsider, 10); err != nil || len(hist) != 0 {
		t.Fatalf("outsider sees no history: %+v %v", hist, err)
	}
	if _, err := rent.SetSettings(ctx, s, false, 250, 7, nil); err != nil {
		t.Fatal(err)
	}
}
