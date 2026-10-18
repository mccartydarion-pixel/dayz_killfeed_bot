package discord

import (
	"sync/atomic"

	"github.com/bwmarrin/discordgo"
)

// Who may run the admin slash commands (/setup, /server, /admin, /features and the admin
// subcommands of /season, /economy, /tournament, /war, /welcome and the competitive commands):
//
//   - a member with Discord's Administrator or Manage Server permission, as always; or
//   - a member whose Discord roles the installation's owner mapped to the Champion
//     ADMINISTRATOR (or OWNER) level on the website's Staff page. An administrator runs the
//     server exactly as the owner does, in Discord as on the website.
//
// The commands carry no Discord default-member permission: Discord would otherwise hide them from
// a role-mapped administrator who lacks Manage Server, and nothing but the owner editing the
// server's integration settings could show them again. The gate below decides instead, and a
// member it refuses gets a private "not allowed" reply.

// ChampionAdminResolver reports whether any of a member's Discord roles maps to the Champion
// ADMINISTRATOR or OWNER level on the installation for discordGuildID's game server gameServerID
// (0: the command names no server, so the guild's selected one). It is called on the interaction
// goroutine, so it must be quick and must never block on Discord.
type ChampionAdminResolver func(discordGuildID string, roleIDs []string, gameServerID int64) bool

var championAdminResolver atomic.Pointer[ChampionAdminResolver]

// SetChampionAdminResolver installs (or, with nil, removes) the role-mapping look-up the admin
// gate consults after Discord's own permissions say no.
func SetChampionAdminResolver(f ChampionAdminResolver) {
	if f == nil {
		championAdminResolver.Store(nil)
		return
	}
	championAdminResolver.Store(&f)
}

// championAdminMember reports whether the interaction's member holds a mapped ADMINISTRATOR or
// OWNER role on the guild's selected server. Without a resolver, a guild or roles it is false.
func championAdminMember(i *discordgo.InteractionCreate) bool {
	return championAdminMemberFor(i, 0)
}

// championAdminMemberFor is championAdminMember for the installation of game server gameServerID.
func championAdminMemberFor(i *discordgo.InteractionCreate, gameServerID int64) bool {
	if i == nil || i.Member == nil || i.GuildID == "" || len(i.Member.Roles) == 0 {
		return false
	}
	f := championAdminResolver.Load()
	if f == nil {
		return false
	}
	return (*f)(i.GuildID, i.Member.Roles, gameServerID)
}

// isAdminForServer is isAdminInteraction for a command that targets game server gameServerID: a
// Discord administrator always, a role-mapped administrator only when mapped on that server's
// installation rather than another installation in the same Discord server.
func isAdminForServer(i *discordgo.InteractionCreate, gameServerID int64) bool {
	if i == nil || i.Member == nil {
		return false
	}
	if i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer) != 0 {
		return true
	}
	return championAdminMemberFor(i, gameServerID)
}
