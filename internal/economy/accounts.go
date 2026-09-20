package economy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Web-facing account layer (docs/ECONOMY.md). It adds no storage: an account is the existing
// (guild, player) pair behind the Champion Points ledger, reached through an installation
// (organization + Discord guild + DayZ server). Every balance change still goes through
// Service -> applyLedger; nothing here does balance math.

// Currency is the stable currency metadata every economy response carries.
type Currency struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Symbol string `json:"symbol"`
}

// ChampionPoints is the canonical currency: the existing, production Champion Points.
var ChampionPoints = Currency{Code: "CHAMPION_POINTS", Name: "Champion Points", Symbol: "pts"}

// MaxAdminAmount bounds one web admin adjustment (1e9). The ledger allows up to 1e15, but
// JSON numbers are only exact to 2^53, so a web operation is kept far below it.
const MaxAdminAmount int64 = 1_000_000_000

// Limits of the web API.
const (
	DefaultTransactionLimit = 25
	MaxTransactionLimit     = 100
	MinSearchLen            = 2
	MaxSearchLen            = 50
	MaxReasonLen            = 200
)

var (
	ErrInstallationNotFound = errors.New("installation not found")
	ErrIdentityRequired     = errors.New("a verified DayZ link is required")
	ErrAccountNotFound      = errors.New("economy account not found")
	ErrInvalidQuery         = errors.New("search text must be 2-50 characters")
	ErrInvalidCursor        = errors.New("invalid cursor")
	ErrInvalidFilter        = errors.New("unknown transaction type filter")
	ErrReasonRequired       = errors.New("a reason is required")
	ErrInvalidKey           = errors.New("idempotency key must be 8-64 characters of A-Z a-z 0-9 . _ : -")
	ErrIdempotencyMismatch  = errors.New("idempotency key was already used with a different amount")
	ErrNoServer             = errors.New("the installation has no DayZ server selected")
	ErrSuspended            = errors.New("the installation is suspended")
)

// Direction of a transaction.
const (
	DirectionCredit = "CREDIT"
	DirectionDebit  = "DEBIT"
)

// FilterEventPrize groups every EVENT_* prize type in a history filter.
const FilterEventPrize = "EVENT_PRIZE"

// AccountStore is the read side the account layer needs; *repository.EconomyRepository implements it.
type AccountStore interface {
	InstallationScope(ctx context.Context, organizationID, installationID int64) (repository.EconomyScope, bool, error)
	VerifiedPlayerID(ctx context.Context, guildID int64, discordUserID string) (int64, bool, error)
	Account(ctx context.Context, guildID, playerID int64) (repository.EconomyAccountRow, bool, error)
	SearchAccounts(ctx context.Context, guildID int64, q string, limit int) ([]repository.EconomyAccountRow, error)
	Transactions(ctx context.Context, guildID, playerID int64, limit int, beforeID int64, f repository.TransactionFilter) ([]repository.LedgerEntry, int64, error)
	Reconcile(ctx context.Context, guildID int64, limit int) ([]repository.BalanceMismatch, error)
}

// Accounts is the installation-scoped account API over Service.
type Accounts struct {
	svc   *Service
	store AccountStore
}

func NewAccounts(svc *Service, store AccountStore) *Accounts {
	return &Accounts{svc: svc, store: store}
}

// Scope resolves organization + installation; another tenant's installation is ErrInstallationNotFound.
func (a *Accounts) Scope(ctx context.Context, organizationID, installationID int64) (repository.EconomyScope, error) {
	s, found, err := a.store.InstallationScope(ctx, organizationID, installationID)
	if err != nil {
		return repository.EconomyScope{}, err
	}
	if !found {
		return repository.EconomyScope{}, ErrInstallationNotFound
	}
	return s, nil
}

// Account is one player's economy account as the API shows it.
type Account struct {
	AccountID     int64 // the DayZ player id inside the installation's guild
	Gamertag      string
	Balance       int64
	UpdatedAt     *time.Time
	Linked        bool
	DiscordUserID string // only for an admin view
	DisplayName   string // the linked Champion user's display name
	Avatar        string
}

func toAccount(r repository.EconomyAccountRow) Account {
	name := r.DiscordGlobalName
	if name == "" {
		name = r.DiscordUsername
	}
	return Account{AccountID: r.PlayerID, Gamertag: r.PlayerName, Balance: r.Balance, UpdatedAt: r.UpdatedAt,
		Linked: r.DiscordUserID != "", DiscordUserID: r.DiscordUserID, DisplayName: name, Avatar: r.Avatar}
}

