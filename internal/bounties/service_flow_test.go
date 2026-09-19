package bounties

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeStore is an in-memory Store. players/servers describe what exists; the
// counters and captured values let tests prove what the Service did (and did not).
type fakeStore struct {
	players map[[2]int64]string // (guild, player) -> display name
	servers map[int64]struct {
		guild int64
		org   *int64
	}

	created   []repository.Bounty
	createErr error
	nextID    int64

	claimResult []repository.Bounty
	claimErr    error
	lastClaim   repository.KillClaim
	claimCalls  int

	expired []repository.Bounty

	automatic  *repository.Bounty
	upgraded   *repository.Bounty
	upgradeErr error

	increased *repository.Bounty
	cancelled *repository.Bounty
	storeErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		players: map[[2]int64]string{{1, 10}: "Target", {1, 11}: "Hunter", {2, 20}: "OtherGuildPlayer"},
		servers: map[int64]struct {
			guild int64
			org   *int64
		}{100: {1, nil}, 101: {1, ptr(7)}, 200: {2, nil}},
	}
}

func ptr(v int64) *int64 { return &v }

func (f *fakeStore) Create(_ context.Context, b repository.Bounty, creator string) (*repository.Bounty, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	b.ID = f.nextID
	b.Status = repository.BountyActive
	f.created = append(f.created, b)
	return &b, nil
}
func (f *fakeStore) Increase(context.Context, int64, int64, int64) (*repository.Bounty, error) {
	return f.increased, f.storeErr
}
func (f *fakeStore) Cancel(context.Context, int64, int64) (*repository.Bounty, error) {
	return f.cancelled, f.storeErr
}
func (f *fakeStore) UpgradeReturning(context.Context, int64, int64, int) (*repository.Bounty, error) {
	if f.upgradeErr != nil {
		return nil, f.upgradeErr
	}
	return f.upgraded, nil
}
func (f *fakeStore) GetActiveAutomatic(context.Context, int64, int64) (*repository.Bounty, error) {
	return f.automatic, nil
}
func (f *fakeStore) ClaimForKill(_ context.Context, c repository.KillClaim) ([]repository.Bounty, error) {
	f.claimCalls++
	f.lastClaim = c
	return f.claimResult, f.claimErr
}
func (f *fakeStore) ExpireDue(context.Context, time.Time) ([]repository.Bounty, error) {
	return f.expired, f.storeErr
}
func (f *fakeStore) PlayerName(_ context.Context, guildID, playerID int64) (string, bool, error) {
	name, ok := f.players[[2]int64{guildID, playerID}]
	return name, ok, nil
}
func (f *fakeStore) ServerScope(_ context.Context, serverID int64) (int64, *int64, bool, error) {
	s, ok := f.servers[serverID]
	return s.guild, s.org, ok, nil
}

type recordingNotifier struct{ events []Event }

func (r *recordingNotifier) Notify(e Event) { r.events = append(r.events, e) }

type panickingNotifier struct{}

func (panickingNotifier) Notify(Event) { panic("discord notifier bug") }

func newService() (*Service, *fakeStore, *recordingNotifier) {
	store, n := newFakeStore(), &recordingNotifier{}
	return NewService(store, n), store, n
}

// --- A/B: placement ------------------------------------------------------------------

func TestPlaceCreatesBountyAndReportsIt(t *testing.T) {
	svc, store, n := newService()
	b, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 10, Amount: 250000, PlacedBy: "u1"})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if b.RewardPoints != 250000 || b.ServerID != 100 || b.TargetPlayerID != 10 || b.CreatedByType != repository.BountyAdmin {
		t.Fatalf("unexpected bounty: %+v", b)
	}
	if len(store.created) != 1 || len(n.events) != 1 {
		t.Fatalf("expected 1 stored and 1 reported, got %d/%d", len(store.created), len(n.events))
	}
	if e := n.events[0]; e.Kind != EventPlaced || e.Target != "Target" || e.Amount != 250000 || e.ServerID != 100 || e.GuildID != 1 {
		t.Fatalf("unexpected event: %+v", e)
	}
}

func TestPlaceRejectsInvalidAmountWithoutTouchingTheStore(t *testing.T) {
	for _, amount := range []int64{0, -1, -250000, MaxAmount + 1} {
		svc, store, n := newService()
		if _, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 10, Amount: amount}); !errors.Is(err, ErrInvalidAmount) {
			t.Fatalf("amount %d: expected ErrInvalidAmount, got %v", amount, err)
		}
		if len(store.created) != 0 || len(n.events) != 0 {
			t.Fatalf("amount %d: nothing may be stored or reported", amount)
		}
	}
}

// --- C: tenant / server isolation --------------------------------------------------------

