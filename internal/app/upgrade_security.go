package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 26: (a) a weekly security digest for staff (C.A.S.E. cases, staff alerts and shadow
// verdicts, raid alarms, Perimeter Watch, zone intrusions, appeals), posted to the staff alerts
// channel on Mondays; (b) appeals: a player sends staff an appeal (a ban, a case, anything else)
// from the Player Hub, staff see it in the anti-cheat workspace and answer it, and the player
// hears the answer by direct message.

func buildSecurityDigest(w repository.SecurityWeek, week time.Time, serverID, guildID int64) discord.AdminAlert {
	end := week.AddDate(0, 0, 6)
	return discord.AdminAlert{GuildRowID: guildID, ServerID: serverID, Kind: discord.AlertKindSecurityDigest, Severity: discord.AlertInfo,
		Headline: "SECURITY WEEK " + strings.ToUpper(week.Format("Jan 2")) + " TO " + strings.ToUpper(end.Format("Jan 2")),
		Detail:   "What C.A.S.E. and base security saw last week.",
		Fields: [][2]string{
			{"C.A.S.E. cases", fmt.Sprintf("%d opened · %d closed · %d waiting for review", w.CasesOpened, w.CasesClosed, w.CasesPending)},
			{"Staff alerts", fmt.Sprintf("%d alerts · %d shadow verdicts", w.StaffAlerts, w.ShadowVerdicts)},
			{"Bases", fmt.Sprintf("%d raid alarms · %d Perimeter Watch alerts", w.RaidAlarms, w.PerimeterAlerts)},
			{"Zones", fmt.Sprintf("%d intrusions", w.ZoneIntrusions)},
			{"Appeals", fmt.Sprintf("%d new", w.AppealsOpened)},
		}}
}

func (a *App) runSecurityDigest(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	if !s.Settings.CaseWeeklyDigest || a.AdminAlerts == nil {
		return
	}
	lastWeek, open := recapWindow(now)
	if !open {
		return
	}
	if claimed, err := a.Upgrades.ClaimNotice(ctx, "SECURITY_DIGEST", s.ServerID, 0, lastWeek.Format("2006-01-02"), now); err != nil || !claimed {
		return
	}
	w, err := a.Upgrades.SecurityWeekCounts(ctx, s.ServerID, lastWeek, lastWeek.AddDate(0, 0, 7))
	if err != nil {
		slog.Warn("component=upgrades", "msg", "security digest failed", "server_id", s.ServerID, "err", err.Error())
		return
	}
	a.AdminAlerts.Publish(buildSecurityDigest(w, lastWeek, s.ServerID, guildID))
}

// --- appeals ---------------------------------------------------------------------------------------

var appealTopics = map[string]string{"BAN": "A ban", "CASE": "A C.A.S.E. case", "OTHER": "Something else"}

func (a *App) registerAppealRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/appeals", a.handleStaffAppeals)
	h("POST "+adminBase+"/appeals/{appealID}/decide", a.handleDecideAppeal)
	h("GET /api/saas/player/servers/{installationID}/appeals", a.handlePlayerAppeals)
	h("POST /api/saas/player/servers/{installationID}/appeals", a.handleCreateAppeal)
}

func (a *App) appealsOn(ctx context.Context, serverID int64) bool {
	if a.Upgrades == nil {
		return false
	}
	s, err := a.Upgrades.Settings(ctx, serverID)
	return err == nil && s.CaseAppeals
}

type createAppealRequest struct {
	Topic   string `json:"topic"`
	Message string `json:"message"`
}

func (a *App) handlePlayerAppeals(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	resp := map[string]any{"enabled": false, "items": []repository.Appeal{}}
	if a.Upgrades == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	list, err := a.Upgrades.PlayerAppeals(ctx, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "appeals", err)
		return
	}
	resp["enabled"], resp["items"] = a.appealsOn(ctx, scope.ServerID), list
	writeSaaSJSON(w, http.StatusOK, resp)
}

func (a *App) handleCreateAppeal(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	req, ok := decodeJSONBody[createAppealRequest](w, r)
	if !ok {
		return
	}
	topic := strings.ToUpper(strings.TrimSpace(req.Topic))
	message := strings.TrimSpace(req.Message)
	if _, known := appealTopics[topic]; !known || len([]rune(message)) < 10 || len([]rune(message)) > 1000 {
		writeSaaSError(w, codeInvalidRequest, "pick what the appeal is about and explain it in 10 to 1,000 characters")
		return
	}
	if !a.appealsOn(ctx, scope.ServerID) {
		writeSaaSError(w, codeConflict, "this server does not take appeals here")
		return
	}
	appeal, err := a.Upgrades.CreateAppeal(ctx, scope.GuildID, scope.ServerID, scope.PlayerID, topic, message)
	if errors.Is(err, repository.ErrAppealOpen) {
		writeSaaSError(w, codeConflict, err.Error())
		return
	}
	if err != nil {
		playerFailed(w, "create appeal", err)
		return
	}
	if a.AdminAlerts != nil {
		a.AdminAlerts.Publish(discord.AdminAlert{GuildRowID: scope.GuildID, ServerID: scope.ServerID, Kind: discord.AlertKindPlayerAppeal, Severity: discord.AlertWarning,
			Headline: "NEW APPEAL", Detail: "A player sent an appeal from the Player Hub. Answer it in Client Hub → Anti-cheat → Appeals.",
			Fields: [][2]string{{"Player", orUnknown(appeal.PlayerName)}, {"About", appealTopics[topic]}, {"Message", truncateRunes(message, 900)}}})
	}
	writeSaaSJSON(w, http.StatusCreated, appeal)
}

func (a *App) handleStaffAppeals(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.upgradeAdmin(w, r, permissions.CapPlayerDirectoryView, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	list, err := a.Upgrades.ServerAppeals(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load appeals")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"enabled": a.appealsOn(ctx, serverID), "items": list})
}

type decideAppealRequest struct {
	Accept bool   `json:"accept"`
	Note   string `json:"note"`
}

func (a *App) handleDecideAppeal(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.upgradeAdmin(w, r, permissions.CapBanlistManage, true)
	if !ok {
		return
	}
	id, ok := pathInt64(w, r, "appealID")
	if !ok {
		return
	}
	req, ok := decodeJSONBody[decideAppealRequest](w, r)
	if !ok {
		return
	}
	note := truncateRunes(strings.TrimSpace(req.Note), 500)
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	appeal, err := a.Upgrades.DecideAppeal(ctx, serverID, id, req.Accept, note, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrAppealNotFound) {
		writeSaaSError(w, codeNotFound, err.Error())
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not answer the appeal")
		return
	}
	a.recordAudit(ctx, ac, "APPEAL_"+appeal.Status, fmt.Sprintf("appeal:%d", appeal.ID), note, "success", nil, appeal)
	title, desc := "✅ Your appeal was accepted", "Staff accepted your appeal."
	if !req.Accept {
		title, desc = "❌ Your appeal was not accepted", "Staff looked at your appeal and did not accept it."
	}
	if note != "" {
		desc += "\n\n> " + note
	}
	_ = a.sendPlayerDM(ctx, appeal.GuildID, appeal.PlayerID, dm(upgradeEmbed("CHAMPIONS® APPEALS", title, desc, 0x3B82F6)))
	writeSaaSJSON(w, http.StatusOK, appeal)
}
