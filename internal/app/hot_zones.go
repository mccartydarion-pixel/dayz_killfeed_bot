package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/hotzone"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Hot zones (docs/HOT_ZONES.md): when the PvP heatmap shows enough kills in one grid cell, a short
// event opens there and kills inside the circle score. Everything after the opening - scoring,
// ending, podium rewards, the completion card - is the existing competitive-event machinery; this
// file only decides when one opens, announces it, and exposes it to the website.

// hotZoneCheckInterval is how often each guild's heatmap is checked for a new hot zone.
const hotZoneCheckInterval = 2 * time.Minute

// hotZoneKillSource is the heatmap aggregation a hot zone is detected from.
type hotZoneKillSource interface {
	AggregateKills(ctx context.Context, guildID, serverID int64, from, to time.Time, resolution int, limit int) ([]repository.HeatmapCell, error)
}

// hotZoneTick opens at most one hot zone per enabled server of the guild. Called from the
// competitive scheduler; it throttles itself to hotZoneCheckInterval per guild.
func (a *App) hotZoneTick(ctx context.Context, guildID int64, now time.Time) {
	if a.FeatureSettings == nil || a.Events == nil || a.hotZoneKills == nil {
		return
	}
	a.hotZoneMu.Lock()
	if a.hotZoneChecked == nil {
		a.hotZoneChecked = map[int64]time.Time{}
	}
	if last, ok := a.hotZoneChecked[guildID]; ok && now.Sub(last) < hotZoneCheckInterval {
		a.hotZoneMu.Unlock()
		return
	}
	a.hotZoneChecked[guildID] = now
	a.hotZoneMu.Unlock()

	installations, err := a.FeatureSettings.HotZoneInstallations(ctx, guildID)
	if err != nil {
		slog.Warn("component=hotzone", "event", "settings_failed", "guild_id", guildID, "err", err.Error())
		return
	}
	for _, inst := range installations {
		event, err := a.openHotZone(ctx, inst, now)
		if err != nil {
			slog.Warn("component=hotzone", "event", "open_failed", "server_id", inst.ServerID, "err", err.Error())
			continue
		}
		if event != nil {
			a.announceHotZone(ctx, inst, *event)
		}
	}
}

// openHotZone opens a hot zone for one installation if its heatmap qualifies and the server has no
// hot zone open or cooling down. Returns (nil, nil) when it opens nothing.
func (a *App) openHotZone(ctx context.Context, inst repository.HotZoneInstallation, now time.Time) (*repository.CompetitiveEvent, error) {
	s := inst.Settings
	from := now.Add(-time.Duration(s.WindowMinutes) * time.Minute)
	rows, err := a.hotZoneKills.AggregateKills(ctx, inst.GuildID, inst.ServerID, from, now, hotzone.Resolution, 5000)
	if err != nil {
		return nil, fmt.Errorf("aggregate kills: %w", err)
	}
	cells := make([]hotzone.Cell, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, hotzone.Cell{CellX: r.CellX, CellZ: r.CellZ, Count: r.Count})
	}
	cell, ok := hotzone.Pick(cells, s.MinKills)
	if !ok {
		return nil, nil
	}
	x, z := hotzone.Center(cell)
	config, err := json.Marshal(competitiveevents.HotZoneConfig{ServerID: inst.ServerID, CenterX: x, CenterZ: z, RadiusM: float64(s.RadiusM), Auto: true, KillsObserved: int(cell.Count)})
	if err != nil {
		return nil, err
	}
	ends := now.Add(time.Duration(s.DurationMinutes) * time.Minute)
	var seasonID int64
	if a.Seasons != nil {
		if season, err := a.Seasons.ResolveAt(ctx, inst.GuildID, now); err == nil && season != nil {
			seasonID = season.ID
		}
	}
	return a.Events.CreateHotZone(ctx, repository.CompetitiveEvent{
		GuildID: inst.GuildID, SeasonID: seasonID, Name: hotzone.Name(x, z),
		Description: fmt.Sprintf("%d kills in the last %d minutes", cell.Count, s.WindowMinutes),
		StartsAt:    &now, EndsAt: &ends, Config: config,
	}, inst.ServerID, s.FirstPoints, s.SecondPoints, s.ThirdPoints, now.Add(-time.Duration(s.CooldownMinutes)*time.Minute))
}

