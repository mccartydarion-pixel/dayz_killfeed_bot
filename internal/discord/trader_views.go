package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// The trader's messages. Prices and balances are Champion Points; stock is a word, never a count.

var traderNoPings = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

func traderID(action string, installationID int64, args ...string) string {
	return TraderPrefix + "u:" + action + ":" + strconv.FormatInt(installationID, 10) + traderArgs(args)
}

func traderPrivateID(action string, installationID int64, args ...string) string {
	return TraderPrivatePrefix + action + ":" + strconv.FormatInt(installationID, 10) + traderArgs(args)
}

func traderFormID(action string, installationID int64, args ...string) string {
	return TraderFormPrefix + action + ":" + strconv.FormatInt(installationID, 10) + traderArgs(args)
}

func traderArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return ":" + strings.Join(args, ":")
}

func traderStockText(stock string) string {
	switch stock {
	case TraderStockOut:
		return "Sold out"
	case TraderStockLow:
		return "Low stock"
	default:
		return "In stock"
	}
}

func traderName(s string) string { return presentation.EscapeMarkdown(presentation.CleanName(s, 80)) }

func traderEmbed(hub TraderHub, title string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: title, Color: color,
		Footer: presentation.Footer(hub.ServerName, "Champion Points")}
}

func traderPrivate(embed *discordgo.MessageEmbed, rows ...discordgo.MessageComponent) *discordgo.InteractionResponseData {
	if rows == nil {
		rows = []discordgo.MessageComponent{}
	}
	return &discordgo.InteractionResponseData{Content: "", Embeds: []*discordgo.MessageEmbed{embed}, Components: rows, AllowedMentions: traderNoPings}
}

func traderStatusLine(open bool) string {
	if open {
		return "🟢 **Open**"
	}
	return "🔴 **Closed**"
}

// BuildTraderPost is the shared message in #trader.
func BuildTraderPost(hub TraderHub, cat TraderCatalog) *discordgo.InteractionResponseData {
	color := presentation.Gold
	lines := []string{traderStatusLine(hub.Open)}
	if hub.Open {
		lines = append(lines, "Spend the Champion Points you earn in game. Press **Browse** to shop; everything after that is private to you.")
	} else {
		color = presentation.Red
		if note := strings.TrimSpace(hub.ClosedNote); note != "" {
			lines = append(lines, "> "+presentation.EscapeMarkdown(presentation.CleanName(note, traderMaxNoteRunes)))
		}
		lines = append(lines, "The trader is closed. You can still check your wallet and orders, and orders already placed are still delivered.")
	}
	if hours := traderHoursText(hub); hours != "" {
		lines = append(lines, "🕒 Open every day "+hours+".")
	}
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: "🛒 Trader", Color: color, Description: strings.Join(lines, "\n\n"),
		Footer: presentation.Footer(hub.ServerName, "Updates when the trader changes")}
	if len(cat.Featured) > 0 {
		var b []string
		for idx, p := range cat.Featured {
			if idx == 8 {
				break
			}
			b = append(b, fmt.Sprintf("**%s** · %s · %s", traderName(p.Name), traderPriceText(p), traderStockText(p.Stock)))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "⭐ Featured", Value: strings.Join(b, "\n")})
	}
	if len(cat.Offers) > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "🏷️ On sale now", Value: traderOfferLines(cat.Offers, 6)})
	}
	if len(cat.Categories) > 0 {
		var names []string
		for _, c := range cat.Categories {
			names = append(names, fmt.Sprintf("%s (%d)", traderName(c.Name), c.Count))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Categories", Value: presentation.CleanName(strings.Join(names, " · "), 1000)})
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Browse", Emoji: &discordgo.ComponentEmoji{Name: "🛒"}, Style: discordgo.PrimaryButton, CustomID: traderPrivateID("browse", hub.InstallationID), Disabled: !hub.Open},
		discordgo.Button{Label: "My wallet", Emoji: &discordgo.ComponentEmoji{Name: "💰"}, Style: discordgo.SecondaryButton, CustomID: traderPrivateID("wallet", hub.InstallationID)},
		discordgo.Button{Label: "My orders", Emoji: &discordgo.ComponentEmoji{Name: "📦"}, Style: discordgo.SecondaryButton, CustomID: traderPrivateID("orders", hub.InstallationID)},
	}}
	if hub.DealsRoleID != "" {
		row.Components = append(row.Components, discordgo.Button{Label: "Deal pings", Emoji: &discordgo.ComponentEmoji{Name: "🔔"}, Style: discordgo.SecondaryButton,
			CustomID: traderPrivateID("pings", hub.InstallationID)})
	}
	return &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{row}, AllowedMentions: traderNoPings}
}

