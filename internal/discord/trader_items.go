package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// Trader items (docs/TRADER.md): the Trader role, Administrators and the Owner add and edit the
// shop's items from #trader-panel. It is the website shop's own catalog, so the website and Discord
// always show the same items.

// TraderItem is a shop item as staff manage it, including one turned off.
type TraderItem struct {
	ID           int64
	Name         string
	Description  string
	Price        int64
	CategoryID   int64
	CategoryName string
	Active       bool
	Featured     bool
	// Stock is nil for unlimited.
	Stock *int64
	// Limit is the most one player may buy; nil for no limit.
	Limit *int
	// DropOff: the buyer gives map coordinates to drop it at.
	DropOff bool
	// NeedsMap: a drop-off item can't be bought until staff set the delivery map on the website.
	NeedsMap bool
	// Automatic: delivered by the automatic delivery worker; sold on the website only.
	Automatic bool
}

// TraderItemInput is a new item. Category is a category's name ("" for none): an existing one
// whatever its capitals, else a new one.
type TraderItemInput struct {
	Name, Description, Category string
	Price                       int64
	Stock                       *int64
}

// TraderItemEdit changes an item; a nil field is unchanged.
type TraderItemEdit struct {
	Name, Description, Category *string
	Price                       *int64
	// Stock sets a count; Unlimited sets unlimited stock.
	Stock     *int64
	Unlimited bool
	// Limit sets the per-player limit; ClearLimit removes it.
	Limit      *int
	ClearLimit bool
	Active     *bool
	Featured   *bool
	DropOff    *bool
}

// TraderItemInvalid is an item the shop refused, with why in words.
type TraderItemInvalid struct{ Reason string }

func (e *TraderItemInvalid) Error() string { return "trader: invalid item: " + e.Reason }

// Item form fields.
const (
	traderFieldName     = "trader_name"
	traderFieldPrice    = "trader_price"
	traderFieldStock    = "trader_stock"
	traderFieldCategory = "trader_category"
	traderFieldDesc     = "trader_desc"
	traderFieldLimit    = "trader_limit"
)

// itemAction handles the Items screens. It returns false for any other action.
func (h *TraderHandler) itemAction(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, action string, args, values []string, send traderSend) bool {
	switch action {
	case "items":
		categoryID := int64(0)
		switch {
		case len(values) == 1 && values[0] != "all":
			categoryID, _ = strconv.ParseInt(values[0], 10, 64)
		case len(args) == 1:
			categoryID, _ = strconv.ParseInt(args[0], 10, 64)
		}
		h.itemList(ctx, s, i, caller, hub, categoryID, "", send)
	case "itemedit":
		var productID int64
		switch {
		case len(values) == 1:
			productID, _ = strconv.ParseInt(values[0], 10, 64)
		case len(args) == 1:
			productID, _ = strconv.ParseInt(args[0], 10, 64)
		}
		if productID <= 0 {
			return true
		}
		item, err := h.backend.TraderItem(ctx, caller.PanelCaller, hub.InstallationID, productID)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		send(s, i, BuildTraderItemManage(hub, item, ""))
	case "itemactive", "itemfeat", "itemdrop":
		if len(args) != 2 {
			return true
		}
		productID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || productID <= 0 {
			return true
		}
		on := args[1] == "1"
		var edit TraderItemEdit
		var notice string
		switch action {
		case "itemactive":
			edit.Active = &on
			notice = map[bool]string{true: "Turned on: players can buy it again.", false: "Turned off: players don't see it any more."}[on]
		case "itemfeat":
			edit.Featured = &on
			notice = map[bool]string{true: "Featured on the trader post.", false: "No longer featured."}[on]
		default:
			edit.DropOff = &on
			notice = map[bool]string{true: "Buyers now choose where it's dropped (map coordinates).", false: "Handed over in game; buyers no longer give coordinates."}[on]
		}
		h.saveItem(ctx, s, i, caller, hub, productID, edit, notice)
	default:
		return false
	}
	return true
}

