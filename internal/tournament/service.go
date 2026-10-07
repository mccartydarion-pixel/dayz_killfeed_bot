package tournament

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// Store is the persistence the Service needs; *repository.TournamentRepository implements it.
type Store interface {
	// Create inserts a new tournament (with no entries or matches) and sets its id.
	Create(ctx context.Context, t *Tournament) error
	// Get loads the whole tournament; ErrNotFound when there is none.
	Get(ctx context.Context, id int64) (*Tournament, error)
	// Transact locks the tournament row, loads the aggregate, runs fn and writes back what fn
	// changed, all in one transaction. fn's error rolls back and is returned.
	Transact(ctx context.Context, id int64, fn func(t *Tournament) error) error
	// LiveForServer is the id of the LIVE tournament on a server (0 when none).
	LiveForServer(ctx context.Context, serverID int64) (int64, error)
	// Due lists the tournaments the clock may act on: not over, with a timer at or before now
	// (sign-up opening, check-in opening, start, a match timer).
	Due(ctx context.Context, now time.Time) ([]int64, error)
	// RecordPayout records a prize payment once; false when it was recorded before.
	RecordPayout(ctx context.Context, p Payout) (bool, error)
	// SetTitle gives a player the installation's tournament title.
	SetTitle(ctx context.Context, installationID, playerID int64, title string, tournamentID int64) error
}

// Payout is one prize paid to one player.
type Payout struct {
	TournamentID  int64
	EntryID       int64
	PlayerID      int64
	Place         int
	Points        int64
	TransactionID int64
}

// Payer credits Champion Points; the economy service implements it. The reference makes a
// repeat a no-op.
type Payer interface {
	Pay(ctx context.Context, guildID, serverID, playerID, amount int64, reference, description string) (transactionID int64, err error)
}

// Notifier is told what happened after each committed transition (Discord).
type Notifier interface {
	Notify(ctx context.Context, t *Tournament, events []Event)
}

// Ranker gives the RANKED seeding its figures: the RP of each entry's players on the server.
type Ranker func(ctx context.Context, t *Tournament) []Rank

// Presence says which of the given players are on the server right now.
type Presence func(ctx context.Context, t *Tournament, players []int64) map[int64]bool

// Service runs tournaments.
type Service struct {
	store    Store
	payer    Payer
	ranker   Ranker
	presence Presence
	now      func() time.Time
	seed     func() int64

	mu       sync.RWMutex
	notifier Notifier
}

// NewService wires a service. payer, ranker and presence may be nil (no prizes are paid; RANKED
// seeding falls back to check-in order; a ready timeout always pings an admin).
func NewService(store Store, payer Payer, ranker Ranker, presence Presence) *Service {
	return &Service{store: store, payer: payer, ranker: ranker, presence: presence, now: func() time.Time { return time.Now().UTC() },
		seed: func() int64 { return time.Now().UnixNano() }}
}

