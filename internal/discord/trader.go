package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// The Discord Trader (docs/TRADER.md): a Trader category with a locked voice channel that shows
// whether the trader is open, #trader where players browse and buy from the Champion Shop with
// Champion Points, #deals, and #trader-panel and #trader-orders for the Trader role. Buying goes
// through the website shop's own purchase (same prices, stock, limits, single debit and
// delivery record); everything a player does after Browse is private to them.

// Custom IDs. The router picks the longest matching prefix, so each prefix carries its own
// acknowledgement: TraderPrivatePrefix answers with a new private message (buttons on the shared
// trader post), TraderFormPrefix opens a form (which Discord cannot defer), and the rest update the
// message the button is on.
const (
	TraderPrefix        = "trader:"
	TraderPrivatePrefix = "trader:p:"
	TraderFormPrefix    = "trader:m:"
	traderCoordsPrefix  = "trader:coords:"
	// traderModalPrefix is a submitted form other than the drop point: it updates the message its
	// button was on.
	traderModalPrefix = "trader:f:"
	traderCoordsX     = "trader_x"
	traderCoordsZ     = "trader_z"
)

// The voice channel's name in each state.
const (
	TraderVoiceOpen   = "🟢 Trader: Open"
	TraderVoiceClosed = "🔴 Trader: Closed"
)

// TraderVoiceName is the voice channel's name for an open or closed trader.
func TraderVoiceName(open bool) string {
	if open {
		return TraderVoiceOpen
	}
	return TraderVoiceClosed
}

// traderMaxQuantity is the most a player buys in one order from Discord.
const traderMaxQuantity = 5

// TraderServer is a game server of the Discord server.
type TraderServer struct {
	InstallationID int64
	Name           string
}

// TraderHub is an installation's Trader.
type TraderHub struct {
	InstallationID                  int64
	DiscordGuildID, ServerName      string
	CategoryID, VoiceChannelID      string
	TraderChannelID, DealsChannelID string
	PanelChannelID, OrdersChannelID string
	TraderRoleID                    string
	PostMessageID, PanelMessageID   string
	Open                            bool
	VoiceNameApplied                string
	OpenChangedAt                   *time.Time
	OpenChangedByDiscordID          string
	// DealsRoleID is the opt-in role pinged for new deals and sales.
	DealsRoleID string
	// ClosedNote is what players see while the trader is closed.
	ClosedNote string
	// Opening hours: open at HoursOpen and close at HoursClose (HH:MM in HoursTZ) every day; ""
	// for none.
	HoursOpen, HoursClose, HoursTZ string
}

// TraderCategory is a shop category with its number of items on sale.
type TraderCategory struct {
	ID    int64
	Name  string
	Count int
}

// Stock states, as the shop tells players (never the exact count).
const (
	TraderStockUnlimited = "UNLIMITED"
	TraderStockIn        = "IN_STOCK"
	TraderStockLow       = "LOW_STOCK"
	TraderStockOut       = "OUT_OF_STOCK"
)

// TraderProduct is a shop item as the trader shows it.
type TraderProduct struct {
	ID          int64
	CategoryID  int64
	Name        string
	Description string
	// Price is what the item costs now; with a deal or sale running, ListPrice is the price
	// before it (0 otherwise), PercentOff how much is off and OfferEnds when it ends.
	Price        int64
	ListPrice    int64
	PercentOff   int
	OfferEnds    *time.Time
	Stock        string
	Limit        *int
	Featured     bool
	CategoryName string
	// Coordinates: the buyer gives the X/Z map position to drop it at.
	Coordinates bool
	// Automatic: delivered by the automatic delivery worker, which needs the buyer's logged
	// position; sold on the website only.
	Automatic bool
}

func (p TraderProduct) soldOut() bool { return p.Stock == TraderStockOut }

// TraderCatalog is what the trader post and panel show.
type TraderCatalog struct {
	Categories    []TraderCategory
	Featured      []TraderProduct
	StockAlerts   []TraderProduct
	OrdersWaiting int
	// MapName is the delivery map coordinates are on; "" when staff haven't set one.
	MapName string
	// Offers are the deals and sales running now, soonest ending first.
	Offers []TraderOffer
}

// TraderWallet is a player's Champion Points.
type TraderWallet struct {
	Gamertag string
	Balance  int64
	Recent   []TraderEntry
}

