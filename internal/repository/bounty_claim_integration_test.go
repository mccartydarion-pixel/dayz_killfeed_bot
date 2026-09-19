//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// bountyFix is a two-guild world: guild A has two servers (A1, A2) and three
// players (target, hunter, bystander); guild B has one server and one player.
type bountyFix struct {
	t                                 *testing.T
	ctx                               context.Context
	pool                              *pgxpool.Pool
	repo                              *BountyRepository
	kills                             *KillRepository
	suffix                            int64
	killSeq                           atomic.Int64
	guildA, guildB                    int64
	serverA1, serverA2, serverB1      int64
	target, hunter, bystander, otherB int64
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newBountyFix(t *testing.T) *bountyFix {
	t.Helper()
	db := saasIntegrationDB(t)
	ctx := context.Background()
	f := &bountyFix{t: t, ctx: ctx, pool: db.Pool, repo: NewBountyRepository(db.Pool), kills: NewKillRepository(db.Pool), suffix: time.Now().UnixNano()}
	guilds, players, servers := NewGuildRepository(db.Pool), NewPlayerRepository(db.Pool), NewServerRepository(db.Pool)

	var err error
	f.guildA, err = guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("bounty-A-%d", f.suffix)})
	must(t, err)
	f.guildB, err = guilds.UpsertGuild(ctx, GuildRecord{DiscordGuildID: fmt.Sprintf("bounty-B-%d", f.suffix)})
	must(t, err)
	mkServer := func(guild int64, name string) int64 {
		s, err := servers.UpsertGameServer(ctx, GameServer{GuildID: guild, Provider: "NITRADO", ProviderServiceID: fmt.Sprintf("%s-%d", name, f.suffix), Game: "DAYZ", Platform: "PLAYSTATION", DisplayName: name, Status: "ACTIVE", Active: true})
		must(t, err)
		return s.ID
	}
	f.serverA1, f.serverA2, f.serverB1 = mkServer(f.guildA, "A1"), mkServer(f.guildA, "A2"), mkServer(f.guildB, "B1")
	mkPlayer := func(guild int64, name string) int64 {
		id, err := players.UpsertPlayer(ctx, guild, fmt.Sprintf("dz-%s-%d", name, f.suffix), name, time.Now())
		must(t, err)
		return id
	}
	f.target, f.hunter, f.bystander = mkPlayer(f.guildA, "Target"), mkPlayer(f.guildA, "Hunter"), mkPlayer(f.guildA, "Bystander")
	f.otherB = mkPlayer(f.guildB, "OtherGuild")
	return f
}

// kill inserts a real kills row (claimed_kill_id references it) and returns its id.
func (f *bountyFix) kill(guild, server, killer, victim int64) int64 {
	f.t.Helper()
	id, err := f.kills.InsertKillReturning(f.ctx, KillRecord{
		GuildID: guild, ServerID: server, SessionID: "s", Fingerprint: fmt.Sprintf("bounty-kill-%d-%d", f.suffix, f.killSeq.Add(1)),
		KillerPlayerID: killer, VictimPlayerID: victim,
	})
	must(f.t, err)
	return id
}

// bounty creates an ACTIVE bounty; server 0 = guild-wide.
func (f *bountyFix) bounty(guild, server, target, amount int64, typ string) *Bounty {
	f.t.Helper()
	starts := time.Now().UTC().Add(-time.Hour)
	b, err := f.repo.Create(f.ctx, Bounty{GuildID: guild, ServerID: server, TargetPlayerID: target, CreatedByType: typ, RewardPoints: amount, StartsAt: &starts}, "tester")
	must(f.t, err)
	return b
}

func (f *bountyFix) claim(server, killer, victim int64) []Bounty {
	f.t.Helper()
	got, err := f.repo.ClaimForKill(f.ctx, KillClaim{GuildID: f.guildA, ServerID: server, KillerPlayerID: killer, VictimPlayerID: victim, KillID: f.kill(f.guildA, server, killer, victim), At: time.Now().UTC()})
	must(f.t, err)
	return got
}

