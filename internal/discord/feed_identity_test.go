package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeIdentityStore struct {
	identity repository.FeedIdentitySettings
	ok       bool
	err      error
	lookups  int
}

func (s *fakeIdentityStore) FeedIdentityForServer(context.Context, int64) (repository.FeedIdentitySettings, bool, error) {
	s.lookups++
	return s.identity, s.ok, s.err
}

type fakeWebhookAPI struct {
	existing   []*discordgo.Webhook
	listErr    error
	createErr  error
	executeErr error

	uniqueIDs       bool
	lists, creates  int
	botSends        []*discordgo.MessageSend
	webhookSends    []*discordgo.WebhookParams
	executedWebhook string
	sends           int
	botDeletes      []string
	botDeleteErr    error
	nonces          []string
	webhookDeletes  []string
}

func (a *fakeWebhookAPI) ChannelMessagesBulkDelete(_ string, messages []string, _ ...discordgo.RequestOption) error {
	a.botDeletes = append(a.botDeletes, messages...)
	return nil
}
func (a *fakeWebhookAPI) ChannelMessageDelete(_, messageID string, _ ...discordgo.RequestOption) error {
	if a.botDeleteErr != nil {
		return a.botDeleteErr
	}
	a.botDeletes = append(a.botDeletes, messageID)
	return nil
}
func (a *fakeWebhookAPI) ChannelMessageSendNonce(_ string, data *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
	a.nonces = append(a.nonces, nonce)
	a.botSends = append(a.botSends, data)
	return &discordgo.Message{ID: "bot-nonce"}, nil
}
func (a *fakeWebhookAPI) WebhookMessageDelete(_, _, messageID string, _ ...discordgo.RequestOption) error {
	a.webhookDeletes = append(a.webhookDeletes, messageID)
	return nil
}
func (a *fakeWebhookAPI) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	a.botSends = append(a.botSends, data)
	a.sends++
	if a.uniqueIDs {
		return &discordgo.Message{ID: fmt.Sprint("bot-", a.sends)}, nil
	}
	return &discordgo.Message{ID: "bot"}, nil
}
func (a *fakeWebhookAPI) ChannelWebhooks(string, ...discordgo.RequestOption) ([]*discordgo.Webhook, error) {
	a.lists++
	return a.existing, a.listErr
}
func (a *fakeWebhookAPI) WebhookCreate(_, name, _ string, _ ...discordgo.RequestOption) (*discordgo.Webhook, error) {
	a.creates++
	if a.createErr != nil {
		return nil, a.createErr
	}
	return &discordgo.Webhook{ID: "created", Token: "tok-created", Name: name}, nil
}
func (a *fakeWebhookAPI) WebhookExecute(id, _ string, _ bool, data *discordgo.WebhookParams, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if a.executeErr != nil {
		return nil, a.executeErr
	}
	a.executedWebhook = id
	a.webhookSends = append(a.webhookSends, data)
	a.sends++
	if a.uniqueIDs {
		return &discordgo.Message{ID: fmt.Sprint("hook-", a.sends)}, nil
	}
	return &discordgo.Message{ID: "hook"}, nil
}

func feedMessage() *discordgo.MessageSend {
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{{Title: "kill"}}}
}

func TestFeedIdentitySendsThroughWebhookUnderTheIdentity(t *testing.T) {
	store := &fakeIdentityStore{identity: repository.FeedIdentitySettings{Enabled: true, Name: "Deadzone Feed", AvatarURL: "https://cdn.example/a.png"}, ok: true}
	api := &fakeWebhookAPI{}
	f := NewFeedIdentity(store)
	sender := f.Sender(api, 7)

	for i := 0; i < 3; i++ {
		msg, err := sender.ChannelMessageSendComplex("chan", feedMessage())
		if err != nil || msg.ID != "hook" {
			t.Fatalf("send %d: %v %v", i, msg, err)
		}
	}
	if len(api.botSends) != 0 || len(api.webhookSends) != 3 {
		t.Fatalf("bot=%d webhook=%d", len(api.botSends), len(api.webhookSends))
	}
	got := api.webhookSends[0]
	if got.Username != "Deadzone Feed" || got.AvatarURL != "https://cdn.example/a.png" || len(got.Embeds) != 1 {
		t.Fatalf("webhook params = %+v", got)
	}
	if got.AllowedMentions == nil || len(got.AllowedMentions.Parse) != 0 {
		t.Fatal("a webhook send must never be able to ping")
	}
	// The identity and the webhook are both looked up once, then cached.
	if store.lookups != 1 || api.lists != 1 || api.creates != 1 {
		t.Fatalf("lookups=%d lists=%d creates=%d", store.lookups, api.lists, api.creates)
	}
	if via, fb := f.Stats(); via != 3 || fb != 0 {
		t.Fatalf("stats = %d, %d", via, fb)
	}
}

