package seasons

import (
	"context"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type Store interface {
	EnsureDefaultSeason(context.Context, int64, time.Time) (*repository.Season, error)
	GetActiveSeason(context.Context, int64) (*repository.Season, error)
	GetSeasonByID(context.Context, int64, int64) (*repository.Season, error)
	GetSeasonHistory(context.Context, int64, int) ([]repository.Season, error)
	Start(context.Context, int64, string, time.Time) (*repository.Season, error)
	End(context.Context, int64, int64, time.Time) error
	FinalizeSeason(context.Context, int64, int64, time.Time) (*repository.SeasonResult, error)
	GetSeasonResults(context.Context, int64, int64) (*repository.SeasonResult, error)
}

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }
func (s *Service) GetActiveSeason(ctx context.Context, guildID int64) (*repository.Season, error) {
	return s.store.GetActiveSeason(ctx, guildID)
}
func (s *Service) EnsureDefaultSeason(ctx context.Context, guildID int64, now time.Time) (*repository.Season, error) {
	if guildID <= 0 {
		return nil, fmt.Errorf("invalid guild id")
	}
	return s.store.EnsureDefaultSeason(ctx, guildID, now)
}
func (s *Service) StartSeason(ctx context.Context, guildID int64, name string, at time.Time) (*repository.Season, error) {
	if name == "" {
		return nil, fmt.Errorf("season name is required")
	}
	return s.store.Start(ctx, guildID, name, at)
}
func (s *Service) Start(ctx context.Context, guildID int64, name string, at time.Time) (*repository.Season, error) {
	return s.StartSeason(ctx, guildID, name, at)
}
func (s *Service) EndSeason(ctx context.Context, guildID, seasonID int64, at time.Time) (*repository.SeasonResult, error) {
	return s.store.FinalizeSeason(ctx, guildID, seasonID, at)
}
func (s *Service) FinalizeSeason(ctx context.Context, guildID, seasonID int64, at time.Time) (*repository.SeasonResult, error) {
	return s.store.FinalizeSeason(ctx, guildID, seasonID, at)
}
func (s *Service) GetSeasonHistory(ctx context.Context, guildID int64, limit int) ([]repository.Season, error) {
	return s.store.GetSeasonHistory(ctx, guildID, limit)
}
func (s *Service) GetSeasonResults(ctx context.Context, guildID, seasonID int64) (*repository.SeasonResult, error) {
	return s.store.GetSeasonResults(ctx, guildID, seasonID)
}
