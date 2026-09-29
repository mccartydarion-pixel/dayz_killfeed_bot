//go:build integration

package killfeed

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type rankedADMStore struct {
	players *repository.PlayerRepository
	kills *repository.KillRepository
	deaths *repository.DeathRepository
}
func (s rankedADMStore) UpsertPlayer(ctx context.Context, guild int64, id, name string, at time.Time) (int64, error) {
	return s.players.UpsertPlayer(ctx, guild, id, name, at)
}
func (s rankedADMStore) InsertKill(ctx context.Context, record repository.KillRecord) error {
	return s.kills.InsertKill(ctx, record)
}
func (s rankedADMStore) InsertKillReturning(ctx context.Context, record repository.KillRecord) (int64, error) {
	return s.kills.InsertKillReturning(ctx, record)
}
func (s rankedADMStore) InsertDeath(ctx context.Context, record repository.DeathRecord) error {
	return s.deaths.InsertDeath(ctx, record)
}

type rankedADMPostprocessor struct {
	ranked *repository.RankedRepository
	serverID int64
	decisions chan rankedADMDecision
}
type rankedADMDecision struct {
	award repository.RankedAward
	err error
}
func (p rankedADMPostprocessor) ProcessPersistedKill(ctx context.Context, killID int64, _ repository.KillRecord, _ *Event) {
	award, err := p.ranked.AwardActiveServerKill(ctx, p.serverID, killID)
	p.decisions <- rankedADMDecision{award: award, err: err}
}

func TestRealADMKillAwardsServerRankedRP(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" { t.Fatal("TEST_DATABASE_URL is required") }
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := database.Connect(ctx, url)
	if err != nil { t.Fatal(err) }
	defer db.Close()
	if err = db.Migrate(ctx); err != nil { t.Fatal(err) }

	suffix := time.Now().UnixNano()
	guildID, err := repository.NewGuildRepository(db.Pool).UpsertGuild(ctx,
		repository.GuildRecord{DiscordGuildID: fmt.Sprintf("ranked-adm-%d", suffix)})
	if err != nil { t.Fatal(err) }
	server, err := repository.NewServerRepository(db.Pool).UpsertGameServer(ctx,
		repository.GameServer{GuildID: guildID, Provider: "fixture",
			ProviderServiceID: fmt.Sprintf("ranked-adm-%d", suffix),
			Game: "dayz", Platform: "PLAYSTATION", Status: "ACTIVE", Active: true})
	if err != nil { t.Fatal(err) }
	if err = repository.NewLiveSyncRepository(db.Pool).SetServerUTCOffset(
		ctx, guildID, server.ID, -240, "ranked-adm-integration"); err != nil { t.Fatal(err) }
	seasonStart := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	var seasonID int64
	err = db.Pool.QueryRow(ctx, `INSERT INTO ranked_seasons(scope,platform,server_id,status,rp_per_kill,thresholds,starts_at)
VALUES('SERVER','PLAYSTATION',$1,'ACTIVE',100,ARRAY[100,300,600,1000,1500,2100,2800]::bigint[],$2) RETURNING id`,
		server.ID, seasonStart).Scan(&seasonID)
	if err != nil { t.Fatal(err) }

	store := rankedADMStore{repository.NewPlayerRepository(db.Pool), repository.NewKillRepository(db.Pool), repository.NewDeathRepository(db.Pool)}
	rankedRepo := repository.NewRankedRepository(db.Pool)
	pq := NewPersistenceQueueWithServerID(store, guildID, server.ID, "ranked-adm-session")
	decisions := make(chan rankedADMDecision, 3)
	pq.SetKillPostProcessor(rankedADMPostprocessor{ranked: rankedRepo, serverID: server.ID, decisions: decisions})
	go pq.Run(ctx)
	engine := NewEngine(nil, fmt.Sprint(server.ID), NewADMParser())
	engine.SetPersistence(pq)
	path := "/games/fixture/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_16-00-00.ADM"
	for i, clock := range []string{"16:40:12", "16:44:12", "16:45:12"} {
		line := fmt.Sprintf(`%s | Player "victim" (DEAD) (id=victim-%d pos=<1, 2, 3>) killed by Player "killer" (id=killer-%d pos=<4, 5, 6>) with M4-A1 from 62.1978 meters`,
			clock, suffix, suffix)
		if _, err = engine.processLineAt(line, path, int64(900+i*200)); err != nil { t.Fatal(err) }
	}
	for i, want := range []string{"AWARDED", "COOLDOWN", "AWARDED"} {
		select {
		case decision := <-decisions:
			if decision.err != nil || decision.award.Outcome != want {
				t.Fatalf("decision %d = %+v err=%v, want %s", i, decision.award, decision.err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for persisted ADM award")
		}
	}
	var nullEvents int
	if err = db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM kills WHERE server_id=$1 AND session_id='ranked-adm-session' AND event_time IS NULL`,
		server.ID).Scan(&nullEvents); err != nil || nullEvents != 3 {
		t.Fatalf("real ADM event_time null rows=%d err=%v", nullEvents, err)
	}
	standings, err := rankedRepo.ServerStandings(ctx, server.ID, 15)
	if err != nil || len(standings) != 1 || standings[0].RP != 200 || standings[0].Tier != "ROOKIE" {
		t.Fatalf("server standings=%+v err=%v", standings, err)
	}
	var total int64
	if err = db.Pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0) FROM ranked_awards WHERE season_id=$1`, seasonID).Scan(&total); err != nil || total != 200 {
		t.Fatalf("ranked ledger total=%d err=%v", total, err)
	}
}
