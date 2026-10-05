package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/analytics"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/discord"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/factionstats"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// persistenceStoreAdapter adapts the repositories to the killfeed.PersistenceStore
// interface used by the persistence queue worker.
type persistenceStoreAdapter struct {
	players   *repository.PlayerRepository
	kills     *repository.KillRepository
	deaths    *repository.DeathRepository
	seasons   *repository.SeasonRepository
	ranked    *repository.RankedRepository
	factions  *repository.FactionRepository
	vip       *repository.VIPRepository
	wars      *repository.PostgresWarRepository
	events    *repository.EventRepository
	bounties  *repository.BountyRepository
	bountySvc *bounties.Service
	streaks   *repository.StreakRepository
	anomalies *repository.AnomalyRepository
	activity  *repository.ActivityRepository
	servers   *repository.ServerRepository
	stats     *repository.StatsRepository
	analytics *repository.AnalyticsRepository
	// locations backs killfeed.LocationStore (Phase 3, docs/PLAYER_INTELLIGENCE.md) - nil-safe
	// (InsertLocationEvents/UpsertPlayer below no-op if unset, matching this adapter's existing
	// defensive-nil style for every other optional dependency).
	locations *repository.LocationRepository
	// zones backs killfeed.IntrusionStore/killfeed.ZoneSource (Phase 4, docs/ZONES_UAV_RADAR.md) -
	// nil-safe throughout, matching locations above.
	zones      *repository.ZoneRepository
	panelDirty func()
	// rankedTags reports whether the server shows ranked RP on kill cards (an automation); nil = never.
	rankedTags func(ctx context.Context, serverID int64) bool
	// factionStats is told about every persisted kill and death (nil-safe): it invalidates cached
	// faction figures and queues the killer for achievement evaluation. It never blocks the kill path.
	factionStats *factionstats.Service
	// lives closes a player's life when their death is persisted (docs/LIVES.md); lifeRecap then
	// queues the opt-in recap DM. Both nil-safe.
	lives     *repository.LifeRepository
	lifeRecap *discord.LifeRecapNotifier
}

type admCheckpointStoreAdapter struct {
	repo *repository.CheckpointRepository
}

func (s *admCheckpointStoreAdapter) LoadADMCheckpoint(ctx context.Context, guildID, serverID int64) (*killfeed.DurableCheckpoint, error) {
	checkpoint, err := s.repo.LoadADMCheckpoint(ctx, guildID, serverID)
	if err != nil || checkpoint == nil {
		return nil, err
	}
	return &killfeed.DurableCheckpoint{Filename: checkpoint.Filename, RemoteModifiedAt: checkpoint.RemoteModifiedAt, RemoteSize: checkpoint.RemoteSize, ProcessedOffset: checkpoint.ProcessedOffset, PendingPartialLine: checkpoint.PendingPartialLine}, nil
}

func (s *admCheckpointStoreAdapter) SaveADMCheckpoint(ctx context.Context, guildID, serverID int64, sessionID string, checkpoint killfeed.DurableCheckpoint) error {
	return s.repo.SaveADMCheckpoint(ctx, repository.ADMCheckpoint{ServerID: serverID, SessionID: sessionID, Filename: checkpoint.Filename, RemoteModifiedAt: checkpoint.RemoteModifiedAt, RemoteSize: checkpoint.RemoteSize, ProcessedOffset: checkpoint.ProcessedOffset, PendingPartialLine: checkpoint.PendingPartialLine}, guildID)
}

func (p *persistenceStoreAdapter) RecordConnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.Connect(ctx, guildID, serverID, playerID, at)
}

func (p *persistenceStoreAdapter) RecordDisconnect(ctx context.Context, guildID, serverID, playerID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.Disconnect(ctx, guildID, serverID, playerID, at)
}

func (p *persistenceStoreAdapter) CheckpointConnected(ctx context.Context, guildID, serverID int64, at time.Time) error {
	if p.activity == nil || serverID == 0 {
		return nil
	}
	return p.activity.CheckpointConnected(ctx, guildID, serverID, at)
}

func (p *persistenceStoreAdapter) UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error) {
	return p.players.UpsertPlayer(ctx, guildID, dayzID, displayName, seenAt)
}

