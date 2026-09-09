package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

const (
	TypeBounty          = "BOUNTY"
	TypeMostKills       = "MOST_KILLS"
	TypeLongestKill     = "LONGEST_KILL"
	TypeKillStreak      = "KILL_STREAK"
	TypeFactionKills    = "FACTION_KILLS"
	TypeFactionWarKills = "FACTION_WAR_KILLS"
	TypeHeadshotHunt    = "HEADSHOT_HUNT"
	TypeWeaponChallenge = "WEAPON_CHALLENGE"
	StatusDraft         = "DRAFT"
	StatusScheduled     = "SCHEDULED"
	StatusActive        = "ACTIVE"
	StatusEnded         = "ENDED"
	StatusCancelled     = "CANCELLED"
)

type MostKillsConfig struct {
	MinimumDistance *float64 `json:"minimum_distance,omitempty"`
}
type LongestKillConfig struct {
	MinimumDistance float64 `json:"minimum_distance"`
}
type KillStreakConfig struct {
	MinimumStreak int `json:"minimum_streak,omitempty"`
}
type FactionKillsConfig struct {
	EnemyFactionsOnly bool `json:"enemy_factions_only"`
}
type WeaponChallengeConfig struct {
	WeaponNames []string `json:"weapon_names"`
}
type BountyConfig struct {
	TargetPlayerID int64 `json:"target_player_id"`
	RewardPoints   int   `json:"reward_points"`
}

type Event struct {
	ID, GuildID, SeasonID           int64
	Type, Name, Description, Status string
	StartsAt, EndsAt                *time.Time
	Config                          json.RawMessage
}
type KillInput struct {
	KillID, KillerPlayerID, VictimPlayerID int64
	KillerFactionID, VictimFactionID       *int64
	WarID                                  *int64
	WeaponDisplay                          string
	Distance                               *float64
	Headshot                               bool
	Streak                                 int
	EventTime                              time.Time
}
type Qualification struct {
	Qualifies           bool
	Points              float64
	PlayerID, FactionID int64
	BestDistance        *float64
	BestStreak          int
}

type Store interface {
	CreateEvent(context.Context, repository.CompetitiveEvent, string) (*repository.CompetitiveEvent, error)
	StartEvent(context.Context, int64, int64, time.Time) error
	EndEvent(context.Context, int64, int64, time.Time) error
	CancelEvent(context.Context, int64, int64) error
}

type SchedulerStore interface {
	ActivateDue(context.Context, time.Time) error
	EndDue(context.Context, time.Time) error
}

type FinalizerStore interface {
	GetEvent(context.Context, int64, int64) (*repository.CompetitiveEvent, error)
	Leaderboard(context.Context, int64, int) ([]repository.EventScore, error)
	FinalizeEvent(context.Context, int64, repository.EventResult) error
}

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) Create(ctx context.Context, event repository.CompetitiveEvent, config any, createdBy string) (*repository.CompetitiveEvent, error) {
	if err := ValidateConfig(event.Type, config); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal event config: %w", err)
	}
	event.Config = payload
	if event.Status == "" {
		event.Status = StatusDraft
	}
	return s.store.CreateEvent(ctx, event, createdBy)
}

func (s *Service) Start(ctx context.Context, guildID, eventID int64, at time.Time) error {
	return s.store.StartEvent(ctx, guildID, eventID, at)
}

func (s *Service) End(ctx context.Context, guildID, eventID int64, at time.Time) error {
	return s.store.EndEvent(ctx, guildID, eventID, at)
}

func (s *Service) Cancel(ctx context.Context, guildID, eventID int64) error {
	return s.store.CancelEvent(ctx, guildID, eventID)
}

func (s *Service) SchedulerTick(ctx context.Context, now time.Time) error {
	store, ok := s.store.(SchedulerStore)
	if !ok {
		return fmt.Errorf("event store does not support scheduling")
	}
	if err := store.ActivateDue(ctx, now); err != nil {
		return err
	}
	return store.EndDue(ctx, now)
}

func (s *Service) FinalizeEvent(ctx context.Context, guildID, eventID int64, at time.Time) error {
	store, ok := s.store.(FinalizerStore)
	if !ok {
		return fmt.Errorf("event store does not support finalization")
	}
	if _, err := store.GetEvent(ctx, guildID, eventID); err != nil {
		return err
	}
	rankings, err := store.Leaderboard(ctx, eventID, 2)
	if err != nil {
		return err
	}
	if len(rankings) == 0 {
		return store.FinalizeEvent(ctx, eventID, repository.EventResult{EventID: eventID, FinalizedAt: at})
	}
	winner := rankings[0]
	return store.FinalizeEvent(ctx, eventID, repository.EventResult{EventID: eventID, WinnerPlayerID: winner.PlayerID, WinnerFactionID: winner.FactionID, WinningScore: winner.Score, FinalizedAt: at})
}