func (f *bountyFix) status(id int64) string {
	var s string
	must(f.t, f.pool.QueryRow(f.ctx, `SELECT status FROM bounties WHERE id=$1`, id).Scan(&s))
	return s
}

func (f *bountyFix) points(player int64) (lifetime int64, txs int64) {
	err := f.pool.QueryRow(f.ctx, `SELECT COALESCE((SELECT lifetime_points FROM player_points WHERE guild_id=$1 AND player_id=$2),0), (SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND reason_type='BOUNTY_CLAIM')`, f.guildA, player).Scan(&lifetime, &txs)
	must(f.t, err)
	return lifetime, txs
}

func ids(bs []Bounty) []int64 {
	out := make([]int64, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// --- schema rules -------------------------------------------------------------------------

// Several manual bounties stack on one target; the streak-driven AUTOMATIC bounty
// keeps its "at most one active per target" rule.
func TestBountyStackingSchemaRules(t *testing.T) {
	f := newBountyFix(t)
	f.bounty(f.guildA, 0, f.target, 100, BountyAdmin)
	f.bounty(f.guildA, f.serverA1, f.target, 200, BountyAdmin)
	f.bounty(f.guildA, f.serverA1, f.target, 300, BountyAdmin) // same target, same server: stacks

	first := f.bounty(f.guildA, 0, f.target, 500, BountyAutomatic)
	starts := time.Now().UTC()
	if _, err := f.repo.Create(f.ctx, Bounty{GuildID: f.guildA, TargetPlayerID: f.target, CreatedByType: BountyAutomatic, RewardPoints: 750, StartsAt: &starts}, ""); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("a second ACTIVE automatic bounty on the same target must be rejected, got %v", err)
	}
	// Once the automatic bounty is no longer active a new one may be created.
	if _, err := f.repo.Cancel(f.ctx, f.guildA, first.ID); err != nil {
		t.Fatal(err)
	}
	f.bounty(f.guildA, 0, f.target, 750, BountyAutomatic)

	total, count, err := f.repo.ActiveTotal(f.ctx, f.guildA, f.target)
	if err != nil || total != 100+200+300+750 || count != 4 {
		t.Fatalf("expected 4 stacked bounties totalling 1350, got total=%d count=%d err=%v", total, count, err)
	}
}

// A pre-existing bounty (no server_id, exactly what the old code wrote) stays
// guild-wide: claimable on any server of its guild.
func TestBountyLegacyGuildWideRowIsClaimableOnAnyServer(t *testing.T) {
	f := newBountyFix(t)
	starts := time.Now().UTC().Add(-time.Hour)
	var id int64
	must(t, f.pool.QueryRow(f.ctx, `INSERT INTO bounties(guild_id,target_player_id,created_by_type,status,reward_points,starts_at) VALUES($1,$2,'ADMIN','ACTIVE',400,$3) RETURNING id`, f.guildA, f.target, starts).Scan(&id))
	got := f.claim(f.serverA2, f.hunter, f.target)
	if len(got) != 1 || got[0].ID != id || got[0].ServerID != 0 {
		t.Fatalf("expected the legacy guild-wide bounty claimed on server A2, got %+v", got)
	}
}

// --- D/J/K: claim, stacking, server scope -------------------------------------------------

func TestBountyClaimStacksEveryEligibleBounty(t *testing.T) {
	f := newBountyFix(t)
	wide := f.bounty(f.guildA, 0, f.target, 100, BountyAdmin)
	onA1 := f.bounty(f.guildA, f.serverA1, f.target, 200, BountyAdmin)
	onA1b := f.bounty(f.guildA, f.serverA1, f.target, 50, BountyAdmin)
	onA2 := f.bounty(f.guildA, f.serverA2, f.target, 400, BountyAdmin)

	got := f.claim(f.serverA1, f.hunter, f.target)
	if want := []int64{wide.ID, onA1.ID, onA1b.ID}; fmt.Sprint(ids(got)) != fmt.Sprint(want) {
		t.Fatalf("expected the guild-wide and both server-A1 bounties claimed together, got %v want %v", ids(got), want)
	}
	var total int64
	for _, b := range got {
		total += b.RewardPoints
		if b.Status != BountyClaimed || b.ClaimedByPlayerID == nil || *b.ClaimedByPlayerID != f.hunter || b.ClaimedKillID == nil || b.ClaimedAt == nil {
			t.Fatalf("claimed bounty must carry the claimant, kill and time: %+v", b)
		}
	}
	if total != 350 {
		t.Fatalf("expected the stacked total 350, got %d", total)
	}
	if f.status(onA2.ID) != BountyActive {
		t.Fatal("the server-A2 bounty must stay ACTIVE after a kill on A1")
	}
	if life, txs := f.points(f.hunter); life != 350 || txs != 3 {
		t.Fatalf("expected 350 points in 3 transactions awarded once, got lifetime=%d txs=%d", life, txs)
	}

	// A later kill on A2 claims the remaining server-A2 bounty.
	if again := f.claim(f.serverA2, f.hunter, f.target); len(again) != 1 || again[0].ID != onA2.ID {
		t.Fatalf("expected the A2 bounty claimed on A2, got %v", ids(again))
	}
}

// K: a bounty for server A can never be claimed by a kill on server B.
func TestBountyServerScopeIsolation(t *testing.T) {
	f := newBountyFix(t)
	scoped := f.bounty(f.guildA, f.serverA1, f.target, 500, BountyAdmin)

	if got := f.claim(f.serverA2, f.hunter, f.target); len(got) != 0 {
		t.Fatalf("a kill on A2 must not claim an A1 bounty, got %v", ids(got))
	}
	if got := f.claim(0, f.hunter, f.target); len(got) != 0 {
		t.Fatalf("a kill with no server must not claim a server-scoped bounty, got %v", ids(got))
	}
	if f.status(scoped.ID) != BountyActive {
		t.Fatal("the scoped bounty must still be ACTIVE")
	}
	if got := f.claim(f.serverA1, f.hunter, f.target); len(got) != 1 {
		t.Fatalf("a kill on A1 must claim it, got %v", ids(got))
	}
}

// C: a kill in another guild can never claim this guild's bounty.
func TestBountyCrossGuildIsolation(t *testing.T) {
	f := newBountyFix(t)
	b := f.bounty(f.guildA, 0, f.target, 500, BountyAdmin)
	got, err := f.repo.ClaimForKill(f.ctx, KillClaim{GuildID: f.guildB, ServerID: f.serverB1, KillerPlayerID: f.otherB, VictimPlayerID: f.target, KillID: f.kill(f.guildB, f.serverB1, f.otherB, f.target), At: time.Now().UTC()})
	must(t, err)
	if len(got) != 0 || f.status(b.ID) != BountyActive {
		t.Fatalf("guild B must not claim guild A's bounty, got %v", ids(got))
	}
	// Guild-scoped mutations are guild-scoped too.
	if _, err := f.repo.Cancel(f.ctx, f.guildB, b.ID); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("cancelling another guild's bounty must fail, got %v", err)
	}
	if _, err := f.repo.Increase(f.ctx, f.guildB, b.ID, 9999); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("increasing another guild's bounty must fail, got %v", err)
	}
}

