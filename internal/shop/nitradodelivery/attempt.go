package nitradodelivery

import (
	"errors"
	"sync"
)

// Delivery attempt states (docs/SHOP_DELIVERY_PHASE2B.md "Duplicate prevention"). They are NOT active
// in production: shop_deliveries still only allows MANUAL_READY / FULFILLED / CANCELLED, and this
// prototype keeps attempts in memory. An uploaded file is not proof of delivery; a restart is not
// proof of delivery - only VERIFICATION_REQUIRED -> FULFILLED (a human or verified evidence) is.
const (
	AttemptPlanCreated          = "PLAN_CREATED"
	AttemptFilePrepared         = "FILE_PREPARED"
	AttemptFileStaged           = "FILE_STAGED"
	AttemptAwaitingRestart      = "AWAITING_RESTART"
	AttemptRestartObserved      = "RESTART_OBSERVED"
	AttemptVerificationRequired = "VERIFICATION_REQUIRED"
	AttemptFulfilled            = "FULFILLED"
	// Terminal side states.
	AttemptAbandoned    = "ABANDONED"     // never written to the server: safe to plan again
	AttemptUnstaged     = "UNSTAGED"      // written, then proven removed BEFORE any restart: safe to plan again
	AttemptFailedReview = "FAILED_REVIEW" // uncertain outcome: a human decides (manual handover or refund); never retried automatically
)

var attemptTransitions = map[string][]string{
	AttemptPlanCreated:          {AttemptFilePrepared, AttemptAbandoned},
	AttemptFilePrepared:         {AttemptFileStaged, AttemptAbandoned},
	AttemptFileStaged:           {AttemptAwaitingRestart, AttemptUnstaged, AttemptFailedReview},
	AttemptAwaitingRestart:      {AttemptRestartObserved, AttemptUnstaged, AttemptFailedReview},
	AttemptRestartObserved:      {AttemptVerificationRequired, AttemptFailedReview},
	AttemptVerificationRequired: {AttemptFulfilled, AttemptFailedReview},
}

// CanAdvance reports whether an attempt may move between two states.
func CanAdvance(from, to string) bool {
	for _, s := range attemptTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// terminal states end an attempt; "safe" terminal states prove nothing reached a server start.
func terminal(s string) bool {
	return s == AttemptFulfilled || s == AttemptAbandoned || s == AttemptUnstaged || s == AttemptFailedReview
}
func provenNotSpawned(s string) bool { return s == AttemptAbandoned || s == AttemptUnstaged }

var (
	ErrAttemptInProgress   = errors.New("another attempt for this delivery is in progress")
	ErrAttemptUncertain    = errors.New("an earlier attempt may have spawned the item: resolve it by review before any new attempt")
	ErrAttemptFulfilled    = errors.New("this delivery was already fulfilled")
	ErrAttemptNotFound     = errors.New("attempt not found")
	ErrAttemptTransition   = errors.New("invalid attempt transition")
	ErrAttemptPlanMismatch = errors.New("the attempt number does not follow the delivery's history")
)

// AttemptRecord is one attempt. A durable implementation stores exactly these fields (see docs).
type AttemptRecord struct {
	AttemptID   string
	DeliveryID  int64
	Attempt     int
	State       string
	Fingerprint string
}

// MemoryLedger is the prototype of the durable attempt ledger: the rules are the ones a table with a
// partial unique index would enforce (at most one non-terminal attempt per delivery, and no new
// attempt after any attempt that might have spawned).
type MemoryLedger struct {
	mu         sync.Mutex
	byDelivery map[int64][]*AttemptRecord
}

func NewMemoryLedger() *MemoryLedger { return &MemoryLedger{byDelivery: map[int64][]*AttemptRecord{}} }

// NextAttempt is the attempt number a new plan for the delivery must use.
func (l *MemoryLedger) NextAttempt(deliveryID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byDelivery[deliveryID]) + 1
}

// Begin records PLAN_CREATED for the plan, or refuses: while another attempt is open, after a
// fulfillment, or after any attempt that is not proven to have spawned nothing.
func (l *MemoryLedger) Begin(p Plan) (AttemptRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prior := l.byDelivery[p.DeliveryID()]
	for _, a := range prior {
		switch {
		case a.State == AttemptFulfilled:
			return AttemptRecord{}, ErrAttemptFulfilled
		case !terminal(a.State):
			return AttemptRecord{}, ErrAttemptInProgress
		case !provenNotSpawned(a.State):
			return AttemptRecord{}, ErrAttemptUncertain
		}
	}
	if p.Attempt() != len(prior)+1 {
		return AttemptRecord{}, ErrAttemptPlanMismatch
	}
	rec := &AttemptRecord{AttemptID: p.AttemptID(), DeliveryID: p.DeliveryID(), Attempt: p.Attempt(), State: AttemptPlanCreated, Fingerprint: p.Fingerprint()}
	l.byDelivery[p.DeliveryID()] = append(prior, rec)
	return *rec, nil
}

// Advance moves an attempt along the state machine (compare-and-set on the current state).
func (l *MemoryLedger) Advance(deliveryID int64, attemptID, from, to string) (AttemptRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range l.byDelivery[deliveryID] {
		if a.AttemptID != attemptID {
			continue
		}
		if a.State != from || !CanAdvance(from, to) {
			return *a, ErrAttemptTransition
		}
		a.State = to
		return *a, nil
	}
	return AttemptRecord{}, ErrAttemptNotFound
}

// Reconcile is what a restarted worker does for an attempt whose last known state is uncertain: it
// never re-stages. Anything that may have reached a server start goes to review; anything provably
// never written is abandoned; a staged-but-not-restarted file must be unstaged first.
func Reconcile(state string, fileStillContainsAttempt bool, restartSinceStaging bool) (next string, action string) {
	switch state {
	case AttemptPlanCreated, AttemptFilePrepared:
		if fileStillContainsAttempt {
			return AttemptFileStaged, "the upload landed although the worker did not record it: continue from FILE_STAGED"
		}
		return AttemptAbandoned, "nothing was written: safe to plan a new attempt"
	case AttemptFileStaged, AttemptAwaitingRestart:
		if restartSinceStaging {
			return AttemptVerificationRequired, "a server start happened while staged: unstage now, then verify with the player - never re-stage"
		}
		if fileStillContainsAttempt {
			return state, "still staged: either wait for the owner-confirmed restart or unstage (then UNSTAGED)"
		}
		return AttemptUnstaged, "the entry is gone and no start happened: nothing spawned"
	case AttemptRestartObserved:
		return AttemptVerificationRequired, "unstage if still present, then verify - a restart is not proof of delivery"
	default:
		return state, "no action"
	}
}
