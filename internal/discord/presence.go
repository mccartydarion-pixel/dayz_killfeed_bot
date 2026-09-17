package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/health"
)

// PresenceMode selects how PresenceManager chooses what to display.
type PresenceMode string

const (
	PresenceModeStatic  PresenceMode = "static"
	PresenceModeDynamic PresenceMode = "dynamic"
)

// presenceHealthPollInterval bounds how often the presence manager samples
// the application's already-computed overall health (see
// PresenceHealthProvider) - independent of, and typically far more frequent
// than, the display rotation interval.
const presenceHealthPollInterval = 10 * time.Second

// presenceHealthDebounce is the minimum time between two applied Discord
// status changes driven by health, so a temporary API blip does not flap the
// bot's status back and forth.
const presenceHealthDebounce = 60 * time.Second

// PresenceUpdater is the minimal Discord surface the presence manager needs,
// so unit tests never require a live gateway connection. *discord.Client
// satisfies this directly.
type PresenceUpdater interface {
	UpdatePresence(status string, activityType discordgo.ActivityType, text string) error
}

// PresenceStatsProvider exposes already-known application state for display.
// Implementations must never trigger a new Nitrado poll or other network
// call - only report state the application already tracks. ok is false when
// nothing reliable is available yet (e.g. no server connected), in which
// case the manager skips any activity that would need these values rather
// than showing a fabricated 0.
type PresenceStatsProvider interface {
	PresenceCounts() (totalPlayers, onlineServers, configuredServers int, ok bool)
}

// PresenceHealthProvider exposes the application's already-computed overall
// health (see internal/health.Registry), so presence can reflect it without
// running its own duplicate health checks.
type PresenceHealthProvider interface {
	OverallHealth() health.State
}

type presenceActivity struct {
	activityType discordgo.ActivityType
	text         string
}

// PresenceManager owns Champions® Killfeed's Discord bot presence: an
// immediate professional status on startup, an optional rotation through
// live status information, and status (online/idle/dnd) reacting to overall
// application health with a debounce so brief blips never flap it.
type PresenceManager struct {
	updater PresenceUpdater
	stats   PresenceStatsProvider
	health  PresenceHealthProvider

	mode     PresenceMode
	interval time.Duration

	mu                 sync.Mutex
	hasApplied         bool
	currentStatus      string
	currentActivity    presenceActivity
	rotationIndex      int
	lastHealthState    health.State
	lastHealthChangeAt time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// NewPresenceManager creates a presence manager. stats/healthProvider may be
// nil (dynamic player/server activities and health-reactive status are
// simply skipped). interval is clamped to a 30s floor regardless of caller
// input, matching the documented minimum rotation interval.
func NewPresenceManager(updater PresenceUpdater, stats PresenceStatsProvider, healthProvider PresenceHealthProvider, mode PresenceMode, interval time.Duration) *PresenceManager {
	if mode != PresenceModeStatic {
		mode = PresenceModeDynamic
	}
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	return &PresenceManager{
		updater:  updater,
		stats:    stats,
		health:   healthProvider,
		mode:     mode,
		interval: interval,
	}
}

// Start applies the initial professional presence immediately (ONLINE,
// COMPETING "Competitive DayZ"), then begins the rotation/health worker.
// Safe to call once; the worker stops cleanly when ctx is cancelled or Stop
// is called.
func (m *PresenceManager) Start(ctx context.Context) {
	if m == nil || m.updater == nil {
		return
	}
	activities := m.buildActivities()
	if len(activities) > 0 {
		m.apply("online", activities[0])
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.mu.Lock()
	m.cancel = cancel
	m.done = done
	m.mu.Unlock()

	slog.Info("component=discord_presence", "event", "started", "mode", string(m.mode), "interval", m.interval.String())

	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=discord_presence", "event", "panic_recovered", "err", fmt.Sprint(r))
			}
		}()
		rotationTicker := time.NewTicker(m.interval)
		defer rotationTicker.Stop()
		healthTicker := time.NewTicker(presenceHealthPollInterval)
		defer healthTicker.Stop()
		for {
			select {
			case <-runCtx.Done():
				slog.Info("component=discord_presence", "event", "stopped")
				return
			case <-rotationTicker.C:
				m.tick()
			case <-healthTicker.C:
				m.pollHealth()
			}
		}
	}()
}

// Stop cancels the rotation/health worker and blocks until it has fully
// exited, so shutdown never races a presence update against the Discord
// session closing. Safe to call even if Start was never called.
func (m *PresenceManager) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// UpdateNow immediately applies the next rotation entry, bypassing the
// ticker. Useful for tests and for an operator-triggered refresh.
func (m *PresenceManager) UpdateNow() {
	if m == nil {
		return
	}
	m.tick()
}

// SetHealth applies a health-driven status change immediately, subject to
// the same debounce as the periodic health poll.
func (m *PresenceManager) SetHealth(state health.State) {
	if m == nil {
		return
	}
	m.applyHealth(state)
}

