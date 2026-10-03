package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Feature upgrades (docs/FEATURE_UPGRADES.md): optional automations on top of existing features.
// Staff switch them on per server in Client Hub → Growth → Automations; the competitive scheduler
// runs each one's pass (runUpgrades). Every automation is off until switched on.

func (a *App) registerUpgradeRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/upgrades", a.handleUpgradeSettings)
	h("PUT "+adminBase+"/upgrades", a.handleSaveUpgradeSettings)
	a.registerAppealRoutes(adminBase)
	a.registerZoneAlertRoutes(adminBase)
}

// upgradeAdmin is the gate both routes share; it returns the server the request acts on.
func (a *App) upgradeAdmin(w http.ResponseWriter, r *http.Request, capability permissions.Capability, write bool) (adminActor, int64, bool) {
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return ac, 0, false
	}
	if a.Upgrades == nil {
		writeSaaSError(w, codeInternalError, "automations are unavailable")
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

func (a *App) handleUpgradeSettings(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.upgradeAdmin(w, r, permissions.CapFeatureSettingsView, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s, err := a.Upgrades.Settings(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load automations")
		return
	}
	writeSaaSJSON(w, http.StatusOK, s)
}

func (a *App) handleSaveUpgradeSettings(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.upgradeAdmin(w, r, permissions.CapFeatureSettingsManage, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.UpgradeSettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	before, err := a.Upgrades.SaveSettings(ctx, serverID, req, ac.user.DiscordUserID, now)
	if errors.Is(err, repository.ErrUpgradeSettingsInvalid) {
		writeSaaSError(w, codeInvalidRequest, "check the numbers: priority top 0-50, win-back 3-30 days, credits 0-1,000,000")
		return
	}
	if err != nil {
		slog.Warn("component=upgrades", "event", "settings_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save automations")
		return
	}
	a.recordAudit(ctx, ac, "AUTOMATIONS_SAVE", fmt.Sprintf("server:%d", serverID), "", "success", before, req)
	saved, err := a.Upgrades.Settings(ctx, serverID)
	if err != nil {
		saved = req
	}
	a.announceSwitchedOn(ctx, ac.scope.GuildID, serverID, before, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

// upgradeThrottle remembers when a slow pass last ran per key, so a pass that calls Nitrado or
// sends many DMs runs every few minutes instead of on every 45-second scheduler tick.
type upgradeThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (t *upgradeThrottle) due(key string, every time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if last, ok := t.last[key]; ok && now.Sub(last) < every {
		return false
	}
	t.last[key] = now
	return true
}

// runUpgrades runs each automation's pass for the guild's servers.
func (a *App) runUpgrades(ctx context.Context, guildID int64, now time.Time) {
	if a.Upgrades == nil {
		return
	}
	servers, err := a.Upgrades.GuildServers(ctx, guildID)
	if err != nil {
		slog.Warn("component=upgrades", "msg", "automations pass failed", "err", err.Error())
		return
	}
	for _, s := range servers {
		a.runPriorityRewards(ctx, guildID, s, now)
		a.runWinback(ctx, guildID, s, now)
		a.runSeasonRewards(ctx, guildID, s, now)
		a.runRentReminders(ctx, guildID, s, now)
		a.runDailyPlayReward(ctx, guildID, s, now)
		a.runShopOrderUpdates(ctx, guildID, s, now)
		a.runSecurityDigest(ctx, guildID, s, now)
	}
	a.runEventScoreboards(ctx, guildID, servers, now)
	a.runPerkReminders(ctx, guildID, servers, now)
	a.runSpotlight(ctx, now)
}
