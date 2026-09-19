//go:build integration

package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// economyWorld is the bounty world plus the real economy service and ECONOMY feed,
// wired exactly as app.go wires them (feed -> economy service and bounty claims).
type economyWorld struct {
	*bountyWorld
	svc  *economy.Service
	feed *discord.EconomyFeed
}

// newEconomyWorld builds a world whose servers carry the given ECONOMY channels
// ("" = no ECONOMY route for that server). Channels are seeded before the routes
// are saved, since the route API validates them against the guild's channels.
func newEconomyWorld(t *testing.T, ecoA, ecoB string) *economyWorld {
	t.Helper()
	w := newBountyWorld(t, nil, nil)
	for _, id := range []string{"eco-A", "eco-B", "eco-A2"} {
		seedGuildChannels(w.verifier, w.fixture.DiscordGuildID, fakeGuildChannel{ID: id, Name: id, Type: discordgo.ChannelTypeGuildText})
	}
	ew := &economyWorld{bountyWorld: w}
	ew.setRoutes(ecoA, ecoB)

	ew.feed = discord.NewEconomyFeed(trackingSender{w.d}, w.a.ChannelRoutes, w.servers)
	ew.svc = economy.NewService(repository.NewEconomyRepository(w.a.DB.Pool), ew.feed)
	w.a.EconomyService = ew.svc
	w.a.BountyService.SetEconomyNotifier(ew.feed)
	return ew
}

func (w *economyWorld) setRoutes(ecoA, ecoB string) {
	w.t.Helper()
	// An empty value removes the route; an absent key would leave it untouched.
	w.save(w.fixture.InstallationID, "kf-A", map[string]string{"ECONOMY": ecoA})
	w.save(w.installB, "kf-B", map[string]string{"ECONOMY": ecoB})
}

func (w *economyWorld) balance(player int64) int64 {
	w.t.Helper()
	b, err := w.svc.Balance(w.ctx, w.guildRowID, player)
	must(w.t, err)
	return b
}

func (w *economyWorld) adminCredit(player, amount int64, reason string) (economy.Result, error) {
	return w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, PlayerID: player, Amount: amount, Type: economy.TypeAdminCredit, Actor: "admin-42", Description: reason})
}

func (w *economyWorld) adminDebit(player, amount int64, reason string) (economy.Result, error) {
	return w.svc.AdminDebit(w.ctx, economy.Request{GuildID: w.guildRowID, PlayerID: player, Amount: amount, Type: economy.TypeAdminDebit, Actor: "admin-42", Description: reason})
}

// Persist -> atomic claim + ledger credit (one transaction per bounty) -> commit ->
// ECONOMY card on the kill's server. Replays pay nothing and announce nothing.
func TestEconomyBountyRewardEndToEnd(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "eco-B")
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 125000, PlacedBy: "admin-1"})
	must(t, err)

	q := w.queue(w.serverA)
	must(t, q.EnqueueAndWait(w.ctx, killOf("10:10:00")))
	w.feed.Flush()

	if got := w.balance(w.hunter); got != 125000 {
		t.Fatalf("the reward must land in the spendable balance, got %d", got)
	}
	cards := w.d.cardsIn("eco-A")
	if len(cards) != 1 || cards[0] != "💰 **BOUNTY REWARD**\nHunter earned 125,000 pts\nBalance: 125,000 pts" {
		t.Fatalf("unexpected reward card: %q", cards)
	}
	if len(w.d.cardsIn("eco-B")) != 0 {
		t.Fatal("a kill on server A must not reach server B's ECONOMY route")
	}
	if len(w.d.cardsIn("kf-A")) != 0 || len(w.d.cardsIn("kf-B")) != 0 {
		t.Fatal("economy events never fall back to the KILLFEED")
	}
	page, err := w.svc.History(w.ctx, w.guildRowID, w.hunter, 10, 0)
	must(t, err)
	if len(page.Items) != 1 || page.Items[0].Label != "Bounty reward" || page.Items[0].Amount != 125000 || page.Items[0].BalanceAfter != 125000 {
		t.Fatalf("unexpected history: %+v", page.Items)
	}

	// Replay after a "restart": the durable insert is a duplicate; nothing is paid or announced.
	must(t, w.queue(w.serverA).EnqueueAndWait(w.ctx, killOf("10:10:00")))
	w.feed.Flush()
	if w.balance(w.hunter) != 125000 || len(w.d.cardsIn("eco-A")) != 1 {
		t.Fatalf("a replay must pay and announce nothing: balance=%d cards=%d", w.balance(w.hunter), len(w.d.cardsIn("eco-A")))
	}
	if life, txs := w.points(); life != 125000 || txs != 1 {
		t.Fatalf("expected one BOUNTY_CLAIM ledger row and 125000 lifetime, got %d/%d", life, txs)
	}
}

