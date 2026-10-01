package discord

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestParseShopOrderCustomID(t *testing.T) {
	good := map[string]string{
		"champion:shoporder:received:12":   ShopOrderActionReceived,
		"champion:shoporder:issue:12":      ShopOrderActionIssue,
		"champion:shoporder:issuemodal:12": ShopOrderActionIssueModal,
	}
	for id, want := range good {
		action, purchase, ok := ParseShopOrderCustomID(id)
		if !ok || action != want || purchase != 12 || !IsShopOrderInteraction(id) {
			t.Fatalf("%s: action=%q purchase=%d ok=%v", id, action, purchase, ok)
		}
	}
	for _, id := range []string{"", "champion:faction:join:12", "champion:shoporder:received", "champion:shoporder:received:", "champion:shoporder:received:0",
		"champion:shoporder:received:-4", "champion:shoporder:received:12x", "champion:shoporder:refund:12", "champion:shoporder:received:12:13"} {
		if _, _, ok := ParseShopOrderCustomID(id); ok {
			t.Fatalf("%q was accepted", id)
		}
	}
}

// buttonIDs lists the custom ids (or URLs for link buttons) of a message's buttons.
func buttonIDs(t *testing.T, components []discordgo.MessageComponent) []string {
	t.Helper()
	var out []string
	for _, c := range components {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("component %T is not an action row", c)
		}
		for _, b := range row.Components {
			btn, ok := b.(discordgo.Button)
			if !ok {
				t.Fatalf("component %T is not a button", b)
			}
			if btn.Style == discordgo.LinkButton {
				out = append(out, btn.URL)
			} else {
				out = append(out, btn.CustomID)
			}
		}
	}
	return out
}

func TestDeliveredMessageOffersBothAnswers(t *testing.T) {
	deadline := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	n := repository.ShopOrderNotice{PurchaseID: 7, GuildName: "Champions", TotalPoints: 3, DeadlineAt: deadline,
		Items: []repository.ShopNoticeItem{{Name: "BandageDressing", Quantity: 2}, {Name: "Canteen", Quantity: 1}}}
	embed, components := BuildShopOrderDeliveredMessage(n, "https://example.test/")

	for _, want := range []string{"#7", "Champions", "2× BandageDressing", "1× Canteen", "Received order", "Issue with order"} {
		if !strings.Contains(embed.Description, want) {
			t.Fatalf("description lacks %q:\n%s", want, embed.Description)
		}
	}
	if embed.Fields[0].Value != "3 Champion Points" || !strings.Contains(embed.Fields[1].Value, "1791028800") {
		t.Fatalf("fields = %+v %+v", embed.Fields[0], embed.Fields[1])
	}
	got := buttonIDs(t, components)
	want := []string{"champion:shoporder:received:7", "champion:shoporder:issue:7", "https://example.test/dashboard/player/shop/purchases/7"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("buttons = %v, want %v", got, want)
	}
	// Without a site URL there is no link button, and many items are summarised.
	many := n
	many.Items = make([]repository.ShopNoticeItem, 14)
	embed, components = BuildShopOrderDeliveredMessage(many, "")
	if len(buttonIDs(t, components)) != 2 || !strings.Contains(embed.Description, "… and 4 more") {
		t.Fatalf("buttons=%v description=%s", buttonIDs(t, components), embed.Description)
	}
}

func TestStateMessageOffersOnlyWhatIsStillOpen(t *testing.T) {
	cases := []struct {
		state   string
		buttons []string
		says    string
	}{
		{repository.ConfirmationAwaitingBuyer, []string{"champion:shoporder:received:7", "champion:shoporder:issue:7"}, "Did you get it"},
		{repository.ConfirmationReceived, nil, "received"},
		{repository.ConfirmationIssueReported, nil, "Ticket #31"},
		{repository.ConfirmationAutoCompleted, []string{"champion:shoporder:issue:7"}, "completed automatically"},
		{repository.ConfirmationVoid, nil, "refunded"},
		{"SOMETHING_NEW", nil, "no longer"},
	}
	for _, tc := range cases {
		embed, components := BuildShopOrderStateMessage(repository.ShopOrderConfirmation{PurchaseID: 7, State: tc.state, DeadlineAt: time.Unix(1791028800, 0)}, 31, "")
		if components == nil {
			t.Fatalf("%s: components are nil, which would leave the old buttons on the message", tc.state)
		}
		if got := buttonIDs(t, components); strings.Join(got, "|") != strings.Join(tc.buttons, "|") {
			t.Fatalf("%s: buttons = %v, want %v", tc.state, got, tc.buttons)
		}
		if !strings.Contains(embed.Description, tc.says) {
			t.Fatalf("%s: description %q lacks %q", tc.state, embed.Description, tc.says)
		}
	}
}