// traderOfferLines lists up to max running deals and sales.
func traderOfferLines(offers []TraderOffer, max int) string {
	var b []string
	for idx, o := range offers {
		if idx == max {
			b = append(b, fmt.Sprintf("…and %d more", len(offers)-idx))
			break
		}
		b = append(b, traderOfferLine(o))
	}
	return strings.Join(b, "\n")
}

// BuildTraderPanel is the staff panel in #trader-panel.
func BuildTraderPanel(hub TraderHub, cat TraderCatalog, now time.Time) *discordgo.InteractionResponseData {
	color := presentation.Green
	if !hub.Open {
		color = presentation.Red
	}
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: "🧰 Trader panel", Color: color,
		Footer: presentation.Footer(hub.ServerName, "Trader role, Owner and Administrators")}
	state := "🟢 Open"
	if !hub.Open {
		state = "🔴 Closed"
	}
	embed.Fields = append(embed.Fields,
		&discordgo.MessageEmbedField{Name: "Trader", Value: state, Inline: true},
		&discordgo.MessageEmbedField{Name: "Orders to hand over", Value: strconv.Itoa(cat.OrdersWaiting), Inline: true})
	if hub.VoiceChannelID != "" {
		voice := traderMention(hub.VoiceChannelID)
		if hub.VoiceNameApplied != TraderVoiceName(hub.Open) {
			voice += "\nRenaming: Discord allows 2 renames every 10 minutes, so it can take a few minutes."
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Status channel", Value: voice, Inline: true})
	}
	if hub.OpenChangedAt != nil && hub.OpenChangedByDiscordID != "" {
		verb := "Opened"
		if !hub.Open {
			verb = "Closed"
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Last change",
			Value: fmt.Sprintf("%s by <@%s> %s", verb, hub.OpenChangedByDiscordID, presentation.Timestamp(*hub.OpenChangedAt, 'R'))})
	}
	if !hub.Open && strings.TrimSpace(hub.ClosedNote) != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Message to players", Value: presentation.EscapeMarkdown(presentation.CleanName(hub.ClosedNote, traderMaxNoteRunes))})
	}
	hours := traderHoursText(hub)
	if hours == "" {
		hours = "Not set: open and close by hand"
	} else {
		hours = "Every day " + hours
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Opening hours", Value: hours})
	if len(cat.Offers) > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "On sale now", Value: traderOfferLines(cat.Offers, 10)})
	}
	if len(cat.StockAlerts) > 0 {
		var b []string
		for idx, p := range cat.StockAlerts {
			if idx == 10 {
				b = append(b, fmt.Sprintf("…and %d more", len(cat.StockAlerts)-idx))
				break
			}
			b = append(b, fmt.Sprintf("%s · %s", traderName(p.Name), traderStockText(p.Stock)))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Stock alerts", Value: strings.Join(b, "\n")})
	}
	if hub.OrdersChannelID != "" {
		embed.Description = "New orders arrive in " + traderMention(hub.OrdersChannelID) + ". Hand them over in game, then press **Mark delivered** on the order's card."
	}
	presentation.StampEmbed(embed, now)
	power := discordgo.Button{Label: "Close trader", Emoji: &discordgo.ComponentEmoji{Name: "🔴"}, Style: discordgo.DangerButton, CustomID: traderFormID("close", hub.InstallationID)}
	if !hub.Open {
		power = discordgo.Button{Label: "Open trader", Emoji: &discordgo.ComponentEmoji{Name: "🟢"}, Style: discordgo.SuccessButton, CustomID: traderID("open", hub.InstallationID)}
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{power,
		discordgo.Button{Label: "Post deal", Emoji: &discordgo.ComponentEmoji{Name: "🏷️"}, Style: discordgo.PrimaryButton, CustomID: traderPrivateID("deal", hub.InstallationID)},
		discordgo.Button{Label: "Start sale", Emoji: &discordgo.ComponentEmoji{Name: "🔥"}, Style: discordgo.PrimaryButton, CustomID: traderPrivateID("sale", hub.InstallationID)},
		discordgo.Button{Label: "Announce", Emoji: &discordgo.ComponentEmoji{Name: "📣"}, Style: discordgo.SecondaryButton, CustomID: traderFormID("announce", hub.InstallationID)},
	}}
	row2 := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Opening hours", Emoji: &discordgo.ComponentEmoji{Name: "🕒"}, Style: discordgo.SecondaryButton, CustomID: traderFormID("hours", hub.InstallationID)},
		discordgo.Button{Label: "End deals", Emoji: &discordgo.ComponentEmoji{Name: "🛑"}, Style: discordgo.SecondaryButton, CustomID: traderPrivateID("offers", hub.InstallationID), Disabled: len(cat.Offers) == 0},
		discordgo.Button{Label: "Items", Emoji: &discordgo.ComponentEmoji{Name: "📦"}, Style: discordgo.PrimaryButton, CustomID: traderPrivateID("items", hub.InstallationID)},
		discordgo.Button{Label: "↻ Refresh", Style: discordgo.SecondaryButton, CustomID: traderID("panel", hub.InstallationID)},
	}}
	return &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{row, row2}, AllowedMentions: traderNoPings}
}

