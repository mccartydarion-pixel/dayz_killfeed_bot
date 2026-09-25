package canary

import (
	"errors"
	"sort"
	"time"

	nd "github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// StagingQuietPeriod: a boot that started less than this before the staged upload is ambiguous (the
// spawner runs about 30-40 s after the boot starts and the new boot's ADM is listed 7.5-9 min
// after it), so a stage must never be attempted inside it and an attempt that was is sent to review.
const StagingQuietPeriod = 10 * time.Minute

// Spawner outcome for one boot, from the RPT. DayZ logs a [::SpawnObjects] line only on an error; a
// successful spawn writes nothing, so "no error" is never proof that the item exists.
const (
	SpawnerNoErrorReported = "NO_ERROR_REPORTED"
	SpawnerFailed          = "FAILED"
	SpawnerUnknown         = "UNKNOWN"
)

// BootObservation is one boot as Live Sync accepted it (boot authority: the newest ADM file of the
// bound service), with its RPT facts.
type BootObservation struct {
	BootFile             string
	StartUTC             time.Time // the ADM file's local start converted with the learned server clock
	SourceAuthoritative  bool      // accepted by boot authority (not an older or unverified candidate)
	CEInitSeen           bool      // the boot's RPT reached the Central Economy init
	SpawnerErrorArtifact bool      // the boot's RPT has [::SpawnObjects] [ERROR] naming the Champion file
}

// RestartEvidence is everything the decision uses. All times are UTC.
type RestartEvidence struct {
	State               string
	StagedAt            time.Time // when the staged upload's read-back was verified
	StagedBootFile      string    // the accepted boot when staging was verified
	PreStartSeenAt      *time.Time
	Boots               []BootObservation
	FileContainsAttempt *bool      // nil = not read since the last write
	UnstageVerifiedAt   *time.Time // the unstaged read-back was verified at this time
}

// RestartDecision is what to do next. Next is the state the evidence supports; Action is the only
// thing a worker (or the operator) may do now.
type RestartDecision struct {
	Next             string
	Action           string
	NewBoots         int
	SpawnerOutcome   string
	SecondStartClean bool // a start after the verified unstage happened without re-spawning the entry
}

// DecideRestart applies the Phase 2C.2 restart rules:
//   - restart.log's pre-start line alone never unstages (the entry must still be in the file when the
//     boot starts); the new boot is identified only by boot authority;
//   - the first source-authoritative boot that started after the verified stage is RESTART_OBSERVED ->
//     UNSTAGE_REQUIRED at once;
//   - if a second new boot starts before the unstage is verified, the item may have spawned twice:
//     FAILED_REVIEW;
//   - a boot too close to the stage is ambiguous: FAILED_REVIEW.
func DecideRestart(e RestartEvidence) RestartDecision {
	boots := append([]BootObservation(nil), e.Boots...)
	sort.Slice(boots, func(i, j int) bool { return boots[i].StartUTC.Before(boots[j].StartUTC) })
	var newer []BootObservation
	for _, b := range boots {
		if !b.SourceAuthoritative || b.StartUTC.IsZero() || b.BootFile == e.StagedBootFile {
			continue
		}
		if b.StartUTC.After(e.StagedAt) {
			newer = append(newer, b)
		} else if e.StagedAt.Sub(b.StartUTC) < StagingQuietPeriod {
			return RestartDecision{Next: nd.AttemptFailedReview, SpawnerOutcome: SpawnerUnknown,
				Action: "a boot started within the staging quiet period of the upload: whether the spawner read the file is ambiguous - unstage and review"}
		}
	}
	d := RestartDecision{NewBoots: len(newer), SpawnerOutcome: SpawnerUnknown}
	if len(newer) > 0 {
		first := newer[0]
		switch {
		case first.SpawnerErrorArtifact:
			d.SpawnerOutcome = SpawnerFailed
		case first.CEInitSeen:
			d.SpawnerOutcome = SpawnerNoErrorReported
		}
	}
	switch e.State {
	case nd.AttemptFileStaged, nd.AttemptAwaitingRestart:
		if len(newer) == 0 {
			d.Next = e.State
			if e.PreStartSeenAt != nil && e.PreStartSeenAt.After(e.StagedAt) {
				d.Action = "restart.log reports a restart beginning: keep the entry staged and wait for the new boot's ADM to be accepted"
			} else {
				d.Action = "keep staged; wait for the approved restart"
			}
			return d
		}
		if len(newer) >= 2 {
			return fail(d, "two boots started after staging without a verified unstage: the item may have spawned twice")
		}
		d.Next = nd.AttemptUnstageRequired
		d.Action = "the new boot is accepted: unstage now (upload the unstaged file and verify its SHA-256) before the next start"
		if d.SpawnerOutcome == SpawnerFailed {
			d.Action = "the server reported the Champion file missing or invalid: unstage now, then review (nothing was spawned by this file)"
		}
		return d
	case nd.AttemptRestartObserved, nd.AttemptUnstageRequired:
		if e.UnstageVerifiedAt == nil {
			if len(newer) >= 2 {
				return fail(d, "a second boot started before the unstage was verified: the item may have spawned twice")
			}
			d.Next = nd.AttemptUnstageRequired
			d.Action = "unstage now and verify the file before the next start"
			return d
		}
		if len(newer) >= 2 && !newer[1].StartUTC.After(*e.UnstageVerifiedAt) {
			return fail(d, "the unstage was verified only after a second boot started: the item may have spawned twice")
		}
		if e.FileContainsAttempt == nil || *e.FileContainsAttempt {
			d.Next = nd.AttemptUnstageRequired
			d.Action = "the file must be read back without the attempt before verification"
			return d
		}
		d.Next = nd.AttemptVerificationRequired
		d.Action = "unstaged and verified: confirm the item physically at the drop point"
		return d
	case nd.AttemptVerificationRequired:
		d.Next = nd.AttemptVerificationRequired
		if e.UnstageVerifiedAt != nil && e.FileContainsAttempt != nil && !*e.FileContainsAttempt {
			for _, b := range newer {
				if b.StartUTC.After(*e.UnstageVerifiedAt) && b.CEInitSeen && !b.SpawnerErrorArtifact {
					d.SecondStartClean = true
				}
			}
		}
		if d.SecondStartClean {
			d.Action = "a later start ran with the entry removed: ready for the owner's physical confirmation (Gate F)"
		} else {
			d.Action = "wait for the approved second start (Gate E) with the entry removed"
		}
		return d
	}
	d.Next = e.State
	d.Action = "no restart action for this state"
	return d
}

func fail(d RestartDecision, why string) RestartDecision {
	d.Next = nd.AttemptFailedReview
	d.Action = why + " - unstage if present, never re-stage, a human decides"
	return d
}

// PhysicalConfirmation is the only evidence that fulfills a canary: a named operator saw the item.
type PhysicalConfirmation struct {
	ConfirmedBy      string // Discord user id of the approving owner/admin
	Method           string // IN_GAME_OBSERVED
	ObservedAt       time.Time
	SecondStartClean bool // DecideRestart reported a clean later start
}

var ErrFulfillmentNotAllowed = errors.New("fulfillment requires VERIFICATION_REQUIRED, a clean second start and a named in-game observation")

// Fulfill is the single VERIFICATION_REQUIRED -> FULFILLED step: never from a restart, an upload or
// a log line.
func Fulfill(state string, c PhysicalConfirmation) (string, error) {
	if state != nd.AttemptVerificationRequired || c.ConfirmedBy == "" || c.Method != "IN_GAME_OBSERVED" || c.ObservedAt.IsZero() || !c.SecondStartClean {
		return state, ErrFulfillmentNotAllowed
	}
	return nd.AttemptFulfilled, nil
}
