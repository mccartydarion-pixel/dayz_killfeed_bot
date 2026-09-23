package discord

import (
	"fmt"
	"math"
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

// WarCompletionCard is everything the war-final card can show. Every name is
// resolved by the caller; an empty string means "unknown" and is omitted,
// never replaced with a placeholder or an internal ID.
type WarCompletionCard struct {
	FactionA, FactionB string
	ScoreA, ScoreB     int64
	// Winner is the winning faction's label; Draw marks an authoritative tie.
	Winner string
	Draw   bool
	// TopKiller/TopKills and LongestKiller/Longest are shown only when the
	// holder is known.
	TopKiller     string
	TopKills      int64
	LongestKiller string
	Longest       float64
	Season        string
}

// BuildWarCompletionEmbed is the war-final scoreboard: the matchup and result
// as the headline, one inline field per side, then the war's records.
func BuildWarCompletionEmbed(c WarCompletionCard) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("⚔️ FACTION WAR COMPLETE", presentation.FactionGold)
	embed.Footer.Text = presentation.SeasonFooterText(c.Season)
	// Emptiness is checked before SafeName, which turns "" into "Unknown".
	known := func(s string) bool { return strings.TrimSpace(s) != "" }
	var lines []string
	if known(c.FactionA) && known(c.FactionB) {
		lines = append(lines, "**"+presentation.SafeName(c.FactionA, 60)+"** vs **"+presentation.SafeName(c.FactionB, 60)+"**")
	}
	switch {
	case known(c.Winner):
		lines = append(lines, "👑 **"+presentation.SafeName(c.Winner, 60)+"** wins the war")
	case c.Draw:
		lines = append(lines, "🤝 **DRAW**")
	}
	embed.Description = strings.Join(lines, "\n")
	side := func(name string, score int64) *discordgo.MessageEmbedField {
		heading := "⚔️ FACTION"
		if known(name) {
			heading = presentation.CleanName(name, 60)
		}
		return &discordgo.MessageEmbedField{Name: heading, Value: "**" + presentation.Plural(score, "Kill", "Kills") + "**", Inline: true}
	}
	record := func(heading, value, holder string) *discordgo.MessageEmbedField {
		if !known(holder) {
			return nil
		}
		return &discordgo.MessageEmbedField{Name: heading, Value: "**" + value + "**\n" + presentation.SafeName(holder, presentation.MaxRankNameRunes), Inline: true}
	}
	presentation.AppendFields(embed,
		side(c.FactionA, c.ScoreA),
		side(c.FactionB, c.ScoreB),
		record("🔥 TOP KILLER", presentation.Plural(c.TopKills, "Kill", "Kills"), c.TopKiller),
		record("🎯 LONGEST KILL", presentation.FormatDistance(c.Longest), c.LongestKiller),
	)
	return presentation.FitEmbed(embed)
}

// EventPlacement is one resolved row of an event podium.
type EventPlacement struct {
	Rank  int
	Name  string
	Score float64
}

// BuildEventCompletionEmbed is the event-final podium: one ranking field of
// rank lines (🥇🥈🥉 for the top three). Placements must carry resolved names.
func BuildEventCompletionEmbed(name, season string, placements []EventPlacement) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("👑 EVENT COMPLETE", presentation.EventGold)
	embed.Footer.Text = presentation.SeasonFooterText(season)
	embed.Description = "**" + presentation.SafeName(name, 60) + "** is in the books."
	rows := make([]string, 0, len(placements))
	for i, pl := range placements {
		if strings.TrimSpace(pl.Name) == "" {
			continue
		}
		rank := pl.Rank
		if rank <= 0 {
			rank = i + 1
		}
		rows = append(rows, presentation.RankLine(rank, pl.Name, formatEventScore(pl.Score)))
	}
	if len(rows) == 0 {
		rows = append(rows, presentation.EmptyRanking)
	}
	presentation.AppendFields(embed, &discordgo.MessageEmbedField{Name: "🏆 FINAL STANDINGS", Value: strings.Join(rows, "\n")})
	return presentation.FitEmbed(embed)
}

// formatEventScore shows event points: whole scores as "12,500 pts",
// fractional ones to one decimal.
func formatEventScore(score float64) string {
	if score == math.Trunc(score) && math.Abs(score) < 1e15 {
		return presentation.FormatPoints(int64(score))
	}
	return fmt.Sprintf("%.1f pts", score)
}