// BuildTraderClosed answers a player while the trader is closed.
func BuildTraderClosed(hub TraderHub) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, "🔴 The trader is closed", presentation.Red)
	var lines []string
	if note := strings.TrimSpace(hub.ClosedNote); note != "" {
		lines = append(lines, "> "+presentation.EscapeMarkdown(presentation.CleanName(note, traderMaxNoteRunes)))
	}
	if hours := traderHoursText(hub); hours != "" {
		lines = append(lines, "🕒 Open every day "+hours+".")
	}
	lines = append(lines, "Watch the Trader voice channel for when it opens. You can still check **My wallet** and **My orders**.")
	embed.Description = strings.Join(lines, "\n\n")
	return traderPrivate(embed)
}

func traderBackRow(hub TraderHub) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Categories", Emoji: &discordgo.ComponentEmoji{Name: "⬅️"}, Style: discordgo.SecondaryButton, CustomID: traderID("browse", hub.InstallationID)},
	}}
}

// BuildTraderBrowse is the category picker.
func BuildTraderBrowse(hub TraderHub, cat TraderCatalog, balance int64) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, "🛒 Pick a category", presentation.Gold)
	embed.Description = "Your balance: **" + presentation.FormatPoints(balance) + "**"
	total := 0
	for _, c := range cat.Categories {
		total += c.Count
	}
	if total == 0 && len(cat.Featured) == 0 {
		embed.Description += "\n\nThe trader has nothing for sale yet. Check back soon."
		return traderPrivate(embed)
	}
	options := []discordgo.SelectMenuOption{{Label: "All items", Value: "all", Emoji: &discordgo.ComponentEmoji{Name: "📋"}}}
	for _, c := range cat.Categories {
		if len(options) == 25 {
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(c.Name, 100), 100), Value: strconv.FormatInt(c.ID, 10),
			Description: presentation.Plural(int64(c.Count), "item", "items")})
	}
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderID("cat", hub.InstallationID), Placeholder: "Choose a category", Options: options}}}
	return traderPrivate(embed, menu)
}

// BuildTraderCategory lists a category's items with a picker.
func BuildTraderCategory(hub TraderHub, categoryID int64, name string, products []TraderProduct, balance int64) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, presentation.CleanName(name, 200), presentation.Gold)
	if len(products) == 0 {
		embed.Description = "Nothing for sale here right now.\n\nYour balance: **" + presentation.FormatPoints(balance) + "**"
		return traderPrivate(embed, traderBackRow(hub))
	}
	var lines []string
	options := make([]discordgo.SelectMenuOption, 0, len(products))
	for idx, p := range products {
		if idx == 25 {
			break
		}
		lines = append(lines, fmt.Sprintf("**%s** · %s · %s", traderName(p.Name), traderPriceText(p), traderStockText(p.Stock)))
		desc := panelClip(traderPricePlain(p)+" · "+traderStockText(p.Stock), 100)
		options = append(options, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(p.Name, 100), 100), Value: strconv.FormatInt(p.ID, 10), Description: desc})
	}
	embed.Description = strings.Join(lines, "\n") + "\n\nYour balance: **" + presentation.FormatPoints(balance) + "**"
	menu := discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderID("item", hub.InstallationID), Placeholder: "Choose an item", Options: options}}}
	return traderPrivate(embed, menu, traderBackRow(hub))
}

