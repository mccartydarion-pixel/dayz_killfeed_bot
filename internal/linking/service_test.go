package linking

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type linkTestRepository struct {
	players []PlayerCandidate
	pending *LinkRecord
	claimed *LinkRecord

	// challenge state for a single pending link - enough for these tests,
	// which only ever exercise one link's disconnect/reconnect sequence at a time.
	challengeGuildID    int64
	challengePlayerID   int64
	challengeDiscordID  string
	challengeStatus     string // "", StatusPending, StatusVerified
	challengeExpiresAt  time.Time
	disconnectObserved  bool
	disconnectCallCount int

	verifyCalls         int
	verifyDiscordUserID string
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

func (r *linkTestRepository) Verify(_ context.Context, guildID int64, discordUserID string) error {
	r.verifyCalls++
	r.verifyDiscordUserID = discordUserID
	if r.challengeGuildID == guildID && r.challengeDiscordID == discordUserID {
		r.challengeStatus = StatusVerified
	}
	return nil
}

// setPendingChallenge seeds a single pending link's challenge state, as if
// CreatePending had just run for it.
func (r *linkTestRepository) setPendingChallenge(guildID, playerID int64, discordUserID string, expiresAt time.Time) {
	r.challengeGuildID = guildID
	r.challengePlayerID = playerID
	r.challengeDiscordID = discordUserID
	r.challengeStatus = StatusPending
	r.challengeExpiresAt = expiresAt
	r.disconnectObserved = false
	r.disconnectCallCount = 0
}

func (r *linkTestRepository) RecordChallengeDisconnect(_ context.Context, guildID, playerID int64, at time.Time) (bool, error) {
	if r.challengeStatus != StatusPending || r.challengeGuildID != guildID || r.challengePlayerID != playerID {
		return false, nil
	}
	if at.After(r.challengeExpiresAt) {
		return false, nil
	}
	if r.disconnectObserved {
		r.disconnectCallCount++
		return false, nil
	}
	r.disconnectObserved = true
	r.disconnectCallCount++
	return true, nil
}

func (r *linkTestRepository) CompletePendingChallenge(_ context.Context, guildID, playerID int64, at time.Time) (string, bool, error) {
	if r.challengeStatus != StatusPending || r.challengeGuildID != guildID || r.challengePlayerID != playerID {
		return "", false, nil
	}
	if !r.disconnectObserved || at.After(r.challengeExpiresAt) {
		return "", false, nil
	}
	r.challengeStatus = StatusVerified
	return r.challengeDiscordID, true, nil
}

func (r *linkTestRepository) GetPendingLinkByPlayerName(_ context.Context, guildID int64, name string) (string, int64, bool, error) {
	if r.challengeStatus != StatusPending || r.challengeGuildID != guildID {
		return "", 0, false, nil
	}
	for _, p := range r.players {
		if p.ID == r.challengePlayerID && strings.EqualFold(p.DisplayName, name) {
			return r.challengeDiscordID, r.challengePlayerID, true, nil
		}
	}
	return "", 0, false, nil
}

type spyRoleAssigner struct {
	calls             int
	lastDiscordUserID string
	err               error
}

func (a *spyRoleAssigner) AssignVerifiedRole(_ context.Context, discordUserID string) error {
	a.calls++
	a.lastDiscordUserID = discordUserID
	return a.err
}

type spyNotifier struct {
	calls             int
	lastDiscordUserID string
	lastRoleAssigned  bool
}

func (n *spyNotifier) NotifyVerified(_ context.Context, discordUserID string, roleAssigned bool) error {
	n.calls++
	n.lastDiscordUserID = discordUserID
	n.lastRoleAssigned = roleAssigned
	return nil
}

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

// TestDisconnectThenReconnectCompletesChallenge proves the primary ADM
// verification path: a pending link's player disconnecting and then
// reconnecting completes the link and assigns the configured role.
func TestDisconnectThenReconnectCompletesChallenge(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	roles := &spyRoleAssigner{}
	service := NewService(repo, repo, roles)
	ctx := context.Background()

	if err := service.ObserveDisconnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveDisconnect: %v", err)
	}
	if err := service.ObserveConnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveConnect: %v", err)
	}

	if repo.challengeStatus != StatusVerified {
		t.Fatalf("expected challenge to be verified, got %q", repo.challengeStatus)
	}
	if roles.calls != 1 || roles.lastDiscordUserID != "discord-1" {
		t.Fatalf("expected role assigned once to discord-1, got calls=%d last=%q", roles.calls, roles.lastDiscordUserID)
	}
}

