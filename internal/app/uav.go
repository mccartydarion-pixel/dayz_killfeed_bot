package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The live map UAV (docs/LIVE_MAP.md "UAV"): bought with Champion Points in the live map, it shows
// the buyer (and, when the owner shares it, their faction) every connected player for the time
// bought. Basic: a dot per player. Precision: the dot plus name, faction tag, the weapon of their
// latest kill this life, time alive, kills this life and heading. Off until the owner turns it on.

const uavHistoryDays = 30

func (a *App) registerUAVRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/map/uav", a.handleAdminUAV)
	h("PUT "+adminBase+"/map/uav", a.handleSaveUAV)
	h("GET /api/saas/player/servers/{installationID}/map/uav", a.handlePlayerUAV)
	h("POST /api/saas/player/servers/{installationID}/map/uav", a.handleBuyUAV)
}

func (a *App) uavAdmin(w http.ResponseWriter, r *http.Request, write bool) (adminActor, int64, bool) {
	capability := permissions.CapFeatureSettingsView
	limiter := a.saasAdminReadLimiter
	if write {
		capability, limiter = permissions.CapFeatureSettingsManage, a.saasAdminActionLimiter
	}
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return ac, 0, false
	}
	if a.UAV == nil {
		writeSaaSError(w, codeInternalError, "the UAV is unavailable")
		return ac, 0, false
	}
	if ac.scope.ServerID == nil || *ac.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return ac, 0, false
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return ac, 0, false
	}
	return ac, *ac.scope.ServerID, true
}

