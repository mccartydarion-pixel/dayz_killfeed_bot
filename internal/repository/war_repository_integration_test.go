//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
)

func TestWarChallengeNeedsBothFactionsOfTheGuild(t *testing.T) {
	db := saasIntegrationDB(t)
	home, other := newSaaSFixture(t, db), newSaaSFixture(t, db)
	ctx := context.Background()
	faction := func(fx saasFixture, tag string) int64 {
		t.Helper()
		var player, id int64
		if err := db.Pool.QueryRow(ctx, `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$2) RETURNING id`, fx.GuildRowID, "war-"+tag).Scan(&player); err != nil {
			t.Fatal(err)
		}
		if err := db.Pool.QueryRow(ctx, `INSERT INTO factions(guild_id,name,tag,owner_player_id) VALUES($1,$2,$2,$3) RETURNING id`, fx.GuildRowID, tag, player).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a, b, foreign := faction(home, "WA"), faction(home, "WB"), faction(other, "WX")
	wars := NewPostgresWarRepository(db.Pool)
	if _, err := wars.Challenge(ctx, home.GuildRowID, 0, a, foreign, 0); !errors.Is(err, ErrWarFactionNotInGuild) {
		t.Fatalf("a war against another server's faction: %v", err)
	}
	if _, err := wars.Challenge(ctx, other.GuildRowID, 0, a, b, 0); !errors.Is(err, ErrWarFactionNotInGuild) {
		t.Fatalf("a war between this guild's factions created from another guild: %v", err)
	}
	w, err := wars.Challenge(ctx, home.GuildRowID, 0, a, b, 0)
	if err != nil || w.GuildID != home.GuildRowID || w.Status != WarPending {
		t.Fatalf("a war between the guild's own factions: %+v %v", w, err)
	}
}