// Stacked bounties: one ledger transaction and one card per bounty, in order.
func TestEconomyStackedBountiesPayOncePerBounty(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "")
	for _, amt := range []int64{100, 200} {
		_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: amt, PlacedBy: "a"})
		must(t, err)
	}
	must(t, w.queue(w.serverA).EnqueueAndWait(w.ctx, killOf("10:00:00")))
	w.feed.Flush()
	if w.balance(w.hunter) != 300 {
		t.Fatalf("both rewards must be paid, got %d", w.balance(w.hunter))
	}
	cards := w.d.cardsIn("eco-A")
	if len(cards) != 2 || !strings.Contains(cards[0], "earned 100 pts") || !strings.Contains(cards[0], "Balance: 100 pts") ||
		!strings.Contains(cards[1], "earned 200 pts") || !strings.Contains(cards[1], "Balance: 300 pts") {
		t.Fatalf("expected one card per bounty with running balances, got %q", cards)
	}
	if _, txs := w.points(); txs != 2 {
		t.Fatalf("expected 2 ledger transactions, got %d", txs)
	}
}

// Admin adjustments are guild-wide: every server's route gets the card, deduplicated
// by channel, and the card shows neither the admin nor the reason.
func TestEconomyAdminAdjustmentsFanOutWithoutLeakingAudit(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "eco-B")
	cr, err := w.adminCredit(w.hunter, 50000, "compensation for a lost base")
	must(t, err)
	if cr.Balance != 50000 || cr.Duplicate {
		t.Fatalf("credit result: %+v", cr)
	}
	db, err := w.adminDebit(w.hunter, 25000, "duping penalty")
	must(t, err)
	if db.Balance != 25000 {
		t.Fatalf("debit result: %+v", db)
	}
	w.feed.Flush()
	for _, ch := range []string{"eco-A", "eco-B"} {
		cards := w.d.cardsIn(ch)
		if len(cards) != 2 || cards[0] != "➕ **ADMIN CREDIT**\nHunter received 50,000 pts" || cards[1] != "➖ **ADMIN DEBIT**\nHunter lost 25,000 pts" {
			t.Fatalf("%s: unexpected admin cards %q", ch, cards)
		}
		for _, c := range cards {
			if strings.Contains(c, "admin-42") || strings.Contains(c, "compensation") || strings.Contains(c, "duping") {
				t.Fatalf("a public card must not reveal the admin or the reason: %q", c)
			}
		}
	}
	// The audit trail is stored (internal), not published.
	var by, desc string
	must(t, w.a.DB.Pool.QueryRow(w.ctx, `SELECT created_by, description FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='ADMIN_DEBIT'`, w.guildRowID, w.hunter).Scan(&by, &desc))
	if by != "admin-42" || desc != "duping penalty" {
		t.Fatalf("the audit trail must record who and why, got %q %q", by, desc)
	}

	// One channel behind both servers gets one card, not two.
	w.setRoutes("eco-A", "eco-A")
	if _, err := w.adminCredit(w.target, 10, ""); err != nil {
		t.Fatal(err)
	}
	w.feed.Flush()
	if got := len(w.d.cardsIn("eco-A")); got != 3 {
		t.Fatalf("a shared channel must receive one card per event, got %d cards total", got)
	}
}

