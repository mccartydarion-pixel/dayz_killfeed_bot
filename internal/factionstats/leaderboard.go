package factionstats

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction leaderboards (docs/FACTION_LEADERBOARDS.md): one explicit metric per leaderboard, scoped to a
// single installation (and therefore its one DayZ server). There is deliberately NO overall rating, power
// score or weighting: a leaderboard ranks exactly the metric it names, using the same figures the faction
// profile shows - both come from ONE SQL definition (repository.hubStatsCTE), so they cannot disagree.

// LeaderboardMetric is a typed, approved leaderboard metric.
type LeaderboardMetric string

const (
	LeaderboardKills           LeaderboardMetric = "KILLS"
	LeaderboardDeaths          LeaderboardMetric = "DEATHS"
	LeaderboardKD              LeaderboardMetric = "KD"
	LeaderboardHeadshots       LeaderboardMetric = "HEADSHOTS"
	LeaderboardLongshots       LeaderboardMetric = "LONGSHOTS"
	LeaderboardBestStreak      LeaderboardMetric = "BEST_STREAK"
	LeaderboardBountiesClaimed LeaderboardMetric = "BOUNTIES_CLAIMED"
	LeaderboardBountyValue     LeaderboardMetric = "BOUNTY_VALUE"
	LeaderboardAchievements    LeaderboardMetric = "ACHIEVEMENTS"
)

// LeaderboardMetrics is the stable, ordered list of approved metrics.
var LeaderboardMetrics = []LeaderboardMetric{
	LeaderboardKills, LeaderboardDeaths, LeaderboardKD, LeaderboardHeadshots, LeaderboardLongshots,
	LeaderboardBestStreak, LeaderboardBountiesClaimed, LeaderboardBountyValue, LeaderboardAchievements,
}

// DefaultLeaderboardMetric is used when the request names none.
const DefaultLeaderboardMetric = LeaderboardKills

// ParseLeaderboardMetric accepts exactly the approved metric names (case-insensitively); anything else
// is rejected, never mapped to a default.
func ParseLeaderboardMetric(raw string) (LeaderboardMetric, bool) {
	s := strings.ToUpper(strings.TrimSpace(raw))
	for _, m := range LeaderboardMetrics {
		if string(m) == s {
			return m, true
		}
	}
	return "", false
}

// Direction is DESC for every competitive metric ("more is better") and ASC for DEATHS ("fewer is
// better"): rank 1 has the fewest deaths, and factions with no tracked activity always come last so an
// empty faction is never "best" at deaths.
func (m LeaderboardMetric) Direction() string {
	if m == LeaderboardDeaths {
		return "ASC"
	}
	return "DESC"
}

// IsDecimal reports whether the metric's value is a decimal (KD); every other metric is an integer.
func (m LeaderboardMetric) IsDecimal() bool { return m == LeaderboardKD }

// Leaderboard limits (the project's list convention).
const (
	DefaultLeaderboardLimit = 25
	MaxLeaderboardLimit     = 100
)

// LeaderboardStats are one faction's figures for every metric, named exactly like the faction profile's summary.
type LeaderboardStats struct {
	Kills                int64   `json:"kills"`
	Deaths               int64   `json:"deaths"`
	KDRatio              float64 `json:"kdRatio"`
	Headshots            int64   `json:"headshots"`
	Longshots            int64   `json:"longshots"`
	BestKillStreak       int     `json:"bestKillStreak"`
	BountiesClaimed      int64   `json:"bountiesClaimed"`
	BountyValueClaimed   int64   `json:"bountyValueClaimed"`
	AchievementsUnlocked int     `json:"achievementsUnlocked"`
}

// LeaderboardEntry is one ranked faction.
type LeaderboardEntry struct {
	Rank    int // the global ordinal position on this installation (1 = first), unaffected by search or paging
	Faction repository.HubLeaderboardRow
	// Value is the ranked metric: an integer for every metric except KD (a decimal).
	Value float64
	Stats LeaderboardStats
	// HasTrackedActivity is false for a faction with no counted kill, death or bounty (a new faction, or one
	// whose members have no verified link yet): it is listed, with zero figures, and the UI can say so.
	HasTrackedActivity bool
	key                [5]int64
}

// Leaderboard is one page.
type Leaderboard struct {
	Metric     LeaderboardMetric
	Direction  string
	Items      []LeaderboardEntry
	NextCursor *string
	Limit      int
	// Total is the number of factions matching the search (all factions when there is none).
	Total     int
	UpdatedAt string
}

// LeaderboardQuery selects a page. Zero Limit means the default.
type LeaderboardQuery struct {
	Metric LeaderboardMetric
	Search string // already normalized; matched case-insensitively against name and tag
	Limit  int
	Cursor *LeaderboardCursor
}