func TestFeedIdentityReusesAnExistingChampionWebhook(t *testing.T) {
	store := &fakeIdentityStore{identity: repository.FeedIdentitySettings{Enabled: true, Name: "X"}, ok: true}
	api := &fakeWebhookAPI{existing: []*discordgo.Webhook{
		{ID: "other", Name: "Someone Else", Token: "t"}, {ID: "tokenless", Name: feedWebhookName}, {ID: "ours", Name: feedWebhookName, Token: "tok"}}}
	if _, err := NewFeedIdentity(store).Sender(api, 1).ChannelMessageSendComplex("chan", feedMessage()); err != nil {
		t.Fatal(err)
	}
	if api.creates != 0 || api.executedWebhook != "ours" {
		t.Fatalf("creates=%d executed=%q", api.creates, api.executedWebhook)
	}
}

func TestFeedIdentityFallsBackToTheBot(t *testing.T) {
	enabled := repository.FeedIdentitySettings{Enabled: true, Name: "X"}
	on := func() *fakeIdentityStore { return &fakeIdentityStore{identity: enabled, ok: true} }
	cases := map[string]struct {
		store *fakeIdentityStore
		api   *fakeWebhookAPI
		msg   *discordgo.MessageSend
	}{
		"no identity":             {&fakeIdentityStore{}, &fakeWebhookAPI{}, feedMessage()},
		"identity lookup fails":   {&fakeIdentityStore{err: errors.New("db down")}, &fakeWebhookAPI{}, feedMessage()},
		"cannot list webhooks":    {on(), &fakeWebhookAPI{listErr: errors.New("403 Missing Permissions")}, feedMessage()},
		"cannot create webhook":   {on(), &fakeWebhookAPI{createErr: errors.New("403 Missing Permissions")}, feedMessage()},
		"execute fails":           {on(), &fakeWebhookAPI{executeErr: errors.New("404 Unknown Webhook")}, feedMessage()},
		"message with a file":     {on(), &fakeWebhookAPI{}, &discordgo.MessageSend{Files: []*discordgo.File{{Name: "a.png", Reader: strings.NewReader("x")}}}},
		"message with components": {on(), &fakeWebhookAPI{}, &discordgo.MessageSend{Content: "x", Components: []discordgo.MessageComponent{discordgo.ActionsRow{}}}},
		"empty message":           {on(), &fakeWebhookAPI{}, &discordgo.MessageSend{}},
	}
	for name, c := range cases {
		msg, err := NewFeedIdentity(c.store).Sender(c.api, 1).ChannelMessageSendComplex("chan", c.msg)
		if err != nil || msg.ID != "bot" || len(c.api.botSends) != 1 || len(c.api.webhookSends) != 0 {
			t.Errorf("%s: msg=%v err=%v bot=%d webhook=%d", name, msg, err, len(c.api.botSends), len(c.api.webhookSends))
		}
	}
}

