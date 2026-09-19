package factionstats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func row(id int64, name, tag string, kills, deaths int64) repository.HubLeaderboardRow {
	return repository.HubLeaderboardRow{FactionID: id, Name: name, Tag: tag, Slug: strings.ToLower(tag), Kills: kills, Deaths: deaths, MemberCount: 1}
}

func boardSvc(rows []repository.HubLeaderboardRow) (*Service, *fakeStore, *clock) {
	f := newFakeStore()
	f.setBoard(1, 10, 100, 200, rows)
	s, c := newSvc(f)
	return s, f, c
}

func ids(p *Leaderboard) []int64 {
	out := make([]int64, 0, len(p.Items))
	for _, e := range p.Items {
		out = append(out, e.Faction.FactionID)
	}
	return out
}

func get(t *testing.T, s *Service, q LeaderboardQuery) *Leaderboard {
	t.Helper()
	p, err := s.GetFactionLeaderboard(context.Background(), 1, 10, q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLeaderboardMetricVocabulary(t *testing.T) {
	want := []string{"KILLS", "DEATHS", "KD", "HEADSHOTS", "LONGSHOTS", "BEST_STREAK", "BOUNTIES_CLAIMED", "BOUNTY_VALUE", "ACHIEVEMENTS"}
	if len(LeaderboardMetrics) != len(want) {
		t.Fatalf("approved metrics: %v", LeaderboardMetrics)
	}
	for i, m := range LeaderboardMetrics {
		if string(m) != want[i] {
			t.Errorf("metric %d = %s, want %s", i, m, want[i])
		}
		if got, ok := ParseLeaderboardMetric(strings.ToLower(want[i])); !ok || got != m {
			t.Errorf("metric names are case-insensitive: %s", want[i])
		}
		wantDir := "DESC"
		if m == LeaderboardDeaths {
			wantDir = "ASC"
		}
		if m.Direction() != wantDir {
			t.Errorf("%s direction %s, want %s", m, m.Direction(), wantDir)
		}
		if m.IsDecimal() != (m == LeaderboardKD) {
			t.Errorf("only KD is decimal: %s", m)
		}
	}
	for _, bad := range []string{"", "OVERALL", "SKILL", "POWER", "kills;drop", "KILLS DESC", "kd_ratio", "Score", "combat", "1"} {
		if _, ok := ParseLeaderboardMetric(bad); ok {
			t.Errorf("metric %q must be rejected", bad)
		}
	}
	// No overall rating exists in any output type.
	for _, banned := range []string{"overall", "power", "skill", "combatscore", "rating"} {
		for _, typ := range []any{LeaderboardStats{}, LeaderboardEntry{}, Leaderboard{}} {
			if strings.Contains(strings.ToLower(fmt.Sprintf("%+v", typ)), banned) {
				t.Errorf("no %q field may exist", banned)
			}
		}
	}
}

func TestRankingEveryMetricAndDeterministicTies(t *testing.T) {
	mk := func(id int64, kills, deaths, hs, ls int64, streak int, bounties, value int64, ach int) repository.HubLeaderboardRow {
		r := row(id, fmt.Sprintf("Faction %d", id), fmt.Sprintf("F%d", id), kills, deaths)
		r.Headshots, r.Longshots, r.BestStreak, r.Bounties, r.BountyValue, r.Achievements = hs, ls, streak, bounties, value, ach
		return r
	}
	rows := []repository.HubLeaderboardRow{
		mk(1, 50, 10, 5, 1, 4, 2, 200, 3), // KD 5.00
		mk(2, 80, 40, 9, 0, 9, 0, 0, 1),   // KD 2.00
		mk(3, 80, 20, 2, 7, 4, 5, 90, 3),  // KD 4.00
		mk(4, 10, 1, 9, 7, 1, 5, 500, 0),  // KD 10.00
		mk(5, 0, 0, 0, 0, 0, 0, 0, 0),     // no tracked activity
		mk(6, 50, 10, 5, 1, 4, 2, 200, 3), // exact tie with faction 1 on every figure
	}
	s, _, _ := boardSvc(rows)
	cases := map[LeaderboardMetric][]int64{
		// kills DESC, deaths ASC, id ASC: 3 (80/20) before 2 (80/40); 1 before 6 (full tie -> id)
		LeaderboardKills: {3, 2, 1, 6, 4, 5},
		// fewer deaths first, activity before none: 4(1) 1(10) 6(10) 3(20) 2(40) then the empty faction 5; ties: kills DESC then id
		LeaderboardDeaths: {4, 1, 6, 3, 2, 5},
		// K/D as displayed: 4(10.00) 1(5.00) 6(5.00) 3(4.00) 2(2.00) 5(0)
		LeaderboardKD: {4, 1, 6, 3, 2, 5},
		// headshots: 2 and 4 have 9 -> kills DESC puts 2 (80) before 4 (10); then 1,6 (5); 3 (2)
		LeaderboardHeadshots: {2, 4, 1, 6, 3, 5},
		// longshots: 3 and 4 have 7 -> 3 (80 kills) first; then 1,6 (1); then 2 (0, 80 kills) before 5 (0, 0 kills)
		LeaderboardLongshots: {3, 4, 1, 6, 2, 5},
		// best streak: 2 (9); 1,3,6 have 4 -> kills DESC: 3 (80), then 1, 6 (50); 4 (1); 5 (0)
		LeaderboardBestStreak: {2, 3, 1, 6, 4, 5},
		// bounties claimed: 3 and 4 have 5 -> 3 (80 kills); 1,6 (2); then 2 (0, 80 kills), 5
		LeaderboardBountiesClaimed: {3, 4, 1, 6, 2, 5},
		// bounty value: 4 (500), 1/6 (200), 3 (90), 2 (0, 80 kills) , 5
		LeaderboardBountyValue: {4, 1, 6, 3, 2, 5},
		// achievements: 1,3,6 have 3 -> kills DESC: 3 (80) then 1 (50, id) 6; 2 (1); 4,5 (0) -> 4 (10 kills) before 5
		LeaderboardAchievements: {3, 1, 6, 2, 4, 5},
	}
	for metric, want := range cases {
		p := get(t, s, LeaderboardQuery{Metric: metric, Limit: 100})
		got := ids(p)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: got %v, want %v", metric, got, want)
		}
		for i, e := range p.Items {
			if e.Rank != i+1 {
				t.Errorf("%s: ranks are the ordinal position 1..n (ties are broken, never shared): %d at index %d", metric, e.Rank, i)
			}
		}
		if p.Direction != metric.Direction() || p.Metric != metric || p.Total != 6 {
			t.Errorf("%s: envelope %+v", metric, p)
		}
	}
	// Values have the right type and meaning per metric.
	byMetric := map[LeaderboardMetric]float64{LeaderboardKills: 80, LeaderboardKD: 10, LeaderboardBountyValue: 500}
	for m, want := range byMetric {
		p := get(t, s, LeaderboardQuery{Metric: m, Limit: 1})
		if m == LeaderboardKills && p.Items[0].Value != 80 || m == LeaderboardKD && p.Items[0].Value != 10 || m == LeaderboardBountyValue && p.Items[0].Value != want {
			t.Errorf("%s top value: %v", m, p.Items[0].Value)
		}
	}
	// A faction with no counted events is listed (never silently hidden) and flagged.
	p := get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Limit: 100})
	last := p.Items[len(p.Items)-1]
	if last.Faction.FactionID != 5 || last.HasTrackedActivity {
		t.Fatalf("the empty faction is listed last and flagged: %+v", last)
	}
	// Repeated calls order identically (no map or random ordering anywhere).
	for i := 0; i < 20; i++ {
		if fmt.Sprint(ids(get(t, s, LeaderboardQuery{Metric: LeaderboardKD, Limit: 100}))) != fmt.Sprint(cases[LeaderboardKD]) {
			t.Fatal("ordering must be deterministic")
		}
	}
}

