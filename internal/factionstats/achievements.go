package factionstats

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Metric names the measurable figure an achievement tracks.
type Metric string

const (
	MetricKills      Metric = "kills"
	MetricHeadshots  Metric = "headshots"
	MetricLongshots  Metric = "longshots"
	MetricBounties   Metric = "bounties"
	MetricBestStreak Metric = "bestKillStreak"
	MetricMembers    Metric = "members"
	MetricAgeDays    Metric = "ageDays"
)

// Definition is one system-defined faction achievement. Definitions live in code (never
// user-editable); only the unlock records are stored. Every condition is provable from existing
// data: counted kills, headshot and longshot flags on those kills (a longshot is the killfeed's own
// >= 100 m flag), claimed bounty rows, the attributed streak, the live membership count and the
// faction's creation time.
type Definition struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Metric      Metric `json:"metric"`
	Target      int    `json:"target"`
	// Unit labels progress for display ("kills", "members", "days").
	Unit string `json:"unit"`
}

// Definitions is the stable, ordered achievement catalog. Keys are the public contract.
var Definitions = []Definition{
	{"FIRST_BLOOD", "First Blood", "Land the faction's first kill.", MetricKills, 1, "kills"},
	{"KILLS_100", "Centurions", "Reach 100 faction kills.", MetricKills, 100, "kills"},
	{"KILLS_500", "Warband", "Reach 500 faction kills.", MetricKills, 500, "kills"},
	{"KILLS_1000", "Legion", "Reach 1,000 faction kills.", MetricKills, 1000, "kills"},
	{"HEADHUNTERS", "Headhunters", "Land 25 headshot kills.", MetricHeadshots, 25, "headshots"},
	{"LONG_RANGE", "Long Range", "Land 10 longshot kills.", MetricLongshots, 10, "longshots"},
	{"BOUNTY_HUNTERS", "Bounty Hunters", "Claim 5 bounties.", MetricBounties, 5, "bounties"},
	{"KILLING_MACHINE", "Killing Machine", "Have a member reach a 10 kill streak while in the faction.", MetricBestStreak, 10, "kills"},
	{"FULL_SQUAD", "Full Squad", "Have 5 members at once.", MetricMembers, 5, "members"},
	{"VETERAN_FACTION", "Veteran Faction", "Stay together for 30 days.", MetricAgeDays, 30, "days"},
}

