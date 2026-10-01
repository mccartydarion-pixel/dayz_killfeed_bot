package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Real C.A.S.E. staff alerts. Every minute the worker:
//
//  1. if Suspicious Logins is released (caseintel.ModuleReleased), checks the
//     players who connected or disconnected recently on each server whose
//     owner turned staff alerts on, and queues each new SUSPICIOUS finding
//     the detector says may notify (CanNotify);
//  2. posts queued alerts to the installation's private CASE_ALERTS channel,
//     with mentions disabled, rechecking the owner switch, the release and
//     the channel's privacy right before each send.
//
// No detector is released today, so step 1 queues nothing. Nothing here bans,
// kicks or changes a player.

const (
	caseAlertModuleID     = "CASE-LOGIN-001"
	caseAlertTick         = time.Minute
	caseAlertScanWindow   = 30 * time.Minute
	caseAlertScanPlayers  = 100
	caseAlertClaimBatch   = 10
	caseAlertLease        = 2 * time.Minute
	caseAlertSendTimeout  = 20 * time.Second
	caseAlertEvalTimeout  = 10 * time.Second
	caseAlertOwnerOff     = "OWNER_DISABLED"
	caseAlertNotReleased  = "DETECTOR_NOT_RELEASED"
	caseAlertBadFinding   = "INVALID_FINDING"
	caseAlertSendRejected = "DISCORD_REJECTED"
)

type caseAlertStore interface {
	GetSettings(ctx context.Context, installationID, guildID, serverID int64) (repository.CaseAlertSettings, error)
	EnabledServers(ctx context.Context) ([]repository.CaseAlertServer, error)
	RecentSessionPlayers(ctx context.Context, guildID, serverID int64, since time.Time, limit int) ([]int64, error)
	Enqueue(ctx context.Context, s repository.CaseAlertServer, f caseintel.Finding) (bool, error)
	ClaimDue(ctx context.Context, limit int, leaseFor time.Duration) ([]repository.CaseAlertDelivery, error)
	MarkSent(ctx context.Context, id int64, channelID, messageID string) error
	MarkRetry(ctx context.Context, id int64, code string, after time.Duration) error
	MarkDead(ctx context.Context, id int64, code string) error
}

type caseAlertWorker struct {
	store      caseAlertStore
	routes     embedRouteLister
	discord    embedDiscord
	released   func(moduleID string) bool
	evaluate   func(ctx context.Context, s repository.CaseAlertServer, playerID int64) (caseintel.Core8Result, error)
	serverName func(serverID int64) string
	siteURL    string
	now        func() time.Time
}

func (w *caseAlertWorker) run(ctx context.Context) {
	t := time.NewTicker(caseAlertTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *caseAlertWorker) tick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=case_alerts", "msg", "worker panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	if w.released(caseAlertModuleID) {
		w.scan(ctx)
	}
	w.deliver(ctx)
}

func (w *caseAlertWorker) scan(ctx context.Context) {
	servers, err := w.store.EnabledServers(ctx)
	if err != nil {
		slog.Warn("component=case_alerts", "msg", "list enabled servers failed", "err", err.Error())
		return
	}
	for _, s := range servers {
		players, err := w.store.RecentSessionPlayers(ctx, s.GuildID, s.ServerID, w.now().Add(-caseAlertScanWindow), caseAlertScanPlayers)
		if err != nil {
			slog.Warn("component=case_alerts", "msg", "list recent players failed", "server_id", s.ServerID, "err", err.Error())
			continue
		}
		for _, playerID := range players {
			evalCtx, cancel := context.WithTimeout(ctx, caseAlertEvalTimeout)
			res, err := w.evaluate(evalCtx, s, playerID)
			cancel()
			if err != nil {
				slog.Warn("component=case_alerts", "msg", "evaluate failed", "server_id", s.ServerID, "player_id", playerID, "err", err.Error())
				continue
			}
			if !res.CanNotify {
				continue
			}
			for _, f := range res.Findings {
				if f.Tier != caseintel.TierSuspicious {
					continue
				}
				queued, err := w.store.Enqueue(ctx, s, f)
				if err != nil {
					slog.Warn("component=case_alerts", "msg", "enqueue failed", "server_id", s.ServerID, "err", err.Error())
				} else if queued {
					slog.Info("component=case_alerts", "event", "queued", "server_id", s.ServerID, "player_id", playerID, "detector_id", f.DetectorID)
				}
			}
		}
	}
}

func (w *caseAlertWorker) deliver(ctx context.Context) {
	due, err := w.store.ClaimDue(ctx, caseAlertClaimBatch, caseAlertLease)
	if err != nil {
		slog.Warn("component=case_alerts", "msg", "claim failed", "err", err.Error())
		return
	}
	for _, d := range due {
		sendCtx, cancel := context.WithTimeout(ctx, caseAlertSendTimeout)
		w.deliverOne(sendCtx, d)
		cancel()
	}
}

