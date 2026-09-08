package discord

import "github.com/bwmarrin/discordgo"

// ServerStatusEmbed builds a status embed for the /server command.
func ServerStatusEmbed(serviceID, game, status string) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:       "DayZ Killfeed",
		Description: "Server status overview",
		Color:       0x00AAFF,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "API Status", Value: "Online", Inline: true},
			{Name: "Nitrado", Value: "Connected", Inline: true},
		},
	}

	if serviceID != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Service ID", Value: serviceID, Inline: true})
	}
	if game != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Game", Value: game, Inline: true})
	}
	if status != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Server Status", Value: status, Inline: true})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Bot Status", Value: "Online", Inline: true})

	return embed
}
