package killfeed

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// --- fakes -----------------------------------------------------------------------------------

type zonePlayerKey struct{ ZoneID, PlayerID int64 }

type fakeIntrusionStore struct {
	mu sync.Mutex

	ignoredPlayers     map[zonePlayerKey]bool
	ignoredFactions    map[int64]map[int64]bool // zoneID -> factionID -> ignored
	discordRoleIgnores map[int64][]string        // zoneID -> role snowflakes
	authorized         map[zonePlayerKey]bool
	bans               map[zonePlayerKey]*repository.ZoneBan

	playerDiscordUser map[int64]string // playerID -> discord user id
	playerFaction     map[int64]int64  // playerID -> faction id (0 = none)
	discordGuildID    string

	presence map[zonePlayerKey]*repository.ZonePresence
	open     map[zonePlayerKey]*repository.ZoneIntrusion

	nextID    int64
	created   []repository.ZoneIntrusion
	exitedIDs []int64
}

func newFakeIntrusionStore() *fakeIntrusionStore {
	return &fakeIntrusionStore{
		ignoredPlayers: map[zonePlayerKey]bool{}, ignoredFactions: map[int64]map[int64]bool{},
		discordRoleIgnores: map[int64][]string{}, authorized: map[zonePlayerKey]bool{},
		bans: map[zonePlayerKey]*repository.ZoneBan{}, playerDiscordUser: map[int64]string{},
		playerFaction: map[int64]int64{}, presence: map[zonePlayerKey]*repository.ZonePresence{},
		open: map[zonePlayerKey]*repository.ZoneIntrusion{}, discordGuildID: "guild-1",
	}
}

