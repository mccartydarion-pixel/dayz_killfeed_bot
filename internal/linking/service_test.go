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

// TestRequestTreatsMissingActivityRowAsZeroPlaytime is the ACTIVITY_LOOKUP_FAILED
// regression test: a player who has never connected to the selected server has
// no player_server_activity row. That is zero observed playtime (playtime
// required), not a database/lookup failure.
func TestRequestTreatsMissingActivityRowAsZeroPlaytime(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	service := NewService(repo, linkTestActivity{playtime: 0, err: nil}, linkTestServer{id: 7})
	_, err := service.Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrPlaytimeRequired) {
		t.Fatalf("expected playtime-required for an unobserved player, got %v", err)
	}
}

func TestRequestEligibleOverFiveMinutes(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	_, err := newLinkTestService(repo, 301*time.Second).Request(context.Background(), 1, "discord", "TCP")
	if err != nil {
		t.Fatalf("expected eligible player over the minimum to succeed, got %v", err)
	}
}

// TestRequestPropagatesResolvedServerAndGuildScope guards against a
// wrong-server/wrong-guild regression: the activity lookup must use the exact
// guild ID passed to Request and the exact server ID the resolver returns.
func TestRequestPropagatesResolvedServerAndGuildScope(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	spy := &scopeSpyActivity{playtime: 5 * time.Minute}
	service := NewService(repo, spy, linkTestServer{id: 42})
	if _, err := service.Request(context.Background(), 99, "discord", "TCP"); err != nil {
		t.Fatalf("expected eligible request to succeed, got %v", err)
	}
	if spy.gotGuildID != 99 || spy.gotServerID != 42 || spy.gotPlayerID != 10 {
		t.Fatalf("expected activity lookup scoped to guild=99 server=42 player=10, got guild=%d server=%d player=%d", spy.gotGuildID, spy.gotServerID, spy.gotPlayerID)
	}
}

type scopeSpyActivity struct {
	playtime                             time.Duration
	gotGuildID, gotServerID, gotPlayerID int64
}

func (a *scopeSpyActivity) GetObservedPlaytime(_ context.Context, guildID, serverID, playerID int64, _ time.Time) (time.Duration, error) {
	a.gotGuildID, a.gotServerID, a.gotPlayerID = guildID, serverID, playerID
	return a.playtime, nil
}

func TestSanitizeLinkErrorRedactsConnectionSecrets(t *testing.T) {
	if got := sanitizeLinkError(errors.New("postgres://user:pass@host:5432/db")); got != "redacted" {
		t.Fatalf("expected DSN-like error to be redacted, got %q", got)
	}
	if got := sanitizeLinkError(errors.New("column \"foo\" does not exist")); got == "redacted" {
		t.Fatal("expected an ordinary query error to pass through unredacted")
	}
}
