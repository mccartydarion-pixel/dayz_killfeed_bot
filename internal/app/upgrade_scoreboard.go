package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// Upgrade 5, live event scoreboard: while a competitive event runs, one message in the events
// channel is edited every few minutes with the standings; when it ends it shows the final
// standings once more and is left alone. Hot zones have their own cards and are skipped.

const scoreboardEvery = 3 * time.Minute

// eventScoreLine is one row of the scoreboard.
type eventScoreLine struct {
	Name  string
	Score float64
	Kills int64
}

// formatEventScore says a score in the event's own unit.
func formatEventScore(eventType string, score float64, kills int64) string {
	switch eventType {
	case competitiveevents.TypeLongestKill:
		return presentation.FormatWholeDistance(score)
	case competitiveevents.TypeKillStreak:
		return fmt.Sprintf("%.0f streak", score)
	case competitiveevents.TypeHeadshotHunt:
		return fmt.Sprintf("%.0f headshots", score)
	}
	if kills > 0 && float64(kills) != score {
		return fmt.Sprintf("%.0f pts • %s", score, presentation.Plural(kills, "kill", "kills"))
	}
	return fmt.Sprintf("%.0f kills", score)
}

func buildEventScoreboard(e repository.CompetitiveEvent, lines []eventScoreLine, ended bool, now time.Time) *discordgo.MessageEmbed {
	name := presentation.SafeName(e.Name, 80)
	title := "📋 Live: " + name
	desc := "Standings update every few minutes."
	color := presentation.Crimson // the fight is on
	if ended {
		title, desc, color = "🏁 Final: "+name, "This event is over. These are the final standings.", presentation.Gold
	} else if e.EndsAt != nil {
		desc = "Ends " + presentation.Timestamp(*e.EndsAt, 'R') + ". Standings update every few minutes."
	}
	rows := make([]string, 0, len(lines))
	for i, l := range lines {
		prefix := fmt.Sprintf("`%2d.`", i+1)
		if i < len(placeMedals) {
			prefix = placeMedals[i]
		}
		rows = append(rows, fmt.Sprintf("%s %s • %s", prefix, orUnknown(l.Name), formatEventScore(e.Type, l.Score, l.Kills)))
	}
	value := "No qualifying kills yet. Be the first!"
	if len(rows) > 0 {
		value = strings.Join(rows, "\n")
	}
	return &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Events"), Title: title, Description: desc, Color: color,
		Fields: []*discordgo.MessageEmbedField{{Name: "Standings", Value: value}}, Timestamp: now.UTC().Format(time.RFC3339)}
}

func (a *App) eventScoreLines(ctx context.Context, e repository.CompetitiveEvent) ([]eventScoreLine, error) {
	scores, err := a.Events.Leaderboard(ctx, e.ID, 10)
	if err != nil {
		return nil, err
	}
	var players, factions []int64
	for _, s := range scores {
		if s.PlayerID > 0 {
			players = append(players, s.PlayerID)
		} else if s.FactionID > 0 {
			factions = append(factions, s.FactionID)
		}
	}
	names := map[int64]string{}
	if a.Players != nil && len(players) > 0 {
		if got, err := a.Players.DisplayNamesByID(ctx, e.GuildID, players); err == nil {
			names = got
		}
	}
	factionNames, _ := a.Upgrades.FactionNames(ctx, e.GuildID, factions)
	out := make([]eventScoreLine, 0, len(scores))
	for _, s := range scores {
		name := names[s.PlayerID]
		if s.PlayerID <= 0 {
			name = factionNames[s.FactionID]
		}
		out = append(out, eventScoreLine{Name: name, Score: s.Score, Kills: s.Kills})
	}
	return out, nil
}

// runEventScoreboards keeps the scoreboards of the guild's events up to date.
func (a *App) runEventScoreboards(ctx context.Context, guildID int64, servers []repository.UpgradeServer, now time.Time) {
	if a.routePanels == nil || a.Events == nil || a.ChannelRoutes == nil || !a.upgradeRuns.due(fmt.Sprintf("scoreboard:%d", guildID), scoreboardEvery, now) {
		return
	}
	seen := map[string]bool{}
	var channels []string
	for _, s := range servers {
		if !s.Settings.EventScoreboard {
			continue
		}
		for _, route := range []string{routing.RouteEvents, routing.RouteServerRanks} {
			ch, found, err := a.ChannelRoutes.Resolve(ctx, guildID, s.ServerID, route)
			if err == nil && found && ch != "" {
				if !seen[ch] {
					seen[ch] = true
					channels = append(channels, ch)
				}
				break
			}
		}
	}
	if len(channels) == 0 {
		return
	}
	active, err := a.Events.GetActiveEvents(ctx, guildID)
	if err != nil {
		return
	}
	for _, e := range active {
		if e.Type == competitiveevents.TypeHotZone {
			continue
		}
		a.syncScoreboard(ctx, guildID, e, channels, false, now)
	}
	ended, err := a.Upgrades.RecentlyEndedEvents(ctx, guildID, now.Add(-30*time.Minute))
	if err != nil {
		return
	}
	for _, e := range ended {
		// Only events that had a live board get a final one, and only once.
		if had, err := a.Upgrades.NoticeExists(ctx, "EVENT_SCOREBOARD_LIVE", 0, 0, fmt.Sprint(e.ID)); err != nil || !had {
			continue
		}
		if ok, err := a.Upgrades.ClaimNotice(ctx, "EVENT_SCOREBOARD_FINAL", 0, 0, fmt.Sprint(e.ID), now); err != nil || !ok {
			continue
		}
		a.syncScoreboard(ctx, guildID, e, channels, true, now)
	}
}

func (a *App) syncScoreboard(ctx context.Context, guildID int64, e repository.CompetitiveEvent, channels []string, ended bool, now time.Time) {
	lines, err := a.eventScoreLines(ctx, e)
	if err != nil {
		return
	}
	key := fmt.Sprintf("EVENT_LIVE:%d", e.ID)
	if _, err := a.routePanels.Sync(ctx, guildID, key, channels, discord.PanelContent{Embed: buildEventScoreboard(e, lines, ended, now)}, true, false); err != nil {
		slog.Warn("component=upgrades", "msg", "event scoreboard failed", "event_id", e.ID, "err", err.Error())
		return
	}
	if !ended {
		_, _ = a.Upgrades.ClaimNotice(ctx, "EVENT_SCOREBOARD_LIVE", 0, 0, fmt.Sprint(e.ID), now)
	}
}
