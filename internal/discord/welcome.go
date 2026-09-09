package discord

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
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
	store SetupStore
}

func NewWelcomeHandler(store SetupStore) *WelcomeHandler {
	return &WelcomeHandler{store: store}
}

// HandleMemberJoin sends one safe welcome message to the configured channel.
func (h *WelcomeHandler) HandleMemberJoin(s *discordgo.Session, event *discordgo.GuildMemberAdd) {
	if h == nil || h.store == nil || event == nil || event.Member == nil {
		return
	}
	setup, err := h.store.Get(event.GuildID)
	if err != nil || setup == nil || !setup.WelcomeEnabled || setup.WelcomeChannelID == "" {
		return
	}
	if event.Member.User == nil {
		return
	}
	_, err = s.ChannelMessageSendComplex(setup.WelcomeChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{WelcomeEmbed(event.Member)},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Users: []string{event.Member.User.ID},
			Parse: []discordgo.AllowedMentionType{},
		},
	})
	if err != nil {
		slog.Warn("component=discord", "msg", "welcome message failed", "err", err.Error())
		return
	}
	slog.Info("component=discord", "msg", "welcome message sent", "guild_id", event.GuildID)
}
