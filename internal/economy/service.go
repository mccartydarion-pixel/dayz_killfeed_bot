// Package economy is the Champion Points economy service: credit, debit, balance
// lookup, transaction history and admin adjustments, on top of the single
// point_transactions ledger / player_points balance store. Balances are
// guild-wide (see repository/economy_repository.go). Shop purchases and
// refunds are written by the shop's own transaction (see Announce), never
// through Credit/Debit.
package economy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Transaction types the service accepts or reports. BountyClaim is paid only from
// inside the bounty claim's own transaction (never through Credit) and keeps its
// historical name because it is part of every existing row's idempotency key.
const (
	TypeBountyClaim  = repository.TxBountyClaim
	TypeAdminCredit  = repository.TxAdminCredit
	TypeAdminDebit   = repository.TxAdminDebit
	TypeSystemReward = repository.TxSystemReward
	// Shop payments are written by the shop repository through the same ledger path; the
	// economy service never accepts them from Credit/Debit (only the shop's transaction may).
	TypeShopPurchase = repository.TxShopPurchase
	TypeShopRefund   = repository.TxShopRefund
)

const (
	MaxAmount         = repository.MaxLedgerAmount
	MaxDescriptionLen = 200
	MaxReferenceLen   = 100
)

var (
	ErrInvalidAmount     = errors.New("amount must be a positive number within the allowed range")
	ErrPlayerNotFound    = errors.New("player not found in this guild")
	ErrServerNotInGuild  = errors.New("server does not belong to this guild")
	ErrForbiddenTenant   = errors.New("server belongs to a different organization")
	ErrServerRequired    = errors.New("an organization-scoped operation must name a server")
	ErrActorRequired     = errors.New("an admin adjustment must record who performed it")
	ErrTypeNotAllowed    = errors.New("transaction type is not allowed for this operation")
	ErrInsufficientFunds = repository.ErrInsufficientFunds
)

// Event is one committed economy transaction, handed to the Notifier AFTER the
// database commit. It carries no internal ids and no actor: only what the public
// ECONOMY feed may show (display name, type, amount, and the resulting balance).
type Event struct {
	Type         string
	GuildID      int64
	ServerID     int64 // attribution only (0 = guild-wide)
	PlayerName   string
	Amount       int64 // magnitude, always positive
	Credit       bool  // true = points added, false = points removed
	BalanceAfter int64
	Item         string // shop events only: the public product name
}

// Notifier receives committed transactions. It must not block and cannot fail the
// operation; the Service recovers a panicking one.
type Notifier interface{ Notify(Event) }

// Store is the persistence the Service needs; *repository.EconomyRepository
// implements it.
type Store interface {
	Credit(ctx context.Context, p repository.LedgerParams) (repository.LedgerEntry, error)
	Debit(ctx context.Context, p repository.LedgerParams) (repository.LedgerEntry, error)
	Balance(ctx context.Context, guildID, playerID int64) (int64, error)
	History(ctx context.Context, guildID, playerID int64, limit int, beforeID int64) ([]repository.LedgerEntry, int64, error)
	PlayerName(ctx context.Context, guildID, playerID int64) (string, bool, error)
	ServerScope(ctx context.Context, serverID int64) (guildID int64, organizationID *int64, found bool, err error)
}

type Service struct {
	store Store

	mu       sync.RWMutex
	notifier Notifier
}

func NewService(store Store, notifier Notifier) *Service {
	return &Service{store: store, notifier: notifier}
}

// SetNotifier attaches (or replaces) the transaction notifier.
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
			slog.Error("component=economy", "msg", "economy notifier panic recovered", "type", e.Type, "panic", fmt.Sprint(r))
		}
	}()
	n.Notify(e)
}

// Request is a credit or debit. Amount is positive; the operation decides the
// direction. Actor is the Discord user id of an admin (internal audit; never
// published). ReferenceID makes the operation idempotent per (guild, player, type);
// empty = every call is a new transaction. OrganizationID (optional) makes the
// call tenant-checked: the named server must not belong to another organization.
type Request struct {
	GuildID        int64
	ServerID       int64 // attribution only; 0 = none
	OrganizationID int64 // 0 = trusted internal caller
	PlayerID       int64
	Amount         int64
	Type           string
	ReferenceID    string
	Description    string
	Actor          string
}

// Result is what a committed operation returns: the new balance and the
// transaction id (an internal handle for callers; never shown to players).
// Duplicate reports an idempotent replay - nothing changed and no event fired.
type Result struct {
	TransactionID int64
	Balance       int64
	Duplicate     bool
	Amount        int64     // signed: debits are negative (for a replay, the original entry's amount)
	CreatedAt     time.Time // when the transaction was recorded
}

var creditTypes = map[string]bool{TypeAdminCredit: true, TypeSystemReward: true}
var debitTypes = map[string]bool{TypeAdminDebit: true}

// Credit adds points. Only ADMIN_CREDIT and SYSTEM_REWARD are creditable here;
// SYSTEM_REWARD is an *earned* credit (it also raises the leaderboard scores),
// an admin credit is a balance adjustment only.
func (s *Service) Credit(ctx context.Context, req Request) (Result, error) {
	return s.apply(ctx, req, false)
}

// Debit removes points, never below zero (ErrInsufficientFunds).
func (s *Service) Debit(ctx context.Context, req Request) (Result, error) {
	return s.apply(ctx, req, true)
}

// AdminCredit / AdminDebit are the audited admin adjustments: the actor is
// required and stored on the ledger row (internal), with an optional reason.
func (s *Service) AdminCredit(ctx context.Context, req Request) (Result, error) {
	req.Type = TypeAdminCredit
	return s.apply(ctx, req, false)
}

