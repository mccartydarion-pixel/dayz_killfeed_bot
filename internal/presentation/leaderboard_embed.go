package presentation

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

// BuildPlayerLeaderboardEmbed is the /leaderboard response: one ranking
// category rendered as ONE field through the shared rank formatter - the same
// scoreboard style as the persistent season board.
func BuildPlayerLeaderboardEmbed(category string, entries []RankedEntry, subtitle string) *discordgo.MessageEmbed {
	embed := NewFeedEmbed("🏆 Player leaderboard", ChampionGold)
	desc := "Competitive rankings"
	if s := strings.TrimSpace(subtitle); s != "" {
		desc = CleanName(s, 60) + " rankings"
	}
	cat := RankCategoryOf(category)
	if f := RankingField(cat, entries, 0); f != nil {
		if cat == RankOther && strings.TrimSpace(category) != "" {
			f.Name = "🏆 " + CleanName(category, 40)
		}
		embed.Fields = append(embed.Fields, f)
	} else {
		desc += "\n\n" + EmptyRanking
	}
	embed.Description = desc
	return FitEmbed(embed)
}

func BuildLeaderboardErrorEmbed(category string) *discordgo.MessageEmbed {
	embed := NewFeedEmbed("⚠️ Leaderboard unavailable", ErrorRed)
	embed.Description = "The **" + EscapeMarkdown(CleanName(category, 40)) + "** leaderboard is temporarily unavailable.\nTry again shortly."
	return embed
}
