package linking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
)

// ErrPlayerNotFound means Champion has not observed the requested PlayStation name.
var ErrPlayerNotFound = errors.New("player not found")
var ErrAlreadyLinked = errors.New("account already linked")
var ErrPlayerClaimed = errors.New("player already linked")
var ErrPlaytimeRequired = errors.New("minimum observed playtime required")
var ErrLinkCheckUnavailable = errors.New("link check unavailable")
var ErrNoConnectedServer = errors.New("no connected server")
var ErrMultipleConnectedServers = errors.New("multiple connected servers")

// The outcomes below refine the two broad ones above so the player (and an
// admin reading the logs) can tell exactly why a /link did not go through.
// Each wraps its broad parent, so errors.Is(err, ErrLinkCheckUnavailable) /
// errors.Is(err, ErrPlaytimeRequired) keep matching for existing callers.

// ErrInvalidUsername means the typed name cannot be a console username
// (a Discord mention, a URL, far too short/long) - rejected before any lookup.
var ErrInvalidUsername = errors.New("invalid username")

// ErrInstallationNotConfigured means the guild has no active game server to
// verify against (no /server connect or SaaS installation yet).
var ErrInstallationNotConfigured = fmt.Errorf("%w: installation not configured", ErrLinkCheckUnavailable)

// ErrServerSelectionRequired means the guild has several active game servers
// and none is selected as the public server, so it is ambiguous which
// server's activity proves the account.
var ErrServerSelectionRequired = fmt.Errorf("%w: server selection required", ErrLinkCheckUnavailable)

// ErrActivityUnavailable means the activity store could not be read (database
// failure), which is distinct from the player simply having no activity.
var ErrActivityUnavailable = fmt.Errorf("%w: activity data unavailable", ErrLinkCheckUnavailable)

// ErrPlayerNotObserved means the name is known to Champion but no connected
// time has been recorded for it on the selected server.
var ErrPlayerNotObserved = fmt.Errorf("%w: player not observed on the selected server", ErrPlaytimeRequired)

// MinimumObservedPlaytime is how long a player must have been observed
// connected to the selected server before a link request is accepted.
const MinimumObservedPlaytime = 5 * time.Minute

// PlaytimeShortfallError reports some, but not enough, observed playtime.
type PlaytimeShortfallError struct {
	Observed, Required time.Duration
}

func (e *PlaytimeShortfallError) Error() string {
	return fmt.Sprintf("observed %s of required %s", e.Observed.Truncate(time.Second), e.Required)
}

func (e *PlaytimeShortfallError) Unwrap() error { return ErrPlaytimeRequired }

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
	// Verify promotes a guild's pending link for discordUserID to VERIFIED.
	Verify(ctx context.Context, guildID int64, discordUserID string) error
}

// ChallengeRepository backs the disconnect-then-reconnect ADM verification
// challenge and the admin manual-approval fallback. Optional: a Repository
// that doesn't also implement this can still create pending links, but they
// can never complete.
type ChallengeRepository interface {
	// RecordChallengeDisconnect marks the first observed disconnect for a
	// pending link's player after the challenge was issued. Returns false if
	// there was no matching, not-yet-disconnected pending challenge.
	RecordChallengeDisconnect(ctx context.Context, guildID, playerID int64, at time.Time) (bool, error)
	// CompletePendingChallenge completes a pending link whose player
	// disconnected (per RecordChallengeDisconnect) and has now reconnected.
	// Returns the Discord user ID and ok=true on success.
	CompletePendingChallenge(ctx context.Context, guildID, playerID int64, at time.Time) (discordUserID string, ok bool, err error)
	// GetPendingLinkByPlayerName resolves a pending link by DayZ display name,
	// for the admin manual-approval fallback.
	GetPendingLinkByPlayerName(ctx context.Context, guildID int64, name string) (discordUserID string, playerID int64, ok bool, err error)
}

// RoleAssigner assigns the configured @Verified role after a link completes.
// This app serves a single Discord guild per process (mirroring
// KillfeedPublisher's binding), so no guild ID is needed per call.
type RoleAssigner interface {
	AssignVerifiedRole(ctx context.Context, discordUserID string) error
}

// Notifier tells the player their link finished verifying. Completion can
// happen with no active Discord interaction to reply to (the ADM
// disconnect/reconnect challenge completes from background log processing),
// so this is a DM, not an interaction response.
type Notifier interface {
	NotifyVerified(ctx context.Context, discordUserID string, roleAssigned bool) error
}

// LinkVerificationService creates pending links. It intentionally does not
// auto-verify a typed username: current ADM data cannot prove Discord ownership.
// Verification instead comes from ChallengeRepository (an observed ADM
// disconnect-then-reconnect, or an admin's manual approval).
type LinkVerificationService struct {
	repo      Repository
	expires   time.Duration
	activity  ActivityReader
	serverID  ServerResolver
	challenge ChallengeRepository
	roles     RoleAssigner
	notifier  Notifier
}

