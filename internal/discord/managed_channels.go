package discord

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// RawGuildChannel is an unfiltered channel or category discovered in a
// guild - used only for the SaaS Step 5 one-click auto-setup's own
// category/name matching (internal/app/saas_api_channels.go), never
// returned to a customer directly. See GuildChannelInfo/ListGuildChannels
// for the customer-facing, permission-filtered equivalent.
type RawGuildChannel struct {
	ID       string
	Name     string
	Type     discordgo.ChannelType
	ParentID string
}

// ListAllGuildChannels returns every channel of guildID, including
// categories, unfiltered by the bot's view permission - the raw list
// auto-setup's category/name-based recovery matching needs (section 4).
// Same state-cache-first, single-REST-fallback sourcing as
// ListGuildChannels.
func (c *Client) ListAllGuildChannels(guildID string) ([]RawGuildChannel, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("discord session not initialized")
	}
	if guildID == "" {
		return nil, fmt.Errorf("guild id is required")
	}

	guild, err := c.resolveGuild(guildID)
	if err != nil {
		return nil, fmt.Errorf("fetch guild: %w", err)
	}
	channels, err := c.resolveGuildChannels(guildID, guild)
	if err != nil {
		return nil, fmt.Errorf("fetch guild channels: %w", err)
	}

	out := make([]RawGuildChannel, 0, len(channels))
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		out = append(out, RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID})
	}
	return out, nil
}

// GuildPermissions returns the bot's own base role permissions in guildID -
// no channel overwrites, since this gates guild-level actions like category/
// channel creation where no target channel exists yet. State-cache-first,
// single-REST-fallback, same as ListGuildChannels.
func (c *Client) GuildPermissions(guildID string) (int64, error) {
	if c == nil || c.session == nil {
		return 0, fmt.Errorf("discord session not initialized")
	}
	if guildID == "" {
		return 0, fmt.Errorf("guild id is required")
	}

	guild, err := c.resolveGuild(guildID)
	if err != nil {
		return 0, fmt.Errorf("fetch guild: %w", err)
	}
	botID := c.BotID()
	if botID != "" && botID == guild.OwnerID {
		return discordgo.PermissionAll, nil
	}
	memberRoles, err := c.botGuildRoles(guildID, botID)
	if err != nil {
		return 0, fmt.Errorf("resolve bot guild membership: %w", err)
	}

	var perms int64
	for _, role := range guild.Roles {
		if role.ID == guild.ID {
			perms |= role.Permissions
			break
		}
	}
	for _, role := range guild.Roles {
		for _, roleID := range memberRoles {
			if role.ID == roleID {
				perms |= role.Permissions
				break
			}
		}
	}
	if perms&discordgo.PermissionAdministrator == discordgo.PermissionAdministrator {
		perms |= discordgo.PermissionAll
	}
	return perms, nil
}

// CreateGuildCategory creates a new category channel named name in guildID -
// the "CHAMPION KILLFEED" default category (section 1). Callers must verify
// PermissionManageChannels via GuildPermissions first; Discord's own error
// on a permission failure here is never surfaced raw to the customer (see
// saas_api_channels.go).
func (c *Client) CreateGuildCategory(guildID, name string) (*RawGuildChannel, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("discord session not initialized")
	}
	ch, err := c.session.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name: name,
		Type: discordgo.ChannelTypeGuildCategory,
	})
	if err != nil {
		return nil, fmt.Errorf("create guild category: %w", err)
	}
	return &RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID}, nil
}

// CreateGuildTextChannel creates a new text channel named name in guildID,
// optionally nested under parentCategoryID (empty string means no parent).
func (c *Client) CreateGuildTextChannel(guildID, name, parentCategoryID string) (*RawGuildChannel, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("discord session not initialized")
	}
	ch, err := c.session.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:     name,
		Type:     discordgo.ChannelTypeGuildText,
		ParentID: parentCategoryID,
	})
	if err != nil {
		return nil, fmt.Errorf("create guild text channel: %w", err)
	}
	return &RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID}, nil
}

// CreatePrivateGuildCategory creates a category hidden from @everyone and
// visible to the bot itself - the staff category (admin logs). Text channels
// created under it without their own overwrites inherit this privacy.
func (c *Client) CreatePrivateGuildCategory(guildID, name string) (*RawGuildChannel, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("discord session not initialized")
	}
	overwrites := []*discordgo.PermissionOverwrite{
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole, Deny: discordgo.PermissionViewChannel},
	}
	if botID := c.BotID(); botID != "" {
		overwrites = append(overwrites, &discordgo.PermissionOverwrite{
			ID:    botID,
			Type:  discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks | discordgo.PermissionReadMessageHistory,
		})
	}
	ch, err := c.session.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 name,
		Type:                 discordgo.ChannelTypeGuildCategory,
		PermissionOverwrites: overwrites,
	})
	if err != nil {
		return nil, fmt.Errorf("create private guild category: %w", err)
	}
	return &RawGuildChannel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID}, nil
}

// SendChannelEmbed posts one embed to channelID - used for a managed
// channel's single starter card.
func (c *Client) SendChannelEmbed(channelID string, embed *discordgo.MessageEmbed) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("discord session not initialized")
	}
	if _, err := c.session.ChannelMessageSendEmbed(channelID, embed); err != nil {
		return fmt.Errorf("send channel embed: %w", err)
	}
	return nil
}

// botMessageScanLimit bounds how many recent messages ChannelHasBotMessage
// reads: enough to find a panel or starter card in a quiet channel without
// paging through a busy feed.
const botMessageScanLimit = 50

// ChannelHasBotMessage reports whether any of channelID's most recent
// messages was posted by the bot - the "visible Champion content" check.
func (c *Client) ChannelHasBotMessage(channelID string) (bool, error) {
	if c == nil || c.session == nil {
		return false, fmt.Errorf("discord session not initialized")
	}
	botID := c.BotID()
	if botID == "" {
		return false, fmt.Errorf("bot identity unavailable")
	}
	msgs, err := c.session.ChannelMessages(channelID, botMessageScanLimit, "", "", "")
	if err != nil {
		return false, fmt.Errorf("read channel messages: %w", err)
	}
	for _, m := range msgs {
		if m != nil && m.Author != nil && m.Author.ID == botID {
			return true, nil
		}
	}
	return false, nil
}
