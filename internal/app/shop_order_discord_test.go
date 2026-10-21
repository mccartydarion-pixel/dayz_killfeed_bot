package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeDeskStore struct {
	notices     []repository.ShopOrderNotice
	jobs        []repository.ShopTicketChannelJob
	marked      map[int64][3]string
	released    []int64
	claimedFor  []int64
	setup       repository.ShopTicketDiscordSetup
	savedSetups []repository.ShopTicketDiscordSetup
	channels    map[int64]string
	ticketsFree []int64
}

func newFakeDeskStore() *fakeDeskStore {
	return &fakeDeskStore{marked: map[int64][3]string{}, channels: map[int64]string{}}
}

func (f *fakeDeskStore) ClaimPendingNotices(context.Context, time.Time, int) ([]repository.ShopOrderNotice, error) {
	out := f.notices
	f.notices = nil
	return out, nil
}
func (f *fakeDeskStore) MarkNotice(_ context.Context, purchaseID int64, state, channelID, messageID string) error {
	f.marked[purchaseID] = [3]string{state, channelID, messageID}
	return nil
}
func (f *fakeDeskStore) ReleaseNotice(_ context.Context, purchaseID int64) error {
	f.released = append(f.released, purchaseID)
	return nil
}
func (f *fakeDeskStore) ClaimTicketsNeedingChannel(_ context.Context, _ time.Time, ticketID int64, _ int) ([]repository.ShopTicketChannelJob, error) {
	f.claimedFor = append(f.claimedFor, ticketID)
	out := f.jobs
	f.jobs = nil
	return out, nil
}
func (f *fakeDeskStore) ReleaseTicketChannel(_ context.Context, ticketID int64) error {
	f.ticketsFree = append(f.ticketsFree, ticketID)
	return nil
}
func (f *fakeDeskStore) TicketDiscordSetup(context.Context, int64) (repository.ShopTicketDiscordSetup, error) {
	return f.setup, nil
}
func (f *fakeDeskStore) SaveTicketDiscordSetup(_ context.Context, _ int64, s repository.ShopTicketDiscordSetup) error {
	f.setup = s
	f.savedSetups = append(f.savedSetups, s)
	return nil
}
func (f *fakeDeskStore) SetTicketChannel(_ context.Context, _, _, id int64, channelID string) (repository.ShopOrderTicket, error) {
	f.channels[id] = channelID
	return repository.ShopOrderTicket{ID: id}, nil
}

type fakeDeskDiscord struct {
	dmErr, sendErr, roleErr error
	dmFor                   []string
	sent                    map[string][]*discordgo.MessageSend
	rolesCreated            int
	channelsCreated         []discordgo.GuildChannelCreateData
	roles                   []*discordgo.Role
	channels                []*discordgo.Channel
}

func (f *fakeDeskDiscord) UserChannelCreate(recipientID string) (*discordgo.Channel, error) {
	f.dmFor = append(f.dmFor, recipientID)
	if f.dmErr != nil {
		return nil, f.dmErr
	}
	return &discordgo.Channel{ID: "dm-" + recipientID}, nil
}
func (f *fakeDeskDiscord) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend) (*discordgo.Message, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	if f.sent == nil {
		f.sent = map[string][]*discordgo.MessageSend{}
	}
	f.sent[channelID] = append(f.sent[channelID], data)
	return &discordgo.Message{ID: "m-" + channelID}, nil
}
func (f *fakeDeskDiscord) GuildOwnerID(string) (string, error)          { return "owner-user", nil }
func (f *fakeDeskDiscord) GuildRoles(string) ([]*discordgo.Role, error) { return f.roles, nil }
func (f *fakeDeskDiscord) GuildRoleCreate(_ string, data *discordgo.RoleParams) (*discordgo.Role, error) {
	if f.roleErr != nil {
		return nil, f.roleErr
	}
	f.rolesCreated++
	r := &discordgo.Role{ID: "role-" + data.Name, Name: data.Name}
	f.roles = append(f.roles, r)
	return r, nil
}
func (f *fakeDeskDiscord) GuildMemberRoleAdd(string, string, string) error { return nil }
func (f *fakeDeskDiscord) GuildChannels(string) ([]*discordgo.Channel, error) {
	return f.channels, nil
}
func (f *fakeDeskDiscord) GuildChannelCreateComplex(_ string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	f.channelsCreated = append(f.channelsCreated, data)
	ch := &discordgo.Channel{ID: "ch-" + data.Name, Name: data.Name, Type: data.Type}
	f.channels = append(f.channels, ch)
	return ch, nil
}
func (f *fakeDeskDiscord) BotUserID() string { return "bot" }

