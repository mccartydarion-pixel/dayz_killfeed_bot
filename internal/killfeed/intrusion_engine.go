package killfeed

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// IntrusionStore is the DB surface the intrusion engine needs, implemented by
// internal/app's persistenceStoreAdapter over internal/repository.ZoneRepository. Split out as its
// own small interface (rather than growing LocationStore) because it is only ever used by
// IntrusionEngine, never by LocationQueue directly.
type IntrusionStore interface {
	IsIgnored(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error)
	DiscordRoleIgnoreEntries(ctx context.Context, zoneID int64) ([]string, error)
	IsAuthorized(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error)
	PlayerDiscordUserID(ctx context.Context, guildID, playerID int64) (*string, error)
	PlayerFactionID(ctx context.Context, guildID, playerID int64) (*int64, error)
	DiscordGuildID(ctx context.Context, guildID int64) (string, error)
	ActiveZoneBan(ctx context.Context, zoneID, playerID int64) (*repository.ZoneBan, error)
	GetPresence(ctx context.Context, zoneID, playerID int64) (*repository.ZonePresence, error)
	UpsertPresence(ctx context.Context, zoneID, playerID int64, status string, enteredAt *time.Time, lastSeenAt time.Time, lastLocationEventID int64, lastAlertAt *time.Time) error
	GetOpenIntrusion(ctx context.Context, zoneID, playerID int64) (*repository.ZoneIntrusion, error)
	CreateIntrusion(ctx context.Context, zoneID, installationID, guildID, serverID, playerID int64, gamertag string, banned bool, enteredAt time.Time, alerted bool) (*repository.ZoneIntrusion, error)
	MarkExited(ctx context.Context, intrusionID int64, exitedAt time.Time) error
}

// RoleChecker resolves a live Discord role membership check for DISCORD_ROLE zone-ignore entries
// only. internal/killfeed never imports internal/discord (existing layering, matching how this
// package has no Discord dependency anywhere else) - a small internal/app adapter satisfies this.
type RoleChecker interface {
	MemberHasRole(discordGuildID, discordUserID, roleID string) (bool, error)
}

// IntrusionAlertKind is the operational event vocabulary (task section 18).
type IntrusionAlertKind string

const (
	AlertZoneIntrusion      IntrusionAlertKind = "ZONE_INTRUSION"
	AlertZoneExit           IntrusionAlertKind = "ZONE_EXIT"
	AlertUAVIntrusion       IntrusionAlertKind = "UAV_INTRUSION"
	AlertBaseRadarIntrusion IntrusionAlertKind = "BASE_RADAR_INTRUSION"
	AlertZoneBanViolation   IntrusionAlertKind = "ZONE_BAN_VIOLATION"
)

// IntrusionEvent is one operational event the engine emits - every transition it decides is
// noteworthy, whether or not a Discord alert was actually sent for it (Suppressed distinguishes
// the two). internal/app's IntrusionPublisher decides what, if anything, to do with each kind:
// only entry-type kinds with Suppressed=false and the zone's AlertChannelID set are ever posted to
// Discord; every kind is always available for future Live Ops/metrics consumption regardless.
type IntrusionEvent struct {
	Kind        IntrusionAlertKind
	Zone        repository.Zone
	PlayerID    int64
	Gamertag    string
	IntrusionID int64
	At          time.Time
	Suppressed  bool
}

// IntrusionPublisher is the consumer for intrusion engine events. Like HitPublisher/
// ConnectionPublisher, this runs off the hot path already (LocationQueue's own consumer goroutine)
// but implementations must still not block indefinitely - a slow Discord call should have its own
// timeout, never left to stall the location queue.
type IntrusionPublisher interface {
	PublishIntrusionEvent(ev IntrusionEvent)
}

