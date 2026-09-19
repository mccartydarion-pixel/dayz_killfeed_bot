package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

func TestEconomyRouteKeyMatchesRoutingPackage(t *testing.T) {
	if routeKeyEconomy != routing.RouteEconomy || routing.RouteEconomy != "ECONOMY" {
		t.Fatalf("route key drifted: %q vs %q", routeKeyEconomy, routing.RouteEconomy)
	}
}

// --- ECONOMY feed ------------------------------------------------------------------------

type economyFeedFixture struct {
	res    *keyedResolver
	sender *fakeHitSender
	feed   *EconomyFeed
}

func newEconomyFeedFixture() *economyFeedFixture {
	f := &economyFeedFixture{res: newKeyedResolver(), sender: &fakeHitSender{fail: map[string]error{}}}
	f.feed = NewEconomyFeed(f.sender, f.res, func(context.Context) (int64, []int64, error) { return 7, []int64{1, 2}, nil })
	return f
}

func (f *economyFeedFixture) last(channel string) string {
	msgs := f.sender.messages(channel)
	if len(msgs) == 0 {
		return ""
	}
	parts := []string{}
	for _, e := range msgs[len(msgs)-1].embeds {
		parts = append(parts, e.Description)
	}
	return strings.Join(parts, "\n---\n")
}

func TestEconomyFeedCards(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-1")
	for _, tc := range []struct {
		name string
		ev   economy.Event
		want string
	}{
		{"bounty reward", economy.Event{Type: economy.TypeBountyClaim, GuildID: 7, ServerID: 1, PlayerName: "PlayerA", Amount: 125000, Credit: true, BalanceAfter: 340000},
			"💰 **BOUNTY REWARD**\nPlayerA earned 125,000 pts\nBalance: 340,000 pts"},
		{"system reward", economy.Event{Type: economy.TypeSystemReward, GuildID: 7, ServerID: 1, PlayerName: "PlayerA", Amount: 10, Credit: true, BalanceAfter: 10},
			"🎁 **REWARD**\nPlayerA earned 10 pts\nBalance: 10 pts"},
		{"admin credit (no balance shown)", economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "PlayerA", Amount: 50000, Credit: true, BalanceAfter: 999999},
			"➕ **ADMIN CREDIT**\nPlayerA received 50,000 pts"},
		{"admin debit (no balance shown)", economy.Event{Type: economy.TypeAdminDebit, GuildID: 7, ServerID: 1, PlayerName: "PlayerA", Amount: 25000, BalanceAfter: 999999},
			"➖ **ADMIN DEBIT**\nPlayerA lost 25,000 pts"},
		{"unknown future type", economy.Event{Type: "SHOP_PURCHASE", GuildID: 7, ServerID: 1, PlayerName: "PlayerA", Amount: 5},
			"💠 **ECONOMY**\nPlayerA lost 5 pts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.feed.Notify(tc.ev)
			f.feed.tick(false)
			if got := f.last("eco-1"); got != tc.want {
				t.Fatalf("unexpected card:\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	for _, m := range f.sender.messages("eco-1") {
		if m.mention == nil || len(m.mention.Parse) != 0 {
			t.Fatal("economy cards must not be able to ping anyone")
		}
	}
}

// K: with no ECONOMY route the feed is a no-op and never borrows another route.
func TestEconomyFeedNoRouteAndLookupErrorAreNoOps(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "KILLFEED", "kill-chan")
	f.res.set(7, 1, "BOUNTY_TRACKING", "track-chan")
	ev := economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "A", Amount: 5, Credit: true}
	f.feed.Notify(ev)
	f.feed.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("no ECONOMY route: expected nothing (no KILLFEED fallback), got %+v", f.sender.sent)
	}
	f.res.set(7, 1, "ECONOMY", "eco-1")
	f.res.fail(7, 1, "ECONOMY", errors.New("db down"))
	f.feed.Notify(ev)
	f.feed.tick(false)
	if f.sender.total() != 0 {
		t.Fatalf("lookup error: expected no messages and no fallback, got %+v", f.sender.sent)
	}
	f.res.fail(7, 1, "ECONOMY", nil)
	f.feed.Notify(ev)
	f.feed.tick(false)
	if len(f.sender.messages("eco-1")) != 1 {
		t.Fatal("expected recovery once lookups work")
	}
}