// traderCannotBuy is why the player can't buy p, "" when they can.
func traderCannotBuy(p TraderProduct, balance int64, mapName string) string {
	switch {
	case p.soldOut():
		return "Sold out."
	case p.Automatic:
		return "This item is delivered to where you stand in game, so it's sold on the Champion website shop only."
	case p.Coordinates && mapName == "":
		return "Staff haven't set the map for drop-off deliveries yet."
	case balance < p.Price:
		return "You need " + presentation.FormatPoints(p.Price-balance) + " more."
	}
	return ""
}

func traderDeliveryText(p TraderProduct, mapName string) string {
	if p.Coordinates {
		if mapName != "" {
			return "Dropped at map coordinates you choose (" + mapName + ")"
		}
		return "Dropped at map coordinates you choose"
	}
	return "Handed over by the trader in game"
}

// BuildTraderItem is one item's card.
func BuildTraderItem(hub TraderHub, p TraderProduct, balance int64, mapName string) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, presentation.CleanName(p.Name, 200), presentation.Gold)
	if d := strings.TrimSpace(p.Description); d != "" {
		embed.Description = presentation.CleanName(d, 1000)
	}
	embed.Fields = append(embed.Fields,
		&discordgo.MessageEmbedField{Name: "Price", Value: traderPriceText(p), Inline: true},
		&discordgo.MessageEmbedField{Name: "Stock", Value: traderStockText(p.Stock), Inline: true})
	if p.Limit != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Limit", Value: strconv.Itoa(*p.Limit) + " per player", Inline: true})
	}
	if p.OfferEnds != nil && p.PercentOff > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "🏷️ Deal", Value: fmt.Sprintf("%d%% off until %s", p.PercentOff, presentation.Timestamp(*p.OfferEnds, 'R'))})
	}
	embed.Fields = append(embed.Fields,
		&discordgo.MessageEmbedField{Name: "Delivery", Value: traderDeliveryText(p, mapName)},
		&discordgo.MessageEmbedField{Name: "Your balance", Value: presentation.FormatPoints(balance), Inline: true})
	why := traderCannotBuy(p, balance, mapName)
	if why != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Can't buy", Value: why})
	}
	pid := strconv.FormatInt(p.ID, 10)
	buttons := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Buy for " + presentation.FormatPoints(p.Price), Style: discordgo.SuccessButton, CustomID: traderID("buy", hub.InstallationID, pid), Disabled: why != ""},
		discordgo.Button{Label: "Categories", Emoji: &discordgo.ComponentEmoji{Name: "⬅️"}, Style: discordgo.SecondaryButton, CustomID: traderID("browse", hub.InstallationID)},
	}}
	rows := []discordgo.MessageComponent{}
	maxQty := traderMaxQuantity
	if p.Limit != nil && *p.Limit < maxQty {
		maxQty = *p.Limit
	}
	if why == "" && maxQty > 1 {
		var options []discordgo.SelectMenuOption
		for q := 2; q <= maxQty; q++ {
			options = append(options, discordgo.SelectMenuOption{Label: fmt.Sprintf("Buy %d for %s", q, presentation.FormatPoints(p.Price*int64(q))), Value: strconv.Itoa(q)})
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{CustomID: traderID("qty", hub.InstallationID, pid), Placeholder: "Buy more than one", Options: options}}})
	}
	rows = append(rows, buttons)
	return traderPrivate(embed, rows...)
}

// BuildTraderConfirm is the last screen before paying.
func BuildTraderConfirm(hub TraderHub, p TraderProduct, qty int, balance int64, nonce string) *discordgo.InteractionResponseData {
	total := p.Price * int64(qty)
	embed := traderEmbed(hub, "Confirm your purchase", presentation.Amber)
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Item", Value: fmt.Sprintf("%d × %s", qty, traderName(p.Name)), Inline: true},
		{Name: "Total", Value: presentation.FormatPoints(total), Inline: true},
		{Name: "Balance after", Value: presentation.FormatPoints(balance - total), Inline: true},
	}
	args := []string{strconv.FormatInt(p.ID, 10), strconv.Itoa(qty), nonce}
	var pay discordgo.Button
	if p.Coordinates {
		embed.Description = "Next, enter where to drop it."
		pay = discordgo.Button{Label: "Enter drop point", Emoji: &discordgo.ComponentEmoji{Name: "📍"}, Style: discordgo.SuccessButton, CustomID: traderFormID("where", hub.InstallationID, args...)}
	} else {
		embed.Description = "The trader hands it over in game. The price is checked again when you confirm."
		pay = discordgo.Button{Label: "Confirm purchase", Style: discordgo.SuccessButton, CustomID: traderID("pay", hub.InstallationID, args...)}
	}
	if balance < total {
		embed.Description = "You need " + presentation.FormatPoints(total-balance) + " more for this."
		pay.Disabled = true
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{pay,
		discordgo.Button{Label: "Cancel", Style: discordgo.SecondaryButton, CustomID: traderID("cancel", hub.InstallationID)}}}
	return traderPrivate(embed, row)
}