func ValidateConfig(eventType string, config any) error {
	switch eventType {
	case TypeMostKills, TypeHeadshotHunt, TypeFactionWarKills:
		return nil
	case TypeLongestKill:
		v, ok := config.(LongestKillConfig)
		if !ok {
			return fmt.Errorf("%s requires LongestKillConfig", eventType)
		}
		if v.MinimumDistance < 0 {
			return fmt.Errorf("minimum distance must be non-negative")
		}
	case TypeKillStreak:
		v, ok := config.(KillStreakConfig)
		if !ok {
			return fmt.Errorf("%s requires KillStreakConfig", eventType)
		}
		if v.MinimumStreak < 0 {
			return fmt.Errorf("minimum streak must be non-negative")
		}
	case TypeFactionKills:
		if _, ok := config.(FactionKillsConfig); !ok {
			return fmt.Errorf("%s requires FactionKillsConfig", eventType)
		}
	case TypeWeaponChallenge:
		v, ok := config.(WeaponChallengeConfig)
		if !ok || len(v.WeaponNames) == 0 {
			return fmt.Errorf("weapon challenge requires at least one weapon")
		}
	case TypeBounty:
		v, ok := config.(BountyConfig)
		if !ok || v.TargetPlayerID <= 0 || v.RewardPoints <= 0 {
			return fmt.Errorf("bounty requires target and positive reward")
		}
	default:
		return fmt.Errorf("unsupported event type %q", eventType)
	}
	return nil
}

func InWindow(event Event, at time.Time) bool {
	return event.Status == StatusActive && !at.Before(valueTime(event.StartsAt)) && (event.EndsAt == nil || at.Before(*event.EndsAt))
}
func valueTime(v *time.Time) time.Time {
	if v == nil {
		return time.Time{}
	}
	return v.UTC()
}

func Qualify(event Event, kill KillInput) Qualification {
	if !InWindow(event, kill.EventTime) {
		return Qualification{}
	}
	if kill.KillerPlayerID == 0 || kill.VictimPlayerID == 0 || kill.KillerPlayerID == kill.VictimPlayerID {
		return Qualification{}
	}
	var out = Qualification{Qualifies: true, Points: 1, PlayerID: kill.KillerPlayerID}
	switch event.Type {
	case TypeMostKills:
		var c MostKillsConfig
		if json.Unmarshal(event.Config, &c) != nil || (c.MinimumDistance != nil && (kill.Distance == nil || *kill.Distance < *c.MinimumDistance)) {
			return Qualification{}
		}
	case TypeLongestKill:
		var c LongestKillConfig
		if json.Unmarshal(event.Config, &c) != nil || kill.Distance == nil || *kill.Distance < c.MinimumDistance {
			return Qualification{}
		}
		out.Points = *kill.Distance
		out.BestDistance = kill.Distance
	case TypeKillStreak:
		var c KillStreakConfig
		if json.Unmarshal(event.Config, &c) != nil || kill.Streak < c.MinimumStreak {
			return Qualification{}
		}
		out.Points = float64(kill.Streak)
		out.BestStreak = kill.Streak
	case TypeHeadshotHunt:
		if !kill.Headshot {
			return Qualification{}
		}
	case TypeWeaponChallenge:
		var c WeaponChallengeConfig
		if json.Unmarshal(event.Config, &c) != nil {
			return Qualification{}
		}
		matched := false
		for _, weapon := range c.WeaponNames {
			if strings.EqualFold(strings.TrimSpace(weapon), strings.TrimSpace(kill.WeaponDisplay)) {
				matched = true
				break
			}
		}
		if !matched {
			return Qualification{}
		}
	case TypeFactionKills:
		var c FactionKillsConfig
		if json.Unmarshal(event.Config, &c) != nil || kill.KillerFactionID == nil || (c.EnemyFactionsOnly && (kill.VictimFactionID == nil || *kill.KillerFactionID == *kill.VictimFactionID)) {
			return Qualification{}
		}
		out.FactionID = *kill.KillerFactionID
	case TypeFactionWarKills:
		if kill.WarID == nil || kill.KillerFactionID == nil || kill.VictimFactionID == nil || *kill.KillerFactionID == *kill.VictimFactionID {
			return Qualification{}
		}
		out.FactionID = *kill.KillerFactionID
	default:
		return Qualification{}
	}
	return out
}

var _ = repository.ErrDuplicate
