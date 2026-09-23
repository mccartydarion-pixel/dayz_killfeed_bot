package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The publisher's view of the repositories: exactly the reads it needs, so
// tests can substitute fakes.
type completionSeasonStore interface {
	GetSeasonByID(ctx context.Context, guildID, seasonID int64) (*repository.Season, error)
	GetSeasonResults(ctx context.Context, guildID, seasonID int64) (*repository.SeasonResult, error)
	GetEndedSeasons(ctx context.Context, guildID int64, limit int) ([]repository.Season, error)
}
type completionWarStore interface {
	GetWar(ctx context.Context, guildID, warID int64) (*repository.War, error)
	GetWarScore(ctx context.Context, guildID, warID int64) (int64, int64, error)
	GetWarTopKiller(ctx context.Context, guildID, warID int64) (int64, int64, error)
	GetWarLongestKill(ctx context.Context, guildID, warID int64) (int64, float64, error)
	GetEndedWars(ctx context.Context, guildID int64, limit int) ([]repository.War, error)
}
type completionEventStore interface {
	GetEvent(ctx context.Context, guildID, eventID int64) (*repository.CompetitiveEvent, error)
	Leaderboard(ctx context.Context, eventID int64, limit int) ([]repository.EventScore, error)
	GetEndedEvents(ctx context.Context, guildID int64, limit int) ([]repository.CompetitiveEvent, error)
}
type completionPlayerNames interface {
	DisplayNamesByID(ctx context.Context, guildID int64, ids []int64) (map[int64]string, error)
}
type completionFactions interface {
	GetFactionsByID(ctx context.Context, guildID int64, ids []int64) (map[int64]repository.Faction, error)
}

type LiveCompletionPublisher struct {
	claims         *CompletionAnnouncementService
	api            *SessionAPI
	setup          SetupStore
	seasons        completionSeasonStore
	wars           completionWarStore
	events         completionEventStore
	players        completionPlayerNames
	factions       completionFactions
	guilds         *repository.GuildRepository
	discordGuildID string
	// routes/servers resolve the SERVER_STATUS route, where completion
	// announcements live in Channel System V2. Optional.
	routes  RouteResolver
	servers GuildServersFunc
}

// SetRouting sends announcements to the guild's SERVER_STATUS route first;
// the legacy GuildSetup channels remain the fallback.
func (p *LiveCompletionPublisher) SetRouting(routes RouteResolver, servers GuildServersFunc) {
	if p != nil {
		p.routes, p.servers = routes, servers
	}
}

func (p *LiveCompletionPublisher) routedChannel(ctx context.Context) string {
	if p.routes == nil || p.servers == nil {
		return ""
	}
	guildRowID, serverIDs, err := p.servers(ctx)
	if err != nil {
		return ""
	}
	for _, serverID := range serverIDs {
		if ch, found, err := p.routes.Resolve(ctx, guildRowID, serverID, routeKeyServerStatus); err == nil && found && ch != "" {
			return ch
		}
	}
	return ""
}

func NewLiveCompletionPublisher(claims *CompletionAnnouncementService, api *SessionAPI, setup SetupStore, seasons *repository.SeasonRepository, wars *repository.PostgresWarRepository, events *repository.EventRepository, players *repository.PlayerRepository, factions *repository.FactionRepository, guilds *repository.GuildRepository, guildID string) *LiveCompletionPublisher {
	p := &LiveCompletionPublisher{claims: claims, api: api, setup: setup, guilds: guilds, discordGuildID: guildID}
	// Assign only non-nil repositories so the interface nil checks hold.
	if seasons != nil {
		p.seasons = seasons
	}
	if wars != nil {
		p.wars = wars
	}
	if events != nil {
		p.events = events
	}
	if players != nil {
		p.players = players
	}
	if factions != nil {
		p.factions = factions
	}
	return p
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
	channel := p.routedChannel(ctx)
	if channel == "" {
		channel = p.channel()
	}
	if channel == "" {
		return fmt.Errorf("announcement channel unavailable")
	}
	_, err := p.api.ChannelMessageSendEmbed(channel, embed)
	return err
}
func (p *LiveCompletionPublisher) PublishPendingSeasonCompletion(ctx context.Context, guildID, seasonID int64) error {
	_, err := p.claims.Publish(ctx, "SEASON", seasonID, func(ctx context.Context) error {
		embed, err := p.buildSeasonCompletion(ctx, guildID, seasonID)
		if err != nil {
			return err
		}
		return p.send(ctx, embed)
	})
	return err
}

// buildSeasonCompletion resolves the season's record holders to their real
// names. A holder that cannot be resolved is passed as "" and omitted.
func (p *LiveCompletionPublisher) buildSeasonCompletion(ctx context.Context, guildID, seasonID int64) (*discordgo.MessageEmbed, error) {
	season, err := p.seasons.GetSeasonByID(ctx, guildID, seasonID)
	if err != nil {
		return nil, err
	}
	if season == nil {
		return nil, fmt.Errorf("season not found")
	}
	result, err := p.seasons.GetSeasonResults(ctx, guildID, seasonID)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("season result not found")
	}
	names, err := p.playerNames(ctx, guildID, result.TopPlayerID, result.LongestKillPlayerID, result.BestStreakPlayerID)
	if err != nil {
		return nil, err
	}
	factions, err := p.factionsByID(ctx, guildID, result.TopFactionID)
	if err != nil {
		return nil, err
	}
	return BuildSeasonCompletionEmbed(season.Name,
		names[result.TopPlayerID], result.TopPlayerKills,
		factionLabel(factions[result.TopFactionID]), result.TopFactionKills,
		names[result.LongestKillPlayerID], result.LongestKillValue,
		names[result.BestStreakPlayerID], result.BestStreakValue), nil
}

