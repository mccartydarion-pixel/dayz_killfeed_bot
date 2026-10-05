package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

// Discord side of the Shop buyer confirmation (docs/SHOP_ORDER_CONFIRMATION.md "Discord").
//
//   - When an order is delivered the buyer gets a DM with two buttons, "Received order" and "Issue
//     with order". A buyer without a verified Discord account here, or with DMs closed, simply
//     answers on the website: nothing is retried forever and nothing is posted in public.
//   - "Issue with order" opens a form; submitting it opens the ticket.
//   - Every ticket (from Discord or from the website) gets a private channel visible to the buyer,
//     the Staff role and the Owner role. The bot creates those roles and the Tickets category the
//     first time they are needed, and gives Owner to the Discord server owner.
//
// A button id only names a purchase. Whether the presser may answer for it is decided by the shop
// service from their verified link, exactly as on the website.

const (
	shopNoticeInterval        = 30 * time.Second
	shopTicketChannelInterval = time.Minute
	shopDeskTimeout           = 20 * time.Second
)

// shopDeskStore is the persistence the desk needs (*repository.ShopConfirmationRepository).
type shopDeskStore interface {
	ClaimPendingNotices(ctx context.Context, now time.Time, limit int) ([]repository.ShopOrderNotice, error)
	MarkNotice(ctx context.Context, purchaseID int64, state, dmChannelID, dmMessageID string) error
	ReleaseNotice(ctx context.Context, purchaseID int64) error
	ClaimTicketsNeedingChannel(ctx context.Context, now time.Time, ticketID int64, limit int) ([]repository.ShopTicketChannelJob, error)
	ReleaseTicketChannel(ctx context.Context, ticketID int64) error
	TicketDiscordSetup(ctx context.Context, guildRowID int64) (repository.ShopTicketDiscordSetup, error)
	SaveTicketDiscordSetup(ctx context.Context, guildRowID int64, s repository.ShopTicketDiscordSetup) error
	SetTicketChannel(ctx context.Context, org, inst, id int64, channelID string) (repository.ShopOrderTicket, error)
}

// shopDeskDiscord is the slice of Discord the desk needs (discord.ShopTicketSession, or a fake).
type shopDeskDiscord interface {
	discord.ShopTicketGuildAPI
	UserChannelCreate(recipientID string) (*discordgo.Channel, error)
}

// shopOrderDesk delivers the order DMs and creates the ticket channels.
type shopOrderDesk struct {
	store   shopDeskStore
	api     shopDeskDiscord
	siteURL string
	now     func() time.Time
	kick    chan int64
	// setupMu serializes role/category creation so two tickets never create them twice.
	setupMu sync.Mutex
}

func newShopOrderDesk(store shopDeskStore, api shopDeskDiscord, siteURL string) *shopOrderDesk {
	return &shopOrderDesk{store: store, api: api, siteURL: siteURL, now: time.Now, kick: make(chan int64, 32)}
}

// ticketOpened asks for a ticket's channel right away (the sweep picks it up otherwise).
func (d *shopOrderDesk) ticketOpened(ticketID int64) {
	if d == nil {
		return
	}
	select {
	case d.kick <- ticketID:
	default:
	}
}

// run delivers notices and creates ticket channels until ctx ends.
func (d *shopOrderDesk) run(ctx context.Context) {
	notices := time.NewTicker(shopNoticeInterval)
	channels := time.NewTicker(shopTicketChannelInterval)
	defer notices.Stop()
	defer channels.Stop()
	d.sendNotices(ctx)
	d.openTicketChannels(ctx, 0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-notices.C:
			d.sendNotices(ctx)
		case <-channels.C:
			d.openTicketChannels(ctx, 0)
		case id := <-d.kick:
			d.openTicketChannels(ctx, id)
		}
	}
}