func TestKDRanksByTheDisplayedValue(t *testing.T) {
	// 2/3 = 0.67 and 200/300 = 0.67 display identically; 7/10 = 0.70 is higher. Equal displayed values fall to kills DESC.
	rows := []repository.HubLeaderboardRow{row(1, "A", "A", 2, 3), row(2, "B", "B", 200, 300), row(3, "C", "C", 7, 10)}
	s, _, _ := boardSvc(rows)
	got := ids(get(t, s, LeaderboardQuery{Metric: LeaderboardKD, Limit: 10}))
	if fmt.Sprint(got) != "[3 2 1]" {
		t.Fatalf("K/D order: %v", got)
	}
	p := get(t, s, LeaderboardQuery{Metric: LeaderboardKD, Limit: 10})
	if p.Items[1].Value != 0.67 || p.Items[2].Value != 0.67 || p.Items[0].Value != 0.7 {
		t.Fatalf("K/D values are 2-decimal numbers: %v %v %v", p.Items[0].Value, p.Items[1].Value, p.Items[2].Value)
	}
}

func TestLeaderboardPaginationSearchAndRankStability(t *testing.T) {
	var rows []repository.HubLeaderboardRow
	for i := 1; i <= 23; i++ {
		rows = append(rows, row(int64(i), fmt.Sprintf("Wolf Pack %02d", i), fmt.Sprintf("W%02d", i), int64(i%7), int64(i%3))) // many ties
	}
	rows = append(rows, row(100, "Ravens", "RVN", 1, 1), row(101, "Alpha RAVEN Squad", "ARS", 3, 0))
	s, _, _ := boardSvc(rows)
	full := ids(get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Limit: 100}))
	if len(full) != 25 {
		t.Fatalf("all factions listed: %d", len(full))
	}
	for _, size := range []int{1, 2, 3, 7, 24, 25, 26} {
		var got []int64
		var cursor *LeaderboardCursor
		for pages := 0; ; pages++ {
			p := get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Limit: size, Cursor: cursor})
			if len(p.Items) > size || p.Limit != size || p.Total != 25 {
				t.Fatalf("page size %d: %d items limit=%d total=%d", size, len(p.Items), p.Limit, p.Total)
			}
			got = append(got, ids(p)...)
			if p.NextCursor == nil {
				break
			}
			c, ok := DecodeLeaderboardCursor(*p.NextCursor)
			if !ok || c.Metric != LeaderboardKills {
				t.Fatalf("cursor round trip: %v", ok)
			}
			cursor = c
			if pages > 60 {
				t.Fatal("pagination did not terminate")
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(full) {
			t.Fatalf("size %d: pages must reproduce the full ranking with no gap or repeat:\n got %v\nwant %v", size, got, full)
		}
	}
	// Exactly-full last page has no cursor.
	if p := get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Limit: 25}); p.NextCursor != nil || len(p.Items) != 25 {
		t.Fatalf("a page that ends the list has no cursor: %+v", p.NextCursor)
	}
	// Limits: default 25, max 100.
	if p := get(t, s, LeaderboardQuery{}); p.Limit != 25 || p.Metric != LeaderboardKills {
		t.Fatalf("defaults: %+v", p)
	}
	if p := get(t, s, LeaderboardQuery{Limit: 100000}); p.Limit != 100 {
		t.Fatalf("clamp: %d", p.Limit)
	}

	// Search: name or tag, case-insensitive; a faction's rank never changes with the filter.
	rank := map[int64]int{}
	for i, id := range full {
		rank[id] = i + 1
	}
	for _, c := range []struct {
		q    string
		want []int64
	}{{"raven", []int64{101, 100}}, {"RVN", []int64{100}}, {"ars", []int64{101}}, {"wolf pack 07", []int64{7}}, {"w2", []int64{20, 21, 22, 23}}, {"nothing here", nil}} {
		p := get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Search: c.q, Limit: 100})
		var got []int64
		for _, e := range p.Items {
			got = append(got, e.Faction.FactionID)
			if e.Rank != rank[e.Faction.FactionID] {
				t.Errorf("search %q changed the rank of %d: %d vs %d", c.q, e.Faction.FactionID, e.Rank, rank[e.Faction.FactionID])
			}
		}
		if len(c.want) > 0 && fmt.Sprint(sortInts(got)) != fmt.Sprint(sortInts(append([]int64(nil), c.want...))) {
			t.Errorf("search %q: got %v want %v", c.q, got, c.want)
		}
		if len(c.want) == 0 && (len(got) != 0 || p.Total != 0 || p.NextCursor != nil) {
			t.Errorf("search %q must find nothing: %v", c.q, got)
		}
		if p.Total != len(c.want) {
			t.Errorf("search %q total = %d, want %d", c.q, p.Total, len(c.want))
		}
	}
	// A search paged by 1 walks the matches exactly once.
	var walked []int64
	var cur *LeaderboardCursor
	for i := 0; i < 10; i++ {
		p := get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Search: "w2", Limit: 1, Cursor: cur})
		walked = append(walked, ids(p)...)
		if p.NextCursor == nil {
			break
		}
		cur, _ = DecodeLeaderboardCursor(*p.NextCursor)
	}
	if len(walked) != 4 {
		t.Fatalf("paged search: %v", walked)
	}
}