// BuildTraderCoordsForm asks where to drop a coordinate delivery.
func BuildTraderCoordsForm(installationID, productID int64, qty int, nonce, mapName string) *discordgo.InteractionResponseData {
	title := "Where should it be dropped?"
	placeholderX, placeholderZ := "e.g. 4621", "e.g. 10318"
	label := func(axis string) string {
		if mapName != "" {
			return axis + " coordinate on " + mapName
		}
		return axis + " coordinate"
	}
	return &discordgo.InteractionResponseData{
		CustomID: traderCoordsPrefix + strconv.FormatInt(installationID, 10) + ":" + strconv.FormatInt(productID, 10) + ":" + strconv.Itoa(qty) + ":" + nonce,
		Title:    title,
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: traderCoordsX, Label: panelClip(label("X"), 45), Style: discordgo.TextInputShort, Placeholder: placeholderX, Required: true, MaxLength: 12}}},
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: traderCoordsZ, Label: panelClip(label("Z"), 45), Style: discordgo.TextInputShort, Placeholder: placeholderZ, Required: true, MaxLength: 12}}},
		},
	}
}

func traderDropText(x, z *float64, mapName string) string {
	if x == nil || z == nil {
		return "Handed over in game"
	}
	at := fmt.Sprintf("X %s / Z %s", traderCoordText(*x), traderCoordText(*z))
	if mapName != "" {
		at += " on " + mapName
	}
	return "Dropped at " + at
}

// traderCoordText is a map coordinate as players type it: whole metres without ".0".
func traderCoordText(v float64) string {
	if v == float64(int64(v)) {
		return presentation.FormatThousands(int64(v))
	}
	return presentation.FormatDecimal(v)
}

// BuildTraderReceipt confirms a placed order.
func BuildTraderReceipt(hub TraderHub, r TraderReceipt) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, fmt.Sprintf("✅ Order #%d placed", r.OrderID), presentation.Green)
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Item", Value: fmt.Sprintf("%d × %s", r.Quantity, traderName(r.Item)), Inline: true},
		{Name: "Paid", Value: presentation.FormatPoints(r.Total), Inline: true},
		{Name: "Balance", Value: presentation.FormatPoints(r.Balance), Inline: true},
		{Name: "Delivery", Value: traderDropText(r.X, r.Z, r.MapName)},
	}
	if r.Saved > 0 {
		embed.Fields = append(embed.Fields[:2], append([]*discordgo.MessageEmbedField{{Name: "Saved", Value: presentation.FormatPoints(r.Saved), Inline: true}}, embed.Fields[2:]...)...)
	}
	embed.Description = "The trader has your order. You'll get a DM when it's delivered."
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "My orders", Emoji: &discordgo.ComponentEmoji{Name: "📦"}, Style: discordgo.SecondaryButton, CustomID: traderID("orders", hub.InstallationID)},
		discordgo.Button{Label: "Keep shopping", Style: discordgo.SecondaryButton, CustomID: traderID("browse", hub.InstallationID)},
	}}
	return traderPrivate(embed, row)
}

// BuildTraderOrderCard tells staff about a new order.
func BuildTraderOrderCard(hub TraderHub, buyerDiscordID string, r TraderReceipt) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: fmt.Sprintf("🛒 New order #%d", r.OrderID), Color: presentation.Amber,
		Footer: presentation.Footer(hub.ServerName, "Hand it over in game, then mark it delivered")}
	player := traderName(r.Gamertag)
	if buyerDiscordID != "" {
		player += " (<@" + buyerDiscordID + ">)"
	}
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Player", Value: player, Inline: true},
		{Name: "Item", Value: fmt.Sprintf("%d × %s", r.Quantity, traderName(r.Item)), Inline: true},
		{Name: "Paid", Value: presentation.FormatPoints(r.Total), Inline: true},
		{Name: "Delivery", Value: traderDropText(r.X, r.Z, r.MapName)},
	}
	embed.Timestamp = time.Now().UTC().Format(time.RFC3339)
	order := strconv.FormatInt(r.OrderID, 10)
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Mark delivered", Emoji: &discordgo.ComponentEmoji{Name: "✅"}, Style: discordgo.SuccessButton, CustomID: traderID("deliver", hub.InstallationID, order)},
		discordgo.Button{Label: "Refund", Emoji: &discordgo.ComponentEmoji{Name: "↩️"}, Style: discordgo.SecondaryButton, CustomID: traderFormID("refund", hub.InstallationID, order)},
	}}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: []discordgo.MessageComponent{row}, AllowedMentions: traderNoPings}
}

