//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// bountyDiscord is an in-memory Discord for the bounty feeds: it is both the
// route-panel API (persistent board) and the plain sender (tracking cards), and
// records what was posted where. failSend makes every plain send fail.
type bountyDiscord struct {
	mu       sync.Mutex
	next     int
	live     map[string]bool // "channel/id"
	text     map[string]string
	sends    int
	edits    int
	cards    map[string][]string // tracking cards per channel
	failSend bool
}

func newBountyDiscord() *bountyDiscord {
	return &bountyDiscord{live: map[string]bool{}, text: map[string]string{}, cards: map[string][]string{}}
}

func (d *bountyDiscord) ChannelMessageSendComplex(ch string, embed *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	id := fmt.Sprintf("m%d", d.next)
	d.live[ch+"/"+id] = true
	d.text[ch] = embed.Description
	d.sends++
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}

func (d *bountyDiscord) ChannelMessageEditComplex(ch, id string, embed *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live[ch+"/"+id] {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	d.text[ch] = embed.Description
	d.edits++
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}

func (d *bountyDiscord) ChannelMessage(ch, id string) (*discordgo.Message, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live[ch+"/"+id] {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}

func (d *bountyDiscord) ChannelMessageDelete(ch, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.live, ch+"/"+id)
	return nil
}

// ChannelMessageSendComplex for the tracker (discordgo.MessageSend form) lives on a
// separate adapter type so both signatures can coexist.
type trackingSender struct{ d *bountyDiscord }

func (s trackingSender) ChannelMessageSendComplex(ch string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	s.d.mu.Lock()
	defer s.d.mu.Unlock()
	if s.d.failSend {
		return nil, errors.New("discord unavailable")
	}
	for _, e := range data.Embeds {
		s.d.cards[ch] = append(s.d.cards[ch], e.Description)
	}
	return &discordgo.Message{ID: "t", ChannelID: ch}, nil
}

func (d *bountyDiscord) liveIn(ch string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for k := range d.live {
		if strings.HasPrefix(k, ch+"/") {
			n++
		}
	}
	return n
}

func (d *bountyDiscord) boardText(ch string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.text[ch]
}

func (d *bountyDiscord) cardsIn(ch string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.cards[ch]...)
}

type killRecorder struct {
	mu    sync.Mutex
	kills []*killfeed.Event
}

func (k *killRecorder) PublishKill(ev *killfeed.Event) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kills = append(k.kills, ev)
	return nil
}

func (k *killRecorder) all() []*killfeed.Event {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*killfeed.Event(nil), k.kills...)
}

// bountyWorld is a guild with two servers, real routes, the real resolver, the
// real persistence adapter and the real bounty service, plus recording Discord fakes.
type bountyWorld struct {
	t                *testing.T
	a                *App
	verifier         *fakeDiscordVerifier
	fixture          installationFixture
	installB         int64
	guildRowID       int64
	serverA, serverB int64
	target, hunter   int64
	d                *bountyDiscord
	tracker          *discord.BountyTracker
	board            *discord.BountyBoard
	panelStore       discord.RoutePanelStore
	kills            *killRecorder
	adapter          *persistenceStoreAdapter
	servers          discord.GuildServersFunc
	ctx              context.Context
}