// E/F/G: invalid claims never claim (the caller only offers persisted PvP kills,
// and the repository refuses the rest itself).
func TestBountyInvalidClaimsAreRefused(t *testing.T) {
	f := newBountyFix(t)
	b := f.bounty(f.guildA, 0, f.target, 500, BountyAdmin)
	at := time.Now().UTC()
	for name, c := range map[string]KillClaim{
		"attacker == victim (self kill)": {GuildID: f.guildA, KillerPlayerID: f.target, VictimPlayerID: f.target},
		"unresolved killer":              {GuildID: f.guildA, KillerPlayerID: 0, VictimPlayerID: f.target},
		"unresolved victim":              {GuildID: f.guildA, KillerPlayerID: f.hunter, VictimPlayerID: 0},
		"someone else is the victim":     {GuildID: f.guildA, KillerPlayerID: f.hunter, VictimPlayerID: f.bystander},
	} {
		c.At = at
		c.KillID = f.kill(f.guildA, f.serverA1, f.hunter, f.bystander)
		got, err := f.repo.ClaimForKill(f.ctx, c)
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: expected no claim, got %v %v", name, ids(got), err)
		}
	}
	if f.status(b.ID) != BountyActive {
		t.Fatal("no invalid claim may change the bounty")
	}
}

func TestBountyClaimRespectsTheStartAndExpiryWindow(t *testing.T) {
	f := newBountyFix(t)
	now := time.Now().UTC()
	future, past := now.Add(time.Hour), now.Add(-time.Minute)
	notYet, err := f.repo.Create(f.ctx, Bounty{GuildID: f.guildA, TargetPlayerID: f.target, CreatedByType: BountyAdmin, RewardPoints: 10, StartsAt: &future}, "t")
	must(t, err)
	oldStart := now.Add(-time.Hour)
	lapsed, err := f.repo.Create(f.ctx, Bounty{GuildID: f.guildA, TargetPlayerID: f.target, CreatedByType: BountyAdmin, RewardPoints: 20, StartsAt: &oldStart, ExpiresAt: &past}, "t")
	must(t, err)
	if got := f.claim(f.serverA1, f.hunter, f.target); len(got) != 0 {
		t.Fatalf("neither a future nor a lapsed bounty is claimable, got %v", ids(got))
	}
	if f.status(notYet.ID) != BountyActive || f.status(lapsed.ID) != BountyActive {
		t.Fatal("a refused claim must not change anything")
	}
}