// SetNotifier attaches the Discord side.
func (s *Service) SetNotifier(n Notifier) {
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

// SetClock replaces the clock and the random seed (tests).
func (s *Service) SetClock(now func() time.Time, seed func() int64) {
	s.now, s.seed = now, seed
}

func (s *Service) notify(ctx context.Context, t *Tournament, events []Event) {
	if len(events) == 0 {
		return
	}
	s.mu.RLock()
	n := s.notifier
	s.mu.RUnlock()
	if n == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=tournament", "msg", "notifier panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	n.Notify(ctx, t, events)
}

// Create stores a new DRAFT tournament.
func (s *Service) Create(ctx context.Context, t *Tournament, p Params) (*Tournament, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if t.InstallationID == 0 || t.GuildID == 0 || t.ServerID == 0 {
		return nil, fmt.Errorf("%w: an installation with a server is required", ErrInvalid)
	}
	t.Apply(p)
	t.Status = StatusDraft
	t.CreatedAt = s.now()
	t.UpdatedAt = t.CreatedAt
	if err := s.store.Create(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// Get loads a tournament.
func (s *Service) Get(ctx context.Context, id int64) (*Tournament, error) {
	return s.store.Get(ctx, id)
}

// Update edits a DRAFT or SIGNUP tournament.
func (s *Service) Update(ctx context.Context, id int64, p Params) (*Tournament, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		if t.Status != StatusDraft && t.Status != StatusSignup {
			return nil, ErrWrongStatus
		}
		if len(t.Entries) > 0 && (p.TeamSize != t.TeamSize || p.BracketSize < t.entered()) {
			return nil, fmt.Errorf("%w: entries have joined; the team size cannot change and the bracket cannot shrink below them", ErrWrongStatus)
		}
		t.Apply(p)
		return []Event{{Kind: EvSignupChanged}}, nil
	})
}

// ErrNoChange is returned by a transition that found nothing to do: the transaction is rolled
// back (nothing was written) and nothing is announced.
var ErrNoChange = errors.New("nothing to change")

// transition runs one mutation under the row lock and notifies after the commit.
func (s *Service) transition(ctx context.Context, id int64, fn func(t *Tournament) ([]Event, error)) (*Tournament, error) {
	var out *Tournament
	var events []Event
	err := s.store.Transact(ctx, id, func(t *Tournament) error {
		out = t
		ev, err := fn(t)
		if err != nil {
			return err
		}
		t.UpdatedAt = s.now()
		events = ev
		return nil
	})
	if errors.Is(err, ErrNoChange) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	s.afterCommit(ctx, out, events)
	return out, nil
}

// afterCommit pays prizes and writes the title when a tournament just finished, then notifies.
func (s *Service) afterCommit(ctx context.Context, t *Tournament, events []Event) {
	for _, e := range events {
		if e.Kind == EvFinished {
			s.SettlePrizes(ctx, t)
		}
	}
	s.notify(ctx, t, events)
}

// SettlePrizes pays every prize once and gives the champion the title. Idempotent: a payout is
// recorded per tournament, entry and player before the credit, and the credit's own reference
// makes a repeat a no-op.
func (s *Service) SettlePrizes(ctx context.Context, t *Tournament) {
	for _, e := range t.Entries {
		place := t.Place(e.ID)
		if place == 0 {
			continue
		}
		for _, prize := range t.Prizes {
			if prize.Place != place {
				continue
			}
			for _, p := range e.Players {
				if place == 1 && prize.Title != nil {
					if err := s.store.SetTitle(ctx, t.InstallationID, p.PlayerID, *prize.Title, t.ID); err != nil {
						slog.Warn("component=tournament", "event", "title_failed", "tournament_id", t.ID, "player_id", p.PlayerID, "err", err.Error())
					}
				}
				if prize.Points <= 0 {
					continue
				}
				po := Payout{TournamentID: t.ID, EntryID: e.ID, PlayerID: p.PlayerID, Place: place, Points: prize.Points}
				fresh, err := s.store.RecordPayout(ctx, po)
				if err != nil {
					slog.Warn("component=tournament", "event", "payout_record_failed", "tournament_id", t.ID, "player_id", p.PlayerID, "err", err.Error())
					continue
				}
				if !fresh || s.payer == nil {
					continue
				}
				ref := fmt.Sprintf("tournament:%d:entry:%d", t.ID, e.ID)
				desc := fmt.Sprintf("Tournament prize: %s place in %s", ordinal(place), t.Name)
				if _, err := s.payer.Pay(ctx, t.GuildID, t.ServerID, p.PlayerID, prize.Points, ref, desc); err != nil {
					slog.Warn("component=tournament", "event", "payout_failed", "tournament_id", t.ID, "player_id", p.PlayerID, "err", err.Error())
				}
			}
		}
	}
}

func ordinal(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}

// --- owner actions ------------------------------------------------------------------------------------

func (s *Service) Open(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.OpenSignup(s.now()) })
}

// Start draws the bracket now (an admin starting early, or on time).
func (s *Service) Start(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		return t.Start(s.now(), s.ranks(ctx, t), rand.New(rand.NewSource(s.seed())))
	})
}

func (s *Service) Pause(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.Pause(s.now()) })
}

func (s *Service) Resume(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.Resume(s.now()) })
}

func (s *Service) Cancel(ctx context.Context, id int64, why string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.Cancel(s.now(), why) })
}

// Call calls the next match, or re-calls the one in play.
func (s *Service) Call(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		_, ev, err := t.CallNext(s.now())
		return ev, err
	})
}

// StartMatch is an admin starting the match in play before its first kill.
func (s *Service) StartMatch(ctx context.Context, id int64) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.StartCurrent(s.now()) })
}

func (s *Service) Result(ctx context.Context, id, matchID, winnerEntryID int64, by, note string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		return t.AdminResult(matchID, winnerEntryID, by, note, s.now())
	})
}

func (s *Service) Replay(ctx context.Context, id, matchID int64, by string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.Replay(matchID, by, s.now()) })
}

func (s *Service) DQ(ctx context.Context, id, entryID int64, by, why string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) { return t.DQ(entryID, by, why, s.now()) })
}

// SetMessages records the Discord messages the tournament edits (the sign-up card, the bracket).
func (s *Service) SetMessages(ctx context.Context, id int64, channelID, signupMessageID, bracketMessageID string) error {
	return s.store.Transact(ctx, id, func(t *Tournament) error {
		if channelID != "" {
			t.DiscordChannelID = channelID
		}
		if signupMessageID != "" {
			t.SignupMessageID = signupMessageID
		}
		if bracketMessageID != "" {
			t.BracketMessageID = bracketMessageID
		}
		return nil
	})
}

