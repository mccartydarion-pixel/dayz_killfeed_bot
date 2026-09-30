//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

// linkBindingFixture builds the full Guild -> Organization -> Installation ->
// game_servers chain the way the SaaS dashboard connect flow does
// (UpsertForInstallation with Nitrado power state as status), then records
// observed activity for one player on that server.
type linkBindingFixture struct {
	GuildRowID, ServerID, PlayerID int64
	PlayerName                     string
}

func newLinkBindingFixture(t *testing.T, repoStatus string, observed time.Duration) linkBindingFixture {
	t.Helper()
	db := saasIntegrationDB(t)
	ctx := context.Background()
	base := newSaaSFixture(t, db)
	suffix := time.Now().UnixNano()

	// The legacy fixture server is CONNECTED; retire it so the guild has
	// exactly one bound server - the dashboard-connected one.
	servers := NewServerRepository(db.Pool)
	if err := servers.Deactivate(ctx, base.ServerRowID); err != nil {
		t.Fatal(err)
	}
	dash, err := NewSaaSServerRepository(db.Pool).UpsertForInstallation(ctx, base.OrgID, base.GuildRowID, GameServer{
		Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("dash-svc-%d", suffix), Game: "DayZ",
		Platform: "PLAYSTATION", DisplayName: "Dashboard Server", Status: repoStatus, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("PsnPlayer%d", suffix%100000)
	playerID, err := NewPlayerRepository(db.Pool).UpsertPlayer(ctx, base.GuildRowID, fmt.Sprintf("dayz-%d", suffix), name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	activity := NewActivityRepository(db.Pool)
	start := time.Now().Add(-observed)
	if err := activity.Connect(ctx, base.GuildRowID, dash.ID, playerID, start); err != nil {
		t.Fatal(err)
	}
	// Checkpoint every 60s like the live worker so observed time accrues.
	for at := start.Add(time.Minute); at.Before(time.Now()); at = at.Add(time.Minute) {
		if err := activity.CheckpointConnected(ctx, base.GuildRowID, dash.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	return linkBindingFixture{GuildRowID: base.GuildRowID, ServerID: dash.ID, PlayerID: playerID, PlayerName: name}
}

// TestConnectedServerIDResolvesDashboardConnectedServer reproduces the
// production NO_CONNECTED_SERVER failure: the SaaS dashboard writes Nitrado
// power state (ONLINE/OFFLINE) into game_servers.status, which the old
// status allow-list ('connected','ready','active') never matched, while the
// activity worker (which keys on active) kept recording events for it.
func TestConnectedServerIDResolvesDashboardConnectedServer(t *testing.T) {
	for _, status := range []string{"ONLINE", "OFFLINE", "CONNECTED"} {
		t.Run(status, func(t *testing.T) {
			f := newLinkBindingFixture(t, status, 6*time.Minute)
			db := saasIntegrationDB(t)
			got, err := NewServerRepository(db.Pool).ConnectedServerID(context.Background(), f.GuildRowID)
			if err != nil {
				t.Fatalf("expected the active dashboard server to resolve, got %v", err)
			}
			if got != f.ServerID {
				t.Fatalf("resolved server %d, want %d", got, f.ServerID)
			}
		})
	}
}

// TestConnectedServerIDExcludesDeactivatedAndOtherGuilds keeps teardown and
// tenant isolation intact: a deactivated server never resolves, and one
// guild's server is never visible to another guild.
func TestConnectedServerIDExcludesDeactivatedAndOtherGuilds(t *testing.T) {
	f := newLinkBindingFixture(t, "ONLINE", time.Minute)
	other := newLinkBindingFixture(t, "ONLINE", time.Minute)
	db := saasIntegrationDB(t)
	ctx := context.Background()
	servers := NewServerRepository(db.Pool)

	if got, err := servers.ConnectedServerID(ctx, other.GuildRowID); err != nil || got != other.ServerID {
		t.Fatalf("other guild resolved (%d, %v), want its own server %d", got, err, other.ServerID)
	}
	if ids, err := servers.ActiveServerIDs(ctx, f.GuildRowID); err != nil || len(ids) != 1 || ids[0] != f.ServerID {
		t.Fatalf("guild must see only its own active server, got %v %v", ids, err)
	}
	if err := servers.Deactivate(ctx, f.ServerID); err != nil {
		t.Fatal(err)
	}
	if _, err := servers.ConnectedServerID(ctx, f.GuildRowID); !errors.Is(err, linking.ErrNoConnectedServer) {
		t.Fatalf("expected no connected server after teardown, got %v", err)
	}
}

// TestPSNLinkRequestEndToEnd drives the real link service over the real
// repositories: guild -> dashboard server -> activity -> five-minute rule.
func TestPSNLinkRequestEndToEnd(t *testing.T) {
	ctx := context.Background()

	t.Run("five minutes observed creates pending link", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		link, err := svc.Request(ctx, f.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), strings.ToLower(f.PlayerName))
		if err != nil {
			t.Fatalf("expected a pending link, got %v", err)
		}
		if link.Status != linking.StatusPending || link.PlayerID != f.PlayerID {
			t.Fatalf("unexpected link %+v", link)
		}
	})

	t.Run("under five minutes is playtime required not unavailable", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 2*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		_, err := svc.Request(ctx, f.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), f.PlayerName)
		if !errors.Is(err, linking.ErrPlaytimeRequired) {
			t.Fatalf("expected ErrPlaytimeRequired, got %v", err)
		}
	})

	t.Run("challenge completes, verifies and assigns the role", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &recordingRoles{}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		link, err := svc.Status(ctx, f.GuildRowID, discordID)
		if err != nil || link == nil || link.Status != linking.StatusVerified {
			t.Fatalf("expected VERIFIED link, got %+v %v", link, err)
		}
		if len(roles.assigned) != 1 || roles.assigned[0] != discordID {
			t.Fatalf("expected the verified role assigned once to %s, got %v", discordID, roles.assigned)
		}
	})

	t.Run("activity on another guild's server never counts", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		other := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		// f's player name is unknown in other's guild.
		_, err := svc.Request(ctx, other.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), f.PlayerName)
		if !errors.Is(err, linking.ErrPlayerNotFound) {
			t.Fatalf("expected cross-guild lookup to be not found, got %v", err)
		}
	})
}

