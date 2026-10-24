package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Trader phase 2 (docs/TRADER.md): the panel's deals, sales, announcements, opening hours and
// closing note; Mark delivered and Refund on the order cards; and the opt-in @Deals role.

// Deal and sale kinds, as the shop stores them.
const (
	TraderOfferDeal = "DEAL"
	TraderOfferSale = "SALE"
	// TraderMaxPercentOff caps a deal or sale so a typo can't give items away.
	TraderMaxPercentOff = 75
	// TraderMaxOfferHours is the longest a deal or sale runs (30 days).
	TraderMaxOfferHours = 720
)

// TraderOffer is a deal (one item) or sale (a category, or every item) on the shop.
type TraderOffer struct {
	ID         int64
	Kind       string
	ProductID  int64
	CategoryID int64
	// Name is the item's or category's name; "" for a sale on every item.
	Name          string
	PercentOff    int
	Title         string
	EndsAt        time.Time
	Ended         bool
	PostChannelID string
	PostMessageID string
}

// TraderOfferInput is a deal or sale to start.
type TraderOfferInput struct {
	Kind                  string
	ProductID, CategoryID int64
	PercentOff, Hours     int
	Title                 string
}

// Form field ids.
const (
	traderFieldPercent   = "trader_pct"
	traderFieldHours     = "trader_hours"
	traderFieldTitle     = "trader_title"
	traderFieldNote      = "trader_note"
	traderFieldReason    = "trader_reason"
	traderFieldText      = "trader_text"
	traderFieldOpenAt    = "trader_open"
	traderFieldCloseAt   = "trader_close"
	traderFieldTimeZone  = "trader_tz"
	traderDealsRoleName  = "Deals"
	traderMaxNoteRunes   = 200
	traderMaxTitleRunes  = 100
	traderMaxAnnounceLen = 1500
)

const traderHoursHelp = "Enter the opening and closing times in 24-hour time, like `18:00` and `02:00`, and a time zone like `Europe/London`, `America/New_York` or `UTC`. Leave both times empty to turn opening hours off. Nothing was changed."

// openForm answers the buttons and menus that open a form, before anything slower runs: Discord
// only accepts a form as the first answer. It returns false for an action that isn't one of them.
func (h *TraderHandler) openForm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, action string, installationID int64, args, values []string) bool {
	var form *discordgo.InteractionResponseData
	switch action {
	case "close":
		form = BuildTraderCloseForm(installationID)
	case "announce":
		form = BuildTraderAnnounceForm(installationID)
	case "hours":
		caller, ok := panelCallerOf(i)
		if !ok {
			return true
		}
		hub, err := h.backend.TraderHub(ctx, caller.DiscordGuildID, installationID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		form = BuildTraderHoursForm(hub)
	case "refund":
		if len(args) != 1 {
			return true
		}
		orderID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || orderID <= 0 {
			return true
		}
		form = BuildTraderRefundForm(installationID, orderID)
	case "dealitem":
		if len(values) != 1 {
			return true
		}
		productID, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || productID <= 0 {
			return true
		}
		form = BuildTraderOfferForm(TraderOfferDeal, installationID, productID)
	case "salecat":
		if len(values) != 1 {
			return true
		}
		categoryID, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || categoryID < 0 {
			return true
		}
		form = BuildTraderOfferForm(TraderOfferSale, installationID, categoryID)
	default:
		return false
	}
	if err := respondModalData(s, i, form); err != nil {
		slog.Warn("component=discord", "msg", "trader form failed", "action", action, "err", err.Error())
	}
	return true
}

// submitForm handles a submitted trader form (other than the drop point).
func (h *TraderHandler) submitForm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, action string, args []string) {
	if i.Type != discordgo.InteractionModalSubmit {
		return
	}
	data := i.ModalSubmitData()
	switch action {
	case "close":
		h.setOpen(ctx, s, i, caller, hub, false, traderClip(modalValue(data, traderFieldNote), traderMaxNoteRunes))
	case "announce":
		h.announce(ctx, s, i, caller, hub, modalValue(data, traderFieldTitle), modalValue(data, traderFieldText))
	case "hours":
		h.setHours(ctx, s, i, caller, hub, modalValue(data, traderFieldOpenAt), modalValue(data, traderFieldCloseAt), modalValue(data, traderFieldTimeZone))
	case "refund":
		if len(args) != 1 {
			return
		}
		orderID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || orderID <= 0 {
			return
		}
		order, err := h.backend.TraderRefund(ctx, caller, hub.InstallationID, orderID, modalValue(data, traderFieldReason))
		if err != nil {
			h.reportError(s, i, err)
			return
		}
		respondUpdate(s, i, traderOrderCardDone(i.Message, hub, order, caller.DiscordUserID, h.now()))
		h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
	case "deal", "sale":
		if len(args) != 1 {
			return
		}
		target, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || target < 0 {
			return
		}
		in := TraderOfferInput{Kind: TraderOfferDeal, Title: traderClip(modalValue(data, traderFieldTitle), traderMaxTitleRunes)}
		if action == "sale" {
			in.Kind, in.CategoryID = TraderOfferSale, target
		} else {
			in.ProductID = target
		}
		in.PercentOff, err = parseTraderWhole(modalValue(data, traderFieldPercent), "%")
		if err != nil || in.PercentOff < 1 || in.PercentOff > TraderMaxPercentOff {
			h.reportError(s, i, ErrTraderOfferBad)
			return
		}
		in.Hours, err = parseTraderWhole(modalValue(data, traderFieldHours), "h")
		if err != nil || in.Hours < 1 || in.Hours > TraderMaxOfferHours {
			h.reportError(s, i, ErrTraderOfferBad)
			return
		}
		h.startOffer(ctx, s, i, caller, hub, in)
	}
}

