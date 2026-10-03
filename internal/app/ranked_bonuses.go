package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Ranked bonuses (docs/RANKED_BONUSES.md): bounty, underdog, revenge and daily first kill RP, plus
// rank-up cards and DMs and a weekly recap. Each is switched on per server in Growth → Events.
// The RP is decided with the award (repository.scoreBonuses); this file is the settings API and
// the Discord side, which the competitive scheduler drives.

func (a *App) registerRankedBonusRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/ranked/bonuses", a.handleRankedBonuses)
	h("PUT "+adminBase+"/ranked/bonuses", a.handleSaveRankedBonuses)
}

func (a *App) handleRankedBonuses(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.rpBoostAdmin(w, r, permissions.CapEventsView, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	settings, err := a.Ranked.BonusSettings(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load ranked bonuses")
		return
	}
	season, err := a.Ranked.ActiveServerSeason(ctx, ac.scope.GuildID, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the ranked season")
		return
	}
	wanted := []repository.WantedPlayer{}
	if season != nil {
		if list, err := a.Ranked.CurrentWanted(ctx, *season, ac.scope.GuildID, settings, time.Now().UTC()); err == nil {
			wanted = list
		}
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": settings, "season": season, "wanted": wanted})
}

func (a *App) handleSaveRankedBonuses(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.rpBoostAdmin(w, r, permissions.CapEventsManage, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.RankedBonusSettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	saved, err := a.Ranked.SaveBonusSettings(ctx, serverID, req, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrRankedBonusInvalid) {
		writeSaaSError(w, codeInvalidRequest, "bonuses are 10% to 500% of a kill's RP, a bounty streak is 3 to 20 kills and revenge lasts 5 to 120 minutes")
		return
	}
	if err != nil {
		slog.Warn("component=ranked", "event", "bonus_settings_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save ranked bonuses")
		return
	}
	a.recordAudit(ctx, ac, "RANKED_BONUSES_SAVE", fmt.Sprintf("server:%d", serverID), "", "success", nil, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

// playerBonus is one bonus that is on, for the Player Hub.
type playerBonus struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	RP    int64  `json:"rp"`
	Rule  string `json:"rule"`
}

// playerRankedBonuses lists the bonuses that are on and who is wanted, for the Player Hub. Errors
// leave both empty.
func (a *App) playerRankedBonuses(ctx context.Context, guildID, serverID int64) ([]playerBonus, []repository.WantedPlayer) {
	bonuses, wanted := []playerBonus{}, []repository.WantedPlayer{}
	season, err := a.Ranked.ActiveServerSeason(ctx, guildID, serverID)
	if err != nil || season == nil {
		return bonuses, wanted
	}
	s, err := a.Ranked.BonusSettings(ctx, serverID)
	if err != nil {
		return bonuses, wanted
	}
	pct := func(p int) int64 {
		if v := season.RPPerKill * int64(p) / 100; v > 0 {
			return v
		}
		return 1
	}
	if s.BountyEnabled {
		bonuses = append(bonuses, playerBonus{repository.BonusBounty, "Bounty", pct(s.BountyPercent),
			fmt.Sprintf("Kill the #1 player or anyone on a %d-kill streak.", s.BountyStreak)})
	}
	if s.UnderdogEnabled {
		bonuses = append(bonuses, playerBonus{repository.BonusUnderdog, "Underdog", pct(s.UnderdogPercent), "Kill a player ranked in a higher tier than you."})
	}
	if s.RevengeEnabled {
		bonuses = append(bonuses, playerBonus{repository.BonusRevenge, "Revenge", pct(s.RevengePercent),
			fmt.Sprintf("Kill the player who last killed you within %d minutes.", s.RevengeMinutes)})
	}
	if s.DailyFirstEnabled {
		bonuses = append(bonuses, playerBonus{repository.BonusDailyFirst, "First kill of the day", pct(s.DailyFirstPercent), "Your first ranked kill each day (UTC)."})
	}
	if list, err := a.Ranked.CurrentWanted(ctx, *season, guildID, s, time.Now().UTC()); err == nil {
		wanted = list
	}
	return bonuses, wanted
}

// --- Discord ---------------------------------------------------------------------------------------

const (
	rankedWantedColor = 0xC0392B
	rankedRecapColor  = 0x3B82F6
	rankUpsPerPass    = 10
)

var tierColors = map[ranked.Tier]int{
	ranked.Bronze: 0xCD7F32, ranked.Silver: 0xC0C0C0, ranked.Gold: 0xF5B700, ranked.Platinum: 0x7FD1C7,
	ranked.Diamond: 0x5DADE2, ranked.Master: 0x9B59B6,
}

func rankedCardAuthor() *discordgo.MessageEmbedAuthor {
	return &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® RANKED"}
}

func orUnknown(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Unknown player"
	}
	return name
}

func buildWantedCard(wp repository.WantedPlayer, serverName string) *discordgo.MessageEmbed {
	where := "the server"
	if strings.TrimSpace(serverName) != "" {
		where = serverName
	}
	name := orUnknown(wp.Name)
	desc := fmt.Sprintf("**%s** is #1 on %s with %d RP.", name, where, wp.RP)
	if wp.Reason == repository.WantedStreak {
		desc = fmt.Sprintf("**%s** is on a **%d-kill streak** on %s.", name, wp.Streak, where)
	}
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedWantedColor,
		Title:       "💀 BOUNTY ON " + strings.ToUpper(name),
		Description: desc + fmt.Sprintf("\nTake them down for **+%d RP** on top of the kill.", wp.Bounty)}
}

func buildBountyClaimCard(c repository.BountyClaim) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedWantedColor, Title: "🎯 Bounty claimed",
		Description: fmt.Sprintf("**%s** killed **%s**, who %s, and collected the **+%d RP** bounty (%d RP for the kill).",
			orUnknown(c.KillerName), orUnknown(c.VictimName), c.Detail, c.BountyRP, c.TotalRP),
		Timestamp: c.At.Format(time.RFC3339)}
}

