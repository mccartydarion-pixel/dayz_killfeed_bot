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

// --- exposure, milestones and fulfillment -----------------------------------------------------------

// Once the item is staged, ANY start spawns it - the server also restarts on its own schedule (four
// consecutive 68-minute intervals observed live). The exposure lasts from the staged upload until the
// empty file is read back, which can take up to one full restart interval plus boot acceptance plus
// the unstage. MinOperatorWindow covers two intervals so that an operator is present for the worst case.
const (
	ObservedRestartInterval = 68 * time.Minute
	MinOperatorWindow       = 2 * ObservedRestartInterval
)

var (
	ErrNoOperator        = errors.New("no operator has confirmed availability for the staging window")
	ErrOperatorWindow    = errors.New("the operator's availability does not cover the whole exposure window")
	ErrBootTooRecent     = errors.New("the current boot started inside the staging quiet period")
	ErrPreStartPending   = errors.New("restart.log reports a restart beginning: do not stage now")
	ErrBootNotAccepted   = errors.New("the current boot is not accepted by boot authority")
	ErrDropPointTooStale = errors.New("the drop point is no longer fresh: take a new observation")
)

// OperatorWindow is the operator's confirmed availability, recorded before staging.
type OperatorWindow struct {
	Operator    string // Discord user id
	ConfirmedAt time.Time
	From, Until time.Time
}

// StagingReadiness is Gate E's final check, evaluated immediately before the staged upload.
type StagingReadiness struct {
	Now               time.Time
	Operator          OperatorWindow
	CurrentBootStart  time.Time
	BootAccepted      bool
	PreStartPending   bool // restart.log pre-start line seen and no newer boot accepted yet
	DropPointObserved time.Time
}

// CheckStagingReadiness refuses to stage unless an operator covers the exposure window, the current
// boot is accepted and old enough, no restart is beginning and the drop point is still fresh.
func CheckStagingReadiness(r StagingReadiness) error {
	w := r.Operator
	switch {
	case w.Operator == "" || w.ConfirmedAt.IsZero() || w.ConfirmedAt.After(r.Now):
		return ErrNoOperator
	case w.From.After(r.Now) || w.Until.Before(r.Now.Add(MinOperatorWindow)):
		return ErrOperatorWindow
	case !r.BootAccepted || r.CurrentBootStart.IsZero():
		return ErrBootNotAccepted
	case r.Now.Sub(r.CurrentBootStart) < StagingQuietPeriod:
		return ErrBootTooRecent
	case r.PreStartPending:
		return ErrPreStartPending
	case r.DropPointObserved.IsZero() || r.Now.Sub(r.DropPointObserved) > DropPointMaxAgeDflt:
		return ErrDropPointTooStale
	}
	return nil
}

// Milestone names, in the order the canary must reach them. Each is separate evidence: a restart is
// not a boot, a boot is not a spawn, an absent RPT error is not an item, and an item is not proof that
// it will not spawn again.
const (
	MilestoneRestartInitiated = "RESTART_INITIATED" // restart.log pre-start line, or the owner's own restart
	MilestoneNewBootAccepted  = "NEW_BOOT_ACCEPTED" // boot authority accepted a boot that started after staging
	MilestoneSpawnerAttempted = "SPAWNER_ATTEMPTED" // that boot's RPT reached CE init (the spawner runs ~11 s later)
	MilestoneItemObserved     = "ITEM_OBSERVED"     // in game: seen at the drop point and picked up, by a named observer
	MilestoneEntryRemoved     = "ENTRY_REMOVED"     // the empty Champion file read back, before any further boot
	MilestoneNoSecondSpawn    = "NO_SECOND_SPAWN"   // a later accepted boot, and in game no new item at the drop point
)

// Evidence is one milestone's proof.
type Evidence struct {
	At     time.Time
	By     string // who observed or recorded it
	Detail string
}

// Milestones is the canary's evidence record.
type Milestones struct {
	RestartInitiated, NewBootAccepted, SpawnerAttempted *Evidence
	ItemObserved, EntryRemoved, NoSecondSpawn           *Evidence
	SecondBootStart                                     time.Time // start of the first boot after EntryRemoved
}

// MilestoneStatus is one line of the checklist.
type MilestoneStatus struct {
	Name    string
	Reached bool
	Problem string
}

// Check lists every milestone and the ordering problems: the entry must be removed before the second
// boot starts, the no-respawn check needs that second boot, and every in-game observation needs a
// named observer.
func (m Milestones) Check() []MilestoneStatus {
	st := func(name string, e *Evidence, needBy bool) MilestoneStatus {
		s := MilestoneStatus{Name: name, Reached: e != nil && !e.At.IsZero()}
		if s.Reached && needBy && e.By == "" {
			s.Reached, s.Problem = false, "needs a named observer"
		}
		return s
	}
	out := []MilestoneStatus{
		st(MilestoneRestartInitiated, m.RestartInitiated, false),
		st(MilestoneNewBootAccepted, m.NewBootAccepted, false),
		st(MilestoneSpawnerAttempted, m.SpawnerAttempted, false),
		st(MilestoneItemObserved, m.ItemObserved, true),
		st(MilestoneEntryRemoved, m.EntryRemoved, false),
		st(MilestoneNoSecondSpawn, m.NoSecondSpawn, true),
	}
	if out[4].Reached && !m.SecondBootStart.IsZero() && !m.EntryRemoved.At.Before(m.SecondBootStart) {
		out[4].Reached, out[4].Problem = false, "removed only after the second boot started: possible second spawn"
	}
	if out[5].Reached && (m.SecondBootStart.IsZero() || !out[4].Reached || m.NoSecondSpawn.At.Before(m.SecondBootStart)) {
		out[5].Reached, out[5].Problem = false, "needs a second boot after the verified removal, then the in-game check"
	}
	return out
}

// Complete reports whether every milestone is reached without a problem.
func (m Milestones) Complete() bool {
	for _, s := range m.Check() {
		if !s.Reached {
			return false
		}
	}
	return true
}

// PhysicalConfirmation is the only evidence that fulfills a canary. The original item does not have
// to remain at the drop point: it must have been seen there and picked up, and a later start must
// not have produced a new one.
type PhysicalConfirmation struct {
	ConfirmedBy        string // Discord user id of the approving owner/admin
	Method             string // IN_GAME_OBSERVED
	ObservedAt         time.Time
	PickedUpBy         string // the in-game character that collected it
	PickupObservedAt   time.Time
	NoRespawnCheckedAt time.Time // in game, after the second start
	Milestones         Milestones
}

var ErrFulfillmentNotAllowed = errors.New("fulfillment requires VERIFICATION_REQUIRED, every canary milestone, and a named in-game observation with pickup")

// Fulfill is the single VERIFICATION_REQUIRED -> FULFILLED step: never from a restart, an upload or
// a log line.
func Fulfill(state string, c PhysicalConfirmation) (string, error) {
	if state != nd.AttemptVerificationRequired || c.ConfirmedBy == "" || c.Method != "IN_GAME_OBSERVED" || c.ObservedAt.IsZero() ||
		c.PickedUpBy == "" || c.PickupObservedAt.IsZero() || c.PickupObservedAt.Before(c.ObservedAt) || c.NoRespawnCheckedAt.IsZero() || !c.Milestones.Complete() {
		return state, ErrFulfillmentNotAllowed
	}
	return nd.AttemptFulfilled, nil
}