func (h *TraderHandler) itemList(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, categoryID int64, notice string, send traderSend) {
	cats, err := h.backend.TraderItemCategories(ctx, caller.PanelCaller, hub.InstallationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	items, err := h.backend.TraderItems(ctx, caller.PanelCaller, hub.InstallationID, categoryID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	send(s, i, BuildTraderItems(hub, cats, categoryID, items, notice))
}

// saveItem changes an item and shows it again, refreshing the trader post and panel.
func (h *TraderHandler) saveItem(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, productID int64, edit TraderItemEdit, notice string) {
	item, err := h.backend.TraderEditItem(ctx, caller.PanelCaller, hub.InstallationID, productID, edit)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondUpdate(s, i, BuildTraderItemManage(hub, item, notice))
	h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
}

// openItemForm opens the item forms; false for any other action.
func (h *TraderHandler) openItemForm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, action string, installationID int64, args []string) (*discordgo.InteractionResponseData, bool) {
	switch action {
	case "additem":
		return BuildTraderAddItemForm(installationID), true
	case "edititem", "stockitem":
	default:
		return nil, false
	}
	caller, ok := panelCallerOf(i)
	if !ok || len(args) != 1 {
		return nil, true
	}
	productID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || productID <= 0 {
		return nil, true
	}
	item, err := h.backend.TraderItem(ctx, caller, installationID, productID)
	if err != nil {
		h.reportError(s, i, err)
		return nil, true
	}
	if action == "stockitem" {
		return BuildTraderStockForm(installationID, item), true
	}
	return BuildTraderEditItemForm(installationID, item), true
}

// submitItemForm handles a submitted item form; false for any other action.
func (h *TraderHandler) submitItemForm(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller TraderCaller, hub TraderHub, action string, args []string) bool {
	data := i.ModalSubmitData()
	switch action {
	case "additem":
		in := TraderItemInput{Name: traderClip(modalValue(data, traderFieldName), 80), Description: strings.TrimSpace(modalValue(data, traderFieldDesc)),
			Category: traderClip(modalValue(data, traderFieldCategory), 60)}
		var err error
		if in.Price, err = parseTraderPoints(modalValue(data, traderFieldPrice)); err != nil {
			h.reportError(s, i, &TraderItemInvalid{Reason: "Enter the price as a whole number of points, like 1500."})
			return true
		}
		if in.Stock, err = parseTraderStock(modalValue(data, traderFieldStock)); err != nil {
			h.reportError(s, i, &TraderItemInvalid{Reason: "Enter the stock as a whole number, or leave it empty for unlimited."})
			return true
		}
		item, err := h.backend.TraderAddItem(ctx, caller.PanelCaller, hub.InstallationID, in)
		if err != nil {
			h.reportError(s, i, err)
			return true
		}
		respondUpdate(s, i, BuildTraderItemManage(hub, item, "✅ Added. It's on sale now; turn it off here to hide it."))
		h.publish(context.WithoutCancel(ctx), h.apiFor(s), hub)
	case "edititem":
		productID, ok := traderItemArg(args)
		if !ok {
			return true
		}
		name, desc, category := traderClip(modalValue(data, traderFieldName), 80), strings.TrimSpace(modalValue(data, traderFieldDesc)), traderClip(modalValue(data, traderFieldCategory), 60)
		edit := TraderItemEdit{Name: &name, Description: &desc, Category: &category}
		price, err := parseTraderPoints(modalValue(data, traderFieldPrice))
		if err != nil {
			h.reportError(s, i, &TraderItemInvalid{Reason: "Enter the price as a whole number of points, like 1500."})
			return true
		}
		edit.Price = &price
		if raw := strings.TrimSpace(modalValue(data, traderFieldLimit)); raw == "" {
			edit.ClearLimit = true
		} else {
			limit, err := strconv.Atoi(raw)
			if err != nil {
				h.reportError(s, i, &TraderItemInvalid{Reason: "Enter the limit per player as a whole number, or leave it empty for no limit."})
				return true
			}
			edit.Limit = &limit
		}
		h.saveItem(ctx, s, i, caller, hub, productID, edit, "✅ Saved.")
	case "stockitem":
		productID, ok := traderItemArg(args)
		if !ok {
			return true
		}
		stock, err := parseTraderStock(modalValue(data, traderFieldStock))
		if err != nil {
			h.reportError(s, i, &TraderItemInvalid{Reason: "Enter the stock as a whole number, or leave it empty for unlimited."})
			return true
		}
		edit := TraderItemEdit{Stock: stock, Unlimited: stock == nil}
		notice := "📦 Stock is now unlimited."
		if stock != nil {
			notice = "📦 Stock set to " + presentation.FormatThousands(*stock) + "."
		}
		h.saveItem(ctx, s, i, caller, hub, productID, edit, notice)
	default:
		return false
	}
	return true
}

func traderItemArg(args []string) (int64, bool) {
	if len(args) != 1 {
		return 0, false
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	return id, err == nil && id > 0
}

// parseTraderPoints reads a price as typed ("1500", "1,500", "1500 pts").
func parseTraderPoints(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(s, "points"), "pts"))
	s = strings.NewReplacer(",", "", " ", "", "_", "").Replace(s)
	return strconv.ParseInt(s, 10, 64)
}