func buildRankUpCard(u repository.RankUp, serverName string) *discordgo.MessageEmbed {
	where := ""
	if strings.TrimSpace(serverName) != "" {
		where = " on " + serverName
	}
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: tierColors[u.To],
		Title:       fmt.Sprintf("🏅 %s reached %s", orUnknown(u.Name), repository.TierName(u.To)),
		Description: fmt.Sprintf("%d RP · #%d%s", u.RP, u.Position, where)}
}

func buildRankUpDM(u repository.RankUp, serverName, hubURL string) *discordgo.MessageSend {
	where := "the server"
	if strings.TrimSpace(serverName) != "" {
		where = serverName
	}
	embed := &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: tierColors[u.To],
		Title:       "🏅 You reached " + repository.TierName(u.To),
		Description: fmt.Sprintf("You are now %s on %s with %d RP (#%d). Keep climbing!", repository.TierName(u.To), where, u.RP, u.Position)}
	if hubURL != "" {
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "See your rank in the Player Hub", Value: hubURL}}
	}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

func buildWeeklyRecapCard(rc repository.RankedRecap, serverName string) *discordgo.MessageEmbed {
	where := ""
	if strings.TrimSpace(serverName) != "" {
		where = " on " + serverName
	}
	end := rc.WeekStart.AddDate(0, 0, 6)
	embed := &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedRecapColor, Title: "📊 Ranked week in review",
		Description: fmt.Sprintf("%s to %s%s · %d ranked kills", rc.WeekStart.Format("Jan 2"), end.Format("Jan 2"), where, rc.Kills)}
	if len(rc.Climbers) > 0 {
		medals := []string{"🥇", "🥈", "🥉"}
		lines := []string{}
		for i, l := range rc.Climbers {
			if i >= len(medals) {
				break
			}
			lines = append(lines, fmt.Sprintf("%s %s · +%d RP from %d kills", medals[i], orUnknown(l.PlayerName), l.RP, l.Kills))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Top climbers", Value: strings.Join(lines, "\n")})
	}
	if rc.Bounty != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Biggest bounty",
			Value: fmt.Sprintf("%s collected +%d RP on %s", orUnknown(rc.Bounty.KillerName), rc.Bounty.BountyRP, orUnknown(rc.Bounty.VictimName)), Inline: true})
	}
	if rc.BestStreak > 1 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Longest kill streak",
			Value: fmt.Sprintf("%s · %d kills", orUnknown(rc.StreakName), rc.BestStreak), Inline: true})
	}
	if rc.Revenges > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Revenge kills",
			Value: fmt.Sprintf("%d this week · most: %s (%d)", rc.Revenges, orUnknown(rc.RevengeName), rc.RevengeMost), Inline: true})
	}
	return embed
}

// postRankedCard sends a ranked card the way double RP does (events channel, else server ranks).
func (a *App) postRankedCard(ctx context.Context, guildID, serverID int64, embed *discordgo.MessageEmbed) {
	if a.rpBoostAnnouncer != nil {
		a.rpBoostAnnouncer(serverID, embed)
		return
	}
	a.postRPBoostCard(ctx, guildID, serverID, embed)
}

// recapWindow is when the weekly recap goes out: Monday 12:00 to Tuesday 12:00 UTC, for the week
// that just ended. Outside it nothing is posted (so turning it on mid-week does not post old news).
func recapWindow(now time.Time) (lastWeek time.Time, open bool) {
	week := repository.RecapWeekStart(now)
	return week.AddDate(0, 0, -7), !now.Before(week.Add(12*time.Hour)) && now.Before(week.Add(36*time.Hour))
}

