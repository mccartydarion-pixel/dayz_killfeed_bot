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
