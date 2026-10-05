package app

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/livemap"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Live map (docs/LIVE_MAP.md): three views of one server's map, each showing strictly what its
// audience may see.
//
//   - Public: recent kills and a pressure grid - nothing the public kill feed does not already
//     show - behind the installation's delay. No player position ever leaves this route.
//   - Faction layer: a verified player sees the latest positions of their own hub faction's
//     members (and nobody else's), their bases and the raids on them.
//   - Staff: every connected player's latest position and a track history, audited like every
//     other location read.

const (
	liveMapCacheTTL       = 5 * time.Second
	liveMapNitradoTTL     = 5 * time.Minute
	liveMapNitradoTimeout = 4 * time.Second
	liveMapAuditEvery     = 10 * time.Minute
	liveMapKillLimit      = 100
	liveMapPressureWindow = 30 * time.Minute
	liveMapPressureRes    = 500.0
	liveMapPressureCells  = 400
	liveMapPressurePoints = 20000
	liveMapClockMaxAge    = 30 * time.Minute
	liveMapTrailLength    = 6
	liveMapTrailWindow    = 2 * time.Hour
	liveMapAlertWindow    = time.Hour
	liveMapAlertLimit     = 20
	liveMapHistoryMax     = 6 * time.Hour
	liveMapHistoryKills   = 500
	liveMapHistoryPoints  = 20000
	liveMapHistoryIntr    = 500
	liveMapLookback       = 24 * time.Hour // positions without a recorded boot session
)

func (a *App) registerLiveMapRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET /api/saas/player/servers/{installationID}/map/faction", a.handlePlayerFactionMap)
	h("GET "+adminBase+"/map/live", a.handleAdminLiveMap)
	h("GET "+adminBase+"/map/history", a.handleAdminMapHistory)
	a.registerUAVRoutes(adminBase)
}

// --- DTOs ----------------------------------------------------------------------------------------

type liveMapMapDTO struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Size    int    `json:"size"`
	Guessed bool   `json:"guessed"`
}

type liveMapInGameDTO struct {
	Time      string `json:"time"`
	Estimated bool   `json:"estimated"`
}

type liveMapClockDTO struct {
	ServerLocalTime  *string           `json:"serverLocalTime"`
	UTCOffsetMinutes *int              `json:"utcOffsetMinutes"`
	BootedAt         *string           `json:"bootedAt"`
	NextRestartAt    *string           `json:"nextRestartAt"`
	InGame           *liveMapInGameDTO `json:"inGame"`
}

type liveMapCellDTO struct {
	CenterX   float64 `json:"centerX"`
	CenterZ   float64 `json:"centerZ"`
	Count     int     `json:"count"`
	Intensity float64 `json:"intensity"`
}

type liveMapPressureDTO struct {
	Resolution    int              `json:"resolution"`
	WindowMinutes int              `json:"windowMinutes"`
	Cells         []liveMapCellDTO `json:"cells"`
}

type liveMapKillDTO struct {
	ID             int64    `json:"id"`
	At             string   `json:"at"`
	KillerName     string   `json:"killerName"`
	VictimName     string   `json:"victimName"`
	Weapon         string   `json:"weapon"`
	DistanceMeters *float64 `json:"distanceMeters"`
	Headshot       bool     `json:"headshot"`
	Longshot       bool     `json:"longshot"`
	KillerX        *float64 `json:"killerX"`
	KillerZ        *float64 `json:"killerZ"`
	VictimX        *float64 `json:"victimX"`
	VictimZ        *float64 `json:"victimZ"`
}

type liveMapPublicDTO struct {
	InstallationID int64              `json:"installationId"`
	Name           string             `json:"name"`
	Platform       string             `json:"platform"`
	Map            liveMapMapDTO      `json:"map"`
	Public         bool               `json:"public"`
	// Listed is whether the server is listed on the network (the website lets search engines
	// index only listed servers' maps).
	Listed       bool `json:"listed"`
	DelaySeconds int  `json:"delaySeconds"`
	GeneratedAt    string             `json:"generatedAt"`
	PlayersOnline  int                `json:"playersOnline"`
	LastActivityAt *string            `json:"lastActivityAt"`
	Clock          liveMapClockDTO    `json:"clock"`
	Pressure       liveMapPressureDTO `json:"pressure"`
	HotZone        *hotZoneDTO        `json:"hotZone"`
	Kills          []liveMapKillDTO   `json:"kills"`
	LastKillID     int64              `json:"lastKillId"`
	// Territory is the zones and who holds them, when the server runs territory control
	// (docs/PROGRESSION.md); absent otherwise.
	Territory []territoryZoneDTO `json:"territory,omitempty"`
}

type liveMapPositionDTO struct {
	X          float64 `json:"x"`
	Z          float64 `json:"z"`
	ObservedAt string  `json:"observedAt"`
	AgeSeconds int64   `json:"ageSeconds"`
	EventType  string  `json:"eventType"`
}

