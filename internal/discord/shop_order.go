package discord

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Discord side of the Shop buyer confirmation (docs/SHOP_ORDER_CONFIRMATION.md "Discord"): the
// delivered-order DM with its two buttons, the issue form, and the private ticket channel with the
// Owner and Staff roles. Everything here is presentation and Discord plumbing; who may answer for an
// order is decided by the shop service, never by a button id.

// Button and form ids. The number after the last colon is the purchase id.
const (
	ShopOrderPrefix       = "champion:shoporder:"
	shopOrderReceivedID   = ShopOrderPrefix + "received:"
	shopOrderIssueID      = ShopOrderPrefix + "issue:"
	shopOrderIssueModalID = ShopOrderPrefix + "issuemodal:"
	// ShopOrderReasonInputID is the text input of the issue form.
	ShopOrderReasonInputID = "reason"

	ShopOrderActionReceived   = "received"
	ShopOrderActionIssue      = "issue"
	ShopOrderActionIssueModal = "issuemodal"
)

// Names of what the bot creates on a server for tickets.
const (
	ShopOwnerRoleName      = "Owner"
	ShopStaffRoleName      = "Staff"
	ShopTicketCategoryName = "Tickets"
)

const (
	shopOrderColorWaiting  = 0xD4A017
	shopOrderColorReceived = 0x2ECC71
	shopOrderColorIssue    = 0xE74C3C
	shopOrderColorNeutral  = 0x95A5A6
	shopOrderMaxItemLines  = 10
)

// IsShopOrderInteraction reports whether a component or modal id belongs to the order confirmation.
func IsShopOrderInteraction(customID string) bool {
	return strings.HasPrefix(customID, ShopOrderPrefix)
}

