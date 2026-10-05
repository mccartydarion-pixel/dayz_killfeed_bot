package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The owner-ops worker (docs/OWNER_OPS.md): a small loop that watches every installation's
// server, opens and closes incidents, and - only for what the owner switched on in the Owner
// Hub - restarts broken workers, messages the platform admins and the affected customer, and
// sends the daily briefing.
//
// Watching is always on and changes nothing but the incident list. Acting and messaging are
// off until switched on. Every automatic action is claimed with one conditional UPDATE, so
// two instances never both act, and is recorded in platform_audit_log as actor "system".

const (
	ownerOpsWorkerName = "owner-ops"
	ownerOpsInterval   = 2 * time.Minute
	ownerOpsSystem     = "system"
	// broadcastSendGap spaces the Discord copies of a broadcast.
	broadcastSendGap = 400 * time.Millisecond
)

// runOwnerOps runs the monitor until ctx ends.
func (a *App) runOwnerOps(ctx context.Context) {
	if a.PlatformOps == nil {
		return
	}
	if a.Workers != nil {
		a.Workers.Register(ownerOpsWorkerName)
		defer a.Workers.Stop(ownerOpsWorkerName)
	}
	ticker := time.NewTicker(ownerOpsInterval)
	defer ticker.Stop()
	// The registry calls a worker stalled after 30 seconds of silence, far shorter than a tick.
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if a.Workers != nil {
				a.Workers.Heartbeat(ownerOpsWorkerName)
			}
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			err := a.ownerOpsTick(tickCtx, time.Now().UTC())
			cancel()
			if a.Workers != nil {
				if err != nil {
					a.Workers.Error(ownerOpsWorkerName, err)
				} else {
					a.Workers.Heartbeat(ownerOpsWorkerName)
				}
			}
			if err != nil {
				slog.Warn("component=owner_ops", "event", "tick_failed", "err", err.Error())
			}
		}
	}
}

func ownerOpsKey(installationID int64, kind string) string {
	return fmt.Sprintf("%d|%s", installationID, kind)
}

// ownerOpsFirstSeen records when a problem was first observed and reports whether it has
// lasted long enough to count.
func (a *App) ownerOpsFirstSeen(key string, now time.Time) bool {
	a.ownerOpsMu.Lock()
	defer a.ownerOpsMu.Unlock()
	if a.ownerOpsSeen == nil {
		a.ownerOpsSeen = map[string]time.Time{}
	}
	first, ok := a.ownerOpsSeen[key]
	if !ok {
		a.ownerOpsSeen[key] = now
		first = now
	}
	return now.Sub(first) >= ownerops.DetectGrace
}

func (a *App) ownerOpsForget(installationID int64, kind string) {
	a.ownerOpsMu.Lock()
	delete(a.ownerOpsSeen, ownerOpsKey(installationID, kind))
	a.ownerOpsMu.Unlock()
}

// ownerOpsTick is one pass of the monitor.
func (a *App) ownerOpsTick(ctx context.Context, now time.Time) error {
	settings, _, err := a.PlatformOps.Settings(ctx)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	// While the process is still starting its workers nothing can be concluded: neither that a
	// worker is missing nor that an open incident has cleared.
	if a.ownerOpsRuntimeReady() {
		if err := a.ownerOpsMonitor(ctx, now, settings); err != nil {
			return err
		}
	}
	if settings.BriefingEnabled && now.Hour() >= settings.BriefingHourUTC {
		a.ownerOpsBriefing(ctx, now)
	}
	return nil
}

