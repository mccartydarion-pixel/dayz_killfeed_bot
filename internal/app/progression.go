package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Progression (docs/PROGRESSION.md): daily and weekly challenges, the battle pass and territory
// control. Staff set them up per server in Client Hub → Growth → Progression; players follow them
// in the Player Hub. The competitive scheduler runs each one's pass (runProgression). All three
// are off until switched on.

func (a *App) registerProgressionRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/challenges", a.handleAdminChallenges)
	h("PUT "+adminBase+"/challenges", a.handleSaveChallenges)
	h("GET "+adminBase+"/battle-pass", a.handleAdminBattlePass)
	h("POST "+adminBase+"/battle-pass", a.handleCreateBattlePass)
	h("PUT "+adminBase+"/battle-pass/{seasonID}", a.handleUpdateBattlePass)
	h("POST "+adminBase+"/battle-pass/{seasonID}/end", a.handleEndBattlePass)
	h("GET "+adminBase+"/territory", a.handleAdminTerritory)
	h("PUT "+adminBase+"/territory", a.handleSaveTerritory)

	h("GET /api/saas/player/servers/{installationID}/challenges", a.handlePlayerChallenges)
	h("GET /api/saas/player/servers/{installationID}/battle-pass", a.handlePlayerBattlePass)
	h("POST /api/saas/player/servers/{installationID}/battle-pass/premium", a.handleBuyBattlePassPremium)
	h("PUT /api/saas/player/servers/{installationID}/cosmetics", a.handleSetCosmetics)
	h("GET /api/saas/player/servers/{installationID}/territory", a.handlePlayerTerritory)
}

// progressionAdmin is the gate every progression admin route shares; it returns the server the
// request acts on.
func (a *App) progressionAdmin(w http.ResponseWriter, r *http.Request, write bool) (adminActor, int64, bool) {
	capability := permissions.CapEventsView
	if write {
		capability = permissions.CapEventsManage
	}
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return ac, 0, false
	}
	if a.Challenges == nil || a.BattlePass == nil || a.Territory == nil {
		writeSaaSError(w, codeInternalError, "progression is unavailable")
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

// playerCtx is a resolved player request; call cancel when done.
type playerCtx struct {
	scope  repository.PlayerInstallationScope
	ctx    context.Context
	cancel context.CancelFunc
}

// progressionPlayer resolves the acting player for the player routes; writes are rate limited
// like purchases.
func (a *App) progressionPlayer(w http.ResponseWriter, r *http.Request, write bool) (playerCtx, bool) {
	if a.Challenges == nil || a.BattlePass == nil || a.Territory == nil {
		writeSaaSError(w, codeInternalError, "progression is unavailable")
		return playerCtx{}, false
	}
	if write && a.saasShopPurchaseLimiter != nil && !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return playerCtx{}, false
	}
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return playerCtx{}, false
	}
	return playerCtx{scope: scope, ctx: ctx, cancel: cancel}, true
}

// Progression passes run at most this often per server.
const (
	territoryEvery  = time.Minute
	challengesEvery = 2 * time.Minute
	battlePassEvery = 2 * time.Minute
)

// runProgression runs territory first (challenges count its kills), then challenges (the battle pass
// counts their XP), then the battle pass.
func (a *App) runProgression(ctx context.Context, guildID int64, now time.Time) {
	if a.Challenges == nil || a.BattlePass == nil || a.Territory == nil {
		return
	}
	a.runTerritory(ctx, guildID, now)
	a.runChallenges(ctx, guildID, now)
	a.runBattlePass(ctx, guildID, now)
}

// postProgressionCard posts a progression card where ranked cards go (events channel, else ranks).
func (a *App) postProgressionCard(ctx context.Context, guildID, serverID int64, embed *discordgo.MessageEmbed) {
	a.postRankedCard(ctx, guildID, serverID, embed)
}

func progressionWarn(what string, serverID int64, err error) {
	slog.Warn("component=progression", "msg", what+" failed", "server_id", serverID, "err", err.Error())
}

func pointsText(n int64) string {
	return fmt.Sprintf("%s Champion Points", commaInt(n))
}

// commaInt is 12,345.
func commaInt(n int64) string { return presentation.FormatThousands(n) }