// InsertLocationEvents satisfies killfeed.LocationStore (Phase 3, docs/PLAYER_INTELLIGENCE.md).
// A nil locations repository (Phase 3 not wired up) reports success with nothing written, rather
// than erroring the location queue's batch on every flush.
func (p *persistenceStoreAdapter) InsertLocationEvents(ctx context.Context, events []repository.LocationEventInput) (int, error) {
	if p.locations == nil {
		return 0, nil
	}
	return p.locations.InsertLocationEvents(ctx, events)
}

// --- killfeed.ZoneSource / killfeed.IntrusionStore (Phase 4, docs/ZONES_UAV_RADAR.md) ----------
// Every method here is a thin, nil-safe delegate to p.zones - a nil zones repository (Phase 4 not
// wired up, or a test harness that never configured it) reports "no zones"/"not ignored"/"not
// authorized"/"not banned" rather than erroring the intrusion engine on every evaluation.

func (p *persistenceStoreAdapter) ActiveZonesForServer(ctx context.Context, serverID int64) ([]repository.Zone, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.ActiveZonesForServer(ctx, serverID)
}

func (p *persistenceStoreAdapter) IsIgnored(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	if p.zones == nil {
		return false, nil
	}
	return p.zones.IsIgnored(ctx, zoneID, playerID, factionID)
}

func (p *persistenceStoreAdapter) DiscordRoleIgnoreEntries(ctx context.Context, zoneID int64) ([]string, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.DiscordRoleIgnoreEntries(ctx, zoneID)
}

func (p *persistenceStoreAdapter) IsAuthorized(ctx context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	if p.zones == nil {
		return false, nil
	}
	return p.zones.IsAuthorized(ctx, zoneID, playerID, factionID)
}

func (p *persistenceStoreAdapter) PlayerDiscordUserID(ctx context.Context, guildID, playerID int64) (*string, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.PlayerDiscordUserID(ctx, guildID, playerID)
}

func (p *persistenceStoreAdapter) PlayerFactionID(ctx context.Context, guildID, playerID int64) (*int64, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.PlayerFactionID(ctx, guildID, playerID)
}

func (p *persistenceStoreAdapter) DiscordGuildID(ctx context.Context, guildID int64) (string, error) {
	if p.zones == nil {
		return "", nil
	}
	return p.zones.DiscordGuildID(ctx, guildID)
}

func (p *persistenceStoreAdapter) ActiveZoneBan(ctx context.Context, zoneID, playerID int64) (*repository.ZoneBan, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.ActiveZoneBan(ctx, zoneID, playerID)
}

func (p *persistenceStoreAdapter) GetPresence(ctx context.Context, zoneID, playerID int64) (*repository.ZonePresence, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.GetPresence(ctx, zoneID, playerID)
}

func (p *persistenceStoreAdapter) UpsertPresence(ctx context.Context, zoneID, playerID int64, status string, enteredAt *time.Time, lastSeenAt time.Time, lastLocationEventID int64, lastAlertAt *time.Time) error {
	if p.zones == nil {
		return nil
	}
	return p.zones.UpsertPresence(ctx, zoneID, playerID, status, enteredAt, lastSeenAt, lastLocationEventID, lastAlertAt)
}

func (p *persistenceStoreAdapter) GetOpenIntrusion(ctx context.Context, zoneID, playerID int64) (*repository.ZoneIntrusion, error) {
	if p.zones == nil {
		return nil, nil
	}
	return p.zones.GetOpenIntrusion(ctx, zoneID, playerID)
}

func (p *persistenceStoreAdapter) CreateIntrusion(ctx context.Context, zoneID, installationID, guildID, serverID, playerID int64, gamertag string, banned bool, enteredAt time.Time, alerted bool) (*repository.ZoneIntrusion, error) {
	if p.zones == nil {
		return nil, errors.New("zone repository unavailable")
	}
	return p.zones.CreateIntrusion(ctx, zoneID, installationID, guildID, serverID, playerID, gamertag, banned, enteredAt, alerted)
}

func (p *persistenceStoreAdapter) MarkExited(ctx context.Context, intrusionID int64, exitedAt time.Time) error {
	if p.zones == nil {
		return nil
	}
	return p.zones.MarkExited(ctx, intrusionID, exitedAt)
}

func (p *persistenceStoreAdapter) InsertKill(ctx context.Context, k repository.KillRecord) error {
	return p.kills.InsertKill(ctx, k)
}

