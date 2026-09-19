package factionstats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeStore is an in-memory Store so cache, invalidation, single-flight and evaluation logic can be
// tested (and raced) without a database.
type fakeStore struct {
	mu                sync.Mutex
	scopes            map[[3]int64]repository.HubStatsScope // (org, inst, faction)
	computes          atomic.Int64
	onCompute         func()
	result            func(s repository.HubStatsScope) *repository.HubStatsResult
	unlocks           map[int64]map[string]repository.HubUnlock
	nth               map[string]time.Time
	forPlayers        func(guild, server int64, players []int64) []repository.HubStatsScope
	lastActivityLimit atomic.Int64
	inserts           atomic.Int64
	boards            map[int64]*repository.HubLeaderboardData
	boardOrg          map[int64]int64
	boardComputes     atomic.Int64
	onLeaderboard     func()
}

func newFakeStore() *fakeStore {
	return &fakeStore{scopes: map[[3]int64]repository.HubStatsScope{}, unlocks: map[int64]map[string]repository.HubUnlock{}, nth: map[string]time.Time{},
		result: func(repository.HubStatsScope) *repository.HubStatsResult {
			return &repository.HubStatsResult{Members: []repository.HubMemberStats{}}
		}}
}

func (f *fakeStore) add(org, inst, faction, guild, server int64) repository.HubStatsScope {
	s := repository.HubStatsScope{FactionID: faction, OrganizationID: org, InstallationID: inst, GuildID: guild, ServerID: server, CreatedAt: time.Now().Add(-48 * time.Hour)}
	f.mu.Lock()
	f.scopes[[3]int64{org, inst, faction}] = s
	f.mu.Unlock()
	return s
}

func (f *fakeStore) Scope(_ context.Context, org, inst, faction int64) (repository.HubStatsScope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.scopes[[3]int64{org, inst, faction}]; ok {
		return s, nil
	}
	return repository.HubStatsScope{}, factionhub.ErrNotFound
}
func (f *fakeStore) ListScopes(_ context.Context, after int64, limit int) ([]repository.HubStatsScope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []repository.HubStatsScope
	for id := after + 1; len(out) < limit && id < after+1000; id++ {
		for _, s := range f.scopes {
			if s.FactionID == id {
				out = append(out, s)
			}
		}
	}
	return out, nil
}
func (f *fakeStore) ScopesForPlayers(_ context.Context, g, s int64, p []int64) ([]repository.HubStatsScope, error) {
	if f.forPlayers != nil {
		return f.forPlayers(g, s, p), nil
	}
	return nil, nil
}
func (f *fakeStore) ComputeStats(_ context.Context, s repository.HubStatsScope) (*repository.HubStatsResult, error) {
	f.computes.Add(1)
	if f.onCompute != nil {
		f.onCompute()
	}
	return f.result(s), nil
}
func (f *fakeStore) Unlocks(_ context.Context, faction int64) (map[string]repository.HubUnlock, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]repository.HubUnlock{}
	for k, v := range f.unlocks[faction] {
		out[k] = v
	}
	return out, nil
}
func (f *fakeStore) InsertUnlock(_ context.Context, s repository.HubStatsScope, key string, at time.Time, meta map[string]any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlocks[s.FactionID] == nil {
		f.unlocks[s.FactionID] = map[string]repository.HubUnlock{}
	}
	if _, dup := f.unlocks[s.FactionID][key]; dup {
		return false, nil // the unique (faction, achievement) key
	}
	f.unlocks[s.FactionID][key] = repository.HubUnlock{Key: key, UnlockedAt: at, Metadata: meta}
	f.inserts.Add(1)
	return true, nil
}
func (f *fakeStore) NthEventTime(_ context.Context, _ repository.HubStatsScope, metric string, n int) (*time.Time, error) {
	if t, ok := f.nth[fmt.Sprintf("%s:%d", metric, n)]; ok {
		return &t, nil
	}
	return nil, nil
}
func (f *fakeStore) Activity(_ context.Context, _ repository.HubStatsScope, limit int, _ *repository.HubActivityCursor) ([]repository.HubActivityRow, bool, error) {
	f.lastActivityLimit.Store(int64(limit))
	return nil, false, nil
}

