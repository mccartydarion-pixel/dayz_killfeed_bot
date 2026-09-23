//go:build integration

package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestADMLocationAxisFixSwapsOnlyADMRows seeds pre-fix rows (z = ADM altitude, y = ADM north) and
// re-applies the 0044 SQL: ADM rows must come back as x=east, z=north, y=altitude, and rows from
// any other source must be untouched.
func TestADMLocationAxisFixSwapsOnlyADMRows(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	var guildID, serverID, playerID int64
	if err := db.Pool.QueryRow(ctx, `INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`, fmt.Sprintf("axis-guild-%d", suffix)).Scan(&guildID); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id,provider,provider_service_id,game,platform,status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		guildID, fmt.Sprintf("axis-svc-%d", suffix)).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,'Axis') RETURNING id`,
		guildID, fmt.Sprintf("axis-%d", suffix)).Scan(&playerID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	// Pre-fix shape of ADM "pos=<7504.7, 1334.4, 0.9>": z got altitude, y got north.
	insert := `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,y,event_type,observed_at,source) VALUES($1,$2,$3,'Axis',$4,$5,$6,$7,$8,$9) RETURNING id`
	var admID, otherID int64
	if err := db.Pool.QueryRow(ctx, insert, guildID, serverID, playerID, 7504.7, 0.9, 1334.4, "CONNECT", now, "ADM").Scan(&admID); err != nil {
		t.Fatal(err)
	}
	if err := db.Pool.QueryRow(ctx, insert, guildID, serverID, playerID, 100.0, 200.0, 3.0, "HIT", now, "OTHER").Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	// Re-run the exact migration statement, scoped to this fixture's guild so other rows in a shared
	// integration database are never flipped a second time.
	scoped := strings.TrimSuffix(strings.TrimSpace(admLocationAxisFixSQL), ";") + ` AND guild_id = $1`
	if _, err := db.Pool.Exec(ctx, scoped, guildID); err != nil {
		t.Fatal(err)
	}

	read := func(id int64) (x, z, y float64) {
		t.Helper()
		if err := db.Pool.QueryRow(ctx, `SELECT x, z, y FROM player_location_events WHERE id=$1`, id).Scan(&x, &z, &y); err != nil {
			t.Fatal(err)
		}
		return
	}
	if x, z, y := read(admID); x != 7504.7 || z != 1334.4 || y != 0.9 {
		t.Fatalf("expected ADM row repaired to x=7504.7 z=1334.4 y=0.9, got x=%v z=%v y=%v", x, z, y)
	}
	if x, z, y := read(otherID); x != 100 || z != 200 || y != 3 {
		t.Fatalf("expected non-ADM row untouched, got x=%v z=%v y=%v", x, z, y)
	}
	_, _ = db.Pool.Exec(ctx, `DELETE FROM guilds WHERE id=$1`, guildID)
}