// --- H/I: atomicity, replays ------------------------------------------------------------------

// H: many concurrent claim attempts (workers / retries) on one bounty - exactly one
// wins, the points are awarded exactly once.
func TestBountyConcurrentClaimExactlyOneWinner(t *testing.T) {
	f := newBountyFix(t)
	b := f.bounty(f.guildA, f.serverA1, f.target, 1000, BountyAdmin)

	const attempts = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	var winners atomic.Int64
	var claimedRows atomic.Int64
	killIDs := make([]int64, attempts)
	for i := range killIDs {
		killIDs[i] = f.kill(f.guildA, f.serverA1, f.hunter, f.target)
	}
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, err := f.repo.ClaimForKill(f.ctx, KillClaim{GuildID: f.guildA, ServerID: f.serverA1, KillerPlayerID: f.hunter, VictimPlayerID: f.target, KillID: killIDs[i], At: time.Now().UTC()})
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if len(got) > 0 {
				winners.Add(1)
				claimedRows.Add(int64(len(got)))
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners.Load() != 1 || claimedRows.Load() != 1 {
		t.Fatalf("expected exactly one claimant of exactly one bounty, got winners=%d rows=%d", winners.Load(), claimedRows.Load())
	}
	if f.status(b.ID) != BountyClaimed {
		t.Fatal("the bounty must be CLAIMED")
	}
	if life, txs := f.points(f.hunter); life != 1000 || txs != 1 {
		t.Fatalf("the reward must be awarded exactly once, got lifetime=%d txs=%d", life, txs)
	}
}

// The same race with a stack: every bounty is claimed exactly once across all racers.
func TestBountyConcurrentClaimOfAStackClaimsEachOnce(t *testing.T) {
	f := newBountyFix(t)
	var all []int64
	for i := 0; i < 5; i++ {
		all = append(all, f.bounty(f.guildA, 0, f.target, int64(100*(i+1)), BountyAdmin).ID)
	}
	const attempts = 16
	killIDs := make([]int64, attempts)
	for i := range killIDs {
		killIDs[i] = f.kill(f.guildA, f.serverA1, f.hunter, f.target)
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, err := f.repo.ClaimForKill(f.ctx, KillClaim{GuildID: f.guildA, ServerID: f.serverA1, KillerPlayerID: f.hunter, VictimPlayerID: f.target, KillID: killIDs[i], At: time.Now().UTC()})
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			mu.Lock()
			for _, b := range got {
				seen[b.ID]++
			}
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	for _, id := range all {
		if seen[id] != 1 {
			t.Fatalf("bounty %d was claimed %d times, want exactly 1 (seen=%v)", id, seen[id], seen)
		}
	}
	if life, txs := f.points(f.hunter); life != 1500 || txs != 5 {
		t.Fatalf("expected 1500 points in 5 transactions, got lifetime=%d txs=%d", life, txs)
	}
}

// I: replaying the same persisted kill claims nothing and awards nothing more.
func TestBountyReplayedKillDoesNotClaimTwice(t *testing.T) {
	f := newBountyFix(t)
	f.bounty(f.guildA, 0, f.target, 600, BountyAdmin)
	killID := f.kill(f.guildA, f.serverA1, f.hunter, f.target)
	claim := KillClaim{GuildID: f.guildA, ServerID: f.serverA1, KillerPlayerID: f.hunter, VictimPlayerID: f.target, KillID: killID, At: time.Now().UTC()}
	first, err := f.repo.ClaimForKill(f.ctx, claim)
	must(t, err)
	for i := 0; i < 3; i++ {
		again, err := f.repo.ClaimForKill(f.ctx, claim)
		must(t, err)
		if len(again) != 0 {
			t.Fatalf("replay %d claimed %v", i, ids(again))
		}
	}
	if len(first) != 1 {
		t.Fatalf("the first claim must claim, got %v", ids(first))
	}
	if life, txs := f.points(f.hunter); life != 600 || txs != 1 {
		t.Fatalf("a replay must not award again, got lifetime=%d txs=%d", life, txs)
	}
}

// A cancel and a claim racing on one bounty: exactly one of them wins, never both.
func TestBountyCancelAndClaimNeverBothSucceed(t *testing.T) {
	f := newBountyFix(t)
	for round := 0; round < 25; round++ {
		b := f.bounty(f.guildA, 0, f.target, 10, BountyAdmin)
		killID := f.kill(f.guildA, f.serverA1, f.hunter, f.target)
		var claimed, cancelled bool
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			got, _ := f.repo.ClaimForKill(f.ctx, KillClaim{GuildID: f.guildA, ServerID: f.serverA1, KillerPlayerID: f.hunter, VictimPlayerID: f.target, KillID: killID, At: time.Now().UTC()})
			claimed = len(got) > 0
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := f.repo.Cancel(f.ctx, f.guildA, b.ID)
			cancelled = err == nil
		}()
		close(start)
		wg.Wait()
		if claimed == cancelled {
			t.Fatalf("round %d: exactly one of claim/cancel must win, claimed=%v cancelled=%v", round, claimed, cancelled)
		}
		want := BountyCancelled
		if claimed {
			want = BountyClaimed
		}
		if got := f.status(b.ID); got != want {
			t.Fatalf("round %d: status %s, want %s", round, got, want)
		}
		// Clear the target for the next round.
		f.pool.Exec(f.ctx, `UPDATE bounties SET status='CANCELLED' WHERE guild_id=$1 AND target_player_id=$2 AND status='ACTIVE'`, f.guildA, f.target)
	}
}

// --- expiry / increase --------------------------------------------------------------------------

// Concurrent sweepers each get a disjoint set of expired bounties: every expiry is
// reported exactly once, and bounties that are not due are untouched.
func TestBountyExpireDueIsAtomicAcrossSweepers(t *testing.T) {
	f := newBountyFix(t)
	now := time.Now().UTC()
	start := now.Add(-2 * time.Hour)
	due, notDue := now.Add(-time.Minute), now.Add(time.Hour)
	var dueIDs []int64
	for i := 0; i < 6; i++ {
		b, err := f.repo.Create(f.ctx, Bounty{GuildID: f.guildA, TargetPlayerID: f.target, CreatedByType: BountyAdmin, RewardPoints: 10, StartsAt: &start, ExpiresAt: &due}, "t")
		must(t, err)
		dueIDs = append(dueIDs, b.ID)
	}
	later, err := f.repo.Create(f.ctx, Bounty{GuildID: f.guildA, TargetPlayerID: f.target, CreatedByType: BountyAdmin, RewardPoints: 10, StartsAt: &start, ExpiresAt: &notDue}, "t")
	must(t, err)
	forever := f.bounty(f.guildA, 0, f.target, 10, BountyAdmin) // no expiry

	var mu sync.Mutex
	got := map[int64]int{}
	var wg sync.WaitGroup
	sweepStart := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-sweepStart
			rows, err := f.repo.ExpireDue(f.ctx, time.Now().UTC())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, r := range rows {
				got[r.ID]++
			}
			mu.Unlock()
		}()
	}
	close(sweepStart)
	wg.Wait()
	for _, id := range dueIDs {
		if got[id] != 1 || f.status(id) != BountyExpired {
			t.Fatalf("bounty %d must be expired and reported exactly once, reported %d times, status %s", id, got[id], f.status(id))
		}
	}
	if f.status(later.ID) != BountyActive || f.status(forever.ID) != BountyActive {
		t.Fatal("bounties that are not due must stay ACTIVE")
	}
}

