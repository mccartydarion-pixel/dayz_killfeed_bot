//go:build integration

package repository

import (
	"context"
	"os"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func TestTailTrustRepositoryRoundTrip(t *testing.T) {
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
	ctx := context.Background()
	db, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewTailTrustRepository(db.Pool)
	svc := "it-tail-trust"
	defer db.Pool.Exec(ctx, `DELETE FROM nitrado_tail_trust WHERE service_id IN ($1, $2)`, svc, svc+"-old")
	if err := repo.SaveTailTrust(ctx, svc, nitrado.TailTrustState{Matches: 2}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTailTrust(ctx, svc, nitrado.TailTrustState{Matches: 3, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveTailTrust(ctx, svc+"-old", nitrado.TailTrustState{Matches: 3, Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE nitrado_tail_trust SET updated_at = NOW() - INTERVAL '8 days' WHERE service_id = $1`, svc+"-old"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LoadTailTrust(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st := got[svc]; !st.Trusted || st.Matches != 3 {
		t.Fatalf("saved trust: %+v", st)
	}
	if _, ok := got[svc+"-old"]; ok {
		t.Fatal("trust older than 7 days is not loaded")
	}
}