// Server-attributed events go to that server's route; guild-wide ones to every
// server's route, deduplicated; other guilds' routes never receive anything.
func TestEconomyFeedRoutesPerServer(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "chan-A")
	f.res.set(7, 2, "ECONOMY", "chan-B")
	f.res.set(99, 1, "ECONOMY", "foreign-chan")

	f.feed.Notify(economy.Event{Type: economy.TypeBountyClaim, GuildID: 7, ServerID: 2, PlayerName: "OnB", Amount: 1, Credit: true, BalanceAfter: 1})
	f.feed.tick(false)
	if !strings.Contains(f.last("chan-B"), "OnB") || f.last("chan-A") != "" {
		t.Fatalf("a payout on server B goes to B's route only: A=%q B=%q", f.last("chan-A"), f.last("chan-B"))
	}
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, PlayerName: "GuildWide", Amount: 2, Credit: true})
	f.feed.tick(false)
	if !strings.Contains(f.last("chan-A"), "GuildWide") || !strings.Contains(f.last("chan-B"), "GuildWide") {
		t.Fatal("a guild-wide adjustment must reach every server's route")
	}
	if len(f.sender.messages("foreign-chan")) != 0 {
		t.Fatal("another guild's route must never receive events")
	}

	g := newEconomyFeedFixture()
	g.res.set(7, 1, "ECONOMY", "shared")
	g.res.set(7, 2, "ECONOMY", "shared")
	g.feed.Notify(economy.Event{Type: economy.TypeAdminDebit, GuildID: 7, PlayerName: "Once", Amount: 3})
	g.feed.tick(false)
	if msgs := g.sender.messages("shared"); len(msgs) != 1 || len(msgs[0].embeds) != 1 {
		t.Fatalf("a shared channel gets the event once, got %+v", msgs)
	}
}

func TestEconomyFeedRouteChangeUsesNewChannel(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "old")
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "A", Amount: 1, Credit: true})
	f.feed.tick(false)
	f.res.set(7, 1, "ECONOMY", "new")
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "B", Amount: 1, Credit: true})
	f.feed.tick(false)
	if len(f.sender.messages("old")) != 1 || len(f.sender.messages("new")) != 1 {
		t.Fatalf("expected the new channel used after the change, got %+v", f.sender.sent)
	}
}

// M: a Discord failure is logged and dropped - never retried (a retry could not
// redo a committed transaction anyway) and never a panic.
func TestEconomyFeedDiscordFailureIsIsolated(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-1")
	f.sender.fail["eco-1"] = errors.New("discord 500")
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "A", Amount: 1, Credit: true})
	f.feed.tick(false)
	f.feed.mu.Lock()
	failed, queued := f.feed.failedSends, len(f.feed.queue)
	f.feed.mu.Unlock()
	if failed != 1 || queued != 0 {
		t.Fatalf("expected one counted failure and no retry backlog, failed=%d queued=%d", failed, queued)
	}
	delete(f.sender.fail, "eco-1")
	f.sender.panicOn = "eco-1"
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "A", Amount: 1, Credit: true})
	f.feed.safeTick(false)
	f.sender.panicOn = ""
	f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: "B", Amount: 1, Credit: true})
	f.feed.safeTick(false)
	if len(f.sender.messages("eco-1")) != 1 {
		t.Fatalf("expected the feed to keep working afterwards, got %+v", f.sender.sent)
	}
}

func TestEconomyFeedFloodIsBoundedAndNeverBlocks(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-1")
	f.sender.mu.Lock() // a hung sender: Notify must not wait on it
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			f.feed.Notify(economy.Event{Type: economy.TypeAdminCredit, GuildID: 7, ServerID: 1, PlayerName: fmt.Sprintf("P%d", i), Amount: 1, Credit: true})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Notify blocked on the Discord sender")
	}
	f.sender.mu.Unlock()
	f.feed.mu.Lock()
	queued := len(f.feed.queue)
	f.feed.mu.Unlock()
	if queued != economyFeedMaxQueue {
		t.Fatalf("expected the queue capped at %d, got %d", economyFeedMaxQueue, queued)
	}
	f.feed.tick(false)
	cards := 0
	for _, m := range f.sender.messages("eco-1") {
		if len(m.embeds) > economyFeedEmbedsPerFrame {
			t.Fatalf("a message carried %d cards, max %d", len(m.embeds), economyFeedEmbedsPerFrame)
		}
		cards += len(m.embeds)
	}
	if cards != economyFeedEventsPerTick {
		t.Fatalf("one tick may send at most %d events, sent %d", economyFeedEventsPerTick, cards)
	}
}