func (w *caseAlertWorker) deliverOne(ctx context.Context, d repository.CaseAlertDelivery) {
	mark := func(err error) {
		if err != nil {
			slog.Warn("component=case_alerts", "msg", "record delivery outcome failed", "alert_id", d.ID, "err", err.Error())
		}
	}
	settings, err := w.store.GetSettings(ctx, d.InstallationID, d.GuildID, d.ServerID)
	if err != nil {
		mark(w.store.MarkRetry(ctx, d.ID, codeInternalError, caseAlertBackoff(d.Attempts)))
		return
	}
	switch {
	case !settings.Enabled:
		mark(w.store.MarkDead(ctx, d.ID, caseAlertOwnerOff))
		return
	case !w.released(d.DetectorID):
		mark(w.store.MarkDead(ctx, d.ID, caseAlertNotReleased))
		return
	}
	f := d.Finding
	if f.DetectorID != d.DetectorID || f.Scope != (caseintel.Core8Scope{GuildID: d.GuildID, InstallationID: d.InstallationID, ServerID: d.ServerID}) {
		mark(w.store.MarkDead(ctx, d.ID, caseAlertBadFinding))
		return
	}
	dest, derr := privateCaseAlertsChannel(ctx, w.routes, w.discord, d.OrganizationID, d.InstallationID, d.DiscordGuildID)
	if derr != nil {
		// The owner can fix a missing or public channel, so keep retrying
		// (slowly) until the attempts run out.
		mark(w.store.MarkRetry(ctx, d.ID, derr.code, caseAlertBackoff(d.Attempts)))
		return
	}
	msg := caseStaffAlertMessage(f, w.detectorName(f.DetectorID), w.server(d.ServerID), w.reviewURL(f.PlayerID))
	if msg == nil {
		mark(w.store.MarkDead(ctx, d.ID, caseAlertBadFinding))
		return
	}
	messageID, err := w.discord.SendMessage(dest.ChannelID, msg)
	if err != nil {
		slog.Warn("component=case_alerts", "msg", "send failed", "alert_id", d.ID, "err", err.Error())
		mark(w.store.MarkRetry(ctx, d.ID, caseAlertSendRejected, caseAlertBackoff(d.Attempts)))
		return
	}
	mark(w.store.MarkSent(ctx, d.ID, dest.ChannelID, messageID))
	slog.Info("component=case_alerts", "event", "sent", "alert_id", d.ID, "server_id", d.ServerID, "channel_id", dest.ChannelID)
}

func (w *caseAlertWorker) detectorName(id string) string {
	for _, def := range caseintel.ClientCatalog() {
		if def.ID == id {
			return def.Name
		}
	}
	return id
}

func (w *caseAlertWorker) server(id int64) string {
	if w.serverName != nil {
		return w.serverName(id)
	}
	return ""
}

func (w *caseAlertWorker) reviewURL(playerID int64) string {
	base := strings.TrimRight(w.siteURL, "/")
	if !strings.HasPrefix(base, "https://") || playerID <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/dashboard/anti-cheat?tab=players&playerId=%d", base, playerID)
}

// caseAlertBackoff waits 1, 4, 9, 16 minutes between attempts.
func caseAlertBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	return time.Duration(attempts*attempts) * time.Minute
}

// caseStaffAlertMessage wraps the staff card with mentions disabled.
func caseStaffAlertMessage(f caseintel.Finding, detectorName, serverName, reviewURL string) *discordgo.MessageSend {
	embed := discord.BuildCASECore8StaffEmbed(f, detectorName, serverName, reviewURL)
	if embed == nil {
		return nil
	}
	return &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{},
		},
	}
}

// evaluateCaseLoginAlert runs the released Suspicious Logins evaluation for
// one player, exactly as the dashboard's shadow run reads its evidence.
func (a *App) evaluateCaseLoginAlert(ctx context.Context, s repository.CaseAlertServer, playerID int64) (caseintel.Core8Result, error) {
	if !caseEvidenceEnabledForServer(s.ServerID) {
		return caseintel.Core8Result{}, nil
	}
	repo := repository.NewCaseEvidenceRepository(a.DB.Pool)
	events, next, err := repo.ListCaseSessionEvidence(ctx, s.GuildID, s.ServerID, playerID, nil, 200)
	if err != nil {
		return caseintel.Core8Result{}, err
	}
	offset, err := repo.CaseServerUTCOffset(ctx, s.GuildID, s.ServerID)
	if err != nil {
		offset = nil // fail closed: no trusted time, no conclusions
	}
	mode := caseintel.SensitivityBalanced
	if settings, err := repository.NewCaseDetectorSettingsRepository(a.DB.Pool).List(ctx, s.InstallationID, s.GuildID, s.ServerID); err == nil {
		for _, st := range settings {
			if st.ModuleID == caseAlertModuleID {
				mode = st.Sensitivity
			}
		}
	}
	name := ""
	for _, e := range events {
		if e.SubjectID != nil && *e.SubjectID == playerID && e.SubjectName != "" {
			name = e.SubjectName
			break
		}
	}
	return caseintel.EvaluateLoginsForAlert(caseintel.ShadowLoginInput{
		Scope:    caseintel.Core8Scope{GuildID: s.GuildID, InstallationID: s.InstallationID, ServerID: s.ServerID},
		PlayerID: playerID, PlayerName: name, Events: events, UTCOffsetMinutes: offset,
		Telemetry:        a.caseADMTelemetry(s.ServerID, time.Now().UTC()),
		SessionsRetained: true, WindowTruncated: next != nil,
	}, mode), nil
}

// startCaseStaffAlerts starts the worker when the database and the Discord
// bot are available. It sends nothing until a detector is released and an
// owner turns staff alerts on.
func (a *App) startCaseStaffAlerts(ctx context.Context) {
	var d embedDiscord = a.saasDiscordVerifier
	if d == nil && a.Discord != nil {
		d = a.Discord
	}
	if a.DB == nil || a.DB.Pool == nil || a.SaaSChannelRoutes == nil || d == nil {
		return
	}
	site := ""
	if a.Config != nil {
		site = a.Config.SiteBaseURL
	}
	w := &caseAlertWorker{
		store: repository.NewCaseStaffAlertRepository(a.DB.Pool), routes: a.SaaSChannelRoutes, discord: d,
		released: caseintel.ModuleReleased, evaluate: a.evaluateCaseLoginAlert, serverName: a.serverNameFunc(),
		siteURL: site, now: time.Now,
	}
	go w.run(ctx)
}
