package bounties

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type recordingEconomy struct{ events []economy.Event }

func (r *recordingEconomy) Notify(e economy.Event) { r.events = append(r.events, e) }

type panickingEconomy struct{}

func (panickingEconomy) Notify(economy.Event) { panic("economy notifier bug") }

// One economy event per bounty (one ledger transaction per bounty), in payout
// order, each carrying the hunter's balance right after THAT payout.
func TestClaimReportsOneEconomyEventPerBounty(t *testing.T) {
	svc, store, _ := newService()
	eco := &recordingEconomy{}
	svc.SetEconomyNotifier(eco)
	store.claimResult = []repository.Bounty{
		{ID: 1, GuildID: 1, RewardPoints: 100000, Reward: repository.LedgerEntry{ID: 11, BalanceAfter: 100000}},
		{ID: 2, GuildID: 1, RewardPoints: 50000, Reward: repository.LedgerEntry{ID: 12, BalanceAfter: 150000}},
	}
	if _, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, ServerID: 100, VictimPlayerID: 10, KillerPlayerID: 11, HunterName: "Hunter", TargetName: "Target"}); err != nil {
		t.Fatal(err)
	}
	if len(eco.events) != 2 {
		t.Fatalf("expected one economy event per bounty, got %+v", eco.events)
	}
	for i, want := range []struct{ amount, balance int64 }{{100000, 100000}, {50000, 150000}} {
		e := eco.events[i]
		if e.Type != economy.TypeBountyClaim || !e.Credit || e.PlayerName != "Hunter" || e.Amount != want.amount || e.BalanceAfter != want.balance || e.ServerID != 100 || e.GuildID != 1 {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
}

// A payout that was a replay (nothing changed) or has no ledger entry is not announced.
func TestClaimDoesNotAnnounceReplayedOrMissingPayouts(t *testing.T) {
	svc, store, _ := newService()
	eco := &recordingEconomy{}
	svc.SetEconomyNotifier(eco)
	store.claimResult = []repository.Bounty{
		{ID: 1, GuildID: 1, RewardPoints: 10, Reward: repository.LedgerEntry{ID: 11, BalanceAfter: 10, Duplicate: true}},
		{ID: 2, GuildID: 1, RewardPoints: 20}, // no ledger entry
	}
	svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, VictimPlayerID: 10, KillerPlayerID: 11, HunterName: "H"})
	if len(eco.events) != 0 {
		t.Fatalf("expected nothing announced, got %+v", eco.events)
	}
}

// The economy notifier failing leaves the claim result (and the bounty events) intact.
func TestEconomyNotifierFailureNeverAffectsTheClaim(t *testing.T) {
	svc, store, n := newService()
	svc.SetEconomyNotifier(panickingEconomy{})
	store.claimResult = []repository.Bounty{{ID: 1, GuildID: 1, RewardPoints: 500, Reward: repository.LedgerEntry{ID: 9, BalanceAfter: 500}}}
	res, err := svc.ClaimForKill(context.Background(), KillInput{GuildID: 1, VictimPlayerID: 10, KillerPlayerID: 11, HunterName: "H"})
	if err != nil || res.Count != 1 || res.Total != 500 {
		t.Fatalf("a panicking economy notifier must not affect the claim: %+v %v", res, err)
	}
	if store.claimCalls != 1 || len(n.events) != 1 {
		t.Fatalf("the claim runs once and the bounty event still fires: calls=%d events=%d", store.claimCalls, len(n.events))
	}
	var nilSvc *Service
	nilSvc.SetEconomyNotifier(nil)
}
