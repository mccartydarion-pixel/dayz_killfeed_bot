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
