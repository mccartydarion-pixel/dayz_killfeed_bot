package discord

import (
	"fmt"
	"sort"

	"github.com/bwmarrin/discordgo"
)

// selectableChannelTypes are the only Discord channel types Champion setup
// exposes as selectable targets (SaaS Step 5, section 2): plain text
// channels and announcement channels, since only those can receive the
// bot's killfeed/leaderboard/status/admin-log messages. Voice, stage,
// category, forum, and thread channels are never selectable.
var selectableChannelTypes = map[discordgo.ChannelType]bool{
	discordgo.ChannelTypeGuildText: true,
	discordgo.ChannelTypeGuildNews: true,
}

// GuildChannelInfo is one text-capable channel discovered for a guild,
// annotated with the bot's own access to it. internal/app/saas_api_channels.go
// builds its customer-facing DiscordChannelSummary DTO from this - never the
// raw *discordgo.Channel, whose PermissionOverwrites and other internals
// stay inside this package.
type GuildChannelInfo struct {
	ID       string
	Name     string
	Type     discordgo.ChannelType
	Position int
	CanSend  bool
}

// ListGuildChannels returns every text-capable channel of guildID that the
// bot can currently view, sorted by position then name (SaaS Step 5,
// sections 4/5). It prefers the local gateway state cache - zero network
// calls - for both the channel list and the bot's own guild membership, and
// falls back to Discord REST only for whichever piece the cache is missing:
// at most one guild fetch, one channel-list fetch, and one self-member
// fetch, never one REST call per channel.
func (c *Client) ListGuildChannels(guildID string) ([]GuildChannelInfo, error) {
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

	botID := c.BotID()
	memberRoles, err := c.botGuildRoles(guildID, botID)
	if err != nil {
		return nil, fmt.Errorf("resolve bot guild membership: %w", err)
	}

	out := make([]GuildChannelInfo, 0, len(channels))
	for _, ch := range channels {
		if ch == nil || !selectableChannelTypes[ch.Type] {
			continue
		}
		perms := memberChannelPermissions(guild, ch, botID, memberRoles)
		if perms&discordgo.PermissionViewChannel == 0 {
			// Never surface a channel Champion cannot even see (section 5).
			continue
		}
		out = append(out, GuildChannelInfo{
			ID:       ch.ID,
			Name:     ch.Name,
			Type:     ch.Type,
			Position: ch.Position,
			CanSend:  perms&discordgo.PermissionSendMessages != 0,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// resolveGuild returns guildID's guild struct, preferring the local state
// cache and falling back to a single REST fetch on a cache miss.
func (c *Client) resolveGuild(guildID string) (*discordgo.Guild, error) {
	if guild, err := c.session.State.Guild(guildID); err == nil && guild != nil {
		return guild, nil
	}
	return c.session.Guild(guildID)
}

// resolveGuildChannels returns guild's channels, preferring the already-
// cached Channels field (populated when guild came from state) and falling
// back to a single GuildChannels REST call when it's nil (guild came from
// the REST guild fetch instead, whose response never includes channels).
func (c *Client) resolveGuildChannels(guildID string, guild *discordgo.Guild) ([]*discordgo.Channel, error) {
	if guild.Channels != nil {
		return guild.Channels, nil
	}
	return c.session.GuildChannels(guildID)
}

// botGuildRoles returns the bot's own role IDs within guildID, preferring
// the local state cache (populated automatically when the GuildMembers
// intent is enabled - see discord.New) and falling back to a single
// self-member REST fetch otherwise. The fetched member is also seeded back
// into state (best effort) so later calls in the same process hit the cache.
func (c *Client) botGuildRoles(guildID, botID string) ([]string, error) {
	if member, err := c.session.State.Member(guildID, botID); err == nil && member != nil {
		return member.Roles, nil
	}
	member, err := c.session.GuildMember(guildID, "@me")
	if err != nil {
		return nil, fmt.Errorf("fetch bot guild member: %w", err)
	}
	_ = c.session.State.MemberAdd(member)
	return member.Roles, nil
}

// MemberRoles returns an arbitrary member's role IDs within guildID (Client Admin Control Plane
// Phase 1: resolving a website actor's effective Champion permission level from their live
// Discord roles). Same state-cache-first, single-REST-fallback shape as botGuildRoles, just
// generalized to any userID rather than the bot's own id - a member who has left the guild, or
// who Discord otherwise can't resolve, is reported as a normal error (never a panic or a silent
// empty-roles result that could be mistaken for "no permissions" vs. "lookup failed").
func (c *Client) MemberRoles(guildID, userID string) ([]string, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("discord session not initialized")
	}
	if guildID == "" || userID == "" {
		return nil, fmt.Errorf("guild id and user id are required")
	}
	if member, err := c.session.State.Member(guildID, userID); err == nil && member != nil {
		return member.Roles, nil
	}
	member, err := c.session.GuildMember(guildID, userID)
	if err != nil {
		return nil, fmt.Errorf("fetch guild member: %w", err)
	}
	_ = c.session.State.MemberAdd(member)
	return member.Roles, nil
}

// memberChannelPermissions ports discordgo's own permission calculation
// (restapi.go's unexported memberPermissions, mirrored here field-for-field)
// so it can be computed from a guild/channel/role list already in hand -
// this works even when the channel came from a REST GuildChannels fallback
// rather than the state cache (state.Channel would not have it, but this
// function never needs state.Channel at all). Mirrors Discord's documented
// permission hierarchy: base role permissions, then the @everyone channel
// overwrite, then role overwrites, then the member-specific overwrite.
// https://support.discord.com/hc/en-us/articles/206141927
func memberChannelPermissions(guild *discordgo.Guild, channel *discordgo.Channel, memberID string, memberRoles []string) int64 {
	if memberID != "" && memberID == guild.OwnerID {
		return discordgo.PermissionAll
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

	for _, ow := range channel.PermissionOverwrites {
		if ow.ID == guild.ID {
			perms &^= ow.Deny
			perms |= ow.Allow
			break
		}
	}

	var denies, allows int64
	for _, ow := range channel.PermissionOverwrites {
		for _, roleID := range memberRoles {
			if ow.Type == discordgo.PermissionOverwriteTypeRole && ow.ID == roleID {
				denies |= ow.Deny
				allows |= ow.Allow
				break
			}
		}
	}
	perms &^= denies
	perms |= allows

	for _, ow := range channel.PermissionOverwrites {
		if ow.Type == discordgo.PermissionOverwriteTypeMember && ow.ID == memberID {
			perms &^= ow.Deny
			perms |= ow.Allow
			break
		}
	}

	if perms&discordgo.PermissionAdministrator == discordgo.PermissionAdministrator {
		perms |= discordgo.PermissionAllChannel
	}
	return perms
}
