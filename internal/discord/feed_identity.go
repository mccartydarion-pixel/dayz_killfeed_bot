package discord

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Feed identity (docs/FEED_IDENTITY.md): an installation may have its feed messages posted under
// its own name and avatar. Discord only allows that through a channel webhook, so a feed whose
// server has an identity enabled is sent through a Champion-owned webhook in the feed's channel.
// Everything that is not a plain feed message - slash commands, panels, DMs - is still the bot.
//
// The feed never depends on it: no identity, a message a webhook cannot carry, a missing Manage
// Webhooks permission or any webhook error all fall back to the ordinary bot send.

// feedWebhookName is the webhook Champion creates and reuses in a feed channel.
const feedWebhookName = "Champion Feed"

const (
	feedIdentityTTL        = time.Minute      // how long a server's identity is cached
	feedWebhookRetryAfter  = 10 * time.Minute // how long a channel that refused a webhook is left alone
	feedIdentityLookupTime = 3 * time.Second
)

// FeedIdentityStore resolves a server's enabled feed identity.
type FeedIdentityStore interface {
	FeedIdentityForServer(ctx context.Context, serverID int64) (repository.FeedIdentitySettings, bool, error)
}

// FeedIdentityAPI is the Discord surface the identity sender needs; *discordgo.Session satisfies it.
type FeedIdentityAPI interface {
	ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelWebhooks(channelID string, options ...discordgo.RequestOption) ([]*discordgo.Webhook, error)
	WebhookCreate(channelID, name, avatar string, options ...discordgo.RequestOption) (*discordgo.Webhook, error)
	WebhookExecute(webhookID, token string, wait bool, data *discordgo.WebhookParams, options ...discordgo.RequestOption) (*discordgo.Message, error)
	WebhookMessageDelete(webhookID, token, messageID string, options ...discordgo.RequestOption) error
}

type cachedIdentity struct {
	identity repository.FeedIdentitySettings
	ok       bool
	at       time.Time
}

type cachedWebhook struct {
	id, token string
	failedAt  time.Time // non-zero: the channel refused a webhook; retry after feedWebhookRetryAfter
}

// FeedIdentity is the process-wide identity resolver and webhook cache shared by every feed.
type FeedIdentity struct {
	store FeedIdentityStore
	now   func() time.Time

	mu         sync.Mutex
	identities map[int64]cachedIdentity
	webhooks   map[string]cachedWebhook
	// sent remembers which webhook posted each recent message, so a feed that cleans up its own
	// previous batch can delete those through the webhook (which needs no extra permission)
	// instead of as the bot (which would need Manage Messages for a message it did not author).
	sent      map[string]cachedWebhook
	sentOrder []string

	viaWebhook atomic.Int64
	fallbacks  atomic.Int64
}

func NewFeedIdentity(store FeedIdentityStore) *FeedIdentity {
	return &FeedIdentity{store: store, now: time.Now, identities: map[int64]cachedIdentity{}, webhooks: map[string]cachedWebhook{}, sent: map[string]cachedWebhook{}}
}

// Invalidate forgets a server's cached identity so a settings change applies to the next message.
func (f *FeedIdentity) Invalidate(serverID int64) {
	if f == nil {
		return
	}
	f.mu.Lock()
	delete(f.identities, serverID)
	f.mu.Unlock()
}

// Stats reports messages sent under an identity and identity sends that fell back to the bot.
func (f *FeedIdentity) Stats() (viaWebhook, fallbacks int64) {
	if f == nil {
		return 0, 0
	}
	return f.viaWebhook.Load(), f.fallbacks.Load()
}