type liveMapTrailPointDTO struct {
	X          float64 `json:"x"`
	Z          float64 `json:"z"`
	ObservedAt string  `json:"observedAt"`
}

type liveMapFactionDTO struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Tag            string  `json:"tag"`
	PrimaryColor   *string `json:"primaryColor"`
	SecondaryColor *string `json:"secondaryColor"`
}

type liveMapSelfDTO struct {
	PlayerID int64  `json:"playerId"`
	Gamertag string `json:"gamertag"`
}

type liveMapMemberDTO struct {
	PlayerID  int64                  `json:"playerId"`
	Gamertag  string                 `json:"gamertag"`
	IsSelf    bool                   `json:"isSelf"`
	Online    bool                   `json:"online"`
	LastKnown *liveMapPositionDTO    `json:"lastKnown"`
	Trail     []liveMapTrailPointDTO `json:"trail"`
}

type liveMapBaseDTO struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	CenterX       float64 `json:"centerX"`
	CenterZ       float64 `json:"centerZ"`
	Radius        float64 `json:"radius"`
	OwnerPlayerID int64   `json:"ownerPlayerId"`
}

type liveMapAlertDTO struct {
	BaseID     int64  `json:"baseId"`
	BaseName   string `json:"baseName"`
	Kind       string `json:"kind"`
	Part       string `json:"part"`
	Target     string `json:"target"`
	RaiderName string `json:"raiderName"`
	At         string `json:"at"`
}

type liveMapFactionLayerDTO struct {
	Enabled bool               `json:"enabled"`
	Faction *liveMapFactionDTO `json:"faction"`
	Self    liveMapSelfDTO     `json:"self"`
	Members []liveMapMemberDTO `json:"members"`
	Bases   []liveMapBaseDTO   `json:"bases"`
	Alerts  []liveMapAlertDTO  `json:"alerts"`
}

type liveMapStaffPlayerDTO struct {
	PlayerID   int64               `json:"playerId"`
	Gamertag   string              `json:"gamertag"`
	Online     bool                `json:"online"`
	FactionTag *string             `json:"factionTag"`
	LastKnown  *liveMapPositionDTO `json:"lastKnown"`
}

type liveMapIntrusionDTO struct {
	zoneIntrusionDTO
	CenterX float64 `json:"centerX"`
	CenterZ float64 `json:"centerZ"`
}

type liveMapStaffLiveDTO struct {
	GeneratedAt string                  `json:"generatedAt"`
	Players     []liveMapStaffPlayerDTO `json:"players"`
	Zones       []zoneDTO               `json:"zones"`
	Intrusions  []liveMapIntrusionDTO   `json:"intrusions"`
}

type liveMapHistoryKillDTO struct {
	liveMapKillDTO
	KillerID int64 `json:"killerId"`
	VictimID int64 `json:"victimId"`
}

type liveMapHistoryPointDTO struct {
	T    string  `json:"t"`
	X    float64 `json:"x"`
	Z    float64 `json:"z"`
	Type string  `json:"type"`
}

type liveMapHistoryTrackDTO struct {
	PlayerID int64                    `json:"playerId"`
	Gamertag string                   `json:"gamertag"`
	Points   []liveMapHistoryPointDTO `json:"points"`
}

type liveMapHistoryDTO struct {
	From       string                   `json:"from"`
	To         string                   `json:"to"`
	Map        liveMapMapDTO            `json:"map"`
	Kills      []liveMapHistoryKillDTO  `json:"kills"`
	Tracks     []liveMapHistoryTrackDTO `json:"tracks"`
	Intrusions []liveMapIntrusionDTO    `json:"intrusions"`
	Truncated  bool                     `json:"truncated"`
}

// --- shared pieces -------------------------------------------------------------------------------

// liveMapMap resolves the installation's configured map. An unset or unsupported key falls back
// to Chernarus, marked guessed so the website can say so.
func liveMapMap(key string) liveMapMapDTO {
	if m, ok := dayzmap.Lookup(key); ok {
		return liveMapMapDTO{Key: m.Key, Name: m.DisplayName, Size: int(m.SizeMetres)}
	}
	m, _ := dayzmap.Lookup("chernarusplus")
	return liveMapMapDTO{Key: m.Key, Name: m.DisplayName, Size: int(m.SizeMetres), Guessed: true}
}

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func toLiveMapKill(k repository.FightKill) liveMapKillDTO {
	return liveMapKillDTO{ID: k.ID, At: rfc3339(k.At), KillerName: k.KillerName, VictimName: k.VictimName, Weapon: k.Weapon, DistanceMeters: k.Distance,
		Headshot: k.Headshot, Longshot: k.Longshot, KillerX: k.KillerX, KillerZ: k.KillerZ, VictimX: k.VictimX, VictimZ: k.VictimZ}
}