// parseTraderStock reads a stock count; empty or "unlimited" is nil.
func parseTraderStock(s string) (*int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "unlimited" || s == "∞" {
		return nil, nil
	}
	n, err := strconv.ParseInt(strings.NewReplacer(",", "", " ", "").Replace(s), 10, 64)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// --- views -------------------------------------------------------------------------------------------

func traderItemStockText(it TraderItem) string {
	if it.Stock == nil {
		return "Unlimited"
	}
	if *it.Stock <= 0 {
		return "Sold out (0)"
	}
	return presentation.FormatThousands(*it.Stock) + " left"
}

func traderItemStatus(it TraderItem) string {
	if !it.Active {
		return "⚫ Off"
	}
	return "🟢 On sale"
}

// BuildTraderItems is the staff item list: pick a category, pick an item, or add one.
func BuildTraderItems(hub TraderHub, cats []TraderCategory, categoryID int64, items []TraderItem, notice string) *discordgo.InteractionResponseData {
	embed := traderStaffEmbed(hub, "📦 Items", presentation.Gold)
	var lines []string
	if notice != "" {
		lines = append(lines, notice)
	}
	if len(items) == 0 {
		lines = append(lines, "No items here yet. Press **Add item**.")
	} else {
		var list []string
		for _, it := range items {
			line := fmt.Sprintf("%s **%s** · %s · %s", map[bool]string{true: "🟢", false: "⚫"}[it.Active], traderName(it.Name), presentation.FormatPoints(it.Price), traderItemStockText(it))
			list = append(list, line)
		}
		lines = append(lines, strings.Join(list, "\n"))
		if len(items) >= 25 {
			lines = append(lines, "Only the first 25 are listed; pick a category to find others.")
		}
	}
	lines = append(lines, "Changes show on the trader and the website shop at once. Prices are in Champion Points.")
	embed.Description = strings.Join(lines, "\n\n")
	options := []discordgo.SelectMenuOption{{Label: "All items", Value: "all", Emoji: &discordgo.ComponentEmoji{Name: "📋"}, Default: categoryID == 0}}
	for _, c := range cats {
		if len(options) == 25 {
			break
		}
		options = append(options, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(c.Name, 100), 100), Value: strconv.FormatInt(c.ID, 10),
			Description: presentation.Plural(int64(c.Count), "item", "items"), Default: c.ID == categoryID})
	}
	rows := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{CustomID: traderID("items", hub.InstallationID), Placeholder: "Choose a category", Options: options}}}}
	if len(items) > 0 {
		pick := make([]discordgo.SelectMenuOption, 0, len(items))
		for _, it := range items {
			if len(pick) == 25 {
				break
			}
			desc := presentation.FormatPoints(it.Price) + " · " + traderItemStockText(it)
			if !it.Active {
				desc = "Off · " + desc
			}
			pick = append(pick, discordgo.SelectMenuOption{Label: panelClip(presentation.CleanName(it.Name, 100), 100), Value: strconv.FormatInt(it.ID, 10), Description: panelClip(desc, 100)})
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: traderID("itemedit", hub.InstallationID), Placeholder: "Choose an item to change", Options: pick}}})
	}
	rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Add item", Emoji: &discordgo.ComponentEmoji{Name: "➕"}, Style: discordgo.SuccessButton, CustomID: traderFormID("additem", hub.InstallationID)}}})
	return traderPrivate(embed, rows...)
}