// TestCompletionNotifiesPlayerWithRoleAssignedTrue proves the player is
// notified after their link completes, and told the role was assigned when
// it actually was.
func TestCompletionNotifiesPlayerWithRoleAssignedTrue(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	notifier := &spyNotifier{}
	service := NewService(repo, repo, &spyRoleAssigner{}, notifier)
	ctx := context.Background()

	_ = service.ObserveDisconnect(ctx, 1, 10, time.Now())
	if err := service.ObserveConnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveConnect: %v", err)
	}

	if notifier.calls != 1 || notifier.lastDiscordUserID != "discord-1" || !notifier.lastRoleAssigned {
		t.Fatalf("expected one notification for discord-1 with roleAssigned=true, got calls=%d user=%q roleAssigned=%v", notifier.calls, notifier.lastDiscordUserID, notifier.lastRoleAssigned)
	}
}

// TestCompletionNotifiesPlayerWithRoleAssignedFalseWhenRoleFails proves the
// player is still notified (the link completed) even when role assignment
// itself fails or isn't configured - but is told the role was not assigned.
func TestCompletionNotifiesPlayerWithRoleAssignedFalseWhenRoleFails(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	notifier := &spyNotifier{}
	failingRoles := &spyRoleAssigner{err: errors.New("role not configured")}
	service := NewService(repo, repo, failingRoles, notifier)
	ctx := context.Background()

	_ = service.ObserveDisconnect(ctx, 1, 10, time.Now())
	if err := service.ObserveConnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveConnect: %v", err)
	}

	if notifier.calls != 1 || notifier.lastRoleAssigned {
		t.Fatalf("expected one notification with roleAssigned=false, got calls=%d roleAssigned=%v", notifier.calls, notifier.lastRoleAssigned)
	}
}

// TestConnectWithoutPriorDisconnectDoesNotComplete proves a bare connect
// (no disconnect first) never completes the challenge - it must be a real
// disconnect-then-reconnect sequence, not just any connect event.
func TestConnectWithoutPriorDisconnectDoesNotComplete(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	roles := &spyRoleAssigner{}
	service := NewService(repo, repo, roles)

	if err := service.ObserveConnect(context.Background(), 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveConnect: %v", err)
	}
	if repo.challengeStatus != StatusPending {
		t.Fatalf("expected challenge to remain pending, got %q", repo.challengeStatus)
	}
	if roles.calls != 0 {
		t.Fatalf("expected no role assignment, got %d calls", roles.calls)
	}
}

// TestSecondDisconnectBeforeReconnectIsANoOp proves the disconnect guard: a
// player can't "re-arm" or manipulate the challenge by disconnecting
// repeatedly before ever reconnecting.
func TestSecondDisconnectBeforeReconnectIsANoOp(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	service := NewService(repo, repo, &spyRoleAssigner{})
	ctx := context.Background()

	if err := service.ObserveDisconnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("first ObserveDisconnect: %v", err)
	}
	if err := service.ObserveDisconnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("second ObserveDisconnect: %v", err)
	}
	if repo.disconnectCallCount != 2 {
		t.Fatalf("expected both disconnects to be observed, got %d calls", repo.disconnectCallCount)
	}
	if !repo.disconnectObserved {
		t.Fatal("expected disconnect flag to remain set from the first call")
	}
}

// TestExpiredChallengeCannotBeCompleted proves a disconnect/reconnect after
// the link's expiry window never completes it.
func TestExpiredChallengeCannotBeCompleted(t *testing.T) {
	repo := &linkTestRepository{}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(-time.Minute)) // already expired
	roles := &spyRoleAssigner{}
	service := NewService(repo, repo, roles)
	ctx := context.Background()

	_ = service.ObserveDisconnect(ctx, 1, 10, time.Now())
	if err := service.ObserveConnect(ctx, 1, 10, time.Now()); err != nil {
		t.Fatalf("ObserveConnect: %v", err)
	}
	if repo.challengeStatus != StatusPending {
		t.Fatalf("expected an expired challenge to remain pending, got %q", repo.challengeStatus)
	}
	if roles.calls != 0 {
		t.Fatalf("expected no role assignment for an expired challenge, got %d calls", roles.calls)
	}
}

// TestApproveManuallyCompletesRegardlessOfADMActivity proves the admin
// fallback: it completes a pending link without any disconnect/reconnect
// having been observed at all.
func TestApproveManuallyCompletesRegardlessOfADMActivity(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DisplayName: "TCP"}}}
	repo.setPendingChallenge(1, 10, "discord-1", time.Now().Add(10*time.Minute))
	roles := &spyRoleAssigner{}
	service := NewService(repo, repo, roles)

	if err := service.ApproveManually(context.Background(), 1, "tcp"); err != nil {
		t.Fatalf("ApproveManually: %v", err)
	}
	if repo.verifyCalls != 1 || repo.verifyDiscordUserID != "discord-1" {
		t.Fatalf("expected Verify called once for discord-1, got calls=%d last=%q", repo.verifyCalls, repo.verifyDiscordUserID)
	}
	if roles.calls != 1 {
		t.Fatalf("expected role assigned once, got %d calls", roles.calls)
	}
}

