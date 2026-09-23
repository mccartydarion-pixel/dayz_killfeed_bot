package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BuildSeasonCompletionEmbed is the season-final scoreboard: the season as the
// headline and one inline field per record. A holder's name is shown only
// when the caller knows it ("" = omitted, never a placeholder).
func BuildSeasonCompletionEmbed(name string, topPlayer string, topPlayerKills int64, topFaction string, topFactionKills int64, longestPlayer string, longest float64, streakPlayer string, streak int) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("🏆 SEASON COMPLETE", presentation.ChampionGold)
	embed.Footer.Text = presentation.SeasonFooterText(name)
	embed.Description = "**" + presentation.SafeName(name, 60) + "** is in the books."
	record := func(heading, value, holder string) *discordgo.MessageEmbedField {
		v := "**" + value + "**"
		if strings.TrimSpace(holder) != "" {
			v += "\n" + presentation.SafeName(holder, presentation.MaxRankNameRunes)
		}
		return &discordgo.MessageEmbedField{Name: heading, Value: v, Inline: true}
	}
	presentation.AppendFields(embed,
		record("👑 TOP PLAYER", presentation.Plural(topPlayerKills, "Kill", "Kills"), topPlayer),
		record("⚔️ TOP FACTION", presentation.Plural(topFactionKills, "Kill", "Kills"), topFaction),
		record("🎯 LONGEST KILL", presentation.FormatDistance(longest), longestPlayer),
		record("🔥 BEST STREAK", presentation.Plural(int64(streak), "Kill", "Kills"), streakPlayer),
	)
	return presentation.FitEmbed(embed)
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