// BuildTraderItemManage is one item for staff, with what they can change.
func BuildTraderItemManage(hub TraderHub, it TraderItem, notice string) *discordgo.InteractionResponseData {
	color := presentation.Gold
	if !it.Active {
		color = presentation.Neutral
	}
	embed := traderStaffEmbed(hub, "📦 "+presentation.CleanName(it.Name, 200), color)
	var lines []string
	if notice != "" {
		lines = append(lines, notice)
	}
	if d := strings.TrimSpace(it.Description); d != "" {
		lines = append(lines, presentation.CleanName(d, 1000))
	}
	embed.Description = strings.Join(lines, "\n\n")
	category := "None"
	if it.CategoryName != "" {
		category = presentation.EscapeMarkdown(presentation.CleanName(it.CategoryName, 60))
	}
	limit := "None"
	if it.Limit != nil {
		limit = strconv.Itoa(*it.Limit) + " per player"
	}
	delivery := "Handed over in game"
	if it.DropOff {
		delivery = "Dropped at map coordinates the buyer gives"
	}
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Status", Value: traderItemStatus(it), Inline: true},
		{Name: "Price", Value: presentation.FormatPoints(it.Price), Inline: true},
		{Name: "Stock", Value: traderItemStockText(it), Inline: true},
		{Name: "Category", Value: category, Inline: true},
		{Name: "Limit", Value: limit, Inline: true},
		{Name: "Featured", Value: map[bool]string{true: "⭐ Yes", false: "No"}[it.Featured], Inline: true},
		{Name: "Delivery", Value: delivery},
	}
	switch {
	case it.Automatic:
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Note", Value: "Delivered automatically, so it's sold on the website shop only."})
	case it.DropOff && it.NeedsMap:
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Can't be bought yet", Value: "Set the delivery map on the Champion website (Shop › Delivery) so buyers can give coordinates."})
	}
	id := strconv.FormatInt(it.ID, 10)
	flag := func(on bool) string {
		if on {
			return "0"
		}
		return "1"
	}
	power := discordgo.Button{Label: "Turn off", Emoji: &discordgo.ComponentEmoji{Name: "⚫"}, Style: discordgo.DangerButton, CustomID: traderID("itemactive", hub.InstallationID, id, flag(it.Active))}
	if !it.Active {
		power = discordgo.Button{Label: "Turn on", Emoji: &discordgo.ComponentEmoji{Name: "🟢"}, Style: discordgo.SuccessButton, CustomID: traderID("itemactive", hub.InstallationID, id, flag(it.Active))}
	}
	feature := "Feature"
	if it.Featured {
		feature = "Unfeature"
	}
	drop := "Drop-off: off"
	if it.DropOff {
		drop = "Drop-off: on"
	}
	rows := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Edit", Emoji: &discordgo.ComponentEmoji{Name: "✏️"}, Style: discordgo.PrimaryButton, CustomID: traderFormID("edititem", hub.InstallationID, id)},
			discordgo.Button{Label: "Stock", Emoji: &discordgo.ComponentEmoji{Name: "📦"}, Style: discordgo.PrimaryButton, CustomID: traderFormID("stockitem", hub.InstallationID, id)},
			power,
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: feature, Emoji: &discordgo.ComponentEmoji{Name: "⭐"}, Style: discordgo.SecondaryButton, CustomID: traderID("itemfeat", hub.InstallationID, id, flag(it.Featured))},
			discordgo.Button{Label: drop, Emoji: &discordgo.ComponentEmoji{Name: "📍"}, Style: discordgo.SecondaryButton, CustomID: traderID("itemdrop", hub.InstallationID, id, flag(it.DropOff))},
			discordgo.Button{Label: "Items", Emoji: &discordgo.ComponentEmoji{Name: "⬅️"}, Style: discordgo.SecondaryButton, CustomID: traderID("items", hub.InstallationID, strconv.FormatInt(it.CategoryID, 10))},
		}},
	}
	return traderPrivate(embed, rows...)
}

