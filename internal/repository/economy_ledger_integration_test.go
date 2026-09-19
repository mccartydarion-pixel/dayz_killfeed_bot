//go:build integration

package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// ecoFix reuses the bounty fixture's two-guild world (guild A: servers A1/A2,
// players target/hunter/bystander; guild B: one player) and adds ledger helpers.
type ecoFix struct {
	*bountyFix
	eco *EconomyRepository
}

func newEcoFix(t *testing.T) *ecoFix {
	t.Helper()
	return &ecoFix{bountyFix: newBountyFix(t), eco: NewEconomyRepository(nil)}
}

func (f *ecoFix) repoEco() *EconomyRepository { return NewEconomyRepository(f.pool) }

func (f *ecoFix) credit(player, amount int64, typ string, earned bool, ref string) LedgerEntry {
	f.t.Helper()
	e, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: player, Type: typ, Amount: amount, Earned: earned, ReferenceID: ref, CreatedBy: "test"})
	must(f.t, err)
	return e
}

func (f *ecoFix) balance(player int64) int64 {
	f.t.Helper()
	b, err := f.repoEco().Balance(f.ctx, f.guildA, player)
	must(f.t, err)
	return b
}

// invariant: for a player, SUM(ledger amounts) == balance, and the last row's
// balance_after == balance. This must hold after any mix of operations.
func (f *ecoFix) assertLedgerMatchesBalance(player int64) {
	f.t.Helper()
	var sum, last int64
	must(f.t, f.pool.QueryRow(f.ctx, `SELECT COALESCE(SUM(amount),0), COALESCE((SELECT balance_after FROM point_transactions WHERE guild_id=$1 AND player_id=$2 ORDER BY id DESC LIMIT 1),0) FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, f.guildA, player).Scan(&sum, &last))
	if bal := f.balance(player); sum != bal || (sum != 0 && last != bal) {
		f.t.Fatalf("the ledger must reconcile with the balance: SUM(amount)=%d last balance_after=%d balance=%d", sum, last, bal)
	}
}

func (f *ecoFix) scores(player int64) (balance, lifetime, season int64) {
	must(f.t, f.pool.QueryRow(f.ctx, `SELECT balance,lifetime_points,season_points FROM player_points WHERE guild_id=$1 AND player_id=$2`, f.guildA, player).Scan(&balance, &lifetime, &season))
	return
}

func (f *ecoFix) ledgerRows(player int64) int64 {
	var n int64
	must(f.t, f.pool.QueryRow(f.ctx, `SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, f.guildA, player).Scan(&n))
	return n
}

// --- A/B/C/D --------------------------------------------------------------------------------

func TestEconomyInitialBalanceIsZero(t *testing.T) {
	f := newEcoFix(t)
	if f.balance(f.hunter) != 0 {
		t.Fatal("a player with no transactions must have a zero balance")
	}
	entries, next, err := f.repoEco().History(f.ctx, f.guildA, f.hunter, 10, 0)
	if err != nil || len(entries) != 0 || next != 0 {
		t.Fatalf("expected an empty history, got %v next=%d err=%v", entries, next, err)
	}
}

