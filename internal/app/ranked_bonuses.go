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
	"github.com/yourname/dayz-killfeed/internal/presentation"
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
	rankedWantedColor = presentation.Crimson // a hunt
	rankedRecapColor  = presentation.Neutral // a summary
	rankedRankUpColor = presentation.Gold    // an achievement; the tier is named in the title
	rankUpsPerPass    = 10
)

func rankedCardAuthor() *discordgo.MessageEmbedAuthor {
	return presentation.BrandAuthor("Ranked")
}

func orUnknown(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Unknown player"
	}
	// Cleaned (no pings, no invisible characters), capped and markdown-escaped, like every
	// player name on every card.
	return presentation.SafeName(name, presentation.MaxCardNameRunes)
}

func buildWantedCard(wp repository.WantedPlayer, serverName string) *discordgo.MessageEmbed {
	where := serverWord(serverName)
	name := orUnknown(wp.Name)
	desc := fmt.Sprintf("**%s** is #1 on %s with %s RP.", name, where, commaInt(wp.RP))
	if wp.Reason == repository.WantedStreak {
		desc = fmt.Sprintf("**%s** is on a **%s-kill streak** on %s.", name, commaInt(int64(wp.Streak)), where)
	}
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedWantedColor,
		Title:       "💀 Bounty on " + name,
		Description: desc + fmt.Sprintf("\nTake them down for **+%s RP** on top of the kill.", commaInt(wp.Bounty))}
}

func buildBountyClaimCard(c repository.BountyClaim) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedWantedColor, Title: "🎯 Bounty claimed",
		Description: fmt.Sprintf("**%s** killed **%s**, who %s, and collected the **+%s RP** bounty (%s RP for the kill).",
			orUnknown(c.KillerName), orUnknown(c.VictimName), c.Detail, commaInt(c.BountyRP), commaInt(c.TotalRP)),
		Timestamp: c.At.Format(time.RFC3339)}
}

func buildRankUpCard(u repository.RankUp, serverName string) *discordgo.MessageEmbed {
	// The server is named in the footer, like on every card.
	return &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedRankUpColor,
		Title:       fmt.Sprintf("🏅 %s reached %s", orUnknown(u.Name), repository.TierName(u.To)),
		Description: fmt.Sprintf("%s RP • #%s", commaInt(u.RP), commaInt(int64(u.Position))),
		Footer:      presentation.Footer(serverName, "")}
}

func buildRankUpDM(u repository.RankUp, serverName, hubURL string) *discordgo.MessageSend {
	embed := &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedRankUpColor,
		Title:       "🏅 You reached " + repository.TierName(u.To),
		Description: fmt.Sprintf("You are now %s on %s with %s RP (#%s). Keep climbing!", repository.TierName(u.To), serverWord(serverName), commaInt(u.RP), commaInt(int64(u.Position)))}
	if hubURL != "" {
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "See your rank in the Player Hub", Value: hubURL}}
	}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}

func buildWeeklyRecapCard(rc repository.RankedRecap, serverName string) *discordgo.MessageEmbed {
	end := rc.WeekStart.AddDate(0, 0, 6)
	// A ranked week is seven calendar days in UTC, so its two dates are written out: a Discord
	// timestamp would show the day before to readers west of UTC. The server is named in the
	// footer, like on every card.
	embed := &discordgo.MessageEmbed{Author: rankedCardAuthor(), Color: rankedRecapColor, Title: "📊 Ranked week in review",
		Description: fmt.Sprintf("%s to %s • %s", rc.WeekStart.Format("Jan 2"), end.Format("Jan 2"), presentation.Plural(int64(rc.Kills), "ranked kill", "ranked kills")),
		Footer:      presentation.Footer(serverName, "")}
	if len(rc.Climbers) > 0 {
		medals := []string{"🥇", "🥈", "🥉"}
		lines := []string{}
		for i, l := range rc.Climbers {
			if i >= len(medals) {
				break
			}
			lines = append(lines, fmt.Sprintf("%s %s • +%s RP from %s", medals[i], orUnknown(l.PlayerName), commaInt(l.RP), presentation.Plural(int64(l.Kills), "kill", "kills")))
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Top climbers", Value: strings.Join(lines, "\n")})
	}
	if rc.Bounty != nil {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Biggest bounty",
			Value: fmt.Sprintf("%s collected +%s RP on %s", orUnknown(rc.Bounty.KillerName), commaInt(rc.Bounty.BountyRP), orUnknown(rc.Bounty.VictimName)), Inline: true})
	}
	if rc.BestStreak > 1 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Longest kill streak",
			Value: fmt.Sprintf("%s • %s", orUnknown(rc.StreakName), presentation.Plural(int64(rc.BestStreak), "kill", "kills")), Inline: true})
	}
	if rc.Revenges > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Revenge kills",
			Value: fmt.Sprintf("%s this week • most: %s (%s)", commaInt(int64(rc.Revenges)), orUnknown(rc.RevengeName), commaInt(int64(rc.RevengeMost))), Inline: true})
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
