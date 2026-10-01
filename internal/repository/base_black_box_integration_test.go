//go:build integration

package repository

import (
	"context"
	"testing"
	"time"
)

func TestBaseBlackBoxRecordsMergesPrunesAndSells(t *testing.T) {
	zones, fx, seedPlayer := newZoneTestWorld(t)
	pool := zones.pool
	ctx := context.Background()
	repo := NewBaseBlackBoxRepository(pool)
	owner, friend, mate, stranger, other := seedPlayer("Owner"), seedPlayer("Friend"), seedPlayer("Mate"), seedPlayer("Stranger"), seedPlayer("Other")
	var faction int64
	if err := pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,'BB','BB',$2) RETURNING id`, fx.GuildRowID, owner).Scan(&faction); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{owner, mate} {
		if _, err := pool.Exec(ctx, `INSERT INTO faction_members(guild_id,faction_id,player_id,role) VALUES($1,$2,$3,'MEMBER')`, fx.GuildRowID, faction, p); err != nil {
			t.Fatal(err)
		}
	}
	var baseID, otherBase int64
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','North Base',1000,2000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, owner).Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Other Base',6000,2000,50) RETURNING id`, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, other).Scan(&otherBase); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO case_base_authorizations(installation_id,guild_id,server_id,base_id,player_id,valid_from) VALUES($1,$2,$3,$4,$5,$6)`,
		fx.InstallationID, fx.GuildRowID, fx.ServerRowID, baseID, friend, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	visit := func(player int64, x float64) []BlackBoxMatch {
		t.Helper()
		m, err := repo.MatchVisit(ctx, fx.GuildRowID, fx.ServerRowID, player, x, 2000)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	history := func(ownerID int64) []BlackBoxEvent {
		t.Helper()
		_, ev, err := repo.OwnerHistory(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, ownerID, 100)
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}

	if s, err := repo.GetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID); err != nil || s.Enabled || s.RetentionDays != 14 {
		t.Fatalf("default settings: %+v %v", s, err)
	}
	if len(visit(stranger, 1120)) != 0 {
		t.Fatal("matched while the Black Box is off")
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 31, nil); err == nil {
		t.Fatal("retention above the maximum accepted")
	}
	if _, err := repo.SetSettings(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, true, 7, &fx.OwnerUserID); err != nil {
		t.Fatal(err)
	}
	// Radius 50 + 100 m margin = 150 m recorded.
	for name, p := range map[string]int64{"owner": owner, "friend": friend, "faction mate": mate} {
		if len(visit(p, 1120)) != 0 {
			t.Fatalf("%s was recorded", name)
		}
	}
	if len(visit(stranger, 1160)) != 0 {
		t.Fatal("outside the recorded area matched")
	}
	got := visit(stranger, 1120)
	if len(got) != 1 || got[0].BaseID != baseID || got[0].DistanceMeters != 120 || got[0].PlayerID != stranger {
		t.Fatalf("stranger match: %+v", got)
	}
	if err := repo.Record(ctx, got[0], BlackBoxVisit, "Stranger", ""); err != nil {
		t.Fatal(err)
	}
	closer := visit(stranger, 1060)
	if err := repo.Record(ctx, closer[0], BlackBoxVisit, "Stranger", ""); err != nil {
		t.Fatal(err)
	}
	ev := history(owner)
	if len(ev) != 1 || ev[0].Sightings != 2 || ev[0].ClosestMeters != 60 || ev[0].BaseName != "North Base" || ev[0].Kind != BlackBoxVisit {
		t.Fatalf("sightings within ten minutes must merge: %+v", ev)
	}
	if _, err := pool.Exec(ctx, `UPDATE base_black_box_events SET first_seen_at=NOW()-INTERVAL '1 hour',last_seen_at=NOW()-INTERVAL '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, got[0], BlackBoxVisit, "Stranger", ""); err != nil {
		t.Fatal(err)
	}
	if ev := history(owner); len(ev) != 2 {
		t.Fatalf("a later visit must be a new entry: %+v", ev)
	}

	// Dismantles are matched by in-game id and only inside the base circle.
	var admID string
	if err := pool.QueryRow(ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, stranger).Scan(&admID); err != nil {
		t.Fatal(err)
	}
	if m, err := repo.MatchDismantle(ctx, fx.GuildRowID, fx.ServerRowID, admID, 1120, 2000); err != nil || len(m) != 0 {
		t.Fatalf("dismantle outside the circle matched: %+v %v", m, err)
	}
	if m, err := repo.MatchDismantle(ctx, fx.GuildRowID, fx.ServerRowID, "unknown-adm-id", 1000, 2000); err != nil || len(m) != 0 {
		t.Fatalf("unknown player matched: %+v %v", m, err)
	}
	dm, err := repo.MatchDismantle(ctx, fx.GuildRowID, fx.ServerRowID, admID, 1010, 2000)
	if err != nil || len(dm) != 1 || dm[0].PlayerID != stranger {
		t.Fatalf("dismantle match: %+v %v", dm, err)
	}
	if err := repo.Record(ctx, dm[0], BlackBoxDismantle, "Stranger", "Wall"); err != nil {
		t.Fatal(err)
	}
	if ev := history(owner); len(ev) != 3 || ev[0].Kind != BlackBoxDismantle || ev[0].Detail != "Wall" {
		t.Fatalf("dismantle entry: %+v", ev)
	}
	// Owners only see their own bases.
	if ev := history(other); len(ev) != 0 {
		t.Fatalf("another owner saw this base's history: %+v", ev)
	}
	if recent, err := repo.Recent(ctx, fx.InstallationID, fx.GuildRowID, fx.ServerRowID, 10); err != nil || len(recent) != 3 {
		t.Fatalf("server recent: %+v %v", recent, err)
	}

	// Prune keeps the owner's retention (7 days here).
	if _, err := pool.Exec(ctx, `UPDATE base_black_box_events SET last_seen_at=NOW()-INTERVAL '8 days' WHERE kind='VISIT'`); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Prune(ctx); err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	if ev := history(owner); len(ev) != 1 || ev[0].Kind != BlackBoxDismantle {
		t.Fatalf("after prune: %+v", ev)
	}

	// While selling, only owners with paid Black Box time are recorded.
	sales := NewSecurityServiceRepository(pool)
	scope := SecurityScope{InstallationID: fx.InstallationID, GuildID: fx.GuildRowID, ServerID: fx.ServerRowID}
	if _, err := sales.SetOffer(ctx, scope, ServiceBaseBlackBox, true, 300, 7, nil); err != nil {
		t.Fatal(err)
	}
	if len(visit(stranger, 1120)) != 0 {
		t.Fatal("unpaid owner recorded while the Black Box is on sale")
	}
	if _, err := NewEconomyRepository(pool).Credit(ctx, LedgerParams{GuildID: fx.GuildRowID, PlayerID: owner, Type: TxAdminCredit, Amount: 500, ReferenceID: "bb-seed", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	p, err := sales.Purchase(ctx, scope, owner, ServiceBaseBlackBox, "bb-request-1")
	if err != nil || p.Purchase.ServiceID != ServiceBaseBlackBox || p.BalanceAfter != 200 {
		t.Fatalf("black box purchase: %+v %v", p, err)
	}
	if len(visit(stranger, 1120)) != 1 {
		t.Fatal("paying owner not recorded")
	}
	if until, err := sales.ActiveUntil(ctx, fx.InstallationID, owner, ServicePerimeterWatch); err != nil || until != nil {
		t.Fatalf("black box purchase counted as perimeter time: %v %v", until, err)
	}
}