func TestEconomyCreditAndDebitKeepLedgerAndBalanceInStep(t *testing.T) {
	f := newEcoFix(t)
	c1 := f.credit(f.hunter, 1000, TxBountyClaim, true, "bounty:test-1")
	if c1.BalanceAfter != 1000 || c1.Amount != 1000 || c1.ID == 0 || c1.Duplicate {
		t.Fatalf("credit: %+v", c1)
	}
	adminCredit := f.credit(f.hunter, 500, TxAdminCredit, false, "")
	if adminCredit.BalanceAfter != 1500 {
		t.Fatalf("admin credit: %+v", adminCredit)
	}
	d, err := f.repoEco().Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 300, CreatedBy: "admin-1", Description: "correction"})
	must(t, err)
	if d.BalanceAfter != 1200 || d.Amount != -300 {
		t.Fatalf("debit: %+v", d)
	}
	balance, lifetime, season := f.scores(f.hunter)
	// Earned credits raise the leaderboard scores; admin credits and debits do not.
	if balance != 1200 || lifetime != 1000 || season != 1000 {
		t.Fatalf("expected balance 1200 with earn-only scores 1000/1000, got %d/%d/%d", balance, lifetime, season)
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

func TestEconomyDebitRejectsInsufficientFundsAndLeavesNoTrace(t *testing.T) {
	f := newEcoFix(t)
	eco := f.repoEco()
	if _, err := eco.Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 1}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("a debit on a player with no row must be insufficient funds, got %v", err)
	}
	f.credit(f.hunter, 100, TxAdminCredit, false, "")
	if _, err := eco.Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 101}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	if f.balance(f.hunter) != 100 || f.ledgerRows(f.hunter) != 1 {
		t.Fatal("a rejected debit must change nothing and write no ledger row")
	}
	exact, err := eco.Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 100})
	if err != nil || exact.BalanceAfter != 0 {
		t.Fatalf("spending exactly the balance must work: %+v %v", exact, err)
	}
	for _, bad := range []int64{0, -5, MaxLedgerAmount + 1} {
		if _, err := eco.Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminCredit, Amount: bad}); !errors.Is(err, ErrInvalidLedgerAmount) {
			t.Fatalf("amount %d must be rejected, got %v", bad, err)
		}
	}
	// The database itself refuses a negative balance, whatever the code does.
	if _, err := f.pool.Exec(f.ctx, `UPDATE player_points SET balance=-1 WHERE guild_id=$1 AND player_id=$2`, f.guildA, f.hunter); err == nil {
		t.Fatal("the database must refuse a negative balance")
	}
}

// --- E/F: concurrency -----------------------------------------------------------------------

