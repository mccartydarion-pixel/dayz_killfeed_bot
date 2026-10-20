// Package perkstore holds the rules of the perk store (docs/PERK_STORE.md): offers a server owner
// sells to players for Champion Points. An offer grants perks that do not affect gameplay: a
// supporter tier (badge and Discord role), a place on the server's priority queue, and a custom
// perk staff hand out themselves. Persistence, Discord and Nitrado live elsewhere; this package is
// pure.
package perkstore

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// BillingOneTime is paid once and lasts DurationDays (0 = for good).
	BillingOneTime = "ONE_TIME"
	// BillingMonthly is charged again every MonthlyDays until the player cancels or cannot pay.
	BillingMonthly = "MONTHLY"

	MonthlyDays     = 30
	MaxDays         = 3650
	MaxPrice        = 10_000_000
	MaxStock        = 100_000
	MaxOffers       = 50
	MaxNameRunes    = 60
	MaxDescRunes    = 300
	MaxCustomRunes  = 200
	MaxSortOrder    = 1000
	MaxPerkAttempts = 5
)

// Why an offer cannot be bought right now.
const (
	UnavailableDisabled   = "DISABLED"
	UnavailableNotStarted = "NOT_STARTED"
	UnavailableEnded      = "ENDED"
	UnavailableSoldOut    = "SOLD_OUT"
)

// Offer is one thing a player can buy.
type Offer struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PricePoints int64  `json:"pricePoints"`
	Billing     string `json:"billing"`
	// DurationDays is how long a ONE_TIME purchase lasts; 0 = for good. MONTHLY is always MonthlyDays.
	DurationDays int `json:"durationDays"`

	VIPTierID     *int64 `json:"vipTierId"`
	VIPTierName   string `json:"vipTierName,omitempty"`
	PriorityQueue bool   `json:"priorityQueue"`
	CustomPerk    string `json:"customPerk"`

	Giftable       bool       `json:"giftable"`
	AvailableFrom  *time.Time `json:"availableFrom"`
	AvailableUntil *time.Time `json:"availableUntil"`
	StockLimit     *int       `json:"stockLimit"`
	SoldCount      int        `json:"soldCount"`
	Enabled        bool       `json:"enabled"`
	SortOrder      int        `json:"sortOrder"`
}

func clean(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "@", "").Replace(s))
}

// Normalize validates an offer as an owner submitted it.
func Normalize(o Offer) (Offer, error) {
	o.Name = clean(o.Name)
	o.Description = strings.TrimSpace(strings.ReplaceAll(o.Description, "@", ""))
	o.CustomPerk = clean(o.CustomPerk)
	o.Billing = strings.ToUpper(strings.TrimSpace(o.Billing))
	if o.Name == "" || utf8.RuneCountInString(o.Name) > MaxNameRunes {
		return o, fmt.Errorf("name the offer (up to %d characters)", MaxNameRunes)
	}
	if utf8.RuneCountInString(o.Description) > MaxDescRunes {
		return o, fmt.Errorf("keep the description under %d characters", MaxDescRunes)
	}
	if o.PricePoints <= 0 || o.PricePoints > MaxPrice {
		return o, fmt.Errorf("price is 1 to %d Champion Points", MaxPrice)
	}
	switch o.Billing {
	case BillingMonthly:
		o.DurationDays = MonthlyDays
	case BillingOneTime:
		if o.DurationDays < 0 || o.DurationDays > MaxDays {
			return o, fmt.Errorf("length is 0 (for good) to %d days", MaxDays)
		}
	default:
		return o, fmt.Errorf("billing is one-time or monthly")
	}
	if o.VIPTierID != nil && *o.VIPTierID <= 0 {
		o.VIPTierID = nil
	}
	if utf8.RuneCountInString(o.CustomPerk) > MaxCustomRunes {
		return o, fmt.Errorf("keep the custom perk under %d characters", MaxCustomRunes)
	}
	if o.VIPTierID == nil && !o.PriorityQueue && o.CustomPerk == "" {
		return o, fmt.Errorf("pick at least one perk: a supporter tier, priority queue or a custom perk")
	}
	if o.StockLimit != nil && (*o.StockLimit <= 0 || *o.StockLimit > MaxStock) {
		return o, fmt.Errorf("stock is 1 to %d, or empty for no limit", MaxStock)
	}
	if o.AvailableFrom != nil && o.AvailableUntil != nil && !o.AvailableUntil.After(*o.AvailableFrom) {
		return o, fmt.Errorf("the offer must end after it starts")
	}
	if o.SortOrder < 0 || o.SortOrder > MaxSortOrder {
		return o, fmt.Errorf("sort order is between 0 and %d", MaxSortOrder)
	}
	return o, nil
}

// Unavailable says why the offer cannot be bought at now; "" means it can.
func (o Offer) Unavailable(now time.Time) string {
	switch {
	case !o.Enabled:
		return UnavailableDisabled
	case o.AvailableFrom != nil && now.Before(*o.AvailableFrom):
		return UnavailableNotStarted
	case o.AvailableUntil != nil && !now.Before(*o.AvailableUntil):
		return UnavailableEnded
	case o.StockLimit != nil && o.SoldCount >= *o.StockLimit:
		return UnavailableSoldOut
	}
	return ""
}

// Remaining is how many are left of a limited offer; nil when it is unlimited.
func (o Offer) Remaining() *int {
	if o.StockLimit == nil {
		return nil
	}
	left := *o.StockLimit - o.SoldCount
	if left < 0 {
		left = 0
	}
	return &left
}

// Expiry is when a purchase made at start runs out; nil means it never does.
func Expiry(billing string, durationDays int, start time.Time) *time.Time {
	days := durationDays
	if billing == BillingMonthly {
		days = MonthlyDays
	}
	if days <= 0 {
		return nil
	}
	at := start.AddDate(0, 0, days)
	return &at
}

// NextRenewal is when a monthly purchase that ran out at expiresAt is paid up to after a renewal
// charged at now. A renewal that comes late (the worker was down) starts from now, so the player
// is never charged for days that had already passed.
func NextRenewal(expiresAt, now time.Time) time.Time {
	from := expiresAt
	if now.After(from) {
		from = now
	}
	return from.AddDate(0, 0, MonthlyDays)
}

// UnavailableMessage is the player-facing wording of an Unavailable reason.
func UnavailableMessage(reason string) string {
	switch reason {
	case UnavailableNotStarted:
		return "this offer has not started yet"
	case UnavailableEnded:
		return "this offer has ended"
	case UnavailableSoldOut:
		return "this offer is sold out"
	}
	return "this offer is not available right now"
}