type ActivityReader interface {
	GetObservedPlaytime(context.Context, int64, int64, int64, time.Time) (time.Duration, error)
}
type ServerResolver interface {
	ConnectedServerID(context.Context, int64) (int64, error)
}

// SetRoleAssigner attaches the role assigner after construction, for callers
// (like app.go) where the Discord client isn't ready yet when the service is
// first built. Optional: unset means role assignment is skipped.
func (s *LinkVerificationService) SetRoleAssigner(r RoleAssigner) {
	if s == nil {
		return
	}
	s.roles = r
}

// SetNotifier attaches the notifier after construction, for the same
// late-binding reason as SetRoleAssigner. Optional: unset means the player
// is not messaged when their link completes.
func (s *LinkVerificationService) SetNotifier(n Notifier) {
	if s == nil {
		return
	}
	s.notifier = n
}

func NewService(repo Repository, extras ...any) *LinkVerificationService {
	s := &LinkVerificationService{repo: repo, expires: 10 * time.Minute}
	for _, extra := range extras {
		if v, ok := extra.(ActivityReader); ok {
			s.activity = v
		}
		if v, ok := extra.(ServerResolver); ok {
			s.serverID = v
		}
		if v, ok := extra.(ChallengeRepository); ok {
			s.challenge = v
		}
		if v, ok := extra.(RoleAssigner); ok {
			s.roles = v
		}
		if v, ok := extra.(Notifier); ok {
			s.notifier = v
		}
	}
	return s
}