// ParseShopOrderCustomID splits an id into its action and purchase id.
func ParseShopOrderCustomID(customID string) (action string, purchaseID int64, ok bool) {
	rest, found := strings.CutPrefix(customID, ShopOrderPrefix)
	if !found {
		return "", 0, false
	}
	action, raw, found := strings.Cut(rest, ":")
	if !found {
		return "", 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	switch action {
	case ShopOrderActionReceived, ShopOrderActionIssue, ShopOrderActionIssueModal:
		return action, id, true
	}
	return "", 0, false
}

func shopOrderItemLines(items []repository.ShopNoticeItem) string {
	if len(items) == 0 {
		return "_Order contents unavailable._"
	}
	var b strings.Builder
	for i, it := range items {
		if i == shopOrderMaxItemLines {
			fmt.Fprintf(&b, "… and %d more", len(items)-shopOrderMaxItemLines)
			break
		}
		name := shopTruncate(strings.TrimSpace(it.Name), 80)
		fmt.Fprintf(&b, "• %d× %s\n", it.Quantity, name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// shopTruncate cuts text to at most max characters (Discord embed fields hold 1024).
func shopTruncate(text string, max int) string {
	r := []rune(text)
	if len(r) <= max {
		return text
	}
	return string(r[:max-1]) + "…"
}

// ShopOrderURL is the buyer's order page on the website.
func ShopOrderURL(siteURL string, purchaseID int64) string {
	return fmt.Sprintf("%s/dashboard/player/shop/purchases/%d", strings.TrimRight(siteURL, "/"), purchaseID)
}

// shopOrderButtons builds the button row. received/issue say which answers are still open.
func shopOrderButtons(purchaseID int64, received, issue bool, siteURL string) []discordgo.MessageComponent {
	id := strconv.FormatInt(purchaseID, 10)
	var row []discordgo.MessageComponent
	if received {
		row = append(row, discordgo.Button{Label: "Received order", Style: discordgo.SuccessButton, CustomID: shopOrderReceivedID + id})
	}
	if issue {
		row = append(row, discordgo.Button{Label: "Issue with order", Style: discordgo.DangerButton, CustomID: shopOrderIssueID + id})
	}
	if siteURL != "" {
		row = append(row, discordgo.Button{Label: "View order", Style: discordgo.LinkButton, URL: ShopOrderURL(siteURL, purchaseID)})
	}
	if len(row) == 0 {
		return []discordgo.MessageComponent{}
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// BuildShopOrderDeliveredMessage is the DM sent when an order is delivered: what was delivered, the
// deadline, and the two answers.
func BuildShopOrderDeliveredMessage(n repository.ShopOrderNotice, siteURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	where := ""
	if name := strings.TrimSpace(n.GuildName); name != "" {
		where = " on **" + name + "**"
	}
	embed := &discordgo.MessageEmbed{
		Title: "📦 Your order was delivered",
		Color: shopOrderColorWaiting,
		Description: fmt.Sprintf("Order **#%d**%s has been delivered in game.\n\n%s\n\nDid you get it? Press **Received order**, or **Issue with order** to open a support ticket with the server staff.",
			n.PurchaseID, where, shopOrderItemLines(n.Items)),
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Total", Value: fmt.Sprintf("%d Champion Points", n.TotalPoints), Inline: true},
			{Name: "Answer by", Value: fmt.Sprintf("<t:%d:f>", n.DeadlineAt.Unix()), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "No answer by then completes the order automatically. You can still report an issue afterwards."},
	}
	return embed, shopOrderButtons(n.PurchaseID, true, true, siteURL)
}

// BuildShopOrderStateMessage replaces the DM once the order has an answer (or lost its buttons for
// another reason). ticketID is the ticket an issue opened, 0 when unknown.
func BuildShopOrderStateMessage(c repository.ShopOrderConfirmation, ticketID int64, siteURL string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	embed := &discordgo.MessageEmbed{Title: fmt.Sprintf("Order #%d", c.PurchaseID), Color: shopOrderColorNeutral}
	received, issue := false, false
	switch c.State {
	case repository.ConfirmationAwaitingBuyer:
		embed.Color = shopOrderColorWaiting
		embed.Description = fmt.Sprintf("Delivered. Did you get it? Answer by <t:%d:f>.", c.DeadlineAt.Unix())
		received, issue = true, true
	case repository.ConfirmationReceived:
		embed.Color = shopOrderColorReceived
		embed.Description = "✅ You confirmed this order as **received**. Thanks!"
	case repository.ConfirmationIssueReported:
		embed.Color = shopOrderColorIssue
		embed.Description = "🎫 You reported an issue with this order."
		if ticketID > 0 {
			embed.Description = fmt.Sprintf("🎫 You reported an issue with this order. **Ticket #%d** is open.", ticketID)
		}
		embed.Description += " The server staff will reach you in a private ticket channel on the server."
	case repository.ConfirmationAutoCompleted:
		embed.Description = "This order was completed automatically because the answer window passed. If it never arrived you can still report an issue."
		issue = true
	case repository.ConfirmationVoid:
		embed.Description = "This order was refunded, so there is nothing to confirm."
	default:
		embed.Description = "This order can no longer be answered here."
	}
	return embed, shopOrderButtons(c.PurchaseID, received, issue, siteURL)
}

// BuildShopOrderIssueModal is the form behind "Issue with order".
func BuildShopOrderIssueModal(purchaseID int64, maxLength int) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{
		CustomID: shopOrderIssueModalID + strconv.FormatInt(purchaseID, 10),
		Title:    fmt.Sprintf("Issue with order #%d", purchaseID),
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.TextInput{CustomID: ShopOrderReasonInputID, Label: "What went wrong?", Style: discordgo.TextInputParagraph,
				Placeholder: "Example: I was at the drop location after the restart and the item was not there.", Required: true, MinLength: 1, MaxLength: maxLength},
		}}},
	}
}

// ShopOrderModalReason reads the reason out of a submitted issue form.
func ShopOrderModalReason(data discordgo.ModalSubmitInteractionData) string {
	for _, row := range data.Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			if ti, ok := c.(*discordgo.TextInput); ok && ti.CustomID == ShopOrderReasonInputID {
				return ti.Value
			}
		}
	}
	return ""
}

// IsPermanentDMFailure reports whether a DM can never be delivered as things stand: the user has
// DMs closed, blocked the bot, shares no server with it, or the request itself is refused. Rate
// limits and server errors are not permanent.
func IsPermanentDMFailure(err error) bool {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) || rest.Response == nil {
		return false
	}
	code := rest.Response.StatusCode
	return code >= 400 && code < 500 && code != http.StatusTooManyRequests && code != http.StatusRequestTimeout
}

