package app

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/fights"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Fight replay (docs/FIGHT_REPLAY.md): kills grouped into fights, and for one fight the persisted
// position samples of its participants around it, as a timeline the website animates on the map.
//
// Positions are sensitive in DayZ - a track can lead back to a base. So: staff need the location
// capability; players only see replays when the installation opted in, only for fights older than
// the installation's delay, and with a shorter lead-in than staff get.

const (
	fightKillLimit       = 2000 // kills read for one listing or one replay lookup
	fightTrackLimit      = 5000 // position samples in one replay
	fightLookupMargin    = 30 * time.Minute
	fightTail            = 30 * time.Second
	fightStaffLead       = 5 * time.Minute
	fightPublicLead      = 2 * time.Minute
	fightPublicListHours = 72
)

func (a *App) registerFightRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/fights", a.handleAdminFights)
	h("GET "+adminBase+"/fights/{killID}", a.handleAdminFightReplay)
	h("GET /api/saas/player/servers/{installationID}/fights", a.handlePlayerFights)
	h("GET /api/saas/player/servers/{installationID}/fights/{killID}", a.handlePlayerFightReplay)
}

type fightSummaryDTO struct {
	ID           int64    `json:"id"` // the fight's first kill
	StartedAt    string   `json:"startedAt"`
	EndedAt      string   `json:"endedAt"`
	Kills        int      `json:"kills"`
	Participants []string `json:"participants"`
	CenterX      *float64 `json:"centerX"`
	CenterZ      *float64 `json:"centerZ"`
}

type fightPlayerDTO struct {
	PlayerID int64  `json:"playerId"`
	Name     string `json:"name"`
	Kills    int    `json:"kills"`
	Deaths   int    `json:"deaths"`
}

type fightKillDTO struct {
	KillID         int64    `json:"killId"`
	T              float64  `json:"t"` // seconds from the replay's start
	KillerID       int64    `json:"killerId"`
	VictimID       int64    `json:"victimId"`
	Weapon         string   `json:"weapon"`
	DistanceMeters *float64 `json:"distanceMeters"`
	Headshot       bool     `json:"headshot"`
	KillerX        *float64 `json:"killerX"`
	KillerZ        *float64 `json:"killerZ"`
	VictimX        *float64 `json:"victimX"`
	VictimZ        *float64 `json:"victimZ"`
}

type fightPointDTO struct {
	T    float64 `json:"t"`
	X    float64 `json:"x"`
	Z    float64 `json:"z"`
	Type string  `json:"type"` // the ADM event the sample came from (HIT, KILL, OTHER_ADM, ...)
}

type fightTrackDTO struct {
	PlayerID int64           `json:"playerId"`
	Points   []fightPointDTO `json:"points"`
}

type fightBoundsDTO struct {
	MinX float64 `json:"minX"`
	MinZ float64 `json:"minZ"`
	MaxX float64 `json:"maxX"`
	MaxZ float64 `json:"maxZ"`
}

type fightReplayDTO struct {
	fightSummaryDTO
	ReplayStart     string           `json:"replayStart"`
	DurationSeconds float64          `json:"durationSeconds"`
	Players         []fightPlayerDTO `json:"players"`
	KillEvents      []fightKillDTO   `json:"killEvents"`
	Tracks          []fightTrackDTO  `json:"tracks"`
	Bounds          *fightBoundsDTO  `json:"bounds"`
	// Truncated is true when the replay hit the sample cap and later samples were left out.
	Truncated bool `json:"truncated"`
	// MapKey is the server's configured DayZ map ("" when unset), so the website can draw the
	// replay on that map. Fights don't record their own map; the configured one is the best guess.
	MapKey string `json:"mapKey"`
}

func point(x, z *float64) *fights.Point {
	if x == nil || z == nil {
		return nil
	}
	return &fights.Point{X: *x, Z: *z}
}