// A rejected debit changes nothing and announces nothing.
func TestEconomyInsufficientDebitIsNotAnnounced(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "")
	if _, err := w.adminDebit(w.hunter, 1, "x"); !errors.Is(err, economy.ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	if _, err := w.adminCredit(w.hunter, 0, ""); !errors.Is(err, economy.ErrInvalidAmount) {
		t.Fatalf("a zero amount must be rejected, got %v", err)
	}
	if _, err := w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit}); !errors.Is(err, economy.ErrActorRequired) {
		t.Fatalf("an admin adjustment must record its actor, got %v", err)
	}
	w.feed.Flush()
	if w.balance(w.hunter) != 0 || len(w.d.cardsIn("eco-A")) != 0 {
		t.Fatal("rejected operations must leave no balance change and no card")
	}
}

// No ECONOMY route: everything works, nothing is sent anywhere - and never to KILLFEED.
func TestEconomyWorksWithoutAnyRoute(t *testing.T) {
	w := newEconomyWorld(t, "", "")
	_, err := w.a.BountyService.Place(w.ctx, bounties.PlaceRequest{GuildID: w.guildRowID, ServerID: w.serverA, TargetPlayerID: w.target, Amount: 700, PlacedBy: "a"})
	must(t, err)
	must(t, w.queue(w.serverA).EnqueueAndWait(w.ctx, killOf("11:00:00")))
	if _, err := w.adminCredit(w.hunter, 300, "bonus"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.adminDebit(w.hunter, 100, "fix"); err != nil {
		t.Fatal(err)
	}
	w.feed.Flush()
	if got := w.balance(w.hunter); got != 900 {
		t.Fatalf("the economy must work without a route, got balance %d", got)
	}
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	if w.d.sends != 0 || len(w.d.cards) != 0 {
		t.Fatalf("no route: nothing may be sent, sends=%d cards=%v", w.d.sends, w.d.cards)
	}
}

// A Discord failure after the commit leaves the transaction committed and is never
// retried into a second application.
func TestEconomyCommitSurvivesDiscordFailure(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "")
	w.d.mu.Lock()
	w.d.failSend = true
	w.d.mu.Unlock()

	res, err := w.adminCredit(w.hunter, 4000, "bonus")
	if err != nil || res.Balance != 4000 {
		t.Fatalf("the credit must succeed regardless of Discord: %+v %v", res, err)
	}
	w.feed.Flush() // the send fails
	w.feed.Flush()
	if w.balance(w.hunter) != 4000 {
		t.Fatalf("the balance must stay committed, got %d", w.balance(w.hunter))
	}

	// Discord recovers: the dropped card is not retried, and nothing is duplicated.
	w.d.mu.Lock()
	w.d.failSend = false
	w.d.mu.Unlock()
	w.feed.Flush()
	if len(w.d.cardsIn("eco-A")) != 0 {
		t.Fatal("a failed send is dropped, not retried")
	}
	if w.balance(w.hunter) != 4000 {
		t.Fatal("the balance must not change from a feed retry")
	}
	if rows := ledgerRowCount(t, w, w.hunter); rows != 1 {
		t.Fatalf("expected exactly one ledger row, got %d", rows)
	}
}