// runRankedBonusAnnouncements posts the guild's due ranked cards: new bounties, claimed bounties,
// rank-ups (and their DMs) and the weekly recap.
func (a *App) runRankedBonusAnnouncements(ctx context.Context, guildID int64, now time.Time) {
	if a.Ranked == nil {
		return
	}
	seasons, err := a.Ranked.ActiveSeasonsForGuild(ctx, guildID)
	if err != nil {
		slog.Warn("component=ranked", "msg", "ranked bonus announcements failed", "err", err.Error())
		return
	}
	for _, season := range seasons {
		settings, err := a.Ranked.BonusSettings(ctx, season.ServerID)
		if err != nil {
			continue
		}
		serverName := ""
		if a.Servers != nil {
			serverName = a.serverName(season.ServerID)
		}
		a.announceWanted(ctx, guildID, season, settings, serverName, now)
		if settings.RankUpCards || settings.RankUpDMs {
			a.announceRankUps(ctx, guildID, season, settings, serverName, now)
		}
		if settings.WeeklyRecap {
			if lastWeek, open := recapWindow(now); open {
				if claimed, err := a.Ranked.ClaimWeeklyRecap(ctx, season.ServerID, lastWeek, now); err == nil && claimed {
					if rc, err := a.Ranked.WeeklyRecap(ctx, season, lastWeek); err == nil && rc.Kills > 0 {
						a.postRankedCard(ctx, guildID, season.ServerID, buildWeeklyRecapCard(rc, serverName))
						slog.Info("component=ranked", "event", "weekly_recap_posted", "server_id", season.ServerID)
					}
				}
			}
		}
	}
	claims, err := a.Ranked.DueBountyClaims(ctx, guildID, now)
	if err != nil {
		slog.Warn("component=ranked", "msg", "bounty claims failed", "err", err.Error())
		return
	}
	ids := make([]int64, 0, len(claims))
	for _, c := range claims {
		a.postRankedCard(ctx, guildID, c.ServerID, buildBountyClaimCard(c))
		ids = append(ids, c.AwardID)
	}
	if len(ids) > 0 {
		if err := a.Ranked.MarkBountyClaimsAnnounced(ctx, ids, now); err != nil {
			slog.Warn("component=ranked", "msg", "bounty claim stamp failed", "err", err.Error())
		}
	}
	if err := a.Ranked.MarkOtherBonusesSeen(ctx, guildID, now); err != nil {
		slog.Warn("component=ranked", "msg", "bonus stamp failed", "err", err.Error())
	}
}

func (a *App) announceWanted(ctx context.Context, guildID int64, season repository.ServerRankedSeason, s repository.RankedBonusSettings, serverName string, now time.Time) {
	current, err := a.Ranked.CurrentWanted(ctx, season, guildID, s, now)
	if err != nil {
		slog.Warn("component=ranked", "msg", "wanted list failed", "server_id", season.ServerID, "err", err.Error())
		return
	}
	fresh, err := a.Ranked.SyncWanted(ctx, season.ID, current, now)
	if err != nil {
		slog.Warn("component=ranked", "msg", "wanted sync failed", "server_id", season.ServerID, "err", err.Error())
		return
	}
	for _, wp := range fresh {
		a.postRankedCard(ctx, guildID, season.ServerID, buildWantedCard(wp, serverName))
	}
}

func (a *App) announceRankUps(ctx context.Context, guildID int64, season repository.ServerRankedSeason, s repository.RankedBonusSettings, serverName string, now time.Time) {
	ups, err := a.Ranked.DueRankUps(ctx, season, guildID, now, rankUpsPerPass)
	if err != nil {
		slog.Warn("component=ranked", "msg", "rank-ups failed", "server_id", season.ServerID, "err", err.Error())
		return
	}
	for _, u := range ups {
		if s.RankUpCards {
			a.postRankedCard(ctx, guildID, season.ServerID, buildRankUpCard(u, serverName))
		}
		if s.RankUpDMs && u.DiscordUserID != "" && a.VIPNotices != nil {
			ch, err := a.VIPNotices.UserChannelCreate(u.DiscordUserID)
			if err == nil {
				_, err = a.VIPNotices.ChannelMessageSendComplex(ch.ID, buildRankUpDM(u, serverName, a.siteURL()+"/dashboard/player"))
			}
			if err != nil {
				slog.Info("component=ranked", "event", "rank_up_dm_failed", "player_id", u.PlayerID, "err", err.Error())
			}
		}
	}
}
