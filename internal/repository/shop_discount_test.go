package repository

import (
	"testing"
	"time"
)

func TestDiscountedPrice(t *testing.T) {
	for _, c := range []struct {
		price   int64
		percent int
		want    int64
	}{
		{1800, 25, 1350},
		{999, 10, 900}, // 99.9 off rounds down to 99
		{1, 75, 1},     // never free
		{3, 50, 2},
		{500, 0, 500},
		{1000, 90, 250}, // capped at 75%
		{9_000_000_000_000_000, 75, 2_250_000_000_000_000},
	} {
		if got := DiscountedPrice(c.price, c.percent); got != c.want {
			t.Errorf("%d at %d%%: %d, want %d", c.price, c.percent, got, c.want)
		}
	}
}

func TestBestDiscount(t *testing.T) {
	now := time.Now()
	weapons, tools := int64(2), int64(3)
	m4 := int64(9)
	ended := now.Add(-time.Minute)
	running := []ShopDiscount{
		{ID: 1, Kind: DiscountSale, PercentOff: 10, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)},                                      // everything
		{ID: 2, Kind: DiscountSale, CategoryID: &weapons, PercentOff: 20, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)},                // weapons
		{ID: 3, Kind: DiscountDeal, ProductID: &m4, PercentOff: 15, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)},                      // the M4
		{ID: 4, Kind: DiscountDeal, ProductID: &m4, PercentOff: 60, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(-time.Second)},                   // expired
		{ID: 5, Kind: DiscountSale, CategoryID: &tools, PercentOff: 50, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), EndedAt: &ended}, // ended early
		{ID: 6, Kind: DiscountSale, CategoryID: &weapons, PercentOff: 20, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(2 * time.Hour)},
	}
	if d := BestDiscount(running, m4, &weapons, now); d == nil || d.ID != 2 {
		t.Fatalf("the M4 in weapons gets the bigger weapons sale, the older on a tie: %+v", d)
	}
	if d := BestDiscount(running, 10, &tools, now); d == nil || d.ID != 1 {
		t.Fatalf("an ended tools sale doesn't count: %+v", d)
	}
	if d := BestDiscount(running, 11, nil, now); d == nil || d.ID != 1 {
		t.Fatalf("an item without a category gets the sale on everything: %+v", d)
	}
	if d := BestDiscount(running[2:5], 11, nil, now); d != nil {
		t.Fatalf("nothing covers it: %+v", d)
	}
}
