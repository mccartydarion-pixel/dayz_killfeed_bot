package discord

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
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
	return &discordgo.MessageEmbed{
		Title:       "🏆 WELCOME TO CHAMPION",
		Description: fmt.Sprintf("Welcome <@%s>, %s.\n\nYour combat record starts here.", userID, name),
		Color:       ColorChampionGold,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "🎮 LINK YOUR PLAYSTATION", Value: "Connect your Discord account to your DayZ/PlayStation username:\n`/link username:<PlayStationName>`\n\nOnce linked, Champion can associate your verified DayZ combat stats with your Discord profile.", Inline: false},
			{Name: "⚔️ Track Your Combat", Value: "💀 Kills & Deaths\n🎯 Longest Kill\n📊 Leaderboards\n👑 Records", Inline: false},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"},
	}
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
	titleText := ""
	footerText := ""
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
				titleText = cfg.TitleText
				footerText = cfg.FooterText
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
	embed := WelcomeEmbed(event.Member)
	if titleText != "" {
		embed.Title = titleText
	}
	if footerText != "" {
		embed.Footer.Text = footerText
	}
	if messageText != "" {
		if rendered, renderErr := welcomeservice.Render(messageText, map[string]string{"user": "<@" + event.Member.User.ID + ">", "username": event.Member.User.Username, "server": "Champion", "member_count": "", "link_command": "/link"}); renderErr == nil {
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
		slog.Warn("component=discord", "msg", "welcome message failed", "err", err.Error())
		return
	}
	slog.Info("component=discord", "msg", "welcome message sent", "guild_id", event.GuildID)
	if h.repo != nil && guildRowID > 0 {
		_ = h.repo.MarkWelcomeSent(context.Background(), guildRowID, time.Now().UTC())
	}
}
