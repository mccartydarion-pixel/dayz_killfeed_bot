//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
	"time"
)

// TestFeedCardJournalLifecycle covers migration 0057 and every journal
// operation the immediate feed uses on restart (release gate 5).
func TestFeedCardJournalLifecycle(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	r := NewFeedCardRepository(db.Pool)
	key := fmt.Sprintf("KILLFEED:it-%d", time.Now().UnixNano())
	other := key + "-other"
	now := time.Now().UTC().Truncate(time.Microsecond)

	cards := []FeedCard{
		{Nonce: "n1", Embed: []byte(`{"title":"kill-01"}`), DetectedAt: now.Add(-3 * time.Second), EnqueuedAt: now},
		{Nonce: "n2", Embed: []byte(`{"title":"kill-02"}`), EnqueuedAt: now}, // no detection time
		{Nonce: "n3", Embed: []byte(`{"title":"kill-03"}`), EnqueuedAt: now},
	}
	if err := r.Record(ctx, key, cards); err != nil {
		t.Fatal(err)
	}
	// Re-recording is a no-op; another feed's rows are independent.
	if err := r.Record(ctx, key, cards[:1]); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(ctx, other, cards[:1]); err != nil {
		t.Fatal(err)
	}
	open, err := r.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 3 || open[0].Nonce != "n1" || open[2].Nonce != "n3" || open[0].MessageID != "" {
		t.Fatalf("open after record: %+v", open)
	}
	var embed struct{ Title string }
	if err := json.Unmarshal(open[0].Embed, &embed); err != nil || embed.Title != "kill-01" ||
		!open[0].DetectedAt.Equal(cards[0].DetectedAt) || !open[1].DetectedAt.IsZero() {
		t.Fatalf("round trip: %+v", open[0])
	}

	if err := r.MarkPosted(ctx, key, "n1", "chan", "m1", now); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkPosted(ctx, key, "n2", "chan", "m2", now); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDropped(ctx, key, []string{"n3"}, "REJECTED", now); err != nil {
		t.Fatal(err)
	}
	open, _ = r.Open(ctx, key)
	if len(open) != 2 || open[0].MessageID != "m1" || open[0].ChannelID != "chan" || !open[0].PostedAt.Equal(now) {
		t.Fatalf("open after post/drop: %+v", open)
	}
	if err := r.MarkRemoved(ctx, key, []string{"m1"}, now); err != nil {
		t.Fatal(err)
	}
	// A removed card cannot be re-marked dropped.
	if err := r.MarkDropped(ctx, key, []string{"n1"}, "BACKLOG_OVERFLOW", now); err != nil {
		t.Fatal(err)
	}
	var reason *string
	if err := db.Pool.QueryRow(ctx, `SELECT drop_reason FROM discord_feed_cards WHERE feed_key=$1 AND nonce='n1'`, key).Scan(&reason); err != nil || reason != nil {
		t.Fatalf("removed card was re-marked: %v %v", reason, err)
	}
	open, _ = r.Open(ctx, key)
	if len(open) != 1 || open[0].Nonce != "n2" {
		t.Fatalf("open after remove: %+v", open)
	}
	if o, _ := r.Open(ctx, other); len(o) != 1 {
		t.Fatalf("another feed's rows changed: %+v", o)
	}

	// Purge removes only closed rows older than the cutoff.
	if _, err := r.Purge(ctx, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM discord_feed_cards WHERE feed_key=$1`, key).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Fatalf("purge left %d rows, want only the open one", left)
	}
	_, _ = db.Pool.Exec(ctx, `DELETE FROM discord_feed_cards WHERE feed_key IN ($1, $2)`, key, other)
}

// TestFeedCardJournalWriteCost measures what the journal adds to each
// immediate-mode card (one Record + one MarkPosted on the feed goroutine).
// Logged for the staging report; the bound only catches a pathological
// regression (e.g. a missing index).
func TestFeedCardJournalWriteCost(t *testing.T) {
	db := saasIntegrationDB(t)
	ctx := context.Background()
	r := NewFeedCardRepository(db.Pool)
	key := fmt.Sprintf("KILLFEED:cost-%d", time.Now().UnixNano())
	defer db.Pool.Exec(ctx, `DELETE FROM discord_feed_cards WHERE feed_key=$1`, key)
	const n = 300
	costs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		nonce := fmt.Sprintf("c%04d", i)
		start := time.Now()
		if err := r.Record(ctx, key, []FeedCard{{Nonce: nonce, Embed: []byte(`{"title":"kill","description":"x","fields":[{"name":"Weapon","value":"M4-A1"}]}`), EnqueuedAt: start}}); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkPosted(ctx, key, nonce, "chan", fmt.Sprintf("m%d", i), time.Now()); err != nil {
			t.Fatal(err)
		}
		costs = append(costs, time.Since(start))
	}
	sort.Slice(costs, func(i, j int) bool { return costs[i] < costs[j] })
	p50, p95, p99 := costs[n/2], costs[n*95/100], costs[n*99/100]
	t.Logf("journal cost per card (Record+MarkPosted, local Postgres 16): p50=%s p95=%s p99=%s max=%s", p50, p95, p99, costs[n-1])
	if p95 > 250*time.Millisecond {
		t.Fatalf("journal p95 %s", p95)
	}
}