// staffAction handles the panel's private screens, the order cards and the deal pings button. It
// returns false for any other action.
func (h *TraderHandler) staffAction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, action string, args, values []string, send traderSend) bool {
	switch action {
	case "pings":
		h.togglePings(s, i, caller, hub)
		return true
	case "deliver":
		if len(args) != 1 {
			return true
		}
		orderID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || orderID <= 0 {
			return true
		}
		order, err := h.backend.TraderDeliver(ctx, caller, hub.InstallationID, orderID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		respondUpdate(s, i, traderOrderCardDone(i.Message, hub, order, caller.DiscordUserID, h.now()))
		h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
		return true
	case "deal", "dealcat", "sale", "offers", "endoffer", "endall":
	default:
		return false
	}
	if err := h.backend.TraderAllowed(ctx, caller.PanelCaller, hub.InstallationID); err != nil {
		h.reportError(s, i, err)
		return true
	}
	switch action {
	case "deal", "sale":
		cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		if action == "deal" {
			send(s, i, BuildTraderDealPick(hub, cat))
		} else {
			send(s, i, BuildTraderSalePick(hub, cat))
		}
	case "dealcat":
		categoryID := int64(0)
		if len(values) == 1 && values[0] != "all" {
			categoryID, _ = strconv.ParseInt(values[0], 10, 64)
		}
		products, err := h.backend.TraderProducts(ctx, caller.DiscordGuildID, hub.InstallationID, categoryID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		send(s, i, BuildTraderDealItems(hub, products))
	case "offers":
		h.offerList(ctx, s, i, caller, hub, "", send)
	case "endoffer":
		var notice []string
		for _, v := range values {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				continue
			}
			offer, err := h.backend.TraderEndOffer(ctx, caller.PanelCaller, hub.InstallationID, id)
			if err != nil {
				h.reportError(s, i, err)
				return true
			}
			h.endOfferPost(ctx, h.apiFor(s), hub.InstallationID, offer)
			notice = append(notice, traderOfferName(offer))
		}
		h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
		h.offerList(ctx, s, i, caller, hub, traderEndedNotice(notice), send)
	case "endall":
		ended, err := h.backend.TraderEndAllOffers(ctx, caller.PanelCaller, hub.InstallationID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		var notice []string
		for _, offer := range ended {
			h.endOfferPost(ctx, h.apiFor(s), hub.InstallationID, offer)
			notice = append(notice, traderOfferName(offer))
		}
		h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
		h.offerList(ctx, s, i, caller, hub, traderEndedNotice(notice), send)
	}
	return true
}

func traderEndedNotice(names []string) string {
	if len(names) == 0 {
		return "Nothing was running."
	}
	return "Ended: " + strings.Join(names, ", ") + "."
}

func (h *TraderHandler) offerList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, notice string, send traderSend) {
	offers, err := h.backend.TraderOffers(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	send(s, i, BuildTraderOfferList(hub, offers, notice))
}

// startOffer starts a deal or sale, announces it in #deals (pinging @Deals) and refreshes the trader
// post and panel.
func (h *TraderHandler) startOffer(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, in TraderOfferInput) {
	offer, err := h.backend.TraderStartOffer(ctx, caller.PanelCaller, hub.InstallationID, in)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	var product *TraderProduct
	if offer.Kind == TraderOfferDeal {
		if p, err := h.backend.TraderProduct(ctx, caller.DiscordGuildID, hub.InstallationID, offer.ProductID); err == nil {
			product = &p
		}
	}
	posted := ""
	if api := h.apiFor(s); api != nil && hub.DealsChannelID != "" {
		m, err := api.ChannelMessageSendComplex(hub.DealsChannelID, BuildTraderOfferPost(hub, offer, product))
		if err != nil {
			slog.Warn("component=discord", "msg", "trader deal post failed", "installation_id", hub.InstallationID, "err", err.Error())
		} else {
			posted = hub.DealsChannelID
			if err := h.backend.TraderOfferPosted(ctx, hub.InstallationID, offer.ID, m.ChannelID, m.ID); err != nil {
				slog.Warn("component=discord", "msg", "trader deal post save failed", "installation_id", hub.InstallationID, "err", err.Error())
			}
		}
		h.publish(context.WithoutCancel(ctx), api, hub)
	}
	respondUpdate(s, i, BuildTraderOfferStarted(hub, offer, posted))
}

// endOfferPost marks an offer's post in #deals as over.
func (h *TraderHandler) endOfferPost(ctx context.Context, api TraderGuildAPI, installationID int64, offer TraderOffer) {
	if api == nil || offer.PostMessageID == "" || offer.PostChannelID == "" {
		return
	}
	data := BuildTraderOfferEnded(offer)
	edit := discordgo.NewMessageEdit(offer.PostChannelID, offer.PostMessageID)
	content := ""
	edit.Content = &content
	edit.Embeds = &data.Embeds
	edit.Components = &data.Components
	edit.AllowedMentions = traderNoPings
	if _, err := api.ChannelMessageEditComplex(edit); err != nil && !isUnknownMessage(err) {
		slog.Warn("component=discord", "msg", "trader deal post end failed", "installation_id", installationID, "err", err.Error())
		return
	}
	if err := h.backend.TraderOfferPostEnded(ctx, installationID, offer.ID); err != nil {
		slog.Warn("component=discord", "msg", "trader deal post end save failed", "installation_id", installationID, "err", err.Error())
	}
}

// announce posts staff news in #deals.
func (h *TraderHandler) announce(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, title, text string) {
	title, text = traderClip(title, traderMaxTitleRunes), traderClip(text, traderMaxAnnounceLen)
	if title == "" || text == "" {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "Write a title and a message. Nothing was posted."})
		return
	}
	if err := h.backend.TraderAnnounced(ctx, caller.PanelCaller, hub.InstallationID, title); err != nil {
		h.reportError(s, i, err)
		return
	}
	api := h.apiFor(s)
	if api == nil || hub.DealsChannelID == "" {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "The trader has no #deals channel. An admin can run `/trader setup` to make it again."})
		return
	}
	if _, err := api.ChannelMessageSendComplex(hub.DealsChannelID, BuildTraderAnnouncement(hub, title, text, caller.DiscordUserID, h.now())); err != nil {
		slog.Warn("component=discord", "msg", "trader announcement failed", "installation_id", hub.InstallationID, "err", err.Error())
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "Champion couldn't post in " + traderMention(hub.DealsChannelID) + ". Check it can send messages and embed links there."})
		return
	}
	h.refreshPanel(ctx, s, i, hub)
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "📣 Posted in " + traderMention(hub.DealsChannelID) + "."})
}