func (p *LiveCompletionPublisher) PublishPendingWarCompletion(ctx context.Context, guildID, warID int64) error {
	_, err := p.claims.Publish(ctx, "WAR", warID, func(ctx context.Context) error {
		embed, err := p.buildWarCompletion(ctx, guildID, warID)
		if err != nil {
			return err
		}
		return p.send(ctx, embed)
	})
	return err
}

// buildWarCompletion resolves both factions, the winner, the war's top killer
// and longest kill, and the season. Anything the data does not hold is left
// empty so the card omits it.
func (p *LiveCompletionPublisher) buildWarCompletion(ctx context.Context, guildID, warID int64) (*discordgo.MessageEmbed, error) {
	war, err := p.wars.GetWar(ctx, guildID, warID)
	if err != nil {
		return nil, err
	}
	a, b, err := p.wars.GetWarScore(ctx, guildID, warID)
	if err != nil {
		return nil, err
	}
	// No kills in the war: no top killer / longest kill to show.
	topID, topKills, err := p.wars.GetWarTopKiller(ctx, guildID, warID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	longID, longest, err := p.wars.GetWarLongestKill(ctx, guildID, warID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ids := []int64{war.FactionAID, war.FactionBID}
	if war.WinnerFactionID != nil {
		ids = append(ids, *war.WinnerFactionID)
	}
	factions, err := p.factionsByID(ctx, guildID, ids...)
	if err != nil {
		return nil, err
	}
	names, err := p.playerNames(ctx, guildID, topID, longID)
	if err != nil {
		return nil, err
	}
	card := WarCompletionCard{
		FactionA: factionLabel(factions[war.FactionAID]), ScoreA: a,
		FactionB: factionLabel(factions[war.FactionBID]), ScoreB: b,
		TopKiller: names[topID], TopKills: topKills,
		LongestKiller: names[longID], Longest: longest,
	}
	if war.WinnerFactionID != nil {
		card.Winner = factionLabel(factions[*war.WinnerFactionID])
	} else if a == b {
		// End() stores no winner exactly when the kill counts tie.
		card.Draw = true
	}
	if card.Season, err = p.seasonName(ctx, guildID, war.SeasonID); err != nil {
		return nil, err
	}
	return BuildWarCompletionEmbed(card), nil
}

func (p *LiveCompletionPublisher) PublishPendingEventCompletion(ctx context.Context, guildID, eventID int64) error {
	_, err := p.claims.Publish(ctx, "EVENT", eventID, func(ctx context.Context) error {
		embed, err := p.buildEventCompletion(ctx, guildID, eventID)
		if err != nil {
			return err
		}
		return p.send(ctx, embed)
	})
	return err
}

// buildEventCompletion resolves the podium's display names. A row whose
// player cannot be resolved is dropped; ranks stay positional.
func (p *LiveCompletionPublisher) buildEventCompletion(ctx context.Context, guildID, eventID int64) (*discordgo.MessageEmbed, error) {
	event, err := p.events.GetEvent(ctx, guildID, eventID)
	if err != nil {
		return nil, err
	}
	rows, err := p.events.Leaderboard(ctx, eventID, 3)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.PlayerID)
	}
	names, err := p.playerNames(ctx, guildID, ids...)
	if err != nil {
		return nil, err
	}
	placements := make([]EventPlacement, 0, len(rows))
	for i, row := range rows {
		placements = append(placements, EventPlacement{Rank: i + 1, Name: names[row.PlayerID], Score: row.Score})
	}
	season, err := p.seasonName(ctx, guildID, event.SeasonID)
	if err != nil {
		return nil, err
	}
	return BuildEventCompletionEmbed(event.Name, season, placements), nil
}

// playerNames resolves player IDs to display names; unknown IDs map to "".
func (p *LiveCompletionPublisher) playerNames(ctx context.Context, guildID int64, ids ...int64) (map[int64]string, error) {
	if p.players == nil {
		return map[int64]string{}, nil
	}
	names, err := p.players.DisplayNamesByID(ctx, guildID, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve player names: %w", err)
	}
	return names, nil
}

// factionsByID resolves faction IDs; unknown IDs are absent.
func (p *LiveCompletionPublisher) factionsByID(ctx context.Context, guildID int64, ids ...int64) (map[int64]repository.Faction, error) {
	if p.factions == nil {
		return map[int64]repository.Faction{}, nil
	}
	factions, err := p.factions.GetFactionsByID(ctx, guildID, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve factions: %w", err)
	}
	return factions, nil
}

// seasonName is the season's name, or "" when there is no season.
func (p *LiveCompletionPublisher) seasonName(ctx context.Context, guildID, seasonID int64) (string, error) {
	if seasonID == 0 || p.seasons == nil {
		return "", nil
	}
	season, err := p.seasons.GetSeasonByID(ctx, guildID, seasonID)
	if err != nil || season == nil {
		return "", err
	}
	return season.Name, nil
}

// factionLabel is "[TAG] Name", the name alone without a tag, or "" for an
// unresolved faction.
func factionLabel(f repository.Faction) string {
	name, tag := strings.TrimSpace(f.Name), strings.TrimSpace(f.Tag)
	switch {
	case name == "":
		return ""
	case tag == "":
		return name
	default:
		return "[" + tag + "] " + name
	}
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