func toLiveMapPosition(p repository.LiveMapPosition, now time.Time) *liveMapPositionDTO {
	age := int64(now.Sub(p.ObservedAt).Seconds())
	if age < 0 {
		age = 0
	}
	return &liveMapPositionDTO{X: p.X, Z: p.Z, ObservedAt: rfc3339(p.ObservedAt), AgeSeconds: age, EventType: p.EventType}
}

// publicLiveMapKill is a kill as the public map shows it: where the victim died, never where the
// killer - who is still alive - was standing.
func publicLiveMapKill(k repository.FightKill) liveMapKillDTO {
	d := toLiveMapKill(k)
	d.KillerX, d.KillerZ = nil, nil
	return d
}

// filterLiveMapKills keeps the kills at or before cutoff (the public delay) with an id above
// sinceID, newest first, at most limit. Pure, so the delay rule is unit-tested on its own.
func filterLiveMapKills(kills []repository.FightKill, cutoff time.Time, sinceID int64, limit int) []liveMapKillDTO {
	out := []liveMapKillDTO{}
	sorted := append([]repository.FightKill(nil), kills...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].At.Equal(sorted[j].At) {
			return sorted[i].At.After(sorted[j].At)
		}
		return sorted[i].ID > sorted[j].ID
	})
	for _, k := range sorted {
		if k.At.After(cutoff) || k.ID <= sinceID {
			continue
		}
		out = append(out, publicLiveMapKill(k))
		if len(out) >= limit {
			break
		}
	}
	return out
}

// sessionWindow is where "current" positions come from: the recorded boot session's file since
// it was selected, else the last day.
func sessionWindow(st repository.LiveMapStatus, now time.Time) (since time.Time, admFile string) {
	if st.Session != nil {
		return st.Session.SelectedAt, st.Session.ADMFile
	}
	return now.Add(-liveMapLookback), ""
}

// --- caches --------------------------------------------------------------------------------------

type liveMapCacheEntry struct {
	value *liveMapPublicDTO // nil: not found / not public (cached too, so a 404 is as cheap as a hit)
	at    time.Time
}

// liveMapNitradoFacts is what the live map reads from Nitrado, cached per installation.
type liveMapNitradoFacts struct {
	facts    *nitrado.GameserverFacts
	tasks    []nitrado.ScheduledTask
	fetched  time.Time
	attempts int
}

func (a *App) invalidateLiveMapCache() {
	a.liveMapMu.Lock()
	a.liveMapPublic = nil
	a.liveMapMu.Unlock()
}

// liveMapNitrado returns the cached Nitrado facts and scheduled tasks of the installation,
// refreshing them at most once per liveMapNitradoTTL. Every failure is cached as "unknown" for
// the same period and never surfaces to the caller.
func (a *App) liveMapNitrado(ctx context.Context, inst repository.LiveMapInstallation) liveMapNitradoFacts {
	a.liveMapMu.Lock()
	if a.liveMapNitradoCache == nil {
		a.liveMapNitradoCache = map[int64]liveMapNitradoFacts{}
	}
	if e, ok := a.liveMapNitradoCache[inst.InstallationID]; ok && time.Since(e.fetched) < liveMapNitradoTTL {
		a.liveMapMu.Unlock()
		return e
	}
	a.liveMapMu.Unlock()
	entry := liveMapNitradoFacts{fetched: time.Now()}
	func() {
		if inst.ProviderServiceID == "" || a.SaaSCredentials == nil || a.CredentialCipher == nil {
			return
		}
		nctx, cancel := context.WithTimeout(ctx, liveMapNitradoTimeout)
		defer cancel()
		envelope, err := a.SaaSCredentials.GetForOrganizationOnly(nctx, inst.OrganizationID)
		if err != nil || envelope == nil {
			return
		}
		client, err := a.nitradoClientFromEnvelope(*envelope)
		if err != nil {
			return
		}
		if facts, err := client.GameserverFacts(nctx, inst.ProviderServiceID); err == nil {
			entry.facts = &facts
		} else {
			slog.Debug("component=live_map", "event", "gameserver_facts_failed", "installation_id", inst.InstallationID, "err", err.Error())
		}
		if tasks, err := client.ListScheduledTasks(nctx, inst.ProviderServiceID); err == nil {
			entry.tasks = tasks
		} else {
			slog.Debug("component=live_map", "event", "scheduled_tasks_failed", "installation_id", inst.InstallationID, "err", err.Error())
		}
	}()
	a.liveMapMu.Lock()
	a.liveMapNitradoCache[inst.InstallationID] = entry
	if len(a.liveMapNitradoCache) > 1024 {
		for id, e := range a.liveMapNitradoCache {
			if time.Since(e.fetched) >= liveMapNitradoTTL {
				delete(a.liveMapNitradoCache, id)
			}
		}
	}
	a.liveMapMu.Unlock()
	return entry
}