// BuildTraderAddItemForm asks for a new item.
func BuildTraderAddItemForm(installationID int64) *discordgo.InteractionResponseData {
	return &discordgo.InteractionResponseData{CustomID: traderModalID("additem", installationID), Title: "Add an item",
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldName, Label: "Name", Style: discordgo.TextInputShort, Required: true, MaxLength: 80, Placeholder: "e.g. M4-A1 Kit"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldPrice, Label: "Price (Champion Points)", Style: discordgo.TextInputShort, Required: true, MaxLength: 13, Placeholder: "1500"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldStock, Label: "Stock (empty = unlimited)", Style: discordgo.TextInputShort, MaxLength: 13, Placeholder: "10"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldCategory, Label: "Category (new names are created)", Style: discordgo.TextInputShort, MaxLength: 60, Placeholder: "Weapons"}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldDesc, Label: "Description (optional)", Style: discordgo.TextInputParagraph, MaxLength: 1000}),
		}}
}

// BuildTraderEditItemForm changes an item's details, filled in with the current ones.
func BuildTraderEditItemForm(installationID int64, it TraderItem) *discordgo.InteractionResponseData {
	limit := ""
	if it.Limit != nil {
		limit = strconv.Itoa(*it.Limit)
	}
	return &discordgo.InteractionResponseData{CustomID: traderModalID("edititem", installationID, strconv.FormatInt(it.ID, 10)), Title: "Edit item",
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldName, Label: "Name", Style: discordgo.TextInputShort, Required: true, MaxLength: 80, Value: it.Name}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldPrice, Label: "Price (Champion Points)", Style: discordgo.TextInputShort, Required: true, MaxLength: 13, Value: strconv.FormatInt(it.Price, 10)}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldCategory, Label: "Category (empty = none)", Style: discordgo.TextInputShort, MaxLength: 60, Value: it.CategoryName}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldLimit, Label: "Limit per player (empty = none)", Style: discordgo.TextInputShort, MaxLength: 4, Value: limit}),
			traderTextRow(discordgo.TextInput{CustomID: traderFieldDesc, Label: "Description", Style: discordgo.TextInputParagraph, MaxLength: 1000, Value: it.Description}),
		}}
}

// BuildTraderStockForm sets an item's stock.
func BuildTraderStockForm(installationID int64, it TraderItem) *discordgo.InteractionResponseData {
	value := ""
	if it.Stock != nil {
		value = strconv.FormatInt(*it.Stock, 10)
	}
	return &discordgo.InteractionResponseData{CustomID: traderModalID("stockitem", installationID, strconv.FormatInt(it.ID, 10)), Title: "Stock",
		Components: []discordgo.MessageComponent{
			traderTextRow(discordgo.TextInput{CustomID: traderFieldStock, Label: "How many are left (empty = unlimited)", Style: discordgo.TextInputShort, MaxLength: 13, Value: value, Placeholder: "25"}),
		}}
}
