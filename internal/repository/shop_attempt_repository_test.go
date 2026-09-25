package repository

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMapAttemptErr(t *testing.T) {
	for _, c := range []struct {
		err  error
		want error
	}{
		{&pgconn.PgError{Code: "SA409"}, ErrShopDeliveryAttemptActive},
		{&pgconn.PgError{Code: "SA410"}, ErrShopAttemptConflict},
		{&pgconn.PgError{Code: "SA411"}, ErrShopAttemptDeliveryClosed},
		{&pgconn.PgError{Code: "SA412"}, ErrShopAttemptSequence},
		{&pgconn.PgError{Code: "SA422", Message: "x"}, ErrShopAttemptRejected},
		{&pgconn.PgError{Code: "23514", ConstraintName: "shop_delivery_attempts_staged"}, ErrShopAttemptEvidence},
		{&pgconn.PgError{Code: "23505", ConstraintName: "uq_shop_delivery_attempts_open"}, ErrShopAttemptConflict},
		{&pgconn.PgError{Code: "23505", ConstraintName: "shop_delivery_attempts_attempt_id_key"}, ErrShopAttemptConflict},
		{&pgconn.PgError{Code: "23503", TableName: "shop_delivery_attempts"}, ErrShopAttemptDeliveryClosed},
		{fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "SA409"}), ErrShopDeliveryAttemptActive},
	} {
		if got := mapAttemptErr(c.err); !errors.Is(got, c.want) {
			t.Errorf("%v -> %v, want %v", c.err, got, c.want)
		}
	}
	// Unrelated errors pass through unchanged.
	other := &pgconn.PgError{Code: "23514", ConstraintName: "shop_purchases_total_points_check"}
	if got := mapAttemptErr(other); got != other {
		t.Fatalf("unrelated constraint mapped: %v", got)
	}
	if got := mapAttemptErr(nil); got != nil {
		t.Fatal("nil")
	}
}

// Every state in the transition map is a known state, and terminal states have no outgoing edge.
func TestShopAttemptTransitionsShape(t *testing.T) {
	known := map[string]bool{AttemptPlanCreated: true, AttemptFilePrepared: true, AttemptFileStaged: true, AttemptAwaitingRestart: true, AttemptRestartObserved: true,
		AttemptUnstageRequired: true, AttemptVerificationRequired: true, AttemptFulfilled: true, AttemptAbandoned: true, AttemptUnstaged: true, AttemptFailedReview: true}
	for from, tos := range ShopAttemptTransitions {
		if !known[from] {
			t.Fatalf("unknown state %s", from)
		}
		for _, to := range tos {
			if !known[to] {
				t.Fatalf("unknown state %s", to)
			}
		}
	}
	for _, term := range []string{AttemptFulfilled, AttemptAbandoned, AttemptUnstaged, AttemptFailedReview} {
		if len(ShopAttemptTransitions[term]) != 0 {
			t.Fatalf("terminal %s has transitions", term)
		}
	}
	// RESTART_OBSERVED never reaches verification without the unstage step.
	for _, to := range ShopAttemptTransitions[AttemptRestartObserved] {
		if to == AttemptVerificationRequired {
			t.Fatal("RESTART_OBSERVED -> VERIFICATION_REQUIRED skips UNSTAGE_REQUIRED")
		}
	}
}