// Me returns the acting Champion user's own account through their VERIFIED DayZ link.
// A user with no verified link gets ErrIdentityRequired - never an account made from a name.
func (a *Accounts) Me(ctx context.Context, scope repository.EconomyScope, discordUserID string) (Account, error) {
	player, ok, err := a.store.VerifiedPlayerID(ctx, scope.GuildID, discordUserID)
	if err != nil {
		return Account{}, err
	}
	if !ok {
		return Account{}, ErrIdentityRequired
	}
	return a.Account(ctx, scope, player)
}

// Account returns an account of the installation's guild (ErrAccountNotFound for a foreign id).
func (a *Accounts) Account(ctx context.Context, scope repository.EconomyScope, accountID int64) (Account, error) {
	row, found, err := a.store.Account(ctx, scope.GuildID, accountID)
	if err != nil {
		return Account{}, err
	}
	if !found {
		return Account{}, ErrAccountNotFound
	}
	return toAccount(row), nil
}

// Search is the admin lookup: players of the guild by gamertag or verified Discord name.
func (a *Accounts) Search(ctx context.Context, scope repository.EconomyScope, q string, limit int) ([]Account, error) {
	q = strings.Join(strings.Fields(q), " ")
	if n := utf8.RuneCountInString(q); n < MinSearchLen || n > MaxSearchLen || !utf8.ValidString(q) {
		return nil, ErrInvalidQuery
	}
	rows, err := a.store.SearchAccounts(ctx, scope.GuildID, q, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAccount(r))
	}
	return out, nil
}

// Transaction is one ledger line. Amount is a positive magnitude; Direction says which way.
// Description is generated from the type (never free text). The admin-only fields are empty
// in a player view.
type Transaction struct {
	ID            int64
	Type          string
	Direction     string
	Amount        int64
	BalanceAfter  int64
	Description   string
	ReferenceType string
	CreatedAt     time.Time
	// Admin view only:
	Reason      string // the reason the admin gave (or the stored note)
	ActorID     string // Discord user id of the admin, or "SYSTEM"
	ReferenceID string
	ServerID    int64
}

// TransactionPage is one page of history, newest first. NextCursor is "" on the last page.
type TransactionPage struct {
	Items      []Transaction
	NextCursor string
	Limit      int
}

const cursorPrefix = "ec1:"

func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(id, 10)))
}

// DecodeCursor parses a cursor produced by an earlier page.
func DecodeCursor(s string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), cursorPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(string(raw), cursorPrefix), 10, 64)
	return id, err == nil && id > 0
}

// ParseTypeFilter validates a history type filter ("" = all).
func ParseTypeFilter(raw string) (repository.TransactionFilter, error) {
	switch t := strings.ToUpper(strings.TrimSpace(raw)); t {
	case "":
		return repository.TransactionFilter{}, nil
	case TypeBountyClaim, TypeAdminCredit, TypeAdminDebit, TypeSystemReward, TypeShopPurchase, TypeShopRefund:
		return repository.TransactionFilter{Types: []string{t}}, nil
	case FilterEventPrize:
		return repository.TransactionFilter{TypePrefix: "EVENT_"}, nil
	}
	return repository.TransactionFilter{}, ErrInvalidFilter
}

// Transactions returns an account's history, newest first, keyset-paged (default 25, max
// 100). admin adds the reason, actor and reference; a player view never carries them.
func (a *Accounts) Transactions(ctx context.Context, scope repository.EconomyScope, accountID int64, limit int, cursor string, filter repository.TransactionFilter, admin bool) (TransactionPage, error) {
	if limit <= 0 {
		limit = DefaultTransactionLimit
	}
	if limit > MaxTransactionLimit {
		limit = MaxTransactionLimit
	}
	var before int64
	if cursor != "" {
		id, ok := DecodeCursor(cursor)
		if !ok {
			return TransactionPage{}, ErrInvalidCursor
		}
		before = id
	}
	// The account must belong to the installation's guild before its ledger is read.
	if _, err := a.Account(ctx, scope, accountID); err != nil {
		return TransactionPage{}, err
	}
	entries, next, err := a.store.Transactions(ctx, scope.GuildID, accountID, limit, before, filter)
	if err != nil {
		return TransactionPage{}, err
	}
	page := TransactionPage{Items: make([]Transaction, 0, len(entries)), Limit: limit}
	for _, e := range entries {
		page.Items = append(page.Items, toTransaction(e, admin))
	}
	if next != 0 {
		page.NextCursor = encodeCursor(next)
	}
	return page, nil
}