// Request creates a PENDING link after exact case-insensitive player resolution.
func (s *LinkVerificationService) Request(ctx context.Context, guildID int64, discordUserID, username string) (*LinkRecord, error) {
	if s == nil || s.repo == nil || s.activity == nil || s.serverID == nil {
		slog.Warn("component=link", "guild", guildID, "server_resolved", false, "database_available", s != nil && s.repo != nil, "activity_query", "not_run", "error_class", "SERVER_CONTEXT_UNAVAILABLE")
		return nil, ErrLinkCheckUnavailable
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, ErrPlayerNotFound
	}
	if !plausibleUsername(username) {
		return nil, ErrInvalidUsername
	}
	if existing, err := s.repo.GetByDiscord(ctx, guildID, discordUserID); err != nil {
		slog.Warn("component=link", "guild", guildID, "server_resolved", false, "database_available", false, "activity_query", "not_run", "error_class", "DATABASE_UNAVAILABLE")
		return nil, ErrLinkCheckUnavailable
	} else if existing != nil && existing.Status == StatusVerified {
		return existing, ErrAlreadyLinked
	} else if existing != nil && existing.Status == StatusPending && time.Now().Before(existing.ExpiresAt) {
		return existing, nil
	}

	candidates, err := s.repo.FindPlayers(ctx, guildID, username)
	if err != nil {
		slog.Warn("component=link", "guild", guildID, "server_resolved", false, "database_available", false, "activity_query", "not_run", "error_class", "PLAYER_LOOKUP_FAILED")
		return nil, ErrLinkCheckUnavailable
	}
	if len(candidates) != 1 {
		return nil, ErrPlayerNotFound
	}
	candidate := candidates[0]
	serverID, serverErr := s.serverID.ConnectedServerID(ctx, guildID)
	if serverErr != nil {
		errorClass, outcome := "SERVER_CONTEXT_UNAVAILABLE", ErrLinkCheckUnavailable
		lower := strings.ToLower(serverErr.Error())
		switch {
		case errors.Is(serverErr, ErrNoConnectedServer) || strings.Contains(lower, "no connected server"):
			errorClass, outcome = "NO_CONNECTED_SERVER", ErrInstallationNotConfigured
		case errors.Is(serverErr, ErrMultipleConnectedServers) || strings.Contains(lower, "multiple connected servers"):
			errorClass, outcome = "MULTIPLE_CONNECTED_SERVERS", ErrServerSelectionRequired
		}
		slog.Warn("component=link", "guild", guildID, "server_resolved", false, "database_available", errorClass != "SERVER_CONTEXT_UNAVAILABLE", "activity_query", "not_run", "error_class", errorClass, "message", sanitizeLinkError(serverErr))
		return nil, outcome
	}
	slog.Debug("component=link", "guild", guildID, "server_resolved", true, "database_available", true, "activity_query", "pending", "error_class", "SERVER_RESOLVED")
	playtime, activityErr := s.activity.GetObservedPlaytime(ctx, guildID, serverID, candidate.ID, time.Now())
	if activityErr != nil {
		var sqlState string
		var stater sqlStater
		if errors.As(activityErr, &stater) {
			sqlState = stater.SQLState()
		}
		slog.Warn("component=link", "stage", "activity_lookup", "guild", guildID, "server_id", serverID, "server_resolved", true, "database_available", false, "activity_query", "failure", "error_class", "ACTIVITY_LOOKUP_FAILED", "sql_state", sqlState, "message", sanitizeLinkError(activityErr))
		return nil, ErrActivityUnavailable
	}
	slog.Info("component=link", "guild", guildID, "server_id", serverID, "server_resolved", true, "activity_query", "success", "observed_seconds", int64(playtime/time.Second))
	if playtime <= 0 {
		return nil, ErrPlayerNotObserved
	}
	if playtime < MinimumObservedPlaytime {
		return nil, &PlaytimeShortfallError{Observed: playtime, Required: MinimumObservedPlaytime}
	}
	if claimed, err := s.repo.GetByPlayer(ctx, guildID, candidate.ID); err != nil {
		return nil, ErrLinkCheckUnavailable
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

// ObserveDisconnect records the first leg of the disconnect-then-reconnect
// verification challenge for playerID's pending link, if one exists. A no-op
// (not an error) when there is no matching pending challenge - most
// disconnects have nothing to do with linking.
func (s *LinkVerificationService) ObserveDisconnect(ctx context.Context, guildID, playerID int64, at time.Time) error {
	if s == nil || s.challenge == nil {
		return nil
	}
	_, err := s.challenge.RecordChallengeDisconnect(ctx, guildID, playerID, at)
	return err
}

// ObserveConnect completes playerID's pending link if it already has an
// observed disconnect (per ObserveDisconnect) - proving the same account
// just disconnected and reconnected, which is the whole challenge. A no-op
// when there is no completable challenge.
func (s *LinkVerificationService) ObserveConnect(ctx context.Context, guildID, playerID int64, at time.Time) error {
	if s == nil || s.challenge == nil {
		return nil
	}
	discordUserID, ok, err := s.challenge.CompletePendingChallenge(ctx, guildID, playerID, at)
	if err != nil || !ok {
		return err
	}
	return s.complete(ctx, guildID, discordUserID)
}

// ApproveManually completes a pending link for the named player without
// requiring the disconnect/reconnect challenge - the admin fallback for when
// a player can't reconnect in time or the ADM pipeline is degraded.
func (s *LinkVerificationService) ApproveManually(ctx context.Context, guildID int64, playerName string) error {
	if s == nil {
		return ErrLinkCheckUnavailable
	}
	if s.challenge == nil {
		return ErrLinkCheckUnavailable
	}
	discordUserID, _, ok, err := s.challenge.GetPendingLinkByPlayerName(ctx, guildID, strings.TrimSpace(playerName))
	if err != nil {
		return ErrLinkCheckUnavailable
	}
	if !ok {
		return ErrPlayerNotFound
	}
	return s.complete(ctx, guildID, discordUserID)
}

// complete promotes the link to VERIFIED, assigns the configured role, and
// DMs the player. Role assignment and notification failures (or either being
// unconfigured) are logged, not fatal - the link itself is the source of
// truth; the role and message are conveniences on top of it.
// Calling Verify here is redundant-but-harmless when reached via
// ObserveConnect (CompletePendingChallenge already flipped player_links in
// its own transaction) and required when reached via ApproveManually (which
// never touches player_links itself).
func (s *LinkVerificationService) complete(ctx context.Context, guildID int64, discordUserID string) error {
	if err := s.repo.Verify(ctx, guildID, discordUserID); err != nil {
		return err
	}
	roleAssigned := false
	if s.roles != nil {
		if err := s.roles.AssignVerifiedRole(ctx, discordUserID); err != nil {
			slog.Warn("component=link", "msg", "verified role assignment failed", "err", err.Error())
		} else {
			roleAssigned = true
		}
	}
	if s.notifier != nil {
		if err := s.notifier.NotifyVerified(ctx, discordUserID, roleAssigned); err != nil {
			slog.Warn("component=link", "msg", "verification notification failed", "err", err.Error())
		}
	}
	return nil
}

// plausibleUsername rejects input that cannot be a console username before it
// reaches the player lookup: Discord mentions, URLs, control characters, and
// lengths no console platform allows. It is deliberately lenient beyond that
// (Xbox gamertags may contain spaces and a "#1234" suffix); an unknown but
// plausible name is reported as ErrPlayerNotFound instead.
func plausibleUsername(name string) bool {
	n := len([]rune(name))
	if n < 3 || n > 32 {
		return false
	}
	if strings.ContainsAny(name, "@<>/\\:") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// sqlStater matches *pgconn.PgError without importing pgx into this package.
type sqlStater interface{ SQLState() string }

// sanitizeLinkError returns a bounded, credential-free error message safe to
// log. Query-execution errors do not normally carry secrets, but connection
// setup errors can echo a DSN, so anything resembling one is redacted.
func sanitizeLinkError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	for _, marker := range []string{"password", "://", "@", "token", "secret"} {
		if strings.Contains(lower, marker) {
			return "redacted"
		}
	}
	const maxLen = 200
	if len(msg) > maxLen {
		msg = msg[:maxLen]
	}
	return msg
}
