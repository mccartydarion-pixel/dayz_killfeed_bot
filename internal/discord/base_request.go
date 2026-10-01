package discord

import (
	"fmt"
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

// NewBaseRequestMessage tells the server owner a player asked for a base to
// be registered. Nothing is registered until they approve.
func NewBaseRequestMessage(playerName, baseName, serverName string, radius float64, reviewURL string) *discordgo.MessageSend {
	player := caseFallback(caseSafeText(playerName, 64), "A player")
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® BASE REGISTRATION"},
		Title:       "📍 New base request on " + caseFallback(caseSafeText(serverName, 100), "your server"),
		Color:       presentation.InfoSteel,
		Description: "**" + player + "** wants **" + caseFallback(caseSafeText(baseName, 64), "a base") + "** registered. Nothing is registered until you approve it.",
		Fields:      []*discordgo.MessageEmbedField{{Name: "Size", Value: fmt.Sprintf("%.0f m around where they stood", radius), Inline: true}},
	}
	if reviewURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Review", Value: reviewURL, Inline: false})
	} else {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "Approve or decline it on the anti-cheat Bases tab"}
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

// SecurityGiftMessage tells a player the server owner gave them paid time.
func SecurityGiftMessage(serviceLabel, serverName string, days int, endsAt time.Time, note, storeURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® SECURITY STORE"},
		Title:       fmt.Sprintf("🎁 You've been given %d days of %s", days, caseSafeText(serviceLabel, 40)),
		Color:       presentation.SuccessGreen,
		Description: "A gift from the owner of " + caseFallback(caseSafeText(serverName, 100), "your server") + ". Nothing was charged.",
		Fields:      []*discordgo.MessageEmbedField{{Name: "Active until", Value: fmt.Sprintf("<t:%d:f>", endsAt.Unix()), Inline: true}},
	}
	if n := caseSafeText(note, 200); n != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Note", Value: n})
	}
	if storeURL != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Security Store", Value: storeURL})
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}
