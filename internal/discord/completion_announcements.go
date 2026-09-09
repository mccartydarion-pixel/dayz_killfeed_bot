package discord

import (
	"context"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type CompletionAnnouncementService struct {
	claims *repository.AnnouncementRepository
	lease  time.Duration
}

func NewCompletionAnnouncementService(claims *repository.AnnouncementRepository) *CompletionAnnouncementService {
	return &CompletionAnnouncementService{claims: claims, lease: 2 * time.Minute}
}

// Publish claims a durable lease before sending. Failed sends release the lease;
// successful sends transition the row to ANNOUNCED.
func (s *CompletionAnnouncementService) Publish(ctx context.Context, kind string, objectID int64, send func(context.Context) error) (bool, error) {
	if s == nil || s.claims == nil {
		return false, nil
	}
	claimed, err := s.claims.Claim(ctx, kind, objectID, time.Now().UTC(), s.lease)
	if err != nil || !claimed {
		return false, err
	}
	if err = send(ctx); err != nil {
		_ = s.claims.Release(ctx, kind, objectID)
		return false, err
	}
	if err = s.claims.MarkAnnounced(ctx, kind, objectID, time.Now().UTC()); err != nil {
		return false, err
	}
	return true, nil
}