func deskRESTError(status, code int) error {
	return &discordgo.RESTError{Response: &http.Response{StatusCode: status}, Message: &discordgo.APIErrorMessage{Code: code}}
}

func TestDeskNoticeOutcomes(t *testing.T) {
	notice := func(id int64, buyer string) repository.ShopOrderNotice {
		return repository.ShopOrderNotice{PurchaseID: id, BuyerDiscordID: buyer, DeadlineAt: time.Now().Add(48 * time.Hour)}
	}

	t.Run("delivered", func(t *testing.T) {
		store, api := newFakeDeskStore(), &fakeDeskDiscord{}
		store.notices = []repository.ShopOrderNotice{notice(7, "buyer")}
		newShopOrderDesk(store, api, "https://example.test").sendNotices(context.Background())
		if store.marked[7] != [3]string{repository.NoticeSent, "dm-buyer", "m-dm-buyer"} || len(store.released) != 0 {
			t.Fatalf("marked=%v released=%v", store.marked, store.released)
		}
		msg := api.sent["dm-buyer"][0]
		if len(msg.Components) != 1 || msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 {
			t.Fatalf("message = %+v", msg)
		}
	})

	t.Run("no verified Discord account", func(t *testing.T) {
		store, api := newFakeDeskStore(), &fakeDeskDiscord{}
		store.notices = []repository.ShopOrderNotice{notice(7, "")}
		newShopOrderDesk(store, api, "").sendNotices(context.Background())
		if store.marked[7][0] != repository.NoticeUnavailable || len(api.dmFor) != 0 {
			t.Fatalf("marked=%v dmFor=%v", store.marked, api.dmFor)
		}
	})

	t.Run("DMs closed", func(t *testing.T) {
		store, api := newFakeDeskStore(), &fakeDeskDiscord{sendErr: deskRESTError(403, 50007)}
		store.notices = []repository.ShopOrderNotice{notice(7, "buyer")}
		newShopOrderDesk(store, api, "").sendNotices(context.Background())
		if store.marked[7][0] != repository.NoticeUnavailable || len(store.released) != 0 {
			t.Fatalf("marked=%v released=%v, want UNAVAILABLE without a retry", store.marked, store.released)
		}
	})

	t.Run("Discord outage is retried", func(t *testing.T) {
		for _, api := range []*fakeDeskDiscord{{dmErr: deskRESTError(503, 0)}, {sendErr: errors.New("connection reset")}} {
			store := newFakeDeskStore()
			store.notices = []repository.ShopOrderNotice{notice(7, "buyer"), notice(8, "buyer")}
			newShopOrderDesk(store, api, "").sendNotices(context.Background())
			if len(store.marked) != 0 || len(store.released) != 2 {
				t.Fatalf("marked=%v released=%v, want both released for a retry", store.marked, store.released)
			}
		}
	})
}

func deskJob(id int64) repository.ShopTicketChannelJob {
	return repository.ShopTicketChannelJob{GuildRowID: 5, DiscordGuildID: "guild",
		Ticket: repository.ShopOrderTicket{ID: id, OrganizationID: 1, InstallationID: 11, PurchaseID: 7, OpenedByDiscordID: "buyer", Reason: "missing", OpenedAt: time.Now()}}
}