// clock is a controllable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newSvc(f *fakeStore) (*Service, *clock) {
	c := &clock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	return NewService(f, Options{CacheTTL: 45 * time.Second, Now: c.now}), c
}

func TestStatsCacheTTLAndInvalidation(t *testing.T) {
	f := newFakeStore()
	f.add(1, 1, 10, 100, 200)
	f.add(1, 2, 20, 100, 300) // same guild, another server
	f.add(2, 3, 30, 101, 400)
	s, c := newSvc(f)
	ctx := context.Background()
	get := func(org, inst, fac int64) {
		t.Helper()
		if _, err := s.GetFactionStats(ctx, org, inst, fac); err != nil {
			t.Fatal(err)
		}
	}
	get(1, 1, 10)
	get(1, 1, 10)
	if f.computes.Load() != 1 {
		t.Fatalf("a second read inside the TTL is served from cache, computes=%d", f.computes.Load())
	}
	c.add(44 * time.Second)
	get(1, 1, 10)
	if f.computes.Load() != 1 {
		t.Fatal("still fresh at 44s")
	}
	c.add(2 * time.Second)
	get(1, 1, 10)
	if f.computes.Load() != 2 {
		t.Fatalf("expired after the TTL, computes=%d", f.computes.Load())
	}

	// A kill on (guild 100, server 200) invalidates only factions of that guild+server.
	get(1, 2, 20)
	get(2, 3, 30)
	base := f.computes.Load()
	s.NotifyCombat(100, 200, 0)
	get(1, 1, 10)
	get(1, 2, 20)
	get(2, 3, 30)
	if got := f.computes.Load() - base; got != 1 {
		t.Fatalf("only the faction on the affected server recomputes, got %d recomputes", got)
	}
	// Explicit invalidation (membership change) is per faction.
	base = f.computes.Load()
	s.Invalidate(1, 2, 20)
	get(1, 1, 10)
	get(1, 2, 20)
	if got := f.computes.Load() - base; got != 1 {
		t.Fatalf("Invalidate drops exactly one faction, got %d", got)
	}
}

func TestCacheNeverCrossesTenants(t *testing.T) {
	f := newFakeStore()
	f.add(1, 1, 10, 100, 200)
	s, _ := newSvc(f)
	ctx := context.Background()
	if _, err := s.GetFactionStats(ctx, 1, 1, 10); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]int64{{2, 1, 10}, {1, 2, 10}, {1, 1, 11}, {9, 9, 9}} {
		if _, err := s.GetFactionStats(ctx, bad[0], bad[1], bad[2]); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("%v: a warm cache must not answer another tenant/faction: %v", bad, err)
		}
	}
	if _, err := s.GetFactionRecentActivity(ctx, 2, 1, 10, 5, nil); !errors.Is(err, factionhub.ErrNotFound) {
		t.Fatalf("activity is tenant-scoped too: %v", err)
	}
}