func sortInts(in []int64) []int64 {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	return in
}

func TestLeaderboardCursorRejection(t *testing.T) {
	good := encodeLeaderboardCursor(LeaderboardKD, [5]int64{-500, -10, 2, 7, 0})
	c, ok := DecodeLeaderboardCursor(good)
	if !ok || c.Metric != LeaderboardKD || c.Key != [5]int64{-500, -10, 2, 7, 0} {
		t.Fatalf("round trip: %+v %v", c, ok)
	}
	for _, bad := range []string{"", "x", "!!!", "bGIxOktJTExTOjE6MToxOjE6MQ" /* lb1:KILLS:1:1:1:1:1 is valid; mangle below */} {
		if bad == "bGIxOktJTExTOjE6MToxOjE6MQ" {
			continue
		}
		if _, ok := DecodeLeaderboardCursor(bad); ok {
			t.Errorf("cursor %q must be rejected", bad)
		}
	}
	for _, raw := range []string{"lb1:KILLS:1:2:3", "lb1:OVERALL:1:2:3:4:5", "lb1:kills:1:2:3:4:5", "lb1:KILLS:a:2:3:4:5", "lb2:KILLS:1:2:3:4:5", "fh1:1"} {
		if _, ok := DecodeLeaderboardCursor(b64(raw)); ok {
			t.Errorf("cursor %q must be rejected", raw)
		}
	}
}