func ledgerRowCount(t *testing.T, w *economyWorld, player int64) (n int64) {
	t.Helper()
	must(t, w.a.DB.Pool.QueryRow(w.ctx, `SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, w.guildRowID, player).Scan(&n))
	return n
}

// Changing the ECONOMY route takes effect for the very next event (the resolver is
// invalidated on save); removing it stops delivery without touching balances.
func TestEconomyRouteChangeAndRemoval(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "")
	_, err := w.adminCredit(w.hunter, 10, "")
	must(t, err)
	w.feed.Flush()
	if len(w.d.cardsIn("eco-A")) != 1 {
		t.Fatal("setup: expected the first card on eco-A")
	}

	w.setRoutes("eco-A2", "")
	_, err = w.adminCredit(w.hunter, 20, "")
	must(t, err)
	w.feed.Flush()
	if len(w.d.cardsIn("eco-A")) != 1 || len(w.d.cardsIn("eco-A2")) != 1 {
		t.Fatalf("the moved route must receive the next card: eco-A=%d eco-A2=%d", len(w.d.cardsIn("eco-A")), len(w.d.cardsIn("eco-A2")))
	}

	w.setRoutes("", "")
	_, err = w.adminCredit(w.hunter, 30, "")
	must(t, err)
	w.feed.Flush()
	if len(w.d.cardsIn("eco-A")) != 1 || len(w.d.cardsIn("eco-A2")) != 1 {
		t.Fatal("a removed route must stop delivery")
	}
	if w.balance(w.hunter) != 60 {
		t.Fatalf("balances are independent of routes, got %d", w.balance(w.hunter))
	}
}

// Tenant isolation: another organization can neither act on this guild's servers nor
// see or move its players' balances.
func TestEconomyIsTenantIsolated(t *testing.T) {
	w := newEconomyWorld(t, "eco-A", "eco-B")
	other := buildInstallationFixture(t, w.a, w.verifier)
	connectNitrado(t, w.a, other.OrgID, other.OwnerDiscordID)
	otherServer := decodeBody[SelectDayZServerResponse](t, selectDayZServer(t, w.a, other.OrgID, other.InstallationID, 111111, other.OwnerDiscordID)).Server.ID
	otherGuild := mustGuildRowID(t, w.a, other.DiscordGuildID)

	// Another organization's identity against this organization's server.
	_, err := w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, ServerID: w.serverA, OrganizationID: other.OrgID, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit, Actor: "x"})
	if !errors.Is(err, economy.ErrForbiddenTenant) {
		t.Fatalf("another organization must not credit through this organization's server, got %v", err)
	}
	// An organization-scoped call must name a server.
	if _, err = w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, OrganizationID: w.fixture.OrgID, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit, Actor: "x"}); !errors.Is(err, economy.ErrServerRequired) {
		t.Fatalf("expected ErrServerRequired, got %v", err)
	}
	// This guild's server under another guild.
	if _, err = w.svc.AdminCredit(w.ctx, economy.Request{GuildID: otherGuild, ServerID: w.serverA, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit, Actor: "x"}); !errors.Is(err, economy.ErrPlayerNotFound) && !errors.Is(err, economy.ErrServerNotInGuild) {
		t.Fatalf("a foreign guild must not reach this guild's players/servers, got %v", err)
	}
	// Another guild's server under this guild.
	if _, err = w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, ServerID: otherServer, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit, Actor: "x"}); !errors.Is(err, economy.ErrServerNotInGuild) {
		t.Fatalf("another guild's server must be rejected, got %v", err)
	}
	// Balance and history reads are guild-scoped.
	if _, err = w.svc.Balance(w.ctx, otherGuild, w.hunter); !errors.Is(err, economy.ErrPlayerNotFound) {
		t.Fatalf("a player must not resolve in another guild, got %v", err)
	}
	if _, err = w.svc.History(w.ctx, otherGuild, w.hunter, 10, 0); !errors.Is(err, economy.ErrPlayerNotFound) {
		t.Fatalf("history must not resolve in another guild, got %v", err)
	}
	if w.balance(w.hunter) != 0 || ledgerRowCount(t, w, w.hunter) != 0 {
		t.Fatal("no rejected call may change anything")
	}

	// The owning organization succeeds and its card reaches only its own routes.
	if _, err = w.svc.AdminCredit(w.ctx, economy.Request{GuildID: w.guildRowID, ServerID: w.serverA, OrganizationID: w.fixture.OrgID, PlayerID: w.hunter, Amount: 5, Type: economy.TypeAdminCredit, Actor: "owner"}); err != nil {
		t.Fatalf("the owning organization must succeed: %v", err)
	}
	w.feed.Flush()
	if len(w.d.cardsIn("eco-A")) != 1 || len(w.d.cardsIn("eco-B")) != 0 {
		t.Fatalf("a server-attributed event goes to that server's route only: eco-A=%d eco-B=%d", len(w.d.cardsIn("eco-A")), len(w.d.cardsIn("eco-B")))
	}
}
