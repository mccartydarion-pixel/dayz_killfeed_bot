package discord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/health"
)

// fakePresenceUpdater records every applied status+activity and can inject a
// failure, without ever touching a real Discord gateway.
type fakePresenceUpdater struct {
	mu    sync.Mutex
	calls []struct {
		status string
		typ    discordgo.ActivityType
		text   string
	}
	err error
}

func (f *fakePresenceUpdater) UpdatePresence(status string, activityType discordgo.ActivityType, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, struct {
		status string
		typ    discordgo.ActivityType
		text   string
	}{status, activityType, text})
	return nil
}

func (f *fakePresenceUpdater) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePresenceUpdater) last() (status string, typ discordgo.ActivityType, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", 0, ""
	}
	c := f.calls[len(f.calls)-1]
	return c.status, c.typ, c.text
}

type fakeStatsProvider struct {
	total, online, configured int
	ok                        bool
}

func (f fakeStatsProvider) PresenceCounts() (int, int, int, bool) {
	return f.total, f.online, f.configured, f.ok
}

type fakeHealthProvider struct {
	mu    sync.Mutex
	state health.State
}

func (f *fakeHealthProvider) OverallHealth() health.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeHealthProvider) set(s health.State) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}

// TestPresenceStaticModeAlwaysCompetingCompetitiveDayZ covers static mode:
// exactly one activity, regardless of stats availability.
func TestPresenceStaticModeAlwaysCompetingCompetitiveDayZ(t *testing.T) {
	m := NewPresenceManager(&fakePresenceUpdater{}, fakeStatsProvider{total: 10, configured: 2, ok: true}, nil, PresenceModeStatic, time.Minute)
	activities := m.buildActivities()
	if len(activities) != 1 {
		t.Fatalf("expected exactly 1 activity in static mode, got %d", len(activities))
	}
	if activities[0].activityType != discordgo.ActivityTypeCompeting || activities[0].text != "Competitive DayZ" {
		t.Fatalf("unexpected static activity: %+v", activities[0])
	}
}

// TestPresenceDynamicModeIncludesLiveCounts covers dynamic mode with
// available stats: the player/server entries must appear with real values.
func TestPresenceDynamicModeIncludesLiveCounts(t *testing.T) {
	m := NewPresenceManager(&fakePresenceUpdater{}, fakeStatsProvider{total: 27, configured: 3, ok: true}, nil, PresenceModeDynamic, time.Minute)
	activities := m.buildActivities()
	if len(activities) < 5 {
		t.Fatalf("expected at least 5 dynamic activities, got %d", len(activities))
	}
	foundPlayers, foundServers := false, false
	for _, a := range activities {
		if a.text == "27 Players Online" {
			foundPlayers = true
		}
		if a.text == "3 Monitored DayZ Servers" {
			foundServers = true
		}
	}
	if !foundPlayers {
		t.Fatal("expected a players-online activity with the real count")
	}
	if !foundServers {
		t.Fatal("expected a monitored-DayZ-servers activity with the real count")
	}
}

// TestPresenceDynamicModeSkipsUnavailableCounts proves fabricated 0 values
// are never shown: when stats are unavailable, the count-based activities
// are omitted entirely rather than displaying "0 Players Online".
func TestPresenceDynamicModeSkipsUnavailableCounts(t *testing.T) {
	m := NewPresenceManager(&fakePresenceUpdater{}, fakeStatsProvider{ok: false}, nil, PresenceModeDynamic, time.Minute)
	activities := m.buildActivities()
	for _, a := range activities {
		if a.text == "0 Players Online" || a.text == "0 Monitored DayZ Servers" {
			t.Fatalf("expected no fabricated count activity, got %q", a.text)
		}
	}

	// Also true when no stats provider is wired at all.
	m2 := NewPresenceManager(&fakePresenceUpdater{}, nil, nil, PresenceModeDynamic, time.Minute)
	for _, a := range m2.buildActivities() {
		if a.text == "0 Players Online" || a.text == "0 Monitored DayZ Servers" {
			t.Fatalf("expected no fabricated count activity with nil stats, got %q", a.text)
		}
	}
}

// TestPresenceDuplicateUpdateSuppressed proves section 11: an identical
// status+activity is never resent to Discord.
func TestPresenceDuplicateUpdateSuppressed(t *testing.T) {
	updater := &fakePresenceUpdater{}
	m := NewPresenceManager(updater, nil, nil, PresenceModeStatic, time.Minute)

	act := presenceActivity{discordgo.ActivityTypeCompeting, "Competitive DayZ"}
	m.apply("online", act)
	m.apply("online", act)
	m.apply("online", act)

	if got := updater.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 Discord update for repeated identical presence, got %d", got)
	}

	m.apply("idle", act)
	if got := updater.callCount(); got != 2 {
		t.Fatalf("expected a second update once status actually changed, got %d", got)
	}
}

