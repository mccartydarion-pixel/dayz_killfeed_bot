package discord

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// SeasonPlanCard is a scheduled season change as players see it: once when it is scheduled
// (Stage SCHEDULED) and once when it has run (Stage DONE).
type SeasonPlanCard struct {
	Stage         string // SCHEDULED | DONE
	Kind          string // STATS_SEASON | RANKED_RESET
	RunAt         time.Time
	NewSeasonName string
	ServerName    string
}

// BuildSeasonPlanEmbed renders the card. The stats season and Ranked are named separately so
// nobody reads a stats rollover as an RP wipe (or the reverse).
func BuildSeasonPlanEmbed(c SeasonPlanCard) *discordgo.MessageEmbed {
	ranked := c.Kind == "RANKED_RESET"
	var title, what string
	switch {
	case ranked && c.Stage == "DONE":
		title, what = "🎖️ NEW RANKED SEASON", "Ranked RP has been reset. Every kill from now counts toward the new season."
	case ranked:
		title, what = "🗓️ RANKED RESET SCHEDULED", "Ranked RP will be archived and reset. Climb as high as you can before then."
	case c.Stage == "DONE":
		title, what = "📊 NEW STATS SEASON", "The stats season has rolled over. Season kills, deaths and leaderboards start fresh."
	default:
		title, what = "🗓️ STATS SEASON ENDING", "Season stats will be archived and a new season starts. All-time stats are kept."
	}
	embed := presentation.NewFeedEmbed(title, presentation.ChampionGold)
	embed.Description = what
	if !ranked && c.NewSeasonName != "" {
		presentation.AppendFields(embed, presentation.MetricField("NEW SEASON", presentation.SafeName(c.NewSeasonName, 80), true))
	}
	if ranked && c.ServerName != "" {
		presentation.AppendFields(embed, presentation.MetricField("SERVER", presentation.SafeName(c.ServerName, 80), true))
	}
	if c.Stage != "DONE" && !c.RunAt.IsZero() {
		presentation.AppendFields(embed, presentation.MetricField("WHEN", fmt.Sprintf("<t:%d:F> (<t:%d:R>)", c.RunAt.Unix(), c.RunAt.Unix()), false))
	}
	return presentation.FitEmbed(embed)
}
