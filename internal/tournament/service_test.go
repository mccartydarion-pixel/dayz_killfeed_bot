package tournament

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory Store: deep copies in and out, so a failed transition leaves the
// stored tournament untouched like a rolled-back transaction would.
type memStore struct {
	mu      sync.Mutex
	next    int64
	ts      map[int64]*Tournament
	payouts map[string]Payout
	titles  map[string]string
}

func newMemStore() *memStore {
	return &memStore{ts: map[int64]*Tournament{}, payouts: map[string]Payout{}, titles: map[string]string{}}
}

func clone(t *Tournament) *Tournament {
	raw, _ := json.Marshal(t)
	var out Tournament
	_ = json.Unmarshal(raw, &out)
	out.RestoreNextPos()
	return &out
}

func (s *memStore) Create(_ context.Context, t *Tournament) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	t.ID = s.next
	s.ts[t.ID] = clone(t)
	return nil
}

func (s *memStore) Get(_ context.Context, id int64) (*Tournament, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ts[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(t), nil
}

func (s *memStore) Transact(_ context.Context, id int64, fn func(t *Tournament) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ts[id]
	if !ok {
		return ErrNotFound
	}
	w := clone(t)
	if err := fn(w); err != nil {
		return err
	}
	// Ids for new rows, as the repository assigns them.
	var maxE, maxM, maxR int64
	for _, e := range w.Entries {
		maxE = max(maxE, e.ID)
	}
	for _, m := range w.Matches {
		maxM = max(maxM, m.ID)
		for _, r := range m.Rounds {
			maxR = max(maxR, r.ID)
		}
	}
	for _, e := range w.Entries {
		if e.ID == 0 {
			maxE++
			e.ID = maxE
		}
	}
	for _, m := range w.Matches {
		if m.ID == 0 {
			maxM++
			m.ID = maxM
		}
	}
	w.LinkNext()
	seenKills := map[int64]bool{}
	for _, m := range w.Matches {
		for i := range m.Rounds {
			m.Rounds[i].MatchID = m.ID
			if m.Rounds[i].ID == 0 {
				maxR++
				m.Rounds[i].ID = maxR
			}
			if k := m.Rounds[i].KillID; k != nil {
				if seenKills[*k] {
					return errors.New("duplicate kill")
				}
				seenKills[*k] = true
			}
		}
	}
	s.ts[id] = w
	return nil
}

func (s *memStore) LiveForServer(_ context.Context, serverID int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.ts {
		if t.ServerID == serverID && t.Status == StatusLive {
			return t.ID, nil
		}
	}
	return 0, nil
}

func (s *memStore) Due(_ context.Context, now time.Time) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id, t := range s.ts {
		if !t.Over() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *memStore) RecordPayout(_ context.Context, p Payout) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := fmt.Sprint(p.TournamentID, ":", p.EntryID, ":", p.PlayerID)
	if _, ok := s.payouts[k]; ok {
		return false, nil
	}
	s.payouts[k] = p
	return true, nil
}

func (s *memStore) SetTitle(_ context.Context, installationID, playerID int64, title string, tournamentID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.titles[fmt.Sprint(installationID, ":", playerID)] = title
	return nil
}

type memPayer struct {
	mu    sync.Mutex
	paid  map[string]int64
	calls int
}

func (p *memPayer) Pay(_ context.Context, guildID, serverID, playerID, amount int64, reference, description string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.paid == nil {
		p.paid = map[string]int64{}
	}
	key := fmt.Sprint(playerID, "|", reference)
	if _, ok := p.paid[key]; ok {
		return 0, nil // the economy's own idempotency
	}
	p.paid[key] = amount
	return int64(len(p.paid)), nil
}

type memNotifier struct {
	mu     sync.Mutex
	events []Event
}

func (n *memNotifier) Notify(_ context.Context, t *Tournament, ev []Event) {
	n.mu.Lock()
	n.events = append(n.events, ev...)
	n.mu.Unlock()
}

func (n *memNotifier) count(k EventKind) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, e := range n.events {
		if e.Kind == k {
			c++
		}
	}
	return c
}

func newTestService(store *memStore, payer *memPayer) (*Service, *memNotifier, *time.Time) {
	now := t0
	svc := NewService(store, payer, func(ctx context.Context, t *Tournament) []Rank {
		var out []Rank
		for _, e := range t.Entries {
			out = append(out, Rank{EntryID: e.ID, RP: 100 - e.ID})
		}
		return out
	}, func(ctx context.Context, t *Tournament, players []int64) map[int64]bool {
		out := map[int64]bool{}
		for _, p := range players {
			out[p] = true
		}
		return out
	})
	svc.SetClock(func() time.Time { return now }, func() int64 { return 1 })
	n := &memNotifier{}
	svc.SetNotifier(n)
	return svc, n, &now
}