func TestIssueModalRoundTrip(t *testing.T) {
	modal := BuildShopOrderIssueModal(7, 1000)
	action, purchase, ok := ParseShopOrderCustomID(modal.CustomID)
	if !ok || action != ShopOrderActionIssueModal || purchase != 7 {
		t.Fatalf("modal id %q", modal.CustomID)
	}
	input := modal.Components[0].(discordgo.ActionsRow).Components[0].(discordgo.TextInput)
	if !input.Required || input.MaxLength != 1000 || input.CustomID != ShopOrderReasonInputID {
		t.Fatalf("input = %+v", input)
	}
	submitted := discordgo.ModalSubmitInteractionData{Components: []discordgo.MessageComponent{
		&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: ShopOrderReasonInputID, Value: "never arrived"}}},
	}}
	if got := ShopOrderModalReason(submitted); got != "never arrived" {
		t.Fatalf("reason = %q", got)
	}
	if got := ShopOrderModalReason(discordgo.ModalSubmitInteractionData{}); got != "" {
		t.Fatalf("reason of an empty form = %q", got)
	}
}

func shopRESTErr(status, code int) error {
	return &discordgo.RESTError{Response: &http.Response{StatusCode: status}, Message: &discordgo.APIErrorMessage{Code: code}}
}

func TestIsPermanentDMFailure(t *testing.T) {
	for _, err := range []error{shopRESTErr(403, 50007), shopRESTErr(400, 50035), shopRESTErr(404, 10013)} {
		if !IsPermanentDMFailure(err) {
			t.Fatalf("%v should be permanent", err)
		}
	}
	for _, err := range []error{nil, errors.New("connection reset"), shopRESTErr(429, 0), shopRESTErr(500, 0), shopRESTErr(503, 0), shopRESTErr(408, 0), &discordgo.RESTError{}} {
		if IsPermanentDMFailure(err) {
			t.Fatalf("%v should be retried", err)
		}
	}
}

// fakeTicketGuild is an in-memory Discord server.
type fakeTicketGuild struct {
	ownerID      string
	roles        []*discordgo.Role
	channels     []*discordgo.Channel
	created      []discordgo.GuildChannelCreateData
	roleAdds     []string
	sent         []*discordgo.MessageSend
	sentTo       []string
	roleErr      error
	roleAddErr   error
	channelErr   error
	sendErr      error
	nextID       int
	rolesCreated []string
}

func (f *fakeTicketGuild) id(prefix string) string {
	f.nextID++
	return prefix + string(rune('0'+f.nextID))
}

func (f *fakeTicketGuild) GuildOwnerID(string) (string, error) { return f.ownerID, nil }
func (f *fakeTicketGuild) GuildRoles(string) ([]*discordgo.Role, error) {
	return f.roles, nil
}
func (f *fakeTicketGuild) GuildRoleCreate(_ string, data *discordgo.RoleParams) (*discordgo.Role, error) {
	if f.roleErr != nil {
		return nil, f.roleErr
	}
	if data.Permissions == nil || *data.Permissions != 0 {
		return nil, errors.New("a ticket role must be created without permissions")
	}
	r := &discordgo.Role{ID: f.id("role"), Name: data.Name}
	f.roles = append(f.roles, r)
	f.rolesCreated = append(f.rolesCreated, data.Name)
	return r, nil
}
func (f *fakeTicketGuild) GuildMemberRoleAdd(_, userID, roleID string) error {
	f.roleAdds = append(f.roleAdds, userID+">"+roleID)
	return f.roleAddErr
}
func (f *fakeTicketGuild) GuildChannels(string) ([]*discordgo.Channel, error) { return f.channels, nil }
func (f *fakeTicketGuild) GuildChannelCreateComplex(_ string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	if f.channelErr != nil {
		return nil, f.channelErr
	}
	ch := &discordgo.Channel{ID: f.id("chan"), Name: data.Name, Type: data.Type, ParentID: data.ParentID}
	f.channels = append(f.channels, ch)
	f.created = append(f.created, data)
	return ch, nil
}
func (f *fakeTicketGuild) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent, f.sentTo = append(f.sent, data), append(f.sentTo, channelID)
	return &discordgo.Message{ID: "msg"}, nil
}
func (f *fakeTicketGuild) BotUserID() string { return "bot" }