// LeaderboardCursor is the sort key of the last entry of the previous page (keyset pagination).
type LeaderboardCursor struct {
	Metric LeaderboardMetric
	Key    [5]int64
}

const lbCursorPrefix = "lb1:"

func encodeLeaderboardCursor(m LeaderboardMetric, key [5]int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s%s:%d:%d:%d:%d:%d", lbCursorPrefix, m, key[0], key[1], key[2], key[3], key[4])))
}

// DecodeLeaderboardCursor parses a cursor produced by an earlier page; ok is false for anything else.
func DecodeLeaderboardCursor(s string) (*LeaderboardCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), lbCursorPrefix) {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(string(raw), lbCursorPrefix), ":")
	if len(parts) != 6 {
		return nil, false
	}
	m, ok := ParseLeaderboardMetric(parts[0])
	if !ok || string(m) != parts[0] {
		return nil, false
	}
	var key [5]int64
	for i := range key {
		v, err := strconv.ParseInt(parts[i+1], 10, 64)
		if err != nil {
			return nil, false
		}
		key[i] = v
	}
	return &LeaderboardCursor{Metric: m, Key: key}, true
}

func statsOf(r repository.HubLeaderboardRow) LeaderboardStats {
	return LeaderboardStats{Kills: r.Kills, Deaths: r.Deaths, KDRatio: KDRatio(r.Kills, r.Deaths), Headshots: r.Headshots, Longshots: r.Longshots,
		BestKillStreak: r.BestStreak, BountiesClaimed: r.Bounties, BountyValueClaimed: r.BountyValue, AchievementsUnlocked: r.Achievements}
}

func hasActivity(r repository.HubLeaderboardRow) bool { return r.Kills+r.Deaths+r.Bounties > 0 }

// leaderboardKey returns the ascending sort key for a metric (lexicographic). Tie-breaking is fully
// deterministic and ends in the faction id:
//
//	KILLS                          kills DESC, deaths ASC, id ASC
//	DEATHS (ASC: fewer is better)  no-activity last, deaths ASC, kills DESC, id ASC
//	KD                             kdRatio DESC (as displayed, 2 decimals), kills DESC, deaths ASC, id ASC
//	every other metric             metric DESC, kills DESC, deaths ASC, id ASC
func leaderboardKey(m LeaderboardMetric, r repository.HubLeaderboardRow) [5]int64 {
	switch m {
	case LeaderboardKills:
		return [5]int64{-r.Kills, r.Deaths, r.FactionID, 0, 0}
	case LeaderboardDeaths:
		noActivity := int64(0)
		if !hasActivity(r) {
			noActivity = 1
		}
		return [5]int64{noActivity, r.Deaths, -r.Kills, r.FactionID, 0}
	case LeaderboardKD:
		return [5]int64{-kdHundredths(r), -r.Kills, r.Deaths, r.FactionID, 0}
	}
	return [5]int64{-primaryInt(m, r), -r.Kills, r.Deaths, r.FactionID, 0}
}

func kdHundredths(r repository.HubLeaderboardRow) int64 {
	d := r.Deaths
	if d < 1 {
		d = 1
	}
	return int64(math.Round(float64(r.Kills) / float64(d) * 100))
}

func primaryInt(m LeaderboardMetric, r repository.HubLeaderboardRow) int64 {
	switch m {
	case LeaderboardHeadshots:
		return r.Headshots
	case LeaderboardLongshots:
		return r.Longshots
	case LeaderboardBestStreak:
		return int64(r.BestStreak)
	case LeaderboardBountiesClaimed:
		return r.Bounties
	case LeaderboardBountyValue:
		return r.BountyValue
	case LeaderboardAchievements:
		return int64(r.Achievements)
	case LeaderboardDeaths:
		return r.Deaths
	}
	return r.Kills
}

func lessKey(a, b [5]int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// leaderboardTable is one installation's factions with all figures, cached for the TTL. The ranked order
// of each metric is memoized inside the entry, so the cache is effectively keyed by
// (organization, installation, metric) while one database statement serves every metric.
type leaderboardTable struct {
	data          *repository.HubLeaderboardData
	guild, server int64
	at            time.Time
	gen           uint64
	stamp         string

	mu     sync.Mutex
	ranked map[LeaderboardMetric][]LeaderboardEntry
}

func (t *leaderboardTable) rank(m LeaderboardMetric) []LeaderboardEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if r, ok := t.ranked[m]; ok {
		return r
	}
	out := make([]LeaderboardEntry, len(t.data.Rows))
	for i, row := range t.data.Rows {
		v := float64(primaryInt(m, row))
		if m == LeaderboardKD {
			v = KDRatio(row.Kills, row.Deaths)
		}
		out[i] = LeaderboardEntry{Faction: row, Value: v, Stats: statsOf(row), HasTrackedActivity: hasActivity(row), key: leaderboardKey(m, row)}
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].key, out[j].key) })
	for i := range out {
		out[i].Rank = i + 1
	}
	if t.ranked == nil {
		t.ranked = map[LeaderboardMetric][]LeaderboardEntry{}
	}
	t.ranked[m] = out
	return out
}

