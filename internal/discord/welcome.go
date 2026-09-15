package discord

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	welcomeservice "github.com/yourname/dayz-killfeed/internal/welcome"
)

// WelcomeEmbed builds the Champion-branded new-member welcome card.
func WelcomeEmbed(member *discordgo.Member) *discordgo.MessageEmbed {
	name := "there"
	userID := "0"
	if member != nil && member.User != nil && member.User.Username != "" {
		name = member.User.Username
		userID = member.User.ID
	}
	embed := presentation.NewChampionEmbed("WELCOME TO CHAMPIONS", presentation.ChampionGold)
	embed.Description = fmt.Sprintf("Welcome <@%s>, %s.\n\nYou are now connected to the Champions competitive community.", userID, name)
	embed.Fields = []*discordgo.MessageEmbedField{
		presentation.StatusField("GET STARTED", "Link your PlayStation username\nView player stats\nCheck leaderboards\nFollow active wars and events", false),
		presentation.StatusField("NEXT STEP", "Use #link-username to connect your account.", false),
	}
	return embed
}

func welcomeEmbedFromConfig(cfg repository.WelcomeConfig, member *discordgo.Member, test bool) *discordgo.MessageEmbed {
	embed := WelcomeEmbed(member)
	if cfg.TitleText != "" {
		embed.Title = cfg.TitleText
	}
	if test {
		embed.Title = "[TEST WELCOME] " + embed.Title
	}
	if cfg.FooterText != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: cfg.FooterText}
	}
	if cfg.Color != nil {
		embed.Color = *cfg.Color
	}
	if cfg.ImageURL != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: cfg.ImageURL}
	}
	if cfg.ThumbnailURL != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: cfg.ThumbnailURL}
	}
	return embed
}

// WelcomeHandler handles GuildMemberAdd without affecting the ADM pipeline.
type WelcomeHandler struct {
	store  SetupStore
	repo   *repository.WelcomeRepository
	guilds GuildStore
}

func NewWelcomeHandler(store SetupStore) *WelcomeHandler {
	return &WelcomeHandler{store: store}
}
func NewPersistentWelcomeHandler(store SetupStore, repo *repository.WelcomeRepository, guilds GuildStore) *WelcomeHandler {
	return &WelcomeHandler{store: store, repo: repo, guilds: guilds}
}

// HandleMemberJoin sends one safe welcome message to the configured channel.
func (h *WelcomeHandler) HandleMemberJoin(s *discordgo.Session, event *discordgo.GuildMemberAdd) {
	if h == nil || h.store == nil || event == nil || event.Member == nil {
		return
	}
	setup, err := h.store.Get(event.GuildID)
	if err != nil || setup == nil {
		return
	}
	channelID := setup.WelcomeChannelID
	enabled := setup.WelcomeEnabled
	mention := true
	welcomeBots := false
	messageText := ""
	welcomeCfg := repository.WelcomeConfig{MentionUser: true}
	var guildRowID int64
	if h.repo != nil && h.guilds != nil {
		_, rowID, guildErr := h.guilds.GetGuild(context.Background(), event.GuildID)
		if guildErr == nil && rowID > 0 {
			var cfg *repository.WelcomeConfig
			cfg, err = h.repo.Get(context.Background(), rowID)
			if err == nil && cfg != nil {
				channelID = cfg.ChannelID
				enabled = cfg.Enabled
				mention = cfg.MentionUser
				welcomeBots = cfg.WelcomeBots
				messageText = cfg.MessageText
				welcomeCfg = *cfg
				guildRowID = rowID
			}
		}
	}
	if !enabled || channelID == "" {
		return
	}
	if event.Member.User == nil {
		return
	}
	if event.Member.User.Bot && !welcomeBots {
		return
	}
	channelStatus := inspectWelcomeChannel(s, event.GuildID, channelID)
	if !channelStatus.exists || !channelStatus.view || !channelStatus.send || !channelStatus.embed {
		slog.Warn("component=discord", "msg", "welcome send skipped: channel or permission check failed", "guild_id", event.GuildID, "error_class", channelStatus.errClass)
		return
	}
	embed := welcomeEmbedFromConfig(welcomeCfg, event.Member, false)
	if messageText != "" {
		serverName := "Champion"
		memberCount := ""
		if s != nil {
			if guild, guildErr := s.Guild(event.GuildID); guildErr == nil && guild != nil {
				serverName = guild.Name
				if welcomeCfg.ShowMemberCount {
					memberCount = fmt.Sprintf("%d", guild.MemberCount)
				}
			}
		}
		if rendered, renderErr := welcomeservice.Render(messageText, map[string]string{"user": "<@" + event.Member.User.ID + ">", "username": event.Member.User.Username, "server": serverName, "member_count": memberCount, "link_command": "/link"}); renderErr == nil {
			embed.Description = rendered
		}
	}
	_, err = s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Users: func() []string {
				if mention {
					return []string{event.Member.User.ID}
				}
				return nil
			}(),
			Parse: []discordgo.AllowedMentionType{},
		},
	})
	if err != nil {
		slog.Warn("component=discord", "msg", "welcome message failed", "error_class", welcomeErrorClass(err), "retry_after", retryAfterFromError(err), "err", err.Error())
		return
	}
	slog.Info("component=discord", "msg", "welcome message sent", "guild_id", event.GuildID)
	if h.repo != nil && guildRowID > 0 {
		_ = h.repo.MarkWelcomeSent(context.Background(), guildRowID, time.Now().UTC())
	}
}