func (f *fakeIntrusionStore) IsIgnored(_ context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ignoredPlayers[zonePlayerKey{zoneID, playerID}] {
		return true, nil
	}
	if factionID != nil {
		if m, ok := f.ignoredFactions[zoneID]; ok && m[*factionID] {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeIntrusionStore) DiscordRoleIgnoreEntries(_ context.Context, zoneID int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discordRoleIgnores[zoneID], nil
}

func (f *fakeIntrusionStore) IsAuthorized(_ context.Context, zoneID, playerID int64, factionID *int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorized[zonePlayerKey{zoneID, playerID}], nil
}

func (f *fakeIntrusionStore) PlayerDiscordUserID(_ context.Context, _, playerID int64) (*string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.playerDiscordUser[playerID]; ok {
		return &id, nil
	}
	return nil, nil
}

func (f *fakeIntrusionStore) PlayerFactionID(_ context.Context, _, playerID int64) (*int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.playerFaction[playerID]; ok {
		return &id, nil
	}
	return nil, nil
}

func (f *fakeIntrusionStore) DiscordGuildID(_ context.Context, _ int64) (string, error) {
	return f.discordGuildID, nil
}

func (f *fakeIntrusionStore) ActiveZoneBan(_ context.Context, zoneID, playerID int64) (*repository.ZoneBan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bans[zonePlayerKey{zoneID, playerID}], nil
}

func (f *fakeIntrusionStore) GetPresence(_ context.Context, zoneID, playerID int64) (*repository.ZonePresence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence[zonePlayerKey{zoneID, playerID}], nil
}

func (f *fakeIntrusionStore) UpsertPresence(_ context.Context, zoneID, playerID int64, status string, enteredAt *time.Time, lastSeenAt time.Time, lastLocationEventID int64, lastAlertAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := zonePlayerKey{zoneID, playerID}
	existing := f.presence[key]
	p := &repository.ZonePresence{ZoneID: zoneID, PlayerID: playerID, Status: status, EnteredAt: enteredAt, LastSeenAt: lastSeenAt, LastLocationEventID: &lastLocationEventID}
	if lastAlertAt != nil {
		p.LastAlertAt = lastAlertAt
	} else if existing != nil {
		p.LastAlertAt = existing.LastAlertAt
	}
	f.presence[key] = p
	return nil
}

func (f *fakeIntrusionStore) GetOpenIntrusion(_ context.Context, zoneID, playerID int64) (*repository.ZoneIntrusion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[zonePlayerKey{zoneID, playerID}], nil
}

func (f *fakeIntrusionStore) CreateIntrusion(_ context.Context, zoneID, installationID, guildID, serverID, playerID int64, gamertag string, banned bool, enteredAt time.Time, alerted bool) (*repository.ZoneIntrusion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	it := repository.ZoneIntrusion{
		ID: f.nextID, ZoneID: zoneID, InstallationID: installationID, GuildID: guildID, ServerID: serverID,
		PlayerID: playerID, Gamertag: gamertag, Status: repository.IntrusionActive, Banned: banned, EnteredAt: enteredAt,
	}
	if alerted {
		it.LastAlertAt = &enteredAt
		it.AlertCount = 1
	}
	f.open[zonePlayerKey{zoneID, playerID}] = &it
	f.created = append(f.created, it)
	return &it, nil
}

func (f *fakeIntrusionStore) MarkExited(_ context.Context, intrusionID int64, exitedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitedIDs = append(f.exitedIDs, intrusionID)
	for k, it := range f.open {
		if it.ID == intrusionID {
			delete(f.open, k)
		}
	}
	return nil
}

type fakeZoneSource struct{ zones []repository.Zone }

func (f *fakeZoneSource) ActiveZonesForServer(_ context.Context, serverID int64) ([]repository.Zone, error) {
	var out []repository.Zone
	for _, z := range f.zones {
		if z.ServerID == serverID {
			out = append(out, z)
		}
	}
	return out, nil
}

type fakeRoleChecker struct{ hasRole map[string]bool } // key: guildID|userID|roleID

func (f *fakeRoleChecker) MemberHasRole(guildID, userID, roleID string) (bool, error) {
	return f.hasRole[guildID+"|"+userID+"|"+roleID], nil
}

type fakePublisher struct {
	mu     sync.Mutex
	events []IntrusionEvent
}

func (p *fakePublisher) PublishIntrusionEvent(ev IntrusionEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

func testZone(id, serverID int64, zoneType string, cx, cz, radius float64, cooldownSec int, channel string) repository.Zone {
	z := repository.Zone{ID: id, InstallationID: 900, GuildID: 1, ServerID: serverID, Name: "zone", ZoneType: zoneType,
		CenterX: cx, CenterZ: cz, Radius: radius, CooldownSeconds: cooldownSec, Enabled: true}
	if channel != "" {
		z.AlertChannelID = &channel
	}
	return z
}

// --- tests -----------------------------------------------------------------------------------

func TestIntrusionEngineEntryCreatesIntrusionAndAlerts(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 1 {
		t.Fatalf("expected 1 intrusion created, got %d", len(store.created))
	}
	if pub.count() != 1 {
		t.Fatalf("expected 1 published event, got %d", pub.count())
	}
	ev := pub.events[0]
	if ev.Kind != AlertZoneIntrusion || ev.Suppressed {
		t.Fatalf("unexpected event %+v", ev)
	}
	m := engine.Metrics()
	if m.Entries != 1 || m.IntrusionsCreated != 1 || m.AlertsSent != 1 {
		t.Fatalf("unexpected metrics %+v", m)
	}
}

func TestIntrusionEngineOngoingPresenceDoesNotReAlert(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)
	ctx := context.Background()
	now := time.Now()

	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 1, Z: 1, ObservedAt: now.Add(10 * time.Second)}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 2, Z: 2, ObservedAt: now.Add(20 * time.Second)}})

	if len(store.created) != 1 {
		t.Fatalf("expected exactly 1 intrusion across 3 still-inside updates, got %d", len(store.created))
	}
	if pub.count() != 1 {
		t.Fatalf("expected exactly 1 published event (must NOT alert on every location update), got %d", pub.count())
	}
}