type lbKey struct{ org, inst int64 }
type lbCall struct {
	wg    sync.WaitGroup
	table *leaderboardTable
	err   error
}

func (s *Service) lbFresh(t *leaderboardTable) bool {
	return s.now().Sub(t.at) < s.ttl && t.gen == s.gens[genKey{t.guild, t.server}]
}

// leaderboardTable returns the installation's cached table, computing it once per TTL (single-flight).
func (s *Service) leaderboardTable(ctx context.Context, organizationID, installationID int64) (*leaderboardTable, error) {
	key := lbKey{organizationID, installationID}
	s.mu.Lock()
	if t, ok := s.lbCache[key]; ok && s.lbFresh(t) {
		s.mu.Unlock()
		return t, nil
	}
	if c, ok := s.lbInflight[key]; ok {
		s.mu.Unlock()
		c.wg.Wait()
		return c.table, c.err
	}
	c := &lbCall{}
	c.wg.Add(1)
	s.lbInflight[key] = c
	s.mu.Unlock()

	// The generation is read BEFORE the figures are computed, so an event that lands during the
	// computation leaves the resulting table stale instead of being masked by it.
	guild, server, err := s.store.LeaderboardScope(ctx, organizationID, installationID)
	var table *leaderboardTable
	if err == nil {
		s.mu.Lock()
		gen := s.gens[genKey{guild, server}]
		s.mu.Unlock()
		var data *repository.HubLeaderboardData
		if data, err = s.store.Leaderboard(ctx, organizationID, installationID); err == nil {
			s.mu.Lock()
			table = &leaderboardTable{data: data, guild: guild, server: server, at: s.now(), gen: gen, stamp: s.now().UTC().Format(time.RFC3339)}
			if len(s.lbCache) >= maxCacheEntries {
				s.lbCache = map[lbKey]*leaderboardTable{}
			}
			s.lbCache[key] = table
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	delete(s.lbInflight, key)
	s.mu.Unlock()
	c.table, c.err = table, err
	c.wg.Done()
	return table, err
}

// GetFactionLeaderboard returns one page of the installation's faction leaderboard for a single metric.
// The installation must belong to the organization (factionhub.ErrNotFound otherwise); the cache is keyed
// by organization + installation, so one tenant is never served another's table. Search matches faction
// name or tag (case-insensitive) and never changes a faction's rank; paging is keyset over the sort key.
func (s *Service) GetFactionLeaderboard(ctx context.Context, organizationID, installationID int64, q LeaderboardQuery) (*Leaderboard, error) {
	if q.Metric == "" {
		q.Metric = DefaultLeaderboardMetric
	}
	if q.Limit <= 0 {
		q.Limit = DefaultLeaderboardLimit
	}
	if q.Limit > MaxLeaderboardLimit {
		q.Limit = MaxLeaderboardLimit
	}
	table, err := s.leaderboardTable(ctx, organizationID, installationID)
	if err != nil {
		return nil, err
	}
	ranked := table.rank(q.Metric)
	needle := strings.ToLower(strings.TrimSpace(q.Search))
	match := func(e *LeaderboardEntry) bool {
		return needle == "" || strings.Contains(strings.ToLower(e.Faction.Name), needle) || strings.Contains(strings.ToLower(e.Faction.Tag), needle)
	}
	total := 0
	for i := range ranked {
		if match(&ranked[i]) {
			total++
		}
	}
	start := 0
	if q.Cursor != nil {
		start = sort.Search(len(ranked), func(i int) bool { return lessKey(q.Cursor.Key, ranked[i].key) })
	}
	page := &Leaderboard{Metric: q.Metric, Direction: q.Metric.Direction(), Items: make([]LeaderboardEntry, 0, q.Limit), Limit: q.Limit, Total: total, UpdatedAt: table.stamp}
	more := false
	for i := start; i < len(ranked); i++ {
		if !match(&ranked[i]) {
			continue
		}
		if len(page.Items) == q.Limit {
			more = true
			break
		}
		page.Items = append(page.Items, ranked[i])
	}
	if more && len(page.Items) > 0 {
		c := encodeLeaderboardCursor(q.Metric, page.Items[len(page.Items)-1].key)
		page.NextCursor = &c
	}
	return page, nil
}

// InvalidateLeaderboard drops an installation's cached leaderboard (a membership change, a new faction, a
// branding change, an achievement unlock). Kills, deaths and bounty claims invalidate through NotifyCombat.
func (s *Service) InvalidateLeaderboard(organizationID, installationID int64) {
	s.mu.Lock()
	delete(s.lbCache, lbKey{organizationID, installationID})
	s.mu.Unlock()
}
