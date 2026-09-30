package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/missionwrite"
)

// loadAttempt reads one attempt's immutable plan facts and state from the attempt ledger over a
// READ-ONLY connection (default_transaction_read_only=on). It never writes: evidence and transitions
// go through the canary operator API.
func loadAttempt(ctx context.Context, org, inst int64, attemptID string) (*missionwrite.AttemptFacts, error) {
	dsn := os.Getenv("DATABASE_PUBLIC_URL")
	if dsn == "" {
		return nil, errors.New("DATABASE_PUBLIC_URL is required to read the attempt ledger")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("the database URL could not be parsed")
	}
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	defer pool.Close()
	a, err := repository.NewShopAttemptRepository(pool).Get(ctx, org, inst, attemptID)
	if err != nil {
		return nil, fmt.Errorf("attempt %s: %w", attemptID, err)
	}
	return &missionwrite.AttemptFacts{
		AttemptID: a.AttemptID, State: a.State, ClassName: a.ClassName, Quantity: a.Quantity,
		Pos: [3]float64{a.PosX, a.PosY, a.PosZ}, ArtifactPath: a.ArtifactPath,
	}, nil
}

func printAttempt(a *missionwrite.AttemptFacts) {
	if a == nil {
		return
	}
	staged, empty := missionwrite.AttemptFiles(*a)
	fmt.Println("attempt (read-only from the ledger):")
	fmt.Printf("  %s  state=%s  %s x%d at (%.2f, %.2f, %.2f)  artifact=%s\n", a.AttemptID, a.State, a.ClassName, a.Quantity, a.Pos[0], a.Pos[1], a.Pos[2], a.ArtifactPath)
	fmt.Printf("  staged file: %d bytes, SHA-256 %s\n", len(staged), missionwrite.SHA256(staged))
	fmt.Printf("  empty file:  %d bytes, SHA-256 %s\n", len(empty), missionwrite.SHA256(empty))
}

// printEvidence tells the operator exactly what to record through the canary operator API after a
// verified write (the API re-checks these digests against the same attempt facts).
func printEvidence(op missionwrite.Operation, a *missionwrite.AttemptFacts, o missionwrite.Outcome, boot string) {
	if a == nil || o.Status != missionwrite.StatusWrittenVerified {
		return
	}
	fmt.Println("\nrecord through the canary operator API (/shop/canary/attempts/" + a.AttemptID + "/evidence):")
	switch op {
	case missionwrite.OpStageItem:
		fmt.Printf("  kind=STAGED_FILE_HASH source=NITRADO_READBACK sha256=%s previousSha256=%s\n", o.After, o.Before)
		fmt.Printf("  kind=STAGING_BOOT     source=BOOT_AUTHORITY   bootFile=%s (the boot during which the file was staged)\n", boot)
		fmt.Println("  then advance FILE_PREPARED -> FILE_STAGED")
	case missionwrite.OpUnstageItem:
		fmt.Printf("  kind=UNSTAGED_FILE_HASH source=NITRADO_READBACK sha256=%s\n", o.After)
		fmt.Println("  then advance UNSTAGE_REQUIRED -> VERIFICATION_REQUIRED (or FILE_STAGED/AWAITING_RESTART -> UNSTAGED)")
	}
}

// bootFrom returns the boot the write happened in, from the verified "no restart" check.
func bootFrom(o missionwrite.Outcome) string {
	for _, c := range o.Checks {
		if c.Name == "no restart" && c.Result == "PASS" && strings.HasPrefix(c.Detail, "boot ") {
			return strings.TrimSuffix(strings.SplitN(strings.TrimPrefix(c.Detail, "boot "), ",", 2)[0], " ")
		}
	}
	return "<the current accepted boot (the restart check was not PASS: verify it first)>"
}