// --- ticket roles, category and channel ---------------------------------------------------------

// ShopTicketGuildAPI is the slice of Discord the ticket setup needs (ShopTicketSession, or a fake in
// tests).
type ShopTicketGuildAPI interface {
	GuildOwnerID(guildID string) (string, error)
	GuildRoles(guildID string) ([]*discordgo.Role, error)
	GuildRoleCreate(guildID string, data *discordgo.RoleParams) (*discordgo.Role, error)
	GuildMemberRoleAdd(guildID, userID, roleID string) error
	GuildChannels(guildID string) ([]*discordgo.Channel, error)
	GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error)
	BotUserID() string
}

// ShopTicketSession adapts a live session to ShopTicketGuildAPI.
type ShopTicketSession struct{ S *discordgo.Session }

func (a ShopTicketSession) GuildOwnerID(guildID string) (string, error) {
	if a.S.State != nil {
		if g, err := a.S.State.Guild(guildID); err == nil && g != nil && g.OwnerID != "" {
			return g.OwnerID, nil
		}
	}
	g, err := a.S.Guild(guildID)
	if err != nil {
		return "", err
	}
	return g.OwnerID, nil
}

func (a ShopTicketSession) GuildRoles(guildID string) ([]*discordgo.Role, error) {
	return a.S.GuildRoles(guildID)
}

func (a ShopTicketSession) GuildRoleCreate(guildID string, data *discordgo.RoleParams) (*discordgo.Role, error) {
	return a.S.GuildRoleCreate(guildID, data)
}

func (a ShopTicketSession) GuildMemberRoleAdd(guildID, userID, roleID string) error {
	return a.S.GuildMemberRoleAdd(guildID, userID, roleID)
}

func (a ShopTicketSession) GuildChannels(guildID string) ([]*discordgo.Channel, error) {
	return a.S.GuildChannels(guildID)
}

func (a ShopTicketSession) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	return a.S.GuildChannelCreateComplex(guildID, data)
}

func (a ShopTicketSession) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	return a.S.ChannelMessageSendComplex(channelID, data)
}

func (a ShopTicketSession) UserChannelCreate(recipientID string) (*discordgo.Channel, error) {
	return a.S.UserChannelCreate(recipientID)
}

func (a ShopTicketSession) BotUserID() string {
	if a.S == nil || a.S.State == nil || a.S.State.User == nil {
		return ""
	}
	return a.S.State.User.ID
}

// shopTicketAccess is what a ticket participant may do in the ticket channel.
const shopTicketAccess = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory |
	discordgo.PermissionEmbedLinks | discordgo.PermissionAttachFiles

// ensureShopRole returns the id of the role to use: the stored one if it still exists, otherwise an
// existing role of that name (a server that already has a "Staff" role keeps using it), otherwise a
// new one. A new role carries no permissions of its own: it only names who sees tickets.
func ensureShopRole(api ShopTicketGuildAPI, guildID string, roles []*discordgo.Role, storedID, name string) (string, error) {
	for _, r := range roles {
		if r != nil && storedID != "" && r.ID == storedID {
			return r.ID, nil
		}
	}
	for _, r := range roles {
		if r != nil && r.ID != guildID && !r.Managed && strings.EqualFold(strings.TrimSpace(r.Name), name) {
			return r.ID, nil
		}
	}
	none, yes := int64(0), true
	created, err := api.GuildRoleCreate(guildID, &discordgo.RoleParams{Name: name, Permissions: &none, Mentionable: &yes})
	if err != nil {
		return "", fmt.Errorf("create %s role: %w", name, err)
	}
	return created.ID, nil
}

