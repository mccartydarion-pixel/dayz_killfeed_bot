package discord

import "github.com/bwmarrin/discordgo"

// SessionAPI adapts *discordgo.Session to the narrow GuildAPI and MessageEditor
// interfaces used by the setup manager and persistent panels. discordgo methods
// accept variadic RequestOption args, which do not match a plain interface, so
// this adapter pins them to the exact signatures we need (and can fake in tests).
type SessionAPI struct {
	S *discordgo.Session
}

// NewSessionAPI wraps a live session.
func NewSessionAPI(s *discordgo.Session) *SessionAPI { return &SessionAPI{S: s} }

func (a *SessionAPI) GuildChannels(guildID string) ([]*discordgo.Channel, error) {
	return a.S.GuildChannels(guildID)
}

func (a *SessionAPI) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	return a.S.GuildChannelCreateComplex(guildID, data)
}

func (a *SessionAPI) ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return a.S.ChannelMessageSendEmbed(channelID, embed)
}

func (a *SessionAPI) ChannelMessageEditEmbed(channelID, messageID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return a.S.ChannelMessageEditEmbed(channelID, messageID, embed)
}

func (a *SessionAPI) ChannelMessageSendContent(channelID, content string) (*discordgo.Message, error) {
	return a.S.ChannelMessageSend(channelID, content)
}

func (a *SessionAPI) ChannelMessageEditContent(channelID, messageID, content string) (*discordgo.Message, error) {
	return a.S.ChannelMessageEdit(channelID, messageID, content)
}

func (a *SessionAPI) ChannelMessage(channelID, messageID string) (*discordgo.Message, error) {
	return a.S.ChannelMessage(channelID, messageID)
}

func (a *SessionAPI) Channel(channelID string) (*discordgo.Channel, error) {
	return a.S.Channel(channelID)
}

// ChannelEdit renames or edits a channel (used by the voice counter).
func (a *SessionAPI) ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	return a.S.ChannelEditComplex(channelID, data)
}

// ChannelRename renames a channel without discordgo's built-in 429 sleep: a
// rate limit comes back as *discordgo.RateLimitError so the voice counter can
// schedule its own retry instead of blocking (see VoiceChannelRenamer).
func (a *SessionAPI) ChannelRename(channelID, name string) (*discordgo.Channel, error) {
	return a.S.ChannelEditComplex(channelID, &discordgo.ChannelEdit{Name: name}, discordgo.WithRetryOnRatelimit(false))
}

// ChannelMessageSendComplex sends a message with an embed and components
// (buttons), used by the public interactive panels.
func (a *SessionAPI) ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	return a.S.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: components})
}

// ChannelMessageEditComplex edits an existing panel message's embed/components.
func (a *SessionAPI) ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	edit := discordgo.NewMessageEdit(channelID, messageID)
	edit.Embeds = &[]*discordgo.MessageEmbed{embed}
	edit.Components = &components
	return a.S.ChannelMessageEditComplex(edit)
}

// ChannelMessageSendEmbeds sends ONE message carrying every embed (the Auto
// Leaderboard package), so the categories appear and refresh together.
func (a *SessionAPI) ChannelMessageSendEmbeds(channelID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	return a.S.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: embeds, Components: components})
}

// ChannelMessageEditEmbeds replaces a message's whole embed array in one edit.
// Components are only touched when given.
func (a *SessionAPI) ChannelMessageEditEmbeds(channelID, messageID string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	edit := discordgo.NewMessageEdit(channelID, messageID)
	edit.Embeds = &embeds
	if components != nil {
		edit.Components = &components
	}
	return a.S.ChannelMessageEditComplex(edit)
}

// ChannelMessages lists up to limit recent messages in a channel (newest
// first). Used only to find the bot's own obsolete leaderboard boards.
func (a *SessionAPI) ChannelMessages(channelID string, limit int) ([]*discordgo.Message, error) {
	return a.S.ChannelMessages(channelID, limit, "", "", "")
}

// BotUserID is the connected bot's own user id ("" before the gateway is ready).
func (a *SessionAPI) BotUserID() string {
	if a == nil || a.S == nil || a.S.State == nil || a.S.State.User == nil {
		return ""
	}
	return a.S.State.User.ID
}

// ChannelMessageDelete removes a message (used to retire a superseded panel).
func (a *SessionAPI) ChannelMessageDelete(channelID, messageID string) error {
	return a.S.ChannelMessageDelete(channelID, messageID)
}

// The Auto Leaderboard depends on these at runtime (type assertions), so fail
// the build - not a refresh - if SessionAPI ever loses them.
var (
	_ MultiEmbedMessageAPI = (*SessionAPI)(nil)
	_ MessageEditor        = (*SessionAPI)(nil)
	_ RoutePanelAPI        = (*SessionAPI)(nil)
	_ ChannelHistoryAPI    = (*SessionAPI)(nil)
)