func (p *persistenceStoreAdapter) InsertKillReturning(ctx context.Context, k repository.KillRecord) (int64, error) {
	return p.kills.InsertKillReturning(ctx, k)
}

func (p *persistenceStoreAdapter) InsertDeath(ctx context.Context, d repository.DeathRecord) error {
	return p.deaths.InsertDeath(ctx, d)
}

func (p *persistenceStoreAdapter) ResolveDeathSeason(ctx context.Context, guildID int64, at time.Time) *int64 {
	if p.seasons == nil {
		return nil
	}
	s, err := p.seasons.ResolveAt(ctx, guildID, at)
	if err != nil || s == nil {
		return nil
	}
	return &s.ID
}

func (p *persistenceStoreAdapter) ResolveKillAttribution(ctx context.Context, guildID, killerID, victimID int64, at time.Time) (killerFactionID, victimFactionID, seasonID, warID *int64) {
	if p.seasons != nil {
		if s, err := p.seasons.ResolveAt(ctx, guildID, at); err == nil && s != nil {
			seasonID = &s.ID
		}
	}
	if p.factions == nil {
		return
	}
	k, kErr := p.factions.GetActiveFactionForPlayer(ctx, guildID, killerID)
	v, vErr := p.factions.GetActiveFactionForPlayer(ctx, guildID, victimID)
	if kErr == nil {
		killerFactionID = &k.FactionID
	}
	if vErr == nil {
		victimFactionID = &v.FactionID
	}
	if killerFactionID != nil && victimFactionID != nil && *killerFactionID != *victimFactionID && p.wars != nil {
		if w, err := p.wars.GetActivePair(ctx, guildID, *killerFactionID, *victimFactionID); err == nil && w != nil {
			warID = &w.ID
		}
	}
	return
}

// ResolveStreakContext reads the killer/victim current_streak values exactly
// as they stand right now - i.e. before this kill's eventual Increment/Reset
// below in ProcessPersistedKill - so killfeed.PersistenceQueue can classify
// and persist KILLING_SPREE/STREAK_ENDED on the kill row itself, before
// combat stats are mutated. A player with no player_combat_stats row yet
// (first ever kill/death) has an implicit streak of 0, not an error.
func (p *persistenceStoreAdapter) ResolveStreakContext(ctx context.Context, guildID, killerPlayerID, victimPlayerID int64) (killerStreakBefore, victimStreakBefore int) {
	if p.streaks == nil {
		return 0, 0
	}
	if killerPlayerID > 0 {
		if s, err := p.streaks.Get(ctx, guildID, killerPlayerID); err == nil {
			killerStreakBefore = s.Current
		}
	}
	if victimPlayerID > 0 {
		if s, err := p.streaks.Get(ctx, guildID, victimPlayerID); err == nil {
			victimStreakBefore = s.Current
		}
	}
	return killerStreakBefore, victimStreakBefore
}