func (a *App) ownerOpsMonitor(ctx context.Context, now time.Time, settings repository.OwnerOpsSettings) error {
	facts, err := a.PlatformOps.FleetFacts(ctx, now, false)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	open, err := a.PlatformOps.OpenIncidents(ctx)
	if err != nil {
		return fmt.Errorf("incidents: %w", err)
	}
	openBy := map[string]repository.PlatformIncident{}
	for _, inc := range open {
		openBy[ownerOpsKey(inc.InstallationID, inc.Kind)] = inc
	}
	seenInstallations := map[int64]bool{}
	for _, f := range facts {
		seenInstallations[f.InstallationID] = true
		implied := ownerops.Detect(a.watchedServerState(f, a.serverRuntime(f.ServerID, now), true, now))
		for _, kind := range ownerops.Kinds {
			key := ownerOpsKey(f.InstallationID, kind)
			detail, broken := implied[kind]
			existing, isOpen := openBy[key]
			switch {
			case broken:
				if !isOpen && !a.ownerOpsFirstSeen(key, now) {
					continue // not yet lasting: one slow poll is not an incident
				}
				incident := existing
				if !isOpen {
					opened := false
					if incident, opened, err = a.PlatformOps.OpenIncident(ctx, f.InstallationID, f.OrganizationID, kind, detail); err != nil {
						slog.Warn("component=owner_ops", "event", "open_failed", "installation_id", f.InstallationID, "kind", kind, "err", err.Error())
						continue
					}
					if incident.ID == 0 {
						continue
					}
					if opened {
						slog.Info("component=owner_ops", "event", "incident_opened", "incident_id", incident.ID, "installation_id", f.InstallationID, "kind", kind)
						a.ownerOpsAudit(ctx, "incident.opened", incident, "detected by the fleet monitor", "OK", map[string]any{"detail": detail})
					}
				}
				a.ownerOpsHandleOpen(ctx, settings, f, incident)
			case isOpen:
				a.ownerOpsForget(f.InstallationID, kind)
				a.ownerOpsResolve(ctx, settings, f, existing)
			default:
				a.ownerOpsForget(f.InstallationID, kind)
			}
		}
	}
	// An incident whose installation is gone can never clear by itself.
	for _, inc := range open {
		if !seenInstallations[inc.InstallationID] {
			if _, _, err := a.PlatformOps.ResolveIncident(ctx, inc.ID, "the installation no longer exists"); err != nil {
				slog.Warn("component=owner_ops", "event", "resolve_failed", "incident_id", inc.ID, "err", err.Error())
			}
		}
	}
	return nil
}

// ownerOpsHandleOpen does what the owner switched on for one open incident: tell the admins,
// try to heal, tell the customer.
func (a *App) ownerOpsHandleOpen(ctx context.Context, settings repository.OwnerOpsSettings, f repository.FleetFact, incident repository.PlatformIncident) {
	if settings.AlertsEnabled && incident.OwnerNotifiedAt == nil {
		if claimed, err := a.PlatformOps.ClaimIncidentNotice(ctx, incident.ID, false); err == nil && claimed {
			a.ownerOpsNotifyAdmins(fmt.Sprintf("**Incident opened: %s**\n%s / %s (installation %d)\n%s", ownerops.KindLabel(incident.Kind),
				f.OrganizationName, serverLabel(f), f.InstallationID, incident.Detail))
		}
	}
	if settings.SelfHealEnabled && ownerops.Healable(incident.Kind) && f.ServerID > 0 {
		claimed, err := a.PlatformOps.ClaimIncidentAction(ctx, incident.ID, "worker_restart", ownerops.HealMaxAttempts, ownerops.HealMinGap)
		if err != nil {
			slog.Warn("component=owner_ops", "event", "claim_failed", "incident_id", incident.ID, "err", err.Error())
		} else if claimed {
			result := "OK"
			a.DisconnectServer(f.ServerID)
			if err := a.RepairServer(context.WithoutCancel(ctx), f.ServerID); err != nil {
				result = "FAILED"
				slog.Warn("component=owner_ops", "event", "self_heal_failed", "incident_id", incident.ID, "server_id", f.ServerID, "err", err.Error())
			} else {
				slog.Info("component=owner_ops", "event", "self_heal_restarted", "incident_id", incident.ID, "server_id", f.ServerID, "attempt", incident.Attempts+1)
			}
			a.ownerOpsAudit(ctx, "incident.self_heal", incident, "automatic worker restart", result, map[string]any{"attempt": incident.Attempts + 1})
		}
	}
	if settings.CustomerNoticesEnabled && ownerops.NeedsCustomer(incident.Kind) && incident.CustomerNotifiedAt == nil {
		if claimed, err := a.PlatformOps.ClaimIncidentNotice(ctx, incident.ID, true); err == nil && claimed {
			a.ownerOpsNotifyCustomer(ctx, f, customerIncidentMessage(incident.Kind, serverLabel(f), a.dashboardURL()))
		}
	}
}