func (a *App) announceHotZone(ctx context.Context, inst repository.HotZoneInstallation, event repository.CompetitiveEvent) {
	slog.Info("component=hotzone", "event", "opened", "server_id", inst.ServerID, "event_id", event.ID, "name", event.Name)
	if a.CompletionPublisher == nil {
		return
	}
	var c competitiveevents.HotZoneConfig
	_ = json.Unmarshal(event.Config, &c)
	embed := discord.BuildHotZoneOpenedEmbed(discord.HotZoneAnnouncement{
		Name: event.Name, ServerName: inst.ServerName, CenterX: c.CenterX, CenterZ: c.CenterZ, RadiusM: c.RadiusM, KillsObserved: c.KillsObserved,
		WindowMinutes: inst.Settings.WindowMinutes, EndsAt: event.EndsAt,
		FirstPoints: inst.Settings.FirstPoints, SecondPoints: inst.Settings.SecondPoints, ThirdPoints: inst.Settings.ThirdPoints,
	})
	if err := a.CompletionPublisher.Announce(ctx, embed); err != nil {
		slog.Warn("component=hotzone", "event", "announce_failed", "event_id", event.ID, "err", err.Error())
	}
}

// killPoint converts an ADM position to the horizontal map point events score on.
func killPoint(p *killfeed.PlayerRef) *competitiveevents.Point {
	if p == nil || p.Position == nil {
		return nil
	}
	return &competitiveevents.Point{X: p.Position.MapX(), Z: p.Position.MapZ()}
}

// --- API -----------------------------------------------------------------------------------------

func (a *App) registerHotZoneRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/hot-zones", a.handleAdminHotZones)
	h("GET /api/saas/player/servers/{installationID}/hot-zone", a.handlePlayerHotZone)
	a.registerRivalRoutes()
}

type hotZoneDTO struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	CenterX       float64           `json:"centerX"`
	CenterZ       float64           `json:"centerZ"`
	RadiusMeters  float64           `json:"radiusMeters"`
	KillsObserved int               `json:"killsObserved"`
	StartsAt      *string           `json:"startsAt"`
	EndsAt        *string           `json:"endsAt"`
	Standings     []hotZoneScoreDTO `json:"standings"`
}

type hotZoneScoreDTO struct {
	Rank       int    `json:"rank"`
	PlayerName string `json:"playerName"`
	Kills      int64  `json:"kills"`
}

func (a *App) toHotZoneDTO(ctx context.Context, e repository.CompetitiveEvent, standings int) hotZoneDTO {
	var c competitiveevents.HotZoneConfig
	_ = json.Unmarshal(e.Config, &c)
	dto := hotZoneDTO{ID: e.ID, Name: e.Name, Status: e.Status, CenterX: c.CenterX, CenterZ: c.CenterZ, RadiusMeters: c.RadiusM,
		KillsObserved: c.KillsObserved, StartsAt: nullableTimeStr(e.StartsAt), EndsAt: nullableTimeStr(e.EndsAt), Standings: []hotZoneScoreDTO{}}
	if standings <= 0 {
		return dto
	}
	scores, err := a.Events.Leaderboard(ctx, e.ID, standings)
	if err != nil || len(scores) == 0 {
		return dto
	}
	ids := make([]int64, 0, len(scores))
	for _, s := range scores {
		ids = append(ids, s.PlayerID)
	}
	names := map[int64]string{}
	if a.Players != nil {
		if got, err := a.Players.DisplayNamesByID(ctx, e.GuildID, ids); err == nil {
			names = got
		}
	}
	for i, s := range scores {
		dto.Standings = append(dto.Standings, hotZoneScoreDTO{Rank: i + 1, PlayerName: names[s.PlayerID], Kills: s.Kills})
	}
	return dto
}