func TestEnsureShopTicketSetupCreatesRolesCategoryAndAssignsOwner(t *testing.T) {
	g := &fakeTicketGuild{ownerID: "owner-user", roles: []*discordgo.Role{{ID: "guild", Name: "@everyone"}}}
	setup, warning, err := EnsureShopTicketSetup(g, "guild", repository.ShopTicketDiscordSetup{})
	if err != nil || warning != nil {
		t.Fatalf("err=%v warning=%v", err, warning)
	}
	if strings.Join(g.rolesCreated, ",") != "Owner,Staff" || setup.OwnerRoleID == "" || setup.StaffRoleID == "" || setup.OwnerRoleID == setup.StaffRoleID {
		t.Fatalf("roles created=%v setup=%+v", g.rolesCreated, setup)
	}
	if len(g.roleAdds) != 1 || g.roleAdds[0] != "owner-user>"+setup.OwnerRoleID {
		t.Fatalf("role assignments = %v, want only the server owner getting Owner", g.roleAdds)
	}
	if len(g.created) != 1 || g.created[0].Type != discordgo.ChannelTypeGuildCategory || g.created[0].Name != ShopTicketCategoryName || setup.CategoryID == "" {
		t.Fatalf("category = %+v setup=%+v", g.created, setup)
	}
	assertTicketPrivacy(t, g.created[0].PermissionOverwrites, setup, "")

	// A second run reuses everything.
	again, _, err := EnsureShopTicketSetup(g, "guild", setup)
	if err != nil || again != setup || len(g.rolesCreated) != 2 || len(g.created) != 1 {
		t.Fatalf("second run: err=%v setup=%+v roles=%v categories=%d", err, again, g.rolesCreated, len(g.created))
	}
}

func TestEnsureShopTicketSetupAdoptsExistingAndReplacesDeleted(t *testing.T) {
	g := &fakeTicketGuild{
		ownerID: "owner-user",
		roles: []*discordgo.Role{{ID: "guild", Name: "@everyone"}, {ID: "existing-staff", Name: " staff "},
			{ID: "bot-managed", Name: "Owner", Managed: true}},
		channels: []*discordgo.Channel{{ID: "text-tickets", Name: "tickets", Type: discordgo.ChannelTypeGuildText},
			{ID: "existing-cat", Name: "TICKETS", Type: discordgo.ChannelTypeGuildCategory}},
	}
	// The stored ids point at a role and a category that were deleted since.
	setup, _, err := EnsureShopTicketSetup(g, "guild", repository.ShopTicketDiscordSetup{OwnerRoleID: "gone", StaffRoleID: "gone-too", CategoryID: "gone-cat"})
	if err != nil {
		t.Fatal(err)
	}
	if setup.StaffRoleID != "existing-staff" {
		t.Fatalf("staff role = %q, want the server's existing Staff role adopted", setup.StaffRoleID)
	}
	if setup.OwnerRoleID == "bot-managed" || setup.OwnerRoleID == "gone" || strings.Join(g.rolesCreated, ",") != "Owner" {
		t.Fatalf("owner role = %q created=%v, want a new Owner role (an integration's role is never adopted)", setup.OwnerRoleID, g.rolesCreated)
	}
	if setup.CategoryID != "existing-cat" || len(g.created) != 0 {
		t.Fatalf("category = %q created=%d, want the existing Tickets category (not the text channel) adopted", setup.CategoryID, len(g.created))
	}
}

