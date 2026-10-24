package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// Trader phase 2 messages and forms: deals, sales, announcements, opening hours, the closing note
// and the handled order card.

func traderModalID(action string, installationID int64, args ...string) string {
	return traderModalPrefix + action + ":" + strconv.FormatInt(installationID, 10) + traderArgs(args)
}

func traderTextRow(input discordgo.TextInput) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{input}}
}

// traderPriceText is an item's price, struck through and marked down while a deal or sale runs.
func traderPriceText(p TraderProduct) string {
	if p.ListPrice > p.Price && p.PercentOff > 0 {
		return fmt.Sprintf("~~%s~~ **%s** (−%d%%)", presentation.FormatThousands(p.ListPrice), presentation.FormatPoints(p.Price), p.PercentOff)
	}
	return presentation.FormatPoints(p.Price)
}

// traderPricePlain is traderPriceText without markdown, for menus and buttons.
func traderPricePlain(p TraderProduct) string {
	if p.ListPrice > p.Price && p.PercentOff > 0 {
		return fmt.Sprintf("%s (%d%% off)", presentation.FormatPoints(p.Price), p.PercentOff)
	}
	return presentation.FormatPoints(p.Price)
}

// traderOfferLine is one running deal or sale.
func traderOfferLine(o TraderOffer) string {
	what := "🏷️ **" + strconv.Itoa(o.PercentOff) + "% off** " + traderOfferName(o)
	if o.Kind == TraderOfferSale {
		what = "🔥 **" + strconv.Itoa(o.PercentOff) + "% off** " + traderOfferName(o)
	}
	return what + " · ends " + presentation.Timestamp(o.EndsAt, 'R')
}

// BuildTraderCloseForm asks for an optional note to players before closing.
func BuildTraderCloseForm(installationID int64) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{CustomID: traderModalID("close", installationID), Title: "Close the trader",
		Components: []discordgo.MessageComponent{traderTextRow(discordgo.TextInput{CustomID: traderFieldNote, Label: "Message to players (optional)",
			Style: discordgo.TextInputParagraph, Placeholder: "e.g. Restocking, back at 8pm", MaxLength: traderMaxNoteRunes})}}
}

// BuildTraderAnnounceForm asks for an announcement for #deals.
func BuildTraderAnnounceForm(installationID int64) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{CustomID: traderModalID("announce", installationID), Title: "Announcement in #deals",
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldTitle, Label: "Title", Style: discordgo.TextInputShort, Required: true, MaxLength: traderMaxTitleRunes,
				Placeholder: "e.g. New stock in"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldText, Label: "Message", Style: discordgo.TextInputParagraph, Required: true, MaxLength: traderMaxAnnounceLen}),
		}}
}

// BuildTraderHoursForm sets the opening hours, filled in with the current ones.
func BuildTraderHoursForm(hub TraderHub) *discordgo.InteractionResponseData {
	tz := hub.HoursTZ
	if tz == "" {
		tz = "UTC"
	}
	return &discordgo.InteractionResponseData{CustomID: traderModalID("hours", hub.InstallationID), Title: "Opening hours (every day)",
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldOpenAt, Label: "Opens at (24h, empty = no hours)", Style: discordgo.TextInputShort,
				Placeholder: "18:00", Value: hub.HoursOpen, MaxLength: 5}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldCloseAt, Label: "Closes at (24h, empty = no hours)", Style: discordgo.TextInputShort,
				Placeholder: "02:00", Value: hub.HoursClose, MaxLength: 5}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldTimeZone, Label: "Time zone", Style: discordgo.TextInputShort,
				Placeholder: "Europe/London, America/New_York or UTC", Value: tz, MaxLength: 64}),
		}}
}

// BuildTraderRefundForm asks why an order is refunded.
func BuildTraderRefundForm(installationID, orderID int64) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{CustomID: traderModalID("refund", installationID, strconv.FormatInt(orderID, 10)), Title: fmt.Sprintf("Refund order #%d", orderID),
		Components: []discordgo.MessageComponent{traderTextRow(discordgo.TextInput{CustomID: traderFieldReason, Label: "Reason (only staff see it)",
			Style: discordgo.TextInputShort, Required: true, MaxLength: 200, Placeholder: "e.g. Couldn't deliver, out of stock in game"})}}
}