// TraderEntry is one points movement.
type TraderEntry struct {
	At          time.Time
	Amount      int64
	Credit      bool
	Description string
}

// Order statuses as the shop records them.
const (
	TraderOrderWaiting   = "PENDING_FULFILLMENT"
	TraderOrderDelivered = "FULFILLED"
	TraderOrderRefunded  = "REFUNDED"
)

// TraderOrder is one of a player's shop orders.
type TraderOrder struct {
	ID     int64
	Items  string
	Total  int64
	Status string
	At     time.Time
	X, Z   *float64
}

// TraderBuy is an order to place.
type TraderBuy struct {
	ProductID int64
	Quantity  int
	// Key makes a repeated click one order: the same key returns the first order.
	Key  string
	X, Z *float64
}

// TraderReceipt is a placed order.
type TraderReceipt struct {
	OrderID   int64
	Item      string
	Quantity  int
	Total     int64
	Balance   int64
	Gamertag  string
	X, Z      *float64
	MapName   string
	Duplicate bool
	// Saved is how much a deal or sale took off.
	Saved int64
}

// TraderStaff is who runs a Trader besides the Trader role: the Discord roles the server maps to
// Administrator or Owner on the Champion website, and the organization owner's Discord account.
type TraderStaff struct {
	RoleIDs        []string
	OwnerDiscordID string
}

// TraderCaller is the member using the trader, with what the shop needs to know them by.
type TraderCaller struct {
	PanelCaller
	Username, GlobalName, Avatar string
}

// TraderBackend is what the trader needs from the app. Every method is scoped to the Discord server
// the interaction came from: an installation of another Discord server is ErrPanelServerNotFound.
type TraderBackend interface {
	TraderServers(ctx context.Context, discordGuildID string) ([]TraderServer, error)
	// TraderLevel is the caller's staff level on installationID (the server panel's rule).
	TraderLevel(ctx context.Context, c PanelCaller, installationID int64) (permissions.Level, error)
	// TraderHub is installationID's Trader; ErrTraderNotSetUp when it has none.
	TraderHub(ctx context.Context, discordGuildID string, installationID int64) (TraderHub, error)
	TraderHubs(ctx context.Context) ([]TraderHub, error)
	// TraderStaff is who, besides the Trader role, may see and use the private trader channels.
	TraderStaff(ctx context.Context, discordGuildID string, installationID int64) (TraderStaff, error)
	// TraderSaveHub stores what /trader setup made (Administrator; the plan must include the shop).
	TraderSaveHub(ctx context.Context, c PanelCaller, hub TraderHub) (TraderHub, error)
	// TraderSetOpen opens or closes the trader (the Trader role, Administrator or Owner); note is
	// what players see while it is closed. changed is false when it already was.
	TraderSetOpen(ctx context.Context, c PanelCaller, installationID int64, open bool, note string) (hub TraderHub, changed bool, err error)
	TraderSetMessages(ctx context.Context, installationID int64, postMessageID, panelMessageID string) error
	TraderVoiceApplied(ctx context.Context, installationID int64, name string) error
	TraderCatalog(ctx context.Context, discordGuildID string, installationID int64) (TraderCatalog, error)
	// TraderProducts lists the items of categoryID (0: every item), at most 25.
	TraderProducts(ctx context.Context, discordGuildID string, installationID, categoryID int64) ([]TraderProduct, error)
	TraderProduct(ctx context.Context, discordGuildID string, installationID, productID int64) (TraderProduct, error)
	TraderWallet(ctx context.Context, c TraderCaller, installationID int64) (TraderWallet, error)
	TraderOrders(ctx context.Context, c TraderCaller, installationID int64) ([]TraderOrder, error)
	// TraderBuy places an order through the shop, after checking the trader is open.
	TraderBuy(ctx context.Context, c TraderCaller, installationID int64, req TraderBuy) (TraderReceipt, error)

	// TraderAllowed is nil when the caller runs the trader (the Trader role, Administrator or
	// Owner), ErrTraderForbidden otherwise.
	TraderAllowed(ctx context.Context, c PanelCaller, installationID int64) error
	// TraderDeliver marks an order delivered; TraderRefund gives the points back (a reason is
	// required). Both are the shop's own, and ErrTraderOrderDone when the order is no longer waiting.
	TraderDeliver(ctx context.Context, c TraderCaller, installationID, orderID int64) (TraderOrder, error)
	TraderRefund(ctx context.Context, c TraderCaller, installationID, orderID int64, reason string) (TraderOrder, error)
	// TraderStartOffer starts a deal or sale now; TraderEndOffer and TraderEndAllOffers stop them.
	TraderStartOffer(ctx context.Context, c PanelCaller, installationID int64, in TraderOfferInput) (TraderOffer, error)
	TraderEndOffer(ctx context.Context, c PanelCaller, installationID, offerID int64) (TraderOffer, error)
	TraderEndAllOffers(ctx context.Context, c PanelCaller, installationID int64) ([]TraderOffer, error)
	TraderOffers(ctx context.Context, discordGuildID string, installationID int64) ([]TraderOffer, error)
	// TraderOfferPosted records an offer's post in #deals; TraderOfferPostsToEnd lists the offers
	// that are over while their post still shows them, and TraderOfferPostEnded marks one updated.
	TraderOfferPosted(ctx context.Context, installationID, offerID int64, channelID, messageID string) error
	TraderOfferPostsToEnd(ctx context.Context, installationID int64) ([]TraderOffer, error)
	TraderOfferPostEnded(ctx context.Context, installationID, offerID int64) error
	// TraderAnnounced checks the caller may post an announcement and records it.
	TraderAnnounced(ctx context.Context, c PanelCaller, installationID int64, title string) error
	// TraderSetHours sets the opening hours ("" open and close for none) and opens or closes the
	// trader to match them now; changed is whether it did.
	TraderSetHours(ctx context.Context, c PanelCaller, installationID int64, open, close, tz string) (hub TraderHub, changed bool, err error)
	// TraderApplyHours opens or closes the trader when an opening or closing time has passed since
	// the last one applied.
	TraderApplyHours(ctx context.Context, installationID int64) (hub TraderHub, changed bool, err error)
}