// --- player actions ------------------------------------------------------------------------------------

func (s *Service) Join(ctx context.Context, id int64, players []EntryPlayer) (*Tournament, *Entry, error) {
	var entry *Entry
	t, err := s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		e, ev, err := t.Join(s.now(), players)
		entry = e
		return ev, err
	})
	return t, entry, err
}

func (s *Service) Leave(ctx context.Context, id int64, discordUserID string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		_, ev, err := t.Leave(s.now(), discordUserID)
		return ev, err
	})
}

func (s *Service) Checkin(ctx context.Context, id int64, discordUserID string) (*Tournament, error) {
	return s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		_, ev, err := t.Checkin(s.now(), discordUserID)
		return ev, err
	})
}

// --- the feed ------------------------------------------------------------------------------------------

// OnKill hands a persisted kill to the live tournament of its server, if any. It never fails the
// kill path: errors are logged.
func (s *Service) OnKill(ctx context.Context, serverID int64, k Kill) {
	if s == nil || s.store == nil || serverID == 0 || k.KillID == 0 {
		return
	}
	id, err := s.store.LiveForServer(ctx, serverID)
	if err != nil {
		slog.Warn("component=tournament", "event", "live_lookup_failed", "server_id", serverID, "err", err.Error())
		return
	}
	if id == 0 {
		return
	}
	_, err = s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		d := t.Attribute(k)
		if d == nil {
			return nil, ErrNoChange
		}
		ev, err := t.RecordRound(d.Match, d.Round, s.now())
		if err != nil {
			return nil, err
		}
		if d.Ping != "" {
			ev = append(ev, Event{Kind: EvAdminPing, Match: d.Match, Round: &d.Match.Rounds[len(d.Match.Rounds)-1], Text: d.Ping})
		}
		return ev, nil
	})
	if err != nil {
		slog.Warn("component=tournament", "event", "kill_attribution_failed", "tournament_id", id, "kill_id", k.KillID, "err", err.Error())
	}
}

// OnDeath hands a persisted non-player death to the live tournament of its server, if any.
func (s *Service) OnDeath(ctx context.Context, serverID int64, d Death) {
	if s == nil || s.store == nil || serverID == 0 || d.PlayerID == 0 {
		return
	}
	id, err := s.store.LiveForServer(ctx, serverID)
	if err != nil || id == 0 {
		return
	}
	_, err = s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		dec := t.AttributeDeath(d)
		if dec == nil {
			return nil, ErrNoChange
		}
		ev, err := t.RecordRound(dec.Match, dec.Round, s.now())
		if err != nil {
			return nil, err
		}
		return append(ev, Event{Kind: EvAdminPing, Match: dec.Match, Round: &dec.Match.Rounds[len(dec.Match.Rounds)-1], Text: dec.Ping}), nil
	})
	if err != nil {
		slog.Warn("component=tournament", "event", "death_attribution_failed", "tournament_id", id, "err", err.Error())
	}
}

// --- the clock -----------------------------------------------------------------------------------------

// Tick applies everything that is due. One process runs it (the leader), every few seconds.
func (s *Service) Tick(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	now := s.now()
	ids, err := s.store.Due(ctx, now)
	if err != nil {
		slog.Warn("component=tournament", "event", "due_failed", "err", err.Error())
		return
	}
	for _, id := range ids {
		s.tickOne(ctx, id, now)
	}
}

func (s *Service) tickOne(ctx context.Context, id int64, now time.Time) {
	_, err := s.transition(ctx, id, func(t *Tournament) ([]Event, error) {
		var present map[int64]bool
		if m := t.Current(); m != nil && m.Status == MatchCalled && m.TimerEndsAt != nil && !now.Before(*m.TimerEndsAt) {
			present = s.present(ctx, t, m)
		}
		ev := t.Tick(now, s.ranks(ctx, t), rand.New(rand.NewSource(s.seed())), present)
		if len(ev) == 0 {
			return nil, ErrNoChange
		}
		return ev, nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		slog.Warn("component=tournament", "event", "tick_failed", "tournament_id", id, "err", err.Error())
	}
}

func (s *Service) ranks(ctx context.Context, t *Tournament) []Rank {
	if s.ranker == nil || t.Seeding != SeedingRanked {
		return nil
	}
	return s.ranker(ctx, t)
}

func (s *Service) present(ctx context.Context, t *Tournament, m *Match) map[int64]bool {
	if s.presence == nil {
		return nil
	}
	var players []int64
	for _, id := range []*int64{m.EntryA, m.EntryB} {
		if id == nil {
			continue
		}
		if e := t.Entry(*id); e != nil {
			for _, p := range e.Players {
				players = append(players, p.PlayerID)
			}
		}
	}
	return s.presence(ctx, t, players)
}
