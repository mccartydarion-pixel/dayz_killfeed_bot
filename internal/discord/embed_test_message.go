package discord

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// EmbedTestMessage wraps an Embed Designer test render for sending: a plain notice
// ABOVE the embed saying this is a design preview, and the rendered embed itself,
// untouched (it must stay identical to the preview). Mentions are explicitly
// disabled - no users, roles, @everyone or @here are parsed and no reply ping is
// made - so a hostile sample value can never ping anyone.
func EmbedTestMessage(routeLabel string, embed *discordgo.MessageEmbed) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content: fmt.Sprintf("🧪 Champion Embed Test • %s\nThis is a design preview — not a live server event.", routeLabel),
		Embeds:  []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse:       []discordgo.AllowedMentionType{},
			Users:       []string{},
			Roles:       []string{},
			RepliedUser: false,
		},
	}
}

// SendMessage posts msg to channelID and returns the new message's ID.
func (c *Client) SendMessage(channelID string, msg *discordgo.MessageSend) (string, error) {
	if c == nil || c.session == nil {
		return "", fmt.Errorf("discord session not initialized")
	}
	m, err := c.session.ChannelMessageSendComplex(channelID, msg)
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	return m.ID, nil
}