// groupFights reads the server's kills in [from, to) and groups them, returning the kills by id.
func (a *App) groupFights(ctx context.Context, guildID, serverID int64, from, to time.Time) ([]fights.Fight, map[int64]repository.FightKill, error) {
	rows, err := a.Fights.Kills(ctx, guildID, serverID, from, to, fightKillLimit)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]repository.FightKill, len(rows))
	kills := make([]fights.Kill, 0, len(rows))
	for _, k := range rows {
		byID[k.ID] = k
		kills = append(kills, fights.Kill{ID: k.ID, At: k.At, KillerID: k.KillerID, VictimID: k.VictimID,
			KillerPos: point(k.KillerX, k.KillerZ), VictimPos: point(k.VictimX, k.VictimZ)})
	}
	return fights.Group(kills, fights.DefaultGap, fights.DefaultRadius), byID, nil
}

func fightNames(f fights.Fight, byID map[int64]repository.FightKill) map[int64]string {
	names := map[int64]string{}
	for _, k := range f.Kills {
		row := byID[k.ID]
		names[k.KillerID], names[k.VictimID] = row.KillerName, row.VictimName
	}
	return names
}

func toFightSummary(f fights.Fight, byID map[int64]repository.FightKill) fightSummaryDTO {
	names := fightNames(f, byID)
	dto := fightSummaryDTO{ID: f.ID(), StartedAt: f.Start().UTC().Format(time.RFC3339), EndedAt: f.End().UTC().Format(time.RFC3339), Kills: len(f.Kills)}
	for _, id := range f.Participants() {
		dto.Participants = append(dto.Participants, names[id])
	}
	if c, ok := f.Center(); ok {
		dto.CenterX, dto.CenterZ = &c.X, &c.Z
	}
	return dto
}

// listFights returns fights with at least minKills kills that ended in [from, before], newest first.
func (a *App) listFights(ctx context.Context, guildID, serverID int64, from, before time.Time, minKills int) ([]fightSummaryDTO, error) {
	grouped, byID, err := a.groupFights(ctx, guildID, serverID, from, before)
	if err != nil {
		return nil, err
	}
	out := []fightSummaryDTO{}
	for i := len(grouped) - 1; i >= 0; i-- {
		f := grouped[i]
		// A fight whose last kill is within the grouping gap of the cut-off may still be going on
		// past it; it is listed once it is whole.
		if len(f.Kills) < minKills || before.Sub(f.End()) < fights.DefaultGap {
			continue
		}
		out = append(out, toFightSummary(f, byID))
	}
	return out, nil
}