type recordingRoles struct{ assigned []string }

func (r *recordingRoles) AssignVerifiedRole(_ context.Context, discordUserID string) error {
	r.assigned = append(r.assigned, discordUserID)
	return nil
}

// scriptedRoles fails AssignVerifiedRole with errs[user] (nil = success) and
// records every attempt.
type scriptedRoles struct {
	errs     map[string]error
	attempts map[string]int
}

func (r *scriptedRoles) AssignVerifiedRole(_ context.Context, discordUserID string) error {
	if r.attempts == nil {
		r.attempts = map[string]int{}
	}
	r.attempts[discordUserID]++
	return r.errs[discordUserID]
}

func roleSyncStatus(t *testing.T, ctx context.Context, f linkBindingFixture, discordID string) (string, int) {
	t.Helper()
	db := saasIntegrationDB(t)
	var status *string
	var attempts int
	if err := db.Pool.QueryRow(ctx, `SELECT role_sync_status, role_sync_attempts FROM player_links WHERE guild_id=$1 AND discord_user_id=$2`, f.GuildRowID, discordID).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status == nil {
		return "", attempts
	}
	return *status, attempts
}

// verifyViaChallenge runs Request + disconnect/reconnect for a fresh Discord user.
func verifyViaChallenge(t *testing.T, ctx context.Context, svc *linking.LinkVerificationService, f linkBindingFixture) string {
	t.Helper()
	discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
	if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	return discordID
}

