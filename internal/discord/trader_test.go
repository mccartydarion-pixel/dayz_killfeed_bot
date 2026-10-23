package discord

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

func traderButtons(t *testing.T, data *discordgo.InteractionResponseData) map[string]discordgo.Button {
	t.Helper()
	out := map[string]discordgo.Button{}
	for _, c := range data.Components {
		for _, inner := range c.(discordgo.ActionsRow).Components {
			if b, ok := inner.(discordgo.Button); ok {
				out[b.Label] = b
			}
		}
	}
	return out
}

func traderMenus(data *discordgo.InteractionResponseData) []discordgo.SelectMenu {
	var out []discordgo.SelectMenu
	for _, c := range data.Components {
		for _, inner := range c.(discordgo.ActionsRow).Components {
			if m, ok := inner.(discordgo.SelectMenu); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func TestTraderPostFollowsOpenState(t *testing.T) {
	hub := TraderHub{InstallationID: 7, ServerName: "Day & Nite", Open: true}
	cat := TraderCatalog{Categories: []TraderCategory{{ID: 1, Name: "Weapons", Count: 4}},
		Featured: []TraderProduct{{ID: 9, Name: "M4-A1 Kit", Price: 1800, Stock: TraderStockLow}}}
	open := BuildTraderPost(hub, cat)
	e := open.Embeds[0]
	if e.Color != presentation.Gold || !strings.Contains(e.Description, "🟢 **Open**") || !strings.Contains(e.Fields[0].Value, "**M4-A1 Kit** · 1,800 pts · Low stock") ||
		!strings.Contains(e.Fields[1].Value, "Weapons (4)") {
		t.Fatalf("open post: %+v %+v", e, e.Fields)
	}
	b := traderButtons(t, open)
	if b["Browse"].Disabled || b["Browse"].CustomID != "trader:p:browse:7" || b["My wallet"].CustomID != "trader:p:wallet:7" || b["My orders"].CustomID != "trader:p:orders:7" {
		t.Fatalf("open buttons: %+v", b)
	}
	hub.Open = false
	closed := BuildTraderPost(hub, cat)
	if closed.Embeds[0].Color != presentation.Red || !strings.Contains(closed.Embeds[0].Description, "🔴 **Closed**") || !traderButtons(t, closed)["Browse"].Disabled {
		t.Fatalf("closed post: %+v", closed.Embeds[0])
	}
	if traderButtons(t, closed)["My orders"].Disabled {
		t.Fatal("orders stay reachable while closed")
	}
}

func TestTraderPanelShowsStateAndAlerts(t *testing.T) {
	changed := time.Now().Add(-time.Minute)
	hub := TraderHub{InstallationID: 7, Open: true, VoiceChannelID: "v", VoiceNameApplied: TraderVoiceClosed, OrdersChannelID: "o",
		OpenChangedAt: &changed, OpenChangedByDiscordID: "42"}
	cat := TraderCatalog{OrdersWaiting: 3, StockAlerts: []TraderProduct{{Name: "SVD", Stock: TraderStockOut}}}
	data := BuildTraderPanel(hub, cat, time.Now())
	text := ""
	for _, f := range data.Embeds[0].Fields {
		text += f.Name + "=" + f.Value + "\n"
	}
	if !strings.Contains(text, "Trader=🟢 Open") || !strings.Contains(text, "Orders to hand over=3") || !strings.Contains(text, "Renaming") ||
		!strings.Contains(text, "SVD · Sold out") || !strings.Contains(text, "Opened by <@42>") {
		t.Fatalf("panel fields: %s", text)
	}
	if b := traderButtons(t, data); b["Close trader"].CustomID != "trader:u:close:7" || b["↻ Refresh"].CustomID != "trader:u:panel:7" {
		t.Fatalf("panel buttons: %+v", b)
	}
	hub.Open = false
	hub.VoiceNameApplied = TraderVoiceClosed
	closed := BuildTraderPanel(hub, TraderCatalog{}, time.Now())
	if b := traderButtons(t, closed); b["Open trader"].CustomID != "trader:u:open:7" {
		t.Fatalf("closed panel: %+v", b)
	}
	for _, f := range closed.Embeds[0].Fields {
		if strings.Contains(f.Value, "Renaming") {
			t.Fatal("a voice channel already renamed shows no renaming note")
		}
	}
}

func TestTraderItemExplainsWhyItCantBeBought(t *testing.T) {
	hub := TraderHub{InstallationID: 7, Open: true}
	limit := 2
	p := TraderProduct{ID: 12, Name: "Olga 24", Price: 2500, Stock: TraderStockIn, Limit: &limit}
	ok := BuildTraderItem(hub, p, 6000, "Chernarus")
	if b := traderButtons(t, ok)["Buy for 2,500 pts"]; b.Disabled || b.CustomID != "trader:u:buy:7:12" {
		t.Fatalf("buy button: %+v", b)
	}
	menus := traderMenus(ok)
	if len(menus) != 1 || menus[0].CustomID != "trader:u:qty:7:12" || len(menus[0].Options) != 1 || menus[0].Options[0].Value != "2" {
		t.Fatalf("a limit of 2 offers only 2 in the quantity menu: %+v", menus)
	}
	cases := map[string]struct {
		p       TraderProduct
		balance int64
		mapName string
		why     string
	}{
		"sold out":  {TraderProduct{ID: 1, Name: "A", Price: 10, Stock: TraderStockOut}, 100, "", "Sold out"},
		"automatic": {TraderProduct{ID: 1, Name: "A", Price: 10, Stock: TraderStockIn, Automatic: true}, 100, "", "website shop only"},
		"no map":    {TraderProduct{ID: 1, Name: "A", Price: 10, Stock: TraderStockIn, Coordinates: true}, 100, "", "map for drop-off"},
		"too poor":  {TraderProduct{ID: 1, Name: "A", Price: 300, Stock: TraderStockIn}, 100, "", "You need 200 pts more"},
	}
	for name, c := range cases {
		data := BuildTraderItem(hub, c.p, c.balance, c.mapName)
		var why string
		for _, f := range data.Embeds[0].Fields {
			if f.Name == "Can't buy" {
				why = f.Value
			}
		}
		buy := traderButtons(t, data)["Buy for "+presentation.FormatPoints(c.p.Price)]
		if !strings.Contains(why, c.why) || !buy.Disabled || len(traderMenus(data)) != 0 {
			t.Errorf("%s: %q disabled=%v", name, why, buy.Disabled)
		}
	}
}

func TestTraderConfirmCarriesOneOrderKey(t *testing.T) {
	hub := TraderHub{InstallationID: 7}
	nonce := traderNonce()
	pickup := BuildTraderConfirm(hub, TraderProduct{ID: 12, Name: "AKM", Price: 1500}, 2, 5000, nonce)
	pay := traderButtons(t, pickup)["Confirm purchase"]
	if pay.CustomID != "trader:u:pay:7:12:2:"+nonce || len(pay.CustomID) > 100 || !strings.Contains(pickup.Embeds[0].Fields[1].Value, "3,000 pts") {
		t.Fatalf("pickup confirm: %+v", pay)
	}
	drop := BuildTraderConfirm(hub, TraderProduct{ID: 12, Name: "Tent", Price: 100, Coordinates: true}, 1, 5000, nonce)
	if where := traderButtons(t, drop)["Enter drop point"]; !strings.HasPrefix(where.CustomID, TraderFormPrefix+"where:7:12:1:") {
		t.Fatalf("drop-off confirm: %+v", where)
	}
	if b := traderButtons(t, BuildTraderConfirm(hub, TraderProduct{ID: 12, Name: "AKM", Price: 1500}, 2, 1000, nonce))["Confirm purchase"]; !b.Disabled {
		t.Fatal("a confirm the balance can't cover is disabled")
	}
	form := BuildTraderCoordsForm(7, 12, 1, nonce, "Chernarus")
	if form.CustomID != "trader:coords:7:12:1:"+nonce || len(form.Components) != 2 {
		t.Fatalf("form: %+v", form)
	}
	productID, qty, got, ok := parseTraderOrderArgs([]string{"12", "2", nonce})
	if !ok || productID != 12 || qty != 2 || got != nonce || traderOrderKey(got) != "discord-trader."+nonce {
		t.Fatalf("parse: %v %v %v %v", productID, qty, got, ok)
	}
	for _, bad := range [][]string{{"12", "6", nonce}, {"12", "0", nonce}, {"x", "1", nonce}, {"12", "1", "NOT-HEX!"}, {"12", "1"}} {
		if _, _, _, ok := parseTraderOrderArgs(bad); ok {
			t.Errorf("accepted %v", bad)
		}
	}
	if v, err := parseTraderCoordinate(" 10,318 "); err != nil || v != 10318 {
		t.Fatalf("coordinate: %v %v", v, err)
	}
	if _, err := parseTraderCoordinate("north"); err == nil {
		t.Fatal("a word is not a coordinate")
	}
}

// traderAPIFake records what setup and the shared messages did.
type traderAPIFake struct {
	channels    []*discordgo.Channel
	roles       []*discordgo.Role
	created     []discordgo.GuildChannelCreateData
	rolesMade   int
	sent        map[string][]*discordgo.MessageSend
	edited      []string
	deleted     map[string]bool
	renamed     []string
	renameLimit bool
	nextID      int
	forbidden   bool
	permSet     []string
	permDeleted []string
}

func (f *traderAPIFake) id() string {
	f.nextID++
	return "id" + strconv.Itoa(f.nextID)
}
func (f *traderAPIFake) BotID() string { return "bot" }
func (f *traderAPIFake) GuildChannels(string) ([]*discordgo.Channel, error) {
	return f.channels, nil
}
func (f *traderAPIFake) GuildRoles(string) ([]*discordgo.Role, error) { return f.roles, nil }
func (f *traderAPIFake) GuildRoleCreate(_ string, data *discordgo.RoleParams) (*discordgo.Role, error) {
	f.rolesMade++
	r := &discordgo.Role{ID: f.id(), Name: data.Name}
	f.roles = append(f.roles, r)
	return r, nil
}
func (f *traderAPIFake) GuildChannelCreateComplex(_ string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	if f.forbidden {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}
	}
	f.created = append(f.created, data)
	c := &discordgo.Channel{ID: f.id(), Name: data.Name, Type: data.Type}
	f.channels = append(f.channels, c)
	return c, nil
}
func (f *traderAPIFake) ChannelRename(channelID, name string) (*discordgo.Channel, error) {
	if f.renameLimit {
		return nil, &discordgo.RateLimitError{RateLimit: &discordgo.RateLimit{TooManyRequests: &discordgo.TooManyRequests{}}}
	}
	f.renamed = append(f.renamed, name)
	return &discordgo.Channel{ID: channelID, Name: name}, nil
}
func (f *traderAPIFake) ChannelPermissionSet(channelID, targetID string, _ discordgo.PermissionOverwriteType, allow, _ int64) error {
	f.permSet = append(f.permSet, channelID+"="+targetID)
	return nil
}
func (f *traderAPIFake) ChannelPermissionDelete(channelID, targetID string) error {
	f.permDeleted = append(f.permDeleted, channelID+"="+targetID)
	return nil
}
func (f *traderAPIFake) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	if f.sent == nil {
		f.sent = map[string][]*discordgo.MessageSend{}
	}
	f.sent[channelID] = append(f.sent[channelID], data)
	return &discordgo.Message{ID: f.id(), ChannelID: channelID}, nil
}
func (f *traderAPIFake) ChannelMessageEditComplex(m *discordgo.MessageEdit) (*discordgo.Message, error) {
	if f.deleted[m.ID] {
		return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	f.edited = append(f.edited, m.ID)
	return &discordgo.Message{ID: m.ID}, nil
}

func TestTraderSetupCreatesAndReusesChannels(t *testing.T) {
	api := &traderAPIFake{}
	h := NewTraderHandler(nil)
	hub := TraderHub{InstallationID: 7, DiscordGuildID: "g", Open: true}
	made, err := h.ensureChannels(api, &hub, "")
	if err != nil {
		t.Fatal(err)
	}
	if api.rolesMade != 1 || len(api.created) != 6 || !made.voice || hub.TraderRoleID == "" || hub.PanelChannelID == "" || hub.OrdersChannelID == "" {
		t.Fatalf("first setup: %+v %+v", made, hub)
	}
	byName := map[string]discordgo.GuildChannelCreateData{}
	for _, c := range api.created {
		byName[c.Name] = c
	}
	voice := byName[TraderVoiceOpen]
	if voice.Type != discordgo.ChannelTypeGuildVoice || voice.ParentID != hub.CategoryID || voice.PermissionOverwrites[0].Deny&discordgo.PermissionVoiceConnect == 0 {
		t.Fatalf("voice channel: %+v", voice)
	}
	public := byName["trader"]
	if public.PermissionOverwrites[0].ID != "g" || public.PermissionOverwrites[0].Allow&discordgo.PermissionViewChannel == 0 || public.PermissionOverwrites[0].Deny&discordgo.PermissionSendMessages == 0 {
		t.Fatalf("#trader is readable and closed to posting: %+v", public.PermissionOverwrites[0])
	}
	panel := byName["trader-panel"]
	if panel.PermissionOverwrites[0].Deny&discordgo.PermissionViewChannel == 0 || panel.PermissionOverwrites[1].ID != hub.TraderRoleID ||
		panel.PermissionOverwrites[1].Allow&discordgo.PermissionViewChannel == 0 || panel.PermissionOverwrites[2].ID != "bot" {
		t.Fatalf("#trader-panel is the Trader role's: %+v", panel.PermissionOverwrites)
	}
	if orders := byName["trader-orders"]; orders.PermissionOverwrites[1].Allow&discordgo.PermissionSendMessages == 0 {
		t.Fatalf("the Trader role can talk in #trader-orders: %+v", orders.PermissionOverwrites)
	}

	// Run again: everything exists, nothing is created.
	before := len(api.created)
	made, err = h.ensureChannels(api, &hub, "")
	if err != nil || len(api.created) != before || api.rolesMade != 1 || made.voice || len(made.names) != 0 {
		t.Fatalf("second setup created again: %+v %v", made, err)
	}
	// A deleted channel is made again; a role given in the command is used as is.
	api.channels = api.channels[1:]
	made, err = h.ensureChannels(api, &hub, "chosen-role")
	if err != nil || len(api.created) != before+1 || hub.TraderRoleID != "chosen-role" || api.rolesMade != 1 {
		t.Fatalf("repair: %+v %+v %v", made, hub, err)
	}
	// The private channels move to the chosen role.
	if len(api.permSet) != 2 || api.permSet[0] != hub.PanelChannelID+"=chosen-role" || len(api.permDeleted) != 2 || !strings.HasSuffix(api.permDeleted[0], "=id1") {
		t.Fatalf("role change: set=%v deleted=%v", api.permSet, api.permDeleted)
	}
	if !strings.Contains(traderSetupFailedText(&discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}), "Manage Channels") {
		t.Fatal("a missing permission names it")
	}
}

// traderBackendFake serves the publish and order tests; unused methods panic through the embedded nil.
type traderBackendFake struct {
	TraderBackend
	hubs     []TraderHub
	cat      TraderCatalog
	messages map[int64][2]string
	voice    map[int64]string
	receipt  TraderReceipt
}

func (f *traderBackendFake) TraderHubs(context.Context) ([]TraderHub, error) { return f.hubs, nil }
func (f *traderBackendFake) TraderCatalog(context.Context, string, int64) (TraderCatalog, error) {
	return f.cat, nil
}
func (f *traderBackendFake) TraderSetMessages(_ context.Context, inst int64, post, panel string) error {
	if f.messages == nil {
		f.messages = map[int64][2]string{}
	}
	f.messages[inst] = [2]string{post, panel}
	return nil
}
func (f *traderBackendFake) TraderVoiceApplied(_ context.Context, inst int64, name string) error {
	if f.voice == nil {
		f.voice = map[int64]string{}
	}
	f.voice[inst] = name
	return nil
}
func (f *traderBackendFake) TraderBuy(context.Context, TraderCaller, int64, TraderBuy) (TraderReceipt, error) {
	return f.receipt, nil
}
func (f *traderBackendFake) TraderLevel(context.Context, PanelCaller, int64) (permissions.Level, error) {
	return permissions.LevelNone, nil
}

func TestTraderRefreshPostsMessagesAndRetriesTheVoiceName(t *testing.T) {
	api := &traderAPIFake{renameLimit: true}
	hub := TraderHub{InstallationID: 7, DiscordGuildID: "g", TraderChannelID: "t", PanelChannelID: "p", VoiceChannelID: "v", Open: false, VoiceNameApplied: TraderVoiceOpen}
	backend := &traderBackendFake{hubs: []TraderHub{hub}}
	h := NewTraderHandler(backend)
	h.refreshAll(context.Background(), api)
	if len(api.sent["t"]) != 1 || len(api.sent["p"]) != 1 || backend.messages[7][0] == "" || backend.messages[7][1] == "" {
		t.Fatalf("first refresh posts both: %+v %+v", api.sent, backend.messages)
	}
	if len(api.renamed) != 0 || backend.voice[7] != "" {
		t.Fatal("a rate-limited rename is not recorded as done")
	}
	// Next pass: the messages are edited in place, the rename goes through.
	hub.PostMessageID, hub.PanelMessageID = backend.messages[7][0], backend.messages[7][1]
	backend.hubs = []TraderHub{hub}
	api.renameLimit = false
	h.refreshAll(context.Background(), api)
	if len(api.edited) != 2 || len(api.sent["t"]) != 1 || backend.voice[7] != TraderVoiceClosed || api.renamed[0] != TraderVoiceClosed {
		t.Fatalf("second refresh: edited=%v renamed=%v voice=%v", api.edited, api.renamed, backend.voice)
	}
	// A deleted trader post is posted again.
	api.deleted = map[string]bool{hub.PostMessageID: true}
	hub.VoiceNameApplied = TraderVoiceClosed
	backend.hubs = []TraderHub{hub}
	h.refreshAll(context.Background(), api)
	if len(api.sent["t"]) != 2 || backend.messages[7][0] == hub.PostMessageID || len(api.renamed) != 1 {
		t.Fatalf("repost: %+v %v", backend.messages, api.renamed)
	}
}

func TestTraderOrderCardGoesToTheOrdersChannelOnce(t *testing.T) {
	api := &traderAPIFake{}
	backend := &traderBackendFake{receipt: TraderReceipt{OrderID: 1043, Item: "M4-A1 Kit", Quantity: 1, Total: 1800, Balance: 2450, Gamertag: "RustyAxe_77"}}
	h := NewTraderHandler(backend)
	h.apiFor = func(*discordgo.Session) TraderGuildAPI { return api }
	hub := TraderHub{InstallationID: 7, DiscordGuildID: "g", OrdersChannelID: "orders", TraderChannelID: "t", PanelChannelID: "p", Open: true}
	caller := TraderCaller{PanelCaller: PanelCaller{DiscordGuildID: "g", DiscordUserID: "555"}}
	h.place(context.Background(), nil, nil, caller, hub, TraderBuy{ProductID: 9, Quantity: 1, Key: traderOrderKey(traderNonce())})
	cards := api.sent["orders"]
	if len(cards) != 1 {
		t.Fatalf("order cards: %+v", api.sent)
	}
	e := cards[0].Embeds[0]
	if e.Title != "🛒 New order #1043" || !strings.Contains(e.Fields[0].Value, `RustyAxe\_77 (<@555>)`) || e.Fields[3].Value != "Handed over in game" ||
		cards[0].AllowedMentions == nil || len(cards[0].AllowedMentions.Parse) != 0 {
		t.Fatalf("order card: %+v %+v", e, e.Fields)
	}
	backend.receipt.Duplicate = true
	h.place(context.Background(), nil, nil, caller, hub, TraderBuy{ProductID: 9, Quantity: 1, Key: traderOrderKey(traderNonce())})
	if len(api.sent["orders"]) != 1 {
		t.Fatal("a repeated click posted a second order card")
	}
}

func TestTraderDropText(t *testing.T) {
	x, z := 4621.0, 10318.5
	if got := traderDropText(&x, &z, "Chernarus"); got != "Dropped at X 4,621 / Z 10,318.5 on Chernarus" {
		t.Fatalf("drop text: %q", got)
	}
	if errors.Is(ErrTraderClosed, ErrTraderForbidden) {
		t.Fatal("errors are distinct")
	}
}
