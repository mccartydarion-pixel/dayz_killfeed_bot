package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// Upgrade 27, zone alert player: staff can pick one player (say, the base owner) who also gets a
// zone's intrusion, UAV and base radar alerts by direct message. It works for a zone with no alert
// channel too, and follows the zone's cooldown. The website shows the intruder's path on the map
// from their location history.

func (a *App) registerZoneAlertRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/zones/{zoneID}/alert-player", a.handleGetZoneAlertPlayer)
	h("PUT "+adminBase+"/zones/{zoneID}/alert-player", a.handlePutZoneAlertPlayer)
}

func (a *App) zoneAlertAdmin(w http.ResponseWriter, r *http.Request, write bool) (adminActor, int64, bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return ac, 0, false
	}
	if a.Upgrades == nil || a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return ac, 0, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, 0, false
	}
	zoneID, ok := pathInt64(w, r, "zoneID")
	if !ok {
		return ac, 0, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return ac, 0, false
	}
	return ac, zoneID, true
}

func (a *App) handleGetZoneAlertPlayer(w http.ResponseWriter, r *http.Request) {
	ac, zoneID, ok := a.zoneAlertAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	p, err := a.Upgrades.ZoneAlertPlayer(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the alert player")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"player": p})
}

type zoneAlertPlayerRequest struct {
	// Name is the player's in-game name; empty clears it.
	Name string `json:"name"`
}

func (a *App) handlePutZoneAlertPlayer(w http.ResponseWriter, r *http.Request) {
	ac, zoneID, ok := a.zoneAlertAdmin(w, r, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[zoneAlertPlayerRequest](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var playerID int64
	if name := strings.TrimSpace(req.Name); name != "" {
		id, err := a.Upgrades.FindPlayerByName(ctx, ac.scope.GuildID, name)
		if err != nil {
			writeSaaSError(w, codeInternalError, "could not look the player up")
			return
		}
		if id == 0 {
			writeSaaSError(w, codeNotFound, "no player with that name has been seen on this server")
			return
		}
		playerID = id
	}
	if err := a.Upgrades.SetZoneAlertPlayer(ctx, ac.scope.InstallationID, zoneID, playerID, ac.user.DiscordUserID); err != nil {
		writeSaaSError(w, codeInternalError, "could not save the alert player")
		return
	}
	a.recordAudit(ctx, ac, "ZONE_ALERT_PLAYER_SET", fmt.Sprintf("zone:%d", zoneID), req.Name, "success", nil, map[string]int64{"playerId": playerID})
	p, _ := a.Upgrades.ZoneAlertPlayer(ctx, ac.scope.InstallationID, zoneID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"player": p})
}

// notifyZoneAlertPlayer DMs the zone's alert player about an entry. It runs off the location
// queue's goroutine, so the DM is sent in the background.
func (a *App) notifyZoneAlertPlayer(ev killfeed.IntrusionEvent) {
	if a.Upgrades == nil || !ev.Alertable || ev.Kind == killfeed.AlertZoneExit {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		target, err := a.Upgrades.ZoneAlertPlayer(ctx, ev.Zone.InstallationID, ev.Zone.ID)
		if err != nil || target == nil || target.PlayerID == ev.PlayerID {
			return
		}
		// One message per zone, intruder and cooldown period.
		cooldown := time.Duration(ev.Zone.CooldownSeconds) * time.Second
		if cooldown < time.Minute {
			cooldown = time.Minute
		}
		ref := fmt.Sprintf("%d:%d:%d", ev.Zone.ID, ev.PlayerID, ev.At.Unix()/int64(cooldown.Seconds()))
		embed := buildIntrusionEmbed(ev) // the same card the zone's channel gets
		a.notifyOnce(ctx, "ZONE_ALERT", ev.Zone.GuildID, ev.Zone.ServerID, target.PlayerID, ref, time.Now().UTC(), dm(embed))
	}()
}
