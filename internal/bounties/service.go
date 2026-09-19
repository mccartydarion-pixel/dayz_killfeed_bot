package bounties

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

const AllowTeamKillClaims = false

type Tier struct{ MinimumStreak, RewardPoints int }

var Tiers = []Tier{{10, 500}, {15, 750}, {20, 1000}, {25, 1500}}

func RewardForStreak(streak int) int {
	reward := 0
	for _, tier := range Tiers {
		if streak >= tier.MinimumStreak {
			reward = tier.RewardPoints
		}
	}
	return reward
}
func ShouldUpgrade(current, recommended int64) bool { return recommended > current }

// MaxAmount is the largest bounty amount (the column is a 32-bit integer).
const MaxAmount = math.MaxInt32

var (
	ErrInvalidAmount    = errors.New("bounty amount must be a positive number")
	ErrTargetNotFound   = errors.New("target player not found in this guild")
	ErrServerNotInGuild = errors.New("server does not belong to this guild")
	ErrForbiddenTenant  = errors.New("server belongs to a different organization")
	ErrServerRequired   = errors.New("an organization-scoped bounty must name a server")
	ErrInvalidExpiry    = errors.New("bounty expiry must be in the future")
)

// EventKind is a bounty lifecycle event.
type EventKind string

const (
	EventPlaced    EventKind = "PLACED"
	EventIncreased EventKind = "INCREASED"
	EventClaimed   EventKind = "CLAIMED"
	EventExpired   EventKind = "EXPIRED"
	EventCancelled EventKind = "CANCELLED"
)

// Event is one lifecycle event handed to the Notifier. It deliberately carries
// no internal ids (bounty, player, kill): only what the public feeds may show.
//
// Routing scope: ServerID is the server the bounty is scoped to (0 = guild-wide).
// KillServerID is the server a claim (or a streak-driven placement) happened on.
// A publisher routes by ServerID when set, else by KillServerID, else to every
// server of the guild.
type Event struct {
	Kind         EventKind
	GuildID      int64
	ServerID     int64
	KillServerID int64

	Target string
	Hunter string // claims only
	Amount int64  // placed/new/claimed-total amount, in Champion Points
	Count  int    // claims: how many bounties were claimed by the kill

	Weapon   string   // claims: only when the authoritative kill has one
	Distance *float64 // claims: only when the authoritative kill has one

	Automatic bool // placed/increased by the streak system, not by a person
}

// Notifier receives lifecycle events AFTER the database change committed. It must
// not block and cannot fail the operation; the Service recovers a panicking one.
type Notifier interface{ Notify(Event) }

// Store is the persistence the Service needs; *repository.BountyRepository
// implements it.
type Store interface {
	Create(ctx context.Context, b repository.Bounty, creator string) (*repository.Bounty, error)
	Increase(ctx context.Context, guildID, bountyID, newAmount int64) (*repository.Bounty, error)
	Cancel(ctx context.Context, guildID, bountyID int64) (*repository.Bounty, error)
	UpgradeReturning(ctx context.Context, guildID, bountyID int64, reward int) (*repository.Bounty, error)
	GetActiveAutomatic(ctx context.Context, guildID, targetID int64) (*repository.Bounty, error)
	ClaimForKill(ctx context.Context, c repository.KillClaim) ([]repository.Bounty, error)
	ExpireDue(ctx context.Context, now time.Time) ([]repository.Bounty, error)
	PlayerName(ctx context.Context, guildID, playerID int64) (string, bool, error)
	ServerScope(ctx context.Context, serverID int64) (guildID int64, organizationID *int64, found bool, err error)
}

// Service is the bounty application service: placement with tenant validation,
// the atomic claim for persisted kills, streak-driven bounties, expiry, and
// lifecycle notifications. Bounty correctness never depends on Discord: the
// Notifier is invoked only after a change committed, is optional, and a failing
// or absent one changes nothing that was stored.
type Service struct {
	store Store

	mu       sync.RWMutex
	notifier Notifier
}

func NewService(store Store, notifier Notifier) *Service {
	return &Service{store: store, notifier: notifier}
}

// SetNotifier attaches (or replaces) the lifecycle notifier.
func (s *Service) SetNotifier(n Notifier) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