func (f *FeedIdentity) identity(serverID int64) (repository.FeedIdentitySettings, bool) {
	now := f.now()
	f.mu.Lock()
	if c, hit := f.identities[serverID]; hit && now.Sub(c.at) < feedIdentityTTL {
		f.mu.Unlock()
		return c.identity, c.ok
	}
	f.mu.Unlock()
	if f.store == nil {
		return repository.FeedIdentitySettings{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedIdentityLookupTime)
	defer cancel()
	identity, ok, err := f.store.FeedIdentityForServer(ctx, serverID)
	if err != nil {
		// Unknown is not cached: the next message asks again, and this one goes out as the bot.
		slog.Warn("component=feed_identity", "event", "lookup_failed", "server_id", serverID, "err", err.Error())
		return repository.FeedIdentitySettings{}, false
	}
	f.mu.Lock()
	f.identities[serverID] = cachedIdentity{identity: identity, ok: ok, at: now}
	f.mu.Unlock()
	return identity, ok
}

// webhook returns the Champion webhook of a channel, finding or creating it on first use.
func (f *FeedIdentity) webhook(api FeedIdentityAPI, channelID string) (cachedWebhook, bool) {
	now := f.now()
	f.mu.Lock()
	c, hit := f.webhooks[channelID]
	f.mu.Unlock()
	if hit {
		if c.token != "" {
			return c, true
		}
		if now.Sub(c.failedAt) < feedWebhookRetryAfter {
			return cachedWebhook{}, false
		}
	}
	found := cachedWebhook{}
	hooks, err := api.ChannelWebhooks(channelID)
	if err == nil {
		for _, h := range hooks {
			if h != nil && h.Name == feedWebhookName && h.Token != "" {
				found = cachedWebhook{id: h.ID, token: h.Token}
				break
			}
		}
		if found.token == "" {
			var created *discordgo.Webhook
			if created, err = api.WebhookCreate(channelID, feedWebhookName, ""); err == nil && created != nil && created.Token != "" {
				found = cachedWebhook{id: created.ID, token: created.Token}
			}
		}
	}
	if found.token == "" {
		reason := "no usable webhook"
		if err != nil {
			reason = err.Error()
		}
		slog.Warn("component=feed_identity", "event", "webhook_unavailable", "channel_id", channelID, "err", reason)
		found = cachedWebhook{failedAt: now}
	}
	f.mu.Lock()
	f.webhooks[channelID] = found
	f.mu.Unlock()
	return found, found.token != ""
}

// feedSentRemembered bounds the sent-message memory; feeds only ever delete their latest batch.
const feedSentRemembered = 2048

func (f *FeedIdentity) remember(messageID string, hook cachedWebhook) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sentOrder) >= feedSentRemembered {
		delete(f.sent, f.sentOrder[0])
		f.sentOrder = f.sentOrder[1:]
	}
	f.sent[messageID] = hook
	f.sentOrder = append(f.sentOrder, messageID)
}

// sentBy returns (and forgets) the webhook that posted messageID.
func (f *FeedIdentity) sentBy(messageID string) (cachedWebhook, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hook, ok := f.sent[messageID]
	if ok {
		delete(f.sent, messageID)
	}
	return hook, ok
}

func (f *FeedIdentity) forgetWebhook(channelID string) {
	f.mu.Lock()
	delete(f.webhooks, channelID)
	f.mu.Unlock()
}

// Sender wraps a feed's Discord API for one server. The result satisfies every feed sender
// interface (HitSender, rotatingFeedAPI). A nil FeedIdentity returns api unchanged.
func (f *FeedIdentity) Sender(api FeedIdentityAPI, serverID int64) FeedIdentityAPI {
	if f == nil || api == nil {
		return api
	}
	return &identitySender{FeedIdentityAPI: api, identities: f, serverID: serverID}
}

type identitySender struct {
	FeedIdentityAPI
	identities *FeedIdentity
	serverID   int64
}

// webhookCarries reports whether a message can be sent through a webhook as-is. Feed messages are
// embeds and text; anything with attachments, components or a reply stays with the bot.
func webhookCarries(data *discordgo.MessageSend) bool {
	return data != nil && len(data.Files) == 0 && len(data.Components) == 0 && data.Reference == nil && data.File == nil &&
		(len(data.Embeds) > 0 || data.Content != "")
}

func (s *identitySender) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	identity, ok := s.identities.identity(s.serverID)
	if !ok || !webhookCarries(data) {
		return s.FeedIdentityAPI.ChannelMessageSendComplex(channelID, data, options...)
	}
	hook, ok := s.identities.webhook(s.FeedIdentityAPI, channelID)
	if !ok {
		s.identities.fallbacks.Add(1)
		return s.FeedIdentityAPI.ChannelMessageSendComplex(channelID, data, options...)
	}
	mentions := data.AllowedMentions
	if mentions == nil {
		mentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	}
	msg, err := s.FeedIdentityAPI.WebhookExecute(hook.id, hook.token, true, &discordgo.WebhookParams{
		Content: data.Content, Username: identity.Name, AvatarURL: identity.AvatarURL, Embeds: data.Embeds, AllowedMentions: mentions,
	})
	if err != nil || msg == nil {
		// The webhook may have been deleted or the identity rejected; look it up afresh next time
		// and send this message as the bot so the feed never loses it.
		reason := "empty response"
		if err != nil {
			reason = err.Error()
		}
		slog.Warn("component=feed_identity", "event", "webhook_send_failed", "channel_id", channelID, "err", reason)
		s.identities.forgetWebhook(channelID)
		s.identities.fallbacks.Add(1)
		return s.FeedIdentityAPI.ChannelMessageSendComplex(channelID, data, options...)
	}
	s.identities.viaWebhook.Add(1)
	s.identities.remember(msg.ID, hook)
	return msg, nil
}

// ChannelMessagesBulkDelete deletes a feed's previous batch. Messages this sender posted through
// a webhook are deleted through that webhook; the rest go to the bot as before.
func (s *identitySender) ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error {
	var own []string
	var firstErr error
	for _, id := range messages {
		hook, viaWebhook := s.identities.sentBy(id)
		if !viaWebhook {
			own = append(own, id)
			continue
		}
		if err := s.FeedIdentityAPI.WebhookMessageDelete(hook.id, hook.token, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if len(own) > 0 {
		if err := s.FeedIdentityAPI.ChannelMessagesBulkDelete(channelID, own, options...); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