func TestDeskCreatesRolesOnceAndOneChannelPerTicket(t *testing.T) {
	store, api := newFakeDeskStore(), &fakeDeskDiscord{}
	desk := newShopOrderDesk(store, api, "")

	store.jobs = []repository.ShopTicketChannelJob{deskJob(31), deskJob(32)}
	desk.openTicketChannels(context.Background(), 0)

	if api.rolesCreated != 2 || len(store.savedSetups) != 1 {
		t.Fatalf("roles created=%d setups saved=%d, want Owner and Staff created once and stored once", api.rolesCreated, len(store.savedSetups))
	}
	want := repository.ShopTicketDiscordSetup{OwnerRoleID: "role-Owner", StaffRoleID: "role-Staff", CategoryID: "ch-Tickets"}
	if store.setup != want {
		t.Fatalf("setup = %+v, want %+v", store.setup, want)
	}
	if store.channels[31] != "ch-ticket-31-order-7" || store.channels[32] != "ch-ticket-32-order-7" || len(store.ticketsFree) != 0 {
		t.Fatalf("channels=%v released=%v", store.channels, store.ticketsFree)
	}
	if len(api.channelsCreated) != 3 { // the category and two ticket channels
		t.Fatalf("%d channels created, want 3", len(api.channelsCreated))
	}
	if len(api.sent["ch-ticket-31-order-7"]) != 1 {
		t.Fatalf("opening messages = %v", api.sent)
	}

	// The immediate attempt after a new ticket claims only that ticket.
	store.jobs = []repository.ShopTicketChannelJob{deskJob(33)}
	desk.openTicketChannels(context.Background(), 33)
	if store.claimedFor[len(store.claimedFor)-1] != 33 || store.channels[33] == "" || api.rolesCreated != 2 || len(store.savedSetups) != 1 {
		t.Fatalf("claimedFor=%v channels=%v roles=%d saved=%d", store.claimedFor, store.channels, api.rolesCreated, len(store.savedSetups))
	}
}

func TestDeskReleasesATicketWhenTheBotLacksPermission(t *testing.T) {
	store, api := newFakeDeskStore(), &fakeDeskDiscord{roleErr: deskRESTError(403, 50013)}
	store.jobs = []repository.ShopTicketChannelJob{deskJob(31)}
	newShopOrderDesk(store, api, "").openTicketChannels(context.Background(), 0)
	if len(store.channels) != 0 || len(store.ticketsFree) != 1 || store.ticketsFree[0] != 31 || len(api.channelsCreated) != 0 {
		t.Fatalf("channels=%v released=%v created=%d", store.channels, store.ticketsFree, len(api.channelsCreated))
	}
}

func TestDeskResolvedNoticeAndNilSafety(t *testing.T) {
	store, api := newFakeDeskStore(), &fakeDeskDiscord{}
	desk := newShopOrderDesk(store, api, "")
	channel, completed := "ch-1", repository.TicketResolutionCompleted

	desk.ticketResolved(repository.ShopOrderTicket{ID: 31, Resolution: &completed}) // no channel: nothing to post
	desk.ticketResolved(repository.ShopOrderTicket{ID: 31, Resolution: &completed, DiscordChannelID: &channel})
	if len(api.sent) != 1 || len(api.sent["ch-1"]) != 1 {
		t.Fatalf("sent = %v", api.sent)
	}

	// Before Discord is connected there is no desk: the API hooks must be harmless.
	var none *shopOrderDesk
	none.ticketOpened(1)
	none.ticketResolved(repository.ShopOrderTicket{DiscordChannelID: &channel})
	a := &App{}
	a.shopTicketOpened(1)
	a.shopTicketResolved(repository.ShopOrderTicket{DiscordChannelID: &channel})

	// A full kick queue never blocks the caller.
	for i := 0; i < 100; i++ {
		desk.ticketOpened(int64(i))
	}
}
