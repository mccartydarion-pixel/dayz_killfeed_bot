package discord

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func TestBountyRouteKeysMatchRoutingPackage(t *testing.T) {
	if routeKeyBounty != routing.RouteBounty || routeKeyBountyTracking != routing.RouteBountyTracking {
		t.Fatalf("route keys drifted: %q/%q vs %q/%q", routeKeyBounty, routeKeyBountyTracking, routing.RouteBounty, routing.RouteBountyTracking)
	}
	if routing.RouteBounty != "BOUNTY" || routing.RouteBountyTracking != "BOUNTY_TRACKING" {
		t.Fatal("the SaaS vocabulary keys changed")
	}
}

func TestFormatAmount(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 250000: "250,000", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := formatAmount(in); got != want {
			t.Fatalf("formatAmount(%d) = %q, want %q", in, got, want)
		}
	}
}

// --- BOUNTY_TRACKING ------------------------------------------------------------------

type trackerFixture struct {
	res    *keyedResolver
	sender *fakeHitSender
	t      *BountyTracker
}

func newTrackerFixture() *trackerFixture {
	f := &trackerFixture{res: newKeyedResolver(), sender: &fakeHitSender{fail: map[string]error{}}}
	f.t = NewBountyTracker(f.sender, f.res, func(context.Context) (int64, []int64, error) { return 7, []int64{1, 2}, nil })
	return f
}

func (f *trackerFixture) last(channel string) string {
	msgs := f.sender.messages(channel)
	if len(msgs) == 0 {
		return ""
	}
	last := msgs[len(msgs)-1]
	parts := make([]string, 0, len(last.embeds))
	for _, e := range last.embeds {
		parts = append(parts, e.Description)
	}
	return strings.Join(parts, "\n---\n")
}

func dist(v float64) *float64 { return &v }

func TestBountyTrackerCards(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")

	cases := []struct {
		name string
		ev   bounties.Event
		want string
	}{
		{"placed", bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "PlayerA", Amount: 100000},
			"🎯 **BOUNTY PLACED**\n**PlayerA**\nReward **100,000 pts**"},
		{"placed by streak", bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, KillServerID: 1, Target: "PlayerA", Amount: 500, Automatic: true},
			"🎯 **BOUNTY PLACED**\n**PlayerA**\nReward **500 pts**\nSource: kill streak"},
		{"increased", bounties.Event{Kind: bounties.EventIncreased, GuildID: 7, ServerID: 1, Target: "PlayerA", Amount: 150000},
			"📈 **BOUNTY INCREASED**\n**PlayerA**\nReward **150,000 pts**"},
		{"claimed with weapon and distance", bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Hunter: "PlayerB", Target: "PlayerA", Amount: 100000, Count: 1, Weapon: "M4-A1", Distance: dist(86.4)},
			"👑 **BOUNTY CLAIMED**\n**PlayerB** eliminated **PlayerA**\nReward **100,000 pts**\n`M4-A1` • 86m"},
		{"claimed without weapon or distance", bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Hunter: "PlayerB", Target: "PlayerA", Amount: 100000, Count: 1},
			"👑 **BOUNTY CLAIMED**\n**PlayerB** eliminated **PlayerA**\nReward **100,000 pts**"},
		{"claimed stacked", bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Hunter: "PlayerB", Target: "PlayerA", Amount: 150000, Count: 2},
			"👑 **BOUNTY CLAIMED**\n**PlayerB** eliminated **PlayerA**\nReward **150,000 pts** • 2 bounties"},
		{"expired", bounties.Event{Kind: bounties.EventExpired, GuildID: 7, ServerID: 1, Target: "PlayerA", Amount: 100000},
			"⌛ **BOUNTY EXPIRED**\n**PlayerA**\nReward **100,000 pts**"},
		{"cancelled", bounties.Event{Kind: bounties.EventCancelled, GuildID: 7, ServerID: 1, Target: "PlayerA", Amount: 100000},
			"🚫 **BOUNTY CANCELLED**\n**PlayerA**\nReward **100,000 pts**"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.t.Notify(tc.ev)
			f.t.tick(false)
			if got := f.last("track-1"); got != tc.want {
				t.Fatalf("unexpected card:\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	for _, m := range f.sender.messages("track-1") {
		if m.mention == nil || len(m.mention.Parse) != 0 {
			t.Fatal("tracking cards must not be able to ping anyone")
		}
	}
}

// L/M: with no route the tracker is a no-op (the database is unaffected by
// construction - the Service commits before notifying).
func TestBountyTrackerNoRouteAndLookupErrorAreNoOps(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY", "board-chan") // the BOUNTY route must NOT be borrowed
	f.res.set(7, 1, "KILLFEED", "kill-chan")
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "A", Amount: 5})
	f.t.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("no BOUNTY_TRACKING route: expected no messages, got %+v", f.sender.sent)
	}

	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	f.res.fail(7, 1, "BOUNTY_TRACKING", errors.New("db down"))
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "A", Amount: 5})
	f.t.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("lookup error: expected no messages and no fallback, got %+v", f.sender.sent)
	}
	f.res.fail(7, 1, "BOUNTY_TRACKING", nil)
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "A", Amount: 5})
	f.t.tick(false)
	if len(f.sender.messages("track-1")) != 1 {
		t.Fatal("expected the tracker to recover once lookups work")
	}
}