// sendNotices DMs the buyers of newly delivered orders.
func (d *shopOrderDesk) sendNotices(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, shopDeskTimeout)
	defer cancel()
	notices, err := d.store.ClaimPendingNotices(ctx, d.now(), 20)
	if err != nil {
		if parent.Err() == nil {
			slog.Warn("component=shop_order_discord", "event", "notice_claim_failed", "err", err.Error())
		}
		return
	}
	for _, n := range notices {
		state, channelID, messageID := d.sendNotice(n)
		var merr error
		if state == "" {
			merr = d.store.ReleaseNotice(ctx, n.PurchaseID)
		} else {
			merr = d.store.MarkNotice(ctx, n.PurchaseID, state, channelID, messageID)
		}
		if merr != nil {
			slog.Warn("component=shop_order_discord", "event", "notice_mark_failed", "purchase_id", n.PurchaseID, "err", merr.Error())
		}
	}
}

// sendNotice sends one DM. state is SENT, UNAVAILABLE, or "" for "try again later".
func (d *shopOrderDesk) sendNotice(n repository.ShopOrderNotice) (state, channelID, messageID string) {
	if n.BuyerDiscordID == "" {
		slog.Info("component=shop_order_discord", "event", "notice_unavailable", "purchase_id", n.PurchaseID, "reason", "no_verified_discord_account")
		return repository.NoticeUnavailable, "", ""
	}
	failed := func(step string, err error) (string, string, string) {
		if discord.IsPermanentDMFailure(err) {
			// Closed DMs are the buyer's choice, not a fault: they answer on the website.
			slog.Info("component=shop_order_discord", "event", "notice_unavailable", "purchase_id", n.PurchaseID, "reason", step)
			return repository.NoticeUnavailable, "", ""
		}
		slog.Warn("component=shop_order_discord", "event", "notice_retry", "purchase_id", n.PurchaseID, "step", step, "attempt", n.Attempts, "err", err.Error())
		return "", "", ""
	}
	ch, err := d.api.UserChannelCreate(n.BuyerDiscordID)
	if err != nil {
		return failed("open_dm", err)
	}
	embed, components := discord.BuildShopOrderDeliveredMessage(n, d.siteURL)
	msg, err := d.api.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed}, Components: components,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		return failed("send_dm", err)
	}
	slog.Info("component=shop_order_discord", "event", "notice_sent", "purchase_id", n.PurchaseID)
	return repository.NoticeSent, ch.ID, msg.ID
}

// openTicketChannels creates the private channel of OPEN tickets that have none yet. ticketID > 0
// limits it to that ticket.
func (d *shopOrderDesk) openTicketChannels(parent context.Context, ticketID int64) {
	d.setupMu.Lock()
	defer d.setupMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 2*shopDeskTimeout)
	defer cancel()
	jobs, err := d.store.ClaimTicketsNeedingChannel(ctx, d.now(), ticketID, 10)
	if err != nil {
		if parent.Err() == nil {
			slog.Warn("component=shop_order_discord", "event", "ticket_claim_failed", "err", err.Error())
		}
		return
	}
	for _, job := range jobs {
		if err := d.openTicketChannel(ctx, job); err != nil {
			slog.Warn("component=shop_order_discord", "event", "ticket_channel_failed", "ticket_id", job.Ticket.ID, "attempt", job.Attempts,
				"err", err.Error(), "action", "the bot needs Manage Roles and Manage Channels on this Discord server")
			if rerr := d.store.ReleaseTicketChannel(ctx, job.Ticket.ID); rerr != nil {
				slog.Warn("component=shop_order_discord", "event", "ticket_release_failed", "ticket_id", job.Ticket.ID, "err", rerr.Error())
			}
		}
	}
}

