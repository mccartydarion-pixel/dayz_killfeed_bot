package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// championAdminResolver is the slash-command admin gate's look-up (internal/discord
// admin_gate.go): whether any of a member's Discord roles maps to the Champion ADMINISTRATOR or
// OWNER level in the installation for that Discord server. A failed look-up refuses.
func (a *App) championAdminResolver() discord.ChampionAdminResolver {
	if a.Permissions == nil {
		return nil
	}
	return func(discordGuildID string, roleIDs []string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		levels, err := a.Permissions.LevelsForGuildRoles(ctx, discordGuildID, roleIDs)
		if err != nil {
			slog.Warn("component=discord", "event", "champion_admin_lookup_failed", "err", err.Error())
			return false
		}
		for _, name := range levels {
			if l, ok := permissions.ParseLevel(name); ok && l >= permissions.LevelAdministrator {
				return true
			}
		}
		return false
	}
}