// liveMapClock assembles the clock block from the ADM reading, the boot session and Nitrado.
func (a *App) liveMapClock(ctx context.Context, inst repository.LiveMapInstallation, st repository.LiveMapStatus, now time.Time) liveMapClockDTO {
	clock := liveMapClockDTO{UTCOffsetMinutes: st.UTCOffsetMinutes}
	if st.Clock != nil {
		if s, ok := livemap.WallClock(st.Clock.LocalTime, st.Clock.ObservedAt, now, liveMapClockMaxAge); ok {
			clock.ServerLocalTime = &s
		}
	}
	n := a.liveMapNitrado(ctx, inst)
	if at, ok := livemap.NextRestart(liveMapTasks(n.tasks), now); ok {
		clock.NextRestartAt = nullableTimeStr(&at)
	}
	if st.Session == nil {
		return clock
	}
	booted := st.Session.SelectedAt
	clock.BootedAt = nullableTimeStr(&booted)
	if n.facts != nil {
		if t, ok := livemap.InGameTime(n.facts.ServerTime, n.facts.ServerTimeAcceleration, booted, st.Session.LocalStart, now); ok {
			clock.InGame = &liveMapInGameDTO{Time: t, Estimated: true}
		}
	}
	return clock
}

func liveMapTasks(tasks []nitrado.ScheduledTask) []livemap.Task {
	out := make([]livemap.Task, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, livemap.Task{ActionMethod: t.ActionMethod, NextRun: t.NextRun})
	}
	return out
}

// liveMapAuditOnce records action for the actor at most once per liveMapAuditEvery: the live
// view polls every few seconds and one audit row per poll would bury the log.
func (a *App) liveMapAuditOnce(ctx context.Context, ac adminActor, action string) {
	key := action + "|" + strconv.FormatInt(ac.user.ID, 10) + "|" + strconv.FormatInt(ac.scope.InstallationID, 10)
	now := time.Now()
	a.liveMapMu.Lock()
	if a.liveMapAudited == nil {
		a.liveMapAudited = map[string]time.Time{}
	}
	last, seen := a.liveMapAudited[key]
	if seen && now.Sub(last) < liveMapAuditEvery {
		a.liveMapMu.Unlock()
		return
	}
	a.liveMapAudited[key] = now
	if len(a.liveMapAudited) > 4096 {
		for k, t := range a.liveMapAudited {
			if now.Sub(t) >= liveMapAuditEvery {
				delete(a.liveMapAudited, k)
			}
		}
	}
	a.liveMapMu.Unlock()
	a.recordAudit(ctx, ac, action, "installation", "", "SUCCESS", nil, nil)
}

// --- 1. public map state -------------------------------------------------------------------------

// buildPublicLiveMap loads the public map of one installation. nil means not found or not
// public; the two are deliberately indistinguishable.
func (a *App) buildPublicLiveMap(ctx context.Context, installationID int64, windowMinutes int) (*liveMapPublicDTO, error) {
	inst, err := a.LiveMap.Installation(ctx, installationID)
	if err != nil || inst == nil || !inst.Settings.Public {
		return nil, err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(inst.Settings.DelaySeconds) * time.Second)
	st, err := a.LiveMap.Status(ctx, inst.GuildID, inst.ServerID, now)
	if err != nil {
		return nil, err
	}
	out := &liveMapPublicDTO{InstallationID: inst.InstallationID, Name: inst.Name, Platform: inst.Platform, Map: liveMapMap(inst.MapKey), Public: true, Listed: inst.Listed,
		DelaySeconds: inst.Settings.DelaySeconds, GeneratedAt: rfc3339(now), PlayersOnline: st.PlayersOnline, LastActivityAt: nullableTimeStr(st.LastActivityAt),
		Kills: []liveMapKillDTO{}, Pressure: liveMapPressureDTO{Resolution: int(liveMapPressureRes), WindowMinutes: int(liveMapPressureWindow.Minutes()), Cells: []liveMapCellDTO{}}}
	out.PlayersOnline = a.liveMapPlayersOnline(inst.ServerID, st.PlayersOnline, now)
	out.Clock = a.liveMapClock(ctx, *inst, st, now)
	out.Territory = a.publicTerritory(ctx, inst.ServerID, now)

	if a.Fights != nil {
		kills, err := a.Fights.RecentKills(ctx, inst.GuildID, inst.ServerID, cutoff.Add(-time.Duration(windowMinutes)*time.Minute), cutoff.Add(time.Microsecond), liveMapKillLimit)
		if err != nil {
			return nil, err
		}
		out.Kills = filterLiveMapKills(kills, cutoff, 0, liveMapKillLimit)
		out.LastKillID = maxKillID(out.Kills)
	}
	points, err := a.LiveMap.PressurePoints(ctx, inst.GuildID, inst.ServerID, cutoff.Add(-liveMapPressureWindow), cutoff.Add(time.Microsecond), liveMapPressurePoints)
	if err != nil {
		return nil, err
	}
	pts := make([]livemap.Point, 0, len(points))
	for _, p := range points {
		pts = append(pts, livemap.Point{X: p.X, Z: p.Z, At: p.At})
	}
	for _, c := range publicPressure(livemap.Pressure(pts, cutoff, liveMapPressureWindow, liveMapPressureRes, liveMapPressureCells)) {
		out.Pressure.Cells = append(out.Pressure.Cells, liveMapCellDTO{CenterX: c.CenterX, CenterZ: c.CenterZ, Count: c.Count, Intensity: c.Intensity})
	}
	if a.Events != nil {
		events, err := a.Events.HotZones(ctx, inst.GuildID, inst.ServerID, true, 1)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 && (events[0].EndsAt == nil || events[0].EndsAt.After(now)) {
			dto := a.toHotZoneDTO(ctx, events[0], 10)
			out.HotZone = &dto
		}
	}
	return out, nil
}

