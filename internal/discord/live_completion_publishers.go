package discord

import (
	"context"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type LiveCompletionPublisher struct {
	claims         *CompletionAnnouncementService
	api            *SessionAPI
	setup          SetupStore
	seasons        *repository.SeasonRepository
	wars           *repository.PostgresWarRepository
	events         *repository.EventRepository
	guilds         *repository.GuildRepository
	discordGuildID string
}

func NewLiveCompletionPublisher(claims *CompletionAnnouncementService, api *SessionAPI, setup SetupStore, seasons *repository.SeasonRepository, wars *repository.PostgresWarRepository, events *repository.EventRepository, guilds *repository.GuildRepository, guildID string) *LiveCompletionPublisher {
	return &LiveCompletionPublisher{claims: claims, api: api, setup: setup, seasons: seasons, wars: wars, events: events, guilds: guilds, discordGuildID: guildID}
}
func (p *LiveCompletionPublisher) channel() string {
	if p == nil || p.setup == nil {
		return ""
	}
	s, _ := p.setup.Get(p.discordGuildID)
	if s == nil {
		return ""
	}
	if s.ServerStatusChannelID != "" {
		return s.ServerStatusChannelID
	}
	return s.LeaderboardsChannelID
}
func (p *LiveCompletionPublisher) send(ctx context.Context, embed *discordgo.MessageEmbed) error {
	if p.api == nil {
		return fmt.Errorf("discord API unavailable")
	}
	channel := p.channel()
	if channel == "" {
		return fmt.Errorf("announcement channel unavailable")
	}
	_, err := p.api.ChannelMessageSendEmbed(channel, embed)
	return err
}
func (p *LiveCompletionPublisher) PublishPendingSeasonCompletion(ctx context.Context, guildID, seasonID int64) error {
	_, err := p.claims.Publish(ctx, "SEASON", seasonID, func(ctx context.Context) error {
		season, err := p.seasons.GetSeasonByID(ctx, guildID, seasonID)
		if err != nil {
			return err
		}
		result, err := p.seasons.GetSeasonResults(ctx, guildID, seasonID)
		if err != nil {
			return err
		}
		if result == nil {
			return fmt.Errorf("season result not found")
		}
		embed := BuildSeasonCompletionEmbed(season.Name, "Player", result.TopPlayerKills, "Faction", result.TopFactionKills, "Player", result.LongestKillValue, "Player", result.BestStreakValue)
		return p.send(ctx, embed)
	})
	return err
}
func (p *LiveCompletionPublisher) PublishPendingWarCompletion(ctx context.Context, guildID, warID int64) error {
	_, err := p.claims.Publish(ctx, "WAR", warID, func(ctx context.Context) error {
		war, err := p.wars.GetWar(ctx, guildID, warID)
		if err != nil {
			return err
		}
		a, b, err := p.wars.GetWarScore(ctx, guildID, warID)
		if err != nil {
			return err
		}
		embed := &discordgo.MessageEmbed{Title: "🏆 FACTION WAR COMPLETE", Description: BuildWarCompletionText(fmt.Sprintf("Faction %d", war.FactionAID), a, fmt.Sprintf("Faction %d", war.FactionBID), b, fmt.Sprintf("Winner %d", valueID(war.WinnerFactionID)), "Player", 0, 0, "Season"), Color: ColorChampionGold, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • FACTION WAR"}}
		return p.send(ctx, embed)
	})
	return err
}
func (p *LiveCompletionPublisher) PublishPendingEventCompletion(ctx context.Context, guildID, eventID int64) error {
	_, err := p.claims.Publish(ctx, "EVENT", eventID, func(ctx context.Context) error {
		event, err := p.events.GetEvent(ctx, guildID, eventID)
		if err != nil {
			return err
		}
		rows, err := p.events.Leaderboard(ctx, eventID, 3)
		if err != nil {
			return err
		}
		placements := make([]string, 0, len(rows))
		for i, row := range rows {
			placements = append(placements, fmt.Sprintf("%d. Player %d — %.1f", i+1, row.PlayerID, row.Score))
		}
		embed := &discordgo.MessageEmbed{Title: "👑 CHAMPION EVENT COMPLETE", Description: BuildEventCompletionText(event.Name, placements), Color: ColorChampionGold, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • COMPETITIVE EVENTS"}}
		return p.send(ctx, embed)
	})
	return err
}
func (p *LiveCompletionPublisher) RecoverPending(ctx context.Context, guildID int64) error {
	if p == nil {
		return nil
	}
	if p.seasons != nil {
		seasons, err := p.seasons.GetEndedSeasons(ctx, guildID, 25)
		if err != nil {
			return err
		}
		for _, season := range seasons {
			if err := p.PublishPendingSeasonCompletion(ctx, guildID, season.ID); err != nil {
				return err
			}
		}
	}
	if p.wars != nil {
		wars, err := p.wars.GetEndedWars(ctx, guildID, 25)
		if err != nil {
			return err
		}
		for _, war := range wars {
			if war.Status == repository.WarEnded {
				if err := p.PublishPendingWarCompletion(ctx, guildID, war.ID); err != nil {
					return err
				}
			}
		}
	}
	if p.events != nil {
		events, err := p.events.GetEndedEvents(ctx, guildID, 25)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := p.PublishPendingEventCompletion(ctx, guildID, event.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
func valueID(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

var _ = time.Now
