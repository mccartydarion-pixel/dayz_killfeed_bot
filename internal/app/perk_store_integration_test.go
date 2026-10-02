//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/perkstore"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/vip"
)

// perkWorld is a Client Admin world with the perk store, supporter tiers, the economy and a fake
// Nitrado priority list, plus an owner who has a linked in-game character.
type perkWorld struct {
	*clientAdminWorld
	nitrado    *fakeNitradoPriority
	shoutouts  []*discordgo.MessageEmbed
	ownerID    int64 // the owner's in-game character
	tierID     int64
	keySeq     int
	playerBase string
}

func newPerkWorld(t *testing.T) *perkWorld {
	t.Helper()
	w := &perkWorld{clientAdminWorld: newClientAdminWorld(t)}
	a := w.a
	econ := repository.NewEconomyRepository(a.DB.Pool)
	a.EconomyService = economy.NewService(econ, nil)
	a.EconomyAccounts = economy.NewAccounts(a.EconomyService, econ)
	a.VIP = repository.NewVIPRepository(a.DB.Pool)
	a.Perks = repository.NewPerkStoreRepository(a.DB.Pool)
	a.perkInline = true
	a.perkAnnouncer = func(_ repository.PerkPurchase, e *discordgo.MessageEmbed) { w.shoutouts = append(w.shoutouts, e) }
	w.nitrado = withFakeNitradoPriority(t, a, true, "")
	connectNitrado(t, a, w.f.OrgID, w.f.OwnerDiscordID)
	w.ownerID = w.linked(w.f.OwnerDiscordID, "TheOwner")
	tier, err := a.VIP.SaveTier(context.Background(), w.guildID, vip.Tier{Name: fmt.Sprintf("Gold %d", time.Now().UnixNano()), Badge: "GOLD", Color: "#E7B94A", RewardMultiplier: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.tierID = tier.ID
	w.playerBase = fmt.Sprintf("/api/saas/organizations/%d/installations/%d/perks", w.f.OrgID, w.f.InstallationID)
	if rr := w.call(a.handleSavePerkSettings, http.MethodPut, w.path("/perks/settings"), w.f.OwnerDiscordID, repository.PerkSettings{Enabled: true, Shoutouts: true}, nil); rr.Code != http.StatusOK {
		t.Fatalf("open the store: %d %s", rr.Code, rr.Body.String())
	}
	return w
}

// linked creates an in-game character with a verified link to discordID and returns its id.
func (w *perkWorld) linked(discordID, name string) int64 {
	w.t.Helper()
	id := w.seedPlayer(fmt.Sprintf("%s%d", name, time.Now().UnixNano()%1_000_000))
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_links(guild_id,player_id,discord_user_id,status,verified_at) VALUES($1,$2,$3,'VERIFIED',NOW())`, w.guildID, id, discordID); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// player makes a signed-in website user with a linked character holding points.
func (w *perkWorld) player(name string, points int64) (discordID string, playerID int64) {
	w.t.Helper()
	user := syncUser(w.t, w.a, fmt.Sprintf("perk-%s-%d", strings.ToLower(name), time.Now().UnixNano()), name)
	playerID = w.linked(user.DiscordUserID, name)
	if points > 0 {
		if _, err := repository.NewEconomyRepository(w.a.DB.Pool).Credit(context.Background(), repository.LedgerParams{GuildID: w.guildID, PlayerID: playerID, Type: repository.TxAdminCredit, Amount: points, CreatedBy: "test"}); err != nil {
			w.t.Fatal(err)
		}
	}
	return user.DiscordUserID, playerID
}

func (w *perkWorld) balance(playerID int64) int64 {
	w.t.Helper()
	b, err := repository.NewEconomyRepository(w.a.DB.Pool).Balance(context.Background(), w.guildID, playerID)
	if err != nil {
		w.t.Fatal(err)
	}
	return b
}

func (w *perkWorld) offer(o perkstore.Offer) perkstore.Offer {
	w.t.Helper()
	if o.Name == "" {
		o.Name = fmt.Sprintf("Offer %d", time.Now().UnixNano())
	}
	o.Enabled = true
	rr := w.call(w.a.handleSavePerkOffer, http.MethodPut, w.path("/perks/offers"), w.f.OwnerDiscordID, o, nil)
	if rr.Code != http.StatusOK {
		w.t.Fatalf("save offer: %d %s", rr.Code, rr.Body.String())
	}
	return decodeBody[perkstore.Offer](w.t, rr)
}

type perkBuyResponse struct {
	Purchase         repository.PerkPurchase `json:"purchase"`
	RemainingBalance int64                   `json:"remainingBalance"`
	Duplicate        bool                    `json:"duplicate"`
}

func (w *perkWorld) buy(actor string, offerID, recipient int64, key string) *httptest.ResponseRecorder {
	w.t.Helper()
	if key == "" {
		w.keySeq++
		key = fmt.Sprintf("perk-test-key-%d-%d", time.Now().UnixNano(), w.keySeq)
	}
	return w.call(w.a.handlePerkPurchase, http.MethodPost, w.playerBase+"/purchases", actor, perkPurchaseBody{OfferID: offerID, RecipientPlayerID: recipient, IdempotencyKey: key}, nil)
}

func (w *perkWorld) purchase(id int64) repository.PerkPurchase {
	w.t.Helper()
	p, err := w.a.Perks.GetPurchase(context.Background(), w.guildID, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *perkWorld) activeTier(playerID int64) bool {
	w.t.Helper()
	var ok bool
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM vip_members WHERE guild_id=$1 AND player_id=$2 AND tier_id=$3 AND revoked_at IS NULL)`, w.guildID, playerID, w.tierID).Scan(&ok); err != nil {
		w.t.Fatal(err)
	}
	return ok
}

func (w *perkWorld) onPriorityList(name string) bool {
	w.nitrado.mu.Lock()
	defer w.nitrado.mu.Unlock()
	for _, line := range strings.Split(w.nitrado.value, "\r\n") {
		if strings.EqualFold(line, name) {
			return true
		}
	}
	return false
}

func (w *perkWorld) name(playerID int64) string {
	w.t.Helper()
	var name string
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT display_name FROM players WHERE id=$1`, playerID).Scan(&name); err != nil {
		w.t.Fatal(err)
	}
	return name
}

func TestPerkPurchaseChargesPaysTheOwnerAndGrantsPerks(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Buyer", 1000)
	offer := w.offer(perkstore.Offer{PricePoints: 300, Billing: perkstore.BillingOneTime, DurationDays: 30, VIPTierID: &w.tierID, PriorityQueue: true, CustomPerk: "Custom nameplate", Giftable: true})

	rr := w.buy(buyer, offer.ID, 0, "first-purchase-key")
	if rr.Code != http.StatusCreated {
		t.Fatalf("purchase: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[perkBuyResponse](t, rr)
	if got.RemainingBalance != 700 || w.balance(buyerID) != 700 {
		t.Fatalf("the buyer should have 700 left, got %d / %d", got.RemainingBalance, w.balance(buyerID))
	}
	if w.balance(w.ownerID) != 300 {
		t.Fatalf("the owner should have received 300, has %d", w.balance(w.ownerID))
	}
	if !got.Purchase.PerksApplied || got.Purchase.PerksError != "" {
		t.Fatalf("perks should be in place: %+v", got.Purchase)
	}
	if !w.activeTier(buyerID) || !w.onPriorityList(w.name(buyerID)) {
		t.Fatal("the buyer should hold the tier and be on the priority list")
	}
	if len(w.shoutouts) != 1 || !strings.Contains(w.shoutouts[0].Description, offer.Name) {
		t.Fatalf("expected one shout-out naming the offer, got %d", len(w.shoutouts))
	}

	// The same request again charges nothing and announces nothing.
	again := w.buy(buyer, offer.ID, 0, "first-purchase-key")
	if again.Code != http.StatusOK || !decodeBody[perkBuyResponse](t, again).Duplicate || w.balance(buyerID) != 700 || len(w.shoutouts) != 1 {
		t.Fatalf("a replay must not charge again: %d %s", again.Code, again.Body.String())
	}
	// A second purchase of the offer the player already holds is refused.
	if rr := w.buy(buyer, offer.ID, 0, ""); rr.Code != http.StatusConflict || w.balance(buyerID) != 700 {
		t.Fatalf("holding the offer already must refuse: %d %s", rr.Code, rr.Body.String())
	}

	// The Donate tab shows the balance, the held offer and the board.
	view := decodeBody[map[string]any](t, w.call(w.a.handlePerkStore, http.MethodGet, w.playerBase, buyer, nil, nil))
	if view["linked"] != true || view["balance"].(float64) != 700 || len(view["mine"].([]any)) != 1 {
		t.Fatalf("unexpected player view: %v", view)
	}
	if offers := view["offers"].([]any); len(offers) != 1 || offers[0].(map[string]any)["owned"] != true {
		t.Fatalf("the offer should be listed as owned: %v", view["offers"])
	}
	month := view["topSupporters"].(map[string]any)["month"].([]any)
	if len(month) != 1 || month[0].(map[string]any)["points"].(float64) != 300 {
		t.Fatalf("the buyer should top the board with 300: %v", month)
	}

	// When the time runs out the purchase ends and the store takes the player off the priority list.
	w.a.runPerkStore(context.Background(), w.guildID, time.Now().UTC().AddDate(0, 0, 31))
	ended := w.purchase(got.Purchase.ID)
	if ended.Status != repository.PerkEnded || ended.EndReason != perkReasonExpired || !ended.PerksRemoved {
		t.Fatalf("the purchase should have expired: %+v", ended)
	}
	if w.onPriorityList(w.name(buyerID)) {
		t.Fatal("an expired purchase must leave the priority list")
	}
}

func TestPerkPurchaseRefusesWhatItShould(t *testing.T) {
	w := newPerkWorld(t)
	rich, richID := w.player("Rich", 5000)
	poor, poorID := w.player("Poor", 50)
	_, friendID := w.player("Friend", 0)
	limited := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingOneTime, CustomPerk: "Founder title", Giftable: true, StockLimit: ptrInt(1)})
	personal := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingOneTime, CustomPerk: "Personal thing", Giftable: false})

	if rr := w.buy(poor, personal.ID, 0, ""); rr.Code != http.StatusConflict || w.balance(poorID) != 50 {
		t.Fatalf("too few points must refuse and charge nothing: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.buy(rich, personal.ID, friendID, ""); rr.Code != http.StatusConflict || w.balance(richID) != 5000 {
		t.Fatalf("an offer that cannot be gifted must refuse: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.buy(rich, limited.ID, 999999999, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("a gift to nobody must be 404: %d", rr.Code)
	}

	// A gift: the buyer pays, the friend holds it, and the shout-out says so.
	gift := w.buy(rich, limited.ID, friendID, "")
	if gift.Code != http.StatusCreated {
		t.Fatalf("gift: %d %s", gift.Code, gift.Body.String())
	}
	p := decodeBody[perkBuyResponse](t, gift).Purchase
	if !p.Gift || p.RecipientPlayerID != friendID || p.BuyerPlayerID != richID || w.balance(richID) != 4900 {
		t.Fatalf("unexpected gift: %+v", p)
	}
	if len(w.shoutouts) != 1 || !strings.Contains(w.shoutouts[0].Title, "gifted") {
		t.Fatal("a gift gets a gift shout-out")
	}
	// The one unit is gone.
	if rr := w.buy(rich, limited.ID, 0, ""); rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "sold out") {
		t.Fatalf("a sold-out offer must refuse: %d %s", rr.Code, rr.Body.String())
	}

	// A closed store sells nothing and lists nothing.
	if rr := w.call(w.a.handleSavePerkSettings, http.MethodPut, w.path("/perks/settings"), w.f.OwnerDiscordID, repository.PerkSettings{Enabled: false}, nil); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := w.buy(rich, personal.ID, 0, ""); rr.Code != http.StatusConflict {
		t.Fatalf("a closed store must refuse: %d", rr.Code)
	}
	view := decodeBody[map[string]any](t, w.call(w.a.handlePerkStore, http.MethodGet, w.playerBase, rich, nil, nil))
	if view["enabled"] != false || len(view["offers"].([]any)) != 0 {
		t.Fatalf("a closed store lists no offers: %v", view)
	}
}

func TestPerkMonthlyRenewsUntilThePlayerCannotPay(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Subscriber", 250)
	offer := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingMonthly, VIPTierID: &w.tierID})

	rr := w.buy(buyer, offer.ID, 0, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("subscribe: %d %s", rr.Code, rr.Body.String())
	}
	id := decodeBody[perkBuyResponse](t, rr).Purchase.ID
	start := w.purchase(id)
	if !start.AutoRenew || start.ExpiresAt == nil || w.balance(buyerID) != 150 {
		t.Fatalf("a monthly purchase renews and is paid for a month: %+v", start)
	}

	// Not due yet: nothing happens.
	w.a.runPerkStore(context.Background(), w.guildID, time.Now().UTC().AddDate(0, 0, 10))
	if p := w.purchase(id); p.Renewals != 0 || w.balance(buyerID) != 150 {
		t.Fatalf("nothing is charged before the month is up: %+v", p)
	}
	// Due: charged again, a month added, the owner paid again, the tier still held.
	first := start.ExpiresAt.Add(time.Minute)
	w.a.runPerkStore(context.Background(), w.guildID, first)
	renewed := w.purchase(id)
	if renewed.Renewals != 1 || renewed.Status != repository.PerkActive || !renewed.ExpiresAt.After(first) || w.balance(buyerID) != 50 || w.balance(w.ownerID) != 200 {
		t.Fatalf("the renewal should have charged 100 more: %+v (buyer %d, owner %d)", renewed, w.balance(buyerID), w.balance(w.ownerID))
	}
	// Running the same moment again never charges twice.
	w.a.runPerkStore(context.Background(), w.guildID, first)
	if w.balance(buyerID) != 50 {
		t.Fatal("a renewal must not be charged twice")
	}
	// Next time the player cannot pay: it ends, and nothing is taken.
	w.a.runPerkStore(context.Background(), w.guildID, renewed.ExpiresAt.Add(time.Minute))
	ended := w.purchase(id)
	if ended.Status != repository.PerkEnded || ended.EndReason != perkReasonNoCredits || w.balance(buyerID) != 50 {
		t.Fatalf("an unpaid renewal ends the purchase: %+v", ended)
	}
}

func TestPerkAutoRenewOffEndsAtThePaidTime(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Canceller", 500)
	stranger, _ := w.player("Stranger", 0)
	offer := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingMonthly, CustomPerk: "Monthly thanks"})
	id := decodeBody[perkBuyResponse](t, w.buy(buyer, offer.ID, 0, "")).Purchase.ID
	path := fmt.Sprintf("%s/purchases/%d/auto-renew", w.playerBase, id)
	extra := map[string]string{"purchaseID": fmt.Sprint(id)}

	if rr := w.call(w.a.handlePerkAutoRenew, http.MethodPost, path, stranger, perkAutoRenewBody{Enabled: false}, extra); rr.Code != http.StatusNotFound {
		t.Fatalf("only the buyer may change the renewal: %d", rr.Code)
	}
	if rr := w.call(w.a.handlePerkAutoRenew, http.MethodPost, path, buyer, perkAutoRenewBody{Enabled: false}, extra); rr.Code != http.StatusOK {
		t.Fatalf("cancel renewal: %d %s", rr.Code, rr.Body.String())
	}
	w.a.runPerkStore(context.Background(), w.guildID, w.purchase(id).ExpiresAt.Add(time.Minute))
	if p := w.purchase(id); p.Status != repository.PerkEnded || p.EndReason != perkReasonExpired || w.balance(buyerID) != 400 {
		t.Fatalf("with renewal off the purchase simply runs out: %+v", p)
	}
}

func TestPerkRefundGivesThePointsBackAndTakesThePerks(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Refundee", 400)
	offer := w.offer(perkstore.Offer{PricePoints: 400, Billing: perkstore.BillingOneTime, DurationDays: 30, VIPTierID: &w.tierID, PriorityQueue: true, StockLimit: ptrInt(1)})
	id := decodeBody[perkBuyResponse](t, w.buy(buyer, offer.ID, 0, "")).Purchase.ID
	path := w.path(fmt.Sprintf("/perks/purchases/%d/refund", id))
	extra := map[string]string{"purchaseID": fmt.Sprint(id)}

	// The owner spent the points: the refund is refused and nothing moves.
	econ := repository.NewEconomyRepository(w.a.DB.Pool)
	if _, err := econ.Debit(context.Background(), repository.LedgerParams{GuildID: w.guildID, PlayerID: w.ownerID, Type: repository.TxAdminDebit, Amount: 100, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if rr := w.call(w.a.handleRefundPerkPurchase, http.MethodPost, path, w.f.OwnerDiscordID, nil, extra); rr.Code != http.StatusConflict || w.balance(buyerID) != 0 || w.purchase(id).Status != repository.PerkActive {
		t.Fatalf("a refund the owner cannot cover must change nothing: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := econ.Credit(context.Background(), repository.LedgerParams{GuildID: w.guildID, PlayerID: w.ownerID, Type: repository.TxAdminCredit, Amount: 100, CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}

	rr := w.call(w.a.handleRefundPerkPurchase, http.MethodPost, path, w.f.OwnerDiscordID, nil, extra)
	if rr.Code != http.StatusOK {
		t.Fatalf("refund: %d %s", rr.Code, rr.Body.String())
	}
	if w.balance(buyerID) != 400 || w.balance(w.ownerID) != 0 {
		t.Fatalf("the points go back to the buyer from the owner: buyer %d, owner %d", w.balance(buyerID), w.balance(w.ownerID))
	}
	p := w.purchase(id)
	if p.Status != repository.PerkRefunded || !p.PerksRemoved || w.activeTier(buyerID) || w.onPriorityList(w.name(buyerID)) {
		t.Fatalf("a refund takes the perks back: %+v", p)
	}
	if rr := w.call(w.a.handleRefundPerkPurchase, http.MethodPost, path, w.f.OwnerDiscordID, nil, extra); rr.Code != http.StatusBadRequest || w.balance(buyerID) != 400 {
		t.Fatalf("a purchase is refunded once: %d", rr.Code)
	}
	// The unit is back on sale and off the board.
	if rr := w.buy(buyer, offer.ID, 0, ""); rr.Code != http.StatusCreated {
		t.Fatalf("the refunded unit should be for sale again: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPerkPriorityAddedByStaffIsNotRemovedByTheStore(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Regular", 300)
	// Staff had already put this player on the list by hand.
	w.nitrado.mu.Lock()
	w.nitrado.value = w.name(buyerID)
	w.nitrado.mu.Unlock()
	offer := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingOneTime, DurationDays: 7, PriorityQueue: true})
	id := decodeBody[perkBuyResponse](t, w.buy(buyer, offer.ID, 0, "")).Purchase.ID
	w.a.runPerkStore(context.Background(), w.guildID, time.Now().UTC().AddDate(0, 0, 8))
	if p := w.purchase(id); p.Status != repository.PerkEnded || !p.PerksRemoved {
		t.Fatalf("the purchase should have ended cleanly: %+v", p)
	}
	if !w.onPriorityList(w.name(buyerID)) {
		t.Fatal("an entry staff added themselves is not the store's to remove")
	}
}

func TestPerkFailedPriorityIsKeptForStaffAndRetried(t *testing.T) {
	w := newPerkWorld(t)
	buyer, buyerID := w.player("Unlucky", 300)
	offer := w.offer(perkstore.Offer{PricePoints: 100, Billing: perkstore.BillingOneTime, DurationDays: 7, PriorityQueue: true})
	// The server has no priority setting: the player is still charged once, and staff are told.
	w.nitrado.mu.Lock()
	w.nitrado.present = false
	w.nitrado.mu.Unlock()
	rr := w.buy(buyer, offer.ID, 0, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("purchase: %d %s", rr.Code, rr.Body.String())
	}
	p := decodeBody[perkBuyResponse](t, rr).Purchase
	if p.PerksApplied || !strings.Contains(p.PerksError, "priority") || w.balance(buyerID) != 200 {
		t.Fatalf("the failure should be recorded for staff: %+v", p)
	}
	// Once Nitrado answers, the scheduler finishes the job.
	w.nitrado.mu.Lock()
	w.nitrado.present = true
	w.nitrado.mu.Unlock()
	w.a.runPerkStore(context.Background(), w.guildID, time.Now().UTC())
	if p := w.purchase(p.ID); !p.PerksApplied || p.PerksError != "" || !w.onPriorityList(w.name(buyerID)) {
		t.Fatalf("the retry should have put the player on the list: %+v", p)
	}
}

func TestPerkControlHubPermissionsAndValidation(t *testing.T) {
	w := newPerkWorld(t)
	stranger, _ := w.player("Nobody", 0)
	if rr := w.call(w.a.handlePerkStoreAdmin, http.MethodGet, w.path("/perks"), stranger, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("a player must not open the control hub: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSavePerkOffer, http.MethodPut, w.path("/perks/offers"), stranger, perkstore.Offer{Name: "X", PricePoints: 1, Billing: perkstore.BillingOneTime, PriorityQueue: true}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("a player must not create offers: %d", rr.Code)
	}
	for name, bad := range map[string]perkstore.Offer{
		"no perk":      {Name: "Nothing", PricePoints: 10, Billing: perkstore.BillingOneTime},
		"free":         {Name: "Free", PricePoints: 0, Billing: perkstore.BillingOneTime, PriorityQueue: true},
		"unknown tier": {Name: "Ghost tier", PricePoints: 10, Billing: perkstore.BillingOneTime, VIPTierID: ptrInt64(987654321)},
	} {
		if rr := w.call(w.a.handleSavePerkOffer, http.MethodPut, w.path("/perks/offers"), w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	offer := w.offer(perkstore.Offer{Name: "Queue pass", PricePoints: 50, Billing: perkstore.BillingOneTime, DurationDays: 7, PriorityQueue: true})
	offer.PricePoints = 75
	if rr := w.call(w.a.handleSavePerkOffer, http.MethodPut, w.path("/perks/offers"), w.f.OwnerDiscordID, offer, nil); rr.Code != http.StatusOK || decodeBody[perkstore.Offer](t, rr).PricePoints != 75 {
		t.Fatalf("editing an offer should save: %d %s", rr.Code, rr.Body.String())
	}
	hub := decodeBody[map[string]any](t, w.call(w.a.handlePerkStoreAdmin, http.MethodGet, w.path("/perks"), w.f.OwnerDiscordID, nil, nil))
	if hub["ownerLinked"] != true || len(hub["offers"].([]any)) != 1 || len(hub["tiers"].([]any)) != 1 {
		t.Fatalf("unexpected control hub: %v", hub)
	}
	extra := map[string]string{"offerID": fmt.Sprint(offer.ID)}
	if rr := w.call(w.a.handleArchivePerkOffer, http.MethodDelete, w.path(fmt.Sprintf("/perks/offers/%d", offer.ID)), w.f.OwnerDiscordID, nil, extra); rr.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	buyer, _ := w.player("Late", 500)
	if rr := w.buy(buyer, offer.ID, 0, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("an archived offer cannot be bought: %d", rr.Code)
	}
}

func ptrInt(v int) *int       { return &v }
func ptrInt64(v int64) *int64 { return &v }

func perkChannelRouted(t *testing.T, w *clientAdminWorld) bool {
	t.Helper()
	routes, err := w.a.SaaSChannelRoutes.ListForInstallation(context.Background(), w.f.OrgID, w.f.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.RouteKey == "DONATION_PERKS" && r.ChannelID != "" {
			return true
		}
	}
	return false
}

// The donations channel belongs to servers that sell something: a layout run for a server whose
// store is closed never makes it, and opening the store does.
func TestPerkChannelIsCreatedOnlyForAnOpenStore(t *testing.T) {
	w := newClientAdminWorld(t)
	a := w.a
	econ := repository.NewEconomyRepository(a.DB.Pool)
	a.EconomyService = economy.NewService(econ, nil)
	a.Perks = repository.NewPerkStoreRepository(a.DB.Pool)
	a.perkInline = true
	ctx := context.Background()

	if _, err := a.runChannelLayout(ctx, w.f.OrgID, w.f.InstallationID, w.f.DiscordGuildID, true); err != nil {
		t.Fatalf("layout with the store closed: %v", err)
	}
	if perkChannelRouted(t, w) {
		t.Fatal("a closed store must not get a donations channel")
	}
	if p := a.installationRouteProducers(ctx, w.f.InstallationID)["DONATION_PERKS"]; p.Health != HealthDisabled {
		t.Fatalf("a closed store's route is DISABLED, got %s", p.Health)
	}

	rr := w.call(a.handleSavePerkSettings, http.MethodPut, w.path("/perks/settings"), w.f.OwnerDiscordID, repository.PerkSettings{Enabled: true, Shoutouts: true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("open the store: %d %s", rr.Code, rr.Body.String())
	}
	if a.installationRouteProducers(ctx, w.f.InstallationID)["DONATION_PERKS"].Health != HealthActive {
		t.Fatal("an open store's route is ACTIVE")
	}
	if !perkChannelRouted(t, w) {
		t.Fatal("opening the store should create and route the donations channel")
	}
}