// handlePublicLiveMap is GET /api/saas/network/servers/{installationID}/map?sinceKillId=&window=
// (bearer only). Not gated on the network listing: the installation's own live_map_public switch
// decides, and an installation that is off, missing or without a server is a plain 404.
func (a *App) handlePublicLiveMap(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if a.LiveMap == nil {
		writeSaaSError(w, codeInternalError, "live map unavailable")
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	window, ok := queryInt(w, r, "window", 60, 5, 180)
	if !ok {
		return
	}
	var sinceKillID int64
	if raw := strings.TrimSpace(r.URL.Query().Get("sinceKillId")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeSaaSError(w, codeInvalidRequest, "sinceKillId must be a non-negative integer")
			return
		}
		sinceKillID = v
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	key := strconv.FormatInt(installationID, 10) + "|" + strconv.Itoa(window)
	a.liveMapMu.Lock()
	if a.liveMapPublic == nil {
		a.liveMapPublic = map[string]liveMapCacheEntry{}
	}
	entry, hit := a.liveMapPublic[key]
	a.liveMapMu.Unlock()
	if !hit || time.Since(entry.at) >= liveMapCacheTTL {
		// Pollers that miss together share one build instead of each running the queries.
		built, err, _ := a.liveMapBuilds.Do(key, func() (any, error) { return a.buildPublicLiveMap(context.WithoutCancel(ctx), installationID, window) })
		v, _ := built.(*liveMapPublicDTO)
		if err != nil {
			slog.Warn("component=live_map", "event", "public_map_failed", "installation_id", installationID, "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not load the map")
			return
		}
		entry = liveMapCacheEntry{value: v, at: time.Now()}
		a.liveMapMu.Lock()
		if a.liveMapPublic == nil {
			a.liveMapPublic = map[string]liveMapCacheEntry{}
		}
		if len(a.liveMapPublic) >= networkCacheMax {
			a.liveMapPublic = map[string]liveMapCacheEntry{}
		}
		a.liveMapPublic[key] = entry
		a.liveMapMu.Unlock()
	}
	if entry.value == nil {
		writeSaaSError(w, codeNotFound, "map not found")
		return
	}
	resp := *entry.value
	if sinceKillID > 0 {
		resp.Kills = killsSince(resp.Kills, sinceKillID, liveMapRecentResend)
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// --- 2. faction layer ----------------------------------------------------------------------------

// handlePlayerFactionMap is GET /api/saas/player/servers/{installationID}/map/faction: the
// acting player's own hub faction on the map. Only members' positions are ever read, so a player
// outside the faction - or in none - can never see anyone but themselves.
func (a *App) handlePlayerFactionMap(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if !enforceRateLimit(w, a.saasLiveMapLimiter, rateLimitKey(r)) {
		return
	}
	if a.LiveMap == nil || a.FeatureSettings == nil {
		writeSaaSError(w, codeInternalError, "live map unavailable")
		return
	}
	now := time.Now().UTC()
	self, err := a.LiveMap.Player(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	if self == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	resp := liveMapFactionLayerDTO{Self: liveMapSelfDTO{PlayerID: self.PlayerID, Gamertag: self.Gamertag},
		Members: []liveMapMemberDTO{}, Bases: []liveMapBaseDTO{}, Alerts: []liveMapAlertDTO{}}
	settings, err := a.FeatureSettings.Get(ctx, scope.InstallationID)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	if !settings.LiveMap.FactionLayer {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	resp.Enabled = true
	faction, err := a.LiveMap.CurrentFaction(ctx, scope.InstallationID, strings.TrimSpace(r.Header.Get(actingUserHeader)))
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	members := []repository.LiveMapPlayer{*self}
	if faction != nil {
		resp.Faction = &liveMapFactionDTO{ID: faction.ID, Name: faction.Name, Tag: faction.Tag, PrimaryColor: faction.PrimaryColor, SecondaryColor: faction.SecondaryColor}
		rows, err := a.LiveMap.FactionMembers(ctx, scope.InstallationID, faction.ID, scope.GuildID, scope.ServerID)
		if err != nil {
			playerFailed(w, "faction map", err)
			return
		}
		hasSelf := false
		for _, m := range rows {
			if m.PlayerID == self.PlayerID {
				hasSelf = true
			}
		}
		if hasSelf {
			members = rows
		} else {
			members = append(members, rows...)
		}
	}
	ids := make([]int64, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.PlayerID)
	}
	st, err := a.LiveMap.Status(ctx, scope.GuildID, scope.ServerID, now)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	since, admFile := sessionWindow(st, now)
	positions, err := a.LiveMap.LatestPositions(ctx, scope.ServerID, ids, since, admFile)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	trailSince := now.Add(-liveMapTrailWindow)
	if since.After(trailSince) {
		trailSince = since
	}
	trails, err := a.LiveMap.Trails(ctx, scope.ServerID, ids, trailSince, admFile, liveMapTrailLength)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	for _, m := range members {
		dto := liveMapMemberDTO{PlayerID: m.PlayerID, Gamertag: m.Gamertag, IsSelf: m.PlayerID == self.PlayerID, Online: m.Online, Trail: []liveMapTrailPointDTO{}}
		if p, ok := positions[m.PlayerID]; ok {
			dto.LastKnown = toLiveMapPosition(p, now)
			for _, t := range trails[m.PlayerID] {
				dto.Trail = append(dto.Trail, liveMapTrailPointDTO{X: t.X, Z: t.Z, ObservedAt: rfc3339(t.ObservedAt)})
			}
		}
		resp.Members = append(resp.Members, dto)
	}
	bases, err := a.LiveMap.MemberBases(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, ids, now)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	baseIDs := make([]int64, 0, len(bases))
	for _, b := range bases {
		baseIDs = append(baseIDs, b.ID)
		resp.Bases = append(resp.Bases, liveMapBaseDTO{ID: b.ID, Name: b.Name, CenterX: b.CenterX, CenterZ: b.CenterZ, Radius: b.Radius, OwnerPlayerID: b.OwnerPlayerID})
	}
	alerts, err := a.LiveMap.RaidAlerts(ctx, scope.InstallationID, baseIDs, now.Add(-liveMapAlertWindow), liveMapAlertLimit)
	if err != nil {
		playerFailed(w, "faction map", err)
		return
	}
	for _, al := range alerts {
		resp.Alerts = append(resp.Alerts, liveMapAlertDTO{BaseID: al.BaseID, BaseName: al.BaseName, Kind: "RAID", Part: al.Part, Target: al.Target, RaiderName: al.RaiderName, At: rfc3339(al.At)})
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// --- 3. staff live and history -------------------------------------------------------------------

func liveMapIntrusions(rows []repository.ZoneIntrusion, zones []repository.Zone, withPresence bool) []liveMapIntrusionDTO {
	centers := map[int64]repository.Zone{}
	for _, z := range zones {
		centers[z.ID] = z
	}
	out := make([]liveMapIntrusionDTO, 0, len(rows))
	for _, it := range rows {
		dto := liveMapIntrusionDTO{zoneIntrusionDTO: toZoneIntrusionDTO(it, withPresence)}
		if z, ok := centers[it.ZoneID]; ok {
			dto.CenterX, dto.CenterZ = z.CenterX, z.CenterZ
			if dto.ZoneName == "" {
				dto.ZoneName, dto.ZoneType = z.Name, z.ZoneType
			}
		}
		out = append(out, dto)
	}
	return out
}

// handleAdminLiveMap is GET .../admin/map/live (PLAYER_LAST_LOCATION_VIEW): every connected
// player's latest position, the zones and the active intrusions. Audited once per actor per ten
// minutes rather than per poll.
func (a *App) handleAdminLiveMap(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLastLocationView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.LiveMap == nil {
		writeSaaSError(w, codeInternalError, "live map unavailable")
		return
	}
	now := time.Now().UTC()
	resp := liveMapStaffLiveDTO{GeneratedAt: rfc3339(now), Players: []liveMapStaffPlayerDTO{}, Zones: []zoneDTO{}, Intrusions: []liveMapIntrusionDTO{}}
	if ac.scope.ServerID == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	serverID := *ac.scope.ServerID
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	fail := func(err error) {
		slog.Warn("component=live_map", "event", "staff_live_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the live map")
	}
	players, err := a.LiveMap.OnlinePlayers(ctx, ac.scope.InstallationID, ac.scope.GuildID, serverID)
	if err != nil {
		fail(err)
		return
	}
	st, err := a.LiveMap.Status(ctx, ac.scope.GuildID, serverID, now)
	if err != nil {
		fail(err)
		return
	}
	ids := make([]int64, 0, len(players))
	for _, p := range players {
		ids = append(ids, p.PlayerID)
	}
	since, admFile := sessionWindow(st, now)
	positions, err := a.LiveMap.LatestPositions(ctx, serverID, ids, since, admFile)
	if err != nil {
		fail(err)
		return
	}
	for _, p := range players {
		dto := liveMapStaffPlayerDTO{PlayerID: p.PlayerID, Gamertag: p.Gamertag, Online: p.Online, FactionTag: p.FactionTag}
		if pos, ok := positions[p.PlayerID]; ok {
			dto.LastKnown = toLiveMapPosition(pos, now)
		}
		resp.Players = append(resp.Players, dto)
	}
	if a.Zones != nil {
		all, err := a.Zones.ActiveZonesForServer(ctx, serverID)
		if err != nil {
			fail(err)
			return
		}
		// Zones belong to an installation; only this installation's are shown.
		zones := make([]repository.Zone, 0, len(all))
		for _, z := range all {
			if z.InstallationID == ac.scope.InstallationID {
				zones = append(zones, z)
				resp.Zones = append(resp.Zones, toZoneDTO(z))
			}
		}
		intrusions, err := a.Zones.ListActiveForInstallation(ctx, ac.scope.InstallationID, repository.ActiveIntrusionFilter{})
		if err != nil {
			fail(err)
			return
		}
		resp.Intrusions = liveMapIntrusions(intrusions, zones, true)
	}
	a.liveMapAuditOnce(ctx, ac, "LIVE_MAP_VIEWED")
	writeSaaSJSON(w, http.StatusOK, resp)
}

// parseLiveMapWindow reads from/to (RFC 3339): default the last hour, at most liveMapHistoryMax.
func parseLiveMapWindow(w http.ResponseWriter, r *http.Request, now time.Time) (from, to time.Time, ok bool) {
	q := r.URL.Query()
	to = now
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "to must be an RFC 3339 time")
			return from, to, false
		}
		to = t.UTC()
	}
	from = to.Add(-time.Hour)
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "from must be an RFC 3339 time")
			return from, to, false
		}
		from = t.UTC()
	}
	if !from.Before(to) {
		writeSaaSError(w, codeInvalidRequest, "from must be before to")
		return from, to, false
	}
	if to.Sub(from) > liveMapHistoryMax {
		writeSaaSError(w, codeInvalidRequest, "the window must be 6 hours or shorter")
		return from, to, false
	}
	return from, to, true
}