// E: many simultaneous credits - no lost updates: the balance is the exact sum, and
// every credit's balance_after is a distinct step of a single running total.
func TestEconomyConcurrentCreditsLoseNothing(t *testing.T) {
	f := newEcoFix(t)
	const workers, each = 40, int64(25)
	var wg sync.WaitGroup
	start := make(chan struct{})
	afters := make([]int64, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			e, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminCredit, Amount: each, CreatedBy: "t"})
			if err != nil {
				t.Errorf("credit %d: %v", i, err)
				return
			}
			afters[i] = e.BalanceAfter
		}(i)
	}
	close(start)
	wg.Wait()
	if got := f.balance(f.hunter); got != workers*each {
		t.Fatalf("lost update: balance %d, want %d", got, workers*each)
	}
	sort.Slice(afters, func(i, j int) bool { return afters[i] < afters[j] })
	for i, a := range afters {
		if a != int64(i+1)*each {
			t.Fatalf("balance_after values must be one running total, got %v", afters)
		}
	}
	if f.ledgerRows(f.hunter) != workers {
		t.Fatalf("expected %d ledger rows, got %d", workers, f.ledgerRows(f.hunter))
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// F: racing debits against a fixed balance - never a double spend: exactly
// balance/amount succeed, the rest are insufficient funds, and it never goes negative.
func TestEconomyConcurrentDebitsNeverDoubleSpend(t *testing.T) {
	f := newEcoFix(t)
	f.credit(f.hunter, 100, TxAdminCredit, false, "")
	const workers = 30
	var ok, insufficient atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.repoEco().Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 10, CreatedBy: "t"})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInsufficientFunds):
				insufficient.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 10 || insufficient.Load() != workers-10 {
		t.Fatalf("expected exactly 10 debits to succeed, got ok=%d insufficient=%d", ok.Load(), insufficient.Load())
	}
	if f.balance(f.hunter) != 0 {
		t.Fatalf("the balance must end at 0, got %d", f.balance(f.hunter))
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// Credits and debits interleaved: whatever order they run in, the ledger reconciles
// with the balance and the balance never goes negative.
func TestEconomyMixedConcurrentOperationsReconcile(t *testing.T) {
	f := newEcoFix(t)
	f.credit(f.hunter, 200, TxAdminCredit, false, "")
	var wg sync.WaitGroup
	var creditsDone, debitsDone atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				if _, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminCredit, Amount: 7}); err == nil {
					creditsDone.Add(1)
				}
				return
			}
			if _, err := f.repoEco().Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 11}); err == nil {
				debitsDone.Add(1)
			} else if !errors.Is(err, ErrInsufficientFunds) {
				t.Errorf("debit: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	want := 200 + 7*creditsDone.Load() - 11*debitsDone.Load()
	if got := f.balance(f.hunter); got != want || got < 0 {
		t.Fatalf("balance %d, want %d (credits=%d debits=%d)", got, want, creditsDone.Load(), debitsDone.Load())
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// --- G: history --------------------------------------------------------------------------------

func TestEconomyHistoryOrderingPaginationAndBounds(t *testing.T) {
	f := newEcoFix(t)
	for i := 1; i <= 60; i++ {
		f.credit(f.hunter, int64(i), TxAdminCredit, false, "")
	}
	page1, next, err := f.repoEco().History(f.ctx, f.guildA, f.hunter, 10, 0)
	must(t, err)
	if len(page1) != 10 || next == 0 || page1[0].Amount != 60 || page1[9].Amount != 51 {
		t.Fatalf("page 1 must be the 10 newest, newest first: %v next=%d", amounts(page1), next)
	}
	page2, next2, err := f.repoEco().History(f.ctx, f.guildA, f.hunter, 10, next)
	must(t, err)
	if len(page2) != 10 || page2[0].Amount != 50 || next2 == 0 {
		t.Fatalf("page 2 must continue after the cursor: %v", amounts(page2))
	}
	def, _, _ := f.repoEco().History(f.ctx, f.guildA, f.hunter, 0, 0)
	if len(def) != DefaultHistoryLimit {
		t.Fatalf("limit 0 must use the default %d, got %d", DefaultHistoryLimit, len(def))
	}
	huge, hugeNext, _ := f.repoEco().History(f.ctx, f.guildA, f.hunter, 1_000_000, 0)
	if len(huge) != MaxHistoryLimit || hugeNext == 0 {
		t.Fatalf("history must be hard-capped at %d, got %d", MaxHistoryLimit, len(huge))
	}
	last, lastNext, _ := f.repoEco().History(f.ctx, f.guildA, f.hunter, 50, page2[len(page2)-1].ID)
	if len(last) != 40 || lastNext != 0 {
		t.Fatalf("the last page must end the cursor, got %d next=%d", len(last), lastNext)
	}
	// Other players and other guilds never appear.
	f.credit(f.bystander, 5, TxAdminCredit, false, "")
	other, _, _ := f.repoEco().History(f.ctx, f.guildB, f.hunter, 50, 0)
	if len(other) != 0 {
		t.Fatalf("guild B must not see guild A's history, got %d", len(other))
	}
}

func amounts(es []LedgerEntry) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Amount)
	}
	return out
}

// --- H: idempotency ------------------------------------------------------------------------------

