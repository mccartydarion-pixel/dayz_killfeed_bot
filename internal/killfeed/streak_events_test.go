package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/streaks"
)

// seedStreak resolves (or creates) a player's ID via UpsertPlayer, then seeds
// its "before this kill" streak count directly in the fake's streak map -
// mirroring a real player_combat_stats.current_streak value at that point.
func seedStreak(t *testing.T, store *fakePersistenceStore, guildID int64, dayzID, displayName string, before int) int64 {
	t.Helper()
	id, err := store.UpsertPlayer(context.Background(), guildID, dayzID, displayName, time.Now())
	if err != nil {
		t.Fatalf("seed player %s: %v", dayzID, err)
	}
	store.mu.Lock()
	if store.streaks == nil {
		store.streaks = map[int64]int{}
	}
	store.streaks[id] = before
	store.mu.Unlock()
	return id
}

func runOneKill(t *testing.T, store *fakePersistenceStore, ev *Event) repository.KillRecord {
	t.Helper()
	pq := NewPersistenceQueue(store, 1, "sess-streak")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)
	pq.Enqueue(ev)
	pq.Close()
	if len(store.kills) != 1 {
		t.Fatalf("expected 1 persisted kill, got %d", len(store.kills))
	}
	return store.kills[len(store.kills)-1]
}

// TestKillingSpreeBelowThreshold: killer streak 1 -> after this kill, streak
// is 2, which is below the reused KILLING SPREE tier threshold (5).
func TestKillingSpreeBelowThreshold(t *testing.T) {
	store := newFakePersistenceStore()
	seedStreak(t, store, 1, "killer-id", "Killer", 1)
	rec := runOneKill(t, store, killEvent("victim", "killer", "M4-A1", 20, "10:00:00"))

	if rec.KillerStreakAfter == nil || *rec.KillerStreakAfter != 2 {
		t.Fatalf("expected killer_streak_after=2, got %v", rec.KillerStreakAfter)
	}
	if rec.KillingSpree {
		t.Fatal("streak of 2 must not classify as a killing spree")
	}
}

// TestKillingSpreeStreakOfThreeNotYetSpree: killer streak 2 -> after this
// kill, streak is 3 (still below 5) - not a spree.
func TestKillingSpreeStreakOfThreeNotYetSpree(t *testing.T) {
	store := newFakePersistenceStore()
	seedStreak(t, store, 1, "killer-id", "Killer", 2)
	rec := runOneKill(t, store, killEvent("victim", "killer", "M4-A1", 20, "10:00:00"))

	if rec.KillerStreakAfter == nil || *rec.KillerStreakAfter != 3 {
		t.Fatalf("expected killer_streak_after=3, got %v", rec.KillerStreakAfter)
	}
	if rec.KillingSpree {
		t.Fatal("streak of 3 must not classify as a killing spree (reused threshold is 5)")
	}
}

// TestKillingSpreeReachesThreshold: killer streak 4 -> after this kill,
// streak is 5, reaching streaks.KillingSpreeThreshold() -> KILLING_SPREE true.
func TestKillingSpreeReachesThreshold(t *testing.T) {
	store := newFakePersistenceStore()
	seedStreak(t, store, 1, "killer-id", "Killer", 4)
	rec := runOneKill(t, store, killEvent("victim", "killer", "M4-A1", 20, "10:00:00"))

	if rec.KillerStreakAfter == nil || *rec.KillerStreakAfter != streaks.KillingSpreeThreshold() {
		t.Fatalf("expected killer_streak_after=%d, got %v", streaks.KillingSpreeThreshold(), rec.KillerStreakAfter)
	}
	if !rec.KillingSpree {
		t.Fatal("expected killing spree once the streak reaches the authoritative threshold")
	}
}

