package discord

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
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
	configErr := err
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
		respondEphemeral(s, i, h.welcomeStatus(s, *cfg, configErr))
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
		h.sendWelcomeTest(s, i, *cfg)
		return
	}

	if err := h.repo.Upsert(context.Background(), *cfg); err != nil {
		respondEphemeral(s, i, "Could not update welcome configuration.")
		return
	}
	respondEphemeral(s, i, "Welcomer updated.")
}

type welcomeChannelStatus struct {
	exists, view, send, embed bool
	errClass                  string
	retryAfter                string
}

func inspectWelcomeChannel(s *discordgo.Session, channelID string) welcomeChannelStatus {
	status := welcomeChannelStatus{}
	if strings.TrimSpace(channelID) == "" {
		status.errClass = "not_configured"
		return status
	}
	if s == nil {
		status.errClass = "discord_unavailable"
		return status
	}
	if _, err := s.Channel(channelID); err != nil {
		status.errClass = welcomeErrorClass(err)
		return status
	}
	status.exists = true
	if s.State == nil || s.State.User == nil || s.State.User.ID == "" {
		status.errClass = "bot_user_unavailable"
		return status
	}
	perms, err := s.UserChannelPermissions(s.State.User.ID, channelID)
	if err != nil {
		status.errClass = welcomeErrorClass(err)
		return status
	}
	status.view = perms&discordgo.PermissionViewChannel != 0
	status.send = perms&discordgo.PermissionSendMessages != 0
	status.embed = perms&discordgo.PermissionEmbedLinks != 0
	if !status.view || !status.send || !status.embed {
		status.errClass = "missing_permission"
	}
	return status
}

func (h *WelcomeCommandHandler) welcomeStatus(s *discordgo.Session, cfg repository.WelcomeConfig, configErr error) string {
	channel := inspectWelcomeChannel(s, cfg.ChannelID)
	lastWelcome := "never"
	if cfg.LastWelcomeAt != nil {
		lastWelcome = fmt.Sprintf("<t:%d:R>", cfg.LastWelcomeAt.Unix())
	}
	lastError := channel.errClass
	if lastError == "" && configErr != nil {
		lastError = welcomeErrorClass(configErr)
	}
	if lastError == "" {
		lastError = "none"
	}
	state := "disabled"
	if cfg.Enabled {
		state = "enabled"
	}
	intentRequested := false
	if s != nil {
		intentRequested = s.Identify.Intents&discordgo.IntentsGuildMembers != 0
	}
	return fmt.Sprintf("🏆 **CHAMPION WELCOMER**\n\nEnabled\n%s\nChannel\n%s\nChannel Exists\n%s\nView Permission\n%s\nSend Permission\n%s\nEmbed Permission\n%s\nGuild Members Intent Required\n%s\nLast Successful Welcome\n%s\nLast Error Class\n%s", state, configuredLabel(cfg.ChannelID), boolLabel(channel.exists), boolLabel(channel.view), boolLabel(channel.send), boolLabel(channel.embed), boolLabel(intentRequested), lastWelcome, lastError)
}

func (h *WelcomeCommandHandler) sendWelcomeTest(s *discordgo.Session, i *discordgo.InteractionCreate, cfg repository.WelcomeConfig) {
	channel := inspectWelcomeChannel(s, cfg.ChannelID)
	if !channel.exists {
		respondEphemeral(s, i, "❌ TEST WELCOME FAILED\nWelcome channel was not found.\nError class: "+channel.errClass)
		return
	}
	if !channel.view || !channel.send || !channel.embed {
		respondEphemeral(s, i, fmt.Sprintf("❌ TEST WELCOME FAILED\nMissing permission: View=%s Send=%s Embed=%s\nError class: %s", boolLabel(channel.view), boolLabel(channel.send), boolLabel(channel.embed), channel.errClass))
		return
	}
	embed := welcomeEmbedFromConfig(cfg, &discordgo.Member{User: &discordgo.User{ID: "0", Username: "Test Member"}}, true)
	_, err := s.ChannelMessageSendComplex(cfg.ChannelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
	if err != nil {
		class := welcomeErrorClass(err)
		if class == "rate_limited" {
			respondEphemeral(s, i, "❌ TEST WELCOME FAILED\nDiscord rate limited the test send. Retry-After: "+retryAfterFromError(err))
			return
		}
		respondEphemeral(s, i, "❌ TEST WELCOME FAILED\nError class: "+class)
		return
	}
	respondEphemeral(s, i, "✅ TEST WELCOME SENT\nOne TEST WELCOME embed was sent to the configured channel.")
}

func configuredLabel(channelID string) string {
	if strings.TrimSpace(channelID) == "" {
		return "not configured"
	}
	return "configured"
}

func boolLabel(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func welcomeErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil {
		switch restErr.Response.StatusCode {
		case http.StatusNotFound:
			return "not_found"
		case http.StatusForbidden:
			return "missing_permission_50013"
		case http.StatusTooManyRequests:
			return "rate_limited"
		}
		if restErr.Response.StatusCode >= 500 {
			return "discord_5xx"
		}
	}
	return "unavailable"
}

func retryAfterFromError(err error) string {
	if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil {
		if value := restErr.Response.Header.Get("Retry-After"); value != "" {
			if seconds, parseErr := strconv.ParseFloat(value, 64); parseErr == nil {
				return fmt.Sprintf("%gs", seconds)
			}
		}
	}
	return "unknown"
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