// DefinitionByKey returns a definition, ok=false for an unknown key.
func DefinitionByKey(key string) (Definition, bool) {
	for _, d := range Definitions {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}

// metricValue reads the current value of a metric.
func metricValue(m Metric, sum Summary, scope repository.HubStatsScope, now time.Time) int {
	switch m {
	case MetricKills:
		return int(sum.Kills)
	case MetricHeadshots:
		return int(sum.Headshots)
	case MetricLongshots:
		return int(sum.Longshots)
	case MetricBounties:
		return int(sum.BountiesClaimed)
	case MetricBestStreak:
		return sum.BestKillStreak
	case MetricMembers:
		return sum.MemberCount
	case MetricAgeDays:
		if d := now.Sub(scope.CreatedAt); d > 0 {
			return int(d / (24 * time.Hour))
		}
	}
	return 0
}

// Achievement is the public state of one achievement for one faction.
type Achievement struct {
	Key         string  `json:"key"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Unlocked    bool    `json:"unlocked"`
	UnlockedAt  *string `json:"unlockedAt"`
	// Progress / Target are exposed for every current achievement because each one has a measurable
	// value; Progress is capped at Target. Unit labels them.
	Progress int    `json:"progress"`
	Target   int    `json:"target"`
	Unit     string `json:"unit"`
}

// GetFactionAchievements returns every definition with its unlocked state and progress. If the
// figures already prove an achievement that has not been recorded (a faction that qualified
// before the evaluator ran), it is evaluated first so the response is never stale.
func (s *Service) GetFactionAchievements(ctx context.Context, organizationID, installationID, factionID int64) ([]Achievement, error) {
	st, err := s.GetFactionStats(ctx, organizationID, installationID, factionID)
	if err != nil {
		return nil, err
	}
	scope, err := s.store.Scope(ctx, organizationID, installationID, factionID)
	if err != nil {
		return nil, err
	}
	unlocks, err := s.store.Unlocks(ctx, factionID)
	if err != nil {
		return nil, err
	}
	pending := false
	for _, d := range Definitions {
		if _, done := unlocks[d.Key]; !done && metricValue(d.Metric, st.Summary, scope, s.now()) >= d.Target {
			pending = true
			break
		}
	}
	if pending {
		if _, err := s.Evaluate(ctx, scope); err != nil {
			warn("achievement evaluation on read failed", err, "faction_id", factionID)
		} else if unlocks, err = s.store.Unlocks(ctx, factionID); err != nil {
			return nil, err
		}
	}
	out := make([]Achievement, 0, len(Definitions))
	for _, d := range Definitions {
		a := Achievement{Key: d.Key, Name: d.Name, Description: d.Description, Target: d.Target, Unit: d.Unit}
		v := metricValue(d.Metric, st.Summary, scope, s.now())
		if u, ok := unlocks[d.Key]; ok {
			a.Unlocked = true
			t := u.UnlockedAt.UTC().Format(time.RFC3339)
			a.UnlockedAt = &t
			v = d.Target
		}
		if v > d.Target {
			v = d.Target
		}
		a.Progress = v
		out = append(out, a)
	}
	return out, nil
}

// Evaluate checks every achievement against the faction's current figures and records the ones
// newly proven, returning their keys. It is idempotent and safe to run concurrently: the unique
// (faction, achievement) row makes concurrent evaluators unlock exactly once, and only the winner
// reports the key. It computes the figures fresh (never from the cache) and refreshes the cache.
// unlocked_at is the moment the condition was really met when the data says so (the n-th counted
// kill/headshot/longshot/bounty, the day a faction reached 30 days), otherwise the evaluation time.
func (s *Service) Evaluate(ctx context.Context, scope repository.HubStatsScope) ([]string, error) {
	key := cacheKey{scope.OrganizationID, scope.InstallationID, scope.FactionID}
	st, err := s.computeAndStore(ctx, key, scope)
	if err != nil {
		return nil, err
	}
	unlocks, err := s.store.Unlocks(ctx, scope.FactionID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var won []string
	for _, d := range Definitions {
		if _, done := unlocks[d.Key]; done {
			continue
		}
		value := metricValue(d.Metric, st.Summary, scope, now)
		if value < d.Target {
			continue
		}
		at := s.unlockTime(ctx, scope, d, now)
		meta := map[string]any{"value": value, "target": d.Target}
		inserted, err := s.store.InsertUnlock(ctx, scope, d.Key, at, meta)
		if err != nil {
			return won, fmt.Errorf("unlock %s: %w", d.Key, err)
		}
		if inserted {
			won = append(won, d.Key)
			slog.Info("component=faction_stats", "event", "faction_achievement_unlocked", "organization_id", scope.OrganizationID,
				"installation_id", scope.InstallationID, "faction_id", scope.FactionID, "achievement", d.Key)
		}
	}
	if len(won) > 0 {
		s.Invalidate(scope.OrganizationID, scope.InstallationID, scope.FactionID)
	}
	return won, nil
}

// unlockTime is the real moment a definition was earned, falling back to now.
func (s *Service) unlockTime(ctx context.Context, scope repository.HubStatsScope, d Definition, now time.Time) time.Time {
	var metric string
	switch d.Metric {
	case MetricKills:
		metric = "kills"
	case MetricHeadshots:
		metric = "headshots"
	case MetricLongshots:
		metric = "longshots"
	case MetricBounties:
		metric = "bounties"
	case MetricAgeDays:
		return scope.CreatedAt.Add(time.Duration(d.Target) * 24 * time.Hour)
	default:
		return now
	}
	if at, err := s.store.NthEventTime(ctx, scope, metric, d.Target); err == nil && at != nil {
		return *at
	}
	return now
}

// ProcessPending evaluates the factions affected by kills queued through NotifyCombat. The queue
// is drained per (guild, server); each affected faction is evaluated once however many kills
// arrived, so evaluation cost scales with active factions, not with kill volume.
func (s *Service) ProcessPending(ctx context.Context) (evaluated int) {
	s.pendMu.Lock()
	batch := s.pending
	s.pending = map[genKey]map[int64]struct{}{}
	s.pendMu.Unlock()
	seen := map[int64]bool{}
	for g, players := range batch {
		ids := make([]int64, 0, len(players))
		for p := range players {
			ids = append(ids, p)
		}
		scopes, err := s.store.ScopesForPlayers(ctx, g.guild, g.server, ids)
		if err != nil {
			warn("resolve factions for kills failed", err)
			continue
		}
		for _, sc := range scopes {
			if seen[sc.FactionID] || ctx.Err() != nil {
				continue
			}
			seen[sc.FactionID] = true
			if _, err := s.Evaluate(ctx, sc); err != nil {
				warn("achievement evaluation failed", err, "faction_id", sc.FactionID)
				continue
			}
			evaluated++
		}
	}
	return evaluated
}

// Run drains the event-driven queue every interval until ctx ends. It never panics the process.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("component=faction_stats", "msg", "pending evaluation panic recovered", "panic", fmt.Sprint(r))
					}
				}()
				c, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				s.ProcessPending(c)
			}()
		}
	}
}

// ReconcileAll evaluates every faction: the historical backfill for factions that already qualify
// (data that predates the evaluator) and the time-based achievements no event triggers. Unlock
// rows are unique, so it never double-unlocks, and it sends nothing anywhere - unlocking is
// silent (no Discord announcement), the website simply shows the achievement. It returns how many
// factions it evaluated and how many achievements it newly recorded.
func (s *Service) ReconcileAll(ctx context.Context, batch int) (factions, unlocked int, err error) {
	if batch <= 0 {
		batch = 100
	}
	var after int64
	for {
		scopes, err := s.store.ListScopes(ctx, after, batch)
		if err != nil {
			return factions, unlocked, err
		}
		if len(scopes) == 0 {
			return factions, unlocked, nil
		}
		for _, sc := range scopes {
			if ctx.Err() != nil {
				return factions, unlocked, ctx.Err()
			}
			after = sc.FactionID
			won, err := s.Evaluate(ctx, sc)
			if err != nil {
				warn("reconcile evaluation failed", err, "faction_id", sc.FactionID)
				continue
			}
			factions++
			unlocked += len(won)
		}
	}
}

// RunReconciler runs ReconcileAll once after initialDelay and then every interval, until ctx ends.
func (s *Service) RunReconciler(ctx context.Context, initialDelay, interval time.Duration) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("component=faction_stats", "msg", "reconcile panic recovered", "panic", fmt.Sprint(r))
					}
				}()
				c, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				f, u, err := s.ReconcileAll(c, 100)
				if err != nil {
					warn("faction reconcile failed", err)
				} else {
					slog.Info("component=faction_stats", "event", "faction_reconcile_done", "factions", f, "unlocked", u)
				}
			}()
			timer.Reset(interval)
		}
	}
}
