package discord

import (
	"encoding/json"

	"github.com/bwmarrin/discordgo"
)

// nonceSender is the optional idempotent send RotatingFeed prefers: Discord's
// message nonce with enforce_nonce makes a retried create (after a timeout or
// a 5xx where the first request may have succeeded) return the already
// created message instead of posting a duplicate card. discordgo v0.29's
// MessageSend has no nonce field, so FeedSession sends it itself.
type nonceSender interface {
	ChannelMessageSendNonce(channelID string, data *discordgo.MessageSend, nonce string) (*discordgo.Message, error)
}

// FeedSession adapts a *discordgo.Session for RotatingFeed, adding the
// nonce-enforced send. All other calls pass straight through.
type FeedSession struct{ S *discordgo.Session }

// NewFeedSession wraps s for RotatingFeed.
func NewFeedSession(s *discordgo.Session) *FeedSession { return &FeedSession{S: s} }

func (f *FeedSession) ChannelMessagesBulkDelete(channelID string, messages []string, options ...discordgo.RequestOption) error {
	return f.S.ChannelMessagesBulkDelete(channelID, messages, options...)
}

func (f *FeedSession) ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error {
	return f.S.ChannelMessageDelete(channelID, messageID, options...)
}

func (f *FeedSession) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	return f.S.ChannelMessageSendComplex(channelID, data, options...)
}

// ChannelMessageSendNonce creates a message with nonce + enforce_nonce. Feed
// cards carry embeds only (no files), so the JSON body is the whole request.
// It uses the session's own request path, so rate limits and buckets behave
// exactly like ChannelMessageSendComplex.
func (f *FeedSession) ChannelMessageSendNonce(channelID string, data *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
	for _, embed := range data.Embeds {
		if embed.Type == "" {
			embed.Type = "rich"
		}
	}
	body := struct {
		*discordgo.MessageSend
		Nonce        string `json:"nonce"`
		EnforceNonce bool   `json:"enforce_nonce"`
	}{data, nonce, true}
	endpoint := discordgo.EndpointChannelMessages(channelID)
	raw, err := f.S.RequestWithBucketID("POST", endpoint, body, endpoint)
	if err != nil {
		return nil, err
	}
	var msg discordgo.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