func TestBountyIncreaseAndUpgradeOnlyRaise(t *testing.T) {
	f := newBountyFix(t)
	m := f.bounty(f.guildA, 0, f.target, 100, BountyAdmin)
	if _, err := f.repo.Increase(f.ctx, f.guildA, m.ID, 100); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("an equal amount is not an increase, got %v", err)
	}
	if _, err := f.repo.Increase(f.ctx, f.guildA, m.ID, 50); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("a lower amount is not an increase, got %v", err)
	}
	up, err := f.repo.Increase(f.ctx, f.guildA, m.ID, 250)
	if err != nil || up.RewardPoints != 250 {
		t.Fatalf("expected 250, got %+v %v", up, err)
	}

	auto := f.bounty(f.guildA, 0, f.hunter, 500, BountyAutomatic)
	if _, err := f.repo.UpgradeReturning(f.ctx, f.guildA, auto.ID, 500); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("the same tier is not an upgrade, got %v", err)
	}
	if b, err := f.repo.UpgradeReturning(f.ctx, f.guildA, auto.ID, 750); err != nil || b.RewardPoints != 750 {
		t.Fatalf("expected the automatic bounty raised to 750, got %+v %v", b, err)
	}
	// Upgrade never touches a manual bounty.
	if _, err := f.repo.UpgradeReturning(f.ctx, f.guildA, m.ID, 9999); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("a manual bounty must not be upgraded by the streak rule, got %v", err)
	}
	// A claimed bounty can no longer be raised or cancelled.
	f.claim(f.serverA1, f.hunter, f.target)
	if _, err := f.repo.Increase(f.ctx, f.guildA, m.ID, 999999); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("a claimed bounty cannot be increased, got %v", err)
	}
	if _, err := f.repo.Cancel(f.ctx, f.guildA, m.ID); !errors.Is(err, ErrBountyNotFound) {
		t.Fatalf("a claimed bounty cannot be cancelled, got %v", err)
	}
}

