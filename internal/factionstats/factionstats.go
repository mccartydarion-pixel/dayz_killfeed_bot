// Package factionstats is the Faction Hub's competitive layer (docs/FACTION_STATS.md): faction
// statistics, member contributions, achievements and public activity, all DERIVED from Champion's
// authoritative runtime data (kills, deaths, bounties, server records) joined to membership
// periods. Nothing is stored twice and nothing is invented: a figure the data cannot prove is
// zero or absent, and a member without a verified DayZ identity is reported as UNLINKED, never
// guessed from a name.
//
// HTTP handlers only call this package; they contain no statistics logic.
package factionstats

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// DefaultCacheTTL is how long a computed faction's stats are reused. Kills, deaths, bounty claims
// and membership changes invalidate sooner (see NotifyCombat / Invalidate).
const DefaultCacheTTL = 45 * time.Second

// Identity states for a member.
const (
	IdentityLinked   = "LINKED"   // a verified player_links row: figures are attributable
	IdentityUnlinked = "UNLINKED" // no verified link: nothing is attributed and nothing is guessed
)

// Member statuses.
const (
	StatusActive = "ACTIVE"
	StatusFormer = "FORMER" // left, but counted events from their membership period remain in the totals
)

// Summary is the faction-level block. Every field is a real, attributable figure. The shape is
// deliberately flat and comparable so a future faction leaderboard can rank on kills, kdRatio,
// headshots, longshots, bountiesClaimed and achievementsUnlocked without another query; no
// overall "skill score" exists.
type Summary struct {
	Kills              int64   `json:"kills"`
	Deaths             int64   `json:"deaths"`
	KDRatio            float64 `json:"kdRatio"`
	Headshots          int64   `json:"headshots"`
	Longshots          int64   `json:"longshots"`
	CurrentKillStreak  int     `json:"currentKillStreak"`
	BestKillStreak     int     `json:"bestKillStreak"`
	BountiesClaimed    int64   `json:"bountiesClaimed"`
	BountyValueClaimed int64   `json:"bountyValueClaimed"`
	MemberCount        int     `json:"memberCount"`
	// LinkedMemberCount are the members whose figures can be attributed (verified identity).
	LinkedMemberCount    int `json:"linkedMemberCount"`
	AchievementsUnlocked int `json:"achievementsUnlocked"`
	// TrackingSince is when the faction's first recorded membership period began: figures cover
	// events from then on (members who left before history existed cannot be reconstructed).
	TrackingSince *string `json:"trackingSince"`
}

// MemberContribution is one member's (or former member's) share of the faction's figures.
type MemberContribution struct {
	MemberID           *int64  `json:"memberId"` // current membership id; null for a former member
	DiscordUserID      string  `json:"discordUserId"`
	DisplayName        string  `json:"displayName"`
	Avatar             string  `json:"avatar,omitempty"`
	Gamertag           *string `json:"gamertag"`
	Role               *string `json:"role"` // null for a former member
	Status             string  `json:"status"`
	Identity           string  `json:"identity"`
	StatsEligible      bool    `json:"statsEligible"`
	JoinedAt           string  `json:"joinedAt"`
	Kills              int64   `json:"kills"`
	Deaths             int64   `json:"deaths"`
	KDRatio            float64 `json:"kdRatio"`
	Headshots          int64   `json:"headshots"`
	Longshots          int64   `json:"longshots"`
	CurrentKillStreak  int     `json:"currentKillStreak"`
	BestKillStreak     int     `json:"bestKillStreak"`
	BountiesClaimed    int64   `json:"bountiesClaimed"`
	BountyValueClaimed int64   `json:"bountyValueClaimed"`
}

// Stats is the dedicated stats response.
type Stats struct {
	Summary             Summary              `json:"summary"`
	MemberContributions []MemberContribution `json:"memberContributions"`
	UpdatedAt           string               `json:"updatedAt"`
}

// KDRatio is kills per death, rounded to two decimals, following the project's existing
// convention (StatsRepository: kills / max(deaths, 1)): with zero deaths the ratio is the kill
// count itself - never a division by zero, never infinity.
func KDRatio(kills, deaths int64) float64 {
	d := deaths
	if d < 1 {
		d = 1
	}
	return math.Round(float64(kills)/float64(d)*100) / 100
}

// Store is the persistence the service needs (*repository.HubStatsRepository).
type Store interface {
	Scope(ctx context.Context, organizationID, installationID, factionID int64) (repository.HubStatsScope, error)
	ListScopes(ctx context.Context, afterID int64, limit int) ([]repository.HubStatsScope, error)
	ScopesForPlayers(ctx context.Context, guildID, serverID int64, playerIDs []int64) ([]repository.HubStatsScope, error)
	ComputeStats(ctx context.Context, s repository.HubStatsScope) (*repository.HubStatsResult, error)
	Unlocks(ctx context.Context, factionID int64) (map[string]repository.HubUnlock, error)
	InsertUnlock(ctx context.Context, s repository.HubStatsScope, key string, at time.Time, metadata map[string]any) (bool, error)
	NthEventTime(ctx context.Context, s repository.HubStatsScope, metric string, n int) (*time.Time, error)
	Activity(ctx context.Context, s repository.HubStatsScope, limit int, cursor *repository.HubActivityCursor) ([]repository.HubActivityRow, bool, error)
	// LeaderboardScope / Leaderboard serve the faction leaderboards (leaderboard.go): the installation's
	// guild and server, and every faction of the installation with all metrics in one statement.
	LeaderboardScope(ctx context.Context, organizationID, installationID int64) (guildID, serverID int64, err error)
	Leaderboard(ctx context.Context, organizationID, installationID int64) (*repository.HubLeaderboardData, error)
}

