package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Phase 4 (docs/ZONES_UAV_RADAR.md): installation-scoped geographic zones plus the
// UAV/Base Radar intrusion engine's admin API. Every handler follows the exact requireCapability /
// ac.scope / recordAudit pattern every other Client Admin route already uses (saas_api_permissions.go,
// saas_api_player_intelligence.go) - no second authorization or scoping mechanism.

func (a *App) registerZoneRoutes(base string) {
	h := a.HTTPServer.Handle
	h("GET "+base+"/zones", a.handleListZones)
	h("POST "+base+"/zones", a.handleCreateZone)
	h("GET "+base+"/zones/{zoneID}", a.handleGetZone)
	h("PUT "+base+"/zones/{zoneID}", a.handleUpdateZone)
	h("DELETE "+base+"/zones/{zoneID}", a.handleDeleteZone)

	h("GET "+base+"/zones/{zoneID}/ignore", a.handleListIgnoreEntries)
	h("POST "+base+"/zones/{zoneID}/ignore", a.handleAddIgnoreEntry)
	h("DELETE "+base+"/zones/ignore/{entryID}", a.handleDeleteIgnoreEntry)

	h("GET "+base+"/zones/{zoneID}/authorized", a.handleListAuthorizedEntries)
	h("POST "+base+"/zones/{zoneID}/authorized", a.handleAddAuthorizedEntry)
	h("DELETE "+base+"/zones/authorized/{entryID}", a.handleDeleteAuthorizedEntry)

	h("GET "+base+"/zones/{zoneID}/bans", a.handleListZoneBans)
	h("POST "+base+"/zones/{zoneID}/bans", a.handleAddZoneBan)
	h("DELETE "+base+"/zones/bans/{banID}", a.handleLiftZoneBan)

	h("GET "+base+"/zones/{zoneID}/active-intruders", a.handleActiveIntruders)
	h("GET "+base+"/intrusions/active", a.handleActiveIntrusions)
	h("GET "+base+"/intrusions/history", a.handleIntrusionHistory)
	h("POST "+base+"/intrusions/{intrusionID}/acknowledge", a.handleAcknowledgeIntrusion)
}

// requireZoneManageFor gates zone create/update/delete: ZONE_MANAGE is always required, and
// UAV_MANAGE (Owner) is additionally required whenever zoneType is UAV or BASE_RADAR (task
// section 3) - an Administrator can manage ordinary zones but never a UAV/Base Radar one without
// also holding Owner.
func (a *App) requireZoneManageFor(w http.ResponseWriter, r *http.Request, zoneType string) (adminActor, bool) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return adminActor{}, false
	}
	if repository.IsUAVOrRadar(zoneType) && !permissions.Allows(ac.level, permissions.CapUAVManage) {
		writeSaaSError(w, codeAdminForbidden, "UAV/Base Radar zones require Owner-level access ("+string(permissions.CapUAVManage)+")")
		return adminActor{}, false
	}
	return ac, true
}

// --- DTOs -----------------------------------------------------------------------------------