// handleAdminMapHistory is GET .../admin/map/history?from=&to= (PLAYER_LOCATION_VIEW): the kills
// and every player's track in the window, newest-first truncated at liveMapHistoryPoints samples.
func (a *App) handleAdminMapHistory(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	now := time.Now().UTC()
	from, to, ok := parseLiveMapWindow(w, r, now)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.LiveMap == nil {
		writeSaaSError(w, codeInternalError, "live map unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	fail := func(err error) {
		slog.Warn("component=live_map", "event", "history_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the map history")
	}
	mapKey := ""
	if a.Shop != nil {
		mapKey = a.replayMapKey(ctx, ac.scope.OrganizationID, ac.scope.InstallationID)
	} else if inst, err := a.LiveMap.Installation(ctx, ac.scope.InstallationID); err == nil && inst != nil {
		mapKey = inst.MapKey
	}
	resp := liveMapHistoryDTO{From: rfc3339(from), To: rfc3339(to), Map: liveMapMap(mapKey), Kills: []liveMapHistoryKillDTO{}, Tracks: []liveMapHistoryTrackDTO{}, Intrusions: []liveMapIntrusionDTO{}}
	if ac.scope.ServerID == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	serverID := *ac.scope.ServerID
	if a.Fights != nil {
		kills, err := a.Fights.RecentKills(ctx, ac.scope.GuildID, serverID, from, to.Add(time.Microsecond), liveMapHistoryKills)
		if err != nil {
			fail(err)
			return
		}
		for _, k := range kills {
			resp.Kills = append(resp.Kills, liveMapHistoryKillDTO{liveMapKillDTO: toLiveMapKill(k), KillerID: k.KillerID, VictimID: k.VictimID})
		}
	}
	samples, err := a.LiveMap.Tracks(ctx, ac.scope.GuildID, serverID, from, to, liveMapHistoryPoints+1)
	if err != nil {
		fail(err)
		return
	}
	if len(samples) > liveMapHistoryPoints {
		samples, resp.Truncated = samples[:liveMapHistoryPoints], true
	}
	tracks := map[int64]*liveMapHistoryTrackDTO{}
	order := []int64{}
	for i := len(samples) - 1; i >= 0; i-- { // the query is newest first; tracks run oldest first
		s := samples[i]
		t, ok := tracks[s.PlayerID]
		if !ok {
			t = &liveMapHistoryTrackDTO{PlayerID: s.PlayerID, Gamertag: s.Gamertag, Points: []liveMapHistoryPointDTO{}}
			tracks[s.PlayerID] = t
			order = append(order, s.PlayerID)
		}
		t.Gamertag = s.Gamertag // the newest name wins
		t.Points = append(t.Points, liveMapHistoryPointDTO{T: rfc3339(s.At), X: s.X, Z: s.Z, Type: s.EventType})
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := tracks[order[i]], tracks[order[j]]
		if la, lb := strings.ToLower(a.Gamertag), strings.ToLower(b.Gamertag); la != lb {
			return la < lb
		}
		return a.PlayerID < b.PlayerID
	})
	for _, id := range order {
		resp.Tracks = append(resp.Tracks, *tracks[id])
	}
	if a.Zones != nil {
		zones, err := a.Zones.ListZones(ctx, ac.scope.InstallationID)
		if err != nil {
			fail(err)
			return
		}
		intrusions, err := a.Zones.ListHistory(ctx, ac.scope.InstallationID, repository.IntrusionHistoryFilter{From: &from, To: &to, Limit: liveMapHistoryIntr})
		if err != nil {
			fail(err)
			return
		}
		resp.Intrusions = liveMapIntrusions(intrusions, zones, false)
	}
	a.recordAudit(ctx, ac, "LIVE_MAP_HISTORY_VIEWED", "installation", "", "SUCCESS", nil,
		map[string]any{"from": resp.From, "to": resp.To, "minutes": int(math.Round(to.Sub(from).Minutes()))})
	writeSaaSJSON(w, http.StatusOK, resp)
}

