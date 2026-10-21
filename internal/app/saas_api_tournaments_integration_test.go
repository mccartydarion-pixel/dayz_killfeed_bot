//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

// Tournament mode over the real routes, a real PostgreSQL and the real kill pipeline entry point
// (docs/TOURNAMENTS.md): the owner creates and opens a tournament, players join and check in, the
// bracket is drawn, kills persisted through the persistence queue become rounds, a flagged kill
// waits for an admin, prizes are paid once, the title is written and the public shape is served.

type tournamentWorld struct {
	*factionWorld
	svc   *tournament.Service
	notes *tournamentNotes
	now   time.Time
	queue *killfeed.PersistenceQueue
	ctx   context.Context
	f     installationFixture
	guild int64
	srv   int64
}

type tournamentNotes struct {
	mu     sync.Mutex
	events []tournament.Event
}

func (n *tournamentNotes) Notify(_ context.Context, _ *tournament.Tournament, ev []tournament.Event) {
	n.mu.Lock()
	n.events = append(n.events, ev...)
	n.mu.Unlock()
}

func (n *tournamentNotes) count(k tournament.EventKind) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, e := range n.events {
		if e.Kind == k {
			c++
		}
	}
	return c
}

func newTournamentWorld(t *testing.T) *tournamentWorld {
	t.Helper()
	w := newFactionWorld(t)
	a := w.a
	pool := a.DB.Pool
	a.Players = repository.NewPlayerRepository(pool)
	a.Kills = repository.NewKillRepository(pool)
	a.Deaths = repository.NewDeathRepository(pool)
	a.Ranked = repository.NewRankedRepository(pool)
	a.Servers = repository.NewServerRepository(pool)
	a.Stadium = repository.NewStadiumRepository(pool)
	a.ClientAdmin = repository.NewClientAdminRepository(pool)
	a.AdminAudit = repository.NewAuditRepository(pool)
	a.Tournaments = repository.NewTournamentRepository(pool)
	a.TournamentService = a.newTournamentService()
	a.registerTournamentRoutes()
	tw := &tournamentWorld{factionWorld: w, svc: a.TournamentService, notes: &tournamentNotes{}, now: time.Now().UTC().Truncate(time.Second), ctx: context.Background(), f: w.a1}
	tw.svc.SetNotifier(tw.notes)
	tw.svc.SetClock(func() time.Time { return tw.now }, func() int64 { return 7 })
	tw.guild, tw.srv = w.gameContext(w.a1)
	adapter := &persistenceStoreAdapter{players: a.Players, kills: a.Kills, deaths: a.Deaths, tournaments: tw.svc}
	tw.queue = killfeed.NewPersistenceQueueWithServerID(adapter, tw.guild, tw.srv, "session")
	tw.queue.SetKillPostProcessor(adapter)
	tw.queue.SetDeathPostProcessor(adapter)
	ctx, cancel := context.WithCancel(context.Background())
	go tw.queue.Run(ctx)
	t.Cleanup(cancel)
	return tw
}

func (w *tournamentWorld) ownerPath(suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/tournaments%s", w.f.OrgID, w.f.InstallationID, suffix)
}

func (w *tournamentWorld) playerPath(suffix string) string {
	return fmt.Sprintf("/api/saas/player/servers/%d/tournaments%s", w.f.InstallationID, suffix)
}

func (w *tournamentWorld) publicPath() string {
	return fmt.Sprintf("/api/saas/network/servers/%d/tournament", w.f.InstallationID)
}

func tournamentOf(t *testing.T, r *apiResult) map[string]any {
	t.Helper()
	tr, _ := r.JSON(t)["tournament"].(map[string]any)
	if tr == nil {
		t.Fatalf("no tournament in the response: %s", r.Body)
	}
	return tr
}

// fighter is a linked player: the website user, the players row and its DayZ id for the feed.
type fighter struct {
	discord  string
	playerID int64
	dayzID   string
	name     string
}