// TestApproveManuallyReportsPlayerNotFoundForNoPendingRequest proves the
// admin fallback surfaces a clear error when there's nothing to approve.
func TestApproveManuallyReportsPlayerNotFoundForNoPendingRequest(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DisplayName: "TCP"}}}
	service := NewService(repo, repo, &spyRoleAssigner{})

	err := service.ApproveManually(context.Background(), 1, "tcp")
	if !errors.Is(err, ErrPlayerNotFound) {
		t.Fatalf("expected player-not-found, got %v", err)
	}
}

func TestSanitizeLinkErrorRedactsConnectionSecrets(t *testing.T) {
	if got := sanitizeLinkError(errors.New("postgres://user:pass@host:5432/db")); got != "redacted" {
		t.Fatalf("expected DSN-like error to be redacted, got %q", got)
	}
	if got := sanitizeLinkError(errors.New("column \"foo\" does not exist")); got == "redacted" {
		t.Fatal("expected an ordinary query error to pass through unredacted")
	}
}

// TestRequestOutcomesAreDistinguishable covers every /link outcome the
// player must be able to tell apart.
func TestRequestOutcomesAreDistinguishable(t *testing.T) {
	player := []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}
	cases := []struct {
		name     string
		players  []PlayerCandidate
		username string
		activity linkTestActivity
		server   linkTestServer
		check    func(error) bool
	}{
		{"player not observed at all", nil, "TCP", linkTestActivity{playtime: time.Hour}, linkTestServer{id: 7},
			func(err error) bool { return errors.Is(err, ErrPlayerNotFound) }},
		{"incorrect username (mention)", player, "<@1234567890>", linkTestActivity{playtime: time.Hour}, linkTestServer{id: 7},
			func(err error) bool { return errors.Is(err, ErrInvalidUsername) }},
		{"incorrect username (too short)", player, "ab", linkTestActivity{playtime: time.Hour}, linkTestServer{id: 7},
			func(err error) bool { return errors.Is(err, ErrInvalidUsername) }},
		{"known name, never observed on server", player, "TCP", linkTestActivity{}, linkTestServer{id: 7},
			func(err error) bool {
				return errors.Is(err, ErrPlayerNotObserved) && errors.Is(err, ErrPlaytimeRequired)
			}},
		{"less than five minutes", player, "TCP", linkTestActivity{playtime: 3*time.Minute + 20*time.Second}, linkTestServer{id: 7},
			func(err error) bool {
				var s *PlaytimeShortfallError
				return errors.As(err, &s) && s.Observed == 3*time.Minute+20*time.Second && s.Required == 5*time.Minute && errors.Is(err, ErrPlaytimeRequired)
			}},
		{"activity data unavailable", player, "TCP", linkTestActivity{err: errors.New("connection reset")}, linkTestServer{id: 7},
			func(err error) bool {
				return errors.Is(err, ErrActivityUnavailable) && errors.Is(err, ErrLinkCheckUnavailable)
			}},
		{"installation not configured", player, "TCP", linkTestActivity{playtime: time.Hour}, linkTestServer{err: fmt.Errorf("%w for guild 1", ErrNoConnectedServer)},
			func(err error) bool { return errors.Is(err, ErrInstallationNotConfigured) }},
		{"several servers, none selected", player, "TCP", linkTestActivity{playtime: time.Hour}, linkTestServer{err: fmt.Errorf("%w for guild 1", ErrMultipleConnectedServers)},
			func(err error) bool { return errors.Is(err, ErrServerSelectionRequired) }},
		{"server lookup database failure", player, "TCP", linkTestActivity{playtime: time.Hour}, linkTestServer{err: errors.New("conn refused")},
			func(err error) bool {
				return errors.Is(err, ErrLinkCheckUnavailable) && !errors.Is(err, ErrInstallationNotConfigured)
			}},
		{"successful at exactly five minutes", player, "tcp", linkTestActivity{playtime: 5 * time.Minute}, linkTestServer{id: 7},
			func(err error) bool { return err == nil }},
	}
	for _, tc := range cases {
		repo := &linkTestRepository{players: tc.players}
		_, err := NewService(repo, tc.activity, tc.server).Request(context.Background(), 1, "discord", tc.username)
		if !tc.check(err) {
			t.Errorf("%s: unexpected result %v", tc.name, err)
		}
		if err != nil && repo.pending != nil {
			t.Errorf("%s: a failed request must not create a pending link", tc.name)
		}
	}
}

func TestPlausibleUsernameAcceptsRealConsoleNames(t *testing.T) {
	for _, name := range []string{"TCP", "Champion_TCP", "dark-wolf-77", "Xbox Gamer Tag", "Gamer#1234"} {
		if !plausibleUsername(name) {
			t.Errorf("%q must be accepted", name)
		}
	}
	for _, name := range []string{"ab", "@TCP", "https://psn", "a\x00b", "abcdefghijklmnopqrstuvwxyz0123456"} {
		if plausibleUsername(name) {
			t.Errorf("%q must be rejected", name)
		}
	}
}