func TestSingleFlightAndCancelledEventDuringCompute(t *testing.T) {
	f := newFakeStore()
	f.add(1, 1, 10, 100, 200)
	gate := make(chan struct{})
	f.onCompute = func() { <-gate }
	s, _ := newSvc(f)
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.GetFactionStats(context.Background(), 1, 1, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if f.computes.Load() != 1 {
		t.Fatalf("a cold burst is ONE computation, got %d", f.computes.Load())
	}

	// An event landing DURING a computation must not be masked by the entry it produces.
	f2 := newFakeStore()
	f2.add(1, 1, 10, 100, 200)
	s2, _ := newSvc(f2)
	var fired atomic.Bool
	f2.onCompute = func() {
		if fired.CompareAndSwap(false, true) {
			s2.NotifyCombat(100, 200, 0)
		}
	}
	_, _ = s2.GetFactionStats(context.Background(), 1, 1, 10)
	_, _ = s2.GetFactionStats(context.Background(), 1, 1, 10)
	if f2.computes.Load() != 2 {
		t.Fatalf("the entry computed across an invalidating event is stale and must be recomputed, computes=%d", f2.computes.Load())
	}
}

func TestBuildStatsAttributionAndAggregation(t *testing.T) {
	role := "LEADER"
	member := int64(7)
	raw := &repository.HubStatsResult{MemberCount: 3, LinkedCount: 2, Members: []repository.HubMemberStats{
		{UserID: 1, DiscordUserID: "d1", Username: "u1", GlobalName: "One", MemberID: &member, Role: &role, Active: true, Linked: true, Kills: 10, Deaths: 4, Headshots: 3, Longshots: 2, Bounties: 2, BountyValue: 150, BestStreak: 5, CurrentStreak: 2},
		{UserID: 2, DiscordUserID: "d2", Username: "u2", Active: true, Linked: false, Kills: 999, Deaths: 999},                              // unlinked: any leaked numbers must be dropped
		{UserID: 3, DiscordUserID: "d3", Username: "u3", Active: false, Linked: true, Kills: 5, Deaths: 0, BestStreak: 9, CurrentStreak: 9}, // former: streak counts as best, not current
	}}
	st := buildStats(raw, 3, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	s := st.Summary
	if s.Kills != 15 || s.Deaths != 4 || s.KDRatio != 3.75 || s.Headshots != 3 || s.Longshots != 2 || s.BountiesClaimed != 2 || s.BountyValueClaimed != 150 {
		t.Fatalf("summary sums linked members only: %+v", s)
	}
	if s.BestKillStreak != 9 || s.CurrentKillStreak != 2 {
		t.Fatalf("best is the best ever (incl. former), current is only an ACTIVE member's ongoing streak: %+v", s)
	}
	if s.MemberCount != 3 || s.LinkedMemberCount != 2 || s.AchievementsUnlocked != 3 || s.TrackingSince != nil {
		t.Fatalf("counts: %+v", s)
	}
	u := st.MemberContributions[1]
	if u.Identity != IdentityUnlinked || u.StatsEligible || u.Kills != 0 || u.Deaths != 0 || u.KDRatio != 0 {
		t.Fatalf("an unlinked member is zero and says why: %+v", u)
	}
	if st.MemberContributions[0].DisplayName != "One" || st.MemberContributions[1].DisplayName != "u2" {
		t.Fatal("display name: global name, else username")
	}
	if fm := st.MemberContributions[2]; fm.Status != StatusFormer || fm.KDRatio != 5 || fm.MemberID != nil {
		t.Fatalf("former member: %+v", fm)
	}
}

func TestCursorRoundTripAndRejection(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 30, 45, 123456000, time.UTC)
	c, ok := DecodeCursor(encodeCursor(at, repository.ActivitySrcKill, 987654321))
	if !ok || !c.At.Equal(at) || c.Src != repository.ActivitySrcKill || c.ID != 987654321 {
		t.Fatalf("round trip: %+v %v", c, ok)
	}
	for _, bad := range []string{"", "x", "!!!!", encodeCursor(at, 0, 1), encodeCursor(at, 99, 1), encodeCursor(at, 1, -1)} {
		if _, ok := DecodeCursor(bad); ok {
			t.Errorf("cursor %q must be rejected", bad)
		}
	}
}

func TestActivityLimitClamp(t *testing.T) {
	f := newFakeStore()
	f.add(1, 1, 10, 100, 200)
	s, _ := newSvc(f)
	for in, want := range map[int]int{0: 20, -5: 20, 1: 1, 20: 20, 100: 100, 101: 100, 1 << 30: 100} {
		if _, err := s.GetFactionRecentActivity(context.Background(), 1, 1, 10, in, nil); err != nil {
			t.Fatal(err)
		}
		if int(f.lastActivityLimit.Load()) != want {
			t.Errorf("limit %d -> store limit %d, want %d", in, f.lastActivityLimit.Load(), want)
		}
	}
}

func TestDefinitionsAreStableAndWellFormed(t *testing.T) {
	want := []string{"FIRST_BLOOD", "KILLS_100", "KILLS_500", "KILLS_1000", "HEADHUNTERS", "LONG_RANGE", "BOUNTY_HUNTERS", "KILLING_MACHINE", "FULL_SQUAD", "VETERAN_FACTION"}
	if len(Definitions) != len(want) {
		t.Fatalf("catalog size %d", len(Definitions))
	}
	for i, d := range Definitions {
		if d.Key != want[i] || d.Target <= 0 || d.Name == "" || d.Description == "" || d.Unit == "" {
			t.Errorf("definition %d: %+v", i, d)
		}
		if got, ok := DefinitionByKey(d.Key); !ok || got.Key != d.Key {
			t.Errorf("lookup %s", d.Key)
		}
	}
	if _, ok := DefinitionByKey("MADE_UP"); ok {
		t.Fatal("unknown key")
	}
}

func TestEvaluateIsIdempotentAndConcurrentSafe(t *testing.T) {
	f := newFakeStore()
	sc := f.add(1, 1, 10, 100, 200)
	f.result = func(repository.HubStatsScope) *repository.HubStatsResult {
		return &repository.HubStatsResult{MemberCount: 5, LinkedCount: 5, Members: []repository.HubMemberStats{
			{UserID: 1, DiscordUserID: "d1", Username: "u", Active: true, Linked: true, Kills: 100, Deaths: 1, Headshots: 25, Longshots: 10, Bounties: 5, BestStreak: 10}}}
	}
	nth := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	f.nth["kills:100"], f.nth["kills:1"] = nth, nth.Add(-time.Hour)
	s, _ := newSvc(f)
	var wg sync.WaitGroup
	won := make(chan []string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := s.Evaluate(context.Background(), sc)
			if err != nil {
				t.Error(err)
			}
			won <- w
		}()
	}
	wg.Wait()
	close(won)
	total := 0
	for w := range won {
		total += len(w)
	}
	// FIRST_BLOOD, KILLS_100, HEADHUNTERS, LONG_RANGE, BOUNTY_HUNTERS, KILLING_MACHINE, FULL_SQUAD, VETERAN (48h old is NOT 30 days -> no)
	if total != 7 || f.inserts.Load() != 7 {
		t.Fatalf("each earned achievement is reported by exactly one evaluator: reported %d, inserted %d", total, f.inserts.Load())
	}
	if u := f.unlocks[10]["KILLS_100"]; !u.UnlockedAt.Equal(nth) {
		t.Fatalf("unlock time is the real 100th-kill time, got %v", u.UnlockedAt)
	}
	if u := f.unlocks[10]["FIRST_BLOOD"]; !u.UnlockedAt.Equal(nth.Add(-time.Hour)) {
		t.Fatalf("FIRST_BLOOD time: %v", u.UnlockedAt)
	}
	if again, err := s.Evaluate(context.Background(), sc); err != nil || len(again) != 0 {
		t.Fatalf("re-evaluation unlocks nothing: %v %v", again, err)
	}
	if _, ok := f.unlocks[10]["VETERAN_FACTION"]; ok {
		t.Fatal("a 2-day-old faction is not a veteran")
	}
}