// EnsureShopTicketSetup makes sure the server has the Owner role, the Staff role and the Tickets
// category, creating what is missing, and gives the Owner role to the Discord server owner.
// It returns the setup to store and a non-fatal warning (the owner could not be given the role,
// usually because the role sits above the bot's own role).
func EnsureShopTicketSetup(api ShopTicketGuildAPI, guildID string, stored repository.ShopTicketDiscordSetup) (setup repository.ShopTicketDiscordSetup, warning error, err error) {
	setup = stored
	roles, err := api.GuildRoles(guildID)
	if err != nil {
		return setup, nil, fmt.Errorf("list roles: %w", err)
	}
	if setup.OwnerRoleID, err = ensureShopRole(api, guildID, roles, stored.OwnerRoleID, ShopOwnerRoleName); err != nil {
		return setup, nil, err
	}
	if setup.StaffRoleID, err = ensureShopRole(api, guildID, roles, stored.StaffRoleID, ShopStaffRoleName); err != nil {
		return setup, nil, err
	}
	if ownerID, oerr := api.GuildOwnerID(guildID); oerr != nil {
		warning = fmt.Errorf("find the server owner: %w", oerr)
	} else if ownerID != "" {
		if aerr := api.GuildMemberRoleAdd(guildID, ownerID, setup.OwnerRoleID); aerr != nil {
			warning = fmt.Errorf("give the Owner role to the server owner: %w", aerr)
		}
	}

	channels, err := api.GuildChannels(guildID)
	if err != nil {
		return setup, warning, fmt.Errorf("list channels: %w", err)
	}
	setup.CategoryID = ""
	for _, ch := range channels {
		if ch != nil && ch.Type == discordgo.ChannelTypeGuildCategory && stored.CategoryID != "" && ch.ID == stored.CategoryID {
			setup.CategoryID = ch.ID
		}
	}
	if setup.CategoryID == "" {
		for _, ch := range channels {
			if ch != nil && ch.Type == discordgo.ChannelTypeGuildCategory && strings.EqualFold(strings.TrimSpace(ch.Name), ShopTicketCategoryName) {
				setup.CategoryID = ch.ID
				break
			}
		}
	}
	if setup.CategoryID == "" {
		cat, cerr := api.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
			Name: ShopTicketCategoryName, Type: discordgo.ChannelTypeGuildCategory,
			PermissionOverwrites: shopTicketOverwrites(guildID, api.BotUserID(), setup, ""),
		})
		if cerr != nil {
			return setup, warning, fmt.Errorf("create the Tickets category: %w", cerr)
		}
		setup.CategoryID = cat.ID
	}
	return setup, warning, nil
}

// shopTicketOverwrites hides a channel from everyone except the bot, the Owner and Staff roles and
// (when given) the buyer. Each ticket channel carries these itself, so its privacy never depends on
// the category it sits in.
func shopTicketOverwrites(guildID, botID string, setup repository.ShopTicketDiscordSetup, buyerDiscordID string) []*discordgo.PermissionOverwrite {
	out := []*discordgo.PermissionOverwrite{{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel}}
	if botID != "" {
		out = append(out, &discordgo.PermissionOverwrite{ID: botID, Type: discordgo.PermissionOverwriteTypeMember, Allow: shopTicketAccess})
	}
	for _, roleID := range []string{setup.OwnerRoleID, setup.StaffRoleID} {
		if roleID != "" {
			out = append(out, &discordgo.PermissionOverwrite{ID: roleID, Type: discordgo.PermissionOverwriteTypeRole, Allow: shopTicketAccess})
		}
	}
	if isSnowflake(buyerDiscordID) && buyerDiscordID != botID {
		out = append(out, &discordgo.PermissionOverwrite{ID: buyerDiscordID, Type: discordgo.PermissionOverwriteTypeMember, Allow: shopTicketAccess})
	}
	return out
}