// BuildTraderOfferForm asks how much off and for how long. target is the item of a deal, or the
// category of a sale (0: every item).
func BuildTraderOfferForm(kind string, installationID, target int64) *discordgo.InteractionResponseData {
	action, title := "deal", "Post a deal"
	if kind == TraderOfferSale {
		action, title = "sale", "Start a sale"
	}
	return &discordgo.InteractionResponseData{CustomID: traderModalID(action, installationID, strconv.FormatInt(target, 10)), Title: title,
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldPercent, Label: fmt.Sprintf("Percent off (1 to %d)", TraderMaxPercentOff), Style: discordgo.TextInputShort,
				Required: true, MaxLength: 3, Placeholder: "25"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldHours, Label: fmt.Sprintf("How many hours (1 to %d)", TraderMaxOfferHours), Style: discordgo.TextInputShort,
				Required: true, MaxLength: 4, Value: "24"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldTitle, Label: "Message for players (optional)", Style: discordgo.TextInputShort,
				MaxLength: traderMaxTitleRunes, Placeholder: "e.g. Weekend raid sale"}),
		}}
}

func traderStaffEmbed(hub TraderHub, title string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: title, Color: color,
		Footer: presentation.Footer(hub.ServerName, "Only you see this")}
}

func traderCategoryOptions(cat TraderCatalog, first discordgo.SelectMenuOption) []discordgo.SelectMenuOption {
	options := []discordgo.SelectMenuOption{first}
	for _, c := range cat.Categories {
		if len(options) == 25 {
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(c.Name, 100), 100), Value: strconv.FormatInt(c.ID, 10),
			Description: presentation.Plural(int64(c.Count), "item", "items")})
	}
	return options
}

// BuildTraderDealPick starts a deal: pick the category of the item.
func BuildTraderDealPick(hub TraderHub, cat TraderCatalog) *discordgo.InteractionResponseData {
	embed := traderStaffEmbed(hub, "🏷️ Post a deal", presentation.Gold)
	embed.Description = fmt.Sprintf("A deal takes a percentage off one item (up to %d%%) for as long as you choose, on the website shop too. "+
		"It's announced in %s and pings everyone who turned on deal pings.\n\nPick the item's category.", TraderMaxPercentOff, traderMention(hub.DealsChannelID))
	options := traderCategoryOptions(cat, discordgo.SelectMenuOption{Label: "All items", Value: "all", Emoji: &discordgo.ComponentEmoji{Name: "📋"}})
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderID("dealcat", hub.InstallationID), Placeholder: "Choose a category", Options: options}}}
	return traderPrivate(embed, menu)
}

// BuildTraderDealItems picks the deal's item.
func BuildTraderDealItems(hub TraderHub, products []TraderProduct) *discordgo.InteractionResponseData {
	embed := traderStaffEmbed(hub, "🏷️ Post a deal", presentation.Gold)
	back := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Categories", Emoji: &discordgo.ComponentEmoji{Name: "⬅️"}, Style: discordgo.SecondaryButton, CustomID: traderID("deal", hub.InstallationID)}}}
	if len(products) == 0 {
		embed.Description = "Nothing for sale in that category."
		return traderPrivate(embed, back)
	}
	options := make([]discordgo.SelectMenuOption, 0, len(products))
	for _, p := range products {
		if len(options) == 25 {
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(p.Name, 100), 100), Value: strconv.FormatInt(p.ID, 10),
			Description: panelClip(traderPricePlain(p)+" · "+traderStockText(p.Stock), 100)})
	}
	embed.Description = "Pick the item. Next you choose how much off and for how long."
	if len(products) >= 25 {
		embed.Description += "\n\nOnly the first 25 items of this category are listed; pick a smaller category to find others."
	}
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderFormID("dealitem", hub.InstallationID), Placeholder: "Choose the item", Options: options}}}
	return traderPrivate(embed, menu, back)
}