// handleAdminHotZones is GET .../admin/hot-zones (FEATURE_SETTINGS_VIEW): the server's recent hot
// zones with their standings.
func (a *App) handleAdminHotZones(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapFeatureSettingsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Events == nil {
		writeSaaSError(w, codeInternalError, "events unavailable")
		return
	}
	resp := struct {
		Items []hotZoneDTO `json:"items"`
	}{Items: []hotZoneDTO{}}
	if ac.scope.ServerID == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	events, err := a.Events.HotZones(ctx, ac.scope.GuildID, *ac.scope.ServerID, false, 25)
	if err != nil {
		slog.Warn("component=saas_api", "event", "hot_zones_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load hot zones")
		return
	}
	for _, e := range events {
		resp.Items = append(resp.Items, a.toHotZoneDTO(ctx, e, 3))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handlePlayerHotZone is GET /api/saas/player/servers/{installationID}/hot-zone: the open hot zone
// on the player's server (null when none), with the top ten. The same information the Discord
// announcement already made public.
func (a *App) handlePlayerHotZone(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	resp := struct {
		HotZone *hotZoneDTO `json:"hotZone"`
		// Forecast is when the server is usually busiest (docs/HOT_ZONES.md "Forecast").
		Forecast *hotZoneForecast `json:"forecast"`
	}{}
	resp.Forecast = a.hotZoneForecastFor(ctx, scope.GuildID, scope.ServerID, time.Now().UTC())
	if a.Events == nil {
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	events, err := a.Events.HotZones(ctx, scope.GuildID, scope.ServerID, true, 1)
	if err != nil {
		playerFailed(w, "hot zone", err)
		return
	}
	if len(events) > 0 && (events[0].EndsAt == nil || events[0].EndsAt.After(time.Now())) {
		dto := a.toHotZoneDTO(ctx, events[0], 10)
		resp.HotZone = &dto
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// --- feature settings API --------------------------------------------------------------------------

func (a *App) registerFeatureSettingsRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/features", a.handleGetFeatureSettings)
	h("PUT "+adminBase+"/features/hot-zones", a.handlePutHotZoneSettings)
	h("PUT "+adminBase+"/features/fight-replay", a.handlePutFightReplaySettings)
	h("PUT "+adminBase+"/features/network", a.handlePutNetworkSettings)
	h("PUT "+adminBase+"/features/feed-identity", a.handlePutFeedIdentitySettings)
	h("PUT "+adminBase+"/features/live-map", a.handlePutLiveMapSettings)
}

type hotZoneSettingsDTO struct {
	Enabled         bool `json:"enabled"`
	WindowMinutes   int  `json:"windowMinutes"`
	MinKills        int  `json:"minKills"`
	RadiusMeters    int  `json:"radiusMeters"`
	DurationMinutes int  `json:"durationMinutes"`
	CooldownMinutes int  `json:"cooldownMinutes"`
	FirstPoints     int  `json:"firstPoints"`
	SecondPoints    int  `json:"secondPoints"`
	ThirdPoints     int  `json:"thirdPoints"`
}

type fightReplaySettingsDTO struct {
	Public       bool `json:"public"`
	DelayMinutes int  `json:"delayMinutes"`
}

type networkSettingsDTO struct {
	Listed           bool   `json:"listed"`
	Description      string `json:"description"`
	DiscordInviteURL string `json:"discordInviteUrl"`
}

type feedIdentitySettingsDTO struct {
	Enabled   bool   `json:"enabled"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatarUrl"`
}

type liveMapSettingsDTO struct {
	Public       bool `json:"public"`
	DelaySeconds int  `json:"delaySeconds"`
	FactionLayer bool `json:"factionLayer"`
}

type featureSettingsDTO struct {
	HotZones     hotZoneSettingsDTO      `json:"hotZones"`
	FightReplay  fightReplaySettingsDTO  `json:"fightReplay"`
	Network      networkSettingsDTO      `json:"network"`
	FeedIdentity feedIdentitySettingsDTO `json:"feedIdentity"`
	LiveMap      liveMapSettingsDTO      `json:"liveMap"`
	UpdatedAt    *string                 `json:"updatedAt"`
}

func toFeatureSettingsDTO(s repository.FeatureSettings) featureSettingsDTO {
	hz := s.HotZones
	return featureSettingsDTO{
		HotZones: hotZoneSettingsDTO{Enabled: hz.Enabled, WindowMinutes: hz.WindowMinutes, MinKills: hz.MinKills, RadiusMeters: hz.RadiusM,
			DurationMinutes: hz.DurationMinutes, CooldownMinutes: hz.CooldownMinutes, FirstPoints: hz.FirstPoints, SecondPoints: hz.SecondPoints, ThirdPoints: hz.ThirdPoints},
		FightReplay:  fightReplaySettingsDTO{Public: s.FightReplay.Public, DelayMinutes: s.FightReplay.DelayMinutes},
		Network:      networkSettingsDTO{Listed: s.Network.Listed, Description: s.Network.Description, DiscordInviteURL: s.Network.DiscordInviteURL},
		FeedIdentity: feedIdentitySettingsDTO{Enabled: s.FeedIdentity.Enabled, Name: s.FeedIdentity.Name, AvatarURL: s.FeedIdentity.AvatarURL},
		LiveMap:      liveMapSettingsDTO{Public: s.LiveMap.Public, DelaySeconds: s.LiveMap.DelaySeconds, FactionLayer: s.LiveMap.FactionLayer},
		UpdatedAt:    nullableTimeStr(s.UpdatedAt),
	}
}

// handleGetFeatureSettings is GET .../admin/features (FEATURE_SETTINGS_VIEW).
func (a *App) handleGetFeatureSettings(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapFeatureSettingsView)
	if !ok {
		return
	}
	if a.FeatureSettings == nil {
		writeSaaSError(w, codeInternalError, "feature settings unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	s, err := a.FeatureSettings.Get(ctx, ac.scope.InstallationID)
	if err != nil {
		slog.Warn("component=saas_api", "event", "feature_settings_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load feature settings")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toFeatureSettingsDTO(s))
}

// saveFeatureSection is the shared body of the PUT routes: capability, rate limit, decode,
// plan, save (the repository validates), audit, respond with the full settings.
//
// turnsOn names the Champion-only feature the body switches on, if any. Only switching a feature
// ON needs the plan: a Survivor organization can always read its settings and switch one off, so
// nothing is left stuck after a downgrade (the same rule as custom embeds).
func saveFeatureSection[T any](a *App, w http.ResponseWriter, r *http.Request, capability permissions.Capability, action string,
	turnsOn func(body T) (entitlements.Key, bool),
	section func(repository.FeatureSettings) any, save func(ctx context.Context, installationID, userID int64, body T) (repository.FeatureSettings, error)) {
	ac, ok := a.requireCapability(w, r, capability)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	if a.FeatureSettings == nil {
		writeSaaSError(w, codeInternalError, "feature settings unavailable")
		return
	}
	body, ok := decodeJSONBody[T](w, r)
	if !ok {
		return
	}
	if turnsOn != nil {
		if key, on := turnsOn(body); on && !a.requirePlanFeature(w, r, ac.scope.OrganizationID, key) {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	before, err := a.FeatureSettings.Get(ctx, ac.scope.InstallationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load feature settings")
		return
	}
	after, err := save(ctx, ac.scope.InstallationID, ac.user.ID, body)
	if errors.Is(err, repository.ErrInvalidFeatureSettings) {
		writeSaaSError(w, codeInvalidRequest, featureSettingsMessage(err))
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "event", "feature_settings_save_failed", "action", action, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save feature settings")
		return
	}
	a.recordAudit(ctx, ac, action, "installation", "", "SUCCESS", section(before), section(after))
	a.featureSettingsChanged(ac.scope.ServerID)
	writeSaaSJSON(w, http.StatusOK, toFeatureSettingsDTO(after))
}

// featureSettingsMessage is the caller-fixable part of a validation error (never a raw DB error).
func featureSettingsMessage(err error) string {
	const prefix = "invalid feature settings: "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return "invalid feature settings"
}

// featureSettingsChanged drops caches that hold a server's settings.
func (a *App) featureSettingsChanged(serverID *int64) {
	a.invalidateNetworkCache()
	a.invalidateLiveMapCache()
	if serverID != nil && a.FeedIdentity != nil {
		a.FeedIdentity.Invalidate(*serverID)
	}
}

func (a *App) handlePutHotZoneSettings(w http.ResponseWriter, r *http.Request) {
	saveFeatureSection(a, w, r, permissions.CapFeatureSettingsManage, "HOT_ZONE_SETTINGS_UPDATED",
		func(b hotZoneSettingsDTO) (entitlements.Key, bool) { return entitlements.HotZones, b.Enabled },
		func(s repository.FeatureSettings) any { return toFeatureSettingsDTO(s).HotZones },
		func(ctx context.Context, installationID, userID int64, b hotZoneSettingsDTO) (repository.FeatureSettings, error) {
			return a.FeatureSettings.SaveHotZones(ctx, installationID, userID, repository.HotZoneSettings{Enabled: b.Enabled, WindowMinutes: b.WindowMinutes,
				MinKills: b.MinKills, RadiusM: b.RadiusMeters, DurationMinutes: b.DurationMinutes, CooldownMinutes: b.CooldownMinutes,
				FirstPoints: b.FirstPoints, SecondPoints: b.SecondPoints, ThirdPoints: b.ThirdPoints})
		})
}

func (a *App) handlePutFightReplaySettings(w http.ResponseWriter, r *http.Request) {
	saveFeatureSection(a, w, r, permissions.CapFeatureSettingsManage, "FIGHT_REPLAY_SETTINGS_UPDATED",
		func(b fightReplaySettingsDTO) (entitlements.Key, bool) { return entitlements.FightReplay, b.Public },
		func(s repository.FeatureSettings) any { return toFeatureSettingsDTO(s).FightReplay },
		func(ctx context.Context, installationID, userID int64, b fightReplaySettingsDTO) (repository.FeatureSettings, error) {
			return a.FeatureSettings.SaveFightReplay(ctx, installationID, userID, repository.FightReplaySettings{Public: b.Public, DelayMinutes: b.DelayMinutes})
		})
}

func (a *App) handlePutNetworkSettings(w http.ResponseWriter, r *http.Request) {
	saveFeatureSection(a, w, r, permissions.CapNetworkManage, "NETWORK_SETTINGS_UPDATED", nil, // the listing is part of every plan
		func(s repository.FeatureSettings) any { return toFeatureSettingsDTO(s).Network },
		func(ctx context.Context, installationID, userID int64, b networkSettingsDTO) (repository.FeatureSettings, error) {
			return a.FeatureSettings.SaveNetwork(ctx, installationID, userID, repository.NetworkSettings{Listed: b.Listed, Description: b.Description, DiscordInviteURL: b.DiscordInviteURL})
		})
}

func (a *App) handlePutFeedIdentitySettings(w http.ResponseWriter, r *http.Request) {
	saveFeatureSection(a, w, r, permissions.CapFeedIdentityManage, "FEED_IDENTITY_UPDATED",
		func(b feedIdentitySettingsDTO) (entitlements.Key, bool) { return entitlements.FeedIdentity, b.Enabled },
		func(s repository.FeatureSettings) any { return toFeatureSettingsDTO(s).FeedIdentity },
		func(ctx context.Context, installationID, userID int64, b feedIdentitySettingsDTO) (repository.FeatureSettings, error) {
			return a.FeatureSettings.SaveFeedIdentity(ctx, installationID, userID, repository.FeedIdentitySettings{Enabled: b.Enabled, Name: b.Name, AvatarURL: b.AvatarURL})
		})
}

func (a *App) handlePutLiveMapSettings(w http.ResponseWriter, r *http.Request) {
	saveFeatureSection(a, w, r, permissions.CapFeatureSettingsManage, "LIVE_MAP_SETTINGS_UPDATED", nil, // the live map is part of every plan
		func(s repository.FeatureSettings) any { return toFeatureSettingsDTO(s).LiveMap },
		func(ctx context.Context, installationID, userID int64, b liveMapSettingsDTO) (repository.FeatureSettings, error) {
			return a.FeatureSettings.SaveLiveMap(ctx, installationID, userID, repository.LiveMapSettings{Public: b.Public, DelaySeconds: b.DelaySeconds, FactionLayer: b.FactionLayer})
		})
}
