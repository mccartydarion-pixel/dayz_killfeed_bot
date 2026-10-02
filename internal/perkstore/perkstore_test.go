package perkstore

import (
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestNormalize(t *testing.T) {
	got, err := Normalize(Offer{Name: "  Gold @Supporter\n", Description: " Thanks! ", PricePoints: 500, Billing: "monthly", DurationDays: 7, VIPTierID: ptr(int64(3)), CustomPerk: " Custom @tag "})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Gold Supporter" || got.Description != "Thanks!" || got.Billing != BillingMonthly || got.DurationDays != MonthlyDays || got.CustomPerk != "Custom tag" {
		t.Fatalf("%+v", got)
	}
	if o, err := Normalize(Offer{Name: "Queue", PricePoints: 1, Billing: BillingOneTime, PriorityQueue: true, VIPTierID: ptr(int64(0))}); err != nil || o.VIPTierID != nil || o.DurationDays != 0 {
		t.Fatalf("a zero tier id means no tier, and 0 days means for good: %+v %v", o, err)
	}
}

func TestNormalizeRejects(t *testing.T) {
	base := Offer{Name: "Queue", PricePoints: 100, Billing: BillingOneTime, PriorityQueue: true}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for name, change := range map[string]func(*Offer){
		"no name":        func(o *Offer) { o.Name = " " },
		"long name":      func(o *Offer) { o.Name = strings.Repeat("a", MaxNameRunes+1) },
		"long desc":      func(o *Offer) { o.Description = strings.Repeat("a", MaxDescRunes+1) },
		"free":           func(o *Offer) { o.PricePoints = 0 },
		"too expensive":  func(o *Offer) { o.PricePoints = MaxPrice + 1 },
		"bad billing":    func(o *Offer) { o.Billing = "WEEKLY" },
		"negative days":  func(o *Offer) { o.DurationDays = -1 },
		"too many days":  func(o *Offer) { o.DurationDays = MaxDays + 1 },
		"no perk":        func(o *Offer) { o.PriorityQueue = false },
		"long custom":    func(o *Offer) { o.CustomPerk = strings.Repeat("a", MaxCustomRunes+1) },
		"zero stock":     func(o *Offer) { o.StockLimit = ptr(0) },
		"huge stock":     func(o *Offer) { o.StockLimit = ptr(MaxStock + 1) },
		"ends at start":  func(o *Offer) { o.AvailableFrom, o.AvailableUntil = &start, &start },
		"bad sort order": func(o *Offer) { o.SortOrder = MaxSortOrder + 1 },
	} {
		o := base
		change(&o)
		if _, err := Normalize(o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUnavailableAndRemaining(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	before, after := now.Add(-time.Hour), now.Add(time.Hour)
	o := Offer{Enabled: true}
	if o.Unavailable(now) != "" || o.Remaining() != nil {
		t.Fatal("an enabled, unlimited offer is available")
	}
	for want, offer := range map[string]Offer{
		UnavailableDisabled:   {},
		UnavailableNotStarted: {Enabled: true, AvailableFrom: &after},
		UnavailableEnded:      {Enabled: true, AvailableUntil: &before},
		UnavailableSoldOut:    {Enabled: true, StockLimit: ptr(2), SoldCount: 2},
	} {
		if got := offer.Unavailable(now); got != want {
			t.Errorf("want %s, got %q", want, got)
		}
		if UnavailableMessage(want) == "" {
			t.Errorf("%s has no message", want)
		}
	}
	// The end is exclusive, the start inclusive.
	if (Offer{Enabled: true, AvailableUntil: &now}).Unavailable(now) != UnavailableEnded || (Offer{Enabled: true, AvailableFrom: &now}).Unavailable(now) != "" {
		t.Fatal("window edges")
	}
	if left := (Offer{StockLimit: ptr(5), SoldCount: 3}).Remaining(); left == nil || *left != 2 {
		t.Fatalf("remaining: %v", left)
	}
	if left := (Offer{StockLimit: ptr(1), SoldCount: 4}).Remaining(); left == nil || *left != 0 {
		t.Fatalf("remaining never goes below zero: %v", left)
	}
}

func TestExpiryAndRenewal(t *testing.T) {
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if Expiry(BillingOneTime, 0, start) != nil {
		t.Fatal("0 days never expires")
	}
	if at := Expiry(BillingOneTime, 7, start); at == nil || !at.Equal(start.AddDate(0, 0, 7)) {
		t.Fatalf("one-time expiry: %v", at)
	}
	if at := Expiry(BillingMonthly, 0, start); at == nil || !at.Equal(start.AddDate(0, 0, MonthlyDays)) {
		t.Fatalf("monthly expiry: %v", at)
	}
	// On time: the next period follows the last. Late: it starts from the charge.
	if got := NextRenewal(start, start); !got.Equal(start.AddDate(0, 0, MonthlyDays)) {
		t.Fatalf("on-time renewal: %v", got)
	}
	late := start.Add(72 * time.Hour)
	if got := NextRenewal(start, late); !got.Equal(late.AddDate(0, 0, MonthlyDays)) {
		t.Fatalf("late renewal: %v", got)
	}
}