// TestVerifiedRoleFailureSurvivesRestartAndIsReconciled: a VERIFIED link whose
// role assignment failed is recorded FAILED (never mistaken for assigned), and
// a brand-new service instance (a restarted process) re-delivers it.
func TestVerifiedRoleFailureSurvivesRestartAndIsReconciled(t *testing.T) {
	ctx := context.Background()
	f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
	db := saasIntegrationDB(t)
	links := NewLinkRepository(db.Pool)
	failing := &scriptedRoles{errs: map[string]error{}}
	before := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
	before.SetRoleAssigner(failing)
	// Every assignment fails transiently in the "old process".
	failing.errs = map[string]error{}
	discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
	failing.errs[discordID] = errors.New("discord 503")
	if _, err := before.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = before.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second))
	if err := before.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	link, _ := before.Status(ctx, f.GuildRowID, discordID)
	if link == nil || link.Status != linking.StatusVerified {
		t.Fatalf("the link itself must be VERIFIED regardless of the role, got %+v", link)
	}
	if st, attempts := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncFailed || attempts != 1 {
		t.Fatalf("expected FAILED after one attempt, got %q/%d", st, attempts)
	}

	// Restart: a new service with a working Discord. The persisted FAILED
	// state is picked up once the per-link backoff has elapsed.
	if _, err := db.Pool.Exec(ctx, `UPDATE player_links SET role_sync_last_attempt_at=NOW()-INTERVAL '10 minutes' WHERE guild_id=$1 AND discord_user_id=$2`, f.GuildRowID, discordID); err != nil {
		t.Fatal(err)
	}
	working := &scriptedRoles{}
	after := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
	after.SetRoleAssigner(working)
	n, err := after.ReconcileRoles(ctx, f.GuildRowID, 10)
	if err != nil || n != 1 || working.attempts[discordID] != 1 {
		t.Fatalf("expected the failed role re-delivered once, got n=%d err=%v attempts=%v", n, err, working.attempts)
	}
	if st, _ := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncAssigned {
		t.Fatalf("expected ASSIGNED, got %q", st)
	}
	// Idempotent: an ASSIGNED link is never selected again.
	if n, _ := after.ReconcileRoles(ctx, f.GuildRowID, 10); n != 0 || working.attempts[discordID] != 1 {
		t.Fatalf("ASSIGNED link must not be retried (n=%d attempts=%d)", n, working.attempts[discordID])
	}
}

// TestVerifiedRoleSyncStates covers success-at-verification, member gone,
// role not configured, backoff, and untracked pre-migration links.
func TestVerifiedRoleSyncStates(t *testing.T) {
	ctx := context.Background()

	t.Run("assigned at verification", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(&scriptedRoles{})
		id := verifyViaChallenge(t, ctx, svc, f)
		if st, _ := roleSyncStatus(t, ctx, f, id); st != linking.RoleSyncAssigned {
			t.Fatalf("expected ASSIGNED, got %q", st)
		}
	})

	t.Run("member left is terminal", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &scriptedRoles{errs: map[string]error{}}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		roles.errs[discordID] = fmt.Errorf("assign: %w", linking.ErrMemberNotInGuild)
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		_ = svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second))
		_ = svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second))
		if st, _ := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncMemberGone {
			t.Fatalf("expected MEMBER_GONE, got %q", st)
		}
		_, _ = db.Pool.Exec(ctx, `UPDATE player_links SET role_sync_last_attempt_at=NOW()-INTERVAL '1 hour' WHERE guild_id=$1`, f.GuildRowID)
		if n, _ := svc.ReconcileRoles(ctx, f.GuildRowID, 10); n != 0 || roles.attempts[discordID] != 1 {
			t.Fatalf("MEMBER_GONE must not be retried (attempts=%d)", roles.attempts[discordID])
		}
	})

	t.Run("no role configured stays pending", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &scriptedRoles{errs: map[string]error{}}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		roles.errs[discordID] = fmt.Errorf("%w; run /setup verified-role", linking.ErrRoleNotConfigured)
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		_ = svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second))
		_ = svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second))
		if st, attempts := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncPending || attempts != 0 {
			t.Fatalf("expected PENDING with no attempt burned, got %q/%d", st, attempts)
		}
		// Admin configures the role: the reconciler delivers it.
		delete(roles.errs, discordID)
		if n, err := svc.ReconcileRoles(ctx, f.GuildRowID, 10); err != nil || n != 1 {
			t.Fatalf("expected delivery once a role is configured, got n=%d err=%v", n, err)
		}
	})

	t.Run("failed link respects backoff", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &scriptedRoles{errs: map[string]error{}}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		roles.errs[discordID] = errors.New("discord 500")
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		_ = svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second))
		_ = svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second))
		if n, _ := svc.ReconcileRoles(ctx, f.GuildRowID, 10); n != 0 || roles.attempts[discordID] != 1 {
			t.Fatalf("a link attempted just now must wait for its backoff (attempts=%d)", roles.attempts[discordID])
		}
	})

	t.Run("pre-migration verified links are never touched", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		roles := &scriptedRoles{}
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		id := verifyViaChallenge(t, ctx, svc, f)
		// Simulate a link verified before migration 0056.
		if _, err := db.Pool.Exec(ctx, `UPDATE player_links SET role_sync_status=NULL, role_sync_attempts=0 WHERE guild_id=$1 AND discord_user_id=$2`, f.GuildRowID, id); err != nil {
			t.Fatal(err)
		}
		before := roles.attempts[id]
		if n, _ := svc.ReconcileRoles(ctx, f.GuildRowID, 10); n != 0 || roles.attempts[id] != before {
			t.Fatal("untracked (pre-migration) links must not be reconciled automatically")
		}
	})
}

