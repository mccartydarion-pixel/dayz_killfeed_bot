package discord

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Deferred replies: a handler about to do slow work (Nitrado, many queries,
// Discord calls) calls deferEphemeral first, so Discord shows "thinking..."
// instead of timing out after 3 s. The respond helpers below then edit that
// reply instead of answering again, so handlers keep using them unchanged.

var deferredReplies sync.Map // interaction ID -> time deferred

// deferEphemeral acknowledges the interaction with a private "thinking..."
// reply. It reports false if Discord refused (the interaction has expired).
func deferEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	if s == nil || i == nil || i.Interaction == nil {
		return false
	}
	if isDeferred(i) {
		return true
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		return false
	}
	markDeferred(i)
	return true
}

// DeferEphemeral is the exported deferEphemeral for app-level handlers.
func DeferEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	return deferEphemeral(s, i)
}

func markDeferred(i *discordgo.InteractionCreate) {
	now := time.Now()
	deferredReplies.Range(func(key, value any) bool {
		if now.Sub(value.(time.Time)) > interactionTTL {
			deferredReplies.Delete(key)
		}
		return true
	})
	deferredReplies.Store(i.ID, now)
}

func isDeferred(i *discordgo.InteractionCreate) bool {
	if i == nil || i.Interaction == nil {
		return false
	}
	_, ok := deferredReplies.Load(i.ID)
	return ok
}

// respondPrivate sends the private reply, or fills in the deferred one.
func respondPrivate(s *discordgo.Session, i *discordgo.InteractionCreate, data *discordgo.InteractionResponseData) {
	if isDeferred(i) {
		content := data.Content
		embeds := data.Embeds
		if embeds == nil {
			embeds = []*discordgo.MessageEmbed{}
		}
		edit := &discordgo.WebhookEdit{Content: &content, Embeds: &embeds, AllowedMentions: data.AllowedMentions}
		if data.Components != nil {
			components := data.Components
			edit.Components = &components
		}
		_, _ = s.InteractionResponseEdit(i.Interaction, edit)
		return
	}
	data.Flags |= discordgo.MessageFlagsEphemeral
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: data})
}