// buildReplay returns the replay of the fight containing killID, or nil when the kill does not
// exist on the server or its fight ended after notAfter.
func (a *App) buildReplay(ctx context.Context, guildID, serverID, killID int64, lead time.Duration, notAfter time.Time) (*fightReplayDTO, error) {
	at, found, err := a.Fights.KillTime(ctx, guildID, serverID, killID)
	if err != nil || !found {
		return nil, err
	}
	grouped, byID, err := a.groupFights(ctx, guildID, serverID, at.Add(-fightLookupMargin), at.Add(fightLookupMargin))
	if err != nil {
		return nil, err
	}
	f, ok := fights.Containing(grouped, killID)
	if !ok || f.End().After(notAfter) {
		return nil, nil
	}
	start, end := f.Start().Add(-lead), f.End().Add(fightTail)
	seconds := func(t time.Time) float64 { return math.Round(t.Sub(start).Seconds()*10) / 10 }

	replay := &fightReplayDTO{fightSummaryDTO: toFightSummary(f, byID), ReplayStart: start.UTC().Format(time.RFC3339),
		DurationSeconds: seconds(end), Players: []fightPlayerDTO{}, KillEvents: []fightKillDTO{}, Tracks: []fightTrackDTO{}}
	names := fightNames(f, byID)
	players := map[int64]*fightPlayerDTO{}
	for _, id := range f.Participants() {
		players[id] = &fightPlayerDTO{PlayerID: id, Name: names[id]}
	}
	var bounds *fightBoundsDTO
	grow := func(x, z float64) {
		if bounds == nil {
			bounds = &fightBoundsDTO{MinX: x, MinZ: z, MaxX: x, MaxZ: z}
			return
		}
		bounds.MinX, bounds.MaxX = math.Min(bounds.MinX, x), math.Max(bounds.MaxX, x)
		bounds.MinZ, bounds.MaxZ = math.Min(bounds.MinZ, z), math.Max(bounds.MaxZ, z)
	}
	for _, k := range f.Kills {
		row := byID[k.ID]
		players[k.KillerID].Kills++
		players[k.VictimID].Deaths++
		replay.KillEvents = append(replay.KillEvents, fightKillDTO{KillID: k.ID, T: seconds(k.At), KillerID: k.KillerID, VictimID: k.VictimID, Weapon: row.Weapon,
			DistanceMeters: row.Distance, Headshot: row.Headshot, KillerX: row.KillerX, KillerZ: row.KillerZ, VictimX: row.VictimX, VictimZ: row.VictimZ})
		for _, p := range []*fights.Point{k.KillerPos, k.VictimPos} {
			if p != nil {
				grow(p.X, p.Z)
			}
		}
	}
	ids := f.Participants()
	for _, id := range ids {
		replay.Players = append(replay.Players, *players[id])
	}
	samples, err := a.Fights.Tracks(ctx, serverID, ids, start, end, fightTrackLimit+1)
	if err != nil {
		return nil, err
	}
	if len(samples) > fightTrackLimit {
		samples, replay.Truncated = samples[:fightTrackLimit], true
	}
	tracks := map[int64]*fightTrackDTO{}
	for _, id := range ids {
		tracks[id] = &fightTrackDTO{PlayerID: id, Points: []fightPointDTO{}}
	}
	for _, s := range samples {
		tracks[s.PlayerID].Points = append(tracks[s.PlayerID].Points, fightPointDTO{T: seconds(s.At), X: s.X, Z: s.Z, Type: s.EventType})
		grow(s.X, s.Z)
	}
	for _, id := range ids {
		replay.Tracks = append(replay.Tracks, *tracks[id])
	}
	replay.Bounds = bounds
	return replay, nil
}