// liveMapRecentResend: with sinceKillId, kills from the last few minutes are sent again even when
// their id is not above the cursor. Kill ids follow ingest order, not event time, so behind a delay
// a lower-id kill can reach the map after a higher one; resending the recent ones (the client
// dedupes by id) means none is skipped.
const liveMapRecentResend = 5

// killsSince is the kills a client that already has everything up to sinceID still needs: newer
// ids, plus the most recent `recent` kills again.
func killsSince(kills []liveMapKillDTO, sinceID int64, recent int) []liveMapKillDTO {
	out := make([]liveMapKillDTO, 0, len(kills))
	for i, k := range kills {
		if k.ID > sinceID || i < recent {
			out = append(out, k)
		}
	}
	return out
}

// maxKillID is the highest kill id (the cursor clients send back as sinceKillId).
func maxKillID(kills []liveMapKillDTO) int64 {
	var max int64
	for _, k := range kills {
		if k.ID > max {
			max = k.ID
		}
	}
	return max
}

// liveMapPressureMinCount: a public pressure cell needs this many events, so a single wounded
// player cannot be placed on the map by their own hits.
const liveMapPressureMinCount = 2

// publicPressure drops the cells with too few events and rescales the rest.
func publicPressure(cells []livemap.Cell) []livemap.Cell {
	out := make([]livemap.Cell, 0, len(cells))
	max := 0.0
	for _, c := range cells {
		if c.Count >= liveMapPressureMinCount {
			out = append(out, c)
			if float64(c.Count) > max {
				max = float64(c.Count)
			}
		}
	}
	for i := range out {
		out[i].Intensity = float64(out[i].Count) / max
	}
	return out
}

// liveMapOnlineCounterMaxAge is how old the online counter's reading may be and still be shown
// on the map instead of the count taken from the server log.
const liveMapOnlineCounterMaxAge = 5 * time.Minute

// liveMapPlayersOnline is the number of players shown on the map. The server log only knows a
// player is connected once it has written a line about them, which a quiet server may not do for
// a long time after a restart, so for the server the online counter watches, its reading (the one
// the Discord counter shows, normally Nitrado's own count) wins while it is fresh.
func (a *App) liveMapPlayersOnline(serverID int64, fromLog int, now time.Time) int {
	st := a.OnlineCounterStatus()
	return pickLiveMapPlayersOnline(st, serverID, fromLog, now)
}

func pickLiveMapPlayersOnline(st onlineCounterStatus, serverID int64, fromLog int, now time.Time) int {
	if st.ServerID != serverID || !st.Reading.Known || st.EvaluatedAt.IsZero() || now.Sub(st.EvaluatedAt) > liveMapOnlineCounterMaxAge {
		return fromLog
	}
	if st.Reading.Count < 0 {
		return fromLog
	}
	return st.Reading.Count
}