// isSnowflake reports whether id looks like a Discord id. A ticket opened by the delivery worker for
// a buyer with no verified Discord account carries a placeholder instead, which must never be sent
// to Discord as a user.
func isSnowflake(id string) bool {
	if len(id) < 5 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ShopTicketChannelName is the ticket channel's name.
func ShopTicketChannelName(t repository.ShopOrderTicket) string {
	return fmt.Sprintf("ticket-%d-order-%d", t.ID, t.PurchaseID)
}

// CreateShopTicketChannel creates the private channel of a ticket and posts its opening message.
// posted is false when the channel exists but the opening message could not be sent; the channel id
// is still valid and must be recorded so no second channel is created.
func CreateShopTicketChannel(api ShopTicketGuildAPI, job repository.ShopTicketChannelJob, setup repository.ShopTicketDiscordSetup, siteURL string) (channelID string, posted bool, err error) {
	t := job.Ticket
	ch, err := api.GuildChannelCreateComplex(job.DiscordGuildID, discordgo.GuildChannelCreateData{
		Name:                 ShopTicketChannelName(t),
		Type:                 discordgo.ChannelTypeGuildText,
		Topic:                fmt.Sprintf("Shop order #%d · ticket #%d", t.PurchaseID, t.ID),
		ParentID:             setup.CategoryID,
		PermissionOverwrites: shopTicketOverwrites(job.DiscordGuildID, api.BotUserID(), setup, t.OpenedByDiscordID),
	})
	if err != nil {
		return "", false, fmt.Errorf("create ticket channel: %w", err)
	}
	embed, content, mentions := BuildShopTicketOpening(job, setup, siteURL)
	if _, err := api.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: content, Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: mentions}); err != nil {
		return ch.ID, false, fmt.Errorf("post ticket opening message: %w", err)
	}
	return ch.ID, true, nil
}

// BuildShopTicketOpening is the first message of a ticket channel. It pings exactly the buyer and
// the two roles; the buyer's own text cannot ping anyone.
func BuildShopTicketOpening(job repository.ShopTicketChannelJob, setup repository.ShopTicketDiscordSetup, siteURL string) (*discordgo.MessageEmbed, string, *discordgo.MessageAllowedMentions) {
	t := job.Ticket
	mentions := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	var who []string
	if isSnowflake(t.OpenedByDiscordID) {
		who = append(who, "<@"+t.OpenedByDiscordID+">")
		mentions.Users = []string{t.OpenedByDiscordID}
	}
	for _, roleID := range []string{setup.StaffRoleID, setup.OwnerRoleID} {
		if roleID != "" {
			who = append(who, "<@&"+roleID+">")
			mentions.Roles = append(mentions.Roles, roleID)
		}
	}
	reason := shopTruncate(t.Reason, 1000)
	via, description := "the website", "The buyer reported an issue with a delivered Shop order. Sort it out together here; staff close the ticket on the website when it is settled."
	switch t.OpenedVia {
	case repository.ConfirmationViaDiscord:
		via = "Discord"
	case repository.TicketViaSystem:
		via = "automatic delivery"
		description = "Automatic delivery could not confirm this order and stopped. Staff: check in game whether the item is there, then record the result on the website. The buyer is in this channel."
	}
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🎫 Ticket #%d · order #%d", t.ID, t.PurchaseID),
		Color:       shopOrderColorIssue,
		Description: description,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "What went wrong", Value: reason},
			{Name: "Order", Value: shopOrderItemLines(job.Items)},
			{Name: "Reported from", Value: via, Inline: true},
			{Name: "Opened", Value: fmt.Sprintf("<t:%d:f>", t.OpenedAt.Unix()), Inline: true},
		},
	}
	if siteURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Staff", Value: fmt.Sprintf("[Open the order](%s/dashboard/shop/admin/orders/%d)", strings.TrimRight(siteURL, "/"), t.PurchaseID)})
	}
	return embed, strings.Join(who, " "), mentions
}

// BuildShopTicketResolved is the notice posted in the ticket channel when staff resolve the ticket.
func BuildShopTicketResolved(t repository.ShopOrderTicket) *discordgo.MessageEmbed {
	outcome := "Resolved"
	if t.Resolution != nil {
		switch *t.Resolution {
		case repository.TicketResolutionCompleted:
			outcome = "Resolved: the order is complete"
		case repository.TicketResolutionRefunded:
			outcome = "Resolved: the order is being refunded"
		case repository.TicketResolutionOther:
			outcome = "Resolved"
		}
	}
	embed := &discordgo.MessageEmbed{Title: fmt.Sprintf("✅ Ticket #%d closed", t.ID), Color: shopOrderColorReceived, Description: outcome + "."}
	if t.ResolutionNote != nil && strings.TrimSpace(*t.ResolutionNote) != "" {
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "Note from staff", Value: shopTruncate(*t.ResolutionNote, 1000)}}
	}
	return embed
}