// --- the board view ---------------------------------------------------------------------------------

func TestBountyBoardAggregatesScopesAndExcludesInactive(t *testing.T) {
	f := newBountyFix(t)
	f.bounty(f.guildA, f.serverA1, f.target, 100, BountyAdmin)
	f.bounty(f.guildA, f.serverA1, f.target, 150, BountyAdmin) // stacks on the same target
	f.bounty(f.guildA, f.serverA2, f.hunter, 900, BountyAdmin) // another server only
	f.bounty(f.guildA, 0, f.bystander, 300, BountyAdmin)       // guild-wide
	gone := f.bounty(f.guildA, f.serverA1, f.bystander, 5000, BountyAdmin)
	_, err := f.repo.Cancel(f.ctx, f.guildA, gone.ID)
	must(t, err)
	f.bounty(f.guildB, 0, f.otherB, 7777, BountyAdmin) // another guild

	board, err := f.repo.ListBoard(f.ctx, f.guildA, []int64{f.serverA1}, 10)
	must(t, err)
	if len(board) != 2 || board[0].TargetName != "Bystander" || board[0].Total != 300 || board[1].TargetName != "Target" || board[1].Total != 250 || board[1].Count != 2 {
		t.Fatalf("server A1's board must show the guild-wide and A1 bounties stacked per target, got %+v", board)
	}
	for _, e := range board {
		if e.TargetName == "Hunter" || e.TargetName == "OtherGuild" {
			t.Fatalf("a bounty scoped to another server or guild must not appear: %+v", board)
		}
	}
	both, err := f.repo.ListBoard(f.ctx, f.guildA, []int64{f.serverA1, f.serverA2}, 10)
	must(t, err)
	if len(both) != 3 || both[0].TargetName != "Hunter" || both[0].Total != 900 {
		t.Fatalf("a channel shared by both servers shows the union, got %+v", both)
	}
	all, err := f.repo.ListBoardAll(f.ctx, f.guildA, 2)
	must(t, err)
	if len(all) != 2 || all[0].TargetName != "Hunter" {
		t.Fatalf("expected the top 2 across every server, got %+v", all)
	}
	// No servers at all -> only guild-wide bounties.
	wideOnly, err := f.repo.ListBoard(f.ctx, f.guildA, nil, 10)
	must(t, err)
	if len(wideOnly) != 1 || wideOnly[0].TargetName != "Bystander" {
		t.Fatalf("with no servers only guild-wide bounties show, got %+v", wideOnly)
	}
}

