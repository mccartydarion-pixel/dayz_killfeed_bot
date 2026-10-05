//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type rpBoostWorld struct {
	*clientAdminWorld
	cards  []*discordgo.MessageEmbed
	killer int64
	victim int64
	seq    int
}

func newRPBoostWorld(t *testing.T, withSeason bool) *rpBoostWorld {
	t.Helper()
	w := &rpBoostWorld{clientAdminWorld: newClientAdminWorld(t)}
	w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
	w.a.rpBoostAnnouncer = func(_ int64, e *discordgo.MessageEmbed) { w.cards = append(w.cards, e) }
	if withSeason {
		req := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}}
		if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, w.path("/ranked/server-season"), w.f.OwnerDiscordID, req, nil); rr.Code != http.StatusOK {
			t.Fatalf("start season: %d %s", rr.Code, rr.Body.String())
		}
	}
	w.killer, w.victim = w.newPlayer("Hunter"), w.newPlayer("Prey")
	return w
}

// newPlayer is seedPlayer with an id that cannot collide when called in quick succession (the
// clock on some systems repeats nanosecond readings).
func (w *rpBoostWorld) newPlayer(name string) int64 {
	w.t.Helper()
	w.seq++
	var id int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id,dayz_player_id,display_name) VALUES($1,$2,$3) RETURNING id`,
		w.guildID, fmt.Sprintf("rp-%d-%d", time.Now().UnixNano(), w.seq), name).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *rpBoostWorld) start(hours int, startsAt *time.Time) (repository.RPBoost, int, string) {
	w.t.Helper()
	rr := w.call(w.a.handleCreateRPBoost, http.MethodPost, w.path("/ranked/boosts"), w.f.OwnerDiscordID, createRPBoostRequest{Hours: hours, StartsAt: startsAt}, nil)
	if rr.Code != http.StatusOK {
		return repository.RPBoost{}, rr.Code, rr.Body.String()
	}
	return decodeBody[repository.RPBoost](w.t, rr), rr.Code, ""
}

// award records a kill that happened at `at` (a different victim each time, so the repeat-kill
// cooldown never applies) and returns the RP it earned.
func (w *rpBoostWorld) award(at time.Time) repository.RankedAward {
	w.t.Helper()
	w.seq++
	victim := w.newPlayer(fmt.Sprintf("Prey%d", w.seq))
	id, err := repository.NewKillRepository(w.a.DB.Pool).InsertKillReturning(context.Background(), repository.KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "rp-boost",
		Fingerprint: fmt.Sprintf("rp-boost-%d-%d", time.Now().UnixNano(), w.seq), KillerPlayerID: w.killer, VictimPlayerID: victim, EventTime: &at})
	if err != nil {
		w.t.Fatal(err)
	}
	got, err := w.a.Ranked.AwardActiveServerKill(context.Background(), w.serverID, id)
	if err != nil {
		w.t.Fatalf("award: %v", err)
	}
	return got
}

func TestDoubleRPDoublesKillsInsideTheWindowOnly(t *testing.T) {
	w := newRPBoostWorld(t, true)
	now := time.Now().UTC()

	before := w.award(now)
	if before.Amount != 100 || before.Multiplier != 1 {
		t.Fatalf("before double RP a kill earns the normal 100: %+v", before)
	}
	boost, code, body := w.start(2, nil)
	if code != http.StatusOK || boost.Status != repository.RPBoostLive || boost.Multiplier != 2 {
		t.Fatalf("start: %d %s %+v", code, body, boost)
	}
	if len(w.cards) != 1 || !strings.Contains(w.cards[0].Title, "is live") || !strings.Contains(w.cards[0].Description, "200 RP instead of 100") {
		t.Fatalf("the live card should go out at once: %d cards", len(w.cards))
	}
	during := w.award(now.Add(10 * time.Second))
	if during.Amount != 200 || during.Multiplier != 2 {
		t.Fatalf("inside the window a kill earns 200: %+v", during)
	}
	// A replay returns the stored decision, multiplier included.
	var stored int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT multiplier FROM ranked_awards WHERE season_id=$1 ORDER BY id DESC LIMIT 1`, during.SeasonID).Scan(&stored); err != nil || stored != 2 {
		t.Fatalf("the award records its multiplier: %d %v", stored, err)
	}

	// The player's rank answer carries the window.
	player := syncUser(t, w.a, fmt.Sprintf("rp-fan-%d", time.Now().UnixNano()), "Fan")
	if b, err := w.a.Ranked.CurrentRPBoost(context.Background(), w.serverID, time.Now().UTC()); err != nil || b == nil || b.ID != boost.ID || b.Status != repository.RPBoostLive {
		t.Fatalf("current boost: %+v %v", b, err)
	}
	_ = player

	// Stopping it ends the window: a later kill earns normal RP, an earlier one that arrives late keeps its double.
	stopPath := w.path(fmt.Sprintf("/ranked/boosts/%d/stop", boost.ID))
	if rr := w.call(w.a.handleStopRPBoost, http.MethodPost, stopPath, w.f.OwnerDiscordID, nil, map[string]string{"boostID": fmt.Sprint(boost.ID)}); rr.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", rr.Code, rr.Body.String())
	}
	if after := w.award(time.Now().UTC().Add(time.Minute)); after.Amount != 100 || after.Multiplier != 1 {
		t.Fatalf("after stopping, normal RP: %+v", after)
	}
	if late := w.award(boost.StartsAt); late.Amount != 200 {
		t.Fatalf("a kill that happened during the window still earns double when it is processed late: %+v", late)
	}
	// The results card waits for kills that happened in the window but reach the bot late, then
	// names who earned the most.
	if len(w.cards) != 1 {
		t.Fatalf("no end card until the late kills had time to arrive: %d cards", len(w.cards))
	}
	w.a.runRPBoostAnnouncements(context.Background(), w.guildID, time.Now().UTC().Add(repository.RPBoostResultsDelay+time.Minute))
	if len(w.cards) != 2 || !strings.Contains(w.cards[1].Title, "ended") || len(w.cards[1].Fields) == 0 || !strings.Contains(w.cards[1].Fields[0].Value, "Hunter") {
		t.Fatalf("the end card should list the leaders: %d cards", len(w.cards))
	}
	if rr := w.call(w.a.handleStopRPBoost, http.MethodPost, stopPath, w.f.OwnerDiscordID, nil, map[string]string{"boostID": fmt.Sprint(boost.ID)}); rr.Code != http.StatusNotFound {
		t.Fatalf("a window is stopped once: %d", rr.Code)
	}
}

