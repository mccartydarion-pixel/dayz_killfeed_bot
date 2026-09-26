package discord

import (
	"context"
	"errors"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

var ErrCaseStaffChannelUnsafe = errors.New("C.A.S.E. staff destination is not verified private")

// Do not include bot tokens, member lists or raw API response bodies in QA
// diagnostics. The reason names only the failed policy check.
func caseStaffUnsafe(reason string) error {
	return fmt.Errorf("%w: %s", ErrCaseStaffChannelUnsafe, reason)
}

// validateCaseStaffChannel is a conservative privacy gate, not a claim that
// every human granted a staff role has been audited. It refuses public
// @everyone visibility and any role-specific view grant unless that role
// has elevated guild-management permissions. A bot member allow is safe.
func validateCaseStaffChannel(guild *discordgo.Guild, channel, parent *discordgo.Channel,
	botID string, botRoles []string) error {
	if guild==nil || channel==nil || guild.ID=="" || channel.GuildID!=guild.ID ||
		channel.Type!=discordgo.ChannelTypeGuildText || botID=="" {
		return caseStaffUnsafe("target must be a text channel in the expected guild, and bot identity must be available")
	}
	// Require explicit target-channel overrides. Category naming or an
	// inherited-looking parent is not enough to prove the child's effective
	// @everyone permissions when Discord settings are unsynchronized.
	_ = parent
	overwrites:=channel.PermissionOverwrites
	if len(overwrites)==0{return caseStaffUnsafe("target channel has no explicit permission overrides; configure the channel itself, not just its category")}
	roles:=map[string]int64{}
	for _,role:=range guild.Roles {if role!=nil {roles[role.ID]=role.Permissions}}
	if roles[guild.ID]&discordgo.PermissionAdministrator!=0{return caseStaffUnsafe("@everyone has Administrator at the guild level")}
	everyoneDenied:=false
	for _,ow:=range overwrites {
		if ow==nil {continue}
		if ow.Type==discordgo.PermissionOverwriteTypeRole && ow.ID==guild.ID {
			if ow.Allow&discordgo.PermissionViewChannel!=0 {return caseStaffUnsafe("@everyone explicitly allows View Channel")}
			if ow.Deny&discordgo.PermissionViewChannel!=0 {everyoneDenied=true}
			continue
		}
		if ow.Type==discordgo.PermissionOverwriteTypeRole &&
			ow.Allow&discordgo.PermissionViewChannel!=0 {
			perms,found:=roles[ow.ID]
			if !found || perms&(discordgo.PermissionAdministrator|discordgo.PermissionManageGuild)==0 {
				return caseStaffUnsafe("a non-management role is allowed to View Channel; limit access to reviewed staff roles")
			}
		}
		if ow.Type==discordgo.PermissionOverwriteTypeMember && ow.ID==botID &&
			ow.Deny&(discordgo.PermissionViewChannel|discordgo.PermissionSendMessages|discordgo.PermissionEmbedLinks|discordgo.PermissionReadMessageHistory)!=0 {
			return caseStaffUnsafe("QA bot has an explicit deny for a required channel permission")
		}
		if ow.Type==discordgo.PermissionOverwriteTypeMember &&
			ow.Allow&discordgo.PermissionViewChannel!=0 && ow.ID!=botID &&
			ow.ID!=guild.OwnerID {
			return caseStaffUnsafe("a non-owner, non-bot member has an explicit View Channel allow")
		}
	}
	if !everyoneDenied {return caseStaffUnsafe("target channel must explicitly deny @everyone View Channel")}
	effective:=*channel
	effective.PermissionOverwrites=overwrites
	botPerms:=memberChannelPermissions(guild,&effective,botID,botRoles)
	if botPerms&discordgo.PermissionViewChannel==0 {return caseStaffUnsafe("QA bot lacks effective View Channel permission")}
	if botPerms&discordgo.PermissionSendMessages==0 {return caseStaffUnsafe("QA bot lacks effective Send Messages permission")}
	if botPerms&discordgo.PermissionEmbedLinks==0 {return caseStaffUnsafe("QA bot lacks effective Embed Links permission")}
	if botPerms&discordgo.PermissionReadMessageHistory==0 {return caseStaffUnsafe("QA bot lacks effective Read Message History permission")}
	return nil
}

// VerifyCaseStaffChannel fetches current Discord permissions rather than
// trusting a potentially stale route cache or a channel name. It rejects
// cross-guild destinations, public @everyone channels and broad role allows.
func (c *Client) VerifyCaseStaffChannel(ctx context.Context,guildID,channelID string) error {
	if c==nil || c.session==nil || guildID=="" || channelID=="" || ctx.Err()!=nil {
		return caseStaffUnsafe("Discord client, guild, channel or request context unavailable")
	}
	channel,err:=c.session.Channel(channelID)
	if err!=nil{return caseStaffUnsafe("Discord channel lookup failed; confirm bot membership and View Channel access")}
	if channel==nil || channel.GuildID!=guildID{return caseStaffUnsafe("channel is missing or does not belong to the expected guild")}
	guild,err:=c.session.Guild(guildID)
	if err!=nil{return caseStaffUnsafe("Discord guild lookup failed; confirm the QA bot was installed in this guild")}
	// A private parent is not sufficient: the child must itself explicitly
	// deny @everyone view, preventing unsynced category permissions.
	var parent *discordgo.Channel
	botID:=c.BotID()
	if botID=="" {return caseStaffUnsafe("QA bot identity is unavailable")}
	// GET /guilds/{guild.id}/members/{user.id} requires the actual bot
	// user snowflake. @me is supported by other Discord routes, not this GET.
	member,err:=c.session.GuildMember(guildID,botID)
	if err!=nil{return caseStaffUnsafe("Discord could not read QA bot guild membership; confirm installation and permissions")}
	if member==nil{return caseStaffUnsafe("QA bot is not a guild member")}
	return validateCaseStaffChannel(guild,channel,parent,botID,member.Roles)
}

// VerifyCaseSyntheticQAChannel is exclusively for cmd/case-discord-qa's
// synthetic, zero-player-data transport probe. Unlike the production staff
// verifier, it does not audit individual human channel grants. In particular,
// an operator's direct View Channel grant is not a reason to block a test.
// Never use this method to authorize a real Watch digest or evidence delivery.
func (c *Client) VerifyCaseSyntheticQAChannel(ctx context.Context, guildID, channelID string) error {
	if c==nil || c.session==nil || guildID=="" || channelID=="" || ctx.Err()!=nil {
		return caseStaffUnsafe("QA client or destination unavailable")
	}
	channel,err:=c.session.Channel(channelID)
	if err!=nil{return caseStaffUnsafe("QA bot cannot fetch the target channel; check View Channel permission")}
	if channel==nil || channel.GuildID!=guildID ||
		channel.Type!=discordgo.ChannelTypeGuildText || channel.Name!="case-qa" {
		return caseStaffUnsafe("synthetic probe requires the exact case-qa text channel in the declared guild")
	}
	guild,err:=c.session.Guild(guildID)
	if err!=nil || guild==nil{return caseStaffUnsafe("QA bot cannot fetch the declared guild")}
	botID:=c.BotID()
	if botID==""{return caseStaffUnsafe("QA bot identity unavailable")}
	member,err:=c.session.GuildMember(guildID,botID)
	if err!=nil || member==nil{return caseStaffUnsafe("QA bot guild membership could not be verified")}
	botPerms:=memberChannelPermissions(guild,channel,botID,member.Roles)
	if botPerms&discordgo.PermissionViewChannel==0{return caseStaffUnsafe("QA bot lacks View Channel")}
	if botPerms&discordgo.PermissionSendMessages==0{return caseStaffUnsafe("QA bot lacks Send Messages")}
	if botPerms&discordgo.PermissionEmbedLinks==0{return caseStaffUnsafe("QA bot lacks Embed Links")}
	if botPerms&discordgo.PermissionReadMessageHistory==0{return caseStaffUnsafe("QA bot lacks Read Message History")}
	return nil
}