func toTransaction(e repository.LedgerEntry, admin bool) Transaction {
	t := Transaction{ID: e.ID, Type: e.Type, Direction: DirectionCredit, Amount: e.Amount, BalanceAfter: e.BalanceAfter,
		Description: TypeLabel(e.Type), ReferenceType: referenceType(e.Type), CreatedAt: e.CreatedAt}
	if e.Amount < 0 {
		t.Direction, t.Amount = DirectionDebit, -e.Amount
	}
	if admin {
		t.Reason, t.ActorID, t.ReferenceID, t.ServerID = e.Description, e.CreatedBy, e.ReferenceID, e.ServerID
	}
	return t
}

func referenceType(t string) string {
	switch {
	case t == TypeBountyClaim:
		return "BOUNTY"
	case t == TypeShopPurchase || t == TypeShopRefund:
		return "SHOP"
	case strings.HasPrefix(t, "EVENT_"):
		return "EVENT"
	}
	return ""
}

// AdjustRequest is one admin grant or debit through the web.
type AdjustRequest struct {
	Scope          repository.EconomyScope
	ActorDiscordID string
	AccountID      int64
	Amount         int64
	Reason         string
	IdempotencyKey string // optional: the same key replays the first result instead of applying again
}

// Adjustment is a committed (or replayed) admin adjustment.
type Adjustment struct {
	Transaction Transaction
	Balance     int64
	Duplicate   bool
	AccountID   int64
}

var idemKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,64}$`)

// Grant credits the account (ADMIN_CREDIT: spendable balance only, never leaderboard score).
func (a *Accounts) Grant(ctx context.Context, r AdjustRequest) (Adjustment, error) {
	return a.adjust(ctx, r, false)
}

// Debit removes points from the account (ADMIN_DEBIT); never below zero (ErrInsufficientFunds).
func (a *Accounts) Debit(ctx context.Context, r AdjustRequest) (Adjustment, error) {
	return a.adjust(ctx, r, true)
}

func (a *Accounts) adjust(ctx context.Context, r AdjustRequest, debit bool) (Adjustment, error) {
	if r.Amount <= 0 || r.Amount > MaxAdminAmount {
		return Adjustment{}, ErrInvalidAmount
	}
	reason := sanitizeText(r.Reason, MaxReasonLen)
	if reason == "" {
		return Adjustment{}, ErrReasonRequired
	}
	ref := ""
	if k := strings.TrimSpace(r.IdempotencyKey); k != "" {
		if !idemKey.MatchString(k) {
			return Adjustment{}, ErrInvalidKey
		}
		ref = "admin:" + k
	}
	if r.Scope.Status == "SUSPENDED" {
		return Adjustment{}, ErrSuspended
	}
	if r.Scope.ServerID == 0 {
		return Adjustment{}, ErrNoServer
	}
	if _, err := a.Account(ctx, r.Scope, r.AccountID); err != nil {
		return Adjustment{}, err
	}
	req := Request{GuildID: r.Scope.GuildID, ServerID: r.Scope.ServerID, OrganizationID: r.Scope.OrganizationID, PlayerID: r.AccountID,
		Amount: r.Amount, ReferenceID: ref, Description: reason, Actor: r.ActorDiscordID}
	var res Result
	var err error
	typ := TypeAdminCredit
	if debit {
		typ = TypeAdminDebit
		res, err = a.svc.AdminDebit(ctx, req)
	} else {
		res, err = a.svc.AdminCredit(ctx, req)
	}
	if err != nil {
		return Adjustment{}, err
	}
	if res.Duplicate && abs64(res.Amount) != r.Amount {
		return Adjustment{}, ErrIdempotencyMismatch
	}
	entry := repository.LedgerEntry{ID: res.TransactionID, Type: typ, Amount: res.Amount, BalanceAfter: res.Balance, Description: reason, CreatedBy: r.ActorDiscordID, ReferenceID: ref, ServerID: r.Scope.ServerID, CreatedAt: res.CreatedAt}
	return Adjustment{Transaction: toTransaction(entry, true), Balance: res.Balance, Duplicate: res.Duplicate, AccountID: r.AccountID}, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Mismatch is one reconciliation finding.
type Mismatch = repository.BalanceMismatch

// Reconcile checks, read-only, that every account of the installation's guild equals its
// ledger (docs/ECONOMY.md "Reconciliation"). An empty result means consistent; nothing is repaired.
func (a *Accounts) Reconcile(ctx context.Context, scope repository.EconomyScope, limit int) ([]Mismatch, error) {
	out, err := a.store.Reconcile(ctx, scope.GuildID, limit)
	if err != nil {
		return nil, fmt.Errorf("reconcile guild %d: %w", scope.GuildID, err)
	}
	return out, nil
}