func (d *shopOrderDesk) openTicketChannel(ctx context.Context, job repository.ShopTicketChannelJob) error {
	stored, err := d.store.TicketDiscordSetup(ctx, job.GuildRowID)
	if err != nil {
		return fmt.Errorf("load ticket setup: %w", err)
	}
	setup, warning, err := discord.EnsureShopTicketSetup(d.api, job.DiscordGuildID, stored)
	// Whatever was created is remembered even when a later step failed, so it is never created twice.
	if setup != stored {
		if serr := d.store.SaveTicketDiscordSetup(ctx, job.GuildRowID, setup); serr != nil {
			return fmt.Errorf("save ticket setup: %w", serr)
		}
	}
	if err != nil {
		return err
	}
	if warning != nil {
		slog.Warn("component=shop_order_discord", "event", "owner_role_not_assigned", "ticket_id", job.Ticket.ID, "err", warning.Error(),
			"action", "move the bot's role above the Owner role, or assign it by hand")
	}
	channelID, posted, err := discord.CreateShopTicketChannel(d.api, job, setup, d.siteURL)
	if channelID == "" {
		return err
	}
	if !posted {
		slog.Warn("component=shop_order_discord", "event", "ticket_opening_message_failed", "ticket_id", job.Ticket.ID, "err", err.Error())
	}
	t := job.Ticket
	if _, err := d.store.SetTicketChannel(ctx, t.OrganizationID, t.InstallationID, t.ID, channelID); err != nil {
		return fmt.Errorf("record ticket channel: %w", err)
	}
	slog.Info("component=shop_order_discord", "event", "ticket_channel_created", "ticket_id", t.ID, "purchase_id", t.PurchaseID)
	return nil
}

// ticketResolved posts the resolution in the ticket's channel (when it has one).
func (d *shopOrderDesk) ticketResolved(t repository.ShopOrderTicket) {
	if d == nil || t.DiscordChannelID == nil || *t.DiscordChannelID == "" {
		return
	}
	if _, err := d.api.ChannelMessageSendComplex(*t.DiscordChannelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{discord.BuildShopTicketResolved(t)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}); err != nil {
		slog.Warn("component=shop_order_discord", "event", "ticket_resolved_notice_failed", "ticket_id", t.ID, "err", err.Error())
	}
}

// --- App wiring ---------------------------------------------------------------------------------

// shopTicketOpened and shopTicketResolved are called by the API and the Discord handler; they do
// nothing until the Discord desk is running.
func (a *App) shopTicketOpened(ticketID int64) { a.shopOrderDesk.Load().ticketOpened(ticketID) }

func (a *App) shopTicketResolved(t repository.ShopOrderTicket) {
	if desk := a.shopOrderDesk.Load(); desk != nil {
		go desk.ticketResolved(t)
	}
}

// startShopOrderDesk starts the DM and ticket-channel worker on a connected session.
func (a *App) startShopOrderDesk(ctx context.Context, session *discordgo.Session) {
	if a.ShopConfirmations == nil || a.shopConfirmationRepo == nil || session == nil {
		return
	}
	desk := newShopOrderDesk(a.shopConfirmationRepo, discord.ShopTicketSession{S: session}, a.siteURL())
	a.shopOrderDesk.Store(desk)
	go desk.run(ctx)
}

const (
	shopOrderTextNotYours    = "This order isn't linked to your Discord account, or it no longer exists."
	shopOrderTextPaused      = "The Shop is paused on this server right now. Ask the server staff, or try again later."
	shopOrderTextInvalid     = "This button is no longer valid."
	shopOrderTextTryAgain    = "Something went wrong on our side. Try again in a moment, or answer on the website."
	shopOrderTextNeedsReason = "Please describe what went wrong so the staff can help."
)

// modalLookupBudget bounds the database work a button may do before opening a
// form. Discord allows 3 s for the first answer and a form cannot be deferred.
const modalLookupBudget = 1500 * time.Millisecond