// IntrusionMetricsSnapshot is IntrusionEngine.Metrics()'s point-in-time counters (task section 28).
// zone_active_intrusions is deliberately NOT tracked here - it is a live gauge, computed at read
// time from zone_intrusions/zone_presence by the API layer, never an incrementally-maintained
// counter that could drift from the persisted truth.
type IntrusionMetricsSnapshot struct {
	LocationEventsEvaluated int64
	Entries                 int64
	Exits                   int64
	IntrusionsCreated       int64
	// IntrusionsSuppressed counts geometric entries that were never tracked at all because the
	// entity matched a zone_ignore_entries row (PLAYER/FACTION/DISCORD_ROLE) - distinct from an
	// alert being suppressed (below), since an ignored entity gets no presence row, no intrusion
	// row, and no alert whatsoever.
	IntrusionsSuppressed int64
	AlertsSent           int64
	// AlertsSuppressedCooldown counts entries that WERE tracked as a real intrusion but did not
	// alert because the zone's cooldown had not yet elapsed since this zone+player pair's last
	// alert (task section 15). An authorized-entity or no-configured-channel suppression is not
	// counted here (IntrusionsCreated - AlertsSent - AlertsSuppressedCooldown covers those).
	AlertsSuppressedCooldown int64
	EvaluationLatencyMs      int64 // cumulative; caller derives an average against LocationEventsEvaluated
}

// IntrusionEngine evaluates freshly-persisted location events against a server's cached zones,
// maintaining zone_presence/zone_intrusions in Postgres - never in memory. That single design
// choice is what makes restart/recovery safety (task section 24) automatic rather than a separate
// mechanism to build: the first post-restart location event for an already-inside player finds
// presence.status='INSIDE' already persisted, so it is evaluated as an ongoing presence (a refresh
// of last_seen_at), never as a fresh OUTSIDE->INSIDE transition, so no fake entry alert is ever
// possible.
//
// Evaluate is always called from LocationQueue's own single consumer goroutine, strictly after
// that batch's events were durably persisted, and never concurrently for the same server - so a
// server's own zone_presence/zone_intrusions rows are only ever touched by one goroutine at a
// time, with no additional in-process locking needed beyond Postgres's own row-level guarantees.
type IntrusionEngine struct {
	store     IntrusionStore
	cache     *ZoneCache
	roles     RoleChecker
	publisher IntrusionPublisher

	metricsMu sync.Mutex
	metrics   IntrusionMetricsSnapshot
}

func NewIntrusionEngine(store IntrusionStore, cache *ZoneCache, roles RoleChecker, publisher IntrusionPublisher) *IntrusionEngine {
	return &IntrusionEngine{store: store, cache: cache, roles: roles, publisher: publisher}
}

// Metrics returns a point-in-time copy of the engine's cumulative counters.
func (e *IntrusionEngine) Metrics() IntrusionMetricsSnapshot {
	if e == nil {
		return IntrusionMetricsSnapshot{}
	}
	e.metricsMu.Lock()
	defer e.metricsMu.Unlock()
	return e.metrics
}

// locationRecord is what LocationQueue.persist hands the engine per resolved event - the identity
// resolution (UpsertPlayer) already happened, so this carries no raw ADM fields, only what the
// intrusion engine needs.
type locationRecord struct {
	GuildID, ServerID, PlayerID int64
	Gamertag                    string
	X, Z                        float64
	ObservedAt                  time.Time
	LocationEventID             int64
}

// Evaluate processes one batch of location records for a single server against that server's
// cached zones. Duplicate-event safety (task section 21) is inherited from Phase 3's own
// UNIQUE(player_id,server_id,event_type,observed_at) dedupe backstop - LocationQueue only ever
// calls Evaluate with records it just inserted or attempted to insert in this call, and a replayed
// duplicate resolves to the exact same (zone, player, distance) outcome either way, so re-evaluating
// it is always idempotent (a still-INSIDE update, never a spurious re-entry).
func (e *IntrusionEngine) Evaluate(ctx context.Context, serverID int64, records []locationRecord) {
	if e == nil || e.store == nil || len(records) == 0 {
		return
	}
	start := time.Now()
	zones, err := e.cache.Zones(ctx, serverID)
	if err != nil {
		slog.Warn("component=intrusion_engine", "msg", "zone cache lookup failed", "server_id", serverID, "err", err.Error())
		return
	}
	if len(zones) == 0 {
		return
	}
	for _, rec := range records {
		for _, zone := range zones {
			if !zone.Enabled {
				continue
			}
			e.evaluateOne(ctx, zone, rec)
		}
	}
	e.metricsMu.Lock()
	e.metrics.EvaluationLatencyMs += time.Since(start).Milliseconds()
	e.metricsMu.Unlock()
}

