package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord/panels"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// runCompetitiveSchedulers runs the scheduled competitive work (leader only) every 45 s: event
// start/end and bounty expiry once for every guild, and the per-guild jobs (season planner, perk
// store, upgrades, progression, VIP expiry, rewards, event finalization, hot zones,
// announcements) for the primary guild and every guild with an active server. Each job routes
// its announcements to that guild's own channels.
func (a *App) runCompetitiveSchedulers(ctx context.Context, primaryGuildID int64) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	tick := func() {
		now := time.Now().UTC()
		guilds := a.schedulerGuilds(ctx, primaryGuildID)
		if a.CompletionPublisher != nil {
			for _, guildID := range guilds {
				if err := a.CompletionPublisher.RecoverPending(ctx, guildID); err != nil {
					slog.Warn("component=announcements", "msg", "completion recovery failed", "guild_id", guildID, "err", err.Error())
				}
			}
		}
		if a.EventService != nil {
			if err := a.EventService.SchedulerTick(ctx, now); err != nil {
				slog.Warn("component=events", "msg", "event scheduler tick failed", "err", err.Error())
			}
		}
		for _, guildID := range guilds {
			if ctx.Err() != nil {
				return
			}
			a.runGuildSchedulers(ctx, guildID, now)
		}
		if a.BountyService != nil {
			// One atomic UPDATE ... RETURNING per sweep (no goroutine per bounty);
			// each expiry is reported once, after it committed.
			if _, err := a.BountyService.Sweep(ctx, now); err != nil {
				slog.Warn("component=bounty", "msg", "bounty expiry failed", "err", err.Error())
			}
		}
	}
	tick()
	for {
		select {
		case <-ticker.C:
			tick()
		case <-ctx.Done():
			return
		}
	}
}

// runGuildSchedulers is one guild's share of a competitive scheduler tick.
func (a *App) runGuildSchedulers(ctx context.Context, guildID int64, now time.Time) {
	a.publishEventAnnouncements(ctx, guildID, now)
	a.runSeasonPlanner(ctx, guildID, now)
	a.runPerkStore(ctx, guildID, now)
	a.runRPBoostAnnouncements(ctx, guildID, now)
	a.runRankedBonusAnnouncements(ctx, guildID, now)
	a.runUpgrades(ctx, guildID, now)
	a.runProgression(ctx, guildID, now)
	a.runVIPExpiry(ctx, guildID, now)
	a.runRewards(ctx, guildID, now)
	if a.Events != nil && a.EventService != nil {
		if ended, err := a.Events.GetEndedUnfinalized(ctx, guildID, 25); err == nil {
			for _, event := range ended {
				if err := a.EventService.FinalizeEvent(ctx, guildID, event.ID, now); err != nil {
					slog.Warn("component=events", "msg", "event finalization failed", "guild_id", guildID, "event_id", event.ID, "err", err.Error())
				} else if a.CompletionPublisher != nil {
					if err := a.CompletionPublisher.PublishPendingEventCompletion(ctx, guildID, event.ID); err != nil {
						slog.Warn("component=events", "msg", "event completion announcement failed", "guild_id", guildID, "event_id", event.ID, "err", err.Error())
					}
				}
			}
		}
	}
	a.hotZoneTick(ctx, guildID, now)
}

// schedulerGuilds is the guilds scheduled work runs for: the primary guild, then every guild
// with an active game server (SaaS customers), each once. A failed server listing keeps the
// primary guild only.
func (a *App) schedulerGuilds(ctx context.Context, primaryGuildID int64) []int64 {
	var out []int64
	seen := map[int64]bool{}
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(primaryGuildID)
	if a.Servers == nil {
		return out
	}
	rows, err := a.Servers.ListActive(ctx)
	if err != nil {
		slog.Warn("component=servers", "event", "scheduler_guilds_failed", "err", err.Error())
		return out
	}
	for _, g := range groupServersByGuild(rows) {
		add(g.GuildRowID)
	}
	return out
}

type competitivePanelLoader struct {
	guildID        int64
	events         *repository.EventRepository
	bounties       *repository.BountyRepository
	points         *repository.PointsRepository
	seasons        *repository.SeasonRepository
	guilds         *repository.GuildRepository
	discordGuildID string
}

func (l *competitivePanelLoader) Load(ctx context.Context) (panels.Snapshot, error) {
	if l.guildID == 0 {
		_, id, err := l.guilds.GetGuild(ctx, l.discordGuildID)
		if err != nil {
			return panels.Snapshot{}, err
		}
		l.guildID = id
	}
	out := panels.Snapshot{GeneratedAt: time.Now()}
	active, err := l.events.GetActiveEvents(ctx, l.guildID)
	if err != nil {
		return out, err
	}
	for _, event := range active {
		rows, _ := l.events.Leaderboard(ctx, event.ID, 3)
		line := fmt.Sprintf("%s (%s)", event.Name, event.Type)
		if len(rows) > 0 {
			line += fmt.Sprintf(" — %.0f", rows[0].Score)
		}
		out.EventLines = append(out.EventLines, line)
	}
	wanted, err := l.bounties.ListBoardAll(ctx, l.guildID, 5)
	if err != nil {
		return out, err
	}
	for _, b := range wanted {
		out.BountyLines = append(out.BountyLines, fmt.Sprintf("%s — %d pts", b.TargetName, b.Total))
	}
	points, err := l.points.Leaderboard(ctx, l.guildID, true, 5)
	if err != nil {
		return out, err
	}
	for _, p := range points {
		out.PointLines = append(out.PointLines, p.DisplayName+" — "+p.Value)
	}
	return out, nil
}
