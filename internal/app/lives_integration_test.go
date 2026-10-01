//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Lives over the real routes and a real PostgreSQL (docs/LIVES.md): the persistence adapter closes
// lives from persisted kills and deaths, and the player API reads them back.

func (w *factionWorld) livesPath(installationID int64, suffix string) string {
	return fmt.Sprintf("/api/saas/player/servers/%d/lives%s", installationID, suffix)
}

// seedPlayer adds an unlinked tracked player to the fixture's guild.
func (w *factionWorld) seedPlayer(f installationFixture, name string) int64 {
	w.t.Helper()
	guild, _ := w.gameContext(f)
	var id int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,$3) RETURNING id`,
		guild, fmt.Sprintf("dzs-%d-%d", time.Now().UnixNano(), statsTestSeq.Add(1)), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// session records observed playtime for a player: connect, 30-second checkpoints, disconnect.
func (w *factionWorld) session(f installationFixture, player int64, from, to time.Time) {
	w.t.Helper()
	ctx := context.Background()
	guild, server := w.gameContext(f)
	if err := w.a.ActivityRepository.Connect(ctx, guild, server, player, from); err != nil {
		w.t.Fatal(err)
	}
	for at := from.Add(30 * time.Second); at.Before(to); at = at.Add(30 * time.Second) {
		if err := w.a.ActivityRepository.Checkpoint(ctx, guild, server, player, at); err != nil {
			w.t.Fatal(err)
		}
	}
	if err := w.a.ActivityRepository.Disconnect(ctx, guild, server, player, to); err != nil {
		w.t.Fatal(err)
	}
}

// persistKill runs a kill through the same adapter hook the persistence worker calls.
func (w *factionWorld) persistKill(f installationFixture, killer, victim int64, killerName, victimName string, at time.Time, distance float64) {
	w.t.Helper()
	guild, server := w.gameContext(f)
	rec := repository.KillRecord{GuildID: guild, ServerID: server, SessionID: "s", Fingerprint: fmt.Sprintf("life-kill-%d", statsTestSeq.Add(1)),
		KillerPlayerID: killer, VictimPlayerID: victim, WeaponRaw: "Mosin", WeaponDisplay: "Mosin", Distance: &distance, EventTime: &at}
	id, err := repository.NewKillRepository(w.a.DB.Pool).InsertKillReturning(context.Background(), rec)
	if err != nil {
		w.t.Fatal(err)
	}
	adapter := &persistenceStoreAdapter{lives: w.a.Lives}
	adapter.recordLifeEndFromKill(context.Background(), id, rec, &killfeed.Event{Type: killfeed.EventPlayerKill,
		Killer: &killfeed.PlayerRef{Name: killerName}, Victim: &killfeed.PlayerRef{Name: victimName}})
}

func TestPlayerLivesEndToEnd(t *testing.T) {
	w := newFactionWorld(t)
	actor := w.players[0]
	hero := w.linkPlayer(w.a1, actor, "LifeHero")
	rival := w.seedPlayer(w.a1, "LifeRival")
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)

	w.session(w.a1, hero, t0, t0.Add(20*time.Minute))
	w.session(w.a1, rival, t0, t0.Add(20*time.Minute))
	w.persistKill(w.a1, hero, rival, "LifeHero", "LifeRival", t0.Add(5*time.Minute), 210)
	w.persistKill(w.a1, rival, hero, "LifeRival", "LifeHero", t0.Add(20*time.Minute), 35)
	// The trailing "died" hook for the same death is ignored; a suicide emote alone never ends a life.
	guild, server := w.gameContext(w.a1)
	adapter := &persistenceStoreAdapter{lives: w.a.Lives}
	at := t0.Add(20*time.Minute + time.Second)
	adapter.recordLifeEnd(context.Background(), repository.DeathRecord{GuildID: guild, ServerID: server, PlayerID: hero, Fingerprint: "trailing", EventTime: &at},
		&killfeed.Event{Type: killfeed.EventPlayerDeath, Player: &killfeed.PlayerRef{Name: "LifeHero"}})
	later := t0.Add(40 * time.Minute)
	adapter.recordLifeEnd(context.Background(), repository.DeathRecord{GuildID: guild, ServerID: server, PlayerID: hero, Fingerprint: "emote", EventTime: &later},
		&killfeed.Event{Type: killfeed.EventSuicideAction, Player: &killfeed.PlayerRef{Name: "LifeHero"}})
	// Hero's second life, still in progress: ten more minutes.
	w.session(w.a1, hero, t0.Add(60*time.Minute), t0.Add(70*time.Minute))

	resp := w.getJSON(w.livesPath(w.a1.InstallationID, ""), actor)
	recent := resp["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("expected exactly one ended life, got %v", resp)
	}
	life := recent[0].(map[string]any)
	if life["cause"] != "PVP" || life["killerName"] != "LifeRival" || life["weapon"] != "Mosin" || life["kills"].(float64) != 1 ||
		life["playtimeSeconds"].(float64) != 1200 || life["longestKillMeters"].(float64) != 210 || life["distanceMeters"].(float64) != 35 {
		t.Fatalf("life = %v", life)
	}
	cur := resp["current"].(map[string]any)
	if cur["playtimeSeconds"].(float64) != 600 || cur["kills"].(float64) != 0 || cur["online"].(bool) {
		t.Fatalf("current = %v", cur)
	}
	sum := resp["summary"].(map[string]any)
	if sum["lives"].(float64) != 1 || sum["deathsByPvp"].(float64) != 1 || sum["longestPlaytimeSeconds"].(float64) != 1200 {
		t.Fatalf("summary = %v", sum)
	}

	// Both sessions were recorded before either kill, so both ended lives carry the full 20 minutes.
	board := w.getJSON(w.livesPath(w.a1.InstallationID, "/leaderboard?board=longest"), actor)
	lives := board["lives"].([]any)
	if len(lives) != 2 || lives[0].(map[string]any)["playtimeSeconds"].(float64) != 1200 || lives[1].(map[string]any)["playtimeSeconds"].(float64) != 1200 {
		t.Fatalf("longest board = %v", board)
	}
	// Rival has not played since dying (zero seconds, unranked); Hero is ten minutes into a new life.
	alive := w.getJSON(w.livesPath(w.a1.InstallationID, "/leaderboard"), actor)
	rows := alive["alive"].([]any)
	if alive["board"] != "ALIVE" || len(rows) != 1 || rows[0].(map[string]any)["playerName"] != "LifeHero" || rows[0].(map[string]any)["playtimeSeconds"].(float64) != 600 {
		t.Fatalf("alive board = %v", alive)
	}
	kills := w.getJSON(w.livesPath(w.a1.InstallationID, "/leaderboard?board=KILLS&days=1"), actor)
	if got := kills["lives"].([]any); len(got) != 1 || kills["windowDays"].(float64) != 1 {
		t.Fatalf("kills board = %v", kills)
	}

	for path, code := range map[string]string{"/leaderboard?board=nope": codeInvalidRequest, "/leaderboard?board=kills&days=0": codeInvalidRequest, "?limit=500": codeInvalidRequest} {
		if r := w.do(http.MethodGet, w.livesPath(w.a1.InstallationID, path), actor, nil); r.Status != http.StatusBadRequest || r.errCode(t) != code {
			t.Fatalf("%s: %d %s", path, r.Status, r.Body)
		}
	}
}

func TestPlayerLivesRequiresVerifiedObservedPlayer(t *testing.T) {
	w := newFactionWorld(t)
	unlinked, elsewhere := w.players[1], w.players[2]
	if r := w.do(http.MethodGet, w.livesPath(w.a1.InstallationID, ""), unlinked, nil); r.Status != http.StatusForbidden && r.errCode(t) != codePlayerIdentityRequired {
		t.Fatalf("unlinked: %d %s", r.Status, r.Body)
	}
	// Verified on org A's guild but never observed on org B's server: non-enumerating 404.
	p := w.linkPlayer(w.a1, elsewhere, "OnlyA")
	w.insertKill(w.a1, p, time.Now(), false)
	for _, suffix := range []string{"", "/leaderboard"} {
		if r := w.do(http.MethodGet, w.livesPath(w.b1.InstallationID, suffix), elsewhere, nil); r.errCode(t) == "" || r.Status == http.StatusOK {
			t.Fatalf("cross-tenant %q: %d %s", suffix, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodGet, w.livesPath(w.a1.InstallationID, ""), "", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("no acting user: %d", r.Status)
	}
}
