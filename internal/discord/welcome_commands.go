package discord

import (
	"context"
	"fmt"
	"strings"

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
		{Name: "channel", Description: "Set welcome channel", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "channel", Description: "Channel ID", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "message", Description: "Set custom welcome message", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "text", Description: "Message text", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "title", Description: "Set welcome title", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "text", Description: "Title", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "footer", Description: "Set welcome footer", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "text", Description: "Footer", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "color", Description: "Set welcome color preset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "value", Description: "gold | red | green | blue | orange", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "image", Description: "Set HTTPS welcome image", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "url", Description: "HTTPS URL", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "thumbnail", Description: "Set HTTPS welcome thumbnail", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "url", Description: "HTTPS URL", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
		{Name: "preset", Description: "Apply welcome preset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandOption{{Name: "name", Description: "champion | minimal", Type: discordgo.ApplicationCommandOptionString, Required: true}}},
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
	if err != nil || cfg == nil {
		cfg = &repository.WelcomeConfig{GuildID: gid, Enabled: true}
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
	if name == "channel" {
		cfg.ChannelID = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "channel"))
	} else if name == "message" {
		cfg.MessageText = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "text"))
	} else if name == "title" {
		cfg.TitleText = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "text"))
	} else if name == "footer" {
		cfg.FooterText = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "text"))
	} else if name == "color" {
		if val := colorFromValue(optionString(i.ApplicationCommandData().Options[0], "value")); val != nil {
			cfg.Color = val
		} else {
			respondEphemeral(s, i, "Unsupported welcome color. Use gold, red, green, blue, or orange.")
			return
		}
	} else if name == "image" {
		cfg.ImageURL = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "url"))
	} else if name == "thumbnail" {
		cfg.ThumbnailURL = strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "url"))
	} else if name == "preset" {
		cfg = welcomePresetConfig(strings.TrimSpace(optionString(i.ApplicationCommandData().Options[0], "name")), *cfg)
	} else if name == "preview" {
		respondEphemeral(s, i, "🏆 **WELCOME PREVIEW**\n\nWelcome to Champion!\n\nUse `/link` to connect your PlayStation username.")
		return
	} else if name == "test" {
		respondEphemeral(s, i, "Test welcome configuration loaded.")
		return
	}

	if err := h.repo.Upsert(context.Background(), *cfg); err != nil {
		respondEphemeral(s, i, "Could not update welcome configuration.")
		return
	}
	respondEphemeral(s, i, "Welcomer updated.")
}

func colorFromValue(value string) *int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gold":
		v := ColorChampionGold
		return &v
	case "red":
		v := ColorDangerRed
		return &v
	case "green":
		v := ColorSuccessGreen
		return &v
	case "blue":
		v := ColorInfoBlue
		return &v
	case "orange":
		v := ColorWarningOrange
		return &v
	default:
		return nil
	}
}

func welcomePresetConfig(name string, cfg repository.WelcomeConfig) *repository.WelcomeConfig {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "minimal":
		cfg.TitleText = "Welcome to Champion"
		cfg.MessageText = "Welcome {user} to {server}!"
		cfg.FooterText = "Champion Killfeed"
		v := ColorInfoBlue
		cfg.Color = &v
		return &cfg
	default:
		cfg.TitleText = "🏆 WELCOME TO CHAMPION"
		cfg.MessageText = "Welcome {user} to {server}.\n\nUse `/link` to connect your PlayStation username and unlock your combat history."
		cfg.FooterText = "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"
		v := ColorChampionGold
		cfg.Color = &v
		return &cfg
	}
}