// HandleShopOrderInteraction handles the two order buttons and the issue form, in a DM or a server.
func (a *App) HandleShopOrderInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i == nil || a.ShopConfirmations == nil || a.shopConfirmationRepo == nil {
		return
	}
	user := i.User
	if i.Member != nil && i.Member.User != nil {
		user = i.Member.User
	}
	if user == nil {
		return
	}
	var customID string
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		customID = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	default:
		return
	}
	reply := func(text string) { discord.RespondEphemeral(s, i, text) }
	// show replaces the message the button sits on with the order's current state.
	show := func(c repository.ShopOrderConfirmation, ticketID int64) {
		embed, components := discord.BuildShopOrderStateMessage(c, ticketID, a.siteURL())
		if i.Message == nil {
			reply(embed.Description)
			return
		}
		discord.RespondUpdate(s, i, &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, Components: components})
	}
	action, purchaseID, ok := discord.ParseShopOrderCustomID(customID)
	if !ok {
		reply(shopOrderTextInvalid)
		return
	}
	timeout := economyTimeout
	if action == discord.ShopOrderActionIssue && i.Type == discordgo.InteractionMessageComponent {
		// This press opens a form, which Discord only accepts as the first
		// answer and cannot defer: the order lookup gets a short budget.
		timeout = modalLookupBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	order, err := a.shopConfirmationRepo.OrderForDiscordBuyer(ctx, purchaseID, user.ID)
	if err != nil {
		if errors.Is(err, repository.ErrShopConfirmationNotFound) {
			reply(shopOrderTextNotYours)
		} else {
			slog.Warn("component=shop_order_discord", "event", "order_lookup_failed", "purchase_id", purchaseID, "err", err.Error())
			reply(shopOrderTextTryAgain)
		}
		return
	}
	c := order.Confirmation
	if entitlements.Enforced() {
		plan, perr := a.organizationPlan(ctx, c.OrganizationID)
		if perr != nil {
			reply(shopOrderTextTryAgain)
			return
		}
		if !entitlements.Has(plan, entitlements.Economy) {
			reply(shopOrderTextPaused)
			return
		}
	}
	scope := repository.EconomyScope{OrganizationID: c.OrganizationID, InstallationID: c.InstallationID, GuildID: order.GuildRowID, ServerID: order.ServerID, Status: order.Status}
	failed := func(what string, err error) {
		var invalid *shop.ValidationError
		switch {
		case errors.Is(err, repository.ErrShopConfirmationState):
			// Already answered (here, on the website, or by the deadline): show where it stands.
			if now, gerr := a.ShopConfirmations.Mine(ctx, scope, user.ID, purchaseID); gerr == nil {
				show(now, 0)
				return
			}
			reply("This order was already answered.")
		case errors.Is(err, repository.ErrShopConfirmationNotFound):
			reply(shopOrderTextNotYours)
		case errors.Is(err, shop.ErrReasonRequired):
			reply(shopOrderTextNeedsReason)
		case errors.As(err, &invalid):
			reply("That description is too long. Keep it under 1000 characters.")
		case errors.Is(err, economy.ErrSuspended):
			reply(shopOrderTextPaused)
		default:
			slog.Warn("component=shop_order_discord", "event", what+"_failed", "purchase_id", purchaseID, "err", err.Error())
			reply(shopOrderTextTryAgain)
		}
	}
	audit := func(event string, attrs ...any) {
		base := []any{"event", event, "organization_id", c.OrganizationID, "installation_id", c.InstallationID, "purchase_id", purchaseID, "via", repository.ConfirmationViaDiscord}
		slog.Info("component=shop_order_discord", append(base, attrs...)...)
	}
	switch {
	case action == discord.ShopOrderActionReceived && i.Type == discordgo.InteractionMessageComponent:
		done, err := a.ShopConfirmations.Received(ctx, scope, user.ID, purchaseID, repository.ConfirmationViaDiscord)
		if err != nil {
			failed("received", err)
			return
		}
		audit("shop_order_confirmed_received")
		show(done, 0)
	case action == discord.ShopOrderActionIssue && i.Type == discordgo.InteractionMessageComponent:
		if c.State != repository.ConfirmationAwaitingBuyer && c.State != repository.ConfirmationAutoCompleted {
			show(c, 0)
			return
		}
		_ = discord.RespondModal(s, i, discord.BuildShopOrderIssueModal(purchaseID, shop.MaxIssueReasonRunes))
	case action == discord.ShopOrderActionIssueModal && i.Type == discordgo.InteractionModalSubmit:
		done, ticket, err := a.ShopConfirmations.ReportIssue(ctx, scope, user.ID, purchaseID, repository.ConfirmationViaDiscord, discord.ShopOrderModalReason(i.ModalSubmitData()))
		if err != nil {
			failed("report_issue", err)
			return
		}
		audit("shop_order_issue_reported", "ticket_id", ticket.ID)
		show(done, ticket.ID)
		a.shopTicketOpened(ticket.ID)
	default:
		reply(shopOrderTextInvalid)
	}
}

var _ shopDeskStore = (*repository.ShopConfirmationRepository)(nil)
var _ shopDeskDiscord = discord.ShopTicketSession{}