// TestLinkLifecycleAcrossRestartAndAdminFallback: a pending challenge created
// by one process completes in a restarted one; the admin manual fallback
// verifies and assigns the role; neither weakens the five-minute rule.
func TestLinkLifecycleAcrossRestartAndAdminFallback(t *testing.T) {
	ctx := context.Background()
	newService := func(t *testing.T, roles linking.RoleAssigner) *linking.LinkVerificationService {
		db := saasIntegrationDB(t)
		links := NewLinkRepository(db.Pool)
		svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
		svc.SetRoleAssigner(roles)
		return svc
	}

	t.Run("pending challenge survives an application restart", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		first := newService(t, &scriptedRoles{})
		if _, err := first.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := first.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		// Process restarts between the disconnect and the reconnect.
		roles := &scriptedRoles{}
		second := newService(t, roles)
		if err := second.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		link, err := second.Status(ctx, f.GuildRowID, discordID)
		if err != nil || link == nil || link.Status != linking.StatusVerified || roles.attempts[discordID] != 1 {
			t.Fatalf("expected VERIFIED with the role assigned after restart, got %+v err=%v attempts=%v", link, err, roles.attempts)
		}
		if st, _ := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncAssigned {
			t.Fatalf("expected ASSIGNED, got %q", st)
		}
	})

	t.Run("admin manual fallback verifies and assigns the role", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
		discordID := fmt.Sprintf("discord-%d", time.Now().UnixNano())
		roles := &scriptedRoles{}
		svc := newService(t, roles)
		if _, err := svc.Request(ctx, f.GuildRowID, discordID, f.PlayerName); err != nil {
			t.Fatal(err)
		}
		if err := svc.ApproveManually(ctx, f.GuildRowID, f.PlayerName); err != nil {
			t.Fatalf("admin approval failed: %v", err)
		}
		link, _ := svc.Status(ctx, f.GuildRowID, discordID)
		if link == nil || link.Status != linking.StatusVerified || roles.attempts[discordID] != 1 {
			t.Fatalf("expected VERIFIED with role via admin fallback, got %+v attempts=%v", link, roles.attempts)
		}
		if st, _ := roleSyncStatus(t, ctx, f, discordID); st != linking.RoleSyncAssigned {
			t.Fatalf("expected ASSIGNED, got %q", st)
		}
	})

	t.Run("admin fallback cannot verify without a pending request", func(t *testing.T) {
		f := newLinkBindingFixture(t, "ONLINE", 2*time.Minute) // under five minutes: no pending link can exist
		svc := newService(t, &scriptedRoles{})
		if _, err := svc.Request(ctx, f.GuildRowID, fmt.Sprintf("discord-%d", time.Now().UnixNano()), f.PlayerName); !errors.Is(err, linking.ErrPlaytimeRequired) {
			t.Fatalf("expected the five-minute rule, got %v", err)
		}
		if err := svc.ApproveManually(ctx, f.GuildRowID, f.PlayerName); !errors.Is(err, linking.ErrPlayerNotFound) {
			t.Fatalf("admin fallback must require a pending request, got %v", err)
		}
	})
}