func (w *tournamentWorld) fighter(i int, name string) fighter {
	w.t.Helper()
	id := w.linkPlayer(w.f, w.players[i], name)
	var dz string
	if err := w.a.DB.Pool.QueryRow(w.ctx, `SELECT dayz_player_id FROM players WHERE id=$1`, id).Scan(&dz); err != nil {
		w.t.Fatal(err)
	}
	return fighter{discord: w.players[i], playerID: id, dayzID: dz, name: name}
}

// kill runs a kill through the real persistence queue (the pipeline's entry point).
func (w *tournamentWorld) kill(killer, victim fighter, weapon string, x, z float64, tod string) {
	w.t.Helper()
	d := 42.0
	ev := &killfeed.Event{Type: killfeed.EventPlayerKill, TimeOfDay: tod, Timestamp: w.now, Dead: true, Weapon: weapon, Distance: &d,
		Killer: &killfeed.PlayerRef{Name: killer.name, ID: killer.dayzID, Position: &killfeed.Position{X: x, Y: z, Z: 100}},
		Victim: &killfeed.PlayerRef{Name: victim.name, ID: victim.dayzID}}
	if err := w.queue.EnqueueAndWait(w.ctx, ev); err != nil {
		w.t.Fatal(err)
	}
}

func (w *tournamentWorld) createBody(name string, teamSize int, extra map[string]any) map[string]any {
	body := map[string]any{"name": name, "teamSize": teamSize, "bracketSize": 4, "bestOf": 1, "seeding": "RANKED",
		"startsAt": w.now.Add(2 * time.Hour).Format(time.RFC3339), "checkinMinutes": 30,
		"rules":  map[string]any{"allowedWeapons": []string{"M4-A1"}, "arenas": []map[string]any{{"no": 1, "name": "Arena 1", "x": 1000, "z": 1000, "radius": 200}}, "matchTimerMinutes": 10, "readyMinutes": 2},
		"prizes": []map[string]any{{"place": 1, "points": 1000, "title": "Friday Champion"}, {"place": 2, "points": 500}}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (w *tournamentWorld) balance(playerID int64) int64 {
	w.t.Helper()
	svc := economy.NewService(repository.NewEconomyRepository(w.a.DB.Pool), nil)
	b, err := svc.Balance(w.ctx, w.guild, playerID)
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

func TestTournamentLifecycle1v1(t *testing.T) {
	w := newTournamentWorld(t)
	owner := w.f.OwnerDiscordID

	// --- authorization: the owner's routes need OWNER/ADMIN of the organization.
	w.expect(w.do(http.MethodGet, w.ownerPath(""), "", nil), http.StatusUnauthorized, "no acting user")
	w.expect(w.do(http.MethodGet, w.ownerPath(""), w.member, nil), http.StatusForbidden, "member")
	w.expect(w.do(http.MethodGet, w.ownerPath(""), w.players[0], nil), http.StatusForbidden, "player")
	w.expect(w.do(http.MethodGet, w.ownerPath(""), w.b1.OwnerDiscordID, nil), http.StatusForbidden, "other organization's owner")
	w.expect(w.do(http.MethodPost, w.ownerPath(""), w.admin, map[string]any{"name": ""}), http.StatusBadRequest, "validation")

	// --- create and open.
	created := tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(""), owner, w.createBody("Friday Night 1v1", 1, nil)), http.StatusCreated, "create"))
	tid := int64(created["id"].(float64))
	if created["status"] != "DRAFT" || created["bracketSize"] != float64(4) || len(created["rules"].(map[string]any)["arenas"].([]any)) != 1 || len(created["flags"].([]any)) != 0 {
		t.Fatalf("created: %v", created)
	}
	tp := fmt.Sprintf("/%d", tid)
	list := w.expect(w.do(http.MethodGet, w.ownerPath(""), w.admin, nil), http.StatusOK, "list").JSON(t)
	if items := list["items"].([]any); len(items) != 1 || int64(items[0].(map[string]any)["id"].(float64)) != tid {
		t.Fatalf("list: %v", list)
	}
	// A view-as session reads but never writes.
	viewAs := func(method, suffix string) int {
		req, _ := http.NewRequest(method, w.base+w.ownerPath(suffix), nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, owner)
		req.Header.Set(impersonatorHeader, adminFounderID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if viewAs(http.MethodPost, tp+"/open") != http.StatusForbidden || viewAs(http.MethodGet, tp) != http.StatusOK {
		t.Fatal("view-as must read only")
	}
	patched := tournamentOf(t, w.expect(w.do(http.MethodPatch, w.ownerPath(tp), owner, map[string]any{"name": "Friday Night"}), http.StatusOK, "patch"))
	if patched["name"] != "Friday Night" || len(patched["rules"].(map[string]any)["arenas"].([]any)) != 1 {
		t.Fatalf("patch keeps the arenas: %v", patched)
	}
	opened := tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(tp+"/open"), owner, nil), http.StatusOK, "open"))
	if opened["status"] != "SIGNUP" {
		t.Fatalf("open: %v", opened["status"])
	}
	w.expect(w.do(http.MethodPost, w.ownerPath(tp+"/open"), owner, nil), http.StatusConflict, "open twice")

	// --- players join: a verified link is required, the bracket holds four.
	fighters := []fighter{w.fighter(0, "Ceiyxe"), w.fighter(1, "Varg"), w.fighter(2, "Kestrel"), w.fighter(3, "Saint")}
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), w.players[4], nil), http.StatusConflict, "unlinked player")
	if code := w.do(http.MethodPost, w.playerPath(tp+"/join"), w.players[4], nil).errCode(t); code != codePlayerIdentityRequired {
		t.Fatalf("unlinked code %s", code)
	}
	for _, f := range fighters {
		r := w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), f.discord, nil), http.StatusOK, "join "+f.name)
		if me, _ := r.JSON(t)["me"].(map[string]any); me == nil || me["checkedIn"] != false {
			t.Fatalf("join reply: %s", r.Body)
		}
	}
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), fighters[0].discord, nil), http.StatusConflict, "join twice")
	fifth := w.fighter(5, "Ghost")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), fifth.discord, nil), http.StatusConflict, "bracket full")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/checkin"), fighters[0].discord, nil), http.StatusConflict, "check-in before it opens")
	mine := w.expect(w.do(http.MethodGet, w.playerPath(""), fighters[0].discord, nil), http.StatusOK, "player view").JSON(t)
	if mine["linked"] != true || mine["current"] == nil || mine["me"].(map[string]any)["status"] != "ACTIVE" {
		t.Fatalf("player view: %v", mine)
	}

	// --- the clock opens check-in; everyone checks in; the owner starts.
	w.now = w.now.Add(95 * time.Minute)
	w.svc.Tick(w.ctx)
	if got := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "get")); got["status"] != "CHECKIN" {
		t.Fatalf("after the tick: %v", got["status"])
	}
	for _, f := range fighters {
		w.expect(w.do(http.MethodPost, w.playerPath(tp+"/checkin"), f.discord, nil), http.StatusOK, "check in "+f.name)
	}
	live := tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(tp+"/start"), owner, nil), http.StatusOK, "start"))
	matches := live["matches"].([]any)
	if live["status"] != "LIVE" || len(matches) != 3 || live["current"] == nil || live["startedAt"] == nil {
		t.Fatalf("live: %v", live)
	}
	first := matches[0].(map[string]any)
	if first["status"] != "CALLED" || first["arena"] != float64(1) || first["roundName"] != "Semi-final" {
		t.Fatalf("first match: %v", first)
	}
	if w.notes.count(tournament.EvStarted) != 1 || w.notes.count(tournament.EvMatchCalled) != 1 {
		t.Fatalf("events: %+v", w.notes.events)
	}

	// --- kills through the pipeline. Who plays whom comes from the bracket.
	byID := map[int64]fighter{}
	for _, f := range fighters {
		byID[f.playerID] = f
	}
	entryPlayer := func(tr map[string]any, entryID int64) fighter {
		for _, e := range tr["entries"].([]any) {
			em := e.(map[string]any)
			if int64(em["id"].(float64)) == entryID {
				return byID[int64(em["players"].([]any)[0].(map[string]any)["playerId"].(float64))]
			}
		}
		t.Fatalf("entry %d not found", entryID)
		return fighter{}
	}
	a1, b1 := entryPlayer(live, int64(first["a"].(float64))), entryPlayer(live, int64(first["b"].(float64)))
	// Someone outside the match killing a match player is interference (a flagged round and a
	// ping); the kill between the two players decides the best-of-one.
	w.kill(fifth, a1, "M4-A1", 1000, 1000, "19:00:01")
	w.kill(a1, b1, "M4-A1", 1050, 1000, "19:00:02")
	after := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "after match 1"))
	m1 := after["matches"].([]any)[0].(map[string]any)
	r1 := m1["rounds"].([]any)
	if m1["status"] != "DONE" || m1["winner"] != first["a"] || len(r1) != 2 || r1[0].(map[string]any)["flag"] != "INTERFERENCE" || r1[0].(map[string]any)["counted"] != false || r1[1].(map[string]any)["counted"] != true {
		t.Fatalf("match 1 after the kills: %v", m1)
	}
	if w.notes.count(tournament.EvAdminPing) != 1 {
		t.Fatalf("interference pings an admin: %d", w.notes.count(tournament.EvAdminPing))
	}
	m2 := after["matches"].([]any)[1].(map[string]any)
	if m2["status"] != "CALLED" || after["current"].(map[string]any)["matchId"] != m2["id"] {
		t.Fatalf("match 2 must be called: %v", m2)
	}
	// Match 2: a kill with a weapon that is not allowed is a flagged round waiting for an admin.
	a2, b2 := entryPlayer(live, int64(m2["a"].(float64))), entryPlayer(live, int64(m2["b"].(float64)))
	w.kill(b2, a2, "KA-M", 1000, 1000, "19:05:00")
	flagged := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "flagged"))
	flags := flagged["flags"].([]any)
	m2 = flagged["matches"].([]any)[1].(map[string]any)
	if len(flags) != 1 || flags[0].(map[string]any)["flag"] != "WEAPON" || m2["status"] != "CALLED" || m2["scoreB"] != float64(0) {
		t.Fatalf("flagged: %v %v", flags, m2)
	}
	if w.notes.count(tournament.EvAdminPing) != 2 {
		t.Fatalf("admin pings: %d", w.notes.count(tournament.EvAdminPing))
	}
	// The same kill replayed through the pipeline (a duplicate insert) changes nothing.
	w.kill(b2, a2, "KA-M", 1000, 1000, "19:05:00")
	if again := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "dup")); len(again["matches"].([]any)[1].(map[string]any)["rounds"].([]any)) != 1 {
		t.Fatal("a replayed kill must not add a round")
	}
	// A kill outside the arena is flagged too; one by the right weapon inside it counts.
	w.kill(a2, b2, "M4-A1", 5000, 5000, "19:06:00")
	w.kill(a2, b2, "M4-A1", 1100, 1000, "19:07:00")
	decided := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "decided"))
	m2 = decided["matches"].([]any)[1].(map[string]any)
	rounds := m2["rounds"].([]any)
	if m2["status"] != "DONE" || len(rounds) != 3 || rounds[1].(map[string]any)["flag"] != "OUTSIDE_ARENA" || rounds[2].(map[string]any)["counted"] != true || int64(m2["winner"].(float64)) != int64(m2["a"].(float64)) {
		t.Fatalf("match 2 decided: %v", m2)
	}
	if len(decided["flags"].([]any)) != 0 {
		t.Fatal("flags of a finished match no longer need a ruling")
	}
	// The final is called; an admin decides it.
	final := decided["matches"].([]any)[2].(map[string]any)
	if final["status"] != "CALLED" || final["a"] != first["a"] || final["b"] != m2["a"] {
		t.Fatalf("final: %v", final)
	}
	fid := int64(final["id"].(float64))
	w.expect(w.do(http.MethodPost, w.ownerPath(fmt.Sprintf("%s/matches/%d/result", tp, fid)), owner, map[string]any{"winnerEntryId": 999999}), http.StatusConflict, "result for a stranger")
	done := tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(fmt.Sprintf("%s/matches/%d/result", tp, fid)), owner, map[string]any{"winnerEntryId": final["a"], "note": "Varg left"}), http.StatusOK, "result"))
	if done["status"] != "FINISHED" || done["finishedAt"] == nil || done["champion"] == nil || done["current"] != nil {
		t.Fatalf("finished: %v", done)
	}
	champ := done["champion"].(map[string]any)
	if champ["title"] != "Friday Champion" || int64(champ["entryId"].(float64)) != int64(first["a"].(float64)) {
		t.Fatalf("champion: %v", champ)
	}
	if w.notes.count(tournament.EvFinished) != 1 {
		t.Fatal("one finished event")
	}

	// --- prizes once, the title written, the stats and card routes show it.
	winner, runnerUp := a1, entryPlayer(live, int64(m2["a"].(float64)))
	if w.balance(winner.playerID) != 1000 || w.balance(runnerUp.playerID) != 500 {
		t.Fatalf("prizes: %d %d", w.balance(winner.playerID), w.balance(runnerUp.playerID))
	}
	var payouts, txs int
	if err := w.a.DB.Pool.QueryRow(w.ctx, `SELECT (SELECT COUNT(*) FROM tournament_payouts WHERE tournament_id=$1), (SELECT COUNT(*) FROM point_transactions WHERE guild_id=$2 AND player_id=$3)`, tid, w.guild, winner.playerID).Scan(&payouts, &txs); err != nil {
		t.Fatal(err)
	}
	if payouts != 2 || txs != 1 {
		t.Fatalf("payouts %d transactions %d", payouts, txs)
	}
	// Settling again (a retried notification, a restart) pays nothing twice.
	full, err := w.a.Tournaments.Get(w.ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	w.svc.SettlePrizes(w.ctx, full)
	if w.balance(winner.playerID) != 1000 {
		t.Fatal("prizes must be paid once")
	}
	var title string
	if err := w.a.DB.Pool.QueryRow(w.ctx, `SELECT title FROM tournament_titles WHERE installation_id=$1 AND player_id=$2`, w.f.InstallationID, winner.playerID).Scan(&title); err != nil || title != "Friday Champion" {
		t.Fatalf("title: %q %v", title, err)
	}
	stats := w.expect(w.do(http.MethodGet, w.playerStatsPath(w.f.InstallationID), winner.discord, nil), http.StatusOK, "stats").JSON(t)
	if stats["tournamentTitle"] != "Friday Champion" {
		t.Fatalf("stats title: %v", stats["tournamentTitle"])
	}
	card := w.expect(w.do(http.MethodGet, w.cardPath(w.f.InstallationID, ""), winner.discord, nil), http.StatusOK, "card").JSON(t)
	if card["tournamentTitle"] != "Friday Champion" {
		t.Fatalf("card title: %v", card["tournamentTitle"])
	}
	past := w.expect(w.do(http.MethodGet, w.playerPath(""), winner.discord, nil), http.StatusOK, "player view after").JSON(t)
	if past["current"] != nil || len(past["past"].([]any)) != 1 || past["past"].([]any)[0].(map[string]any)["myPlace"] != float64(1) {
		t.Fatalf("past: %v", past)
	}

	// --- the public shape.
	pub := w.expect(w.do(http.MethodGet, w.publicPath(), "", nil), http.StatusOK, "public").JSON(t)
	for _, k := range []string{"installationId", "server", "tournament", "fighters", "generatedAt"} {
		if _, ok := pub[k]; !ok {
			t.Fatalf("public shape is missing %q: %v", k, pub)
		}
	}
	server := pub["server"].(map[string]any)
	for _, k := range []string{"name", "platform", "map", "discordInvite", "onlinePlayers"} {
		if _, ok := server[k]; !ok {
			t.Fatalf("server block is missing %q", k)
		}
	}
	pt := pub["tournament"].(map[string]any)
	keys := make([]string, 0, len(pt))
	for k := range pt {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"bestOf", "bracketSize", "champion", "checkinMinutes", "current", "entries", "finishedAt", "format", "id", "matches", "name", "prizes", "results", "rules", "seeding", "signupOpensAt", "startedAt", "startsAt", "status", "teamSize"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("tournament keys %v", keys)
	}
	// The owner's extras never leak into the public shape.
	for _, k := range []string{"flags", "discordChannelId"} {
		if _, ok := pt[k]; ok {
			t.Fatalf("public shape must not carry %q", k)
		}
	}
	fighterStats := pub["fighters"].(map[string]any)
	fw := fighterStats[fmt.Sprint(winner.playerID)].(map[string]any)
	if len(fighterStats) != 4 || fw["favouriteWeapon"] != "M4-A1" || fw["kills"] != float64(1) || fw["record"].(map[string]any)["wins"] != float64(2) {
		t.Fatalf("fighters: %v", fighterStats)
	}
	if pt["entries"].([]any)[0].(map[string]any)["players"].([]any)[0].(map[string]any)["rankTier"] != "UNRANKED" {
		t.Fatalf("entries: %v", pt["entries"])
	}
	if r := w.do(http.MethodGet, "/api/saas/network/servers/999999999/tournament", "", nil); r.Status != http.StatusNotFound {
		t.Fatalf("unknown installation: %d", r.Status)
	}
	// The whole thing is one audited trail of owner actions.
	var audits int
	if err := w.a.DB.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE action LIKE 'TOURNAMENT_%'`).Scan(&audits); err == nil && audits < 4 {
		t.Fatalf("audits: %d", audits)
	}
}

func TestTournamentLifecycle2v2AndCancel(t *testing.T) {
	w := newTournamentWorld(t)
	owner := w.f.OwnerDiscordID
	created := tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(""), owner, w.createBody("Duo Night", 2, map[string]any{"checkinMinutes": 0, "seeding": "RANDOM"})), http.StatusCreated, "create 2v2"))
	tid := int64(created["id"].(float64))
	tp := fmt.Sprintf("/%d", tid)
	w.expect(w.do(http.MethodPost, w.ownerPath(tp+"/open"), owner, nil), http.StatusOK, "open")
	a1, a2, b1, b2 := w.fighter(0, "Tusk"), w.fighter(1, "Lynx"), w.fighter(2, "Bishop"), w.fighter(3, "Raven")
	// A 2v2 entry needs a partner with a verified link.
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), a1.discord, nil), http.StatusConflict, "no partner")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), a1.discord, map[string]any{"partnerDiscordId": w.players[4]}), http.StatusBadRequest, "unlinked partner")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), a1.discord, map[string]any{"partnerDiscordId": a2.discord}), http.StatusOK, "team A")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), a2.discord, map[string]any{"partnerDiscordId": b1.discord}), http.StatusConflict, "partner already entered")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), b1.discord, map[string]any{"partnerDiscordId": b2.discord}), http.StatusOK, "team B")
	// The partner can withdraw the team, and the team can come back.
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/leave"), b2.discord, nil), http.StatusOK, "leave")
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), b1.discord, map[string]any{"partnerDiscordId": b2.discord}), http.StatusOK, "team B again")
	// No check-in window: the clock starts the tournament at the start time.
	w.now = w.now.Add(2 * time.Hour)
	w.svc.Tick(w.ctx)
	live := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "live"))
	if live["status"] != "LIVE" || len(live["matches"].([]any)) != 3 {
		t.Fatalf("live: %v %d", live["status"], len(live["matches"].([]any)))
	}
	// With two entries in a four bracket the semi-finals are byes and the final is called.
	cur := live["current"].(map[string]any)
	final := live["matches"].([]any)[2].(map[string]any)
	if cur["matchId"] != final["id"] || final["status"] != "CALLED" {
		t.Fatalf("final must be called: %v", final)
	}
	// A kill between team-mates is interference; a kill on the other team wins the round and,
	// best of one, the match and the title.
	w.kill(a1, a2, "M4-A1", 1000, 1000, "20:00:01")
	w.kill(a2, b1, "M4-A1", 1000, 1000, "20:00:02")
	done := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "done"))
	rounds := done["matches"].([]any)[2].(map[string]any)["rounds"].([]any)
	if done["status"] != "FINISHED" || len(rounds) != 2 || rounds[0].(map[string]any)["flag"] != "INTERFERENCE" || rounds[1].(map[string]any)["counted"] != true {
		t.Fatalf("2v2 finished: %v %v", done["status"], rounds)
	}
	champ := done["champion"].(map[string]any)
	if len(champ["players"].([]any)) != 2 {
		t.Fatalf("champion team: %v", champ)
	}
	// Both players of the winning team are paid; the losing team gets second place.
	for _, f := range []fighter{a1, a2} {
		if w.balance(f.playerID) != 1000 {
			t.Fatalf("%s balance %d", f.name, w.balance(f.playerID))
		}
	}
	for _, f := range []fighter{b1, b2} {
		if w.balance(f.playerID) != 500 {
			t.Fatalf("%s balance %d", f.name, w.balance(f.playerID))
		}
	}
	var payouts int
	if err := w.a.DB.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM tournament_payouts WHERE tournament_id=$1`, tid).Scan(&payouts); err != nil || payouts != 4 {
		t.Fatalf("payouts %d %v", payouts, err)
	}

	// --- a tournament nobody checks in for is cancelled by the clock.
	w.notes.events = nil
	created = tournamentOf(t, w.expect(w.do(http.MethodPost, w.ownerPath(""), owner, w.createBody("Ghost Town", 1, map[string]any{"startsAt": w.now.Add(time.Hour).Format(time.RFC3339), "signupOpensAt": w.now.Add(-time.Minute).Format(time.RFC3339)})), http.StatusCreated, "create"))
	tp = fmt.Sprintf("/%d", int64(created["id"].(float64)))
	w.svc.Tick(w.ctx) // sign-up opens on its own
	if got := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "opened")); got["status"] != "SIGNUP" {
		t.Fatalf("auto open: %v", got["status"])
	}
	w.expect(w.do(http.MethodPost, w.playerPath(tp+"/join"), a1.discord, nil), http.StatusOK, "join")
	w.now = w.now.Add(time.Hour)
	w.svc.Tick(w.ctx)
	if got := tournamentOf(t, w.expect(w.do(http.MethodGet, w.ownerPath(tp), owner, nil), http.StatusOK, "cancelled")); got["status"] != "CANCELLED" {
		t.Fatalf("auto cancel: %v", got["status"])
	}
	if w.notes.count(tournament.EvCancelled) != 1 {
		t.Fatal("one cancelled event")
	}
	// The public shape shows the cancelled one (it ended within the last day) with a null champion.
	pub := w.expect(w.do(http.MethodGet, w.publicPath(), "", nil), http.StatusOK, "public").JSON(t)
	raw, _ := json.Marshal(pub["tournament"])
	var pt map[string]any
	_ = json.Unmarshal(raw, &pt)
	if pt == nil || pt["champion"] != nil {
		t.Fatalf("public after cancel: %v", pt)
	}
}