// BuildTraderWallet is the player's balance and latest points history.
func BuildTraderWallet(hub TraderHub, w TraderWallet) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, "💰 Your wallet", presentation.Gold)
	embed.Fields = []*discordgo.MessageEmbedField{{Name: "Balance", Value: presentation.FormatPoints(w.Balance), Inline: true}}
	if w.Gamertag != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Gamertag", Value: traderName(w.Gamertag), Inline: true})
	}
	if len(w.Recent) > 0 {
		var lines []string
		for _, e := range w.Recent {
			sign := "−"
			if e.Credit {
				sign = "+"
			}
			line := sign + presentation.FormatThousands(e.Amount)
			if d := strings.TrimSpace(e.Description); d != "" {
				line += " · " + presentation.EscapeMarkdown(presentation.CleanName(d, 80))
			}
			lines = append(lines, line+" · "+presentation.Timestamp(e.At, 'R'))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Recent", Value: strings.Join(lines, "\n")})
	}
	embed.Description = "Earn Champion Points with kills, streaks, events and bounties."
	return traderPrivate(embed)
}

func traderOrderStatus(status string) string {
	switch status {
	case TraderOrderWaiting:
		return "🟡 Waiting for delivery"
	case TraderOrderDelivered:
		return "🟢 Delivered"
	case TraderOrderRefunded:
		return "↩️ Refunded"
	default:
		return strings.ToLower(strings.ReplaceAll(status, "_", " "))
	}
}

// BuildTraderOrders lists the player's latest orders.
func BuildTraderOrders(hub TraderHub, orders []TraderOrder) *discordgo.InteractionResponseData {
	embed := traderEmbed(hub, "📦 Your orders", presentation.Gold)
	if len(orders) == 0 {
		embed.Description = "No orders yet. Press **Browse** to shop."
		return traderPrivate(embed)
	}
	var lines []string
	for _, o := range orders {
		lines = append(lines, fmt.Sprintf("**#%d** · %s · %s\n%s · %s", o.ID, traderName(o.Items), presentation.FormatPoints(o.Total), traderOrderStatus(o.Status), presentation.Timestamp(o.At, 'R')))
	}
	embed.Description = strings.Join(lines, "\n\n")
	return traderPrivate(embed)
}

// BuildTraderSetupDone answers /trader setup.
func BuildTraderSetupDone(hub TraderHub, made []string, isNew bool) *discordgo.InteractionResponseData {
	title := "✅ Trader is set up"
	if !isNew {
		title = "✅ Trader checked"
	}
	embed := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Trader"), Title: title, Color: presentation.Green, Footer: presentation.Footer(hub.ServerName, "")}
	var lines []string
	if len(made) > 0 {
		lines = append(lines, "Created "+strings.Join(made, ", ")+".")
	} else {
		lines = append(lines, "Everything was already in place; the trader post and panel are refreshed.")
	}
	lines = append(lines,
		"• Players shop in "+traderMention(hub.TraderChannelID)+".",
		"• The "+traderRoleMention(hub.TraderRoleID)+" role, the Owner and Administrators open and close the trader in "+traderMention(hub.PanelChannelID)+" and get new orders in "+traderMention(hub.OrdersChannelID)+".",
		"• Deals, sales and news go in "+traderMention(hub.DealsChannelID)+traderDealsRoleLine(hub)+".",
		"• Items, prices and stock are edited on the Champion website (Shop).",
		"Give the Trader role to the people who run your trader. Gave someone the Administrator staff level since? Run `/trader setup` again so they see the private channels.")
	embed.Description = strings.Join(lines, "\n")
	return &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: traderNoPings}
}

func traderDealsRoleLine(hub TraderHub) string {
	if hub.DealsRoleID == "" {
		return ""
	}
	return "; players who press **Deal pings** get <@&" + hub.DealsRoleID + "> and are pinged for new ones"
}