func (s *Service) notify(e Event) {
	s.mu.RLock()
	n := s.notifier
	s.mu.RUnlock()
	if n == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=bounty", "msg", "bounty notifier panic recovered", "kind", string(e.Kind), "panic", fmt.Sprint(r))
		}
	}()
	n.Notify(e)
}

func (s *Service) nameOf(ctx context.Context, guildID, playerID int64) string {
	name, ok, err := s.store.PlayerName(ctx, guildID, playerID)
	if err != nil || !ok {
		return ""
	}
	return name
}

// PlaceRequest is a bounty placement. Phase 1 deducts nothing from anyone: there
// is no economy yet, the amount is simply what a claimant is awarded in Champion
// Points.
type PlaceRequest struct {
	GuildID        int64
	ServerID       int64 // 0 = guild-wide
	OrganizationID int64 // 0 = not tenant-checked (trusted internal caller such as the Discord admin command)
	TargetPlayerID int64
	Amount         int64
	PlacedBy       string // Discord user id or a system source
	Reason         string
	ExpiresAt      *time.Time // nil = never expires (the Phase 1 default)
}

// Place creates a bounty after validating: amount > 0; the target exists in the
// guild; the server (if any) belongs to the guild and - when an organization is
// given - is not claimed by a different organization. An organization-scoped
// caller must name a server, so it can never create a guild-wide bounty over
// servers it does not own.
func (s *Service) Place(ctx context.Context, req PlaceRequest) (*repository.Bounty, error) {
	if req.Amount <= 0 || req.Amount > MaxAmount {
		return nil, ErrInvalidAmount
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return nil, ErrInvalidExpiry
	}
	if req.OrganizationID != 0 && req.ServerID == 0 {
		return nil, ErrServerRequired
	}
	name, ok, err := s.store.PlayerName(ctx, req.GuildID, req.TargetPlayerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrTargetNotFound
	}
	if req.ServerID != 0 {
		guildID, org, found, err := s.store.ServerScope(ctx, req.ServerID)
		if err != nil {
			return nil, err
		}
		if !found || guildID != req.GuildID {
			return nil, ErrServerNotInGuild
		}
		if req.OrganizationID != 0 && org != nil && *org != req.OrganizationID {
			return nil, ErrForbiddenTenant
		}
	}
	starts := time.Now().UTC()
	created, err := s.store.Create(ctx, repository.Bounty{
		GuildID: req.GuildID, ServerID: req.ServerID, TargetPlayerID: req.TargetPlayerID,
		CreatedByType: repository.BountyAdmin, RewardPoints: req.Amount, Reason: req.Reason,
		StartsAt: &starts, ExpiresAt: req.ExpiresAt,
	}, req.PlacedBy)
	if err != nil {
		return nil, err
	}
	s.notify(Event{Kind: EventPlaced, GuildID: created.GuildID, ServerID: created.ServerID, Target: name, Amount: created.RewardPoints})
	return created, nil
}

// Increase raises an active bounty of the guild to newAmount (it must be higher).
func (s *Service) Increase(ctx context.Context, guildID, bountyID, newAmount int64) (*repository.Bounty, error) {
	if newAmount <= 0 || newAmount > MaxAmount {
		return nil, ErrInvalidAmount
	}
	b, err := s.store.Increase(ctx, guildID, bountyID, newAmount)
	if err != nil {
		return nil, err
	}
	s.notify(Event{Kind: EventIncreased, GuildID: b.GuildID, ServerID: b.ServerID, Target: s.nameOf(ctx, b.GuildID, b.TargetPlayerID), Amount: b.RewardPoints})
	return b, nil
}

// Cancel cancels an active bounty of the guild.
func (s *Service) Cancel(ctx context.Context, guildID, bountyID int64) (*repository.Bounty, error) {
	b, err := s.store.Cancel(ctx, guildID, bountyID)
	if err != nil {
		return nil, err
	}
	s.notify(Event{Kind: EventCancelled, GuildID: b.GuildID, ServerID: b.ServerID, Target: s.nameOf(ctx, b.GuildID, b.TargetPlayerID), Amount: b.RewardPoints})
	return b, nil
}

