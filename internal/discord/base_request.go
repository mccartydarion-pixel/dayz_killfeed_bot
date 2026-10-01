package discord

import (
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BaseRequestDecisionMessage tells a player the server owner approved or
// declined their base registration request. Text is neutralised and mentions
// are disabled.
func BaseRequestDecisionMessage(approved bool, baseName, serverName, reason string) *discordgo.MessageSend {
	name := caseFallback(caseSafeText(baseName, 64), "your base")
	server := caseFallback(caseSafeText(serverName, 100), "your server")
	embed := &discordgo.MessageEmbed{Author: &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® BASE REGISTRATION"}}
	if approved {
		embed.Title = "✅ " + name + " is registered"
		embed.Color = presentation.SuccessGreen
		embed.Description = "The owner of " + server + " approved your base. You can now use the base services in the Security Store, like the Raid Alarm and Perimeter Watch."
	} else {
		embed.Title = "❌ Your base request wasn't approved"
		embed.Color = presentation.ErrorRed
		embed.Description = "The owner of " + server + " declined your request for " + name + "."
		if r := caseSafeText(reason, 300); r != "" {
			embed.Fields = []*discordgo.MessageEmbedField{{Name: "Reason", Value: r}}
		}
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "You can send a new request from the Security Store"}
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}