func TestEnsureShopTicketSetupFailures(t *testing.T) {
	// The owner cannot be given the role (it sits above the bot): a warning, not a failure.
	g := &fakeTicketGuild{ownerID: "owner-user", roleAddErr: shopRESTErr(403, 50013)}
	setup, warning, err := EnsureShopTicketSetup(g, "guild", repository.ShopTicketDiscordSetup{})
	if err != nil || warning == nil || setup.CategoryID == "" {
		t.Fatalf("err=%v warning=%v setup=%+v", err, warning, setup)
	}

	// Missing Manage Roles: nothing is half-recorded.
	g = &fakeTicketGuild{roleErr: shopRESTErr(403, 50013)}
	setup, _, err = EnsureShopTicketSetup(g, "guild", repository.ShopTicketDiscordSetup{})
	if err == nil || setup != (repository.ShopTicketDiscordSetup{}) {
		t.Fatalf("err=%v setup=%+v", err, setup)
	}

	// Missing Manage Channels: the roles that were created are returned so they are remembered.
	g = &fakeTicketGuild{channelErr: shopRESTErr(403, 50013)}
	setup, _, err = EnsureShopTicketSetup(g, "guild", repository.ShopTicketDiscordSetup{})
	if err == nil || setup.OwnerRoleID == "" || setup.StaffRoleID == "" || setup.CategoryID != "" {
		t.Fatalf("err=%v setup=%+v", err, setup)
	}
}

// assertTicketPrivacy checks that only the bot, the two roles and (when given) the buyer can see a channel.
func assertTicketPrivacy(t *testing.T, overwrites []*discordgo.PermissionOverwrite, setup repository.ShopTicketDiscordSetup, buyer string) {
	t.Helper()
	allowed := map[string]discordgo.PermissionOverwriteType{"bot": discordgo.PermissionOverwriteTypeMember,
		setup.OwnerRoleID: discordgo.PermissionOverwriteTypeRole, setup.StaffRoleID: discordgo.PermissionOverwriteTypeRole}
	if buyer != "" {
		allowed[buyer] = discordgo.PermissionOverwriteTypeMember
	}
	everyoneDenied := false
	for _, o := range overwrites {
		if o.ID == "guild" {
			everyoneDenied = o.Type == discordgo.PermissionOverwriteTypeRole && o.Deny&discordgo.PermissionViewChannel != 0 && o.Allow == 0
			continue
		}
		kind, ok := allowed[o.ID]
		if !ok || kind != o.Type || o.Allow&discordgo.PermissionViewChannel == 0 || o.Allow&discordgo.PermissionSendMessages == 0 || o.Deny != 0 {
			t.Fatalf("unexpected overwrite %+v", o)
		}
		if o.Allow&(discordgo.PermissionManageChannels|discordgo.PermissionManageRoles|discordgo.PermissionAdministrator|discordgo.PermissionMentionEveryone) != 0 {
			t.Fatalf("overwrite %+v grants more than ticket access", o)
		}
		delete(allowed, o.ID)
	}
	if !everyoneDenied || len(allowed) != 0 {
		t.Fatalf("everyone denied=%v, missing overwrites for %v", everyoneDenied, allowed)
	}
}

func ticketJob() repository.ShopTicketChannelJob {
	return repository.ShopTicketChannelJob{DiscordGuildID: "guild", GuildRowID: 5, Items: []repository.ShopNoticeItem{{Name: "BandageDressing", Quantity: 1}},
		Ticket: repository.ShopOrderTicket{ID: 31, PurchaseID: 7, OpenedByDiscordID: "555000111", OpenedVia: repository.ConfirmationViaDiscord,
			Reason: "it never arrived @everyone <@&999>", Status: repository.TicketOpen, OpenedAt: time.Unix(1791028800, 0)}}
}

