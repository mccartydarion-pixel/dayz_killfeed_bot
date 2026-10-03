package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 28, Server of the Week: every Monday (UTC) the public network picks its most active
// listed server of the past seven days (active players count three times as much as kills), never
// the same server two weeks running when there is another. The network page shows it, and the
// winner's own events channel gets a card. Always on: only servers listed on the network take part.

const spotlightEvery = 10 * time.Minute

// spotlightScore ranks a server for the week.
func spotlightScore(s repository.NetworkServer) int {
	return s.ActivePlayers7d*3 + s.Kills7d
}

// pickSpotlight is the week's winner, nil when no server had any activity.
func pickSpotlight(servers []repository.NetworkServer, lastWinner int64) *repository.NetworkServer {
	var best *repository.NetworkServer
	for i := range servers {
		s := &servers[i]
		if spotlightScore(*s) == 0 || (s.InstallationID == lastWinner && len(servers) > 1) {
			continue
		}
		if best == nil || spotlightScore(*s) > spotlightScore(*best) {
			best = s
		}
	}
	return best
}

func (a *App) runSpotlight(ctx context.Context, now time.Time) {
	if a.Network == nil || a.Upgrades == nil || !a.upgradeRuns.due("spotlight", spotlightEvery, now) {
		return
	}
	week := repository.RecapWeekStart(now)
	current, err := a.Upgrades.SpotlightFor(ctx, week)
	if err != nil || (current != nil && current.WeekStart.Equal(week)) {
		return
	}
	var lastWinner int64
	if current != nil {
		lastWinner = current.InstallationID
	}
	servers, err := a.Network.Servers(ctx, now, "", 200)
	if err != nil {
		return
	}
	best := pickSpotlight(servers, lastWinner)
	if best == nil {
		return
	}
	pick := repository.Spotlight{WeekStart: week, InstallationID: best.InstallationID, ServerID: best.ServerID, GuildID: best.GuildID, Name: best.Name,
		Platform: best.Platform, ActivePlayers: best.ActivePlayers7d, Kills: best.Kills7d}
	saved, err := a.Upgrades.SaveSpotlight(ctx, pick)
	if err != nil || !saved {
		return
	}
	slog.Info("component=upgrades", "event", "spotlight_picked", "installation_id", pick.InstallationID, "week", week.Format("2006-01-02"))
	a.postRankedCard(ctx, pick.GuildID, pick.ServerID, &discordgo.MessageEmbed{Author: &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® NETWORK"}, Color: 0xF5B700,
		Title: "🌟 Server of the Week",
		Description: fmt.Sprintf("**%s** is this week's Server of the Week on the Champion Network, with %d active players and %d kills in the last seven days. Thanks for playing!",
			pick.Name, pick.ActivePlayers, pick.Kills),
		Fields: []*discordgo.MessageEmbedField{{Name: "See it on the network", Value: fmt.Sprintf("%s/network/%d", a.siteURL(), pick.InstallationID)}}})
}

// handleNetworkSpotlight is GET /api/saas/network/spotlight: this week's server of the week.
func (a *App) handleNetworkSpotlight(w http.ResponseWriter, r *http.Request) {
	if !a.networkReady(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	var pick *repository.Spotlight
	if a.Upgrades != nil {
		p, err := a.Upgrades.SpotlightFor(ctx, repository.RecapWeekStart(time.Now()))
		if err != nil {
			networkFailed(w, "spotlight", err)
			return
		}
		pick = p
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"spotlight": pick})
}
