package linking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ErrPlayerNotFound means Champion has not observed the requested PlayStation name.
var ErrPlayerNotFound = errors.New("player not found")
var ErrAlreadyLinked = errors.New("account already linked")
var ErrPlayerClaimed = errors.New("player already linked")

// PlayerCandidate is a known player returned by the repository.
type PlayerCandidate struct {
	ID          int64
	DayZID      string
	DisplayName string
}

// LinkRecord is a pending or verified Discord-to-DayZ association.
type LinkRecord struct {
	GuildID       int64
	DiscordUserID string
	PlayerID      int64
	RequestedName string
	Status        string
	ChallengeHash string
	ExpiresAt     time.Time
}

const (
	StatusPending  = "PENDING"
	StatusVerified = "VERIFIED"
	StatusRejected = "REJECTED"
	StatusUnlinked = "UNLINKED"
	StatusExpired  = "EXPIRED"
)

// Repository is the linking persistence boundary. SQL stays out of Discord handlers.
type Repository interface {
	FindPlayers(ctx context.Context, guildID int64, name string) ([]PlayerCandidate, error)
	GetByDiscord(ctx context.Context, guildID int64, discordUserID string) (*LinkRecord, error)
	GetByPlayer(ctx context.Context, guildID, playerID int64) (*LinkRecord, error)
	CreatePending(ctx context.Context, link LinkRecord) error
	Unlink(ctx context.Context, guildID int64, discordUserID string) error
}

// LinkVerificationService creates pending links. It intentionally does not
// auto-verify a typed username: current ADM data cannot prove Discord ownership.
type LinkVerificationService struct {
	repo    Repository
	expires time.Duration
}

func NewService(repo Repository) *LinkVerificationService {
	return &LinkVerificationService{repo: repo, expires: 10 * time.Minute}
}

// Request creates a PENDING link after exact case-insensitive player resolution.
func (s *LinkVerificationService) Request(ctx context.Context, guildID int64, discordUserID, username string) (*LinkRecord, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, ErrPlayerNotFound
	}
	if existing, err := s.repo.GetByDiscord(ctx, guildID, discordUserID); err != nil {
		return nil, err
	} else if existing != nil && existing.Status == StatusVerified {
		return existing, ErrAlreadyLinked
	} else if existing != nil && existing.Status == StatusPending && time.Now().Before(existing.ExpiresAt) {
		return existing, nil
	}

	candidates, err := s.repo.FindPlayers(ctx, guildID, username)
	if err != nil {
		return nil, err
	}
	if len(candidates) != 1 {
		return nil, ErrPlayerNotFound
	}
	candidate := candidates[0]
	if claimed, err := s.repo.GetByPlayer(ctx, guildID, candidate.ID); err != nil {
		return nil, err
	} else if claimed != nil && claimed.Status == StatusVerified {
		return nil, ErrPlayerClaimed
	}

	challenge := make([]byte, 24)
	if _, err := rand.Read(challenge); err != nil {
		return nil, err
	}
	link := LinkRecord{
		GuildID:       guildID,
		DiscordUserID: discordUserID,
		PlayerID:      candidate.ID,
		RequestedName: candidate.DisplayName,
		Status:        StatusPending,
		ChallengeHash: hex.EncodeToString(challenge),
		ExpiresAt:     time.Now().Add(s.expires),
	}
	if err := s.repo.CreatePending(ctx, link); err != nil {
		return nil, err
	}
	return &link, nil
}

func (s *LinkVerificationService) Unlink(ctx context.Context, guildID int64, discordUserID string) error {
	return s.repo.Unlink(ctx, guildID, discordUserID)
}

// Status returns the current link for a Discord user without exposing internal IDs.
func (s *LinkVerificationService) Status(ctx context.Context, guildID int64, discordUserID string) (*LinkRecord, error) {
	return s.repo.GetByDiscord(ctx, guildID, discordUserID)
}