func TestIntrusionEngineExitClosesIntrusion(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 10, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)
	ctx := context.Background()
	now := time.Now()

	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 500, Z: 500, ObservedAt: now.Add(10 * time.Second)}})

	if len(store.exitedIDs) != 1 {
		t.Fatalf("expected 1 exit, got %d", len(store.exitedIDs))
	}
	if pub.count() != 2 {
		t.Fatalf("expected entry+exit = 2 events, got %d", pub.count())
	}
	if pub.events[1].Kind != AlertZoneExit || !pub.events[1].Suppressed {
		t.Fatalf("exit event must be ZONE_EXIT and Suppressed (never posted to Discord): %+v", pub.events[1])
	}
	if m := engine.Metrics(); m.Exits != 1 {
		t.Fatalf("expected Exits=1, got %+v", m)
	}
}

func TestIntrusionEngineCooldownSuppressesReAlertOnFlap(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 10, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)
	ctx := context.Background()
	now := time.Now()

	// Entry -> exit -> re-entry, all within the 300s cooldown window.
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 500, Z: 500, ObservedAt: now.Add(5 * time.Second)}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now.Add(10 * time.Second)}})

	if len(store.created) != 2 {
		t.Fatalf("expected 2 intrusions (exit+re-entry is always a new intrusion), got %d", len(store.created))
	}
	m := engine.Metrics()
	if m.AlertsSent != 1 || m.AlertsSuppressedCooldown != 1 {
		t.Fatalf("expected 1 sent + 1 cooldown-suppressed alert, got %+v", m)
	}
}

func TestIntrusionEngineReEntryAfterCooldownAlertsAgain(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 10, 5, "chan-1")}}) // 5s cooldown
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)
	ctx := context.Background()
	now := time.Now()

	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now}})
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 500, Z: 500, ObservedAt: now.Add(1 * time.Second)}})
	// Re-enter well after the 5s cooldown elapsed.
	engine.Evaluate(ctx, 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: now.Add(10 * time.Second)}})

	if m := engine.Metrics(); m.AlertsSent != 2 || m.AlertsSuppressedCooldown != 0 {
		t.Fatalf("expected both entries to alert once cooldown elapsed, got %+v", m)
	}
}

func TestIntrusionEngineIgnoredPlayerNeverTracked(t *testing.T) {
	store := newFakeIntrusionStore()
	store.ignoredPlayers[zonePlayerKey{1, 10}] = true
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 0 || pub.count() != 0 {
		t.Fatalf("ignored player must produce zero intrusions and zero events: created=%d events=%d", len(store.created), pub.count())
	}
	if _, ok := store.presence[zonePlayerKey{1, 10}]; ok {
		t.Fatalf("ignored player must never get a presence row")
	}
	if m := engine.Metrics(); m.IntrusionsSuppressed != 1 {
		t.Fatalf("expected IntrusionsSuppressed=1, got %+v", m)
	}
}

func TestIntrusionEngineDiscordRoleIgnore(t *testing.T) {
	store := newFakeIntrusionStore()
	store.discordRoleIgnores[1] = []string{"role-admin"}
	store.playerDiscordUser[10] = "discord-user-10"
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	roles := &fakeRoleChecker{hasRole: map[string]bool{"guild-1|discord-user-10|role-admin": true}}
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, roles, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 0 {
		t.Fatalf("player holding an ignored Discord role must not be tracked, got %d intrusions", len(store.created))
	}
}

func TestIntrusionEngineAuthorizedUAVSuppressesAlertButStillTracksIntrusion(t *testing.T) {
	store := newFakeIntrusionStore()
	store.authorized[zonePlayerKey{1, 10}] = true
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeUAV, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 1 {
		t.Fatalf("an authorized UAV entry must still be tracked as an intrusion, got %d", len(store.created))
	}
	if pub.count() != 1 || !pub.events[0].Suppressed {
		t.Fatalf("authorized UAV entry must publish a suppressed event, got %+v", pub.events)
	}
	if m := engine.Metrics(); m.AlertsSent != 0 {
		t.Fatalf("authorized entry must never count as AlertsSent, got %+v", m)
	}
}