// Options configures a Service.
type Options struct {
	CacheTTL time.Duration
	Now      func() time.Time
}

// Service computes and caches faction statistics, evaluates achievements and serves activity.
type Service struct {
	store Store
	ttl   time.Duration
	now   func() time.Time

	mu       sync.Mutex
	cache    map[cacheKey]cacheEntry
	gens     map[genKey]uint64
	inflight map[cacheKey]*call

	// Leaderboard cache: one table of all factions per (organization, installation).
	lbCache    map[lbKey]*leaderboardTable
	lbInflight map[lbKey]*lbCall

	pendMu  sync.Mutex
	pending map[genKey]map[int64]struct{} // (guild, server) -> killer players awaiting evaluation
}

type cacheKey struct{ org, inst, faction int64 }
type genKey struct{ guild, server int64 }
type cacheEntry struct {
	stats *Stats
	scope repository.HubStatsScope
	at    time.Time
	gen   uint64
}
type call struct {
	wg    sync.WaitGroup
	stats *Stats
	err   error
}

func NewService(store Store, opt Options) *Service {
	if opt.CacheTTL <= 0 {
		opt.CacheTTL = DefaultCacheTTL
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	return &Service{store: store, ttl: opt.CacheTTL, now: opt.Now, cache: map[cacheKey]cacheEntry{}, gens: map[genKey]uint64{},
		inflight: map[cacheKey]*call{}, lbCache: map[lbKey]*leaderboardTable{}, lbInflight: map[lbKey]*lbCall{}, pending: map[genKey]map[int64]struct{}{}}
}

const maxCacheEntries = 2048

// GetFactionStats returns the faction's summary and per-member contributions (one grouped query,
// cached for the TTL and invalidated by relevant events). The faction must exist within the
// organization and installation (repository.factionhub.ErrNotFound otherwise) - the cache key
// includes both, so one tenant can never be served another's entry.
func (s *Service) GetFactionStats(ctx context.Context, organizationID, installationID, factionID int64) (*Stats, error) {
	key := cacheKey{organizationID, installationID, factionID}
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.fresh(e) {
		s.mu.Unlock()
		return e.stats, nil
	}
	if c, ok := s.inflight[key]; ok { // single-flight: a cold burst is one computation
		s.mu.Unlock()
		c.wg.Wait()
		return c.stats, c.err
	}
	c := &call{}
	c.wg.Add(1)
	s.inflight[key] = c
	s.mu.Unlock()

	scope, err := s.store.Scope(ctx, organizationID, installationID, factionID)
	var st *Stats
	if err == nil {
		st, err = s.computeAndStore(ctx, key, scope)
	}
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	c.stats, c.err = st, err
	c.wg.Done()
	return st, err
}

// GetFactionMemberStats returns just the per-member contributions (ordered kills DESC, deaths
// ASC, member id).
func (s *Service) GetFactionMemberStats(ctx context.Context, organizationID, installationID, factionID int64) ([]MemberContribution, error) {
	st, err := s.GetFactionStats(ctx, organizationID, installationID, factionID)
	if err != nil {
		return nil, err
	}
	return st.MemberContributions, nil
}

func (s *Service) fresh(e cacheEntry) bool {
	return s.now().Sub(e.at) < s.ttl && e.gen == s.gens[genKey{e.scope.GuildID, e.scope.ServerID}]
}

// computeAndStore always hits the database (achievement evaluation needs current figures) and
// refreshes the cache. The generation is read BEFORE computing, so an event that lands during the
// computation invalidates the entry it produced instead of being masked by it.
func (s *Service) computeAndStore(ctx context.Context, key cacheKey, scope repository.HubStatsScope) (*Stats, error) {
	s.mu.Lock()
	gen := s.gens[genKey{scope.GuildID, scope.ServerID}]
	s.mu.Unlock()
	raw, err := s.store.ComputeStats(ctx, scope)
	if err != nil {
		return nil, err
	}
	unlocks, err := s.store.Unlocks(ctx, scope.FactionID)
	if err != nil {
		return nil, err
	}
	st := buildStats(raw, len(unlocks), s.now())
	s.mu.Lock()
	if len(s.cache) >= maxCacheEntries {
		s.cache = map[cacheKey]cacheEntry{} // bounded: drop everything rather than track LRU
	}
	s.cache[key] = cacheEntry{stats: st, scope: scope, at: s.now(), gen: gen}
	s.mu.Unlock()
	return st, nil
}

func buildStats(raw *repository.HubStatsResult, unlocked int, now time.Time) *Stats {
	sum := Summary{MemberCount: raw.MemberCount, LinkedMemberCount: raw.LinkedCount, AchievementsUnlocked: unlocked}
	if raw.TrackingSince != nil {
		t := raw.TrackingSince.UTC().Format(time.RFC3339)
		sum.TrackingSince = &t
	}
	members := make([]MemberContribution, 0, len(raw.Members))
	for _, m := range raw.Members {
		mc := MemberContribution{MemberID: m.MemberID, DiscordUserID: m.DiscordUserID, DisplayName: displayName(m.GlobalName, m.Username), Avatar: m.Avatar,
			Gamertag: m.Gamertag, Role: m.Role, Status: StatusFormer, Identity: IdentityUnlinked, JoinedAt: m.JoinedAt.UTC().Format(time.RFC3339)}
		if m.Active {
			mc.Status = StatusActive
		}
		if m.Linked {
			mc.Identity, mc.StatsEligible = IdentityLinked, true
			mc.Kills, mc.Deaths, mc.Headshots, mc.Longshots = m.Kills, m.Deaths, m.Headshots, m.Longshots
			mc.BountiesClaimed, mc.BountyValueClaimed = m.Bounties, m.BountyValue
			mc.BestKillStreak, mc.CurrentKillStreak = m.BestStreak, m.CurrentStreak
			mc.KDRatio = KDRatio(m.Kills, m.Deaths)
			sum.Kills += m.Kills
			sum.Deaths += m.Deaths
			sum.Headshots += m.Headshots
			sum.Longshots += m.Longshots
			sum.BountiesClaimed += m.Bounties
			sum.BountyValueClaimed += m.BountyValue
			if m.BestStreak > sum.BestKillStreak {
				sum.BestKillStreak = m.BestStreak
			}
			if m.Active && m.CurrentStreak > sum.CurrentKillStreak {
				sum.CurrentKillStreak = m.CurrentStreak
			}
		}
		members = append(members, mc)
	}
	sum.KDRatio = KDRatio(sum.Kills, sum.Deaths)
	return &Stats{Summary: sum, MemberContributions: members, UpdatedAt: now.UTC().Format(time.RFC3339)}
}

func displayName(global, username string) string {
	if global != "" {
		return global
	}
	return username
}

// Invalidate drops a faction's cached stats (membership or role change, achievement unlock).
func (s *Service) Invalidate(organizationID, installationID, factionID int64) {
	s.mu.Lock()
	delete(s.cache, cacheKey{organizationID, installationID, factionID})
	// Every faction-level change (membership, role, branding, a new faction, an achievement unlock) can
	// move a leaderboard, so the installation's leaderboard goes with it.
	delete(s.lbCache, lbKey{organizationID, installationID})
	s.mu.Unlock()
}

// NotifyCombat is called after a kill (killerPlayerID > 0), a death, or a bounty claim was durably
// persisted on (guild, server). It invalidates every cached faction of that guild+server (one
// counter bump - cheap, and exact: only those factions' figures can have changed) and, for a kill,
// queues the killer for an asynchronous achievement evaluation. It never blocks the kill path.
func (s *Service) NotifyCombat(guildID, serverID, killerPlayerID int64) {
	if s == nil {
		return
	}
	g := genKey{guildID, serverID}
	s.mu.Lock()
	s.gens[g]++
	s.mu.Unlock()
	if killerPlayerID <= 0 {
		return
	}
	s.pendMu.Lock()
	if s.pending[g] == nil {
		s.pending[g] = map[int64]struct{}{}
	}
	if len(s.pending[g]) < 10000 { // bounded queue: a flood is folded into the next reconcile
		s.pending[g][killerPlayerID] = struct{}{}
	}
	s.pendMu.Unlock()
}

// Cursor encoding: opaque base64 of "fa1:<unix micro>:<source>:<id>".
const cursorPrefix = "fa1:"

func encodeCursor(at time.Time, src int, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s%d:%d:%d", cursorPrefix, at.UTC().UnixMicro(), src, id)))
}

// DecodeCursor parses a cursor produced by an earlier page; ok is false for anything else.
func DecodeCursor(s string) (*repository.HubActivityCursor, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), cursorPrefix) {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(string(raw), cursorPrefix), ":")
	if len(parts) != 3 {
		return nil, false
	}
	micro, err1 := strconv.ParseInt(parts[0], 10, 64)
	src, err2 := strconv.Atoi(parts[1])
	id, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || src < 1 || src > repository.ActivitySrcUnlock || id < 0 {
		return nil, false
	}
	return &repository.HubActivityCursor{At: time.UnixMicro(micro).UTC(), Src: src, ID: id}, true
}

func warn(msg string, err error, attrs ...any) {
	slog.Warn("component=faction_stats", append([]any{"msg", msg, "err", err.Error()}, attrs...)...)
}