var (
	ErrTraderNotSetUp    = errors.New("trader: not set up")
	ErrTraderClosed      = errors.New("trader: closed")
	ErrTraderForbidden   = errors.New("trader: not the trader role or an administrator")
	ErrTraderPlan        = errors.New("trader: the plan does not include the shop")
	ErrTraderNotLinked   = errors.New("trader: no verified gamertag link")
	ErrTraderFunds       = errors.New("trader: not enough points")
	ErrTraderSoldOut     = errors.New("trader: sold out")
	ErrTraderLimit       = errors.New("trader: purchase limit reached")
	ErrTraderUnavailable = errors.New("trader: item not for sale")
	ErrTraderAutomatic   = errors.New("trader: delivered automatically, website only")
	ErrTraderNoMap       = errors.New("trader: no delivery map")
	ErrTraderOffMap      = errors.New("trader: position not on the map")
	ErrTraderSuspended   = errors.New("trader: service paused")
	ErrTraderNoServer    = errors.New("trader: no game server selected")
	ErrTraderBanned      = errors.New("trader: account banned")
	ErrTraderOrderDone   = errors.New("trader: order already handled")
	ErrTraderOrderGone   = errors.New("trader: order not found")
	ErrTraderOfferBad    = errors.New("trader: invalid deal or sale")
	ErrTraderHoursBad    = errors.New("trader: invalid opening hours")
	ErrTraderReason      = errors.New("trader: a reason is required")
)

// TraderHandler serves /trader and every trader button and form.
type TraderHandler struct {
	backend TraderBackend
	now     func() time.Time
	// apiFor is the Discord API used for setup and the shared messages; tests replace it.
	apiFor func(s *discordgo.Session) TraderGuildAPI
}

func NewTraderHandler(b TraderBackend) *TraderHandler {
	return &TraderHandler{backend: b, now: time.Now, apiFor: func(s *discordgo.Session) TraderGuildAPI { return NewTraderSession(s) }}
}

func traderCallerOf(i *discordgo.InteractionCreate) (TraderCaller, bool) {
	pc, ok := panelCallerOf(i)
	if !ok {
		return TraderCaller{}, false
	}
	u := i.Member.User
	return TraderCaller{PanelCaller: pc, Username: u.Username, GlobalName: u.GlobalName, Avatar: u.Avatar}, true
}