// newBountyWorld wires everything. routes maps a route key to a channel for server
// A and B respectively (nil = leave that server without the route).
func newBountyWorld(t *testing.T, routesA, routesB map[string]string) *bountyWorld {
	t.Helper()
	a, verifier := saasIntegrationApp(t)
	a.ChannelRoutes = routing.NewResolver(a.SaaSChannelRoutes, time.Hour) // only invalidation can explain immediacy
	a.GuildRoutePanels = repository.NewGuildRoutePanelRepository(a.DB.Pool)
	a.Bounties = repository.NewBountyRepository(a.DB.Pool)
	a.BountyService = bounties.NewService(a.Bounties, nil)
	withFakeNitradoServer(t, a, http.StatusOK, mixedServicesJSON)
	fixture := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixture.OrgID, fixture.OwnerDiscordID)
	for _, id := range []string{"kf-A", "kf-B", "board-A", "board-B", "board-A2", "track-A", "track-B"} {
		seedGuildChannels(verifier, fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	w := &bountyWorld{t: t, a: a, verifier: verifier, fixture: fixture, ctx: context.Background(), d: newBountyDiscord(), kills: &killRecorder{}}
	w.serverA = decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, fixture.InstallationID, 111111, fixture.OwnerDiscordID)).Server.ID
	w.installB = createSecondInstallation(t, a, fixture)
	w.serverB = decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixture.OrgID, w.installB, 222222, fixture.OwnerDiscordID)).Server.ID
	w.guildRowID = mustGuildRowID(t, a, fixture.DiscordGuildID)
	w.save(fixture.InstallationID, "kf-A", routesA)
	w.save(w.installB, "kf-B", routesB)

	w.servers = func(context.Context) (int64, []int64, error) { return w.guildRowID, []int64{w.serverA, w.serverB}, nil }
	w.tracker = discord.NewBountyTracker(trackingSender{w.d}, a.ChannelRoutes, w.servers)
	w.panelStore = discord.NewRoutePanelStore(a.GuildRoutePanels)
	w.board = discord.NewBountyBoard(a.ChannelRoutes, w.servers, discord.NewRoutePanels(w.d, w.panelStore), a.Bounties)
	a.BountyBoard = w.board
	a.BountyService.SetNotifier(discord.BountyEvents{Tracker: w.tracker, Board: w.board})

	players := repository.NewPlayerRepository(a.DB.Pool)
	var err error
	w.target, err = players.UpsertPlayer(w.ctx, w.guildRowID, "dz-target", "Target", time.Now())
	must(t, err)
	w.hunter, err = players.UpsertPlayer(w.ctx, w.guildRowID, "dz-hunter", "Hunter", time.Now())
	must(t, err)
	w.adapter = &persistenceStoreAdapter{
		players: players, kills: repository.NewKillRepository(a.DB.Pool), deaths: repository.NewDeathRepository(a.DB.Pool),
		streaks: repository.NewStreakRepository(a.DB.Pool), bounties: a.Bounties, bountySvc: a.BountyService,
	}
	return w
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (w *bountyWorld) save(installationID int64, killfeedChannel string, routes map[string]string) {
	w.t.Helper()
	merged := map[string]string{"KILLFEED": killfeedChannel}
	for k, v := range routes {
		merged[k] = v
	}
	if rr := saveChannelRoutes(w.t, w.a, w.fixture.OrgID, installationID, w.fixture.OwnerDiscordID, merged); rr.Code != http.StatusOK {
		w.t.Fatalf("save routes: %d %s", rr.Code, rr.Body.String())
	}
}

// queue builds a persistence queue + engine for one server, exactly as a worker
// does: kill post-processing (which claims bounties) runs after the durable insert.
func (w *bountyWorld) queue(serverID int64) *killfeed.PersistenceQueue {
	q := killfeed.NewPersistenceQueueWithServerID(w.adapter, w.guildRowID, serverID, "session")
	q.SetKillPostProcessor(w.adapter)
	go q.Run(w.ctx)
	engine := killfeed.NewEngine(nil, "svc", killfeed.NewADMParser())
	engine.SetPersistence(q)
	engine.SetKillPublisher(w.kills)
	return q
}

func killOf(tod string) *killfeed.Event {
	d := 86.4
	return &killfeed.Event{
		Type: killfeed.EventPlayerKill, TimeOfDay: tod, Dead: true, Weapon: "M4-A1", Distance: &d,
		Killer: &killfeed.PlayerRef{Name: "Hunter", ID: "dz-hunter"}, Victim: &killfeed.PlayerRef{Name: "Target", ID: "dz-target"},
	}
}