// BuildTraderSalePick starts a sale: everything, or one category.
func BuildTraderSalePick(hub TraderHub, cat TraderCatalog) *discordgo.InteractionResponseData {
	embed := traderStaffEmbed(hub, "🔥 Start a sale", presentation.Gold)
	embed.Description = fmt.Sprintf("A sale takes a percentage off every item, or one category, up to %d%%, on the website shop too. "+
		"When a deal and a sale both cover an item, the bigger one counts. It's announced in %s.\n\nPick what's on sale.", TraderMaxPercentOff, traderMention(hub.DealsChannelID))
	options := traderCategoryOptions(cat, discordgo.SelectMenuOption{Label: "Everything", Value: "0", Emoji: &discordgo.ComponentEmoji{Name: "🛒"}})
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderFormID("salecat", hub.InstallationID), Placeholder: "Choose what's on sale", Options: options}}}
	return traderPrivate(embed, menu)
}

// BuildTraderOfferStarted confirms a deal or sale.
func BuildTraderOfferStarted(hub TraderHub, o TraderOffer, postedIn string) *discordgo.InteractionResponseData {
	title := "🏷️ Deal posted"
	if o.Kind == TraderOfferSale {
		title = "🔥 Sale started"
	}
	embed := traderStaffEmbed(hub, title, presentation.Green)
	lines := []string{fmt.Sprintf("**%d%% off** %s until %s (%s).", o.PercentOff, traderOfferName(o), presentation.Timestamp(o.EndsAt, 'f'), presentation.Timestamp(o.EndsAt, 'R'))}
	if postedIn != "" {
		lines = append(lines, "Announced in "+traderMention(postedIn)+".")
	} else {
		lines = append(lines, "It's running, but Champion couldn't post it in #deals. Check it can send messages there.")
	}
	lines = append(lines, "End it early from **End deals** on the panel.")
	embed.Description = strings.Join(lines, "\n")
	return traderPrivate(embed)
}

// BuildTraderOfferList lists the running deals and sales with a way to end them.
func BuildTraderOfferList(hub TraderHub, offers []TraderOffer, notice string) *discordgo.InteractionResponseData {
	embed := traderStaffEmbed(hub, "🛑 Deals and sales running", presentation.Gold)
	var lines []string
	if notice != "" {
		lines = append(lines, notice)
	}
	if len(offers) == 0 {
		lines = append(lines, "No deals or sales are running.")
		embed.Description = strings.Join(lines, "\n\n")
		return traderPrivate(embed)
	}
	var list []string
	options := make([]discordgo.SelectMenuOption, 0, len(offers))
	for _, o := range offers {
		list = append(list, traderOfferLine(o))
		if len(options) < 25 {
			kind := "Deal"
			if o.Kind == TraderOfferSale {
				kind = "Sale"
			}
			label := fmt.Sprintf("%s: %d%% off %s", kind, o.PercentOff, presentation.CleanName(traderOfferPlainName(o), 80))
			options = append(options, discordgo.SelectMenuOption{Label: panelClip(label, 100), Value: strconv.FormatInt(o.ID, 10)})
		}
	}
	lines = append(lines, strings.Join(list, "\n"))
	embed.Description = strings.Join(lines, "\n\n")
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderID("endoffer", hub.InstallationID),
		Placeholder: "End one now", Options: options, MaxValues: len(options)}}}
	all := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "End all now", Emoji: &discordgo.ComponentEmoji{Name: "🛑"}, Style: discordgo.DangerButton, CustomID: traderID("endall", hub.InstallationID)}}}
	return traderPrivate(embed, menu, all)
}

func traderOfferPlainName(o TraderOffer) string {
	if o.Kind == TraderOfferSale && o.CategoryID == 0 {
		return "everything"
	}
	return o.Name
}