func TestServiceLifecycleAndPrizeIdempotency(t *testing.T) {
	ctx := context.Background()
	store, payer := newMemStore(), &memPayer{}
	svc, notes, now := newTestService(store, payer)
	title := "Friday Champion"
	tr, err := svc.Create(ctx, &Tournament{InstallationID: 7, GuildID: 1, ServerID: 9}, Params{Name: "Friday 1v1", BracketSize: 4, BestOf: 1, Seeding: SeedingRanked, StartsAt: t0.Add(time.Hour), CheckinMinutes: 0,
		Prizes: []Prize{{Place: 1, Points: 1000, Title: &title}, {Place: 2, Points: 500}, {Place: 3, Points: 250}}})
	if err != nil {
		t.Fatal(err)
	}
	if tr.ID == 0 || tr.Status != StatusDraft {
		t.Fatalf("created: %+v", tr)
	}
	if _, err := svc.Open(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 4; i++ {
		if _, _, err := svc.Join(ctx, tr.ID, []EntryPlayer{{PlayerID: 100 + i, DiscordUserID: fmt.Sprint("d", i), Name: fmt.Sprint("P", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.Join(ctx, tr.ID, []EntryPlayer{{PlayerID: 101, DiscordUserID: "d1"}}); !errors.Is(err, ErrAlreadyEntered) {
		t.Fatalf("rejoin: %v", err)
	}
	// Nothing to do yet for the clock; the tick writes nothing.
	svc.Tick(ctx)
	if got, _ := store.Get(ctx, tr.ID); got.Status != StatusSignup {
		t.Fatalf("tick: %s", got.Status)
	}
	*now = t0.Add(time.Hour)
	svc.Tick(ctx)
	live, _ := store.Get(ctx, tr.ID)
	if live.Status != StatusLive || len(live.Matches) != 3 || live.Current() == nil {
		t.Fatalf("live: %s %d", live.Status, len(live.Matches))
	}
	if notes.count(EvStarted) != 1 || notes.count(EvMatchCalled) != 1 {
		t.Fatalf("events: %v", notes.events)
	}
	// Kills through OnKill: the current match (1 v 4) then the other semi and the final.
	play := func(killID int64) {
		t.Helper()
		cur, _ := store.Get(ctx, tr.ID)
		m := cur.Current()
		if m == nil {
			t.Fatal("no current match")
		}
		a, b := cur.Entry(*m.EntryA), cur.Entry(*m.EntryB)
		svc.OnKill(ctx, 9, Kill{KillID: killID, KillerPlayerID: a.Players[0].PlayerID, VictimPlayerID: b.Players[0].PlayerID, KillerName: a.Players[0].Name, VictimName: b.Players[0].Name, Weapon: "M4-A1", At: *now})
	}
	play(1)
	// A replayed kill (same id) changes nothing.
	svc.OnKill(ctx, 9, Kill{KillID: 1, KillerPlayerID: 101, VictimPlayerID: 104, Weapon: "M4-A1", At: *now})
	// A kill on another server changes nothing.
	svc.OnKill(ctx, 8, Kill{KillID: 2, KillerPlayerID: 101, VictimPlayerID: 104, Weapon: "M4-A1", At: *now})
	mid, _ := store.Get(ctx, tr.ID)
	if mid.MatchAt(1, 1).Status != MatchDone || len(mid.MatchAt(1, 1).Rounds) != 1 {
		t.Fatalf("first match: %+v", mid.MatchAt(1, 1))
	}
	play(3)
	play(4)
	done, _ := store.Get(ctx, tr.ID)
	if done.Status != StatusFinished || done.Champion() == nil {
		t.Fatalf("finished: %s", done.Status)
	}
	if notes.count(EvFinished) != 1 {
		t.Fatalf("finished events: %d", notes.count(EvFinished))
	}
	// Prizes: the champion 1000, the finalist 500, both semi-final losers 250; one payout each.
	if len(store.payouts) != 4 || len(payer.paid) != 4 {
		t.Fatalf("payouts %d paid %d", len(store.payouts), len(payer.paid))
	}
	champ := done.Champion().Players[0].PlayerID
	if payer.paid[fmt.Sprint(champ, "|tournament:", tr.ID, ":entry:", done.Champion().ID)] != 1000 {
		t.Fatalf("champion prize: %v", payer.paid)
	}
	if store.titles[fmt.Sprint(7, ":", champ)] != title {
		t.Fatalf("title: %v", store.titles)
	}
	// Settling again (a retried notification, a restart) pays nothing twice.
	svc.SettlePrizes(ctx, done)
	svc.SettlePrizes(ctx, done)
	if len(store.payouts) != 4 || len(payer.paid) != 4 || payer.calls != 4 {
		t.Fatalf("idempotency: payouts %d paid %d calls %d", len(store.payouts), len(payer.paid), payer.calls)
	}
	// A finished tournament is no longer the server's live one.
	if id, _ := store.LiveForServer(ctx, 9); id != 0 {
		t.Fatal("finished tournaments are not live")
	}
}

func TestServiceTransactionRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	svc, _, _ := newTestService(store, nil)
	tr, err := svc.Create(ctx, &Tournament{InstallationID: 7, GuildID: 1, ServerID: 9}, Params{Name: "x", StartsAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, tr.ID); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("start a draft: %v", err)
	}
	if got, _ := store.Get(ctx, tr.ID); got.Status != StatusDraft {
		t.Fatal("a refused transition must leave the row alone")
	}
	if _, err := svc.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	// Update while DRAFT works; a shrink below the entries does not.
	if _, err := svc.Update(ctx, tr.ID, Params{Name: "y", StartsAt: t0, BracketSize: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Open(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 3; i++ {
		if _, _, err := svc.Join(ctx, tr.ID, []EntryPlayer{{PlayerID: i, DiscordUserID: fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Update(ctx, tr.ID, Params{Name: "y", StartsAt: t0, BracketSize: 4, TeamSize: 2}); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("team size change with entries: %v", err)
	}
	_ = rand.New
}