// Multi-server, same guild: events go to the server they belong to, claims to the
// server the kill happened on, and a guild-wide event fans out (deduped by channel).
func TestBountyTrackerRoutesPerServer(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "chan-A")
	f.res.set(7, 2, "BOUNTY_TRACKING", "chan-B")
	f.res.set(99, 1, "BOUNTY_TRACKING", "foreign-chan") // another guild (cross-org)

	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "OnlyA", Amount: 1})
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 2, Target: "OnlyB", Amount: 2})
	f.t.Notify(bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 2, Hunter: "H", Target: "KilledOnB", Amount: 3, Count: 1})
	f.t.tick(false)
	a, b := f.last("chan-A"), f.last("chan-B")
	if !strings.Contains(a, "OnlyA") || strings.Contains(a, "OnlyB") || strings.Contains(a, "KilledOnB") {
		t.Fatalf("server A's channel must only carry A's events: %q", a)
	}
	if !strings.Contains(b, "OnlyB") || !strings.Contains(b, "KilledOnB") || strings.Contains(b, "OnlyA") {
		t.Fatalf("server B's channel must carry B's events and the kill on B: %q", b)
	}
	if len(f.sender.messages("foreign-chan")) != 0 {
		t.Fatal("another guild's route must never receive events")
	}

	// A guild-wide event (no server) reaches every routed server once.
	f.t.Notify(bounties.Event{Kind: bounties.EventCancelled, GuildID: 7, Target: "GuildWide", Amount: 9})
	f.t.tick(false)
	if !strings.Contains(f.last("chan-A"), "GuildWide") || !strings.Contains(f.last("chan-B"), "GuildWide") {
		t.Fatal("a guild-wide event must reach every server's route")
	}

	// Two servers sharing one channel: deduplicated, one card.
	g := newTrackerFixture()
	g.res.set(7, 1, "BOUNTY_TRACKING", "shared")
	g.res.set(7, 2, "BOUNTY_TRACKING", "shared")
	g.t.Notify(bounties.Event{Kind: bounties.EventCancelled, GuildID: 7, Target: "Once", Amount: 1})
	g.t.tick(false)
	if msgs := g.sender.messages("shared"); len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("a shared channel must get the event once, got %+v", msgs)
	}
}

func TestBountyTrackerRouteChangeUsesNewChannel(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "chan-old")
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "A", Amount: 1})
	f.t.tick(false)
	f.res.set(7, 1, "BOUNTY_TRACKING", "chan-new")
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "B", Amount: 1})
	f.t.tick(false)
	if len(f.sender.messages("chan-old")) != 1 || len(f.sender.messages("chan-new")) != 1 {
		t.Fatalf("expected the new channel used after the change, got %+v", f.sender.sent)
	}
}