func b64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	b := []byte(s)
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint(chunk[0])<<16 | uint(chunk[1])<<8 | uint(chunk[2])
		for j := 0; j < n+1; j++ {
			out = append(out, alphabet[(v>>(18-6*uint(j)))&63])
		}
	}
	return string(out)
}

func TestLeaderboardCacheHitExpiryAndInvalidation(t *testing.T) {
	rows := []repository.HubLeaderboardRow{row(1, "A", "A", 1, 0), row(2, "B", "B", 2, 0)}
	s, f, c := boardSvc(rows)
	q := LeaderboardQuery{Metric: LeaderboardKills}
	get(t, s, q)
	get(t, s, LeaderboardQuery{Metric: LeaderboardKD})
	get(t, s, LeaderboardQuery{Metric: LeaderboardKills, Search: "a", Limit: 1})
	if f.boardComputes.Load() != 1 {
		t.Fatalf("every metric, search and page is served from ONE cached table, computes=%d", f.boardComputes.Load())
	}
	c.add(44 * time.Second)
	get(t, s, q)
	if f.boardComputes.Load() != 1 {
		t.Fatal("still fresh at 44s")
	}
	c.add(2 * time.Second)
	get(t, s, q)
	if f.boardComputes.Load() != 2 {
		t.Fatalf("expires after the TTL: %d", f.boardComputes.Load())
	}
	// A kill, death or bounty claim on the installation's guild+server invalidates.
	base := f.boardComputes.Load()
	s.NotifyCombat(100, 200, 0)
	get(t, s, q)
	if f.boardComputes.Load() != base+1 {
		t.Fatalf("kill/death notification must invalidate: %d", f.boardComputes.Load())
	}
	// ...but another server's event does not.
	base = f.boardComputes.Load()
	s.NotifyCombat(100, 999, 5)
	s.NotifyCombat(999, 200, 5)
	get(t, s, q)
	if f.boardComputes.Load() != base {
		t.Fatal("an event elsewhere must not invalidate this installation's leaderboard")
	}
	// Membership / achievement / faction changes go through Invalidate(org, inst, faction).
	s.Invalidate(1, 10, 1)
	get(t, s, q)
	if f.boardComputes.Load() != base+1 {
		t.Fatal("Invalidate (membership change, unlock, creation, branding) must drop the leaderboard")
	}
	// Another installation's or organization's invalidation does not.
	base = f.boardComputes.Load()
	s.Invalidate(1, 11, 1)
	s.Invalidate(2, 10, 1)
	get(t, s, q)
	if f.boardComputes.Load() != base {
		t.Fatal("invalidation is per (organization, installation)")
	}
	s.InvalidateLeaderboard(1, 10)
	get(t, s, q)
	if f.boardComputes.Load() != base+1 {
		t.Fatal("explicit leaderboard invalidation")
	}
}