// P + mention safety: names are sanitised; nothing internal appears in a card.
func TestEconomyFeedNamesCannotMentionAndCardsHoldNoInternalIDs(t *testing.T) {
	f := newEconomyFeedFixture()
	f.res.set(7, 1, "ECONOMY", "eco-1")
	for _, n := range []string{"@everyone", "@here", "<@123456789012345678>", "<@&987654321098765432>", "#general", "Bad\x00Name", strings.Repeat("A", 200)} {
		f.feed.Notify(economy.Event{Type: economy.TypeBountyClaim, GuildID: 7, ServerID: 1, PlayerName: n, Amount: 1, Credit: true, BalanceAfter: 2})
	}
	f.feed.tick(false)
	for _, m := range f.sender.messages("eco-1") {
		if m.mention == nil || len(m.mention.Parse) != 0 || len(m.mention.Users) != 0 || len(m.mention.Roles) != 0 {
			t.Fatalf("AllowedMentions must be empty, got %+v", m.mention)
		}
		for _, e := range m.embeds {
			for _, banned := range []string{"@", "#", "\x00"} {
				if strings.Contains(e.Description, banned) {
					t.Fatalf("name leaked %q into %q", banned, e.Description)
				}
			}
			if strings.Contains(e.Description, strings.Repeat("A", 60)) {
				t.Fatalf("over-long names must be truncated: %q", e.Description)
			}
		}
	}
	if f.sender.total() == 0 {
		t.Fatal("expected messages to inspect")
	}
}

func TestEconomyFeedNilSafety(t *testing.T) {
	var feed *EconomyFeed
	feed.Notify(economy.Event{Type: "X"})
	feed.Flush()
	feed.Run(context.Background())
}

// --- commands: identity, privacy, authorization -------------------------------------------

func interaction(userID string, perms int64) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Member: &discordgo.Member{User: &discordgo.User{ID: userID}, Permissions: perms},
	}}
}

type fakeLinks struct {
	byUser map[string]int64
}

func (f fakeLinks) LinkedPlayerID(_ context.Context, _ int64, user string) (int64, bool) {
	id, ok := f.byUser[user]
	return id, ok
}

// Only a VERIFIED link resolves a balance: a pending/rejected link also carries a
// PlayerID (whoever the user asked to link), and must never expose that player.
func TestVerifiedPlayerIDRequiresAVerifiedLink(t *testing.T) {
	if _, ok := VerifiedPlayerID(nil); ok {
		t.Fatal("no link -> no player")
	}
	for _, status := range []string{linking.StatusPending, linking.StatusRejected, linking.StatusUnlinked, ""} {
		if id, ok := VerifiedPlayerID(&linking.LinkRecord{PlayerID: 42, Status: status}); ok {
			t.Fatalf("a %q link must not resolve (got player %d)", status, id)
		}
	}
	if _, ok := VerifiedPlayerID(&linking.LinkRecord{PlayerID: 0, Status: linking.StatusVerified}); ok {
		t.Fatal("a verified link with no player resolves nothing")
	}
	if id, ok := VerifiedPlayerID(&linking.LinkRecord{PlayerID: 42, Status: linking.StatusVerified}); !ok || id != 42 {
		t.Fatalf("a verified link resolves its player, got %d %v", id, ok)
	}
}

func TestEconomyCommandResolvesOwnBalanceThroughLinkAndProtectsOthers(t *testing.T) {
	h := &EconomyCommandHandler{links: fakeLinks{byUser: map[string]int64{"user-1": 10}}}
	ctx := context.Background()

	if id, msg := h.resolveSelfOrTarget(ctx, interaction("user-1", 0), 7, ""); id != 10 || msg != "" {
		t.Fatalf("a linked player must resolve to their own player, got %d %q", id, msg)
	}
	if id, msg := h.resolveSelfOrTarget(ctx, interaction("stranger", 0), 7, ""); id != 0 || !strings.Contains(msg, "NOT LINKED") {
		t.Fatalf("an unlinked user must be asked to link, got %d %q", id, msg)
	}
	// A non-admin naming another player is refused - before any lookup.
	if id, msg := h.resolveSelfOrTarget(ctx, interaction("user-1", 0), 7, "SomeoneElse"); id != 0 || !strings.Contains(msg, "Only admins") {
		t.Fatalf("a non-admin must not read another player's balance, got %d %q", id, msg)
	}
	noLinks := &EconomyCommandHandler{}
	if id, _ := noLinks.resolveSelfOrTarget(ctx, interaction("user-1", 0), 7, ""); id != 0 {
		t.Fatal("without link support nothing may resolve")
	}
}