func TestPlaceEnforcesGuildServerAndTenantIsolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  PlaceRequest
		want error
	}{
		{"target from another guild", PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 20, Amount: 10}, ErrTargetNotFound},
		{"unknown target", PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 999, Amount: 10}, ErrTargetNotFound},
		{"server of another guild", PlaceRequest{GuildID: 1, ServerID: 200, TargetPlayerID: 10, Amount: 10}, ErrServerNotInGuild},
		{"unknown server", PlaceRequest{GuildID: 1, ServerID: 999, TargetPlayerID: 10, Amount: 10}, ErrServerNotInGuild},
		{"server claimed by a different organization", PlaceRequest{GuildID: 1, ServerID: 101, OrganizationID: 8, TargetPlayerID: 10, Amount: 10}, ErrForbiddenTenant},
		{"organization caller without a server", PlaceRequest{GuildID: 1, OrganizationID: 7, TargetPlayerID: 10, Amount: 10}, ErrServerRequired},
		{"expiry in the past", PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 10, Amount: 10, ExpiresAt: timePtr(time.Now().Add(-time.Minute))}, ErrInvalidExpiry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, n := newService()
			if _, err := svc.Place(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
			if len(store.created) != 0 || len(n.events) != 0 {
				t.Fatal("a rejected placement must store and report nothing")
			}
		})
	}

	// Positive controls: the organization that owns the server, and an unclaimed
	// (NULL-organization) server in the right guild.
	svc, _, _ := newService()
	if _, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 101, OrganizationID: 7, TargetPlayerID: 10, Amount: 10}); err != nil {
		t.Fatalf("the owning organization must be allowed: %v", err)
	}
	if _, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 100, OrganizationID: 7, TargetPlayerID: 10, Amount: 10}); err != nil {
		t.Fatalf("an unclaimed server in the guild must be allowed: %v", err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// --- D: claim ----------------------------------------------------------------------------------

func TestClaimForKillReportsTotalCountWeaponAndDistanceAfterCommit(t *testing.T) {
	svc, store, n := newService()
	store.claimResult = []repository.Bounty{{ID: 1, RewardPoints: 100000, GuildID: 1}, {ID: 2, RewardPoints: 50000, GuildID: 1}}
	dist := 86.4

	res, err := svc.ClaimForKill(context.Background(), KillInput{
		GuildID: 1, ServerID: 100, VictimPlayerID: 10, KillerPlayerID: 11, KillID: 5, SeasonID: 3,
		At: time.Now(), HunterName: "Hunter", TargetName: "Target", Weapon: "M4-A1", Distance: &dist,
	})
	if err != nil || res.Count != 2 || res.Total != 150000 {
		t.Fatalf("expected 2 bounties / 150000, got %+v err=%v", res, err)
	}
	if store.lastClaim.ServerID != 100 || store.lastClaim.KillerPlayerID != 11 || store.lastClaim.VictimPlayerID != 10 || store.lastClaim.KillID != 5 {
		t.Fatalf("the claim must carry the kill's server and players: %+v", store.lastClaim)
	}
	if len(n.events) != 1 {
		t.Fatalf("expected exactly one CLAIMED event, got %d", len(n.events))
	}
	e := n.events[0]
	if e.Kind != EventClaimed || e.Hunter != "Hunter" || e.Target != "Target" || e.Amount != 150000 || e.Count != 2 || e.Weapon != "M4-A1" || e.Distance == nil || *e.Distance != 86.4 || e.KillServerID != 100 {
		t.Fatalf("unexpected claim event: %+v", e)
	}
}

func TestClaimForKillWithNothingToClaimReportsNothing(t *testing.T) {
	svc, _, n := newService()
	res, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, ServerID: 100, VictimPlayerID: 10, KillerPlayerID: 11})
	if err != nil || res.Count != 0 || len(n.events) != 0 {
		t.Fatalf("expected no claim and no event, got %+v err=%v events=%d", res, err, len(n.events))
	}
}

func TestClaimForKillStoreErrorReportsNothing(t *testing.T) {
	svc, store, n := newService()
	store.claimErr = errors.New("db down")
	if _, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, VictimPlayerID: 10, KillerPlayerID: 11}); err == nil {
		t.Fatal("expected the store error")
	}
	if len(n.events) != 0 {
		t.Fatal("nothing committed, so nothing may be reported")
	}
}

// --- P: Discord failure never undoes or repeats a committed change -----------------------------

func TestNotifierPanicNeverAffectsCommittedState(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, panickingNotifier{})
	store.claimResult = []repository.Bounty{{ID: 1, RewardPoints: 1000, GuildID: 1}}

	res, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, VictimPlayerID: 10, KillerPlayerID: 11})
	if err != nil || res.Count != 1 || res.Total != 1000 {
		t.Fatalf("a panicking notifier must not affect the claim result: %+v err=%v", res, err)
	}
	if store.claimCalls != 1 {
		t.Fatalf("the claim must run exactly once (never retried), ran %d times", store.claimCalls)
	}
	if _, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 10, Amount: 5}); err != nil {
		t.Fatalf("placement must succeed despite a panicking notifier: %v", err)
	}
	if len(store.created) != 1 {
		t.Fatal("the bounty must stay stored")
	}
}