func (p *persistenceStoreAdapter) ProcessPersistedKill(ctx context.Context, killID int64, record repository.KillRecord, ev *killfeed.Event) {
	if p.ranked != nil && record.ServerID > 0 {
		award, err := p.ranked.AwardActiveServerKill(ctx, record.ServerID, killID)
		if err != nil && !errors.Is(err, repository.ErrRankedIneligible) {
			slog.Warn("component=ranked", "event", "award_failed_retry_scheduled", "server_id", record.ServerID, "kill_id", killID, "err", err.Error())
		}
		if err == nil && ev != nil && p.rankedTags != nil && p.rankedTags(ctx, record.ServerID) {
			ev.RankedTag = rankedTag(award)
		}
	}
	// Runs on every exit (including the bounty claim at the end): the kill is durable, so cached
	// faction figures are stale and the killer's factions may have earned an achievement.
	defer p.factionStats.NotifyCombat(record.GuildID, record.ServerID, record.KillerPlayerID)
	// Before the streak early-returns below: the victim's life ends with this kill regardless.
	p.recordLifeEndFromKill(ctx, killID, record, ev)
	if p.streaks == nil {
		return
	}
	streak, err := p.streaks.Increment(ctx, record.GuildID, record.KillerPlayerID)
	if err != nil {
		slog.Warn("component=killfeed", "msg", "streak processing failed", "err", err.Error())
		return
	}
	if record.VictimPlayerID > 0 {
		_ = p.streaks.Reset(ctx, record.GuildID, record.VictimPlayerID)
	}
	at := time.Now().UTC()
	if ev != nil && !ev.Timestamp.IsZero() {
		at = ev.Timestamp.UTC()
	}

	// Stat-rich embed fields: best-effort, never block the kill from
	// publishing. A guild without stats/analytics wired just gets an embed
	// without these sections (nil-checked in BuildKillEmbed).
	if ev != nil {
		// Prefer the persisted, authoritative streak-after snapshot (computed
		// before this kill's own Increment call above, and durably stored on
		// the kill row) over the value Increment just returned - they agree in
		// the normal case, but the persisted field is what the website and any
		// later re-render must treat as the source of truth.
		streakCurrent := streak.Current
		if record.KillerStreakAfter != nil {
			streakCurrent = *record.KillerStreakAfter
		}
		ev.KillerStreak = &streakCurrent
		ev.KillingSpree = record.KillingSpree
		ev.StreakEnded = record.StreakEnded
		ev.EndedStreakCount = record.EndedStreakCount
		if p.stats != nil {
			if prof, statErr := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.KillerPlayerID); statErr == nil && prof != nil {
				ev.KillerStats = combatRecordOf(prof)
			}
			if record.VictimPlayerID > 0 {
				if prof, statErr := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.VictimPlayerID); statErr == nil && prof != nil {
					ev.VictimStats = combatRecordOf(prof)
				}
			}
		}
		if p.analytics != nil && ev.Killer != nil && ev.Victim != nil && ev.Killer.Name != "" && ev.Victim.Name != "" && ev.Killer.Name != ev.Victim.Name {
			if m, matchupErr := p.analytics.Matchup(ctx, record.GuildID, ev.Killer.Name, ev.Victim.Name, analytics.ScopeLifetime, 0); matchupErr == nil && m != nil {
				ev.Encounters = &killfeed.HeadToHead{KillerWins: m.AKills, VictimWins: m.BKills}
			}
		}
	}
	if p.anomalies != nil && record.KillerPlayerID > 0 && record.VictimPlayerID > 0 && record.KillerPlayerID != record.VictimPlayerID {
		if suspicious, anomalyErr := p.anomalies.ObservePair(ctx, record.GuildID, valueOfID(record.SeasonID), record.KillerPlayerID, record.VictimPlayerID, at); anomalyErr == nil && suspicious {
			slog.Debug("component=anti-farming", "msg", "repeated pair activity observed", "killer_player_id", record.KillerPlayerID, "victim_player_id", record.VictimPlayerID)
		}
	}

	if p.events != nil {
		var eventBadges []string
		if active, listErr := p.events.GetActiveEvents(ctx, record.GuildID); listErr == nil {
			for _, stored := range active {
				competitive := competitiveevents.Event{ID: stored.ID, GuildID: stored.GuildID, SeasonID: stored.SeasonID, Type: stored.Type, Name: stored.Name, Description: stored.Description, Status: stored.Status, StartsAt: stored.StartsAt, EndsAt: stored.EndsAt, Config: stored.Config}
				input := competitiveevents.KillInput{KillID: killID, KillerPlayerID: record.KillerPlayerID, VictimPlayerID: record.VictimPlayerID, KillerFactionID: record.KillerFactionID, VictimFactionID: record.VictimFactionID, WarID: record.WarID, WeaponDisplay: record.WeaponDisplay, Distance: record.Distance, Headshot: record.Headshot, Streak: streak.Current, EventTime: at, ServerID: record.ServerID}
				if ev != nil {
					input.KillerPos, input.VictimPos = killPoint(ev.Killer), killPoint(ev.Victim)
				}
				if score := competitiveevents.Qualify(competitive, input); score.Qualifies {
					if len(eventBadges) < 2 {
						eventBadges = append(eventBadges, "🔥 "+stored.Name)
					}
					if _, scoreErr := p.events.ScoreKill(ctx, stored.ID, killID, score.PlayerID, score.FactionID, score.Points, score.BestDistance, score.BestStreak); scoreErr != nil {
						slog.Warn("component=events", "msg", "event scoring failed", "event_id", stored.ID, "err", scoreErr.Error())
					}
				}
			}
		}
		if ev != nil {
			ev.ActiveEventBadges = eventBadges
		}
	}
	if p.vip != nil && ev != nil && record.KillerPlayerID > 0 && record.KillerPlayerID != record.VictimPlayerID {
		if badge, vipErr := p.vip.ActiveBadge(ctx, record.GuildID, record.KillerPlayerID); vipErr == nil {
			ev.SupporterBadge = badge
		}
	}
	if ev != nil && record.WarID != nil {
		ev.WarBadge = "⚔️ FACTION WAR"
	}

	// Bounty claim. This runs only for a durably persisted, non-duplicate PvP kill
	// (ProcessPersistedKill is the persisted-kill hook), never from a raw ADM line.
	// Suicides, environment and ambiguous deaths are never kills and never reach
	// here; self-kills and same-faction (team) kills do not claim.
	if p.bountySvc != nil && record.VictimPlayerID > 0 && record.KillerPlayerID > 0 && record.KillerPlayerID != record.VictimPlayerID && (record.KillerFactionID == nil || record.VictimFactionID == nil || *record.KillerFactionID != *record.VictimFactionID) {
		in := bounties.KillInput{
			GuildID: record.GuildID, ServerID: record.ServerID,
			VictimPlayerID: record.VictimPlayerID, KillerPlayerID: record.KillerPlayerID,
			KillID: killID, SeasonID: valueOfID(record.SeasonID), At: at,
			Weapon: record.WeaponDisplay, Distance: record.Distance,
		}
		if ev != nil {
			if ev.Killer != nil {
				in.HunterName = ev.Killer.Name
			}
			if ev.Victim != nil {
				in.TargetName = ev.Victim.Name
			}
		}
		// Every eligible active bounty on the victim (this server's plus guild-wide)
		// is claimed atomically in one transaction; the notification runs after the
		// commit and cannot undo it.
		result, claimErr := p.bountySvc.ClaimForKill(ctx, in)
		if claimErr != nil {
			slog.Warn("component=bounty", "msg", "bounty claim failed", "err", claimErr.Error())
		} else if result.Count > 0 && ev != nil {
			ev.BountyClaimed = true
			ev.BountyPoints = result.Total
		}
	}
	if p.bountySvc != nil {
		killerName := ""
		if ev != nil && ev.Killer != nil {
			killerName = ev.Killer.Name
		}
		p.bountySvc.NoteStreak(ctx, bounties.StreakInput{GuildID: record.GuildID, ServerID: record.ServerID, PlayerID: record.KillerPlayerID, SeasonID: valueOfID(record.SeasonID), Streak: streak.Current, At: at, PlayerName: killerName})
		if p.bounties != nil && ev != nil && record.KillerPlayerID > 0 {
			if wanted, wantedErr := p.bounties.HasActiveAt(ctx, record.GuildID, record.ServerID, record.KillerPlayerID, at); wantedErr == nil && wanted {
				ev.BountyTarget = true
			}
		}
	}
	if p.panelDirty != nil {
		p.panelDirty()
	}
}