func TestFeedIdentityBacksOffARefusedChannelAndRecoversADeletedWebhook(t *testing.T) {
	store := &fakeIdentityStore{identity: repository.FeedIdentitySettings{Enabled: true, Name: "X"}, ok: true}
	api := &fakeWebhookAPI{createErr: errors.New("403")}
	now := time.Unix(1_700_000_000, 0)
	f := NewFeedIdentity(store)
	f.now = func() time.Time { return now }
	sender := f.Sender(api, 1)
	for i := 0; i < 5; i++ {
		_, _ = sender.ChannelMessageSendComplex("chan", feedMessage())
	}
	if api.lists != 1 || api.creates != 1 || len(api.botSends) != 5 {
		t.Fatalf("a refused channel was retried on every message: lists=%d creates=%d", api.lists, api.creates)
	}
	// Permission granted later: after the back-off the webhook is created and used.
	api.createErr = nil
	now = now.Add(feedWebhookRetryAfter + time.Second)
	if msg, _ := sender.ChannelMessageSendComplex("chan", feedMessage()); msg.ID != "hook" {
		t.Fatalf("did not recover after the back-off: %v", msg)
	}
	// The webhook is deleted in Discord: this message goes out as the bot, the next finds a new one.
	api.executeErr = errors.New("404 Unknown Webhook")
	if msg, _ := sender.ChannelMessageSendComplex("chan", feedMessage()); msg.ID != "bot" {
		t.Fatalf("a failed webhook send lost the message: %v", msg)
	}
	api.executeErr = nil
	creates := api.creates
	if msg, _ := sender.ChannelMessageSendComplex("chan", feedMessage()); msg.ID != "hook" || api.creates != creates+1 {
		t.Fatalf("did not re-create the deleted webhook: %v creates=%d", msg, api.creates)
	}
}

func TestFeedIdentityDeletesItsOwnWebhookMessagesThroughTheWebhook(t *testing.T) {
	store := &fakeIdentityStore{identity: repository.FeedIdentitySettings{Enabled: true, Name: "X"}, ok: true}
	api := &fakeWebhookAPI{uniqueIDs: true}
	f := NewFeedIdentity(store)
	sender := f.Sender(api, 1)
	viaHook, _ := sender.ChannelMessageSendComplex("chan", feedMessage())
	// A message the webhook cannot carry goes out as the bot.
	asBot, _ := sender.ChannelMessageSendComplex("chan", &discordgo.MessageSend{Content: "x", Components: []discordgo.MessageComponent{discordgo.ActionsRow{}}})
	if err := sender.ChannelMessagesBulkDelete("chan", []string{viaHook.ID, asBot.ID, "older-bot-message"}); err != nil {
		t.Fatal(err)
	}
	if len(api.webhookDeletes) != 1 || api.webhookDeletes[0] != viaHook.ID {
		t.Fatalf("webhook deletes = %v", api.webhookDeletes)
	}
	if len(api.botDeletes) != 2 || api.botDeletes[0] != asBot.ID || api.botDeletes[1] != "older-bot-message" {
		t.Fatalf("bot deletes = %v", api.botDeletes)
	}
	// Deleted once: a second cleanup of the same id is not sent to the webhook again.
	_ = sender.ChannelMessagesBulkDelete("chan", []string{viaHook.ID})
	if len(api.webhookDeletes) != 1 {
		t.Fatalf("a message was deleted through the webhook twice: %v", api.webhookDeletes)
	}
	// The memory of sent messages is bounded.
	for i := 0; i < feedSentRemembered+100; i++ {
		_, _ = sender.ChannelMessageSendComplex("chan", feedMessage())
	}
	f.mu.Lock()
	remembered, order := len(f.sent), len(f.sentOrder)
	f.mu.Unlock()
	if remembered > feedSentRemembered || order > feedSentRemembered {
		t.Fatalf("sent memory grew to %d/%d", remembered, order)
	}
}