func (w *bountyWorld) status(playerTarget int64) (active, claimed int64) {
	must(w.t, w.a.DB.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FILTER (WHERE status='ACTIVE'), COUNT(*) FILTER (WHERE status='CLAIMED') FROM bounties WHERE guild_id=$1 AND target_player_id=$2`, w.guildRowID, playerTarget).Scan(&active, &claimed))
	return
}

func (w *bountyWorld) points() (lifetime, txs int64) {
	must(w.t, w.a.DB.Pool.QueryRow(w.ctx, `SELECT COALESCE((SELECT lifetime_points FROM player_points WHERE guild_id=$1 AND player_id=$2),0),(SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='BOUNTY_CLAIM')`, w.guildRowID, w.hunter).Scan(&lifetime, &txs))
	return
}

// --- the full flow ------------------------------------------------------------------------------

// Persist first -> atomic claim -> commit -> publish. A bounty kill still appears
// on the KILLFEED; the lifecycle feed and the board report the committed state; a
// kill on another server, a suicide and a replay claim nothing.
func TestBountyEndToEndClaimAfterPersistence(t *testing.T) {
	w := newBountyWorld(t,
		map[string]string{"BOUNTY": "board-A", "BOUNTY_TRACKING": "track-A"},
		map[string]string{"BOUNTY": "board-B", "BOUNTY_TRACKING": "track-B"})

	placed, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 250000, PlacedBy: "admin-1"})
	must(t, err)
	if placed.ServerID != w.serverA || placed.RewardPoints != 250000 {
		t.Fatalf("unexpected bounty: %+v", placed)
	}
	w.tracker.Flush()
	w.board.SyncOnce(w.ctx)
	if cards := w.d.cardsIn("track-A"); len(cards) != 1 || !strings.Contains(cards[0], "BOUNTY PLACED") || !strings.Contains(cards[0], "**Target**") || !strings.Contains(cards[0], "250,000 pts") {
		t.Fatalf("expected the placement on server A's tracking feed, got %v", cards)
	}
	if len(w.d.cardsIn("track-B")) != 0 {
		t.Fatal("server B's tracking feed must not see server A's bounty")
	}
	if txt := w.d.boardText("board-A"); !strings.Contains(txt, "🥇 Target • **250,000 pts**") {
		t.Fatalf("server A's board must list the bounty, got %q", txt)
	}
	if txt := w.d.boardText("board-B"); strings.Contains(txt, "Target") {
		t.Fatalf("server B's board must not show A's bounty, got %q", txt)
	}
	boardMsgs := w.d.liveIn("board-A")

	// A kill of the target on SERVER B does not claim A's bounty.
	qB := w.queue(w.serverB)
	must(t, qB.EnqueueAndWait(w.ctx, killOf("10:00:00")))
	if active, claimed := w.status(w.target); active != 1 || claimed != 0 {
		t.Fatalf("a kill on server B must not claim server A's bounty: active=%d claimed=%d", active, claimed)
	}

	// A suicide of the target claims nothing.
	qA := w.queue(w.serverA)
	must(t, qA.EnqueueAndWait(w.ctx, &killfeed.Event{Type: killfeed.EventSuicideAction, TimeOfDay: "10:05:00", Player: &killfeed.PlayerRef{Name: "Target", ID: "dz-target"}, Cause: killfeed.DeathCauseSuicide}))
	if active, claimed := w.status(w.target); active != 1 || claimed != 0 {
		t.Fatalf("a suicide must not claim: active=%d claimed=%d", active, claimed)
	}

	// The PvP kill on server A: persisted, then claimed atomically.
	must(t, qA.EnqueueAndWait(w.ctx, killOf("10:10:00")))
	if active, claimed := w.status(w.target); active != 0 || claimed != 1 {
		t.Fatalf("the bounty must be claimed: active=%d claimed=%d", active, claimed)
	}
	if life, txs := w.points(); life != 250000 || txs != 1 {
		t.Fatalf("expected 250000 points awarded once, got lifetime=%d txs=%d", life, txs)
	}
	// The kill still appears on the KILLFEED (never suppressed), flagged as a bounty kill.
	kills := w.kills.all()
	if len(kills) != 2 {
		t.Fatalf("both PvP kills must reach the KILLFEED, got %d", len(kills))
	}
	if kills[0].BountyClaimed || !kills[1].BountyClaimed || kills[1].BountyPoints != 250000 {
		t.Fatalf("only the server-A kill is a bounty kill: %+v / %+v", kills[0], kills[1])
	}

	// The lifecycle feed and the board report the committed state.
	w.tracker.Flush()
	w.board.SyncOnce(w.ctx)
	cards := w.d.cardsIn("track-A")
	if len(cards) != 2 || cards[1] != "👑 **BOUNTY CLAIMED**\n**Hunter** eliminated **Target**\nReward **250,000 pts**\n`M4-A1` • 86m" {
		t.Fatalf("unexpected claim card: %q", cards)
	}
	if !strings.Contains(w.d.boardText("board-A"), "No active bounties") {
		t.Fatalf("the board must reflect the claim, got %q", w.d.boardText("board-A"))
	}
	if w.d.liveIn("board-A") != boardMsgs {
		t.Fatal("the board must be edited in place, not re-posted")
	}

	// Replay after a "restart": a new queue over the same database sees the same
	// kill; the durable insert is a duplicate, so nothing is claimed or published.
	qA2 := w.queue(w.serverA)
	must(t, qA2.EnqueueAndWait(w.ctx, killOf("10:10:00")))
	w.tracker.Flush()
	if life, txs := w.points(); life != 250000 || txs != 1 {
		t.Fatalf("a replay must not award again, got lifetime=%d txs=%d", life, txs)
	}
	if got := len(w.d.cardsIn("track-A")); got != 2 {
		t.Fatalf("a replay must not announce a second claim, got %d cards", got)
	}
	if len(w.kills.all()) != 2 {
		t.Fatal("a replayed kill must not be published to the KILLFEED again")
	}
}

// L/M: with no BOUNTY and no BOUNTY_TRACKING route everything is still correct in
// the database and Discord is simply not involved.
func TestBountyWorksWithoutAnyRoute(t *testing.T) {
	w := newBountyWorld(t, nil, nil)
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 500, PlacedBy: "a"})
	must(t, err)
	must(t, w.queue(w.serverA).EnqueueAndWait(w.ctx, killOf("11:00:00")))
	w.tracker.Flush()
	w.board.SyncOnce(w.ctx)

	if active, claimed := w.status(w.target); active != 0 || claimed != 1 {
		t.Fatalf("the claim must work without any route: active=%d claimed=%d", active, claimed)
	}
	if life, _ := w.points(); life != 500 {
		t.Fatalf("points must be awarded without any route, got %d", life)
	}
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	if w.d.sends != 0 || len(w.d.cards) != 0 {
		t.Fatalf("no route: nothing may be sent to Discord, sends=%d cards=%v", w.d.sends, w.d.cards)
	}
}

// Only the BOUNTY route: the board works, the (absent) tracking feed is a no-op.
func TestBountyBoardWithoutTrackingRoute(t *testing.T) {
	w := newBountyWorld(t, map[string]string{"BOUNTY": "board-A"}, nil)
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 700, PlacedBy: "a"})
	must(t, err)
	w.tracker.Flush()
	w.board.SyncOnce(w.ctx)
	if !strings.Contains(w.d.boardText("board-A"), "Target • **700 pts**") {
		t.Fatalf("the board must work without a tracking route, got %q", w.d.boardText("board-A"))
	}
	if len(w.d.cardsIn("track-A")) != 0 || len(w.d.cardsIn("track-B")) != 0 {
		t.Fatal("no tracking route: no tracking cards")
	}
}

// P: a Discord failure after the claim leaves the claim committed and never repeats it.
func TestBountyClaimSurvivesDiscordFailure(t *testing.T) {
	w := newBountyWorld(t, map[string]string{"BOUNTY": "board-A", "BOUNTY_TRACKING": "track-A"}, nil)
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 900, PlacedBy: "a"})
	must(t, err)
	w.tracker.Flush()

	w.d.mu.Lock()
	w.d.failSend = true
	w.d.mu.Unlock()
	q := w.queue(w.serverA)
	must(t, q.EnqueueAndWait(w.ctx, killOf("12:00:00")))
	w.tracker.Flush() // the send fails
	w.tracker.Flush()

	if active, claimed := w.status(w.target); active != 0 || claimed != 1 {
		t.Fatalf("the claim must stay committed despite the Discord failure: active=%d claimed=%d", active, claimed)
	}
	if life, txs := w.points(); life != 900 || txs != 1 {
		t.Fatalf("points must be awarded exactly once, got lifetime=%d txs=%d", life, txs)
	}
	if len(w.kills.all()) != 1 || !w.kills.all()[0].BountyClaimed {
		t.Fatal("the kill must still reach the KILLFEED as a bounty kill")
	}
}

// --- board persistence, restart, route change -----------------------------------------------------------

func TestBountyBoardRestartAndRouteChangeAgainstRealStore(t *testing.T) {
	w := newBountyWorld(t, map[string]string{"BOUNTY": "board-A"}, nil)
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 1500, PlacedBy: "a"})
	must(t, err)
	w.board.SyncOnce(w.ctx)
	if w.d.sends != 1 || w.d.liveIn("board-A") != 1 {
		t.Fatalf("expected one board, sends=%d", w.d.sends)
	}
	recorded, err := w.panelStore.List(w.ctx, w.guildRowID, "BOUNTY")
	must(t, err)
	if len(recorded) != 1 || recorded[0].ChannelID != "board-A" {
		t.Fatalf("the board message must be recorded durably, got %+v", recorded)
	}

	// A restart: a brand-new board and panel manager over the SAME database.
	restarted := discord.NewBountyBoard(w.a.ChannelRoutes, w.servers, discord.NewRoutePanels(w.d, discord.NewRoutePanelStore(w.a.GuildRoutePanels)), w.a.Bounties)
	restarted.SyncOnce(w.ctx)
	if w.d.sends != 1 || w.d.liveIn("board-A") != 1 {
		t.Fatalf("a restart must edit the recorded board, not post another: sends=%d", w.d.sends)
	}

	// Route change in-process (1-hour TTL): the board moves, no leftover copy.
	w.save(w.fixture.InstallationID, "kf-A", map[string]string{"BOUNTY": "board-A2"})
	restarted.SyncOnce(w.ctx)
	if w.d.liveIn("board-A") != 0 || w.d.liveIn("board-A2") != 1 || !strings.Contains(w.d.boardText("board-A2"), "Target • **1,500 pts**") {
		t.Fatalf("expected the board moved to board-A2, live A=%d A2=%d", w.d.liveIn("board-A"), w.d.liveIn("board-A2"))
	}
	moved, err := w.panelStore.List(w.ctx, w.guildRowID, "BOUNTY")
	must(t, err)
	if len(moved) != 1 || moved[0].ChannelID != "board-A2" {
		t.Fatalf("expected only the new channel recorded, got %+v", moved)
	}
}

// --- placement validation against the real database ------------------------------------------------------

func TestBountyPlacementIsValidatedAndTenantIsolated(t *testing.T) {
	w := newBountyWorld(t, nil, nil)
	svc := w.a.BountyService
	guild, target := w.guildRowID, w.target

	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, ServerID: w.serverA, TargetPlayerID: target, Amount: 0}); !errors.Is(err, bounties.ErrInvalidAmount) {
		t.Fatalf("amount 0 must be rejected, got %v", err)
	}
	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, ServerID: w.serverA, TargetPlayerID: 987654321, Amount: 5}); !errors.Is(err, bounties.ErrTargetNotFound) {
		t.Fatalf("an unknown target must be rejected, got %v", err)
	}

	// A second, unrelated organization + guild.
	other := buildInstallationFixture(t, w.a, w.verifier)
	connectNitrado(t, w.a, other.OrgID, other.OwnerDiscordID)
	otherServer := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, w.a, other.OrgID, other.InstallationID, 111111, other.OwnerDiscordID)).Server.ID
	otherGuild := mustGuildRowID(t, w.a, other.DiscordGuildID)

	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, ServerID: otherServer, TargetPlayerID: target, Amount: 5}); !errors.Is(err, bounties.ErrServerNotInGuild) {
		t.Fatalf("another guild's server must be rejected, got %v", err)
	}
	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: otherGuild, ServerID: otherServer, TargetPlayerID: target, Amount: 5}); !errors.Is(err, bounties.ErrTargetNotFound) {
		t.Fatalf("a target from another guild must be rejected, got %v", err)
	}
	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, ServerID: w.serverA, OrganizationID: other.OrgID, TargetPlayerID: target, Amount: 5}); !errors.Is(err, bounties.ErrForbiddenTenant) {
		t.Fatalf("another organization must not place a bounty on this organization's server, got %v", err)
	}
	if _, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, OrganizationID: w.fixture.OrgID, TargetPlayerID: target, Amount: 5}); !errors.Is(err, bounties.ErrServerRequired) {
		t.Fatalf("an organization-scoped bounty must name a server, got %v", err)
	}
	if active, _ := w.status(target); active != 0 {
		t.Fatalf("no rejected placement may create a bounty, got %d", active)
	}

	// The owning organization succeeds.
	b, err := svc.Place(w.ctx, bounties.PlaceRequest{GuildID: guild, ServerID: w.serverA, OrganizationID: w.fixture.OrgID, TargetPlayerID: target, Amount: 5, PlacedBy: "owner"})
	if err != nil || b.ServerID != w.serverA {
		t.Fatalf("the owning organization must be able to place: %+v %v", b, err)
	}
}

// BOUNTY / BOUNTY_TRACKING routes never resolve across an organization boundary.
func TestBountyRoutesRejectCrossOrganization(t *testing.T) {
	a, verifier := saasIntegrationApp(t)
	withFakeNitradoServer(t, a, http.StatusOK, psServiceJSON)
	fixtureA := buildInstallationFixture(t, a, verifier)
	connectNitrado(t, a, fixtureA.OrgID, fixtureA.OwnerDiscordID)
	for _, id := range []string{"a-kf", "a-board", "a-track"} {
		seedGuildChannels(verifier, fixtureA.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	serverA := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, a, fixtureA.OrgID, fixtureA.InstallationID, 111111, fixtureA.OwnerDiscordID)).Server.ID
	if rr := saveChannelRoutes(t, a, fixtureA.OrgID, fixtureA.InstallationID, fixtureA.OwnerDiscordID, map[string]string{"KILLFEED": "a-kf", "BOUNTY": "a-board", "BOUNTY_TRACKING": "a-track"}); rr.Code != http.StatusOK {
		t.Fatalf("save A: %d %s", rr.Code, rr.Body.String())
	}
	guildA := mustGuildRowID(t, a, fixtureA.DiscordGuildID)
	ctx := context.Background()

	fixtureB := buildInstallationFixture(t, a, verifier)
	for _, id := range []string{"b-kf", "b-board", "b-track"} {
		seedGuildChannels(verifier, fixtureB.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	if rr := saveChannelRoutes(t, a, fixtureB.OrgID, fixtureB.InstallationID, fixtureB.OwnerDiscordID, map[string]string{"KILLFEED": "b-kf", "BOUNTY": "b-board", "BOUNTY_TRACKING": "b-track"}); rr.Code != http.StatusOK {
		t.Fatalf("save B: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE installations SET game_server_id=$2 WHERE id=$1`, fixtureB.InstallationID, serverA); err != nil {
		t.Fatal(err)
	}
	guildB := mustGuildRowID(t, a, fixtureB.DiscordGuildID)
	for _, key := range []string{"BOUNTY", "BOUNTY_TRACKING"} {
		if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildB, serverA, key); found {
			t.Fatalf("%s: B's forced pairing with A's server must not resolve, got %q", key, got)
		}
		if _, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, key); !found {
			t.Fatalf("%s: A's own route must resolve", key)
		}
	}
	if _, err := a.DB.Pool.Exec(ctx, `UPDATE game_servers SET organization_id=$2 WHERE id=$1`, serverA, fixtureB.OrgID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"BOUNTY", "BOUNTY_TRACKING"} {
		if got, found, _ := a.SaaSChannelRoutes.ResolveChannel(ctx, guildA, serverA, key); found {
			t.Fatalf("%s: a server claimed by another organization must not resolve through A's installation, got %q", key, got)
		}
	}
}