const (
	traderNoServersText = "This Discord server has no game server set up on the Champion website yet. Connect one in the Server Hub first."
	traderNotSetUpText  = "The Trader isn't set up on this Discord server yet. An admin can run `/trader setup`."
	traderSetupForbid   = "Only Administrators and the Owner can set up the Trader."
	traderGoneText      = "That game server is no longer set up on this Discord server."
	traderNotLinkedText = "The trader delivers to your in-game character, so it needs to know your gamertag. Link it with `/link`, then come back."
)

// RegisterTraderCommands queues /trader.
func RegisterTraderCommands(s CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	cmd := &discordgo.ApplicationCommand{
		Name:        "trader",
		Description: "Buy from the trader with Champion Points",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "browse", Description: "Open the trader (only you see it)"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "setup", Description: "Create or repair the Trader category, channels and role (Administrator)",
				Options: []*discordgo.ApplicationCommandOption{
					{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "The Trader role (leave empty to keep or create one)"},
					{Type: discordgo.ApplicationCommandOptionInteger, Name: "server", Description: "Game server (when this Discord has several)", Autocomplete: true},
				}},
		},
	}
	_, err = s.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

// HandleCommand answers /trader browse and /trader setup.
func (h *TraderHandler) HandleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	caller, ok := traderCallerOf(i)
	if !ok || h == nil || h.backend == nil {
		respondEphemeral(s, i, ReplyIsUnavailable("The trader"))
		return
	}
	data := i.ApplicationCommandData()
	if i.Type == discordgo.InteractionApplicationCommandAutocomplete {
		h.autocomplete(s, i, caller)
		return
	}
	if len(data.Options) == 0 {
		respondEphemeral(s, i, ReplyIsUnavailable("The trader"))
		return
	}
	sub := data.Options[0]
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	switch sub.Name {
	case "setup":
		var roleID string
		var installationID int64
		for _, opt := range sub.Options {
			switch opt.Name {
			case "role":
				roleID, _ = opt.Value.(string)
			case "server":
				installationID = opt.IntValue()
			}
		}
		h.setup(ctx, s, i, caller, installationID, roleID)
	default:
		hub, err := h.hubFor(ctx, caller.DiscordGuildID, i.ChannelID)
		if err != nil {
			h.reportError(s, i, err)
			return
		}
		h.browse(ctx, s, i, caller, hub, respondPrivate)
	}
}

func (h *TraderHandler) autocomplete(s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	servers, err := h.backend.TraderServers(ctx, caller.DiscordGuildID)
	choices := []*discordgo.ApplicationCommandOptionChoice{}
	if err == nil {
		typed := ""
		for _, opt := range i.ApplicationCommandData().Options {
			for _, o := range opt.Options {
				if o.Focused {
					typed, _ = o.Value.(string)
				}
			}
		}
		for _, srv := range servers {
			if len(choices) == 25 {
				break
			}
			if typed == "" || strings.Contains(strings.ToLower(srv.Name), strings.ToLower(typed)) {
				choices = append(choices, &discordgo.ApplicationCommandOptionChoice{Name: panelClip(srv.Name, 100), Value: srv.InstallationID})
			}
		}
	}
	respondAutocomplete(s, i, choices)
}

// hubFor is the Trader to open from channelID: the one whose channels it is in, else the Discord
// server's first.
func (h *TraderHandler) hubFor(ctx context.Context, discordGuildID, channelID string) (TraderHub, error) {
	servers, err := h.backend.TraderServers(ctx, discordGuildID)
	if err != nil {
		return TraderHub{}, err
	}
	var first *TraderHub
	for _, srv := range servers {
		hub, err := h.backend.TraderHub(ctx, discordGuildID, srv.InstallationID)
		if errors.Is(err, ErrTraderNotSetUp) {
			continue
		}
		if err != nil {
			return TraderHub{}, err
		}
		if channelID != "" && (hub.TraderChannelID == channelID || hub.DealsChannelID == channelID || hub.PanelChannelID == channelID || hub.OrdersChannelID == channelID) {
			return hub, nil
		}
		if first == nil {
			h := hub
			first = &h
		}
	}
	if first == nil {
		return TraderHub{}, ErrTraderNotSetUp
	}
	return *first, nil
}

type traderSend func(*discordgo.Session, *discordgo.InteractionCreate, *discordgo.InteractionResponseData)

