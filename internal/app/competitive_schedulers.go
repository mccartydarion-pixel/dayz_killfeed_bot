package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/discord/panels"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func (a *App) runCompetitiveSchedulers(ctx context.Context, guildID int64) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	tick := func() {
		now := time.Now().UTC()
		if a.CompletionPublisher != nil {
			if err := a.CompletionPublisher.RecoverPending(ctx, guildID); err != nil {
				slog.Warn("component=announcements", "msg", "completion recovery failed", "err", err.Error())
			}
		}
		if a.EventService != nil {
			if err := a.EventService.SchedulerTick(ctx, now); err != nil {
				slog.Warn("component=events", "msg", "event scheduler tick failed", "err", err.Error())
			}
		}
		a.publishEventAnnouncements(ctx, guildID, now)
		a.runSeasonPlanner(ctx, guildID, now)
		a.runPerkStore(ctx, guildID, now)
		a.runRPBoostAnnouncements(ctx, guildID, now)
		a.runRankedBonusAnnouncements(ctx, guildID, now)
		a.runUpgrades(ctx, guildID, now)
		a.runProgression(ctx, guildID, now)
		a.runVIPExpiry(ctx, guildID, now)
		a.runRewards(ctx, guildID, now)
		if ended, err := a.Events.GetEndedUnfinalized(ctx, guildID, 25); err == nil {
			for _, event := range ended {
				if err := a.EventService.FinalizeEvent(ctx, guildID, event.ID, now); err != nil {
					slog.Warn("component=events", "msg", "event finalization failed", "event_id", event.ID, "err", err.Error())
				} else if a.CompletionPublisher != nil {
					if err := a.CompletionPublisher.PublishPendingEventCompletion(ctx, guildID, event.ID); err != nil {
						slog.Warn("component=events", "msg", "event completion announcement failed", "event_id", event.ID, "err", err.Error())
					}
				}
			}
		}
		a.hotZoneTick(ctx, guildID, now)
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

type contentPanelEditor struct{ api *discord.SessionAPI }

func (e contentPanelEditor) Send(channelID, content string) (string, error) {
	m, err := e.api.ChannelMessageSendContent(channelID, content)
	if m == nil {
		return "", err
	}
	return m.ID, err
}

func (e contentPanelEditor) Edit(channelID, messageID, content string) error {
	_, err := e.api.ChannelMessageEditContent(channelID, messageID, content)
	return err
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