// P: a Discord failure is logged and dropped - never retried (a retry could not
// re-claim anything: the claim is committed before the event exists), never a panic.
func TestBountyTrackerDiscordFailureIsIsolated(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	f.sender.fail["track-1"] = errors.New("discord 500")
	f.t.Notify(bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Hunter: "H", Target: "T", Amount: 1, Count: 1})
	f.t.tick(false)
	f.t.mu.Lock()
	failed, queued := f.t.failedSends, len(f.t.queue)
	f.t.mu.Unlock()
	if failed != 1 || queued != 0 {
		t.Fatalf("expected one counted failure and no retry backlog, failed=%d queued=%d", failed, queued)
	}

	delete(f.sender.fail, "track-1")
	f.sender.panicOn = "track-1"
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "T", Amount: 1})
	f.t.safeTick(false) // a panicking client is recovered
	f.sender.panicOn = ""
	f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "T2", Amount: 1})
	f.t.safeTick(false)
	if len(f.sender.messages("track-1")) != 1 {
		t.Fatalf("expected the feed to keep working afterwards, got %+v", f.sender.sent)
	}
}

// Discord failure AFTER a claim: the claim stays committed and is never repeated.
func TestDiscordFailureAfterClaimLeavesTheClaimCommitted(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	f.sender.fail["track-1"] = errors.New("discord down")
	store := &countingClaimStore{}
	svc := bounties.NewService(store, f.t)

	res, err := svc.ClaimForKill(context.Background(), bounties.KillInput{GuildID: 7, ServerID: 1, VictimPlayerID: 10, KillerPlayerID: 11, HunterName: "H", TargetName: "T"})
	if err != nil || res.Count != 1 || res.Total != 500 {
		t.Fatalf("the claim must succeed independently of Discord, got %+v %v", res, err)
	}
	f.t.tick(false) // the send fails
	f.t.tick(false)
	if store.claims != 1 {
		t.Fatalf("a Discord failure must never trigger a second claim, claims=%d", store.claims)
	}
}

type countingClaimStore struct {
	claims int
	bounties.Store
}

func (s *countingClaimStore) ClaimForKill(context.Context, repository.KillClaim) ([]repository.Bounty, error) {
	s.claims++
	return []repository.Bounty{{ID: 1, GuildID: 7, RewardPoints: 500}}, nil
}

func TestBountyTrackerFloodIsBounded(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	for i := 0; i < 1000; i++ {
		f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: fmt.Sprintf("P%d", i), Amount: 1})
	}
	f.t.mu.Lock()
	queued := len(f.t.queue)
	f.t.mu.Unlock()
	if queued != bountyTrackerMaxQueue {
		t.Fatalf("expected the queue capped at %d, got %d", bountyTrackerMaxQueue, queued)
	}
	f.t.tick(false)
	cards := 0
	for _, m := range f.sender.messages("track-1") {
		if len(m.embeds) > bountyEmbedsPerMessage {
			t.Fatalf("a message carried %d cards, max %d", len(m.embeds), bountyEmbedsPerMessage)
		}
		cards += len(m.embeds)
	}
	if cards != bountyTrackerEventsPerTick {
		t.Fatalf("one tick may send at most %d events, sent %d", bountyTrackerEventsPerTick, cards)
	}
}

func TestBountyTrackerNamesCannotMention(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	for _, n := range []string{"@everyone", "@here", "<@123456789012345678>", "<@&987654321098765432>", "#general", "Bad\x00Name", strings.Repeat("A", 200)} {
		f.t.Notify(bounties.Event{Kind: bounties.EventClaimed, GuildID: 7, KillServerID: 1, Hunter: n, Target: n, Amount: 1, Count: 1, Weapon: n})
	}
	f.t.tick(false)
	for _, m := range f.sender.messages("track-1") {
		if m.mention == nil || len(m.mention.Parse) != 0 || len(m.mention.Users) != 0 || len(m.mention.Roles) != 0 {
			t.Fatalf("AllowedMentions must be empty, got %+v", m.mention)
		}
		for _, e := range m.embeds {
			for _, banned := range []string{"@", "#", "\x00"} {
				if strings.Contains(e.Description, banned) {
					t.Fatalf("name leaked %q into %q", banned, e.Description)
				}
			}
			if strings.Contains(e.Description, strings.Repeat("A", 90)) {
				t.Fatalf("over-long names must be truncated: %q", e.Description)
			}
		}
	}
	if f.sender.total() == 0 {
		t.Fatal("expected messages to inspect")
	}
}

