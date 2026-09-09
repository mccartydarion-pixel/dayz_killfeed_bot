//go:build integration

package repository

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

func TestAnnouncementClaimRace(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("REQUIRE_INTEGRATION_DB") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for integration suite")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	t.Log("PostgreSQL integration tests: RUNNING")
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAnnouncementRepository(db.Pool)
	kind := "integration-race"
	id := time.Now().UnixNano()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, claimErr := repo.Claim(ctx, kind, id, now, time.Minute)
			if claimErr != nil {
				t.Errorf("claim: %v", claimErr)
			}
			results <- ok
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for ok := range results {
		if ok {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected one claimant, got %d", wins)
	}
	if err := repo.MarkAnnounced(ctx, kind, id, now); err != nil {
		t.Fatal(err)
	}
	announced, err := repo.IsAnnounced(ctx, kind, id)
	if err != nil || !announced {
		t.Fatalf("announcement not durable: %v %v", announced, err)
	}
	_, _ = db.Pool.Exec(ctx, `DELETE FROM completion_announcements WHERE kind=$1 AND object_id=$2`, kind, id)
}