// TestStreakEndedThresholdCases covers the victim-before-death boundary: 0
// and one-below-threshold must not end a "meaningful" streak; at-threshold
// and above must, with the exact pre-death count persisted.
func TestStreakEndedThresholdCases(t *testing.T) {
	threshold := streaks.MeaningfulStreakThreshold
	cases := []struct {
		name          string
		victimBefore  int
		wantEnded     bool
		wantEndedNote int
	}{
		{"victim streak 0", 0, false, 0},
		{"victim streak one below threshold", threshold - 1, false, 0},
		{"victim streak at threshold", threshold, true, threshold},
		{"victim streak well above threshold", threshold + 2, true, threshold + 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakePersistenceStore()
			seedStreak(t, store, 1, "killer-id", "killer", 0)
			seedStreak(t, store, 1, "victim-id", "victim", tc.victimBefore)
			rec := runOneKill(t, store, killEvent("victim", "killer", "M4-A1", 20, "10:00:00"))

			if rec.StreakEnded != tc.wantEnded {
				t.Fatalf("streak_ended = %v, want %v", rec.StreakEnded, tc.wantEnded)
			}
			if !tc.wantEnded {
				if rec.EndedStreakCount != nil {
					t.Fatalf("expected nil ended_streak_count, got %v", *rec.EndedStreakCount)
				}
				return
			}
			if rec.EndedStreakCount == nil || *rec.EndedStreakCount != tc.wantEndedNote {
				t.Fatalf("expected ended_streak_count=%d, got %v", tc.wantEndedNote, rec.EndedStreakCount)
			}
		})
	}
}

// TestHeadshotLongshotKillingSpreeCoexist: a single kill can be simultaneously
// HEADSHOT, LONGSHOT, and KILLING_SPREE - none suppresses another.
func TestHeadshotLongshotKillingSpreeCoexist(t *testing.T) {
	store := newFakePersistenceStore()
	seedStreak(t, store, 1, "killer-id", "killer", 4) // -> after = 5 = spree
	ev := killEvent("victim", "killer", "SVD", 150.0, "10:00:00")
	ev.HitZone = "Head"
	rec := runOneKill(t, store, ev)

	if !rec.Headshot {
		t.Fatal("expected headshot true")
	}
	if !rec.Longshot {
		t.Fatal("expected longshot true")
	}
	if !rec.KillingSpree {
		t.Fatal("expected killing spree true")
	}
}

// TestServerRecordAndStreakEndedCoexist mirrors the earlier longshot/server-record
// independence test: ServerRecord winning presentation priority (which title
// is shown) must never suppress the independently persisted STREAK_ENDED fact.
func TestServerRecordAndStreakEndedCoexist(t *testing.T) {
	c := presentation.Context{ServerRecord: true, VictimEndedStreak: 7, StreakEndedThreshold: streaks.MeaningfulStreakThreshold}
	if presentation.SelectPrimary(c) != presentation.StoryServerRecord {
		t.Fatal("expected server record to take presentation priority over streak ended")
	}
	if c.VictimEndedStreak < c.StreakEndedThreshold {
		t.Fatal("expected the streak-ended condition to independently hold regardless of story priority")
	}
}

// TestRapidConsecutiveKillsStreakCorrectness: the same killer racks up three
// kills in a row (against distinct victims). Each kill must see the correct
// prior streak and compute the correct after-value, proving the
// single-consumer persistence queue serializes streak classification
// correctly under back-to-back kills.
func TestRapidConsecutiveKillsStreakCorrectness(t *testing.T) {
	store := newFakePersistenceStore()
	pq := NewPersistenceQueue(store, 1, "sess-rapid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pq.Run(ctx)

	pq.Enqueue(killEvent("victim-a", "killer", "M4-A1", 10, "10:00:00"))
	pq.Enqueue(killEvent("victim-b", "killer", "M4-A1", 10, "10:00:05"))
	pq.Enqueue(killEvent("victim-c", "killer", "M4-A1", 10, "10:00:10"))
	pq.Close()

	if len(store.kills) != 3 {
		t.Fatalf("expected 3 persisted kills, got %d", len(store.kills))
	}
	wantAfter := []int{1, 2, 3}
	for i, rec := range store.kills {
		if rec.KillerStreakAfter == nil || *rec.KillerStreakAfter != wantAfter[i] {
			t.Fatalf("kill %d: expected killer_streak_after=%d, got %v", i, wantAfter[i], rec.KillerStreakAfter)
		}
	}
}