// N: credit and debit are admin-only and refuse before touching anything.
func TestEconomyCommandCreditAndDebitAreAdminOnly(t *testing.T) {
	h := &EconomyCommandHandler{} // no dependencies: a refused call must never reach them
	for _, name := range []string{"credit", "debit"} {
		sub := &discordgo.ApplicationCommandInteractionDataOption{Name: name}
		for _, perms := range []int64{0, discordgo.PermissionSendMessages, discordgo.PermissionManageRoles} {
			if got := h.dispatch(context.Background(), interaction("u", perms), 7, sub); !strings.Contains(got, "permission required") {
				t.Fatalf("%s with permissions %d must be refused, got %q", name, perms, got)
			}
		}
	}
}

// --- shared reply rendering (command + panel buttons) --------------------------------------

type discordEconomyStore struct {
	name    string
	balance int64
	entries []repository.LedgerEntry
}

func (s discordEconomyStore) Credit(context.Context, repository.LedgerParams) (repository.LedgerEntry, error) {
	return repository.LedgerEntry{}, nil
}
func (s discordEconomyStore) Debit(context.Context, repository.LedgerParams) (repository.LedgerEntry, error) {
	return repository.LedgerEntry{}, nil
}
func (s discordEconomyStore) Balance(context.Context, int64, int64) (int64, error) { return s.balance, nil }
func (s discordEconomyStore) History(_ context.Context, _, _ int64, limit int, _ int64) ([]repository.LedgerEntry, int64, error) {
	return s.entries, 0, nil
}
func (s discordEconomyStore) PlayerName(context.Context, int64, int64) (string, bool, error) {
	return s.name, s.name != "", nil
}
func (s discordEconomyStore) ServerScope(context.Context, int64) (int64, *int64, bool, error) {
	return 0, nil, false, nil
}

func TestEconomyRepliesRenderBalanceAndHistoryWithoutInternalIDs(t *testing.T) {
	when := time.Unix(1_800_000_000, 0)
	store := discordEconomyStore{name: "Alice", balance: 340000, entries: []repository.LedgerEntry{
		{ID: 987654, PlayerID: 424242, GuildID: 555, Type: repository.TxAdminDebit, Amount: -25000, BalanceAfter: 340000, Description: "shop refund", CreatedBy: "1234567890123", CreatedAt: when},
		{ID: 987653, PlayerID: 424242, GuildID: 555, Type: repository.TxBountyClaim, Amount: 125000, BalanceAfter: 365000, ReferenceID: "bounty:31337", CreatedAt: when},
	}}
	svc := economy.NewService(store, nil)
	ctx := context.Background()

	if got := economyBalanceMessage(ctx, svc, 555, 424242); got != "💰 **CHAMPION POINTS BALANCE**\n\nBalance: **340,000 pts**" {
		t.Fatalf("unexpected balance reply: %q", got)
	}
	hist := economyHistoryMessage(ctx, svc, 555, 424242)
	for _, want := range []string{"➖ **−25,000** · Admin debit — shop refund · balance 340,000", "➕ **+125,000** · Bounty reward · balance 365,000", "<t:1800000000:R>"} {
		if !strings.Contains(hist, want) {
			t.Fatalf("history is missing %q:\n%s", want, hist)
		}
	}
	for _, secret := range []string{"987654", "424242", "555", "1234567890123", "31337", "bounty:"} {
		if strings.Contains(hist, secret) || strings.Contains(economyBalanceMessage(ctx, svc, 555, 424242), secret) {
			t.Fatalf("a reply leaked an internal id / actor (%q):\n%s", secret, hist)
		}
	}
	if empty := economyHistoryMessage(ctx, economy.NewService(discordEconomyStore{name: "Alice"}, nil), 555, 424242); !strings.Contains(empty, "No transactions yet") {
		t.Fatalf("expected the empty state, got %q", empty)
	}
	if got := economyBalanceMessage(ctx, economy.NewService(discordEconomyStore{}, nil), 555, 999); !strings.Contains(got, "Could not load") {
		t.Fatalf("an unknown player must not produce a balance, got %q", got)
	}
}

// The panel's economy buttons exist and are private-only.
func TestStatsPanelHasEconomyButtons(t *testing.T) {
	row := PlayerStatsPanelComponents()[0].(discordgo.ActionsRow)
	ids := map[string]bool{}
	for _, c := range row.Components {
		ids[c.(discordgo.Button).CustomID] = true
	}
	for _, want := range []string{statsPanelMeID, statsPanelSearchID, economyBalanceID, economyHistoryID} {
		if !ids[want] {
			t.Fatalf("the stats panel is missing the %q button", want)
		}
	}
	if len(row.Components) > 5 {
		t.Fatalf("an action row holds at most 5 buttons, got %d", len(row.Components))
	}
}
