package discord

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type WelcomeCommandHandler struct {
	repo   *repository.WelcomeRepository
	guilds GuildStore
}

func NewWelcomeCommandHandler(r *repository.WelcomeRepository, g GuildStore, _ SetupStore) *WelcomeCommandHandler {
	return &WelcomeCommandHandler{repo: r, guilds: g}
}
func RegisterWelcomeCommands(s CommandRegistrar, guildID string) error {
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
		respondEphemeral(s, i, ReplyIsUnavailable("The welcomer"))
		return
	}
	_, gid, err := h.guilds.GetGuild(context.Background(), i.GuildID)
	if err != nil || gid == 0 {
		respondEphemeral(s, i, ReplyNotSetUp)
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
		respondEphemeral(s, i, ReplyNeedsManageServer)
		return
	}
	if name == "status" {
		deferEphemeral(s, i) // reads the channel and permissions from Discord
		respondEphemeral(s, i, h.welcomeStatus(s, i.GuildID, *cfg, configErr))
		return
	}
	if name == "enable" || name == "disable" {
		cfg.Enabled = name == "enable"
		if err := h.repo.Upsert(context.Background(), *cfg); err != nil {
			respondEphemeral(s, i, ReplyCouldNot("update the welcome settings"))
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
		respondEphemeral(s, i, "🏆 **Welcome preview**\n\nWelcome to Champion!\n\nUse `/link` to connect your PlayStation username.")
		return
	} else if name == "test" {
		h.sendWelcomeTest(s, i, *cfg)
		return
	}

	if err := h.repo.Upsert(context.Background(), *cfg); err != nil {
		respondEphemeral(s, i, ReplyCouldNot("update the welcome settings"))
		return
	}
	respondEphemeral(s, i, "Welcomer updated.")
}

type welcomeChannelStatus struct {
	exists, view, send, embed bool
	errClass                  string
	retryAfter                string
}

func inspectWelcomeChannel(s *discordgo.Session, guildID, channelID string) welcomeChannelStatus {
	status := welcomeChannelStatus{}
	if strings.TrimSpace(channelID) == "" {
		status.errClass = "CHANNEL_MISSING"
		return status
	}
	if s == nil {
		status.errClass = "discord_unavailable"
		return status
	}
	channel, err := s.Channel(channelID)
	if err != nil {
		status.errClass = welcomeErrorClass(err)
		return status
	}
	if channel == nil || channel.GuildID == "" || (guildID != "" && channel.GuildID != guildID) {
		status.errClass = "CHANNEL_MISSING"
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

func (h *WelcomeCommandHandler) welcomeStatus(s *discordgo.Session, guildID string, cfg repository.WelcomeConfig, configErr error) string {
	channel := inspectWelcomeChannel(s, guildID, cfg.ChannelID)
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
	return fmt.Sprintf("🏆 **Champion welcomer**\n\nEnabled\n%s\nChannel\n%s\nChannel Exists\n%s\nView Permission\n%s\nSend Permission\n%s\nEmbed Permission\n%s\nGuild Members Intent Required\n%s\nLast Successful Welcome\n%s\nLast Error Class\n%s", state, configuredLabel(cfg.ChannelID), boolLabel(channel.exists), boolLabel(channel.view), boolLabel(channel.send), boolLabel(channel.embed), boolLabel(intentRequested), lastWelcome, lastError)
}

func (h *WelcomeCommandHandler) sendWelcomeTest(s *discordgo.Session, i *discordgo.InteractionCreate, cfg repository.WelcomeConfig) {
	deferEphemeral(s, i) // reads the channel from Discord and posts the test
	channel := inspectWelcomeChannel(s, i.GuildID, cfg.ChannelID)
	if !channel.exists {
		respondEphemeral(s, i, welcomeTestFailed+"The welcome channel was not found.")
		return
	}
	if !channel.view || !channel.send || !channel.embed {
		respondEphemeral(s, i, fmt.Sprintf(welcomeTestFailed+"Champion is missing a permission in the welcome channel. View Channel: %s · Send Messages: %s · Embed Links: %s", boolLabel(channel.view), boolLabel(channel.send), boolLabel(channel.embed)))
		return
	}
	embed := welcomeEmbedFromConfig(cfg, &discordgo.Member{User: &discordgo.User{ID: "0", Username: "Test Member"}}, true)
	_, err := s.ChannelMessageSendComplex(cfg.ChannelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
	if err != nil {
		class := welcomeErrorClass(err)
		if class == "RATE_LIMITED" {
			respondEphemeral(s, i, welcomeTestFailed+"Discord rate limited the test send. Retry after: "+retryAfterFromError(err))
			return
		}
		respondEphemeral(s, i, welcomeTestFailed+welcomeFailureSentence(class))
		return
	}
	respondEphemeral(s, i, "✅ Test welcome sent\nOne test welcome card was sent to the configured channel.")
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

// welcomeTestFailed starts every answer to a welcome test that did not post.
const welcomeTestFailed = "Test welcome failed\n"

// welcomeFailureSentence says what a failure class means in plain words (the class itself is
// an internal name and is never shown).
func welcomeFailureSentence(class string) string {
	switch class {
	case "CHANNEL_MISSING":
		return "The welcome channel was not found."
	case "PERMISSION_BLOCKED":
		return "Champion is missing a permission in the welcome channel."
	case "RATE_LIMITED":
		return "Discord rate limited the test send. Try again in a moment."
	default:
		return "Discord did not answer. Try again in a moment."
	}
}

func welcomeErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "404") || strings.Contains(message, "not found") || strings.Contains(message, "unknown channel") {
		return "CHANNEL_MISSING"
	}
	if restErr, ok := err.(*discordgo.RESTError); ok && restErr.Response != nil {
		switch restErr.Response.StatusCode {
		case http.StatusNotFound:
			return "CHANNEL_MISSING"
		case http.StatusForbidden:
			return "PERMISSION_BLOCKED"
		case http.StatusTooManyRequests:
			return "RATE_LIMITED"
		}
		if restErr.Response.StatusCode >= 500 {
			return "DISCORD_UNAVAILABLE"
		}
	}
	return "DISCORD_UNAVAILABLE"
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

// welcomeBlue is the "blue" an owner can pick for their own welcome card. The welcome card's
// colour, title and footer are the owner's choice, so they are not held to the built-in palette.
const welcomeBlue = 0x7FA7FF

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
		v := welcomeBlue
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
		cfg.FooterText = "Champions Killfeed"
		v := ColorInfoBlue
		cfg.Color = &v
		return &cfg
	default:
		cfg.TitleText = "🏆 Welcome to Champion"
		cfg.MessageText = "Welcome {user} to {server}.\n\nUse `/link` to connect your PlayStation username and unlock your combat history."
		cfg.FooterText = "Champions Killfeed · " + presentation.ChampionSlogan
		v := presentation.Crimson
		cfg.Color = &v
		return &cfg
	}
}
