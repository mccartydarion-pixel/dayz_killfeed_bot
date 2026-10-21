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
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
)

// Double RP (docs/RANKED_DOUBLE_RP.md): staff open a window during which ranked kills earn twice
// their season's RP. Discord hears about it when it is scheduled, when it starts and when it ends;
// the Player Hub shows it beside the player's rank.

func (a *App) registerRPBoostRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/ranked/boosts", a.handleRPBoosts)
	h("POST "+adminBase+"/ranked/boosts", a.handleCreateRPBoost)
	h("POST "+adminBase+"/ranked/boosts/{boostID}/stop", a.handleStopRPBoost)
}

// rpBoostAdmin runs the gate every double RP route shares and returns the server it acts on.
func (a *App) rpBoostAdmin(w http.ResponseWriter, r *http.Request, capability permissions.Capability, write bool) (adminActor, int64, bool) {
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return ac, 0, false
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.RankedSeasons) {
		return ac, 0, false
	}
	if a.Ranked == nil {
		writeSaaSError(w, codeInternalError, "ranked seasons are unavailable")
		return ac, 0, false
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return ac, 0, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, 0, false
	}
	return ac, *ac.scope.ServerID, true
}

func (a *App) handleRPBoosts(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.rpBoostAdmin(w, r, permissions.CapEventsView, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	season, err := a.Ranked.ActiveServerSeason(ctx, ac.scope.GuildID, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the ranked season")
		return
	}
	boosts, err := a.Ranked.ListRPBoosts(ctx, serverID, now, 20)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load double RP")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"season": season, "boosts": boosts, "maxHours": repository.RPBoostMaxHours, "multiplier": repository.RPBoostMultiplier})
}

type createRPBoostRequest struct {
	Hours int `json:"hours"`
	// StartsAt is when it starts; empty means now.
	StartsAt *time.Time `json:"startsAt"`
}

func (a *App) handleCreateRPBoost(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.rpBoostAdmin(w, r, permissions.CapEventsManage, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[createRPBoostRequest](w, r)
	if !ok {
		return
	}
	if req.Hours < 1 || req.Hours > repository.RPBoostMaxHours {
		writeSaaSError(w, codeInvalidRequest, fmt.Sprintf("double RP lasts 1 to %d hours", repository.RPBoostMaxHours))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	season, err := a.Ranked.ActiveServerSeason(ctx, ac.scope.GuildID, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the ranked season")
		return
	}
	if season == nil {
		writeSaaSError(w, codeInvalidRequest, "start a ranked season first: double RP doubles the RP of ranked kills")
		return
	}
	start := now
	if req.StartsAt != nil && req.StartsAt.After(now) {
		start = req.StartsAt.UTC().Truncate(time.Minute)
	}
	boost, err := a.Ranked.CreateRPBoost(ctx, serverID, start, start.Add(time.Duration(req.Hours)*time.Hour), now, ac.user.DiscordUserID)
	switch {
	case errors.Is(err, repository.ErrRPBoostOverlap), errors.Is(err, repository.ErrRPBoostInvalid):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case err != nil:
		slog.Warn("component=ranked", "event", "rp_boost_create_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not start double RP")
		return
	}
	a.recordAudit(ctx, ac, "RANKED_DOUBLE_RP_START", fmt.Sprintf("rp_boost:%d", boost.ID), "", "success", nil, boost)
	// The card goes out now instead of on the next scheduler pass.
	a.runRPBoostAnnouncements(ctx, ac.scope.GuildID, now)
	writeSaaSJSON(w, http.StatusOK, boost)
}

func (a *App) handleStopRPBoost(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.rpBoostAdmin(w, r, permissions.CapEventsManage, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "boostID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	boost, err := a.Ranked.StopRPBoost(ctx, serverID, id, now, ac.user.DiscordUserID)
	if errors.Is(err, repository.ErrRPBoostNotFound) {
		writeSaaSError(w, codeNotFound, err.Error())
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not stop double RP")
		return
	}
	a.recordAudit(ctx, ac, "RANKED_DOUBLE_RP_STOP", fmt.Sprintf("rp_boost:%d", boost.ID), "", "success", nil, boost)
	a.runRPBoostAnnouncements(ctx, ac.scope.GuildID, now)
	writeSaaSJSON(w, http.StatusOK, boost)
}

// --- Discord ---------------------------------------------------------------------------------------

const rpBoostColor = presentation.Gold // a bonus worth chasing

// buildRPBoostCard is the double RP card for the server's events channel.
func buildRPBoostCard(kind string, b repository.RPBoost, rpPerKill int64, leaders []repository.RPBoostLeader, serverName, hubURL string) *discordgo.MessageEmbed {
	where := serverWord(serverName)
	embed := &discordgo.MessageEmbed{Color: rpBoostColor, Author: rankedCardAuthor()}
	perKill := ""
	if rpPerKill > 0 {
		perKill = fmt.Sprintf(" (%s RP instead of %s)", commaInt(rpPerKill*int64(b.Multiplier)), commaInt(rpPerKill))
	}
	switch kind {
	case "SCHEDULED":
		embed.Title = fmt.Sprintf("⚡ %d× RP is coming", b.Multiplier)
		embed.Description = fmt.Sprintf("Every ranked kill on %s will earn **%d× RP**%s.", where, b.Multiplier, perKill)
		embed.Fields = []*discordgo.MessageEmbedField{
			{Name: "Starts", Value: presentation.TimestampWithRelative(b.StartsAt), Inline: true},
			{Name: "Ends", Value: presentation.Timestamp(b.EndsAt, 'f'), Inline: true},
		}
	case "STARTED":
		embed.Title = fmt.Sprintf("⚡ %d× RP is live", b.Multiplier)
		embed.Description = fmt.Sprintf("Every ranked kill on %s now earns **%d× RP**%s. Get in there!", where, b.Multiplier, perKill)
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "Ends", Value: presentation.TimestampWithRelative(b.EndsAt)}}
	default:
		embed.Title = fmt.Sprintf("%d× RP has ended", b.Multiplier)
		embed.Description = "It ended " + presentation.Timestamp(b.EndsAt, 'R') + ". Ranked kills earn normal RP again."
		if len(leaders) > 0 {
			medals := []string{"🥇", "🥈", "🥉"}
			lines := make([]string, 0, len(leaders))
			for i, l := range leaders {
				if i >= len(medals) {
					break
				}
				lines = append(lines, fmt.Sprintf("%s %s • %s RP from %s", medals[i], orUnknown(l.PlayerName), commaInt(l.RP), presentation.Plural(int64(l.Kills), "kill", "kills")))
			}
			embed.Fields = []*discordgo.MessageEmbedField{{Name: "Most RP during the boost", Value: strings.Join(lines, "\n")}}
		} else {
			embed.Fields = []*discordgo.MessageEmbedField{{Name: "Most RP during the boost", Value: "No ranked kills this time."}}
		}
	}
	if hubURL != "" && kind != "ENDED" {
		// A field, not the footer: Discord shows footers as plain text, so a link there cannot be clicked.
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Track your rank in the Player Hub", Value: hubURL})
	}
	return embed
}

// runRPBoostAnnouncements posts the double RP cards that are due for the guild's servers. A card
// that cannot be posted (no channel routed) is still marked, so it is never posted late.
func (a *App) runRPBoostAnnouncements(ctx context.Context, guildID int64, now time.Time) {
	if a.Ranked == nil {
		return
	}
	due, err := a.Ranked.DueRPBoostAnnouncements(ctx, guildID, now)
	if err != nil {
		slog.Warn("component=ranked", "msg", "double RP announcements failed", "err", err.Error())
		return
	}
	for _, d := range due {
		b := d.Boost
		var rpPerKill int64
		if season, err := a.Ranked.ActiveServerSeason(ctx, guildID, b.ServerID); err == nil && season != nil {
			rpPerKill = season.RPPerKill
		}
		var leaders []repository.RPBoostLeader
		if d.Kind == "ENDED" {
			leaders, _ = a.Ranked.RPBoostLeaders(ctx, b, 3)
		}
		serverName := ""
		if a.Servers != nil {
			serverName = a.serverName(b.ServerID)
		}
		embed := buildRPBoostCard(d.Kind, b, rpPerKill, leaders, serverName, a.siteURL()+"/dashboard/player")
		if a.rpBoostAnnouncer != nil {
			a.rpBoostAnnouncer(b.ServerID, embed)
		} else {
			a.postRPBoostCard(ctx, guildID, b.ServerID, embed)
		}
		if err := a.Ranked.MarkRPBoostAnnounced(ctx, b.ID, d.Kind, now); err != nil {
			slog.Warn("component=ranked", "msg", "double RP announcement stamp failed", "boost_id", b.ID, "err", err.Error())
		}
		slog.Info("component=ranked", "event", "rp_boost_announced", "boost_id", b.ID, "kind", d.Kind)
	}
}

// postRPBoostCard sends the card to the server's events channel, else its server ranks channel.
func (a *App) postRPBoostCard(ctx context.Context, guildID, serverID int64, embed *discordgo.MessageEmbed) {
	if a.ChannelRoutes == nil || a.Discord == nil {
		return
	}
	for _, route := range []string{routing.RouteEvents, routing.RouteServerRanks} {
		channelID, found, err := a.ChannelRoutes.Resolve(ctx, guildID, serverID, route)
		if err != nil || !found || channelID == "" {
			continue
		}
		if _, err := a.feedSender(serverID).ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}); err != nil {
			slog.Warn("component=ranked", "msg", "double RP card failed", "server_id", serverID, "route", route, "err", err.Error())
		}
		return
	}
}