func (h *TraderHandler) setHours(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, open, close, tz string) {
	updated, changed, err := h.backend.TraderSetHours(ctx, caller.PanelCaller, hub.InstallationID, open, close, tz)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	cat, err := h.backend.TraderCatalog(ctx, caller.DiscordGuildID, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderPanel(updated, cat, h.now()))
	if api := h.apiFor(s); api != nil {
		h.publish(context.WithoutCancel(ctx), api, updated)
		if changed {
			h.syncVoice(context.WithoutCancel(ctx), api, updated)
		}
	}
}

// togglePings gives or takes the @Deals role.
func (h *TraderHandler) togglePings(s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub) {
	if hub.DealsRoleID == "" {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "Deal pings aren't set up on this server yet. An admin can run `/trader setup`."})
		return
	}
	api := h.apiFor(s)
	if api == nil {
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: ReplyIsUnavailable("Deal pings")})
		return
	}
	has := false
	for _, r := range caller.RoleIDs {
		has = has || r == hub.DealsRoleID
	}
	var err error
	text := "🔔 You'll be pinged for new deals and sales in " + traderMention(hub.DealsChannelID) + ". Press **Deal pings** again to stop."
	if has {
		err = api.GuildMemberRoleRemove(caller.DiscordGuildID, caller.DiscordUserID, hub.DealsRoleID)
		text = "🔕 No more deal pings. Press **Deal pings** again to turn them back on."
	} else {
		err = api.GuildMemberRoleAdd(caller.DiscordGuildID, caller.DiscordUserID, hub.DealsRoleID)
	}
	if err != nil {
		slog.Warn("component=discord", "msg", "trader deal pings role change failed", "installation_id", hub.InstallationID, "err", err.Error())
		text = "Champion couldn't change your roles. Staff: Champion needs **Manage Roles**, and its role must be above <@&" + hub.DealsRoleID + ">."
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: text, AllowedMentions: traderNoPings})
}

