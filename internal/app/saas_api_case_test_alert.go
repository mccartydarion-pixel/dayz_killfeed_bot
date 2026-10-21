package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// The "Send test alert" button lets a server owner see exactly what a C.A.S.E.
// staff alert looks like in their own Discord. It posts the real card built
// from made-up sample data, marked TEST, and only into the installation's
// private CASE_ALERTS channel. It never uses player data and enables nothing.

const caseAlertsRoute = "CASE_ALERTS"

// One test per 30 seconds per installation.
var caseTestAlertLimiter = newSaaSRateLimiter(30*time.Second, 1)

type caseTestAlertResponse struct {
	Sent        bool   `json:"sent"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	MessageID   string `json:"messageId"`
	SentAt      string `json:"sentAt"`
}

// privateCaseAlertsChannel resolves the installation's CASE_ALERTS channel and
// requires it to be reachable and private to staff. Real alerts and the test
// alert both go through it.
func privateCaseAlertsChannel(ctx context.Context, routes embedRouteLister, d embedDiscord, organizationID, installationID int64, guildID string) (EmbedDestination, *designerError) {
	dest, derr := resolveEmbedDestination(ctx, routes, d, organizationID, installationID, guildID, caseAlertsRoute)
	if derr != nil {
		if derr.code == codeEmbedRouteNotConfigured {
			derr.message = "no C.A.S.E. alerts channel is set up yet - run Repair Champion Discord Layout to create it"
		}
		return dest, derr
	}
	channels, err := d.ListAllGuildChannels(guildID)
	if err != nil {
		return dest, &designerError{codeDiscordUnavailable, "could not reach Discord right now"}
	}
	byID := make(map[string]discord.RawGuildChannel, len(channels))
	for _, ch := range channels {
		byID[ch.ID] = ch
	}
	ch, parent := byID[dest.ChannelID], byID[byID[dest.ChannelID].ParentID]
	// Same rule the Discord layout uses for C.A.S.E. channels: the category
	// must deny @everyone and neither it nor the channel may allow it back.
	if ch.ParentID == "" || !parent.Private || parent.PublicViewOverride || ch.PublicViewOverride {
		return dest, &designerError{codeEmbedSendForbidden, "#" + dest.ChannelName + " is visible to everyone. Make the C.A.S.E. category private to staff before sending alerts there"}
	}
	return dest, nil
}

// sendCaseTestAlert sends one test card to the private CASE_ALERTS channel.
func sendCaseTestAlert(ctx context.Context, routes embedRouteLister, d embedDiscord, organizationID, installationID int64, guildID, serverName string, at time.Time) (caseTestAlertResponse, *designerError) {
	var out caseTestAlertResponse
	dest, derr := privateCaseAlertsChannel(ctx, routes, d, organizationID, installationID, guildID)
	if derr != nil {
		return out, derr
	}
	messageID, err := d.SendMessage(dest.ChannelID, discord.CaseTestAlertMessage(serverName, at))
	if err != nil {
		return out, &designerError{codeEmbedSendFailed, "Discord rejected the test alert - check Champion's permissions in #" + dest.ChannelName}
	}
	return caseTestAlertResponse{Sent: true, ChannelID: dest.ChannelID, ChannelName: dest.ChannelName, MessageID: messageID, SentAt: at.UTC().Format(time.RFC3339)}, nil
}

// handleCaseSendTestAlert is POST .../admin/case/alerts/test (server owner only).
func (a *App) handleCaseSendTestAlert(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapUAVManage)
	if !ok {
		return
	}
	if ac.level != permissions.LevelOwner {
		writeSaaSError(w, codeAdminForbidden, "server owner required")
		return
	}
	if a.SaaSChannelRoutes == nil || a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}
	if ac.scope.DiscordGuildID == "" {
		writeSaaSError(w, codeInvalidRequest, "this server is not connected to Discord")
		return
	}
	if !caseTestAlertLimiter.Allow(strconv.FormatInt(ac.scope.InstallationID, 10)) {
		writeSaaSError(w, codeEmbedRateLimited, "a test alert was just sent - wait 30 seconds and try again")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resp, derr := sendCaseTestAlert(ctx, a.SaaSChannelRoutes, a.saasDiscordVerifier, ac.scope.OrganizationID, ac.scope.InstallationID,
		ac.scope.DiscordGuildID, "Your server", time.Now())
	if derr != nil {
		a.recordAudit(ctx, ac, "CASE_TEST_ALERT_SENT", "case-alerts", "", "failure", nil, map[string]any{"reason": derr.code})
		writeDesignerError(w, derr)
		return
	}
	slog.Info("component=case", "event", "test_alert_sent", "installation_id", ac.scope.InstallationID, "channel_id", resp.ChannelID, "message_id", resp.MessageID)
	a.recordAudit(ctx, ac, "CASE_TEST_ALERT_SENT", "case-alerts", "", "success", nil, map[string]any{"channelId": resp.ChannelID, "messageId": resp.MessageID})
	writeSaaSJSON(w, http.StatusOK, resp)
}