func TestLeaderboardCacheNeverCrossesTenants(t *testing.T) {
	f := newFakeStore()
	f.setBoard(1, 10, 100, 200, []repository.HubLeaderboardRow{row(1, "Mine", "M", 5, 0)})
	f.setBoard(2, 20, 101, 300, []repository.HubLeaderboardRow{row(2, "Theirs", "T", 9, 0)})
	s, _ := newSvc(f)
	ctx := context.Background()
	a, err := s.GetFactionLeaderboard(ctx, 1, 10, LeaderboardQuery{})
	if err != nil || len(a.Items) != 1 || a.Items[0].Faction.Name != "Mine" {
		t.Fatalf("tenant A: %+v %v", a, err)
	}
	b, err := s.GetFactionLeaderboard(ctx, 2, 20, LeaderboardQuery{})
	if err != nil || len(b.Items) != 1 || b.Items[0].Faction.Name != "Theirs" {
		t.Fatalf("tenant B: %+v %v", b, err)
	}
	// A warm cache must never answer a wrong organization/installation pairing.
	for _, bad := range [][2]int64{{2, 10}, {1, 20}, {9, 9}, {1, 11}} {
		if _, err := s.GetFactionLeaderboard(ctx, bad[0], bad[1], LeaderboardQuery{}); !errors.Is(err, factionhub.ErrNotFound) {
			t.Errorf("%v: want ErrNotFound, got %v", bad, err)
		}
	}
}

func TestLeaderboardSingleFlightAndEventDuringCompute(t *testing.T) {
	f := newFakeStore()
	f.setBoard(1, 10, 100, 200, []repository.HubLeaderboardRow{row(1, "A", "A", 1, 0)})
	s, _ := newSvc(f)
	gate := make(chan struct{})
	var computing sync.WaitGroup
	computing.Add(1)
	var once sync.Once
	f.onLeaderboard = func() {
		once.Do(computing.Done)
		<-gate
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.GetFactionLeaderboard(context.Background(), 1, 10, LeaderboardQuery{}); err != nil {
				t.Error(err)
			}
		}()
	}
	computing.Wait()
	time.Sleep(30 * time.Millisecond)
	close(gate)
	wg.Wait()
	if f.boardComputes.Load() != 1 {
		t.Fatalf("a cold burst is one computation, got %d", f.boardComputes.Load())
	}
	// An event arriving DURING a computation must leave that table stale.
	f2 := newFakeStore()
	f2.setBoard(1, 10, 100, 200, []repository.HubLeaderboardRow{row(1, "A", "A", 1, 0)})
	s2, _ := newSvc(f2)
	var fired sync.Once
	f2.onLeaderboard = func() { fired.Do(func() { s2.NotifyCombat(100, 200, 0) }) }
	_, _ = s2.GetFactionLeaderboard(context.Background(), 1, 10, LeaderboardQuery{})
	_, _ = s2.GetFactionLeaderboard(context.Background(), 1, 10, LeaderboardQuery{})
	if f2.boardComputes.Load() != 2 {
		t.Fatalf("the table computed across an invalidating event is stale and must be recomputed: %d", f2.boardComputes.Load())
	}
}

// Readers rank and page while kills, membership changes and reconciliation invalidate the cache.
func TestLeaderboardConcurrentReadsDuringUpdatesAreRaceFree(t *testing.T) {
	f := newFakeStore()
	var rows []repository.HubLeaderboardRow
	for i := 1; i <= 60; i++ {
		rows = append(rows, row(int64(i), fmt.Sprintf("Faction %d", i), fmt.Sprintf("F%d", i), int64(i%9), int64(i%4)))
	}
	f.setBoard(1, 10, 100, 200, rows)
	s, c := newSvc(f)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			m := LeaderboardMetrics[i%len(LeaderboardMetrics)]
			for j := 0; j < 150; j++ {
				var cur *LeaderboardCursor
				for k := 0; k < 3; k++ {
					p, err := s.GetFactionLeaderboard(ctx, 1, 10, LeaderboardQuery{Metric: m, Limit: 7, Cursor: cur, Search: []string{"", "fac", "F1"}[j%3]})
					if err != nil {
						t.Error(err)
						return
					}
					if p.NextCursor == nil {
						break
					}
					cur, _ = DecodeLeaderboardCursor(*p.NextCursor)
				}
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 300; j++ {
				s.NotifyCombat(100, 200, int64(j%5))
				s.Invalidate(1, 10, int64(j%3))
				s.InvalidateLeaderboard(1, 10)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.add(time.Second)
				s.ProcessPending(ctx)
			}
		}()
	}
	wg.Wait()
	cancel()
}
