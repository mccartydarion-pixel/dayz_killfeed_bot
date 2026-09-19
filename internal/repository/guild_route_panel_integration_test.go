//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The route-panel message store round-trips against real PostgreSQL: one row
// per (guild, route key, channel), scoped by guild, replaced on upsert, and
// removed when its guild is deleted.
func TestGuildRoutePanelRepository(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	repo := NewGuildRoutePanelRepository(db.Pool)

	newGuild := func() int64 {
		var id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("route-panel-%d", time.Now().UnixNano())).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	guildA, guildB := newGuild(), newGuild()

	if rows, err := repo.List(ctx, guildA, "LINK_GAMERTAG"); err != nil || len(rows) != 0 {
		t.Fatalf("expected an empty list, got %v err=%v", rows, err)
	}
	if err := repo.Upsert(ctx, guildA, "LINK_GAMERTAG", "chan-1", "msg-1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, guildA, "LINK_GAMERTAG", "chan-2", "msg-2"); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces the message id for the same channel - it never adds a second row.
	if err := repo.Upsert(ctx, guildA, "LINK_GAMERTAG", "chan-1", "msg-1b"); err != nil {
		t.Fatal(err)
	}
	// Other keys and other guilds are separate.
	if err := repo.Upsert(ctx, guildA, "STATS_LEADERBOARDS", "chan-1", "stats-1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, guildB, "LINK_GAMERTAG", "chan-1", "other-guild"); err != nil {
		t.Fatal(err)
	}

	rows, err := repo.List(ctx, guildA, "LINK_GAMERTAG")
	if err != nil || len(rows) != 2 || rows[0].ChannelID != "chan-1" || rows[0].MessageID != "msg-1b" || rows[1].ChannelID != "chan-2" {
		t.Fatalf("expected 2 rows with chan-1 replaced, got %+v err=%v", rows, err)
	}
	if rows, _ := repo.List(ctx, guildB, "LINK_GAMERTAG"); len(rows) != 1 || rows[0].MessageID != "other-guild" {
		t.Fatalf("guild B must only see its own row, got %+v", rows)
	}

	if err := repo.Delete(ctx, guildA, "LINK_GAMERTAG", "chan-2"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, guildA, "LINK_GAMERTAG", "never-existed"); err != nil {
		t.Fatalf("deleting a missing row must be a no-op: %v", err)
	}
	if rows, _ := repo.List(ctx, guildA, "LINK_GAMERTAG"); len(rows) != 1 {
		t.Fatalf("expected 1 row after delete, got %+v", rows)
	}

	// The rows follow their guild.
	if _, err := db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildA); err != nil {
		t.Fatal(err)
	}
	if rows, _ := repo.List(ctx, guildA, "STATS_LEADERBOARDS"); len(rows) != 0 {
		t.Fatalf("expected rows to cascade with the guild, got %+v", rows)
	}
}