// ProcessPersistedDeath fetches the deceased player's stats for the death
// embed. Best-effort: on failure ev.PlayerStats stays nil and the embed
// renders without that section (nil-checked in BuildDeathEmbed).
func (p *persistenceStoreAdapter) ProcessPersistedDeath(ctx context.Context, record repository.DeathRecord, ev *killfeed.Event) {
	defer p.factionStats.NotifyCombat(record.GuildID, record.ServerID, 0) // a death changes deaths/K-D/streaks only
	p.recordLifeEnd(ctx, record, ev)
	if p.stats == nil || ev == nil || record.PlayerID == 0 {
		return
	}
	prof, err := p.stats.GetPlayerProfileByPlayerID(ctx, record.GuildID, record.PlayerID)
	if err != nil || prof == nil {
		return
	}
	ev.PlayerStats = combatRecordOf(prof)
}

// combatRecordOf is a profile as the kill and death cards carry it, with the deaths split into
// PvP and PvE (internal/deathstats).
func combatRecordOf(prof *repository.PlayerProfile) *killfeed.CombatRecord {
	return &killfeed.CombatRecord{Kills: prof.Kills, Deaths: prof.Deaths, PvPDeaths: prof.PvPDeaths, DeathSplitKnown: true}
}

func valueOfID(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