// TestPresenceUpdateFailureDoesNotPanicOrBlockRetry proves section 9: a
// failed presence update is swallowed, not applied to "last known", so the
// very next attempt retries rather than being suppressed as a duplicate.
func TestPresenceUpdateFailureDoesNotPanicOrBlockRetry(t *testing.T) {
	updater := &fakePresenceUpdater{err: errors.New("gateway not connected")}
	m := NewPresenceManager(updater, nil, nil, PresenceModeStatic, time.Minute)

	act := presenceActivity{discordgo.ActivityTypeCompeting, "Competitive DayZ"}
	m.apply("online", act) // fails, must not panic

	m.mu.Lock()
	applied := m.hasApplied
	m.mu.Unlock()
	if applied {
		t.Fatal("a failed update must not be recorded as successfully applied")
	}

	updater.mu.Lock()
	updater.err = nil
	updater.mu.Unlock()
	m.apply("online", act)
	if got := updater.callCount(); got != 1 {
		t.Fatalf("expected the retry to actually reach the updater, got %d recorded successes", got)
	}
}

// TestPresenceHealthStateMapping covers the ONLINE/IDLE/DND mapping.
func TestPresenceHealthStateMapping(t *testing.T) {
	cases := []struct {
		state health.State
		want  string
	}{
		{health.Healthy, "online"},
		{health.Degraded, "idle"},
		{health.Unhealthy, "dnd"},
		{health.Unknown, "online"},
	}
	for _, tc := range cases {
		if got := statusForHealth(tc.state); got != tc.want {
			t.Fatalf("statusForHealth(%s) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// TestPresenceHealthDebounceSuppressesRapidFlapping proves section 3: two
// health changes within the debounce window must not both reach Discord.
func TestPresenceHealthDebounceSuppressesRapidFlapping(t *testing.T) {
	updater := &fakePresenceUpdater{}
	m := NewPresenceManager(updater, nil, nil, PresenceModeStatic, time.Minute)
	m.apply("online", presenceActivity{discordgo.ActivityTypeCompeting, "Competitive DayZ"}) // seed an applied activity

	base := updater.callCount()
	m.applyHealth(health.Degraded)
	afterFirst := updater.callCount()
	if afterFirst != base+1 {
		t.Fatalf("expected the first health change to apply, got %d calls (base %d)", afterFirst, base)
	}

	m.applyHealth(health.Healthy) // immediately flips back - must be debounced
	if got := updater.callCount(); got != afterFirst {
		t.Fatalf("expected rapid health flap to be debounced, got %d calls", got)
	}
}

// TestPresenceCleanContextCancellation proves section 7/13: Start's worker
// goroutine exits promptly and deterministically when its context is
// cancelled, and Stop never leaks - it blocks until the goroutine is gone.
func TestPresenceCleanContextCancellation(t *testing.T) {
	updater := &fakePresenceUpdater{}
	m := NewPresenceManager(updater, nil, nil, PresenceModeStatic, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	if updater.callCount() != 1 {
		t.Fatalf("expected the initial presence applied immediately on Start, got %d calls", updater.callCount())
	}

	cancel()

	done := make(chan struct{})
	go func() {
		m.Stop() // must return once the worker goroutine has exited
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after context cancellation - possible goroutine leak")
	}
}

// TestPresenceStopWithoutStartIsSafe ensures Stop never panics or blocks
// when Start was never called.
func TestPresenceStopWithoutStartIsSafe(t *testing.T) {
	m := NewPresenceManager(&fakePresenceUpdater{}, nil, nil, PresenceModeStatic, time.Minute)
	done := make(chan struct{})
	go func() {
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked with no Start call")
	}
}

// TestNewPresenceManagerEnforcesMinimumInterval proves the 30s floor is
// enforced by the constructor itself, independent of config-layer clamping.
func TestNewPresenceManagerEnforcesMinimumInterval(t *testing.T) {
	m := NewPresenceManager(&fakePresenceUpdater{}, nil, nil, PresenceModeDynamic, 5*time.Second)
	if m.interval != 30*time.Second {
		t.Fatalf("expected interval clamped to 30s floor, got %s", m.interval)
	}
}
