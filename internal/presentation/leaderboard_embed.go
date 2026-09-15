package presentation

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type RankedEntry struct {
	Rank        int
	Name, Value string
}

func safe(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "@", ""), "#", "")
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}
func BuildPlayerLeaderboardEmbed(category string, entries []RankedEntry, subtitle string) *discordgo.MessageEmbed {
	embed := NewChampionEmbed("SEASON LEADERBOARD", ChampionGold)
	embed.Footer = AutoRefreshFooter()
	embed.Description = fmt.Sprintf("**%s**\n%s\nTop %d Players", strings.ToUpper(category), subtitle, len(entries))
	for _, e := range entries {
		medal := ""
		if e.Rank == 1 {
			medal = "#1"
		} else if e.Rank == 2 {
			medal = "#2"
		} else if e.Rank == 3 {
			medal = "#3"
		} else {
			medal = fmt.Sprintf("#%d", e.Rank)
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: medal + " " + safe(e.Name), Value: formatValue(category, e.Value), Inline: false})
	}
	if len(entries) == 0 {
		embed.Description += "\n\nNo qualifying data yet."
	}
	return embed
}
func formatValue(category, value string) string {
	switch strings.ToLower(category) {
	case "kills":
		return "**Kills**  " + safe(value)
	case "kd":
		return "**K/D**  " + safe(value)
	case "longest":
		return "**Distance**  " + safe(value)
	default:
		return "**Value**  " + safe(value)
	}
}
func BuildLeaderboardErrorEmbed(category string) *discordgo.MessageEmbed {
	embed := NewChampionEmbed("LEADERBOARD", ErrorRed)
	embed.Description = fmt.Sprintf("**SERVICE UNAVAILABLE**\n\n**Category**\n%s\n\n**Status**\nTEMPORARILY UNAVAILABLE\n\nTry again shortly.", strings.ToUpper(category))
	return embed
}