func TestEconomyIdempotentReferenceAppliesOnce(t *testing.T) {
	f := newEcoFix(t)
	first := f.credit(f.hunter, 500, TxAdminCredit, false, "admin:grant-1")
	second := f.credit(f.hunter, 500, TxAdminCredit, false, "admin:grant-1")
	if !second.Duplicate || second.ID != first.ID || second.BalanceAfter != first.BalanceAfter {
		t.Fatalf("a replay must return the original entry: %+v vs %+v", second, first)
	}
	if f.balance(f.hunter) != 500 || f.ledgerRows(f.hunter) != 1 {
		t.Fatal("a replay must neither credit nor add a row")
	}
	// A different amount under the same reference is still the same transaction.
	if third := f.credit(f.hunter, 9999, TxAdminCredit, false, "admin:grant-1"); !third.Duplicate || f.balance(f.hunter) != 500 {
		t.Fatal("the reference, not the amount, identifies a transaction")
	}
	// Another type / player is independent.
	if e := f.credit(f.hunter, 1, TxSystemReward, true, "admin:grant-1"); e.Duplicate {
		t.Fatal("the same reference under another type is a different transaction")
	}
	if e := f.credit(f.bystander, 1, TxAdminCredit, false, "admin:grant-1"); e.Duplicate {
		t.Fatal("the same reference for another player is a different transaction")
	}
	// A duplicate debit does not spend twice either.
	f.credit(f.hunter, 1000, TxAdminCredit, false, "")
	d1, err := f.repoEco().Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 100, ReferenceID: "spend-1"})
	must(t, err)
	d2, err := f.repoEco().Debit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminDebit, Amount: 100, ReferenceID: "spend-1"})
	must(t, err)
	if d1.Duplicate || !d2.Duplicate || d1.ID != d2.ID {
		t.Fatalf("a replayed debit must return the original: %+v %+v", d1, d2)
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// Racing same-reference requests: exactly one applies, the balance moves once.
func TestEconomyConcurrentSameReferenceCreditsOnce(t *testing.T) {
	f := newEcoFix(t)
	const workers = 20
	var applied, dup atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminCredit, Amount: 100, ReferenceID: "race-ref", CreatedBy: "t"})
			if err != nil {
				t.Errorf("credit: %v", err)
				return
			}
			if e.Duplicate {
				dup.Add(1)
			} else {
				applied.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if applied.Load() != 1 || dup.Load() != workers-1 {
		t.Fatalf("expected exactly one applied credit, got applied=%d duplicates=%d", applied.Load(), dup.Load())
	}
	if f.balance(f.hunter) != 100 || f.ledgerRows(f.hunter) != 1 {
		t.Fatalf("the balance must move once: balance=%d rows=%d", f.balance(f.hunter), f.ledgerRows(f.hunter))
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// The historical bounty idempotency key ("BOUNTY_CLAIM", "bounty:<id>") - written
// by every claim before this migration, without a balance_after - still blocks a
// replay: a re-award of an already-paid bounty is a duplicate, not a second payout.
func TestEconomyLegacyBountyKeyStillBlocksReplay(t *testing.T) {
	f := newEcoFix(t)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO point_transactions(guild_id,player_id,amount,reason_type,source_id,source_key) VALUES($1,$2,777,'BOUNTY_CLAIM',31337,'bounty:31337')`, f.guildA, f.hunter); err != nil {
		t.Fatal(err)
	}
	e := f.credit(f.hunter, 777, TxBountyClaim, true, "bounty:31337")
	if !e.Duplicate {
		t.Fatal("a replay of an already-paid (legacy-format) bounty must be a duplicate")
	}
	if f.ledgerRows(f.hunter) != 1 {
		t.Fatal("no second ledger row may be written")
	}
}

// --- I/J: bounty integration ---------------------------------------------------------------------

func TestEconomyBountyClaimWritesOneLedgerTransactionPerBounty(t *testing.T) {
	f := newEcoFix(t)
	b1 := f.bounty(f.guildA, 0, f.target, 100, BountyAdmin)
	b2 := f.bounty(f.guildA, f.serverA1, f.target, 250, BountyAdmin)
	f.credit(f.hunter, 1000, TxAdminCredit, false, "") // an existing balance the payouts add to

	got := f.claim(f.serverA1, f.hunter, f.target)
	if len(got) != 2 {
		t.Fatalf("expected both bounties claimed, got %v", ids(got))
	}
	rows, _, err := f.repoEco().History(f.ctx, f.guildA, f.hunter, 10, 0)
	must(t, err)
	if len(rows) != 3 { // the seed credit + two payouts
		t.Fatalf("expected 1 seed + 2 payout ledger rows, got %d", len(rows))
	}
	payouts := rows[:2] // newest first
	refs := map[string]LedgerEntry{}
	for _, r := range payouts {
		refs[r.ReferenceID] = r
		if r.Type != TxBountyClaim || r.ServerID != f.serverA1 || r.CreatedBy != "SYSTEM" {
			t.Fatalf("payout must be a BOUNTY_CLAIM attributed to the kill's server: %+v", r)
		}
	}
	for _, b := range []*Bounty{b1, b2} {
		r, ok := refs[fmt.Sprintf("bounty:%d", b.ID)]
		if !ok || r.Amount != b.RewardPoints {
			t.Fatalf("bounty %d must have its own ledger transaction of %d, got %+v", b.ID, b.RewardPoints, refs)
		}
	}
	// The claim returns each payout with the balance right after it.
	var last int64
	for _, b := range got {
		if b.Reward.ID == 0 || b.Reward.Duplicate || b.Reward.BalanceAfter <= last {
			t.Fatalf("payout balances must rise in payout order: %+v", got)
		}
		last = b.Reward.BalanceAfter
	}
	balance, lifetime, season := f.scores(f.hunter)
	if balance != 1350 || lifetime != 350 || season != 350 {
		t.Fatalf("bounty rewards are earned credits: balance 1350, scores 350/350, got %d/%d/%d", balance, lifetime, season)
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// J: replaying a claim writes no second transaction and pays nothing more.
func TestEconomyReplayedBountyClaimAddsNoTransaction(t *testing.T) {
	f := newEcoFix(t)
	b := f.bounty(f.guildA, 0, f.target, 600, BountyAdmin)
	killID := f.kill(f.guildA, f.serverA1, f.hunter, f.target)
	claim := KillClaim{GuildID: f.guildA, ServerID: f.serverA1, KillerPlayerID: f.hunter, VictimPlayerID: f.target, KillID: killID, At: time.Now().UTC()}
	first, err := f.repo.ClaimForKill(f.ctx, claim)
	must(t, err)
	if len(first) != 1 {
		t.Fatal("setup: the first claim must claim")
	}
	for i := 0; i < 3; i++ {
		again, err := f.repo.ClaimForKill(f.ctx, claim)
		must(t, err)
		if len(again) != 0 {
			t.Fatalf("replay %d claimed again", i)
		}
	}
	if f.ledgerRows(f.hunter) != 1 || f.balance(f.hunter) != 600 {
		t.Fatalf("a replay must add no transaction: rows=%d balance=%d", f.ledgerRows(f.hunter), f.balance(f.hunter))
	}
	// Even a direct re-award of the same bounty is a duplicate.
	if e := f.credit(f.hunter, 600, TxBountyClaim, true, fmt.Sprintf("bounty:%d", b.ID)); !e.Duplicate || f.balance(f.hunter) != 600 {
		t.Fatal("a direct replay of a paid bounty must be a duplicate")
	}
}

// Event prizes also go through the ledger (same type and key as before).
func TestEconomyEventFinalizationPaysThroughTheLedgerOnce(t *testing.T) {
	f := newEcoFix(t)
	events := NewEventRepository(f.pool)
	ev, err := events.CreateEvent(f.ctx, CompetitiveEvent{GuildID: f.guildA, Type: "MOST_KILLS", Name: "Prize test", Status: "ACTIVE", Config: json.RawMessage(`{}`)}, "t")
	must(t, err)
	// Seed the standings directly: event scoring is not what is under test here.
	for _, sc := range []struct {
		player int64
		points float64
	}{{f.hunter, 30}, {f.target, 20}, {f.bystander, 10}} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO event_scores(event_id,player_id,score,kills) VALUES($1,$2,$3,1)`, ev.ID, sc.player, sc.points); err != nil {
			t.Fatal(err)
		}
	}
	must(t, events.FinalizeEvent(f.ctx, ev.ID, EventResult{EventID: ev.ID, WinnerPlayerID: f.hunter, WinningScore: 30, FinalizedAt: time.Now().UTC()}))
	must(t, events.FinalizeEvent(f.ctx, ev.ID, EventResult{EventID: ev.ID, WinnerPlayerID: f.hunter, WinningScore: 30, FinalizedAt: time.Now().UTC()})) // a replay

	for player, want := range map[int64]int64{f.hunter: 1000, f.target: 500, f.bystander: 250} {
		if f.balance(player) != want {
			t.Fatalf("player %d: expected %d, got %d", player, want, f.balance(player))
		}
		balance, lifetime, _ := f.scores(player)
		if lifetime != want || balance != want {
			t.Fatalf("event prizes are earned: balance and lifetime must both be %d, got %d/%d", want, balance, lifetime)
		}
		f.assertLedgerMatchesBalance(player)
		if f.ledgerRows(player) != 1 {
			t.Fatalf("a replayed finalization must not pay twice (player %d has %d rows)", player, f.ledgerRows(player))
		}
	}
	var typ, ref string
	must(t, f.pool.QueryRow(f.ctx, `SELECT reason_type,source_key FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, f.guildA, f.hunter).Scan(&typ, &ref))
	if typ != "EVENT_FIRST_PLACE" || ref != fmt.Sprintf("event:%d:1", ev.ID) {
		t.Fatalf("the historical type/key must be preserved, got %q %q", typ, ref)
	}
}

// PointsRepository.Award (the generic earned credit) uses the same ledger.
func TestEconomyPointsAwardUsesTheLedger(t *testing.T) {
	f := newEcoFix(t)
	points := NewPointsRepository(f.pool)
	ok, err := points.Award(f.ctx, f.guildA, 0, f.hunter, 300, "TEST_AWARD", 5, "award:1")
	if err != nil || !ok {
		t.Fatalf("award: %v %v", ok, err)
	}
	if again, _ := points.Award(f.ctx, f.guildA, 0, f.hunter, 300, "TEST_AWARD", 5, "award:1"); again {
		t.Fatal("a replayed award must report false")
	}
	if balance, lifetime, _ := f.scores(f.hunter); balance != 300 || lifetime != 300 {
		t.Fatalf("award must credit balance and lifetime once, got %d/%d", balance, lifetime)
	}
	f.assertLedgerMatchesBalance(f.hunter)
}

// --- migration, append-only, seasons, overflow -----------------------------------------------------

// The one-time backfill: legacy rows (no balance_after; balance column at its
// default) get balance = lifetime and a running balance_after.
func TestEconomyBackfillMigrationSQL(t *testing.T) {
	f := newEcoFix(t)
	// Legacy shape: three credits written by the old code, totals in player_points.
	for i, amt := range []int64{100, 250, 50} {
		if _, err := f.pool.Exec(f.ctx, `INSERT INTO point_transactions(guild_id,player_id,amount,reason_type,source_key) VALUES($1,$2,$3,'LEGACY',$4)`, f.guildA, f.hunter, amt, fmt.Sprintf("legacy:%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO player_points(guild_id,player_id,lifetime_points,season_points,balance) VALUES($1,$2,400,150,0)`, f.guildA, f.hunter); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, database.EconomyBackfillSQL); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if balance, lifetime, season := f.scores(f.hunter); balance != 400 || lifetime != 400 || season != 150 {
		t.Fatalf("balance must start equal to lifetime (season untouched), got %d/%d/%d", balance, lifetime, season)
	}
	rows, _, err := f.repoEco().History(f.ctx, f.guildA, f.hunter, 10, 0)
	must(t, err)
	if len(rows) != 3 || rows[0].BalanceAfter != 400 || rows[1].BalanceAfter != 350 || rows[2].BalanceAfter != 100 {
		t.Fatalf("legacy rows must get a running balance_after (100, 350, 400), got %+v", rows)
	}
	f.assertLedgerMatchesBalance(f.hunter)
	// Running it again changes nothing (idempotent).
	if _, err := f.pool.Exec(f.ctx, database.EconomyBackfillSQL); err != nil {
		t.Fatal(err)
	}
	if f.balance(f.hunter) != 400 {
		t.Fatal("the backfill must be idempotent")
	}
}