// evaluateOne is the distance = sqrt((playerX-zoneX)^2 + (playerZ-zoneZ)^2) check plus the
// OUTSIDE/INSIDE state machine for one (zone, location record) pair (task section 6/7).
func (e *IntrusionEngine) evaluateOne(ctx context.Context, zone repository.Zone, rec locationRecord) {
	e.metricsMu.Lock()
	e.metrics.LocationEventsEvaluated++
	e.metricsMu.Unlock()

	dx := rec.X - zone.CenterX
	dz := rec.Z - zone.CenterZ
	inside := math.Sqrt(dx*dx+dz*dz) <= zone.Radius

	presence, err := e.store.GetPresence(ctx, zone.ID, rec.PlayerID)
	if err != nil {
		slog.Warn("component=intrusion_engine", "msg", "presence lookup failed", "zone_id", zone.ID, "err", err.Error())
		return
	}
	wasInside := presence != nil && presence.Status == repository.PresenceInside

	if !inside {
		if wasInside {
			e.handleExit(ctx, zone, rec)
		}
		return
	}
	if wasInside {
		// Ongoing presence: refresh last_seen_at/last_location_event_id only - never re-alerts,
		// never creates a second intrusion (task: "must NOT alert on every location update").
		if err := e.store.UpsertPresence(ctx, zone.ID, rec.PlayerID, repository.PresenceInside, presence.EnteredAt, rec.ObservedAt, rec.LocationEventID, nil); err != nil {
			slog.Warn("component=intrusion_engine", "msg", "presence refresh failed", "zone_id", zone.ID, "err", err.Error())
		}
		return
	}
	e.handleEntry(ctx, zone, rec, presence)
}

// handleExit closes the open intrusion (if any) for a genuine INSIDE->OUTSIDE transition. Task
// section 19's presence-uncertainty rule means this is the ONLY path that ever marks an intrusion
// EXITED - a stale/missing subsequent location update never fabricates one.
func (e *IntrusionEngine) handleExit(ctx context.Context, zone repository.Zone, rec locationRecord) {
	now := rec.ObservedAt
	if err := e.store.UpsertPresence(ctx, zone.ID, rec.PlayerID, repository.PresenceOutside, nil, now, rec.LocationEventID, nil); err != nil {
		slog.Warn("component=intrusion_engine", "msg", "presence exit write failed", "zone_id", zone.ID, "err", err.Error())
		return
	}
	intrusion, err := e.store.GetOpenIntrusion(ctx, zone.ID, rec.PlayerID)
	if err != nil || intrusion == nil {
		return
	}
	if err := e.store.MarkExited(ctx, intrusion.ID, now); err != nil {
		slog.Warn("component=intrusion_engine", "msg", "mark exited failed", "intrusion_id", intrusion.ID, "err", err.Error())
		return
	}
	e.metricsMu.Lock()
	e.metrics.Exits++
	e.metricsMu.Unlock()
	if e.publisher != nil {
		e.publisher.PublishIntrusionEvent(IntrusionEvent{Kind: AlertZoneExit, Zone: zone, PlayerID: rec.PlayerID, Gamertag: rec.Gamertag, IntrusionID: intrusion.ID, At: now, Suppressed: true})
	}
}