// BuildTraderOfferPost announces a deal or sale in #deals, pinging @Deals. product is the deal's
// item with its marked-down price, when known.
func BuildTraderOfferPost(hub TraderHub, o TraderOffer, product *TraderProduct) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Color: presentation.Gold, Footer: presentation.Footer(hub.ServerName, "Champion Points")}
	var view discordgo.Button
	if o.Kind == TraderOfferDeal {
		embed.Title = fmt.Sprintf("🏷️ Deal: %d%% off %s", o.PercentOff, presentation.CleanName(o.Name, 150))
		if product != nil && product.ListPrice > product.Price {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Price", Value: traderPriceText(*product), Inline: true})
		}
		view = discordgo.Button{Label: "View in trader", Emoji: &discordgo.ComponentEmoji{Name: "🛒"}, Style: discordgo.PrimaryButton,
			CustomID: traderPrivateID("item", hub.InstallationID, strconv.FormatInt(o.ProductID, 10))}
	} else {
		embed.Title = fmt.Sprintf("🔥 Sale: %d%% off %s", o.PercentOff, traderOfferPlainNameTitle(o))
		view = discordgo.Button{Label: "Shop the sale", Emoji: &discordgo.ComponentEmoji{Name: "🛒"}, Style: discordgo.PrimaryButton, CustomID: traderPrivateID("browse", hub.InstallationID)}
		if o.CategoryID != 0 {
			view.CustomID = traderPrivateID("cat", hub.InstallationID, strconv.FormatInt(o.CategoryID, 10))
		}
	}
	if t := strings.TrimSpace(o.Title); t != "" {
		embed.Description = presentation.EscapeMarkdown(presentation.CleanName(t, 200))
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Ends", Value: presentation.Timestamp(o.EndsAt, 'f') + " (" + presentation.Timestamp(o.EndsAt, 'R') + ")", Inline: true})
	buttons := []discordgo.MessageComponent{view}
	if hub.DealsRoleID != "" {
		buttons = append(buttons, discordgo.Button{Label: "Deal pings", Emoji: &discordgo.ComponentEmoji{Name: "🔔"}, Style: discordgo.SecondaryButton, CustomID: traderPrivateID("pings", hub.InstallationID)})
	}
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}}, AllowedMentions: traderNoPings}
	if hub.DealsRoleID != "" {
		msg.Content = "<@&" + hub.DealsRoleID + ">"
		msg.AllowedMentions = &discordgo.MessageAllowedMentions{Roles: []string{hub.DealsRoleID}}
	}
	return msg
}

func traderOfferPlainNameTitle(o TraderOffer) string {
	if o.CategoryID == 0 {
		return "everything"
	}
	return presentation.CleanName(o.Name, 150)
}

// BuildTraderOfferEnded is a deal's or sale's post once it is over.
func BuildTraderOfferEnded(o TraderOffer) *discordgo.InteractionResponseData {
	kind := "Deal"
	if o.Kind == TraderOfferSale {
		kind = "Sale"
	}
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Color: presentation.Neutral,
		Title:       fmt.Sprintf("⌛ %s ended: %d%% off %s", kind, o.PercentOff, presentation.CleanName(traderOfferPlainName(o), 150)),
		Description: "This one is over. Turn on deal pings to hear about the next one."}
	return &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{}}
}

// BuildTraderAnnouncement is staff news in #deals.
func BuildTraderAnnouncement(hub TraderHub, title, text, byDiscordID string, now time.Time) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Color: presentation.Crimson,
		Title: "📣 " + presentation.CleanName(title, 200), Description: presentation.CleanName(text, traderMaxAnnounceLen),
		Footer: presentation.Footer(hub.ServerName, "Trader news"), Timestamp: now.UTC().Format(time.RFC3339)}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: traderNoPings}
}

// traderOrderCardDone turns an order card into its handled state: delivered or refunded, by whom.
func traderOrderCardDone(card *discordgo.Message, hub TraderHub, o TraderOrder, byDiscordID string, now time.Time) *discordgo.InteractionResponseData {
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Footer: presentation.Footer(hub.ServerName, "")}
	if card != nil && len(card.Embeds) > 0 && card.Embeds[0] != nil {
		copied := *card.Embeds[0]
		embed = &copied
		embed.Fields = append([]*discordgo.MessageEmbedField{}, copied.Fields...)
	}
	verb, color := "delivered", presentation.Green
	if o.Status == TraderOrderRefunded {
		verb, color = "refunded", presentation.Neutral
	}
	embed.Title = fmt.Sprintf("%s Order #%d %s", map[bool]string{true: "✅", false: "↩️"}[verb == "delivered"], o.ID, verb)
	embed.Color = color
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Handled",
		Value: fmt.Sprintf("%s by <@%s> %s", strings.ToUpper(verb[:1])+verb[1:], byDiscordID, presentation.Timestamp(now, 'R'))})
	if o.Status == TraderOrderRefunded {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Points", Value: presentation.FormatPoints(o.Total) + " given back"})
	}
	return &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{}, AllowedMentions: traderNoPings}
}
