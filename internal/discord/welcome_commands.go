package discord

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type WelcomeCommandHandler struct {
	repo   *repository.WelcomeRepository
	guilds GuildStore
}

func NewWelcomeCommandHandler(r *repository.WelcomeRepository, g GuildStore, _ SetupStore) *WelcomeCommandHandler {
	return &WelcomeCommandHandler{repo: r, guilds: g}
}
func RegisterWelcomeCommands(s *discordgo.Session, guildID string) error {
	appID, err := ApplicationID(s)
	if err != nil {
		return err
	}
	cmd := &discordgo.ApplicationCommand{Name: "welcome", Description: "Configure Champion welcomes", Options: []*discordgo.ApplicationCommandOption{
		{Name: "status", Description: "Show welcomer status", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "enable", Description: "Enable welcomes", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "disable", Description: "Disable welcomes", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "preview", Description: "Preview welcome", Type: discordgo.ApplicationCommandOptionSubCommand},
		{Name: "test", Description: "Send test welcome", Type: discordgo.ApplicationCommandOptionSubCommand},
	}}
	_, err = s.ApplicationCommandCreate(appID, guildID, cmd)
	return err
}
func (h *WelcomeCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.repo == nil || h.guilds == nil || i == nil {
		respondEphemeral(s, i, "Welcomer is unavailable.")
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, "Run `/setup run` first.")
		return
	}
	cfg, err := h.repo.Get(context.Background(), gid)
	if err != nil {
		respondEphemeral(s, i, "Run `/setup run` first.")
		return
	}
	if len(i.ApplicationCommandData().Options) == 0 {
		return
	}
	name := i.ApplicationCommandData().Options[0].Name
	if name != "status" && !isAdminInteraction(i) {
		respondEphemeral(s, i, "Administrator or Manage Server permission required.")
		return
	}
	if name == "status" {
		state := "🔴 Disabled"
		if cfg.Enabled {
			state = "🟢 Enabled"
		}
		respondEphemeral(s, i, fmt.Sprintf("🏆 **CHAMPION WELCOMER**\n\nStatus\n%s\nChannel\n%s\nPersistent\n✅", state, cfg.ChannelID))
		return
	}
	if name == "enable" || name == "disable" {
		cfg.Enabled = name == "enable"
		if err := h.repo.Upsert(context.Background(), *cfg); err != nil {
			respondEphemeral(s, i, "Could not update welcome configuration.")
			return
		}
		respondEphemeral(s, i, "Welcomer updated.")
		return
	}
	if name == "preview" {
		respondEphemeral(s, i, "🏆 **WELCOME PREVIEW**\n\nWelcome to Champion!\n\nUse `/link` to connect your PlayStation username.")
		return
	}
	respondEphemeral(s, i, "Test welcome configuration loaded.")
}