func TestDoubleRPSchedulingAndRules(t *testing.T) {
	w := newRPBoostWorld(t, true)
	now := time.Now().UTC()
	later := now.Add(3 * time.Hour)

	boost, code, body := w.start(2, &later)
	if code != http.StatusOK || boost.Status != repository.RPBoostScheduled {
		t.Fatalf("schedule: %d %s %+v", code, body, boost)
	}
	if len(w.cards) != 1 || !strings.Contains(w.cards[0].Title, "coming") {
		t.Fatalf("a scheduled window is announced ahead: %d cards", len(w.cards))
	}
	// Overlapping another window, too long, or too far ahead is refused.
	overlap := later.Add(time.Hour)
	if _, code, _ := w.start(2, &overlap); code != http.StatusBadRequest {
		t.Fatalf("overlap must be refused: %d", code)
	}
	if _, code, _ := w.start(repository.RPBoostMaxHours+1, nil); code != http.StatusBadRequest {
		t.Fatalf("too long must be refused: %d", code)
	}
	far := now.Add(15 * 24 * time.Hour)
	if _, code, _ := w.start(1, &far); code != http.StatusBadRequest {
		t.Fatalf("too far ahead must be refused: %d", code)
	}
	// A kill before the window starts is not doubled.
	if a := w.award(now.Add(time.Minute)); a.Amount != 100 {
		t.Fatalf("before a scheduled window, normal RP: %+v", a)
	}
	// When the scheduler passes the start, the live card goes out once.
	w.a.runRPBoostAnnouncements(context.Background(), w.guildID, later.Add(time.Minute))
	w.a.runRPBoostAnnouncements(context.Background(), w.guildID, later.Add(2*time.Minute))
	if len(w.cards) != 2 || !strings.Contains(w.cards[1].Title, "is live") {
		t.Fatalf("one live card at the start: %d cards", len(w.cards))
	}
	w.a.runRPBoostAnnouncements(context.Background(), w.guildID, later.Add(3*time.Hour))
	if len(w.cards) != 3 || !strings.Contains(w.cards[2].Title, "ended") || !strings.Contains(w.cards[2].Fields[0].Value, "No ranked kills") {
		t.Fatalf("one end card at the end: %d cards", len(w.cards))
	}

	// A cancelled scheduled window announces nothing more.
	next := now.Add(24 * time.Hour)
	cancelled, _, _ := w.start(1, &next)
	extra := map[string]string{"boostID": fmt.Sprint(cancelled.ID)}
	if rr := w.call(w.a.handleStopRPBoost, http.MethodPost, w.path("/ranked/boosts/x/stop"), w.f.OwnerDiscordID, nil, extra); rr.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rr.Code, rr.Body.String())
	}
	cards := len(w.cards)
	w.a.runRPBoostAnnouncements(context.Background(), w.guildID, next.Add(2*time.Hour))
	if len(w.cards) != cards {
		t.Fatal("a cancelled window gets no live or end card")
	}

	// The list shows the windows; a player cannot change anything.
	list := decodeBody[map[string]any](t, w.call(w.a.handleRPBoosts, http.MethodGet, w.path("/ranked/boosts"), w.f.OwnerDiscordID, nil, nil))
	if len(list["boosts"].([]any)) < 2 || list["season"] == nil {
		t.Fatalf("list: %v", list)
	}
	stranger := syncUser(t, w.a, fmt.Sprintf("rp-stranger-%d", time.Now().UnixNano()), "Stranger")
	if rr := w.call(w.a.handleCreateRPBoost, http.MethodPost, w.path("/ranked/boosts"), stranger.DiscordUserID, createRPBoostRequest{Hours: 1}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("a non-staff user must not start double RP: %d", rr.Code)
	}
}

func TestDoubleRPNeedsARankedSeason(t *testing.T) {
	w := newRPBoostWorld(t, false)
	if _, code, body := w.start(1, nil); code != http.StatusBadRequest || !strings.Contains(body, "ranked season") {
		t.Fatalf("without a season double RP is refused with a reason: %d %s", code, body)
	}
}
