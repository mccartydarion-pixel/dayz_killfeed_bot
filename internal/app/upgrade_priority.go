package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 1, priority queue rewards: the server's top N ranked players and supporter tier holders
// go on the Nitrado priority list automatically and come off when they no longer qualify. Champion
// only ever takes off names it added itself, and never one a perk store purchase still pays for.

const priorityRewardsEvery = 10 * time.Minute

// priorityPlan is the list change one pass makes.
type priorityPlan struct {
	list    []string                   // the list to write
	added   []repository.PriorityGrant // names this pass put on the list
	removed []string                   // granted names to forget (taken off, or left for staff/perks)
	changed bool
}

// planPriority works out the new list. keep(name) reports whether a granted name must stay on the
// list although it no longer qualifies (a perk purchase pays for it).
func planPriority(list []string, granted []repository.PriorityGrant, wanted []repository.PriorityGrant, keep func(repository.PriorityGrant) bool) priorityPlan {
	p := priorityPlan{list: append([]string{}, list...)}
	isGranted := map[string]bool{}
	for _, g := range granted {
		isGranted[strings.ToLower(g.Name)] = true
	}
	isWanted := map[string]bool{}
	for _, g := range wanted {
		key := strings.ToLower(g.Name)
		if isWanted[key] || !nitrado.ValidPriorityName(g.Name) {
			continue
		}
		isWanted[key] = true
		if isGranted[key] {
			continue
		}
		if priorityIndex(p.list, g.Name) >= 0 {
			continue // already on the list by staff or a purchase: not ours to manage
		}
		if len(p.list) >= nitrado.MaxPriorityEntries {
			continue
		}
		p.list = append(p.list, g.Name)
		p.added = append(p.added, g)
		p.changed = true
	}
	for _, g := range granted {
		if isWanted[strings.ToLower(g.Name)] {
			continue
		}
		p.removed = append(p.removed, g.Name)
		if keep(g) {
			continue
		}
		if at := priorityIndex(p.list, g.Name); at >= 0 {
			p.list = append(p.list[:at:at], p.list[at+1:]...)
			p.changed = true
		}
	}
	return p
}

func (a *App) runPriorityRewards(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	on := s.Settings.PriorityRankedTop > 0 || s.Settings.PriorityVIP
	granted, err := a.Upgrades.PriorityGrants(ctx, s.ServerID)
	if err != nil || (!on && len(granted) == 0) {
		return
	}
	if !a.upgradeRuns.due(fmt.Sprintf("priority:%d", s.ServerID), priorityRewardsEvery, now) {
		return
	}
	var wanted []repository.PriorityGrant
	if s.Settings.PriorityRankedTop > 0 && a.Ranked != nil {
		if standings, err := a.Ranked.ServerStandings(ctx, s.ServerID, s.Settings.PriorityRankedTop); err == nil {
			for _, st := range standings {
				if st.RP > 0 && st.Name != "" {
					wanted = append(wanted, repository.PriorityGrant{Name: st.Name, Reason: "RANKED", PlayerID: st.PlayerID})
				}
			}
		}
	}
	if s.Settings.PriorityVIP {
		supporters, err := a.Upgrades.ActiveSupporters(ctx, guildID, now)
		if err != nil {
			return // never take names off because the list of who qualifies could not be read
		}
		wanted = append(wanted, supporters...)
	}
	client, serviceID, err := a.perkPriorityClient(ctx, s.InstallationID)
	if err != nil {
		return
	}
	list, err := client.PriorityList(ctx, serviceID)
	if err != nil {
		slog.Info("component=upgrades", "event", "priority_read_failed", "server_id", s.ServerID, "err", err.Error())
		return
	}
	plan := planPriority(list, granted, wanted, func(g repository.PriorityGrant) bool {
		held, err := a.Upgrades.PerkHoldsPriority(ctx, guildID, g.PlayerID)
		return err != nil || held
	})
	if plan.changed {
		if err := client.SetPriorityList(ctx, serviceID, plan.list); err != nil {
			slog.Warn("component=upgrades", "event", "priority_write_failed", "server_id", s.ServerID, "err", err.Error())
			return
		}
	}
	if len(plan.added) > 0 || len(plan.removed) > 0 {
		if err := a.Upgrades.RecordPriorityGrants(ctx, s.ServerID, plan.added, plan.removed, now); err != nil {
			slog.Warn("component=upgrades", "event", "priority_record_failed", "server_id", s.ServerID, "err", err.Error())
		}
		slog.Info("component=upgrades", "event", "priority_rewards_synced", "server_id", s.ServerID, "added", len(plan.added), "removed", len(plan.removed))
	}
}