// Sweep transitions every due ACTIVE bounty to EXPIRED atomically (one statement,
// no per-bounty goroutine) and reports each expiry once. Returns how many expired.
func (s *Service) Sweep(ctx context.Context, now time.Time) (int, error) {
	expired, err := s.store.ExpireDue(ctx, now)
	if err != nil {
		return 0, err
	}
	for _, b := range expired {
		s.notify(Event{Kind: EventExpired, GuildID: b.GuildID, ServerID: b.ServerID, Target: s.nameOf(ctx, b.GuildID, b.TargetPlayerID), Amount: b.RewardPoints})
	}
	return len(expired), nil
}

// KillInput is a durably persisted PvP kill. HunterName/TargetName/Weapon/Distance
// come from the authoritative kill and only feed the lifecycle event.
type KillInput struct {
	GuildID, ServerID              int64
	VictimPlayerID, KillerPlayerID int64
	KillID, SeasonID               int64
	At                             time.Time
	HunterName, TargetName, Weapon string
	Distance                       *float64
}

// ClaimResult is what one kill claimed.
type ClaimResult struct {
	Count int
	Total int64
}

// ClaimForKill claims every eligible active bounty on the victim for the killer in
// one atomic transaction and reports the claim afterwards. It must only be called
// for a persisted, non-duplicate PvP kill; it refuses self-kills and unresolved
// players itself. The claim is committed before any notification, and a
// notification failure can never undo it or cause a second claim.
func (s *Service) ClaimForKill(ctx context.Context, in KillInput) (ClaimResult, error) {
	claimed, err := s.store.ClaimForKill(ctx, repository.KillClaim{
		GuildID: in.GuildID, ServerID: in.ServerID, VictimPlayerID: in.VictimPlayerID,
		KillerPlayerID: in.KillerPlayerID, KillID: in.KillID, SeasonID: in.SeasonID, At: in.At,
	})
	if err != nil || len(claimed) == 0 {
		return ClaimResult{}, err
	}
	var total int64
	for _, b := range claimed {
		total += b.RewardPoints
	}
	s.notify(Event{
		Kind: EventClaimed, GuildID: in.GuildID, KillServerID: in.ServerID,
		Target: in.TargetName, Hunter: in.HunterName, Amount: total, Count: len(claimed),
		Weapon: in.Weapon, Distance: in.Distance,
	})
	return ClaimResult{Count: len(claimed), Total: total}, nil
}

// StreakInput is a killer's post-kill streak (see RewardForStreak).
type StreakInput struct {
	GuildID, ServerID, PlayerID, SeasonID int64
	Streak                                int
	At                                    time.Time
	PlayerName                            string
}

// NoteStreak keeps the existing streak-driven ("automatic") bounty rule: reaching
// a streak tier puts a guild-wide bounty on the killer, and a higher tier raises
// it - never lowers it. There is still at most one active automatic bounty per
// target (a unique index enforces it, so concurrent kills cannot create two);
// manual bounties on the same player are independent and stack.
func (s *Service) NoteStreak(ctx context.Context, in StreakInput) {
	reward := RewardForStreak(in.Streak)
	if reward == 0 || in.PlayerID == 0 {
		return
	}
	current, err := s.store.GetActiveAutomatic(ctx, in.GuildID, in.PlayerID)
	if err != nil {
		return
	}
	if current != nil {
		if int64(reward) > current.RewardPoints {
			if up, upErr := s.store.UpgradeReturning(ctx, in.GuildID, current.ID, reward); upErr == nil {
				s.notify(Event{Kind: EventIncreased, GuildID: in.GuildID, KillServerID: in.ServerID, Target: in.PlayerName, Amount: up.RewardPoints, Automatic: true})
			}
		}
		return
	}
	starts := in.At
	created, err := s.store.Create(ctx, repository.Bounty{
		GuildID: in.GuildID, SeasonID: in.SeasonID, TargetPlayerID: in.PlayerID,
		CreatedByType: repository.BountyAutomatic, RewardPoints: int64(reward),
		Reason: fmt.Sprintf("%d kill streak", in.Streak), StartsAt: &starts,
	}, "")
	if err != nil {
		return // ErrDuplicate: a concurrent kill already created it
	}
	s.notify(Event{Kind: EventPlaced, GuildID: in.GuildID, KillServerID: in.ServerID, Target: in.PlayerName, Amount: created.RewardPoints, Automatic: true})
}