func TestProcessPendingResolvesFactionsOncePerBurst(t *testing.T) {
	f := newFakeStore()
	sc := f.add(1, 1, 10, 100, 200)
	var resolved atomic.Int64
	f.forPlayers = func(g, s int64, players []int64) []repository.HubStatsScope {
		resolved.Add(1)
		if g == 100 && s == 200 {
			return []repository.HubStatsScope{sc}
		}
		return nil
	}
	s, _ := newSvc(f)
	for i := 0; i < 500; i++ {
		s.NotifyCombat(100, 200, int64(1+i%3)) // a burst of kills by three players
	}
	s.NotifyCombat(100, 200, 0) // a death queues nothing
	if n := s.ProcessPending(context.Background()); n != 1 {
		t.Fatalf("one evaluation for the burst, got %d", n)
	}
	if resolved.Load() != 1 {
		t.Fatalf("one player->faction resolution per (guild, server), got %d", resolved.Load())
	}
	if n := s.ProcessPending(context.Background()); n != 0 {
		t.Fatalf("the queue is drained: %d", n)
	}
	var nilSvc *Service
	nilSvc.NotifyCombat(1, 2, 3) // the killfeed hook is nil-safe when the feature is not wired
}

// The kill-path notifier, the cache, the evaluator and the activity reader all run at once.
func TestConcurrentEventsCacheAndEvaluationAreRaceFree(t *testing.T) {
	f := newFakeStore()
	scopes := []repository.HubStatsScope{f.add(1, 1, 10, 100, 200), f.add(1, 2, 20, 100, 300), f.add(2, 3, 30, 101, 400)}
	f.forPlayers = func(g, s int64, _ []int64) []repository.HubStatsScope {
		var out []repository.HubStatsScope
		for _, sc := range scopes {
			if sc.GuildID == g && sc.ServerID == s {
				out = append(out, sc)
			}
		}
		return out
	}
	f.result = func(repository.HubStatsScope) *repository.HubStatsResult {
		return &repository.HubStatsResult{MemberCount: 1, Members: []repository.HubMemberStats{{UserID: 1, DiscordUserID: "d", Username: "u", Active: true, Linked: true, Kills: 200}}}
	}
	s, c := newSvc(f)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(4)
		go func(i int) { // killfeed hook
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.NotifyCombat(100, 200+int64(j%2)*100, int64(j%5))
			}
		}(i)
		go func() { // website reads
			defer wg.Done()
			for j := 0; j < 100; j++ {
				sc := scopes[j%3]
				_, _ = s.GetFactionStats(ctx, sc.OrganizationID, sc.InstallationID, sc.FactionID)
				_, _ = s.GetFactionAchievements(ctx, sc.OrganizationID, sc.InstallationID, sc.FactionID)
				_, _ = s.GetFactionRecentActivity(ctx, sc.OrganizationID, sc.InstallationID, sc.FactionID, 10, nil)
			}
		}()
		go func() { // membership changes and the worker
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Invalidate(1, 1, 10)
				s.ProcessPending(ctx)
				c.add(time.Second)
			}
		}()
		go func() { // reconcile
			defer wg.Done()
			_, _, _ = s.ReconcileAll(ctx, 2)
		}()
	}
	wg.Wait()
	cancel()
	for _, sc := range scopes {
		if n := len(f.unlocks[sc.FactionID]); n != 2 { // 200 kills: FIRST_BLOOD and KILLS_100 only
			t.Fatalf("faction %d ended with %d unlocks", sc.FactionID, n)
		}
	}
}