func TestCreateShopTicketChannelIsPrivateAndPingsOnlyTheParticipants(t *testing.T) {
	g := &fakeTicketGuild{}
	setup := repository.ShopTicketDiscordSetup{OwnerRoleID: "owner-role", StaffRoleID: "staff-role", CategoryID: "cat"}
	channelID, posted, err := CreateShopTicketChannel(g, ticketJob(), setup, "https://example.test")
	if err != nil || !posted || channelID == "" {
		t.Fatalf("channel=%q posted=%v err=%v", channelID, posted, err)
	}
	made := g.created[0]
	if made.Name != "ticket-31-order-7" || made.Type != discordgo.ChannelTypeGuildText || made.ParentID != "cat" {
		t.Fatalf("channel = %+v", made)
	}
	assertTicketPrivacy(t, made.PermissionOverwrites, setup, "555000111")

	msg := g.sent[0]
	if g.sentTo[0] != channelID || msg.Content != "<@555000111> <@&staff-role> <@&owner-role>" {
		t.Fatalf("opening message to %q: %q", g.sentTo[0], msg.Content)
	}
	m := msg.AllowedMentions
	if m == nil || len(m.Parse) != 0 || strings.Join(m.Users, ",") != "555000111" || strings.Join(m.Roles, ",") != "staff-role,owner-role" {
		t.Fatalf("allowed mentions = %+v; the buyer's text must not be able to ping anyone", m)
	}
	embed := msg.Embeds[0]
	if !strings.Contains(embed.Title, "#31") || embed.Fields[0].Value != "it never arrived @everyone <@&999>" || !strings.Contains(embed.Fields[1].Value, "BandageDressing") ||
		embed.Fields[2].Value != "Discord" || !strings.Contains(embed.Fields[4].Value, "https://example.test/dashboard/shop/admin/orders/7") {
		t.Fatalf("embed = %+v", embed)
	}
}

func TestCreateShopTicketChannelFailures(t *testing.T) {
	setup := repository.ShopTicketDiscordSetup{OwnerRoleID: "owner-role", StaffRoleID: "staff-role", CategoryID: "cat"}

	g := &fakeTicketGuild{channelErr: shopRESTErr(403, 50013)}
	if channelID, posted, err := CreateShopTicketChannel(g, ticketJob(), setup, ""); err == nil || channelID != "" || posted {
		t.Fatalf("channel=%q posted=%v err=%v", channelID, posted, err)
	}

	// The channel exists but the message failed: the id is still returned so it is recorded once.
	g = &fakeTicketGuild{sendErr: shopRESTErr(500, 0)}
	channelID, posted, err := CreateShopTicketChannel(g, ticketJob(), setup, "")
	if err == nil || channelID == "" || posted {
		t.Fatalf("channel=%q posted=%v err=%v", channelID, posted, err)
	}
}

func TestShopTicketResolvedNotice(t *testing.T) {
	refunded, note := repository.TicketResolutionRefunded, strings.Repeat("é", 1500)
	embed := BuildShopTicketResolved(repository.ShopOrderTicket{ID: 31, Resolution: &refunded, ResolutionNote: &note})
	if !strings.Contains(embed.Title, "#31") || !strings.Contains(embed.Description, "refunded") {
		t.Fatalf("embed = %+v", embed)
	}
	if got := len([]rune(embed.Fields[0].Value)); got != 1000 {
		t.Fatalf("note is %d characters, want it cut to 1000", got)
	}
	if embed := BuildShopTicketResolved(repository.ShopOrderTicket{ID: 31}); len(embed.Fields) != 0 {
		t.Fatalf("a ticket without a note has fields: %+v", embed.Fields)
	}
}

func TestSystemTicketWithoutADiscordBuyer(t *testing.T) {
	g := &fakeTicketGuild{}
	setup := repository.ShopTicketDiscordSetup{OwnerRoleID: "owner-role", StaffRoleID: "staff-role", CategoryID: "cat"}
	job := ticketJob()
	job.Ticket.OpenedVia, job.Ticket.OpenedByDiscordID = repository.TicketViaSystem, "system"
	if _, posted, err := CreateShopTicketChannel(g, job, setup, ""); err != nil || !posted {
		t.Fatalf("posted=%v err=%v", posted, err)
	}
	// The placeholder opener is never sent to Discord as a member or a mention.
	assertTicketPrivacy(t, g.created[0].PermissionOverwrites, setup, "")
	msg := g.sent[0]
	if strings.Contains(msg.Content, "system") || len(msg.AllowedMentions.Users) != 0 || msg.Content != "<@&staff-role> <@&owner-role>" {
		t.Fatalf("content=%q mentions=%+v", msg.Content, msg.AllowedMentions)
	}
	if !strings.Contains(msg.Embeds[0].Description, "Automatic delivery") || msg.Embeds[0].Fields[2].Value != "automatic delivery" {
		t.Fatalf("embed = %+v", msg.Embeds[0])
	}
}