// refreshOffersAndHours applies opening hours and closes the posts of offers that are over; part of
// the refresher.
func (h *TraderHandler) refreshOffersAndHours(ctx context.Context, api TraderGuildAPI, hub TraderHub) TraderHub {
	if hub.HoursOpen != "" {
		updated, changed, err := h.backend.TraderApplyHours(ctx, hub.InstallationID)
		switch {
		case err != nil:
			slog.Warn("component=discord", "msg", "trader opening hours failed", "installation_id", hub.InstallationID, "err", err.Error())
		case changed:
			slog.Info("component=discord", "event", "trader_hours_applied", "installation_id", hub.InstallationID, "open", updated.Open)
			updated.ServerName = hub.ServerName
			hub = updated
		}
	}
	over, err := h.backend.TraderOfferPostsToEnd(ctx, hub.InstallationID)
	if err != nil {
		slog.Warn("component=discord", "msg", "trader deal posts list failed", "installation_id", hub.InstallationID, "err", err.Error())
		return hub
	}
	for _, offer := range over {
		h.endOfferPost(ctx, api, hub.InstallationID, offer)
	}
	return hub
}

// parseTraderWhole reads a whole number a person typed, allowing a trailing unit ("25%", "48h").
func parseTraderWhole(s, unit string) (int, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), unit))
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New("not a whole number")
	}
	return v, nil
}

// traderClip trims s and keeps at most n characters.
func traderClip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// ParseTraderHours checks opening hours as staff typed them and returns them normalised ("HH:MM"
// and an IANA time zone, "UTC" when none is given). Both times empty means no opening hours.
func ParseTraderHours(open, close, tz string) (string, string, string, error) {
	open, close, tz = strings.TrimSpace(open), strings.TrimSpace(close), strings.TrimSpace(tz)
	if open == "" && close == "" {
		return "", "", "", nil
	}
	oh, om, okOpen := parseTraderClock(open)
	ch, cm, okClose := parseTraderClock(close)
	if !okOpen || !okClose || oh == ch && om == cm {
		return "", "", "", ErrTraderHoursBad
	}
	if tz == "" || strings.EqualFold(tz, "utc") || strings.EqualFold(tz, "gmt") {
		tz = "UTC"
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return "", "", "", ErrTraderHoursBad
	}
	return fmt.Sprintf("%02d:%02d", oh, om), fmt.Sprintf("%02d:%02d", ch, cm), tz, nil
}

func parseTraderClock(s string) (int, int, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ".", ":")
	hh, mm, found := strings.Cut(s, ":")
	if !found {
		mm = "00"
	}
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(mm) != 2 {
		return 0, 0, false
	}
	return h, m, true
}

// TraderHoursState is what opening hours say at now: whether the trader should be open, and the
// opening or closing time that decided it (the latest one at or before now). ok is false when the
// hours can't be read.
func TraderHoursState(open, close, tz string, now time.Time) (shouldOpen bool, at time.Time, ok bool) {
	oh, om, okOpen := parseTraderClock(open)
	ch, cm, okClose := parseTraderClock(close)
	loc, err := time.LoadLocation(tz)
	if !okOpen || !okClose || err != nil || oh == ch && om == cm {
		return false, time.Time{}, false
	}
	local := now.In(loc)
	for day := 0; day >= -1; day-- {
		openAt := time.Date(local.Year(), local.Month(), local.Day()+day, oh, om, 0, 0, loc)
		closeAt := time.Date(local.Year(), local.Month(), local.Day()+day, ch, cm, 0, 0, loc)
		for _, c := range []struct {
			at   time.Time
			open bool
		}{{openAt, true}, {closeAt, false}} {
			if !c.at.After(now) && c.at.After(at) {
				at, shouldOpen = c.at, c.open
			}
		}
		if !at.IsZero() && day == 0 {
			// Today's latest time already passed; yesterday's are earlier.
			break
		}
	}
	return shouldOpen, at, !at.IsZero()
}

// traderHoursText is the opening hours as a line ("" when none).
func traderHoursText(hub TraderHub) string {
	if hub.HoursOpen == "" || hub.HoursClose == "" {
		return ""
	}
	return hub.HoursOpen + "–" + hub.HoursClose + " (" + hub.HoursTZ + ")"
}

// traderOfferName is what an offer is on: the item, the category, or every item.
func traderOfferName(o TraderOffer) string {
	if o.Kind == TraderOfferSale && o.CategoryID == 0 {
		return "everything"
	}
	if strings.TrimSpace(o.Name) == "" {
		return "an item"
	}
	return traderName(o.Name)
}