// LeaderboardScope / Leaderboard: the fake serves a per-installation table registered with setBoard.
func (f *fakeStore) setBoard(org, inst, guild, server int64, rows []repository.HubLeaderboardRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boards == nil {
		f.boards = map[int64]*repository.HubLeaderboardData{}
	}
	f.boards[inst] = &repository.HubLeaderboardData{GuildID: guild, ServerID: server, Rows: rows}
	if f.boardOrg == nil {
		f.boardOrg = map[int64]int64{}
	}
	f.boardOrg[inst] = org
}

func (f *fakeStore) LeaderboardScope(_ context.Context, org, inst int64) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boards[inst]
	if !ok || f.boardOrg[inst] != org {
		return 0, 0, factionhub.ErrNotFound
	}
	return b.GuildID, b.ServerID, nil
}

func (f *fakeStore) Leaderboard(_ context.Context, org, inst int64) (*repository.HubLeaderboardData, error) {
	f.boardComputes.Add(1)
	if f.onLeaderboard != nil {
		f.onLeaderboard()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.boards[inst]
	if !ok || f.boardOrg[inst] != org {
		return nil, factionhub.ErrNotFound
	}
	cp := *b
	cp.Rows = append([]repository.HubLeaderboardRow(nil), b.Rows...)
	return &cp, nil
}