// browse answers with the category picker, or why the player can't shop.
func (h *TraderHandler) browse(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, send traderSend) {
	if !hub.Open {
		send(s, i, BuildTraderClosed(hub))
		return
	}
	wallet, err := h.backend.TraderWallet(ctx, caller, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	send(s, i, BuildTraderBrowse(hub, cat, wallet.Balance))
}

// HandleComponent handles every trader button, menu and form.
func (h *TraderHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	caller, ok := traderCallerOf(i)
	if !ok || h == nil || h.backend == nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: ReplyIsUnavailable("The trader")})
		return
	}
	var customID string
	var values []string
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		d := i.MessageComponentData()
		customID, values = d.CustomID, d.Values
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	default:
		return
	}
	if strings.HasPrefix(customID, traderCoordsPrefix) {
		h.submitCoords(s, i, caller, strings.TrimPrefix(customID, traderCoordsPrefix))
		return
	}
	var rest string
	private := strings.HasPrefix(customID, TraderPrivatePrefix)
	form := strings.HasPrefix(customID, TraderFormPrefix)
	submitted := strings.HasPrefix(customID, traderModalPrefix)
	switch {
	case private:
		rest = strings.TrimPrefix(customID, TraderPrivatePrefix)
	case form:
		rest = strings.TrimPrefix(customID, TraderFormPrefix)
	case submitted:
		rest = strings.TrimPrefix(customID, traderModalPrefix)
	default:
		rest = strings.TrimPrefix(customID, TraderPrefix+"u:")
	}
	parts := strings.Split(rest, ":")
	if len(parts) < 2 {
		return
	}
	action := parts[0]
	installationID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || installationID <= 0 {
		return
	}
	args := parts[2:]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if form && h.openForm(ctx, s, i, action, installationID, args, values) {
		return
	}
	hub, err := h.backend.TraderHub(ctx, caller.DiscordGuildID, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	send := respondUpdate
	if private {
		send = respondPrivate
	}
	if submitted {
		h.submitForm(ctx, s, i, caller, hub, action, args)
		return
	}
	if h.staffAction(ctx, s, i, caller, hub, action, args, values, send) {
		return
	}
	switch action {
	case "browse":
		h.browse(ctx, s, i, caller, hub, send)
	case "wallet":
		wallet, err := h.backend.TraderWallet(ctx, caller, installationID)
		if err != nil {
			h.reportError(s, i, err)
			return
		}
		send(s, i, BuildTraderWallet(hub, wallet))
	case "orders":
		if _, err := h.backend.TraderWallet(ctx, caller, installationID); err != nil {
			h.reportError(s, i, err)
			return
		}
		orders, err := h.backend.TraderOrders(ctx, caller, installationID)
		if err != nil {
			h.reportError(s, i, err)
			return
		}
		send(s, i, BuildTraderOrders(hub, orders))
	case "cat":
		categoryID := int64(0)
		if len(values) == 1 && values[0] != "all" {
			categoryID, _ = strconv.ParseInt(values[0], 10, 64)
		} else if len(args) == 1 {
			categoryID, _ = strconv.ParseInt(args[0], 10, 64)
		}
		h.category(ctx, s, i, caller, hub, categoryID, send)
	case "item":
		var productID int64
		switch {
		case len(values) == 1:
			productID, _ = strconv.ParseInt(values[0], 10, 64)
		case len(args) == 1:
			productID, _ = strconv.ParseInt(args[0], 10, 64)
		}
		if productID <= 0 {
			return
		}
		h.item(ctx, s, i, caller, hub, productID, send)
	case "qty", "buy":
		if len(args) < 1 {
			return
		}
		productID, _ := strconv.ParseInt(args[0], 10, 64)
		qty := 1
		if action == "qty" && len(values) == 1 {
			qty, _ = strconv.Atoi(values[0])
		}
		h.confirm(ctx, s, i, caller, hub, productID, qty)
	case "pay":
		productID, qty, nonce, ok := parseTraderOrderArgs(args)
		if !ok {
			return
		}
		h.place(ctx, s, i, caller, hub, TraderBuy{ProductID: productID, Quantity: qty, Key: traderOrderKey(nonce)})
	case "where":
		productID, qty, nonce, ok := parseTraderOrderArgs(args)
		if !ok {
			return
		}
		cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, installationID)
		if err != nil {
			h.reportError(s, i, err)
			return
		}
		if err := respondModalData(s, i, BuildTraderCoordsForm(installationID, productID, qty, nonce, cat.MapName)); err != nil {
			slog.Warn("component=discord", "msg", "trader coordinates form failed", "err", err.Error())
		}
	case "cancel":
		send(s, i, &discordgo.InteractionResponseData{Content: "Cancelled. No points were taken.", Embeds: []*discordgo.MessageEmbed{}, Components: []discordgo.MessageComponent{}})
	case "open", "close":
		h.setOpen(ctx, s, i, caller, hub, action == "open", "")
	case "panel":
		h.refreshPanel(ctx, s, i, hub)
	}
}