func TestBountyTrackerNotifyNeverBlocksAndNilSafe(t *testing.T) {
	f := newTrackerFixture()
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-1")
	f.sender.mu.Lock() // a hung sender: any send would deadlock
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			f.t.Notify(bounties.Event{Kind: bounties.EventPlaced, GuildID: 7, ServerID: 1, Target: "T", Amount: 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Notify blocked on the Discord sender")
	}
	f.sender.mu.Unlock()

	var nilTracker *BountyTracker
	nilTracker.Notify(bounties.Event{Kind: bounties.EventPlaced})
	nilTracker.Flush()
	var nilBoard *BountyBoard
	nilBoard.Trigger()
	nilBoard.SyncOnce(context.Background())
	BountyEvents{}.Notify(bounties.Event{Kind: bounties.EventPlaced})
}

// --- BOUNTY: the persistent board ------------------------------------------------------------

// fakeBoardLister stands in for the bounties table: rows carry a server scope
// (0 = guild-wide) and mirror ListBoard's filtering and stacking.
type fakeBoardLister struct {
	mu   sync.Mutex
	rows []boardRow
	err  error
}

type boardRow struct {
	server int64
	target string
	amount int64
}

func (l *fakeBoardLister) ListBoard(_ context.Context, _ int64, serverIDs []int64, limit int) ([]repository.BoardEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	in := map[int64]bool{}
	for _, id := range serverIDs {
		in[id] = true
	}
	totals, counts := map[string]int64{}, map[string]int64{}
	for _, r := range l.rows {
		if r.server == 0 || in[r.server] {
			totals[r.target] += r.amount
			counts[r.target]++
		}
	}
	var out []repository.BoardEntry
	for name, total := range totals {
		out = append(out, repository.BoardEntry{TargetPlayerID: 424242, TargetName: name, Total: total, Count: counts[name]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].TargetName < out[j].TargetName
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (l *fakeBoardLister) set(rows ...boardRow) {
	l.mu.Lock()
	l.rows = rows
	l.mu.Unlock()
}

type boardFixture struct {
	*routedFixture
	lister *fakeBoardLister
	board  *BountyBoard
}

func newBoardFixture(t *testing.T) *boardFixture {
	t.Helper()
	f := &boardFixture{routedFixture: newRoutedFixture(t), lister: &fakeBoardLister{}}
	f.board = NewBountyBoard(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, f.panels, f.lister)
	return f
}

func (f *boardFixture) sync() { f.board.SyncOnce(context.Background()) }

func (f *boardFixture) boardText(channel string) string {
	f.api.mu.Lock()
	defer f.api.mu.Unlock()
	return f.api.lastEmbed[channel]
}

// L: no BOUNTY route -> a safe no-op for Discord.
func TestBountyBoardNoRouteIsNoOp(t *testing.T) {
	f := newBoardFixture(t)
	f.lister.set(boardRow{0, "PlayerA", 250000})
	f.resolver.set(f.guild, 1, "BOUNTY_TRACKING", "track-chan") // the other route is not the board
	f.sync()
	if f.api.sendCount() != 0 || f.store.count(f.guild, "BOUNTY") != 0 {
		t.Fatalf("no BOUNTY route: nothing may be posted, live=%v", f.api.live())
	}
}

func TestBountyBoardShowsTopBountiesWithoutInternalIDs(t *testing.T) {
	f := newBoardFixture(t)
	f.resolver.set(f.guild, 1, "BOUNTY", "board-1")
	f.lister.set(boardRow{1, "PlayerC", 75000}, boardRow{1, "PlayerA", 250000}, boardRow{1, "PlayerB", 125000}, boardRow{1, "PlayerB", 1000})
	f.sync()

	want := "🎯 **ACTIVE BOUNTIES**\n\n🥇 PlayerA • **250,000 pts**\n🥈 PlayerB • **126,000 pts** (2 bounties)\n🥉 PlayerC • **75,000 pts**"
	if got := f.boardText("board-1"); got != want {
		t.Fatalf("unexpected board:\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(f.boardText("board-1"), "424242") {
		t.Fatal("no internal id may appear on the board")
	}
}

// N: the board is edited in place - never re-posted - as bounties change.
func TestBountyBoardEditsTheSameMessageInsteadOfDuplicating(t *testing.T) {
	f := newBoardFixture(t)
	f.resolver.set(f.guild, 1, "BOUNTY", "board-1")
	f.lister.set(boardRow{1, "PlayerA", 100})
	f.sync()
	first := f.store.messageFor(f.guild, "BOUNTY", "board-1")

	// Unchanged content: nothing is edited (no needless Discord traffic).
	f.sync()
	f.sync()
	if len(f.api.edits) != 0 {
		t.Fatalf("an unchanged board must not be edited, edits=%v", f.api.edits)
	}

	// create, increase, claim (row gone), expire/cancel (row gone): each edits the same message.
	steps := [][]boardRow{
		{{1, "PlayerA", 100}, {1, "PlayerB", 50}}, // created
		{{1, "PlayerA", 300}, {1, "PlayerB", 50}}, // increased
		{{1, "PlayerB", 50}},                      // claimed
		{},                                        // expired / cancelled
	}
	for i, rows := range steps {
		f.lister.set(rows...)
		f.board.Notify(bounties.Event{Kind: bounties.EventPlaced}) // any lifecycle event triggers a reconcile
		f.sync()
		if len(f.api.edits) != i+1 {
			t.Fatalf("step %d: expected %d edits in total, got %v", i, i+1, f.api.edits)
		}
	}
	if f.api.sendCount() != 1 || f.api.liveIn("board-1") != 1 || f.store.messageFor(f.guild, "BOUNTY", "board-1") != first {
		t.Fatalf("expected ONE persistent board message, sends=%d live=%v", f.api.sendCount(), f.api.live())
	}
	if !strings.Contains(f.boardText("board-1"), "No active bounties") {
		t.Fatalf("expected the empty state at the end, got %q", f.boardText("board-1"))
	}
}

// O: a route change moves the board (old message retired, one new one posted).
func TestBountyBoardRouteChangeMovesTheBoard(t *testing.T) {
	f := newBoardFixture(t)
	f.lister.set(boardRow{1, "PlayerA", 100})
	f.resolver.set(f.guild, 1, "BOUNTY", "chan-old")
	f.sync()
	f.resolver.set(f.guild, 1, "BOUNTY", "chan-new")
	f.sync()
	if f.api.liveIn("chan-old") != 0 || f.api.liveIn("chan-new") != 1 || len(f.api.live()) != 1 {
		t.Fatalf("expected the board to move to chan-new, live=%v", f.api.live())
	}

	// Route removed: the board is retired; a lookup error leaves it alone.
	f.resolver.fail(f.guild, 1, "BOUNTY", errors.New("db down"))
	f.resolver.fail(f.guild, 2, "BOUNTY", errors.New("db down"))
	f.sync()
	if f.api.liveIn("chan-new") != 1 {
		t.Fatal("a failed lookup must not retire the board")
	}
	f.resolver.fail(f.guild, 1, "BOUNTY", nil)
	f.resolver.fail(f.guild, 2, "BOUNTY", nil)
	f.resolver.clear(f.guild, 1, "BOUNTY")
	f.sync()
	if len(f.api.live()) != 0 || f.store.count(f.guild, "BOUNTY") != 0 {
		t.Fatalf("expected the board retired with its route, live=%v", f.api.live())
	}
}

// Q: a restart reconstructs the board from the database and the recorded message.
func TestBountyBoardRestartEditsTheRecordedMessage(t *testing.T) {
	f := newBoardFixture(t)
	f.resolver.set(f.guild, 1, "BOUNTY", "board-1")
	f.lister.set(boardRow{1, "PlayerA", 100})
	f.sync()
	msg := f.store.messageFor(f.guild, "BOUNTY", "board-1")

	// "Restart": brand-new panels/board over the SAME durable store and Discord.
	f.lister.set(boardRow{1, "PlayerA", 100}, boardRow{1, "PlayerZ", 999})
	restarted := NewBountyBoard(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, NewRoutePanels(f.api, f.store), f.lister)
	restarted.SyncOnce(context.Background())

	if f.api.sendCount() != 1 || f.api.liveIn("board-1") != 1 || f.store.messageFor(f.guild, "BOUNTY", "board-1") != msg {
		t.Fatalf("a restart must not post a second board, sends=%d live=%v", f.api.sendCount(), f.api.live())
	}
	if !strings.Contains(f.boardText("board-1"), "PlayerZ") {
		t.Fatalf("the restarted board must show the current database state, got %q", f.boardText("board-1"))
	}
}

// Server scope on the board: a channel shows only its own server's bounties (plus
// guild-wide ones); a shared channel shows the union.
func TestBountyBoardIsServerScoped(t *testing.T) {
	f := newBoardFixture(t)
	f.resolver.set(f.guild, 1, "BOUNTY", "board-A")
	f.resolver.set(f.guild, 2, "BOUNTY", "board-B")
	f.lister.set(boardRow{1, "OnlyOnA", 100}, boardRow{2, "OnlyOnB", 200}, boardRow{0, "GuildWide", 300})
	f.sync()

	a, b := f.boardText("board-A"), f.boardText("board-B")
	if !strings.Contains(a, "OnlyOnA") || strings.Contains(a, "OnlyOnB") || !strings.Contains(a, "GuildWide") {
		t.Fatalf("server A's board: %q", a)
	}
	if !strings.Contains(b, "OnlyOnB") || strings.Contains(b, "OnlyOnA") || !strings.Contains(b, "GuildWide") {
		t.Fatalf("server B's board: %q", b)
	}

	g := newBoardFixture(t)
	g.resolver.set(g.guild, 1, "BOUNTY", "shared")
	g.resolver.set(g.guild, 2, "BOUNTY", "shared")
	g.lister.set(boardRow{1, "OnlyOnA", 100}, boardRow{2, "OnlyOnB", 200})
	g.sync()
	if txt := g.boardText("shared"); !strings.Contains(txt, "OnlyOnA") || !strings.Contains(txt, "OnlyOnB") || g.api.liveIn("shared") != 1 {
		t.Fatalf("a shared channel shows one board with the union: %q live=%v", txt, g.api.live())
	}

	// Another guild's route never produces a board here.
	h := newBoardFixture(t)
	h.resolver.set(99, 1, "BOUNTY", "foreign")
	h.lister.set(boardRow{1, "X", 1})
	h.sync()
	if h.api.sendCount() != 0 {
		t.Fatal("a foreign guild's route must not create a board")
	}
}

// Discord/database hiccups never duplicate or corrupt the board.
func TestBountyBoardFailuresDoNotDuplicate(t *testing.T) {
	f := newBoardFixture(t)
	f.resolver.set(f.guild, 1, "BOUNTY", "board-1")
	f.lister.set(boardRow{1, "PlayerA", 100})
	f.sync()
	msg := f.store.messageFor(f.guild, "BOUNTY", "board-1")

	// A transient edit failure: not recreated.
	f.lister.set(boardRow{1, "PlayerA", 200})
	f.api.editErr["board-1/"+msg] = errors.New("503")
	f.sync()
	if f.api.sendCount() != 1 {
		t.Fatalf("a transient error must not post a second board, sends=%d", f.api.sendCount())
	}
	delete(f.api.editErr, "board-1/"+msg)

	// A database error while rendering: the existing message is left as it is.
	f.lister.mu.Lock()
	f.lister.err = errors.New("db down")
	f.lister.mu.Unlock()
	before := f.boardText("board-1")
	f.sync()
	if f.boardText("board-1") != before || f.api.sendCount() != 1 {
		t.Fatal("a failed listing must leave the board untouched")
	}
	f.lister.mu.Lock()
	f.lister.err = nil
	f.lister.mu.Unlock()

	// A moderator deletes the board: recreated exactly once.
	_ = f.api.ChannelMessageDelete("board-1", msg)
	f.sync()
	if f.api.liveIn("board-1") != 1 {
		t.Fatalf("expected the deleted board recreated once, live=%v", f.api.live())
	}
}