func TestFeedIdentityNonceSendAndSingleDelete(t *testing.T) {
	store := &fakeIdentityStore{identity: repository.FeedIdentitySettings{Enabled: true, Name: "X"}, ok: true}
	api := &fakeWebhookAPI{uniqueIDs: true}
	sender := NewFeedIdentity(store).Sender(api, 1)
	ns, ok := sender.(nonceSender)
	if !ok {
		t.Fatal("the identity sender must keep the nonce send the immediate feed mode relies on")
	}
	single, ok := sender.(singleMessageDeleter)
	if !ok {
		t.Fatal("the identity sender must keep the single-message delete")
	}
	// Under an identity the card goes through the webhook and the nonce send is not used.
	msg, err := ns.ChannelMessageSendNonce("chan", feedMessage(), "n-1")
	if err != nil || len(api.webhookSends) != 1 || len(api.nonces) != 0 {
		t.Fatalf("msg=%v err=%v webhook=%d nonces=%v", msg, err, len(api.webhookSends), api.nonces)
	}
	if err := single.ChannelMessageDelete("chan", msg.ID); err != nil || len(api.webhookDeletes) != 1 || len(api.botDeletes) != 0 {
		t.Fatalf("delete of a webhook card: err=%v webhook=%v bot=%v", err, api.webhookDeletes, api.botDeletes)
	}
	// A failing webhook falls back to the nonce-enforced bot send, with the same nonce.
	api.executeErr = errors.New("500")
	if msg, err := ns.ChannelMessageSendNonce("chan", feedMessage(), "n-2"); err != nil || msg.ID != "bot-nonce" || len(api.nonces) != 1 || api.nonces[0] != "n-2" {
		t.Fatalf("fallback: msg=%v err=%v nonces=%v", msg, err, api.nonces)
	}
	api.executeErr = nil
	// A card from before a restart: the bot delete is refused, the channel webhook removes it.
	api.botDeleteErr = errors.New("HTTP 403 Forbidden, Missing Permissions")
	if err := single.ChannelMessageDelete("chan", "from-last-process"); err != nil || api.webhookDeletes[len(api.webhookDeletes)-1] != "from-last-process" {
		t.Fatalf("recovery delete: err=%v webhook=%v", err, api.webhookDeletes)
	}
	// With no identity the refusal is reported as it is.
	plain := NewFeedIdentity(&fakeIdentityStore{}).Sender(api, 2).(singleMessageDeleter)
	if err := plain.ChannelMessageDelete("chan", "x"); err == nil {
		t.Fatal("a refused delete with no identity was swallowed")
	}
}

func TestFeedIdentityInvalidateAndNilSafety(t *testing.T) {
	store := &fakeIdentityStore{}
	api := &fakeWebhookAPI{}
	f := NewFeedIdentity(store)
	sender := f.Sender(api, 9)
	_, _ = sender.ChannelMessageSendComplex("chan", feedMessage())
	store.identity, store.ok = repository.FeedIdentitySettings{Enabled: true, Name: "Now On"}, true
	if msg, _ := sender.ChannelMessageSendComplex("chan", feedMessage()); msg.ID != "bot" {
		t.Fatal("the cached no-identity answer should still apply inside the TTL")
	}
	f.Invalidate(9)
	if msg, _ := sender.ChannelMessageSendComplex("chan", feedMessage()); msg.ID != "hook" {
		t.Fatal("Invalidate did not make the new identity apply to the next message")
	}

	var none *FeedIdentity
	none.Invalidate(1)
	if got := none.Sender(api, 1); got != FeedIdentityAPI(api) {
		t.Fatal("a nil FeedIdentity must hand back the plain sender")
	}
}

func TestBuildHotZoneOpenedEmbed(t *testing.T) {
	ends := time.Unix(1_700_000_000, 0)
	e := BuildHotZoneOpenedEmbed(HotZoneAnnouncement{Name: "Hot Zone 7750 / 12750", ServerName: "Chernarus", CenterX: 7750, CenterZ: 12750, RadiusM: 500,
		KillsObserved: 9, WindowMinutes: 60, EndsAt: &ends, FirstPoints: 500, SecondPoints: 250})
	if !strings.Contains(e.Description, "9 kills there in the last 60 minutes") {
		t.Fatalf("description = %q", e.Description)
	}
	values := map[string]string{}
	for _, f := range e.Fields {
		values[f.Name] = f.Value
	}
	if values["WHERE"] != "Within 500 m of 7750 / 12750" || values["ENDS"] != "<t:1700000000:R>" || values["PRIZES"] != "🥇 500 pts • 🥈 250 pts" {
		t.Fatalf("fields = %v", values)
	}
	// No prizes configured: the prize field is omitted, never an empty heading.
	for _, f := range BuildHotZoneOpenedEmbed(HotZoneAnnouncement{Name: "Z"}).Fields {
		if f.Name == "PRIZES" {
			t.Fatal("an empty PRIZES field was rendered")
		}
	}
}