// handleAdminFights is GET .../admin/fights?hours=&minKills= (PLAYER_LOCATION_VIEW).
func (a *App) handleAdminFights(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.FightReplay) {
		return
	}
	hours, ok := queryInt(w, r, "hours", 24, 1, 168)
	if !ok {
		return
	}
	minKills, ok := queryInt(w, r, "minKills", 2, 1, 50)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	resp := struct {
		Items []fightSummaryDTO `json:"items"`
	}{Items: []fightSummaryDTO{}}
	if a.Fights == nil || ac.scope.ServerID == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	items, err := a.listFights(ctx, ac.scope.GuildID, *ac.scope.ServerID, now.Add(-time.Duration(hours)*time.Hour), now, minKills)
	if err != nil {
		slog.Warn("component=saas_api", "event", "fights_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load fights")
		return
	}
	resp.Items = items
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handleAdminFightReplay is GET .../admin/fights/{killID} (PLAYER_LOCATION_VIEW): the replay of
// the fight containing that kill. Audited, like every other location read.
func (a *App) handleAdminFightReplay(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	if !a.requirePlanFeature(w, r, ac.scope.OrganizationID, entitlements.FightReplay) {
		return
	}
	killID, ok := pathInt64(w, r, "killID")
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Fights == nil || ac.scope.ServerID == nil {
		writeSaaSError(w, codeNotFound, "fight not found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	replay, err := a.buildReplay(ctx, ac.scope.GuildID, *ac.scope.ServerID, killID, fightStaffLead, time.Now().UTC())
	if err != nil {
		slog.Warn("component=saas_api", "event", "fight_replay_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the fight")
		return
	}
	if replay == nil {
		writeSaaSError(w, codeNotFound, "fight not found")
		return
	}
	replay.MapKey = a.replayMapKey(ctx, ac.scope.OrganizationID, ac.scope.InstallationID)
	a.recordAudit(ctx, ac, "FIGHT_REPLAY_VIEWED", "fight:"+strconv.FormatInt(replay.ID, 10), "", "SUCCESS", nil, nil)
	writeSaaSJSON(w, http.StatusOK, replay)
}

// publicReplayCutoff resolves the installation's public-replay setting: whether players may watch
// at all, and the latest moment a fight may have ended to be shown. An organization whose plan
// does not include fight replay reads as "not public", whatever it saved while it had the plan:
// to its players the feature is simply off, and the setting comes back if it upgrades.
func (a *App) publicReplayCutoff(ctx context.Context, organizationID, installationID int64) (enabled bool, cutoff time.Time, delayMinutes int, err error) {
	if a.FeatureSettings == nil || a.Fights == nil {
		return false, time.Time{}, 0, nil
	}
	if entitlements.Enforced() {
		plan, err := a.organizationPlan(ctx, organizationID)
		if err != nil {
			return false, time.Time{}, 0, err
		}
		if !entitlements.Has(plan, entitlements.FightReplay) {
			return false, time.Time{}, 0, nil
		}
	}
	s, err := a.FeatureSettings.Get(ctx, installationID)
	if err != nil {
		return false, time.Time{}, 0, err
	}
	return s.FightReplay.Public, time.Now().UTC().Add(-time.Duration(s.FightReplay.DelayMinutes) * time.Minute), s.FightReplay.DelayMinutes, nil
}

// handlePlayerFights is GET /api/saas/player/servers/{installationID}/fights: recent fights a
// verified player of the server may watch. enabled=false (and no items) when the installation has
// not made replays public.
func (a *App) handlePlayerFights(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	resp := struct {
		Enabled      bool              `json:"enabled"`
		DelayMinutes int               `json:"delayMinutes"`
		Items        []fightSummaryDTO `json:"items"`
	}{Items: []fightSummaryDTO{}}
	enabled, cutoff, delay, err := a.publicReplayCutoff(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil {
		playerFailed(w, "fights", err)
		return
	}
	if !enabled {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	resp.Enabled, resp.DelayMinutes = true, delay
	items, err := a.listFights(ctx, scope.GuildID, scope.ServerID, cutoff.Add(-fightPublicListHours*time.Hour), cutoff, 2)
	if err != nil {
		playerFailed(w, "fights", err)
		return
	}
	resp.Items = items
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handlePlayerFightReplay is GET .../fights/{killID}: one replay, under the same rules as the
// list. A fight that is not public yet is indistinguishable from one that does not exist.
func (a *App) handlePlayerFightReplay(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	killID, ok := pathInt64(w, r, "killID")
	if !ok {
		return
	}
	enabled, cutoff, _, err := a.publicReplayCutoff(ctx, scope.OrganizationID, scope.InstallationID)
	if err != nil {
		playerFailed(w, "fight", err)
		return
	}
	if !enabled {
		writeSaaSError(w, codeNotFound, "fight not found")
		return
	}
	replay, err := a.buildReplay(ctx, scope.GuildID, scope.ServerID, killID, fightPublicLead, cutoff)
	if err != nil {
		playerFailed(w, "fight", err)
		return
	}
	if replay == nil {
		writeSaaSError(w, codeNotFound, "fight not found")
		return
	}
	replay.MapKey = a.replayMapKey(ctx, scope.OrganizationID, scope.InstallationID)
	writeSaaSJSON(w, http.StatusOK, replay)
}

// replayMapKey is the installation's configured DayZ map (the shop delivery map), or "" when it
// is unset, no longer supported, or cannot be read. It only decorates the replay, so a failure
// never fails the request.
func (a *App) replayMapKey(ctx context.Context, organizationID, installationID int64) string {
	if a.Shop == nil {
		return ""
	}
	s, err := a.Shop.DeliverySettings(ctx, repository.EconomyScope{OrganizationID: organizationID, InstallationID: installationID})
	if err != nil || s.Map == nil {
		return ""
	}
	return s.Map.Key
}
