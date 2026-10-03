package app

import (
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
)

// registerFeaturesCommand registers /features: turn the read-only features channel on or off
// (docs/FEATURES_CHANNEL.md).
func (a *App) registerFeaturesCommand(commands discord.CommandRegistrar) {
	if a.Guilds == nil || a.Discord == nil || a.Config.DiscordGuildID == "" {
		return
	}
	handler := discord.NewFeaturesCommandHandler(a.Guilds)
	if err := discord.RegisterFeaturesCommand(commands, a.Config.DiscordGuildID); err != nil {
		slog.Warn("component=discord", "msg", "failed to register features command", "err", err.Error())
	} else {
		slog.Info("component=discord", "msg", "features command queued")
	}
	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "features" {
			handler.Handle(s, i)
		}
	})
}