func (a *App) ownerOpsResolve(ctx context.Context, settings repository.OwnerOpsSettings, f repository.FleetFact, incident repository.PlatformIncident) {
	resolution := "recovered on its own"
	if incident.Attempts > 0 {
		resolution = fmt.Sprintf("recovered after %d automatic restart(s)", incident.Attempts)
	}
	if !f.ServerActive || (f.Status != repository.InstallationReady && f.Status != repository.InstallationDegraded) {
		resolution = "no longer applies: the installation is " + strings.ToLower(f.Status)
	}
	closed, resolved, err := a.PlatformOps.ResolveIncident(ctx, incident.ID, resolution)
	if err != nil || !resolved {
		if err != nil {
			slog.Warn("component=owner_ops", "event", "resolve_failed", "incident_id", incident.ID, "err", err.Error())
		}
		return
	}
	slog.Info("component=owner_ops", "event", "incident_resolved", "incident_id", incident.ID, "kind", incident.Kind, "resolution", resolution)
	a.ownerOpsAudit(ctx, "incident.resolved", closed, resolution, "OK", nil)
	// Only tell people it is fixed if they were told it was broken.
	if settings.AlertsEnabled && incident.OwnerNotifiedAt != nil {
		a.ownerOpsNotifyAdmins(fmt.Sprintf("**Incident resolved: %s**\n%s / %s\n%s after %s.", ownerops.KindLabel(incident.Kind),
			f.OrganizationName, serverLabel(f), resolution, time.Since(incident.DetectedAt).Round(time.Minute)))
	}
	if settings.CustomerNoticesEnabled && incident.CustomerNotifiedAt != nil {
		a.ownerOpsNotifyCustomer(ctx, f, "Champion is reading your DayZ server "+serverLabel(f)+" again. Nothing more is needed from you.")
	}
}

func serverLabel(f repository.FleetFact) string {
	if f.ServerName != "" {
		return f.ServerName
	}
	return fmt.Sprintf("server %d", f.ServerID)
}

// customerIncidentMessage is the DM an organization's owner gets when only they can fix it.
func customerIncidentMessage(kind, server, dashboard string) string {
	link := ""
	if dashboard != "" {
		link = "\n" + dashboard
	}
	switch kind {
	case ownerops.KindNitradoAccess:
		return "Champion can no longer read the logs of your DayZ server " + server + ": Nitrado is refusing the saved access token, so your killfeed has stopped. " +
			"Reconnect Nitrado from your Champion dashboard to bring it back." + link
	case ownerops.KindDiscordAccess:
		return "The Champion bot is no longer in the Discord server for " + server + ", so nothing can be posted there. " +
			"Add the bot again from your Champion dashboard." + link
	}
	return "Champion found a problem with your DayZ server " + server + "." + link
}

func (a *App) dashboardURL() string {
	if a.Config == nil {
		return ""
	}
	base := strings.TrimRight(a.Config.SiteBaseURL, "/")
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return base + "/dashboard"
}

func (a *App) ownerOpsAudit(ctx context.Context, action string, incident repository.PlatformIncident, reason, result string, after map[string]any) {
	if after == nil {
		after = map[string]any{}
	}
	after["incidentId"], after["kind"] = incident.ID, incident.Kind
	org := incident.OrganizationID
	a.ownerAudit(ctx, adminIdentity{DiscordID: ownerOpsSystem}, action, "installation", incident.InstallationID, &org, reason, result, nil, after)
}

// --- messaging --------------------------------------------------------------------------------------

func (a *App) ownerOpsCanDM() bool {
	if a.ownerOpsDM != nil {
		return true
	}
	return a.Discord != nil && a.Discord.Session() != nil
}

// ownerOpsSend DMs one Discord user. Mentions are disabled: the text can carry customer names.
func (a *App) ownerOpsSend(discordUserID, text string) error {
	if a.ownerOpsDM != nil {
		return a.ownerOpsDM(discordUserID, text)
	}
	if a.Discord == nil || a.Discord.Session() == nil {
		return fmt.Errorf("discord is not connected")
	}
	session := a.Discord.Session()
	ch, err := session.UserChannelCreate(discordUserID)
	if err != nil {
		return err
	}
	_, err = session.ChannelMessageSendComplex(ch.ID, &discordgo.MessageSend{Content: text, AllowedMentions: &discordgo.MessageAllowedMentions{}})
	return err
}

// ownerOpsNotifyAdmins DMs every platform admin; it returns how many were reached.
func (a *App) ownerOpsNotifyAdmins(text string) int {
	if a.Config == nil {
		return 0
	}
	delivered := 0
	for _, id := range a.Config.AdminDiscordIDs {
		if err := a.ownerOpsSend(id, text); err != nil {
			slog.Warn("component=owner_ops", "event", "admin_dm_failed", "err", err.Error())
			continue
		}
		delivered++
	}
	return delivered
}

