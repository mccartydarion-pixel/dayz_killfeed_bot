package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 4, season-end rewards: when a ranked season ends (reset by staff or by the season
// planner), its top 3 get the champ credits set for their place and Discord gets a "Season
// champions" card. Each season is paid once (claimed in upgrade_notices) and each payment is
// idempotent on its own reference, so a crash between the two never pays twice.

// seasonRewardWindow is how long after a season ended it is still paid (a server switching the
// reward on later does not pay seasons from long ago).
const seasonRewardWindow = 7 * 24 * time.Hour

var placeMedals = []string{"🥇", "🥈", "🥉"}

func buildSeasonChampionsCard(top []repository.SeasonFinisher, rewards [3]int64, serverName string, s repository.EndedSeason) *discordgo.MessageEmbed {
	lines := make([]string, 0, len(top))
	for i, f := range top {
		if i >= 3 {
			break
		}
		line := fmt.Sprintf("%s **%s** · %d RP from %d kills", placeMedals[i], orUnknown(f.Name), f.RP, f.Kills)
		if rewards[i] > 0 {
			line += fmt.Sprintf(" · +%d credits", rewards[i])
		}
		lines = append(lines, line)
	}
	desc := fmt.Sprintf("The ranked season on %s has ended (<t:%d:D> to <t:%d:D>).", serverWord(serverName), s.StartsAt.Unix(), s.EndsAt.Unix())
	embed := &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: 0xF5B700, Title: "🏆 Season champions", Description: desc}
	if len(lines) > 0 {
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "Final standings", Value: strings.Join(lines, "\n")}}
	}
	return embed
}

func (a *App) runSeasonRewards(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	rewards := s.Settings.SeasonRewards
	if rewards == [3]int64{} || a.EconomyService == nil {
		return
	}
	ended, err := a.Upgrades.RecentlyEndedSeasons(ctx, s.ServerID, now.Add(-seasonRewardWindow))
	if err != nil || len(ended) == 0 {
		return
	}
	for _, season := range ended {
		claimed, err := a.Upgrades.ClaimNotice(ctx, "SEASON_REWARDS", s.ServerID, 0, fmt.Sprint(season.ID), now)
		if err != nil || !claimed {
			continue
		}
		top, err := a.Upgrades.SeasonTop(ctx, season.ID, 3)
		if err != nil {
			slog.Warn("component=upgrades", "msg", "season top failed", "season_id", season.ID, "err", err.Error())
			continue
		}
		serverName := a.serverName(s.ServerID)
		for i, f := range top {
			if rewards[i] <= 0 {
				continue
			}
			_, err := a.EconomyService.Credit(ctx, economy.Request{GuildID: guildID, ServerID: s.ServerID, PlayerID: f.PlayerID, Amount: rewards[i],
				Type: economy.TypeSystemReward, ReferenceID: fmt.Sprintf("reward:ranked-season:%d:%d", season.ID, i+1),
				Description: fmt.Sprintf("Ranked season reward: #%d", i+1), Actor: "SYSTEM"})
			if err != nil {
				slog.Warn("component=upgrades", "msg", "season reward failed", "season_id", season.ID, "player_id", f.PlayerID, "err", err.Error())
				continue
			}
			_ = a.sendPlayerDM(ctx, guildID, f.PlayerID, dm(upgradeEmbed("CHAMPIONS® RANKED", placeMedals[i]+" You finished #"+fmt.Sprint(i+1)+" in the ranked season",
				fmt.Sprintf("On %s, with %d RP. **+%d champ credits** are in your wallet.", serverWord(serverName), f.RP, rewards[i]), 0xF5B700)))
		}
		if len(top) > 0 {
			a.postRankedCard(ctx, guildID, s.ServerID, buildSeasonChampionsCard(top, rewards, serverName, season))
		}
		slog.Info("component=upgrades", "event", "season_rewards_paid", "season_id", season.ID, "players", len(top))
	}
}