func TestIntrusionEngineBannedPlayerFlaggedAndBanViolationEmitted(t *testing.T) {
	store := newFakeIntrusionStore()
	store.bans[zonePlayerKey{1, 10}] = &repository.ZoneBan{ID: 1, ZoneID: 1, PlayerID: 10, Active: true}
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 1 || !store.created[0].Banned {
		t.Fatalf("expected 1 intrusion flagged banned, got %+v", store.created)
	}
	if pub.count() != 2 || pub.events[1].Kind != AlertZoneBanViolation {
		t.Fatalf("expected ZONE_INTRUSION + ZONE_BAN_VIOLATION events, got %+v", pub.events)
	}
}

func TestIntrusionEngineRestartRecoveryDoesNotFabricateEntry(t *testing.T) {
	store := newFakeIntrusionStore()
	entered := time.Now().Add(-time.Hour)
	// Simulate a process restart: presence was already persisted as INSIDE by a previous process,
	// and a matching open intrusion already exists - the fresh IntrusionEngine below has never seen
	// this player before, exactly like a real restart.
	store.presence[zonePlayerKey{1, 10}] = &repository.ZonePresence{ZoneID: 1, PlayerID: 10, Status: repository.PresenceInside, EnteredAt: &entered, LastSeenAt: entered}
	store.open[zonePlayerKey{1, 10}] = &repository.ZoneIntrusion{ID: 99, ZoneID: 1, PlayerID: 10, Status: repository.IntrusionActive, EnteredAt: entered}
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 0 {
		t.Fatalf("an already-inside player's first post-restart event must never create a fresh intrusion, got %d", len(store.created))
	}
	if pub.count() != 0 {
		t.Fatalf("an already-inside player's first post-restart event must never alert, got %d events", pub.count())
	}
}

func TestIntrusionEngineMultiZoneIndependentTracking(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{
		testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "chan-1"),
		testZone(2, 5, repository.ZoneTypeSafezone, 0, 0, 200, 300, "chan-2"),
	}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 2 {
		t.Fatalf("a player inside two overlapping zones must be tracked independently in both, got %d", len(store.created))
	}
	if _, ok := store.presence[zonePlayerKey{1, 10}]; !ok {
		t.Fatalf("missing presence for zone 1")
	}
	if _, ok := store.presence[zonePlayerKey{2, 10}]; !ok {
		t.Fatalf("missing presence for zone 2")
	}
}

func TestIntrusionEngineNoConfiguredChannelNeverAlerts(t *testing.T) {
	store := newFakeIntrusionStore()
	cache := NewZoneCache(&fakeZoneSource{zones: []repository.Zone{testZone(1, 5, repository.ZoneTypeRestricted, 0, 0, 100, 300, "")}})
	pub := &fakePublisher{}
	engine := NewIntrusionEngine(store, cache, nil, pub)

	engine.Evaluate(context.Background(), 5, []locationRecord{{GuildID: 1, ServerID: 5, PlayerID: 10, Gamertag: "Bob", X: 0, Z: 0, ObservedAt: time.Now()}})

	if len(store.created) != 1 {
		t.Fatalf("expected the intrusion to still be tracked, got %d", len(store.created))
	}
	if !pub.events[0].Suppressed {
		t.Fatalf("a zone with no alert_channel_id must never produce a non-suppressed event")
	}
	if m := engine.Metrics(); m.AlertsSent != 0 {
		t.Fatalf("expected AlertsSent=0, got %+v", m)
	}
}

func TestIntrusionEngineNilAndEmptyInputsAreNoops(t *testing.T) {
	var nilEngine *IntrusionEngine
	nilEngine.Evaluate(context.Background(), 1, []locationRecord{{PlayerID: 1}})
	if got := nilEngine.Metrics(); got != (IntrusionMetricsSnapshot{}) {
		t.Fatalf("nil engine must report zero metrics, got %+v", got)
	}

	engine := NewIntrusionEngine(newFakeIntrusionStore(), NewZoneCache(&fakeZoneSource{}), nil, nil)
	engine.Evaluate(context.Background(), 5, nil)
	if m := engine.Metrics(); m.LocationEventsEvaluated != 0 {
		t.Fatalf("empty batch must evaluate nothing, got %+v", m)
	}
}
