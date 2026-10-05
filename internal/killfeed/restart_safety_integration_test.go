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

// The restart-safety scenarios of restart_safety_test.go on PostgreSQL: the real kills/deaths
// tables with their UNIQUE (guild_id, event_fingerprint) constraint, the real adm_checkpoints row,
// and - for the overlap - one connection pool per process, as two bot processes have.

type pgCheckpoints struct {
	repo *repository.CheckpointRepository
}

func (s pgCheckpoints) LoadADMCheckpoint(ctx context.Context, guildID, serverID int64) (*DurableCheckpoint, error) {
	cp, err := s.repo.LoadADMCheckpoint(ctx, guildID, serverID)
	if err != nil || cp == nil {
		return nil, err
	}
	return &DurableCheckpoint{Filename: cp.Filename, RemoteModifiedAt: cp.RemoteModifiedAt, RemoteSize: cp.RemoteSize,
		ProcessedOffset: cp.ProcessedOffset, PendingPartialLine: cp.PendingPartialLine}, nil
}

func (s pgCheckpoints) SaveADMCheckpoint(ctx context.Context, guildID, serverID int64, sessionID string, cp DurableCheckpoint) error {
	return s.repo.SaveADMCheckpoint(ctx, repository.ADMCheckpoint{ServerID: serverID, SessionID: sessionID, Filename: cp.Filename,
		RemoteModifiedAt: cp.RemoteModifiedAt, RemoteSize: cp.RemoteSize, ProcessedOffset: cp.ProcessedOffset, PendingPartialLine: cp.PendingPartialLine}, guildID)
}

// pgRestartDB creates a fresh guild and server and returns the database two processes share, each
// through its own pool, plus a counter of the kills stored for that guild.
func pgRestartDB(t *testing.T, label string) (restartDB, func() int) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for a non-production database")
	}
	ctx := context.Background()
	var pools [2]*database.DB
	for i := range pools {
		db, err := database.Connect(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		pools[i] = db
	}
	if err := pools[0].Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	guildID, err := repository.NewGuildRepository(pools[0].Pool).UpsertGuild(ctx,
		repository.GuildRecord{DiscordGuildID: fmt.Sprintf("restart-%s-%d", label, suffix)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := repository.NewServerRepository(pools[0].Pool).UpsertGameServer(ctx,
		repository.GameServer{GuildID: guildID, Provider: "fixture", ProviderServiceID: fmt.Sprintf("restart-%s-%d", label, suffix),
			Game: "dayz", Platform: "PLAYSTATION", Status: "CONNECTED", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	out := restartDB{guildID: guildID, serverID: server.ID, checkpoints: pgCheckpoints{repository.NewCheckpointRepository(pools[0].Pool)}}
	for i, db := range pools {
		out.stores[i] = rankedADMStore{repository.NewPlayerRepository(db.Pool), repository.NewKillRepository(db.Pool), repository.NewDeathRepository(db.Pool)}
	}
	storedKills := func() int {
		var n int
		if err := pools[0].Pool.QueryRow(ctx, `SELECT COUNT(*) FROM kills WHERE guild_id=$1`, guildID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return out, storedKills
}

func TestRestartKillsWhileDownArePostedInOrderPostgres(t *testing.T) {
	db, storedKills := pgRestartDB(t, "down")
	runKillsWhileDownArePostedInOrder(t, 90000111, db)
	if got := storedKills(); got != 10 {
		t.Fatalf("kills rows = %d, want 10 (3 before the restart, 7 while down)", got)
	}
}

func TestRestartMidBatchPostsEachKillOncePostgres(t *testing.T) {
	db, storedKills := pgRestartDB(t, "midbatch")
	runRestartMidBatchPostsEachKillOnce(t, 90000112, db)
	if got := storedKills(); got != 6 {
		t.Fatalf("kills rows = %d, want 6", got)
	}
}

func TestOverlappingProcessesPostEachKillOncePostgres(t *testing.T) {
	db, storedKills := pgRestartDB(t, "overlap")
	runOverlappingProcessesPostEachKillOnce(t, 90000113, db)
	if got := storedKills(); got != 25 {
		t.Fatalf("kills rows = %d, want 25 (2 + 4 bursts of 5 + 3)", got)
	}
}
