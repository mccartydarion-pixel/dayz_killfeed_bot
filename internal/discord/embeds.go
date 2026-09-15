package discord

import (
	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// ServerStatusEmbed builds a status embed for the /server command.
func ServerStatusEmbed(serviceID, game, status string) *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("SERVER STATUS", presentation.InfoSteel)
	embed.Description = "Server status overview"
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "API STATUS", Value: "ONLINE", Inline: true},
		{Name: "NITRADO", Value: "CONNECTED", Inline: true},
	}
	if game != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "GAME", Value: game, Inline: true})
	}
	if status != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "SERVER STATUS", Value: status, Inline: true})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "BOT STATUS", Value: "ONLINE", Inline: true})

	return embed
}