func (a *App) handleAdminUAV(w http.ResponseWriter, r *http.Request) {
	_, serverID, ok := a.uavAdmin(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s, err := a.UAV.Settings(ctx, serverID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the UAV")
		return
	}
	sales, err := a.UAV.Sales(ctx, serverID, time.Now().UTC().AddDate(0, 0, -uavHistoryDays), 15)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the UAV")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"settings": s, "sales": sales, "salesDays": uavHistoryDays})
}

func (a *App) handleSaveUAV(w http.ResponseWriter, r *http.Request) {
	ac, serverID, ok := a.uavAdmin(w, r, true)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[repository.UAVSettings](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, _ := a.UAV.Settings(ctx, serverID)
	saved, err := a.UAV.SaveSettings(ctx, serverID, req, ac.user.DiscordUserID, time.Now().UTC())
	if errors.Is(err, repository.ErrUAVSettingsInvalid) {
		writeSaaSError(w, codeInvalidRequest, "a block is 5 to 120 minutes, prices are 1 to 10,000,000 points, 1 to 48 blocks a purchase and a delay of 0 to 600 seconds")
		return
	}
	if err != nil {
		slog.Warn("component=live_map", "event", "uav_settings_save_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save the UAV")
		return
	}
	a.recordAudit(ctx, ac, "UAV_SETTINGS_SAVE", fmt.Sprintf("server:%d", serverID), "", "success", before, saved)
	writeSaaSJSON(w, http.StatusOK, saved)
}

// uavOfferDTO is what a player sees of the owner's settings.
type uavOfferDTO struct {
	Enabled          bool  `json:"enabled"`
	BlockMinutes     int   `json:"blockMinutes"`
	BasicPrice       int64 `json:"basicPrice"`
	PrecisionEnabled bool  `json:"precisionEnabled"`
	PrecisionPrice   int64 `json:"precisionPrice"`
	GhostEnabled     bool  `json:"ghostEnabled"`
	GhostPrice       int64 `json:"ghostPrice"`
	MaxBlocks        int   `json:"maxBlocks"`
	DelaySeconds     int   `json:"delaySeconds"`
	ShareFaction     bool  `json:"shareFaction"`
}

type uavPassDTO struct {
	Tier      string `json:"tier"`
	EndsAt    string `json:"endsAt"`
	BuyerName string `json:"buyerName"`
	Mine      bool   `json:"mine"`
}

// uavPlayerDTO is one player under the UAV. Basic sends only the position (and whether it is the
// viewer or a faction mate, so the map does not draw them twice); precision adds the rest.
type uavPlayerDTO struct {
	Key          string   `json:"key"`
	X            float64  `json:"x"`
	Z            float64  `json:"z"`
	AgeSeconds   int64    `json:"ageSeconds"`
	Self         bool     `json:"self,omitempty"`
	Mate         bool     `json:"mate,omitempty"`
	Name         string   `json:"name,omitempty"`
	FactionTag   string   `json:"factionTag,omitempty"`
	LastWeapon   string   `json:"lastWeapon,omitempty"`
	WeaponAgeSec *int64   `json:"lastWeaponAgeSeconds,omitempty"`
	AliveSeconds *int64   `json:"aliveSeconds,omitempty"`
	LifeKills    *int     `json:"lifeKills,omitempty"`
	Heading      *float64 `json:"heading,omitempty"` // degrees clockwise from north
}

type uavViewDTO struct {
	Offer uavOfferDTO `json:"offer"`
	Pass  *uavPassDTO `json:"pass"`
	// Ghost is the viewer's own running ghost (they are hidden from other players' UAVs).
	Ghost   *uavPassDTO    `json:"ghost"`
	Players []uavPlayerDTO `json:"players"`
	Balance *int64         `json:"balance,omitempty"`
}

func toUAVOffer(s repository.UAVSettings) uavOfferDTO {
	return uavOfferDTO{Enabled: s.Enabled, BlockMinutes: s.BlockMinutes, BasicPrice: s.BasicPrice, PrecisionEnabled: s.PrecisionEnabled,
		PrecisionPrice: s.PrecisionPrice, GhostEnabled: s.GhostEnabled, GhostPrice: s.GhostPrice, MaxBlocks: s.MaxBlocks, DelaySeconds: s.DelaySeconds, ShareFaction: s.ShareFaction}
}

// heading is the bearing (degrees clockwise from north) from one map position to the next, nil
// when the player has not moved enough to tell.
func heading(fromX, fromZ, toX, toZ float64) *float64 {
	dx, dz := toX-fromX, toZ-fromZ
	if math.Hypot(dx, dz) < 15 {
		return nil
	}
	deg := math.Mod(math.Atan2(dx, dz)*180/math.Pi+360, 360)
	deg = math.Round(deg)
	return &deg
}

// uavPlayers builds the UAV's player list: every connected player's position (as of the delay).
func (a *App) uavPlayers(ctx context.Context, scope repository.PlayerInstallationScope, s repository.UAVSettings, tier string, mates map[int64]bool, now time.Time) ([]uavPlayerDTO, error) {
	players, err := a.LiveMap.OnlinePlayers(ctx, scope.InstallationID, scope.GuildID, scope.ServerID)
	if err != nil {
		return nil, err
	}
	st, err := a.LiveMap.Status(ctx, scope.GuildID, scope.ServerID, now)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(players))
	for _, p := range players {
		ids = append(ids, p.PlayerID)
	}
	since, admFile := sessionWindow(st, now)
	cutoff := now.Add(-time.Duration(s.DelaySeconds) * time.Second)
	positions, err := a.LiveMap.LatestPositionsAt(ctx, scope.ServerID, ids, since, cutoff, admFile)
	if err != nil {
		return nil, err
	}
	ghosted, err := a.UAV.Ghosted(ctx, scope.ServerID, now)
	if err != nil {
		return nil, err
	}
	precise := tier == repository.UAVPrecision
	var intel map[int64]repository.UAVIntel
	var trails map[int64][]repository.LiveMapPosition
	if precise {
		if intel, err = a.UAV.PrecisionIntel(ctx, scope.GuildID, scope.ServerID, now); err != nil {
			return nil, err
		}
		if trails, err = a.LiveMap.Trails(ctx, scope.ServerID, ids, cutoff.Add(-30*time.Minute), admFile, 1); err != nil {
			return nil, err
		}
	}
	out := make([]uavPlayerDTO, 0, len(players))
	for i, p := range players {
		pos, ok := positions[p.PlayerID]
		// A ghost is hidden from everyone else's UAV.
		if !ok || (ghosted[p.PlayerID] && p.PlayerID != scope.PlayerID) {
			continue
		}
		dto := uavPlayerDTO{Key: fmt.Sprintf("u%d", i+1), X: pos.X, Z: pos.Z, AgeSeconds: int64(now.Sub(pos.ObservedAt).Seconds()),
			Self: p.PlayerID == scope.PlayerID, Mate: mates[p.PlayerID]}
		if precise {
			dto.Key = fmt.Sprintf("p%d", p.PlayerID)
			dto.Name = p.Gamertag
			if p.FactionTag != nil {
				dto.FactionTag = *p.FactionTag
			}
			if in, ok := intel[p.PlayerID]; ok {
				dto.AliveSeconds = in.AliveSeconds
				kills := in.LifeKills
				dto.LifeKills = &kills
				if strings.TrimSpace(in.LastWeapon) != "" && in.LastWeaponAt != nil {
					dto.LastWeapon = in.LastWeapon
					age := int64(now.Sub(*in.LastWeaponAt).Seconds())
					dto.WeaponAgeSec = &age
				}
			}
			if t := trails[p.PlayerID]; len(t) > 0 {
				dto.Heading = heading(t[len(t)-1].X, t[len(t)-1].Z, pos.X, pos.Z)
			}
		}
		out = append(out, dto)
	}
	return out, nil
}

// uavContext resolves the player's faction and the UAV covering them.
func (a *App) uavContext(ctx context.Context, r *http.Request, scope repository.PlayerInstallationScope, s repository.UAVSettings, now time.Time) (int64, *repository.UAVPass, error) {
	var factionID int64
	if f, err := a.LiveMap.CurrentFaction(ctx, scope.InstallationID, strings.TrimSpace(r.Header.Get(actingUserHeader))); err != nil {
		return 0, nil, err
	} else if f != nil {
		factionID = f.ID
	}
	if !s.Enabled {
		return factionID, nil, nil
	}
	pass, err := a.UAV.ActivePass(ctx, scope.ServerID, scope.PlayerID, factionID, s.ShareFaction, now)
	return factionID, pass, err
}

// factionMates is the set of the player's verified faction members (drawn by the faction layer).
func (a *App) factionMates(ctx context.Context, scope repository.PlayerInstallationScope, factionID int64) map[int64]bool {
	out := map[int64]bool{}
	if factionID == 0 {
		return out
	}
	if rows, err := a.LiveMap.FactionMembers(ctx, scope.InstallationID, factionID, scope.GuildID, scope.ServerID); err == nil {
		for _, m := range rows {
			out[m.PlayerID] = true
		}
	}
	return out
}

// handlePlayerUAV is GET .../player/servers/{installationID}/map/uav: the offer, the UAV covering
// the player and, while one runs, every connected player.
func (a *App) handlePlayerUAV(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if !enforceRateLimit(w, a.saasLiveMapLimiter, rateLimitKey(r)) {
		return
	}
	if a.UAV == nil || a.LiveMap == nil {
		writeSaaSError(w, codeInternalError, "the UAV is unavailable")
		return
	}
	now := time.Now().UTC()
	s, err := a.UAV.Settings(ctx, scope.ServerID)
	if err != nil {
		playerFailed(w, "uav", err)
		return
	}
	resp := uavViewDTO{Offer: toUAVOffer(s), Players: []uavPlayerDTO{}}
	factionID, pass, err := a.uavContext(ctx, r, scope, s, now)
	if err != nil {
		playerFailed(w, "uav", err)
		return
	}
	if s.Enabled {
		if g, err := a.UAV.ActiveGhost(ctx, scope.ServerID, scope.PlayerID, now); err != nil {
			playerFailed(w, "uav", err)
			return
		} else if g != nil {
			resp.Ghost = &uavPassDTO{Tier: g.Tier, EndsAt: rfc3339(g.EndsAt), BuyerName: g.BuyerName, Mine: true}
		}
	}
	if pass != nil {
		resp.Pass = &uavPassDTO{Tier: pass.Tier, EndsAt: rfc3339(pass.EndsAt), BuyerName: pass.BuyerName, Mine: pass.PlayerID == scope.PlayerID}
		if resp.Players, err = a.uavPlayers(ctx, scope, s, pass.Tier, a.factionMates(ctx, scope, factionID), now); err != nil {
			playerFailed(w, "uav", err)
			return
		}
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

type buyUAVBody struct {
	Tier           string `json:"tier"`
	Blocks         int    `json:"blocks"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// handleBuyUAV is POST .../player/servers/{installationID}/map/uav: buy blocks of UAV time.
func (a *App) handleBuyUAV(w http.ResponseWriter, r *http.Request) {
	if a.saasShopPurchaseLimiter != nil && !enforceRateLimit(w, a.saasShopPurchaseLimiter, rateLimitKey(r)) {
		return
	}
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.UAV == nil || a.LiveMap == nil {
		writeSaaSError(w, codeInternalError, "the UAV is unavailable")
		return
	}
	body, ok := decodeJSONBody[buyUAVBody](w, r)
	if !ok {
		return
	}
	if !perkKeyRe.MatchString(body.IdempotencyKey) {
		writeSaaSError(w, codeInvalidRequest, "idempotencyKey must be 8-64 characters of A-Z a-z 0-9 . _ : -")
		return
	}
	now := time.Now().UTC()
	var factionID int64
	if f, err := a.LiveMap.CurrentFaction(ctx, scope.InstallationID, strings.TrimSpace(r.Header.Get(actingUserHeader))); err != nil {
		playerFailed(w, "uav", err)
		return
	} else if f != nil {
		factionID = f.ID
	}
	pass, balance, err := a.UAV.Buy(ctx, repository.PerkScope{GuildID: scope.GuildID, InstallationID: scope.InstallationID, ServerID: scope.ServerID},
		scope.PlayerID, factionID, strings.ToUpper(strings.TrimSpace(body.Tier)), body.Blocks, body.IdempotencyKey, now)
	switch {
	case errors.Is(err, repository.ErrInsufficientFunds):
		writeSaaSError(w, codeInsufficientFunds, "you don't have enough Champion Points")
		return
	case errors.Is(err, repository.ErrUAVClosed):
		writeSaaSError(w, codeConflict, err.Error())
		return
	case errors.Is(err, repository.ErrUAVInvalidRequest):
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	case err != nil:
		playerFailed(w, "uav purchase", err)
		return
	}
	slog.Info("component=live_map", "event", "uav_bought", "server_id", scope.ServerID, "player_id", scope.PlayerID, "tier", pass.Tier, "minutes", pass.Minutes)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"pass": uavPassDTO{Tier: pass.Tier, EndsAt: rfc3339(pass.EndsAt), BuyerName: pass.BuyerName, Mine: true},
		"startsAt": rfc3339(pass.StartsAt), "balance": balance})
}
