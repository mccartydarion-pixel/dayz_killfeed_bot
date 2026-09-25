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
		return ErrCaseStaffChannelUnsafe
	}
	channel,err:=c.session.Channel(channelID)
	if err!=nil{return fmt.Errorf("%w: channel lookup: %v",ErrCaseStaffChannelUnsafe,err)}
	if channel==nil || channel.GuildID!=guildID{return ErrCaseStaffChannelUnsafe}
	guild,err:=c.session.Guild(guildID)
	if err!=nil{return fmt.Errorf("%w: guild lookup: %v",ErrCaseStaffChannelUnsafe,err)}
	// A private parent is not sufficient: the child must itself explicitly
	// deny @everyone view, preventing unsynced category permissions.
	var parent *discordgo.Channel
	botID:=c.BotID()
	if botID=="" {return ErrCaseStaffChannelUnsafe}
	member,err:=c.session.GuildMember(guildID,"@me")
	if err!=nil{return fmt.Errorf("%w: bot membership: %v",ErrCaseStaffChannelUnsafe,err)}
	if member==nil{return ErrCaseStaffChannelUnsafe}
	return validateCaseStaffChannel(guild,channel,parent,botID,member.Roles)
}