// With no notifier at all (no route / no Discord) everything still works.
func TestServiceWorksWithoutANotifier(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store, nil)
	store.claimResult = []repository.Bounty{{ID: 1, RewardPoints: 10, GuildID: 1}}
	if res, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, VictimPlayerID: 10, KillerPlayerID: 11}); err != nil || res.Count != 1 {
		t.Fatalf("claim without a notifier: %+v %v", res, err)
	}
	if _, err := svc.Place(context.Background(), PlaceRequest{GuildID: 1, ServerID: 100, TargetPlayerID: 10, Amount: 5}); err != nil {
		t.Fatalf("place without a notifier: %v", err)
	}
	var nilSvc *Service
	nilSvc.SetNotifier(nil)
}

// --- increase / cancel / sweep -------------------------------------------------------------------------

func TestIncreaseCancelAndSweepReportCommittedChanges(t *testing.T) {
	svc, store, n := newService()
	store.increased = &repository.Bounty{ID: 1, GuildID: 1, ServerID: 100, TargetPlayerID: 10, RewardPoints: 300}
	if _, err := svc.Increase(context.Background(), 1, 1, 300); err != nil {
		t.Fatal(err)
	}
	store.cancelled = &repository.Bounty{ID: 2, GuildID: 1, TargetPlayerID: 10, RewardPoints: 100}
	if _, err := svc.Cancel(context.Background(), 1, 2); err != nil {
		t.Fatal(err)
	}
	store.expired = []repository.Bounty{{ID: 3, GuildID: 1, TargetPlayerID: 10, RewardPoints: 7}, {ID: 4, GuildID: 1, ServerID: 100, TargetPlayerID: 10, RewardPoints: 9}}
	if count, err := svc.Sweep(context.Background(), time.Now()); err != nil || count != 2 {
		t.Fatalf("expected 2 expiries, got %d %v", count, err)
	}
	kinds := []EventKind{EventIncreased, EventCancelled, EventExpired, EventExpired}
	if len(n.events) != len(kinds) {
		t.Fatalf("expected %d events, got %+v", len(kinds), n.events)
	}
	for i, k := range kinds {
		if n.events[i].Kind != k || n.events[i].Target != "Target" {
			t.Fatalf("event %d: %+v", i, n.events[i])
		}
	}

	// A rejected change reports nothing.
	svc2, store2, n2 := newService()
	store2.storeErr = repository.ErrBountyNotFound
	if _, err := svc2.Increase(context.Background(), 1, 1, 5); !errors.Is(err, repository.ErrBountyNotFound) {
		t.Fatalf("expected not-found, got %v", err)
	}
	if _, err := svc2.Increase(context.Background(), 1, 1, 0); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("expected invalid amount, got %v", err)
	}
	if len(n2.events) != 0 {
		t.Fatal("a rejected change must report nothing")
	}
}

// --- streak (automatic) bounties: the pre-existing rule -----------------------------------------------------

func TestNoteStreakKeepsTheExistingAutomaticBountyRule(t *testing.T) {
	in := StreakInput{GuildID: 1, ServerID: 100, PlayerID: 11, PlayerName: "Hunter", At: time.Now()}

	// Below the first tier: nothing.
	svc, store, n := newService()
	in.Streak = 9
	svc.NoteStreak(context.Background(), in)
	if len(store.created) != 0 || len(n.events) != 0 {
		t.Fatal("below the first streak tier nothing is created")
	}

	// First tier: a guild-wide AUTOMATIC bounty (never server-scoped) is created.
	in.Streak = 10
	svc.NoteStreak(context.Background(), in)
	if len(store.created) != 1 || store.created[0].RewardPoints != 500 || store.created[0].CreatedByType != repository.BountyAutomatic || store.created[0].ServerID != 0 {
		t.Fatalf("expected a 500-point guild-wide automatic bounty, got %+v", store.created)
	}
	if len(n.events) != 1 || n.events[0].Kind != EventPlaced || !n.events[0].Automatic || n.events[0].KillServerID != 100 || n.events[0].Amount != 500 {
		t.Fatalf("unexpected placed event: %+v", n.events)
	}

	// A higher tier raises the existing automatic bounty; the same tier does not.
	store.automatic = &repository.Bounty{ID: 9, GuildID: 1, RewardPoints: 500}
	store.upgraded = &repository.Bounty{ID: 9, GuildID: 1, RewardPoints: 750}
	in.Streak = 15
	svc.NoteStreak(context.Background(), in)
	if len(store.created) != 1 {
		t.Fatal("an existing automatic bounty must be raised, not duplicated")
	}
	if len(n.events) != 2 || n.events[1].Kind != EventIncreased || n.events[1].Amount != 750 || !n.events[1].Automatic {
		t.Fatalf("expected an increase to 750, got %+v", n.events)
	}
	in.Streak = 10
	svc.NoteStreak(context.Background(), in)
	if len(n.events) != 2 {
		t.Fatal("a lower/equal tier must not raise or announce anything")
	}

	// A concurrent kill that already created it (unique index): silent.
	svc2, store2, n2 := newService()
	store2.createErr = repository.ErrDuplicate
	in.Streak = 10
	svc2.NoteStreak(context.Background(), in)
	if len(n2.events) != 0 {
		t.Fatal("a duplicate automatic bounty must be silent")
	}
}
