package discord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestTraderHoursParse(t *testing.T) {
	for _, c := range []struct{ open, close, tz, wantOpen, wantClose, wantTZ string }{
		{"18:00", "02:00", "Europe/London", "18:00", "02:00", "Europe/London"},
		{"9", "17.30", "", "09:00", "17:30", "UTC"},
		{" 08:05 ", "20:00", "gmt", "08:05", "20:00", "UTC"},
		{"", "", "Europe/London", "", "", ""},
	} {
		o, cl, tz, err := ParseTraderHours(c.open, c.close, c.tz)
		if err != nil || o != c.wantOpen || cl != c.wantClose || tz != c.wantTZ {
			t.Errorf("%q %q %q: %q %q %q %v", c.open, c.close, c.tz, o, cl, tz, err)
		}
	}
	for _, bad := range [][3]string{{"18:00", "", "UTC"}, {"25:00", "02:00", "UTC"}, {"18:00", "18:00", "UTC"}, {"18:00", "02:00", "Mars/Olympus"}, {"6pm", "2am", "UTC"}, {"18:5", "02:00", "UTC"}} {
		if _, _, _, err := ParseTraderHours(bad[0], bad[1], bad[2]); !errors.Is(err, ErrTraderHoursBad) {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestTraderHoursState(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, london) }
	// Open overnight: 18:00 to 02:00.
	for _, c := range []struct {
		now      time.Time
		open     bool
		boundary time.Time
	}{
		{at(20, 19, 0), true, at(20, 18, 0)},
		{at(21, 1, 30), true, at(20, 18, 0)},
		{at(21, 2, 0), false, at(21, 2, 0)},
		{at(21, 12, 0), false, at(21, 2, 0)},
		{at(21, 17, 59), false, at(21, 2, 0)},
	} {
		open, boundary, ok := TraderHoursState("18:00", "02:00", "Europe/London", c.now.UTC())
		if !ok || open != c.open || !boundary.Equal(c.boundary) {
			t.Errorf("overnight at %v: open=%v at %v", c.now, open, boundary)
		}
	}
	// Open in the day: 09:00 to 17:00, early morning is closed by yesterday's closing.
	open, boundary, ok := TraderHoursState("09:00", "17:00", "Europe/London", at(21, 7, 0))
	if !ok || open || !boundary.Equal(at(20, 17, 0)) {
		t.Fatalf("day hours early: %v %v", open, boundary)
	}
	if _, _, ok := TraderHoursState("09:00", "09:00", "UTC", time.Now()); ok {
		t.Fatal("equal times are not hours")
	}
}

func TestTraderPricesShowTheDeal(t *testing.T) {
	ends := time.Now().Add(time.Hour)
	p := TraderProduct{ID: 9, Name: "M4-A1 Kit", Price: 1350, ListPrice: 1800, PercentOff: 25, OfferEnds: &ends, Stock: TraderStockIn}
	if got := traderPriceText(p); got != "~~1,800~~ **1,350 pts** (−25%)" {
		t.Fatalf("price text: %q", got)
	}
	if got := traderPricePlain(p); got != "1,350 pts (25% off)" {
		t.Fatalf("plain price: %q", got)
	}
	hub := TraderHub{InstallationID: 7, Open: true, DealsRoleID: "deals"}
	item := BuildTraderItem(hub, p, 5000, "")
	found := false
	for _, f := range item.Embeds[0].Fields {
		found = found || f.Name == "🏷️ Deal" && strings.Contains(f.Value, "25% off")
	}
	if !found || traderButtons(t, item)["Buy for 1,350 pts"].CustomID == "" {
		t.Fatalf("item on a deal: %+v", item.Embeds[0].Fields)
	}
	offer := TraderOffer{ID: 3, Kind: TraderOfferDeal, ProductID: 9, Name: "M4-A1 Kit", PercentOff: 25, EndsAt: ends}
	post := BuildTraderPost(hub, TraderCatalog{Featured: []TraderProduct{p}, Offers: []TraderOffer{offer}})
	text := ""
	for _, f := range post.Embeds[0].Fields {
		text += f.Name + "=" + f.Value + "\n"
	}
	if !strings.Contains(text, "~~1,800~~") || !strings.Contains(text, "On sale now=🏷️ **25% off** M4-A1 Kit") || traderButtons(t, post)["Deal pings"].CustomID != "trader:p:pings:7" {
		t.Fatalf("post with a deal: %s", text)
	}
}

func TestTraderOfferPostPingsDealsOnly(t *testing.T) {
	ends := time.Now().Add(24 * time.Hour)
	hub := TraderHub{InstallationID: 7, ServerName: "Day & Nite", DealsRoleID: "deals"}
	p := TraderProduct{ID: 9, Name: "M4-A1 Kit", Price: 1350, ListPrice: 1800, PercentOff: 25}
	deal := BuildTraderOfferPost(hub, TraderOffer{ID: 3, Kind: TraderOfferDeal, ProductID: 9, Name: "M4-A1 Kit", PercentOff: 25, EndsAt: ends, Title: "Weekend raid"}, &p)
	if deal.Content != "<@&deals>" || deal.AllowedMentions == nil || len(deal.AllowedMentions.Roles) != 1 || deal.AllowedMentions.Roles[0] != "deals" || len(deal.AllowedMentions.Parse) != 0 {
		t.Fatalf("deal ping: %q %+v", deal.Content, deal.AllowedMentions)
	}
	if e := deal.Embeds[0]; e.Title != "🏷️ Deal: 25% off M4-A1 Kit" || e.Description != "Weekend raid" || e.Fields[0].Value != "~~1,800~~ **1,350 pts** (−25%)" {
		t.Fatalf("deal post: %+v %+v", e, e.Fields)
	}
	b := traderButtons(t, &discordgo.InteractionResponseData{Components: deal.Components})
	if b["View in trader"].CustomID != "trader:p:item:7:9" || b["Deal pings"].CustomID != "trader:p:pings:7" {
		t.Fatalf("deal buttons: %+v", b)
	}
	sale := BuildTraderOfferPost(TraderHub{InstallationID: 7}, TraderOffer{ID: 4, Kind: TraderOfferSale, CategoryID: 2, Name: "Weapons", PercentOff: 10, EndsAt: ends}, nil)
	if sale.Content != "" || sale.Embeds[0].Title != "🔥 Sale: 10% off Weapons" || len(sale.AllowedMentions.Parse) != 0 || len(sale.AllowedMentions.Roles) != 0 {
		t.Fatalf("sale without a deals role pings nobody: %+v %+v", sale, sale.AllowedMentions)
	}
	if b := traderButtons(t, &discordgo.InteractionResponseData{Components: sale.Components}); b["Shop the sale"].CustomID != "trader:p:cat:7:2" {
		t.Fatalf("sale button: %+v", b)
	}
	all := BuildTraderOfferEnded(TraderOffer{Kind: TraderOfferSale, PercentOff: 20})
	if all.Embeds[0].Title != "⌛ Sale ended: 20% off everything" || len(all.Components) != 0 {
		t.Fatalf("ended: %+v", all.Embeds[0])
	}
}

func TestTraderOfferFormsAndList(t *testing.T) {
	deal := BuildTraderOfferForm(TraderOfferDeal, 7, 9)
	sale := BuildTraderOfferForm(TraderOfferSale, 7, 0)
	if deal.CustomID != "trader:f:deal:7:9" || sale.CustomID != "trader:f:sale:7:0" || len(deal.Components) != 3 {
		t.Fatalf("offer forms: %q %q", deal.CustomID, sale.CustomID)
	}
	if f := BuildTraderRefundForm(7, 1043); f.CustomID != "trader:f:refund:7:1043" {
		t.Fatalf("refund form: %q", f.CustomID)
	}
	hours := BuildTraderHoursForm(TraderHub{InstallationID: 7, HoursOpen: "18:00", HoursClose: "02:00", HoursTZ: "Europe/London"})
	if hours.CustomID != "trader:f:hours:7" || hours.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value != "18:00" {
		t.Fatalf("hours form: %+v", hours)
	}
	for _, c := range []struct {
		in   string
		unit string
		want int
	}{{"25", "%", 25}, {" 25% ", "%", 25}, {"48h", "h", 48}, {"48 H", "h", 48}} {
		if got, err := parseTraderWhole(c.in, c.unit); err != nil || got != c.want {
			t.Errorf("%q: %d %v", c.in, got, err)
		}
	}
	if _, err := parseTraderWhole("a lot", "%"); err == nil {
		t.Fatal("words are not numbers")
	}
	ends := time.Now().Add(time.Hour)
	list := BuildTraderOfferList(TraderHub{InstallationID: 7}, []TraderOffer{{ID: 3, Kind: TraderOfferDeal, Name: "SVD", PercentOff: 30, EndsAt: ends},
		{ID: 4, Kind: TraderOfferSale, PercentOff: 10, EndsAt: ends}}, "Ended: Olga.")
	menus := traderMenus(list)
	if len(menus) != 1 || menus[0].CustomID != "trader:u:endoffer:7" || len(menus[0].Options) != 2 || menus[0].Options[1].Label != "Sale: 10% off everything" ||
		traderButtons(t, list)["End all now"].CustomID != "trader:u:endall:7" || !strings.HasPrefix(list.Embeds[0].Description, "Ended: Olga.") {
		t.Fatalf("offer list: %+v %+v", list.Embeds[0], menus)
	}
	if empty := BuildTraderOfferList(TraderHub{}, nil, ""); len(empty.Components) != 0 {
		t.Fatal("nothing to end shows no menu")
	}
}

func TestTraderOrderCardDoneKeepsTheOrder(t *testing.T) {
	card := &discordgo.Message{Embeds: []*discordgo.MessageEmbed{{Title: "🛒 New order #1043", Fields: []*discordgo.MessageEmbedField{{Name: "Player", Value: "Rusty"}}}}}
	now := time.Now()
	done := traderOrderCardDone(card, TraderHub{}, TraderOrder{ID: 1043, Status: TraderOrderDelivered}, "42", now)
	e := done.Embeds[0]
	if e.Title != "✅ Order #1043 delivered" || len(e.Fields) != 2 || !strings.HasPrefix(e.Fields[1].Value, "Delivered by <@42>") || len(done.Components) != 0 {
		t.Fatalf("delivered card: %+v %+v", e, e.Fields)
	}
	if len(card.Embeds[0].Fields) != 1 {
		t.Fatal("the original card was changed in place")
	}
	refunded := traderOrderCardDone(card, TraderHub{}, TraderOrder{ID: 1043, Status: TraderOrderRefunded, Total: 1800}, "42", now)
	if e := refunded.Embeds[0]; e.Title != "↩️ Order #1043 refunded" || e.Fields[2].Value != "1,800 pts given back" {
		t.Fatalf("refunded card: %+v %+v", e, e.Fields)
	}
}

func TestTraderDealPingsToggle(t *testing.T) {
	api := &traderAPIFake{}
	h := NewTraderHandler(&traderBackendFake{})
	h.apiFor = func(*discordgo.Session) TraderGuildAPI { return api }
	hub := TraderHub{InstallationID: 7, DealsRoleID: "deals"}
	caller := TraderCaller{PanelCaller: PanelCaller{DiscordGuildID: "g", DiscordUserID: "555"}}
	h.togglePings(nil, nil, caller, hub)
	caller.RoleIDs = []string{"deals"}
	h.togglePings(nil, nil, caller, hub)
	if len(api.memberRoles) != 2 || api.memberRoles[0] != "+555=deals" || api.memberRoles[1] != "-555=deals" {
		t.Fatalf("pings: %v", api.memberRoles)
	}
	h.togglePings(nil, nil, caller, TraderHub{InstallationID: 7})
	if len(api.memberRoles) != 2 {
		t.Fatal("no deals role changes nothing")
	}
}

func TestTraderRefreshEndsPostsAndAppliesHours(t *testing.T) {
	api := &traderAPIFake{}
	hub := TraderHub{InstallationID: 7, DiscordGuildID: "g", ServerName: "Day & Nite", TraderChannelID: "t", PanelChannelID: "p", VoiceChannelID: "v",
		Open: true, VoiceNameApplied: TraderVoiceOpen, HoursOpen: "18:00", HoursClose: "02:00", HoursTZ: "UTC"}
	closed := hub
	closed.Open, closed.ServerName = false, ""
	backend := &traderBackendFake{hubs: []TraderHub{hub}, hoursHub: &closed,
		toEnd: []TraderOffer{{ID: 3, Kind: TraderOfferDeal, Name: "SVD", PercentOff: 30, PostChannelID: "deals", PostMessageID: "m1"}}}
	h := NewTraderHandler(backend)
	h.refreshAll(context.Background(), api)
	if len(backend.postsEnded) != 1 || backend.postsEnded[0] != 3 || len(api.edited) != 1 || api.edited[0] != "m1" {
		t.Fatalf("ended posts: %v edited %v", backend.postsEnded, api.edited)
	}
	if len(api.renamed) != 1 || api.renamed[0] != TraderVoiceClosed || backend.voice[7] != TraderVoiceClosed {
		t.Fatalf("hours closed the trader: renamed %v", api.renamed)
	}
	post := api.sent["t"][0].Embeds[0]
	if !strings.Contains(post.Description, "🔴 **Closed**") || !strings.Contains(post.Description, "18:00–02:00 (UTC)") || !strings.Contains(post.Footer.Text, "Day & Nite") {
		t.Fatalf("post after hours: %+v", post)
	}
}

// Discord refuses a form whose title or a field label is over 45 characters, or whose custom id is
// over 100.
func TestTraderFormsFitDiscordLimits(t *testing.T) {
	forms := []*discordgo.InteractionResponseData{BuildTraderCloseForm(1 << 40), BuildTraderAnnounceForm(1 << 40), BuildTraderRefundForm(1<<40, 1<<40),
		BuildTraderHoursForm(TraderHub{InstallationID: 1 << 40}), BuildTraderOfferForm(TraderOfferDeal, 1<<40, 1<<40), BuildTraderOfferForm(TraderOfferSale, 1<<40, 1<<40),
		BuildTraderCoordsForm(1<<40, 1<<40, 5, strings.Repeat("a", 16), "Chernarus")}
	for _, f := range forms {
		if n := len([]rune(f.Title)); n > 45 || len(f.CustomID) > 100 {
			t.Errorf("form %q (%d) id %q", f.Title, n, f.CustomID)
		}
		for _, row := range f.Components {
			for _, c := range row.(discordgo.ActionsRow).Components {
				if in := c.(discordgo.TextInput); len([]rune(in.Label)) > 45 || len([]rune(in.Placeholder)) > 100 {
					t.Errorf("form %q field %q", f.Title, in.Label)
				}
			}
		}
	}
}

func TestTraderItemScreens(t *testing.T) {
	hub := TraderHub{InstallationID: 7}
	stock := int64(3)
	limit := 2
	items := []TraderItem{{ID: 9, Name: "M4-A1 Kit", Price: 1800, Active: true, Stock: &stock, CategoryID: 2, CategoryName: "Weapons"},
		{ID: 10, Name: "Olga 24", Price: 25000, Active: false}}
	list := BuildTraderItems(hub, []TraderCategory{{ID: 2, Name: "Weapons", Count: 1}}, 2, items, "")
	menus := traderMenus(list)
	if len(menus) != 2 || menus[0].CustomID != "trader:u:items:7" || !menus[0].Options[1].Default || menus[1].CustomID != "trader:u:itemedit:7" ||
		menus[1].Options[1].Description != "Off · 25,000 pts · Unlimited" || traderButtons(t, list)["Add item"].CustomID != "trader:m:additem:7" {
		t.Fatalf("item list: %+v", menus)
	}
	if !strings.Contains(list.Embeds[0].Description, "🟢 **M4-A1 Kit** · 1,800 pts · 3 left") || !strings.Contains(list.Embeds[0].Description, "⚫ **Olga 24**") {
		t.Fatalf("item list text: %s", list.Embeds[0].Description)
	}
	it := items[0]
	it.Limit, it.DropOff, it.NeedsMap = &limit, true, true
	card := BuildTraderItemManage(hub, it, "✅ Saved.")
	b := traderButtons(t, card)
	if b["Edit"].CustomID != "trader:m:edititem:7:9" || b["Stock"].CustomID != "trader:m:stockitem:7:9" || b["Turn off"].CustomID != "trader:u:itemactive:7:9:0" ||
		b["Feature"].CustomID != "trader:u:itemfeat:7:9:1" || b["Drop-off: on"].CustomID != "trader:u:itemdrop:7:9:0" || b["Items"].CustomID != "trader:u:items:7:2" {
		t.Fatalf("item card buttons: %+v", b)
	}
	text := card.Embeds[0].Description
	for _, f := range card.Embeds[0].Fields {
		text += "\n" + f.Name + "=" + f.Value
	}
	if !strings.Contains(text, "✅ Saved.") || !strings.Contains(text, "Limit=2 per player") || !strings.Contains(text, "Can't be bought yet") {
		t.Fatalf("item card: %s", text)
	}
	if b := traderButtons(t, BuildTraderItemManage(hub, items[1], "")); b["Turn on"].CustomID != "trader:u:itemactive:7:10:1" {
		t.Fatalf("an item that's off: %+v", b)
	}
	edit := BuildTraderEditItemForm(7, it)
	if edit.CustomID != "trader:f:edititem:7:9" || edit.Components[1].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value != "1800" ||
		edit.Components[3].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value != "2" {
		t.Fatalf("edit form: %+v", edit)
	}
	if f := BuildTraderStockForm(7, it); f.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value != "3" {
		t.Fatalf("stock form: %+v", f)
	}
	if f := BuildTraderStockForm(7, items[1]); f.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput).Value != "" {
		t.Fatal("unlimited stock is an empty field")
	}
	for _, f := range []*discordgo.InteractionResponseData{BuildTraderAddItemForm(1 << 40), BuildTraderEditItemForm(1<<40, TraderItem{ID: 1 << 40}), BuildTraderStockForm(1<<40, TraderItem{ID: 1 << 40})} {
		if len([]rune(f.Title)) > 45 || len(f.CustomID) > 100 || len(f.Components) > 5 {
			t.Errorf("form %q", f.Title)
		}
		for _, row := range f.Components {
			if in := row.(discordgo.ActionsRow).Components[0].(discordgo.TextInput); len([]rune(in.Label)) > 45 {
				t.Errorf("label %q", in.Label)
			}
		}
	}
	panel := traderButtons(t, BuildTraderPanel(hub, TraderCatalog{}, time.Now()))
	if panel["Items"].CustomID != "trader:p:items:7" {
		t.Fatalf("panel items button: %+v", panel)
	}
}

func TestTraderItemParsing(t *testing.T) {
	for in, want := range map[string]int64{"1500": 1500, "1,500": 1500, " 1500 pts": 1500, "2 500 points": 2500} {
		if got, err := parseTraderPoints(in); err != nil || got != want {
			t.Errorf("price %q: %d %v", in, got, err)
		}
	}
	if _, err := parseTraderPoints("lots"); err == nil {
		t.Fatal("words are not a price")
	}
	for in, want := range map[string]int64{"25": 25, "1,000": 1000, "0": 0} {
		if got, err := parseTraderStock(in); err != nil || got == nil || *got != want {
			t.Errorf("stock %q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", " ", "Unlimited", "∞"} {
		if got, err := parseTraderStock(in); err != nil || got != nil {
			t.Errorf("unlimited %q: %v %v", in, got, err)
		}
	}
	if _, err := parseTraderStock("some"); err == nil {
		t.Fatal("words are not stock")
	}
	if !errors.As(error(&TraderItemInvalid{Reason: "x"}), new(*TraderItemInvalid)) {
		t.Fatal("an item refusal is matched by type")
	}
}
