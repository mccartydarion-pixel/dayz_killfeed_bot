package presentation

import (
	"fmt"
	"github.com/bwmarrin/discordgo"
	"strings"
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
	embed := &discordgo.MessageEmbed{Title: "🏆 CHAMPION LEADERBOARD", Color: 0xC9A227, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • EVERY KILL TELLS A STORY"}}
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
	return &discordgo.MessageEmbed{Title: "🏆 CHAMPION LEADERBOARD", Description: fmt.Sprintf("**Leaderboard temporarily unavailable.**\n\n**Category**\n%s\n\n**Status**\nData service unavailable\n\n**Retry**\nPlease try again shortly.\n\nAdmin Diagnostics\n`/admin diagnostics`", strings.ToUpper(category)), Color: 0xC0392B, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • COMPETITIVE DATA"}}
}