func TestEconomyLedgerIsAppendOnly(t *testing.T) {
	f := newEcoFix(t)
	e := f.credit(f.hunter, 100, TxAdminCredit, false, "")
	for name, stmt := range map[string]string{
		"amount":       `UPDATE point_transactions SET amount=999 WHERE id=$1`,
		"balance":      `UPDATE point_transactions SET balance_after=999 WHERE id=$1`,
		"type":         `UPDATE point_transactions SET reason_type='OTHER' WHERE id=$1`,
		"description":  `UPDATE point_transactions SET description='rewritten' WHERE id=$1`,
		"created_by":   `UPDATE point_transactions SET created_by='someone-else' WHERE id=$1`,
		"created_at":   `UPDATE point_transactions SET created_at=NOW()-INTERVAL '1 day' WHERE id=$1`,
		"reference id": `UPDATE point_transactions SET source_key='rewritten' WHERE id=$1`,
	} {
		if _, err := f.pool.Exec(f.ctx, stmt, e.ID); err == nil {
			t.Fatalf("rewriting a ledger row's %s must be refused", name)
		}
	}
	// The foreign keys' ON DELETE SET NULL still work (they null season/server only).
	if _, err := f.pool.Exec(f.ctx, `UPDATE point_transactions SET season_id=NULL, server_id=NULL WHERE id=$1`, e.ID); err != nil {
		t.Fatalf("nulling season_id/server_id must stay possible: %v", err)
	}
	// And a server delete (ON DELETE SET NULL) does not break on the trigger.
	withServer, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, ServerID: f.serverA2, Type: TxAdminCredit, Amount: 1})
	must(t, err)
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM game_servers WHERE id=$1`, f.serverA2); err != nil {
		t.Fatalf("deleting a server must not be blocked by the ledger trigger: %v", err)
	}
	var server int64
	must(t, f.pool.QueryRow(f.ctx, `SELECT COALESCE(server_id,0) FROM point_transactions WHERE id=$1`, withServer.ID).Scan(&server))
	if server != 0 {
		t.Fatal("the ledger row must survive its server, with the attribution cleared")
	}
}

// A season rollover resets the leaderboard score, never the spendable balance.
func TestEconomySeasonResetKeepsTheBalance(t *testing.T) {
	f := newEcoFix(t)
	f.credit(f.hunter, 800, TxBountyClaim, true, "bounty:season-1")
	must(t, NewPointsRepository(f.pool).ResetSeason(f.ctx, f.guildA))
	balance, lifetime, season := f.scores(f.hunter)
	if balance != 800 || lifetime != 800 || season != 0 {
		t.Fatalf("a season reset must only clear the season score, got %d/%d/%d", balance, lifetime, season)
	}
}

// Overflow is refused (the whole operation aborts), never silently wrapped.
func TestEconomyOverflowIsRejectedNotWrapped(t *testing.T) {
	f := newEcoFix(t)
	f.credit(f.hunter, 10, TxAdminCredit, false, "")
	if _, err := f.pool.Exec(f.ctx, `UPDATE player_points SET balance=9223372036854775000 WHERE guild_id=$1 AND player_id=$2`, f.guildA, f.hunter); err != nil {
		t.Fatal(err)
	}
	rowsBefore := f.ledgerRows(f.hunter)
	if _, err := f.repoEco().Credit(f.ctx, LedgerParams{GuildID: f.guildA, PlayerID: f.hunter, Type: TxAdminCredit, Amount: 1000}); err == nil {
		t.Fatal("a credit that would overflow BIGINT must fail")
	}
	if f.balance(f.hunter) != 9223372036854775000 || f.ledgerRows(f.hunter) != rowsBefore {
		t.Fatal("a failed (overflowing) credit must change nothing")
	}
	// The ledger holds far more than a 32-bit bounty amount.
	g := newEcoFix(t)
	big := g.credit(g.hunter, 5_000_000_000, TxAdminCredit, false, "")
	if big.BalanceAfter != 5_000_000_000 {
		t.Fatalf("a 64-bit amount must be stored exactly, got %d", big.BalanceAfter)
	}
}

// --- O: the service over the real repository (tenant isolation) -------------------------------------

func TestEconomyRepositoryLookupsAreGuildScoped(t *testing.T) {
	f := newEcoFix(t)
	eco := f.repoEco()
	if _, ok, _ := eco.PlayerName(f.ctx, f.guildB, f.target); ok {
		t.Fatal("a guild A player must not resolve in guild B")
	}
	if name, ok, err := eco.PlayerName(f.ctx, f.guildA, f.target); err != nil || !ok || name != "Target" {
		t.Fatalf("expected Target in guild A, got %q %v %v", name, ok, err)
	}
	guild, _, found, err := eco.ServerScope(f.ctx, f.serverB1)
	if err != nil || !found || guild != f.guildB {
		t.Fatalf("server B1 belongs to guild B, got %d %v %v", guild, found, err)
	}
	// Balances are per (guild, player): crediting in guild A never shows in guild B.
	f.credit(f.hunter, 50, TxAdminCredit, false, "")
	if b, _ := eco.Balance(f.ctx, f.guildB, f.hunter); b != 0 {
		t.Fatalf("guild B must not see guild A's balance, got %d", b)
	}
}