func parseTraderOrderArgs(args []string) (productID int64, qty int, nonce string, ok bool) {
	if len(args) != 3 {
		return 0, 0, "", false
	}
	productID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || productID <= 0 {
		return 0, 0, "", false
	}
	qty, err = strconv.Atoi(args[1])
	if err != nil || qty < 1 || qty > traderMaxQuantity {
		return 0, 0, "", false
	}
	if !validTraderNonce(args[2]) {
		return 0, 0, "", false
	}
	return productID, qty, args[2], true
}

// traderNonce is a fresh order key part: one confirm screen, one order however often it's clicked.
func traderNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func validTraderNonce(s string) bool {
	if len(s) < 8 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func traderOrderKey(nonce string) string { return "discord-trader." + nonce }

func (h *TraderHandler) category(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, categoryID int64, send traderSend) {
	if !hub.Open {
		send(s, i, BuildTraderClosed(hub))
		return
	}
	wallet, err := h.backend.TraderWallet(ctx, caller, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	products, err := h.backend.TraderProducts(ctx, caller.DiscordGuildID, hub.InstallationID, categoryID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	name := "All items"
	for _, c := range cat.Categories {
		if c.ID == categoryID {
			name = c.Name
		}
	}
	send(s, i, BuildTraderCategory(hub, categoryID, name, products, wallet.Balance))
}

func (h *TraderHandler) item(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, productID int64, send traderSend) {
	if !hub.Open {
		send(s, i, BuildTraderClosed(hub))
		return
	}
	wallet, err := h.backend.TraderWallet(ctx, caller, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	p, err := h.backend.TraderProduct(ctx, caller.DiscordGuildID, hub.InstallationID, productID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	send(s, i, BuildTraderItem(hub, p, wallet.Balance, cat.MapName))
}

func (h *TraderHandler) confirm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, productID int64, qty int) {
	if qty < 1 || qty > traderMaxQuantity {
		return
	}
	if !hub.Open {
		respondUpdate(s, i, BuildTraderClosed(hub))
		return
	}
	wallet, err := h.backend.TraderWallet(ctx, caller, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	p, err := h.backend.TraderProduct(ctx, caller.DiscordGuildID, hub.InstallationID, productID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderConfirm(hub, p, qty, wallet.Balance, traderNonce()))
}

func (h *TraderHandler) submitCoords(s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, rest string) {
	parts := strings.Split(rest, ":")
	if len(parts) != 4 {
		return
	}
	installationID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return
	}
	productID, qty, nonce, ok := parseTraderOrderArgs(parts[1:])
	if !ok {
		return
	}
	data := i.ModalSubmitData()
	x, errX := parseTraderCoordinate(modalValue(data, traderCoordsX))
	z, errZ := parseTraderCoordinate(modalValue(data, traderCoordsZ))
	if errX != nil || errZ != nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "Enter the X and Z coordinates as numbers, like 4621 and 10318. Nothing was bought."})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hub, err := h.backend.TraderHub(ctx, caller.DiscordGuildID, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	h.place(ctx, s, i, caller, hub, TraderBuy{ProductID: productID, Quantity: qty, Key: traderOrderKey(nonce), X: &x, Z: &z})
}

func parseTraderCoordinate(s string) (float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", ""))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v > 100000 {
		return 0, errors.New("not a coordinate")
	}
	return v, nil
}

// place buys the order and, the first time it goes through, tells the Trader role and staff in
// #trader-orders.
func (h *TraderHandler) place(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, req TraderBuy) {
	receipt, err := h.backend.TraderBuy(ctx, caller, hub.InstallationID, req)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderReceipt(hub, receipt))
	if receipt.Duplicate {
		return
	}
	api := h.apiFor(s)
	if api == nil {
		return
	}
	if hub.OrdersChannelID != "" {
		if _, err := api.ChannelMessageSendComplex(hub.OrdersChannelID, BuildTraderOrderCard(hub, caller.DiscordUserID, receipt)); err != nil {
			slog.Warn("component=discord", "msg", "trader order card failed", "installation_id", hub.InstallationID, "err", err.Error())
		}
	}
	// Stock and the orders count changed.
	h.publish(context.WithoutCancel(ctx), api, hub)
}

func (h *TraderHandler) setOpen(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, open bool, note string) {
	updated, changed, err := h.backend.TraderSetOpen(ctx, caller.PanelCaller, hub.InstallationID, open, note)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	api := h.apiFor(s)
	cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderPanel(updated, cat, h.now()))
	if api == nil || !changed && (open || note == hub.ClosedNote) {
		return
	}
	h.publish(context.WithoutCancel(ctx), api, updated)
	h.syncVoice(context.WithoutCancel(ctx), api, updated)
}

