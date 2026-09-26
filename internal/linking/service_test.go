package linking

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type linkTestRepository struct {
	players   []PlayerCandidate
	pending   *LinkRecord
	claimed   *LinkRecord
	byDiscord *LinkRecord

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
	return r.byDiscord, nil
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
	ids []int64
	err error
}

func (s linkTestServer) ActiveServerIDs(context.Context, int64) ([]int64, error) {
	return s.ids, s.err
}

func newLinkTestService(repo *linkTestRepository, playtime time.Duration) *LinkVerificationService {
	return NewService(repo, linkTestActivity{playtime: playtime}, linkTestServer{ids: []int64{7}})
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
	service := NewService(repo, linkTestActivity{err: errors.New("database unavailable")}, linkTestServer{ids: []int64{7}})
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
	service := NewService(repo, linkTestActivity{playtime: 0, err: nil}, linkTestServer{ids: []int64{7}})
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
	service := NewService(repo, spy, linkTestServer{ids: []int64{42}})
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

// perServerActivity returns a fixed playtime per server ID.
type perServerActivity map[int64]time.Duration

func (a perServerActivity) GetObservedPlaytime(_ context.Context, _, serverID, _ int64, _ time.Time) (time.Duration, error) {
	return a[serverID], nil
}

// TestRequestWithNoActiveServerIsNotConfigured is the LINK CHECK UNAVAILABLE
// regression: a guild with no active server is a configuration state with its
// own error, not a backend outage.
func TestRequestWithNoActiveServerIsNotConfigured(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	_, err := NewService(repo, linkTestActivity{playtime: time.Hour}, linkTestServer{}).Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrNoConnectedServer) || errors.Is(err, ErrLinkCheckUnavailable) {
		t.Fatalf("expected no-connected-server, got %v", err)
	}
	if repo.pending != nil {
		t.Fatal("no pending link may be created without a server")
	}
}

// TestRequestServerLookupFailureIsUnavailable: only a real lookup failure is
// reported as unavailable.
func TestRequestServerLookupFailureIsUnavailable(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	_, err := NewService(repo, linkTestActivity{playtime: time.Hour}, linkTestServer{err: errors.New("connection refused")}).Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrLinkCheckUnavailable) {
		t.Fatalf("expected link-check-unavailable, got %v", err)
	}
}

// TestRequestWithMultipleActiveServersUsesBestSingleServer: a multi-server
// guild no longer fails every link. The best single server counts; playtime is
// never summed across servers.
func TestRequestWithMultipleActiveServersUsesBestSingleServer(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	servers := linkTestServer{ids: []int64{3, 4}}
	if _, err := NewService(repo, perServerActivity{3: time.Minute, 4: 6 * time.Minute}, servers).Request(context.Background(), 1, "discord", "TCP"); err != nil {
		t.Fatalf("expected pending link from server 4, got %v", err)
	}
	repo2 := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}}
	_, err := NewService(repo2, perServerActivity{3: 3 * time.Minute, 4: 3 * time.Minute}, servers).Request(context.Background(), 1, "discord", "TCP")
	if !errors.Is(err, ErrPlaytimeRequired) {
		t.Fatalf("3+3 minutes on two servers must not satisfy the 5-minute rule, got %v", err)
	}
}

// TestRequestAlreadyLinkedAndClaimed covers the two ownership refusals.
func TestRequestAlreadyLinkedAndClaimed(t *testing.T) {
	repo := &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}, byDiscord: &LinkRecord{Status: StatusVerified}}
	if _, err := newLinkTestService(repo, time.Hour).Request(context.Background(), 1, "discord", "TCP"); !errors.Is(err, ErrAlreadyLinked) {
		t.Fatalf("expected already-linked, got %v", err)
	}
	repo = &linkTestRepository{players: []PlayerCandidate{{ID: 10, DayZID: "tcp-id", DisplayName: "TCP"}}, claimed: &LinkRecord{DiscordUserID: "someone-else", Status: StatusVerified}}
	if _, err := newLinkTestService(repo, time.Hour).Request(context.Background(), 1, "discord", "TCP"); !errors.Is(err, ErrPlayerClaimed) {
		t.Fatalf("expected player-claimed, got %v", err)
	}
	if repo.pending != nil {
		t.Fatal("a claimed player must not get a second pending link")
	}
}
