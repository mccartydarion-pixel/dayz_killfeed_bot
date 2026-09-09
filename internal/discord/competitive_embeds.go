package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

func BuildSeasonCompletionEmbed(name string, topPlayer string, topPlayerKills int64, topFaction string, topFactionKills int64, longestPlayer string, longest float64, streakPlayer string, streak int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Title: "👑 CHAMPION • " + safePanelText(name) + " COMPLETE", Description: fmt.Sprintf("🏆 **TOP PLAYER**\n%s\n%d Kills\n\n⚔️ **TOP FACTION**\n%s\n%d Kills\n\n🎯 **LONGEST KILL**\n%.1fm\n%s\n\n🔥 **BEST STREAK**\n%d\n%s", safePanelText(topPlayer), topPlayerKills, safePanelText(topFaction), topFactionKills, longest, safePanelText(longestPlayer), streak, safePanelText(streakPlayer)), Color: ColorChampionGold, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • EVERY KILL TELLS A STORY"}}
}

func BuildWarCompletionText(factionA string, scoreA int64, factionB string, scoreB int64, winner string, topKiller string, topCount int64, longest float64, season string) string {
	result := winner
	if scoreA == scoreB {
		result = "🤝 DRAW"
	}
	return fmt.Sprintf("🏆 **FACTION WAR COMPLETE**\n\n%s\n%d\n\nVS\n\n%s\n%d\n\n👑 %s\n\n🔥 TOP KILLER\n%s — %d\n\n🎯 LONGEST KILL\n%.1fm\n\nSeason: %s", safePanelText(factionA), scoreA, safePanelText(factionB), scoreB, safePanelText(result), safePanelText(topKiller), topCount, longest, safePanelText(season))
}

func BuildEventCompletionText(name string, placements []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "👑 **CHAMPION EVENT COMPLETE**\n\n%s\n\n", safePanelText(name))
	for _, placement := range placements {
		b.WriteString(safePanelText(placement))
		b.WriteByte('\n')
	}
	return b.String()
}