func (m *PresenceManager) pollHealth() {
	if m == nil || m.health == nil {
		return
	}
	m.applyHealth(m.health.OverallHealth())
}

// applyHealth maps state to a Discord status and applies it to the
// currently displayed activity, debounced so rapid health flapping cannot
// spam status changes (default 60s - see presenceHealthDebounce).
func (m *PresenceManager) applyHealth(state health.State) {
	m.mu.Lock()
	now := time.Now()
	unchanged := state == m.lastHealthState
	debounced := !m.lastHealthChangeAt.IsZero() && now.Sub(m.lastHealthChangeAt) < presenceHealthDebounce
	if unchanged || (m.hasApplied && debounced) {
		m.mu.Unlock()
		return
	}
	m.lastHealthState = state
	m.lastHealthChangeAt = now
	act := m.currentActivity
	hadActivity := m.hasApplied
	m.mu.Unlock()

	if !hadActivity {
		// Start hasn't applied an initial activity yet; nothing to re-send
		// with a new status. The next rotation tick will use this status.
		return
	}
	slog.Info("component=discord_presence", "event", "health_changed", "state", string(state))
	m.apply(statusForHealth(state), act)
}

func statusForHealth(s health.State) string {
	switch s {
	case health.Unhealthy:
		return "dnd"
	case health.Degraded:
		return "idle"
	default:
		return "online"
	}
}

// tick advances the rotation by one entry and applies it, keeping whatever
// Discord status (online/idle/dnd) is currently in effect.
func (m *PresenceManager) tick() {
	activities := m.buildActivities()
	if len(activities) == 0 {
		return
	}
	m.mu.Lock()
	idx := m.rotationIndex % len(activities)
	m.rotationIndex++
	status := m.currentStatus
	m.mu.Unlock()
	if status == "" {
		status = "online"
	}
	m.apply(status, activities[idx])
}

// buildActivities returns the eligible activities for the current mode.
// Player/server-count entries are included only when PresenceStatsProvider
// reports reliable values - never a fabricated 0.
func (m *PresenceManager) buildActivities() []presenceActivity {
	standard := presenceActivity{discordgo.ActivityTypeCompeting, "Competitive DayZ"}
	if m.mode == PresenceModeStatic {
		return []presenceActivity{standard}
	}

	out := []presenceActivity{standard}
	if m.stats != nil {
		if total, _, configured, ok := m.stats.PresenceCounts(); ok {
			out = append(out, presenceActivity{discordgo.ActivityTypeWatching, fmt.Sprintf("%d Players Online", total)})
			// "configured" here is every server this guild currently has an
			// active worker for (an active game_servers row with a running
			// worker) - not literally every server ever configured in the
			// database (a disconnected one has no worker and is never
			// counted). "Monitored" (rather than "Connected") is the
			// accurate word: a worker keeps retrying and stays counted
			// through a Nitrado/API outage, so "Connected" would imply a
			// live reachability guarantee this count does not make. See
			// App.PresenceCounts/registerPresenceEngine.
			out = append(out, presenceActivity{discordgo.ActivityTypeWatching, fmt.Sprintf("%d Monitored DayZ Servers", configured)})
		}
	}
	out = append(out,
		presenceActivity{discordgo.ActivityTypeGame, "Champions® Killfeed"},
		presenceActivity{discordgo.ActivityTypeWatching, "Live PvP Activity"},
		presenceActivity{discordgo.ActivityTypeCompeting, "DayZ Leaderboards"},
	)
	return out
}

// apply sends status+activity to Discord only when it differs from what was
// last successfully applied (section 11: duplicate-update protection), so
// rotation ticks and health polls never spam the gateway with an identical
// presence. A failed update is logged and never propagated - presence is
// purely cosmetic and must never affect the bot's Discord connection.
func (m *PresenceManager) apply(status string, act presenceActivity) {
	m.mu.Lock()
	if m.hasApplied && m.currentStatus == status && m.currentActivity == act {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	if err := m.updater.UpdatePresence(status, act.activityType, act.text); err != nil {
		slog.Warn("component=discord_presence", "event", "presence_update_failed", "err", err.Error())
		return
	}

	m.mu.Lock()
	m.currentStatus = status
	m.currentActivity = act
	m.hasApplied = true
	m.mu.Unlock()
	slog.Info("component=discord_presence", "event", "updated", "type", activityTypeLabel(act.activityType), "text", act.text, "status", status)
}

func activityTypeLabel(t discordgo.ActivityType) string {
	switch t {
	case discordgo.ActivityTypeCompeting:
		return "competing"
	case discordgo.ActivityTypeWatching:
		return "watching"
	case discordgo.ActivityTypeGame:
		return "playing"
	case discordgo.ActivityTypeListening:
		return "listening"
	case discordgo.ActivityTypeStreaming:
		return "streaming"
	case discordgo.ActivityTypeCustom:
		return "custom"
	default:
		return "unknown"
	}
}
