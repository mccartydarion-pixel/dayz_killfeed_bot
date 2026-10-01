package discord

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// EventAnnouncementCard is what an owner-built event's "upcoming" or "started" card says.
type EventAnnouncementCard struct {
	Kind        string // UPCOMING | STARTED
	Name        string
	Description string
	Type        string
	Config      json.RawMessage
	StartsAt    *time.Time
	EndsAt      *time.Time
	Prizes      [3]int
}

// EventRuleText explains how an event type scores, in one line.
func EventRuleText(eventType string, config json.RawMessage) string {
	var c struct {
		MinimumDistance *float64 `json:"minimum_distance"`
		MinimumStreak   int      `json:"minimum_streak"`
		WeaponNames     []string `json:"weapon_names"`
	}
	_ = json.Unmarshal(config, &c)
	switch eventType {
	case "MOST_KILLS":
		if c.MinimumDistance != nil && *c.MinimumDistance > 0 {
			return fmt.Sprintf("Most PvP kills from %.0f m or further wins.", *c.MinimumDistance)
		}
		return "Most PvP kills wins."
	case "LONGEST_KILL":
		if c.MinimumDistance != nil && *c.MinimumDistance > 0 {
			return fmt.Sprintf("Longest single kill wins (%.0f m minimum).", *c.MinimumDistance)
		}
		return "Longest single kill wins."
	case "KILL_STREAK":
		if c.MinimumStreak > 0 {
			return fmt.Sprintf("Highest kill streak wins (streaks of %d or more count).", c.MinimumStreak)
		}
		return "Highest kill streak wins."
	case "HEADSHOT_HUNT":
		return "Most headshot kills wins."
	case "WEAPON_CHALLENGE":
		names := make([]string, 0, len(c.WeaponNames))
		for _, n := range c.WeaponNames {
			names = append(names, presentation.SafeName(n, 40))
		}
		return "Most kills with: " + strings.Join(names, ", ") + "."
	case "FACTION_KILLS":
		return "The faction with the most kills on enemy factions wins."
	}
	return "Top scorers win."
}

// BuildEventAnnouncementEmbed renders the upcoming / started card.
func BuildEventAnnouncementEmbed(c EventAnnouncementCard) *discordgo.MessageEmbed {
	title, color := "🏁 EVENT STARTED", presentation.EventGold
	if c.Kind == "UPCOMING" {
		title, color = "📅 UPCOMING EVENT", presentation.ChampionGold
	}
	embed := presentation.NewFeedEmbed(title, color)
	desc := "**" + presentation.SafeName(c.Name, 80) + "**"
	if d := strings.TrimSpace(c.Description); d != "" {
		desc += "\n" + presentation.SafeName(d, 300)
	}
	embed.Description = desc
	presentation.AppendFields(embed, presentation.MetricField("HOW TO WIN", EventRuleText(c.Type, c.Config), false))
	if c.Kind == "UPCOMING" && c.StartsAt != nil {
		presentation.AppendFields(embed, presentation.MetricField("STARTS", fmt.Sprintf("<t:%d:F> (<t:%d:R>)", c.StartsAt.Unix(), c.StartsAt.Unix()), true))
	}
	if c.EndsAt != nil {
		presentation.AppendFields(embed, presentation.MetricField("ENDS", fmt.Sprintf("<t:%d:R>", c.EndsAt.Unix()), true))
	}
	var prizes []string
	for i, pts := range c.Prizes {
		if pts > 0 {
			prizes = append(prizes, fmt.Sprintf("%s %s", []string{"🥇", "🥈", "🥉"}[i], presentation.FormatPoints(int64(pts))))
		}
	}
	presentation.AppendFields(embed, presentation.MetricField("PRIZES", strings.Join(prizes, " • "), false))
	return presentation.FitEmbed(embed)
}