func (h *TraderHandler) refreshPanel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, hub TraderHub) {
	cat, err := h.backend.TraderCatalog(ctx, hub.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderPanel(hub, cat, h.now()))
}

func (h *TraderHandler) reportError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	text := ReplyCouldNot("reach the trader")
	switch {
	case errors.Is(err, ErrTraderNotSetUp):
		text = traderNotSetUpText
	case errors.Is(err, ErrPanelServerNotFound):
		text = traderGoneText
	case errors.Is(err, ErrTraderClosed):
		text = "🔴 The trader is closed. Nothing was bought and no points were taken. Watch the Trader voice channel for when it opens."
	case errors.Is(err, ErrTraderForbidden):
		text = "Only the Trader role and the server's Owner and Administrators can use the trader panel."
	case errors.Is(err, ErrPanelForbidden):
		text = traderSetupForbid
	case errors.Is(err, ErrTraderPlan):
		text = "The trader uses the Champion Shop, which isn't part of this server's Champion plan."
	case errors.Is(err, ErrTraderNotLinked):
		text = traderNotLinkedText
	case errors.Is(err, ErrTraderFunds):
		text = "You don't have enough Champion Points for that. Nothing was bought. Earn points with kills, streaks, events and bounties."
	case errors.Is(err, ErrTraderSoldOut):
		text = "That item just sold out. Nothing was bought and no points were taken."
	case errors.Is(err, ErrTraderLimit):
		text = "You've reached this item's purchase limit. Nothing was bought."
	case errors.Is(err, ErrTraderUnavailable):
		text = "That item isn't for sale any more. Nothing was bought."
	case errors.Is(err, ErrTraderAutomatic):
		text = "This item is delivered to where you stand in game, which only works from the Champion website shop. Nothing was bought."
	case errors.Is(err, ErrTraderNoMap):
		text = "Staff haven't set the map for drop-off deliveries yet, so this item can't be bought right now. Nothing was bought."
	case errors.Is(err, ErrTraderOffMap):
		text = "Those coordinates aren't on the map. Check them on the in-game map or iZurvive and try again. Nothing was bought."
	case errors.Is(err, ErrTraderSuspended):
		text = "Champion is paused for this server, so the trader is too. Nothing was bought."
	case errors.Is(err, ErrTraderNoServer):
		text = "This Discord server has no game server selected on the Champion website yet."
	case errors.Is(err, ErrTraderBanned):
		text = "Your Champion account can't use the shop. Contact the server's staff."
	case errors.Is(err, ErrTraderOrderDone):
		text = "That order was already delivered or refunded."
	case errors.Is(err, ErrTraderOrderGone):
		text = "That order no longer exists."
	case errors.Is(err, ErrTraderOfferBad):
		text = fmt.Sprintf("Enter a percentage off from 1 to %d and a length from 1 to %d hours. Nothing was changed.", TraderMaxPercentOff, TraderMaxOfferHours)
	case errors.Is(err, ErrTraderHoursBad):
		text = traderHoursHelp
	case errors.Is(err, ErrTraderReason):
		text = "Write a reason for the refund. Nothing was refunded."
	case errors.Is(err, ErrPanelRateLimited):
		text = "Slow down a little and try again in a minute."
	default:
		slog.Warn("component=discord", "msg", "trader action failed", "err", err.Error())
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: text})
}
