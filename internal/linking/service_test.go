package linking

import (
	"context"
	"errors"
	"testing"
	"time"
)

type linkTestRepository struct {
	players []PlayerCandidate
	pending *LinkRecord
	claimed *LinkRecord
}

func (r *linkTestRepository) FindPlayers(context.Context, int64, string) ([]PlayerCandidate, error) {
	return r.players, nil
}
func (r *linkTestRepository) GetByDiscord(context.Context, int64, string) (*LinkRecord, error) {
	return nil, nil
}
func (r *linkTestRepository) GetByPlayer(context.Context, int64, int64) (*LinkRecord, error) {
	return r.claimed, nil
}
func (r *linkTestRepository) CreatePending(_ context.Context, link LinkRecord) error {
	r.pending = &link
	return nil
}
func (r *linkTestRepository) Unlink(context.Context, int64, string) error { return nil }

type linkTestActivity struct {
	playtime time.Duration
	err      error
}

func (a linkTestActivity) GetObservedPlaytime(context.Context, int64, int64, int64, time.Time) (time.Duration, error) {
	return a.playtime, a.err
}

type linkTestServer struct {
	id  int64
	err error
}

func (s linkTestServer) ConnectedServerID(context.Context, int64) (int64, error) {
	return s.id, s.err
}

func newLinkTestService(repo *linkTestRepository, playtime time.Duration) *LinkVerificationService {
	return NewService(repo, linkTestActivity{playtime: playtime}, linkTestServer{id: 7})
}

func TestRequestRequiresObservedPlayer(t *testing.T) {
	repo := &linkTestRepository{}
	_, err := newLinkTestService(repo, 5*time.Minute).Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("expected player-not-found, got %v", err)
	}
}

func TestRequestRequiresFiveMinutesOfObservedPlaytime(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	_, err := newLinkTestService(repo, 299*time.Second).Request(context.Background(), 1, "discord", " tcp ")
	if !errors.Is(err, ErrPlaytimeRequired) {
		t.Fatalf("expected playtime-required, got %v", err)
	}
}

func TestRequestCreatesPendingLinkAtFiveMinutes(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	link, err := newLinkTestService(repo, 300*time.Second).Request(context.Background(), 1, "discord", "tcp")
	if err != nil {
		t.Fatalf("expected eligible player to create pending link: %v", err)
	}
	if link == nil || repo.pending == nil || repo.pending.PlayerID != 10 {
		t.Fatalf("expected pending link for player 10, got link=%+v pending=%+v", link, repo.pending)
	}
}

func TestRequestDoesNotConvertActivityFailureToPlayerNotFound(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	service := NewService(repo, linkTestActivity{err: errors.New("database unavailable")}, linkTestServer{id: 7})
	_, err := service.Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrLinkCheckUnavailable) {
		t.Fatalf("expected link-check-unavailable, got %v", err)
	}
}
