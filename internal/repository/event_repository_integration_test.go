//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// TestScoreKillAccumulatesAgainstPartialUniqueIndexes is a regression test for
// the event_scores upserts: the only unique indexes are partial (WHERE
// player_id/faction_id IS NOT NULL), so ON CONFLICT must repeat the predicate or
// PostgreSQL rejects the statement with SQLSTATE 42P10.
func TestScoreKillAccumulatesAgainstPartialUniqueIndexes(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	t.Log("PostgreSQL integration tests: RUNNING")
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	guilds := NewGuildRepository(db.Pool)
	players := NewPlayerRepository(db.Pool)
	events := NewEventRepository(db.Pool)

	suffix := time.Now().UnixNano()
	guildID, err := guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("integration-event-score-%d", suffix)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Cascades to events, scores, kills, factions and players.
		_, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID)
	}()

	playerID, err := players.UpsertPlayer(ctx, guildID, fmt.Sprintf("dayz-killer-%d", suffix), "Killer", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var factionID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,$2,$3,$4) RETURNING id`, guildID, fmt.Sprintf("Faction %d", suffix), fmt.Sprintf("F%d", suffix), playerID).Scan(&factionID); err != nil {
		t.Fatal(err)
	}
	insertKill := func(n int) int64 {
		t.Helper()
		var id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO kills(guild_id,session_id,event_fingerprint,killer_player_id) VALUES($1,'integration',$2,$3) RETURNING id`, guildID, fmt.Sprintf("event-score-%d-%d", suffix, n), playerID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	kill1, kill2, kill3 := insertKill(1), insertKill(2), insertKill(3)

	now := time.Now().UTC()
	ev, err := events.CreateEvent(ctx, CompetitiveEvent{GuildID: guildID, Type: "KILLS", Name: "Score test", Status: "ACTIVE", StartsAt: &now, Config: []byte(`{}`)}, "tester")
	if err != nil {
		t.Fatal(err)
	}

	f64 := func(v float64) *float64 { return &v }

	// Two kills for the same player: the second must hit the upsert's DO UPDATE branch.
	if ok, err := events.ScoreKill(ctx, ev.ID, kill1, playerID, 0, 10, f64(100), 2); err != nil || !ok {
		t.Fatalf("first player kill: ok=%v err=%v", ok, err)
	}
	if ok, err := events.ScoreKill(ctx, ev.ID, kill2, playerID, 0, 5, f64(50), 4); err != nil || !ok {
		t.Fatalf("second player kill: ok=%v err=%v", ok, err)
	}
	// One faction kill.
	if ok, err := events.ScoreKill(ctx, ev.ID, kill3, 0, factionID, 7, f64(30), 1); err != nil || !ok {
		t.Fatalf("faction kill: ok=%v err=%v", ok, err)
	}
	// Replaying an already-scored kill must be a no-op.
	if ok, err := events.ScoreKill(ctx, ev.ID, kill1, playerID, 0, 10, f64(100), 2); err != nil || ok {
		t.Fatalf("replayed kill must be ignored: ok=%v err=%v", ok, err)
	}

	board, err := events.Leaderboard(ctx, ev.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 2 {
		t.Fatalf("expected 2 score rows (one player, one faction), got %d: %+v", len(board), board)
	}
	var player, faction *EventScore
	for i := range board {
		switch {
		case board[i].PlayerID == playerID:
			player = &board[i]
		case board[i].FactionID == factionID:
			faction = &board[i]
		}
	}
	if player == nil || faction == nil {
		t.Fatalf("missing player or faction row: %+v", board)
	}
	if player.Score != 15 || player.Kills != 2 || player.BestStreak != 4 || player.BestDistance == nil || *player.BestDistance != 100 {
		t.Fatalf("player accumulation wrong: %+v (best_distance=%v)", *player, player.BestDistance)
	}
	if faction.Score != 7 || faction.Kills != 1 || faction.BestStreak != 1 || faction.BestDistance == nil || *faction.BestDistance != 30 {
		t.Fatalf("faction accumulation wrong: %+v (best_distance=%v)", *faction, faction.BestDistance)
	}
}