func TestBountyHasActiveAtIsServerAware(t *testing.T) {
	f := newBountyFix(t)
	f.bounty(f.guildA, f.serverA1, f.target, 100, BountyAdmin)
	now := time.Now().UTC()
	if ok, err := f.repo.HasActiveAt(f.ctx, f.guildA, f.serverA1, f.target, now); err != nil || !ok {
		t.Fatalf("the target is wanted on A1: %v %v", ok, err)
	}
	if ok, _ := f.repo.HasActiveAt(f.ctx, f.guildA, f.serverA2, f.target, now); ok {
		t.Fatal("the target is not wanted on A2")
	}
	f.bounty(f.guildA, 0, f.target, 50, BountyAdmin)
	if ok, _ := f.repo.HasActiveAt(f.ctx, f.guildA, f.serverA2, f.target, now); !ok {
		t.Fatal("a guild-wide bounty makes the target wanted on every server")
	}
}

// The tenant helpers the placement service relies on.
func TestBountyTenantLookups(t *testing.T) {
	f := newBountyFix(t)
	if name, ok, err := f.repo.PlayerName(f.ctx, f.guildA, f.target); err != nil || !ok || name != "Target" {
		t.Fatalf("expected Target in guild A, got %q %v %v", name, ok, err)
	}
	if _, ok, _ := f.repo.PlayerName(f.ctx, f.guildB, f.target); ok {
		t.Fatal("a guild A player must not resolve in guild B")
	}
	guild, org, found, err := f.repo.ServerScope(f.ctx, f.serverA1)
	if err != nil || !found || guild != f.guildA || org != nil {
		t.Fatalf("unexpected scope: guild=%d org=%v found=%v err=%v", guild, org, found, err)
	}
	if _, _, found, _ := f.repo.ServerScope(f.ctx, 987654321); found {
		t.Fatal("an unknown server must not be found")
	}
}