// handleEntry is the OUTSIDE->INSIDE (or first-ever observation) path: ignore-list check first (an
// ignored entity is never tracked at all), then ban/authorization/cooldown, then a new intrusion
// row and presence write, then an operational event.
func (e *IntrusionEngine) handleEntry(ctx context.Context, zone repository.Zone, rec locationRecord, presence *repository.ZonePresence) {
	ignored, err := e.checkIgnored(ctx, zone, rec)
	if err != nil {
		slog.Warn("component=intrusion_engine", "msg", "ignore check failed", "zone_id", zone.ID, "err", err.Error())
	}
	if ignored {
		e.metricsMu.Lock()
		e.metrics.IntrusionsSuppressed++
		e.metricsMu.Unlock()
		return
	}

	now := rec.ObservedAt
	ban, err := e.store.ActiveZoneBan(ctx, zone.ID, rec.PlayerID)
	if err != nil {
		slog.Warn("component=intrusion_engine", "msg", "ban check failed", "zone_id", zone.ID, "err", err.Error())
	}
	banned := ban != nil

	// Authorization only ever suppresses alerting for UAV/Base Radar zones (task section 12) - it
	// never applies to any other zone type, and never bypasses a ban.
	authorized := false
	if !banned && repository.IsUAVOrRadar(zone.ZoneType) {
		factionID, _ := e.store.PlayerFactionID(ctx, rec.GuildID, rec.PlayerID)
		if a, err := e.store.IsAuthorized(ctx, zone.ID, rec.PlayerID, factionID); err == nil {
			authorized = a
		}
	}

	var lastAlertAt *time.Time
	if presence != nil {
		lastAlertAt = presence.LastAlertAt
	}
	cooldown := time.Duration(zone.CooldownSeconds) * time.Second
	withinCooldown := lastAlertAt != nil && now.Sub(*lastAlertAt) < cooldown
	shouldAlert := !authorized && !withinCooldown && zone.AlertChannelID != nil

	intrusion, err := e.store.CreateIntrusion(ctx, zone.ID, zone.InstallationID, zone.GuildID, zone.ServerID, rec.PlayerID, rec.Gamertag, banned, now, shouldAlert)
	if err != nil {
		slog.Warn("component=intrusion_engine", "msg", "create intrusion failed", "zone_id", zone.ID, "err", err.Error())
		return
	}
	var newAlertAt *time.Time
	if shouldAlert {
		newAlertAt = &now
	}
	if err := e.store.UpsertPresence(ctx, zone.ID, rec.PlayerID, repository.PresenceInside, &now, now, rec.LocationEventID, newAlertAt); err != nil {
		slog.Warn("component=intrusion_engine", "msg", "presence entry write failed", "zone_id", zone.ID, "err", err.Error())
	}

	e.metricsMu.Lock()
	e.metrics.Entries++
	e.metrics.IntrusionsCreated++
	if shouldAlert {
		e.metrics.AlertsSent++
	} else if withinCooldown {
		e.metrics.AlertsSuppressedCooldown++
	}
	e.metricsMu.Unlock()

	if e.publisher == nil {
		return
	}
	kind := AlertZoneIntrusion
	switch zone.ZoneType {
	case repository.ZoneTypeUAV:
		kind = AlertUAVIntrusion
	case repository.ZoneTypeBaseRadar:
		kind = AlertBaseRadarIntrusion
	}
	e.publisher.PublishIntrusionEvent(IntrusionEvent{Kind: kind, Zone: zone, PlayerID: rec.PlayerID, Gamertag: rec.Gamertag, IntrusionID: intrusion.ID, At: now, Suppressed: !shouldAlert})
	if banned {
		e.publisher.PublishIntrusionEvent(IntrusionEvent{Kind: AlertZoneBanViolation, Zone: zone, PlayerID: rec.PlayerID, Gamertag: rec.Gamertag, IntrusionID: intrusion.ID, At: now, Suppressed: !shouldAlert})
	}
}

// checkIgnored evaluates zone_ignore_entries: PLAYER/FACTION via a direct DB check, DISCORD_ROLE
// via a live role lookup (only performed when the zone actually has a DISCORD_ROLE entry and a
// RoleChecker is configured - the common case of no such entries costs nothing extra). Any lookup
// failure fails OPEN (ignored=false): suppressing a genuine intrusion because a role check errored
// would be a silent false-negative safety gap; a spurious alert is the safer failure direction.
func (e *IntrusionEngine) checkIgnored(ctx context.Context, zone repository.Zone, rec locationRecord) (bool, error) {
	factionID, _ := e.store.PlayerFactionID(ctx, rec.GuildID, rec.PlayerID)
	ignored, err := e.store.IsIgnored(ctx, zone.ID, rec.PlayerID, factionID)
	if err != nil {
		return false, err
	}
	if ignored {
		return true, nil
	}
	if e.roles == nil {
		return false, nil
	}
	roleIDs, err := e.store.DiscordRoleIgnoreEntries(ctx, zone.ID)
	if err != nil || len(roleIDs) == 0 {
		return false, err
	}
	discordUserID, err := e.store.PlayerDiscordUserID(ctx, rec.GuildID, rec.PlayerID)
	if err != nil || discordUserID == nil {
		return false, err
	}
	discordGuildID, err := e.store.DiscordGuildID(ctx, rec.GuildID)
	if err != nil || discordGuildID == "" {
		return false, err
	}
	for _, roleID := range roleIDs {
		has, err := e.roles.MemberHasRole(discordGuildID, *discordUserID, roleID)
		if err != nil {
			continue
		}
		if has {
			return true, nil
		}
	}
	return false, nil
}
