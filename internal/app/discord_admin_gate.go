package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// championAdminResolver is the slash-command admin gate's look-up (internal/discord
// admin_gate.go): whether any of a member's Discord roles maps to the Champion ADMINISTRATOR or
// OWNER level on the installation a command targets. A Discord server holds one installation per
// DayZ server, so the level is read for gameServerID; a command that names no server (0) is
// gated by the installation of the guild's selected server, and when none is selected by every
// installation in the guild. A failed look-up refuses.
func (a *App) championAdminResolver() discord.ChampionAdminResolver {
	if a.Permissions == nil {
		return nil
	}
	return func(discordGuildID string, roleIDs []string, gameServerID int64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		rows, err := a.Permissions.InstallationLevelsForGuildRoles(ctx, discordGuildID, roleIDs)
		if err != nil {
			slog.Warn("component=discord", "event", "champion_admin_lookup_failed", "err", err.Error())
			return false
		}
		if gameServerID == 0 && a.Guilds != nil {
			if guild, _, err := a.Guilds.GetGuild(ctx, discordGuildID); err == nil && guild != nil {
				gameServerID = guild.SelectedPublicServerID
			}
		}
		return installationAdmin(rows, gameServerID)
	}
}

// installationAdmin reports whether rows (InstallationLevelsForGuildRoles) grant ADMINISTRATOR or
// above on the installation of gameServerID, or, with 0, on every installation listed.
func installationAdmin(rows []repository.InstallationRoleLevel, gameServerID int64) bool {
	if len(rows) == 0 {
		return false
	}
	best := map[int64]permissions.Level{}
	for _, row := range rows {
		level, _ := permissions.ParseLevel(row.Level)
		if cur, seen := best[row.InstallationID]; !seen || level > cur {
			best[row.InstallationID] = level
		}
	}
	if gameServerID > 0 {
		for _, row := range rows {
			if row.GameServerID == gameServerID {
				return best[row.InstallationID] >= permissions.LevelAdministrator
			}
		}
		return false
	}
	for _, level := range best {
		if level < permissions.LevelAdministrator {
			return false
		}
	}
	return true
}
