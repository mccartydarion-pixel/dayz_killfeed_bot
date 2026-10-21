package discord

import (
	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// ServerStatusEmbed builds a status embed for the /server command.
func ServerStatusEmbed(serviceID, game, status string) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("Server status", presentation.InfoSteel)
	embed.Description = "Server status overview"
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "API status", Value: "Online", Inline: true},
		{Name: "Nitrado", Value: "Connected", Inline: true},
	}
	if game != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Game", Value: game, Inline: true})
	}
	if status != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Server status", Value: status, Inline: true})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Bot status", Value: "Online", Inline: true})

	return embed
}