type zoneDTO struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	ZoneType        string  `json:"zoneType"`
	CenterX         float64 `json:"centerX"`
	CenterZ         float64 `json:"centerZ"`
	Radius          float64 `json:"radius"`
	AlertChannelID  *string `json:"alertChannelId,omitempty"`
	CooldownSeconds int     `json:"cooldownSeconds"`
	Enabled         bool    `json:"enabled"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

func toZoneDTO(z repository.Zone) zoneDTO {
	return zoneDTO{
		ID: z.ID, Name: z.Name, ZoneType: z.ZoneType, CenterX: z.CenterX, CenterZ: z.CenterZ, Radius: z.Radius,
		AlertChannelID: z.AlertChannelID, CooldownSeconds: z.CooldownSeconds, Enabled: z.Enabled,
		CreatedAt: z.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: z.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// --- zone CRUD (task sections 1-3) -----------------------------------------------------------

func (a *App) handleListZones(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	if a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Zones.ListZones(ctx, ac.scope.InstallationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list zones failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list zones")
		return
	}
	out := make([]zoneDTO, 0, len(rows))
	for _, z := range rows {
		out = append(out, toZoneDTO(z))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

type createZoneRequest struct {
	Name            string  `json:"name"`
	ZoneType        string  `json:"zoneType"`
	CenterX         float64 `json:"centerX"`
	CenterZ         float64 `json:"centerZ"`
	Radius          float64 `json:"radius"`
	AlertChannelID  *string `json:"alertChannelId"`
	CooldownSeconds *int    `json:"cooldownSeconds"`
}

const defaultZoneCooldownSeconds = 300

// handleCreateZone is POST .../admin/zones (ZONE_MANAGE, + UAV_MANAGE for UAV/BASE_RADAR).
func (a *App) handleCreateZone(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.resolveAdminActor(w, r)
	if !ok {
		return
	}
	var req createZoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.ZoneType = strings.ToUpper(strings.TrimSpace(req.ZoneType))
	if req.Name == "" {
		writeSaaSError(w, codeInvalidRequest, "name is required")
		return
	}
	if !repository.ValidZoneType(req.ZoneType) {
		writeSaaSError(w, codeInvalidRequest, "zoneType must be one of SAFEZONE, PVP, RESTRICTED, EVENT, UAV, BASE_RADAR, CUSTOM")
		return
	}
	if req.Radius <= 0 {
		writeSaaSError(w, codeInvalidRequest, "radius must be greater than 0")
		return
	}
	cooldown := defaultZoneCooldownSeconds
	if req.CooldownSeconds != nil {
		if *req.CooldownSeconds < 0 {
			writeSaaSError(w, codeInvalidRequest, "cooldownSeconds must not be negative")
			return
		}
		cooldown = *req.CooldownSeconds
	}
	if !permissions.Allows(ac.level, permissions.CapZoneManage) || (repository.IsUAVOrRadar(req.ZoneType) && !permissions.Allows(ac.level, permissions.CapUAVManage)) {
		writeSaaSError(w, codeAdminForbidden, "missing required permission")
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
		return
	}
	if a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	zone, err := a.Zones.CreateZone(ctx, ac.scope.InstallationID, ac.scope.GuildID, *ac.scope.ServerID, req.Name, req.ZoneType, req.CenterX, req.CenterZ, req.Radius, req.AlertChannelID, cooldown, ac.user.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "create zone failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not create zone")
		return
	}
	a.invalidateZoneCache(*ac.scope.ServerID)
	a.recordAudit(ctx, ac, "ZONE_CREATE", zoneTarget(zone.ID), "", "success", nil, toZoneDTO(*zone))
	writeSaaSJSON(w, http.StatusCreated, toZoneDTO(*zone))
}

func zoneTarget(id int64) string { return "zone:" + strconv.FormatInt(id, 10) }

func (a *App) handleGetZone(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	if a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	zone, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toZoneDTO(*zone))
}

// updateZoneRequest's alertChannelId is deliberately NOT a field here - it needs three states
// (omitted / set to a value / explicitly cleared to null), which handleUpdateZone reads straight
// out of the raw JSON map instead of through this struct's typed Decode.
type updateZoneRequest struct {
	Name            *string  `json:"name"`
	CenterX         *float64 `json:"centerX"`
	CenterZ         *float64 `json:"centerZ"`
	Radius          *float64 `json:"radius"`
	CooldownSeconds *int     `json:"cooldownSeconds"`
	Enabled         *bool    `json:"enabled"`
}

// handleUpdateZone is PUT .../admin/zones/{zoneID}. zone_type is immutable after creation (never
// part of this request) - a zone whose type is already UAV/BASE_RADAR keeps requiring UAV_MANAGE
// for every future edit, including this one.
func (a *App) handleUpdateZone(w http.ResponseWriter, r *http.Request) {
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	ac, ok := a.resolveAdminActor(w, r)
	if !ok {
		return
	}
	if a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	existing, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	if !permissions.Allows(ac.level, permissions.CapZoneManage) || (repository.IsUAVOrRadar(existing.ZoneType) && !permissions.Allows(ac.level, permissions.CapUAVManage)) {
		writeSaaSError(w, codeAdminForbidden, "missing required permission")
		return
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	var req updateZoneRequest
	body, _ := json.Marshal(raw)
	_ = json.Unmarshal(body, &req)
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeSaaSError(w, codeInvalidRequest, "name must not be empty")
		return
	}
	if req.Radius != nil && *req.Radius <= 0 {
		writeSaaSError(w, codeInvalidRequest, "radius must be greater than 0")
		return
	}
	if req.CooldownSeconds != nil && *req.CooldownSeconds < 0 {
		writeSaaSError(w, codeInvalidRequest, "cooldownSeconds must not be negative")
		return
	}
	u := repository.ZoneUpdate{Name: req.Name, CenterX: req.CenterX, CenterZ: req.CenterZ, Radius: req.Radius, CooldownSeconds: req.CooldownSeconds, Enabled: req.Enabled}
	if raw, present := raw["alertChannelId"]; present {
		var v *string
		_ = json.Unmarshal(raw, &v)
		u.AlertChannelID = &v
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	updated, err := a.Zones.UpdateZone(ctx, ac.scope.InstallationID, zoneID, u)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "update zone failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not update zone")
		return
	}
	a.invalidateZoneCache(existing.ServerID)
	a.recordAudit(ctx, ac, "ZONE_UPDATE", zoneTarget(zoneID), "", "success", toZoneDTO(*existing), toZoneDTO(*updated))
	writeSaaSJSON(w, http.StatusOK, toZoneDTO(*updated))
}

func (a *App) handleDeleteZone(w http.ResponseWriter, r *http.Request) {
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	ac, ok := a.resolveAdminActor(w, r)
	if !ok {
		return
	}
	if a.Zones == nil {
		writeSaaSError(w, codeInternalError, "zones unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	existing, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	if !permissions.Allows(ac.level, permissions.CapZoneManage) || (repository.IsUAVOrRadar(existing.ZoneType) && !permissions.Allows(ac.level, permissions.CapUAVManage)) {
		writeSaaSError(w, codeAdminForbidden, "missing required permission")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	deleted, err := a.Zones.DeleteZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "delete zone failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not delete zone")
		return
	}
	if !deleted {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	a.invalidateZoneCache(existing.ServerID)
	a.recordAudit(ctx, ac, "ZONE_DELETE", zoneTarget(zoneID), "", "success", toZoneDTO(*existing), nil)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// invalidateZoneCache is called by every zone/ignore/authorized/ban mutation - the intrusion
// engine never queries zones from the database per location event (task section 26), so a stale
// in-memory copy must be dropped explicitly the moment any of this zone's configuration changes.
func (a *App) invalidateZoneCache(serverID int64) {
	if a.ZoneCache != nil {
		a.ZoneCache.Invalidate(serverID)
	}
}

// --- ignore entries (task section 4) ----------------------------------------------------------

type zoneEntryDTO struct {
	ID         int64  `json:"id"`
	ZoneID     int64  `json:"zoneId"`
	EntryType  string `json:"entryType"`
	EntryValue string `json:"entryValue"`
	CreatedAt  string `json:"createdAt"`
}

func (a *App) handleListIgnoreEntries(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	rows, err := a.Zones.ListIgnoreEntries(ctx, zoneID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not list ignore entries")
		return
	}
	out := make([]zoneEntryDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, zoneEntryDTO{ID: e.ID, ZoneID: e.ZoneID, EntryType: e.EntryType, EntryValue: e.EntryValue, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

type addEntryRequest struct {
	EntryType  string `json:"entryType"`
	EntryValue string `json:"entryValue"`
}

func (a *App) handleAddIgnoreEntry(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneIgnoreManage)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	var req addEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.EntryType = strings.ToUpper(strings.TrimSpace(req.EntryType))
	req.EntryValue = strings.TrimSpace(req.EntryValue)
	if req.EntryValue == "" || (req.EntryType != repository.ZoneEntryPlayer && req.EntryType != repository.ZoneEntryFaction && req.EntryType != repository.ZoneEntryDiscordRole) {
		writeSaaSError(w, codeInvalidRequest, "entryType must be PLAYER, FACTION, or DISCORD_ROLE, and entryValue is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	zone, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	entry, err := a.Zones.AddIgnoreEntry(ctx, zoneID, req.EntryType, req.EntryValue, ac.user.ID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not add ignore entry")
		return
	}
	a.invalidateZoneCache(zone.ServerID)
	a.recordAudit(ctx, ac, "ZONE_IGNORE_ADD", zoneTarget(zoneID), "", "success", nil, map[string]string{"entryType": req.EntryType, "entryValue": req.EntryValue})
	writeSaaSJSON(w, http.StatusCreated, zoneEntryDTO{ID: entry.ID, ZoneID: entry.ZoneID, EntryType: entry.EntryType, EntryValue: entry.EntryValue, CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339)})
}

func (a *App) handleDeleteIgnoreEntry(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneIgnoreManage)
	if !ok {
		return
	}
	entryID, good := pathInt64(w, r, "entryID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	deleted, err := a.Zones.DeleteIgnoreEntry(ctx, ac.scope.InstallationID, entryID)
	if err != nil || !deleted {
		writeSaaSError(w, codeNotFound, "ignore entry not found")
		return
	}
	a.recordAudit(ctx, ac, "ZONE_IGNORE_REMOVE", "entry:"+strconv.FormatInt(entryID, 10), "", "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// --- authorized entries (task section 5) ------------------------------------------------------

func (a *App) handleListAuthorizedEntries(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	rows, err := a.Zones.ListAuthorizedEntries(ctx, zoneID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not list authorized entries")
		return
	}
	out := make([]zoneEntryDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, zoneEntryDTO{ID: e.ID, ZoneID: e.ZoneID, EntryType: e.EntryType, EntryValue: e.EntryValue, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339)})
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (a *App) handleAddAuthorizedEntry(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	var req addEntryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.EntryType = strings.ToUpper(strings.TrimSpace(req.EntryType))
	req.EntryValue = strings.TrimSpace(req.EntryValue)
	if req.EntryValue == "" || (req.EntryType != repository.ZoneEntryPlayer && req.EntryType != repository.ZoneEntryFaction) {
		writeSaaSError(w, codeInvalidRequest, "entryType must be PLAYER or FACTION, and entryValue is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	zone, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID)
	if err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	entry, err := a.Zones.AddAuthorizedEntry(ctx, zoneID, req.EntryType, req.EntryValue, ac.user.ID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not add authorized entry")
		return
	}
	a.invalidateZoneCache(zone.ServerID)
	a.recordAudit(ctx, ac, "ZONE_AUTHORIZED_ADD", zoneTarget(zoneID), "", "success", nil, map[string]string{"entryType": req.EntryType, "entryValue": req.EntryValue})
	writeSaaSJSON(w, http.StatusCreated, zoneEntryDTO{ID: entry.ID, ZoneID: entry.ZoneID, EntryType: entry.EntryType, EntryValue: entry.EntryValue, CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339)})
}

func (a *App) handleDeleteAuthorizedEntry(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return
	}
	entryID, good := pathInt64(w, r, "entryID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	deleted, err := a.Zones.DeleteAuthorizedEntry(ctx, ac.scope.InstallationID, entryID)
	if err != nil || !deleted {
		writeSaaSError(w, codeNotFound, "authorized entry not found")
		return
	}
	a.recordAudit(ctx, ac, "ZONE_AUTHORIZED_REMOVE", "entry:"+strconv.FormatInt(entryID, 10), "", "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// --- zone bans (task section 6 - explicitly not server bans) ----------------------------------

type zoneBanDTO struct {
	ID        int64   `json:"id"`
	ZoneID    int64   `json:"zoneId"`
	PlayerID  int64   `json:"playerId"`
	Reason    string  `json:"reason,omitempty"`
	Active    bool    `json:"active"`
	CreatedAt string  `json:"createdAt"`
	LiftedAt  *string `json:"liftedAt,omitempty"`
}

func toZoneBanDTO(b repository.ZoneBan) zoneBanDTO {
	return zoneBanDTO{ID: b.ID, ZoneID: b.ZoneID, PlayerID: b.PlayerID, Reason: b.Reason, Active: b.Active,
		CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339), LiftedAt: nullableTimeStr(b.LiftedAt)}
}

func (a *App) handleListZoneBans(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	rows, err := a.Zones.ListZoneBans(ctx, zoneID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not list zone bans")
		return
	}
	out := make([]zoneBanDTO, 0, len(rows))
	for _, b := range rows {
		out = append(out, toZoneBanDTO(b))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

type addZoneBanRequest struct {
	PlayerID int64  `json:"playerId"`
	Reason   string `json:"reason"`
}

// handleAddZoneBan is POST .../admin/zones/{zoneID}/bans (ZONE_MANAGE). A zone ban is never
// forwarded to Nitrado's banlist and never touches installation_access_entries - it exists purely
// so the intrusion engine can flag a future entry as a ZONE_BAN_VIOLATION (task section 6).
func (a *App) handleAddZoneBan(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	var req addZoneBanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PlayerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "playerId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ban, err := a.Zones.AddZoneBan(ctx, zoneID, req.PlayerID, req.Reason, ac.user.ID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not add zone ban")
		return
	}
	a.recordAudit(ctx, ac, "ZONE_BAN_ADD", zoneTarget(zoneID), req.Reason, "success", nil, toZoneBanDTO(*ban))
	writeSaaSJSON(w, http.StatusCreated, toZoneBanDTO(*ban))
}

func (a *App) handleLiftZoneBan(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneManage)
	if !ok {
		return
	}
	banID, good := pathInt64(w, r, "banID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	lifted, err := a.Zones.LiftZoneBan(ctx, ac.scope.InstallationID, banID, ac.user.ID)
	if err != nil || !lifted {
		writeSaaSError(w, codeNotFound, "active zone ban not found")
		return
	}
	a.recordAudit(ctx, ac, "ZONE_BAN_LIFT", "ban:"+strconv.FormatInt(banID, 10), "", "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"lifted": true})
}

// --- presence freshness (task section 19) ------------------------------------------------------

// presenceStatusFor classifies an intruder's staleness at read time - never stored (matching the
// "no second truth" principle Phase 3 established for location freshness). age is how long ago the
// player's presence was last confirmed by an actual location event.
func presenceStatusFor(age time.Duration) string {
	if repository.ClassifyFreshness(age) == repository.LocationFreshnessStale {
		return "PRESENCE_UNCERTAIN"
	}
	return "PRESENT_RECENT"
}

type activeIntruderDTO struct {
	IntrusionID    int64  `json:"intrusionId"`
	PlayerID       int64  `json:"playerId"`
	Gamertag       string `json:"gamertag"`
	Status         string `json:"status"`
	Banned         bool   `json:"banned"`
	EnteredAt      string `json:"enteredAt"`
	LastSeenAt     string `json:"lastSeenAt"`
	AgeSeconds     int64  `json:"ageSeconds"`
	PresenceStatus string `json:"presenceStatus"`
}

// handleActiveIntruders is GET .../admin/zones/{zoneID}/active-intruders (ZONE_VIEW).
func (a *App) handleActiveIntruders(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	zoneID, good := pathInt64(w, r, "zoneID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if _, err := a.Zones.GetZone(ctx, ac.scope.InstallationID, zoneID); err != nil {
		writeSaaSError(w, codeNotFound, "zone not found")
		return
	}
	rows, err := a.Zones.ListActiveIntrudersForZone(ctx, zoneID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not list active intruders")
		return
	}
	out := make([]activeIntruderDTO, 0, len(rows))
	for _, ai := range rows {
		age := time.Since(ai.LastSeenAt)
		if age < 0 {
			age = 0
		}
		out = append(out, activeIntruderDTO{
			IntrusionID: ai.IntrusionID, PlayerID: ai.PlayerID, Gamertag: ai.Gamertag, Status: ai.Status, Banned: ai.Banned,
			EnteredAt: ai.EnteredAt.UTC().Format(time.RFC3339), LastSeenAt: ai.LastSeenAt.UTC().Format(time.RFC3339),
			AgeSeconds: int64(age.Seconds()), PresenceStatus: presenceStatusFor(age),
		})
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

// --- installation-wide active intrusions / history / acknowledge (task sections 13-16) --------

type zoneIntrusionDTO struct {
	ID             int64   `json:"id"`
	ZoneID         int64   `json:"zoneId"`
	ZoneName       string  `json:"zoneName,omitempty"`
	ZoneType       string  `json:"zoneType,omitempty"`
	PlayerID       int64   `json:"playerId"`
	Gamertag       string  `json:"gamertag"`
	Status         string  `json:"status"`
	Banned         bool    `json:"banned"`
	EnteredAt      string  `json:"enteredAt"`
	ExitedAt       *string `json:"exitedAt,omitempty"`
	AcknowledgedAt *string `json:"acknowledgedAt,omitempty"`
	AlertCount     int     `json:"alertCount"`
	PresenceStatus string  `json:"presenceStatus,omitempty"`
}

func toZoneIntrusionDTO(it repository.ZoneIntrusion, withPresence bool) zoneIntrusionDTO {
	dto := zoneIntrusionDTO{
		ID: it.ID, ZoneID: it.ZoneID, ZoneName: it.ZoneName, ZoneType: it.ZoneType, PlayerID: it.PlayerID, Gamertag: it.Gamertag,
		Status: it.Status, Banned: it.Banned, EnteredAt: it.EnteredAt.UTC().Format(time.RFC3339),
		ExitedAt: nullableTimeStr(it.ExitedAt), AcknowledgedAt: nullableTimeStr(it.AcknowledgedAt), AlertCount: it.AlertCount,
	}
	if withPresence {
		age := time.Since(it.PresenceLastSeenAt)
		if age < 0 {
			age = 0
		}
		dto.PresenceStatus = presenceStatusFor(age)
	}
	return dto
}

// handleActiveIntrusions is GET .../admin/intrusions/active (ZONE_VIEW), filterable by
// zoneId/zoneType/playerId/acknowledged (task section 13).
func (a *App) handleActiveIntrusions(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	q := r.URL.Query()
	f := repository.ActiveIntrusionFilter{ZoneType: strings.ToUpper(strings.TrimSpace(q.Get("zoneType")))}
	if v := strings.TrimSpace(q.Get("zoneId")); v != "" {
		f.ZoneID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := strings.TrimSpace(q.Get("playerId")); v != "" {
		f.PlayerID, _ = strconv.ParseInt(v, 10, 64)
	}
	if ack := parseOptionalBool(q.Get("acknowledged")); ack != nil {
		f.Acknowledged = ack
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Zones.ListActiveForInstallation(ctx, ac.scope.InstallationID, f)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list active intrusions failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list active intrusions")
		return
	}
	out := make([]zoneIntrusionDTO, 0, len(rows))
	for _, it := range rows {
		out = append(out, toZoneIntrusionDTO(it, true))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleIntrusionHistory is GET .../admin/intrusions/history (ZONE_VIEW), filterable by
// zoneId/playerId/from/to/status/cursor/limit, newest-first (task section 14).
func (a *App) handleIntrusionHistory(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapZoneView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	q := r.URL.Query()
	f := repository.IntrusionHistoryFilter{Status: strings.ToUpper(strings.TrimSpace(q.Get("status")))}
	if v := strings.TrimSpace(q.Get("zoneId")); v != "" {
		f.ZoneID, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := strings.TrimSpace(q.Get("playerId")); v != "" {
		f.PlayerID, _ = strconv.ParseInt(v, 10, 64)
	}
	if from := strings.TrimSpace(q.Get("from")); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			f.From = &t
		}
	}
	if to := strings.TrimSpace(q.Get("to")); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			f.To = &t
		}
	}
	if cursor := strings.TrimSpace(q.Get("cursor")); cursor != "" {
		f.Before, _ = strconv.ParseInt(cursor, 10, 64)
	}
	if lim := strings.TrimSpace(q.Get("limit")); lim != "" {
		f.Limit, _ = strconv.Atoi(lim)
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Zones.ListHistory(ctx, ac.scope.InstallationID, f)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list intrusion history failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list intrusion history")
		return
	}
	out := make([]zoneIntrusionDTO, 0, len(rows))
	var nextCursor *string
	for _, it := range rows {
		out = append(out, toZoneIntrusionDTO(it, false))
	}
	if len(rows) > 0 && f.Limit > 0 && len(rows) >= f.Limit {
		c := strconv.FormatInt(rows[len(rows)-1].ID, 10)
		nextCursor = &c
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": out, "nextCursor": nextCursor})
}

// handleAcknowledgeIntrusion is POST .../admin/intrusions/{intrusionID}/acknowledge (INTRUSION_ACK).
func (a *App) handleAcknowledgeIntrusion(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapIntrusionAck)
	if !ok {
		return
	}
	intrusionID, good := pathInt64(w, r, "intrusionID")
	if !good {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	it, err := a.Zones.AcknowledgeIntrusion(ctx, ac.scope.InstallationID, intrusionID, ac.user.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "acknowledge intrusion failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not acknowledge intrusion")
		return
	}
	if it == nil {
		writeSaaSError(w, codeNotFound, "no ACTIVE intrusion found with that id")
		return
	}
	a.recordAudit(ctx, ac, "INTRUSION_ACKNOWLEDGE", zoneTarget(it.ZoneID), "", "success", nil, toZoneIntrusionDTO(*it, false))
	writeSaaSJSON(w, http.StatusOK, toZoneIntrusionDTO(*it, false))
}