func (s *Service) AdminDebit(ctx context.Context, req Request) (Result, error) {
	req.Type = TypeAdminDebit
	return s.apply(ctx, req, true)
}

func (s *Service) apply(ctx context.Context, req Request, debit bool) (Result, error) {
	allowed := creditTypes
	if debit {
		allowed = debitTypes
	}
	if !allowed[req.Type] {
		return Result{}, ErrTypeNotAllowed
	}
	if req.Amount <= 0 || req.Amount > MaxAmount {
		return Result{}, ErrInvalidAmount
	}
	isAdmin := req.Type == TypeAdminCredit || req.Type == TypeAdminDebit
	actor := strings.TrimSpace(req.Actor)
	if isAdmin && actor == "" {
		return Result{}, ErrActorRequired
	}
	if !isAdmin && actor == "" {
		actor = "SYSTEM"
	}
	if req.OrganizationID != 0 && req.ServerID == 0 {
		return Result{}, ErrServerRequired
	}
	name, ok, err := s.store.PlayerName(ctx, req.GuildID, req.PlayerID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, ErrPlayerNotFound
	}
	if req.ServerID != 0 {
		guildID, org, found, err := s.store.ServerScope(ctx, req.ServerID)
		if err != nil {
			return Result{}, err
		}
		if !found || guildID != req.GuildID {
			return Result{}, ErrServerNotInGuild
		}
		if req.OrganizationID != 0 && org != nil && *org != req.OrganizationID {
			return Result{}, ErrForbiddenTenant
		}
	}

	params := repository.LedgerParams{
		GuildID: req.GuildID, PlayerID: req.PlayerID, ServerID: req.ServerID, Type: req.Type,
		Amount: req.Amount, Earned: req.Type == TypeSystemReward,
		ReferenceID: clip(strings.TrimSpace(req.ReferenceID), MaxReferenceLen),
		Description: sanitizeText(req.Description, MaxDescriptionLen), CreatedBy: actor,
	}
	var entry repository.LedgerEntry
	if debit {
		entry, err = s.store.Debit(ctx, params)
	} else {
		entry, err = s.store.Credit(ctx, params)
	}
	if err != nil {
		return Result{}, err
	}
	res := Result{TransactionID: entry.ID, Balance: entry.BalanceAfter, Duplicate: entry.Duplicate, Amount: entry.Amount, CreatedAt: entry.CreatedAt}
	if !entry.Duplicate {
		// Only after the commit; a failing notifier can neither undo nor repeat it.
		s.notify(Event{Type: req.Type, GuildID: req.GuildID, ServerID: req.ServerID, PlayerName: name, Amount: req.Amount, Credit: !debit, BalanceAfter: entry.BalanceAfter})
	}
	return res, nil
}

// Balance returns the player's spendable balance: 0 for a valid player with no
// transactions, ErrPlayerNotFound for a player that is not in this guild.
func (s *Service) Balance(ctx context.Context, guildID, playerID int64) (int64, error) {
	_, ok, err := s.store.PlayerName(ctx, guildID, playerID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrPlayerNotFound
	}
	return s.store.Balance(ctx, guildID, playerID)
}

// HistoryItem is one line of a player's history. It carries no internal ids and
// no actor.
type HistoryItem struct {
	Type         string
	Label        string
	Amount       int64 // signed
	Description  string
	BalanceAfter int64
	Item         string // shop events only: the public product name
	CreatedAt    time.Time
}

// Page is one bounded page of history, newest first. NextCursor is non-zero when
// older entries remain; pass it back as the cursor for the next page.
type Page struct {
	Items      []HistoryItem
	NextCursor int64
}

// History returns up to limit entries (default 10, hard cap 50) before cursor.
func (s *Service) History(ctx context.Context, guildID, playerID int64, limit int, cursor int64) (Page, error) {
	_, ok, err := s.store.PlayerName(ctx, guildID, playerID)
	if err != nil {
		return Page{}, err
	}
	if !ok {
		return Page{}, ErrPlayerNotFound
	}
	entries, next, err := s.store.History(ctx, guildID, playerID, limit, cursor)
	if err != nil {
		return Page{}, err
	}
	page := Page{NextCursor: next, Items: make([]HistoryItem, 0, len(entries))}
	for _, e := range entries {
		page.Items = append(page.Items, HistoryItem{Type: e.Type, Label: TypeLabel(e.Type), Amount: e.Amount, Description: e.Description, BalanceAfter: e.BalanceAfter, CreatedAt: e.CreatedAt})
	}
	return page, nil
}

// TypeLabel is the player-facing name of a transaction type.
func TypeLabel(t string) string {
	switch {
	case t == TypeBountyClaim:
		return "Bounty reward"
	case t == TypeAdminCredit:
		return "Admin credit"
	case t == TypeAdminDebit:
		return "Admin debit"
	case t == TypeSystemReward:
		return "Reward"
	case t == TypeShopPurchase:
		return "Shop purchase"
	case t == TypeShopRefund:
		return "Shop refund"
	case strings.HasPrefix(t, "EVENT_"):
		return "Event prize"
	}
	return "Adjustment"
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// sanitizeText trims, drops control characters and bounds a free-text reason.
func sanitizeText(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return clip(b.String(), max)
}

// Announce hands an already-committed transaction written elsewhere (the shop's purchase and refund,
// which commit inside their own database transaction) to the notifier. Like every notification it
// happens only after the commit and can neither fail nor repeat the operation.
func (s *Service) Announce(e Event) {
	if s == nil {
		return
	}
	s.notify(e)
}
