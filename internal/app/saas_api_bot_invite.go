package app

import (
	"net/http"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

// championInvitePermissions is what the "Add Champion" invite asks for. Champion creates its own
// categories (some private), channels, roles and webhooks and posts across them, so it asks for
// Administrator rather than a list that silently falls short as features grow.
const championInvitePermissions = discordgo.PermissionAdministrator

// BotInviteSummary is what the website needs to build the "Add Champion" invite link. It comes
// from the running bot itself, so the link always names the bot that will actually answer.
type BotInviteSummary struct {
	ApplicationID string `json:"applicationId"`
	Permissions   string `json:"permissions"`
}

// handleBotInvite is GET /api/saas/discord/bot-invite.
func (a *App) handleBotInvite(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	appID := ""
	if a.saasDiscordVerifier != nil {
		appID = a.saasDiscordVerifier.BotID() // a bot's user ID is its application ID
	}
	if appID == "" && a.Config != nil {
		appID = a.Config.DiscordApplicationID
	}
	if appID == "" {
		writeSaaSError(w, codeInternalError, "the bot is not connected to Discord yet")
		return
	}
	writeSaaSJSON(w, http.StatusOK, BotInviteSummary{ApplicationID: appID, Permissions: strconv.FormatInt(championInvitePermissions, 10)})
}