// ownerOpsNotifyCustomer DMs the owner of the organization an installation belongs to.
func (a *App) ownerOpsNotifyCustomer(ctx context.Context, f repository.FleetFact, text string) {
	discordID, _, _, found, err := a.PlatformOps.ViewAsTarget(ctx, f.OrganizationID)
	if err != nil || !found || discordID == "" {
		return
	}
	if err := a.ownerOpsSend(discordID, text); err != nil {
		slog.Warn("component=owner_ops", "event", "customer_dm_failed", "organization_id", f.OrganizationID, "err", err.Error())
	}
}

// ownerOpsBriefing sends today's briefing once.
func (a *App) ownerOpsBriefing(ctx context.Context, now time.Time) {
	b, err := a.buildBriefing(ctx, now)
	if err != nil {
		slog.Warn("component=owner_ops", "event", "briefing_failed", "err", err.Error())
		return
	}
	text := ownerops.BriefingText(b)
	claimed, err := a.PlatformOps.ClaimBriefing(ctx, now, text)
	if err != nil || !claimed {
		return
	}
	delivered := a.ownerOpsNotifyAdmins(text)
	if err := a.PlatformOps.SetBriefingDelivered(ctx, now, delivered); err != nil {
		slog.Warn("component=owner_ops", "event", "briefing_mark_failed", "err", err.Error())
	}
	slog.Info("component=owner_ops", "event", "briefing_sent", "delivered", delivered)
}

// --- broadcast delivery -----------------------------------------------------------------------------

func broadcastMessage(b repository.PlatformBroadcast) string {
	prefix := "Champion notice"
	switch b.Severity {
	case repository.BroadcastMaintenance:
		prefix = "Champion maintenance"
	case repository.BroadcastIncident:
		prefix = "Champion incident"
	}
	text := "**" + prefix + ": " + b.Title + "**\n" + b.Body
	if r := []rune(text); len(r) > 1900 {
		text = string(r[:1900])
	}
	return text
}

// deliverBroadcast posts a broadcast's Discord copy to each target installation's staff alerts
// channel, once. An installation with no such channel is recorded as NO_CHANNEL and skipped.
func (a *App) deliverBroadcast(ctx context.Context, b repository.PlatformBroadcast) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	targets, err := a.PlatformOps.BroadcastTargets(ctx, b.ID, b.AudiencePlan)
	if err != nil {
		slog.Warn("component=owner_ops", "event", "broadcast_targets_failed", "broadcast_id", b.ID, "err", err.Error())
		return
	}
	text := broadcastMessage(b)
	sent, failed, skipped := 0, 0, 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		status := "FAILED"
		if t.ChannelID == "" {
			status = "NO_CHANNEL"
		}
		claimed, err := a.PlatformOps.ClaimBroadcastDelivery(ctx, b.ID, t.InstallationID, status)
		if err != nil || !claimed {
			continue
		}
		if t.ChannelID == "" {
			skipped++
			continue
		}
		if err := a.ownerOpsPost(t.ChannelID, text); err != nil {
			failed++
			slog.Warn("component=owner_ops", "event", "broadcast_post_failed", "broadcast_id", b.ID, "installation_id", t.InstallationID, "err", err.Error())
		} else {
			sent++
			_ = a.PlatformOps.SetBroadcastDelivery(ctx, b.ID, t.InstallationID, "SENT")
		}
		select {
		case <-ctx.Done():
		case <-time.After(broadcastSendGap):
		}
	}
	slog.Info("component=owner_ops", "event", "broadcast_delivered", "broadcast_id", b.ID, "sent", sent, "failed", failed, "no_channel", skipped)
}

// ownerOpsPost sends a plain message to a channel with every mention disabled.
func (a *App) ownerOpsPost(channelID, text string) error {
	if a.ownerOpsChannelPost != nil {
		return a.ownerOpsChannelPost(channelID, text)
	}
	if a.Discord == nil || a.Discord.Session() == nil {
		return fmt.Errorf("discord is not connected")
	}
	_, err := a.Discord.Session().ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Content: text, AllowedMentions: &discordgo.MessageAllowedMentions{}})
	return err
}
