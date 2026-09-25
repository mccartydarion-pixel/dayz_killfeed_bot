// Package canaryops is the Champion Shop Phase 2C.4 canary operator service
// (docs/SHOP_DELIVERY_PHASE2C4.md): tenant-scoped, OWNER/ADMIN-only operations that record a single
// controlled delivery experiment in the durable attempt ledger (migrations 0054/0055).
//
// It is a RECORDER, not an executor: it has no Nitrado client and no HTTP client, it never uploads a
// file, never restarts a server and never runs in the background. The operator performs each approved
// gate by hand and records what was verified; the ledger enforces the state machine. Every mutating
// operation additionally requires the canary execution lock to be open for the installation
// (config.ShopCanaryExecution), which no other Shop or delivery setting opens.
package canaryops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// The canary is exactly one BandageDressing (owner decision, Phase 2C.2).
const (
	CanaryClassName = "BandageDressing"
	CanaryQuantity  = 1
)

// Review outcomes an operator can record for a FAILED_REVIEW attempt.
const (
	OutcomeNotSpawned = "NOT_SPAWNED" // the item definitely did not spawn: a refund becomes possible
	OutcomeSpawned    = "SPAWNED"     // the item was observed spawning: fulfil by hand, never refund
	OutcomeUncertain  = "UNCERTAIN"   // recorded, but the attempt stays blocked
)

var (
	ErrForbidden           = errors.New("organization OWNER or ADMIN role required")
	ErrInstallationUnknown = errors.New("installation not found in this organization")
	ErrExecutionLocked     = errors.New("canary execution is locked for this installation")
	ErrSuspended           = errors.New("the installation is suspended")
	ErrInvalid             = errors.New("invalid canary request")
	ErrMissingEvidence     = errors.New("required evidence has not been recorded")
	ErrAmbiguousBoot       = errors.New("the new boot did not start after the verified staging: record FAILED_REVIEW instead")
	ErrNotPhysicalProof    = errors.New("only an in-game observation can prove a physical fact; server logs are informational")
	ErrUseFulfill          = errors.New("FULFILLED is reached only through FulfillAttempt")
	// ErrArtifactHashMismatch: the read-back is not the exact file the attempt stages (or the empty
	// file). Nothing is recorded: restore the expected file, read it back again, then record.
	ErrArtifactHashMismatch = errors.New("the read-back file hash is not the attempt's expected artifact")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Ledger is the durable attempt ledger (repository.ShopAttemptRepository).
type Ledger interface {
	Create(ctx context.Context, in repository.ShopAttemptCreate, actor string) (repository.ShopAttempt, error)
	Transition(ctx context.Context, org, inst int64, attemptID, from, to, actor string, ev repository.ShopAttemptEvidence) (repository.ShopAttempt, error)
	ResolveReview(ctx context.Context, org, inst int64, attemptID, resolution, actor, note string) (repository.ShopAttempt, error)
	Get(ctx context.Context, org, inst int64, attemptID string) (repository.ShopAttempt, error)
	ListAttempts(ctx context.Context, org, inst int64, openOnly bool, limit int) ([]repository.ShopAttempt, error)
	Events(ctx context.Context, org, inst int64, attemptID string) ([]repository.ShopAttemptEvent, error)
	RecordEvidence(ctx context.Context, org, inst int64, attemptID, actor string, in repository.ShopAttemptEvidenceInput) (repository.ShopAttemptEvidenceRecord, error)
	ListEvidence(ctx context.Context, org, inst int64, attemptID string) ([]repository.ShopAttemptEvidenceRecord, error)
	FulfillAttempt(ctx context.Context, org, inst int64, attemptID string, actorUserID int64, actorDiscordID string, ev repository.ShopAttemptPhysicalEvidence) (*repository.ShopPurchase, error)
	NextAttemptNumber(ctx context.Context, org, inst, deliveryID int64) (int, error)
	CanaryBinding(ctx context.Context, org, inst int64) (repository.ShopCanaryBinding, error)
	CurrentBootSession(ctx context.Context, gameServerID int64) (repository.ShopCanarySession, error)
}

// Deliveries reads Shop deliveries (repository.ShopRepository).
type Deliveries interface {
	GetDelivery(ctx context.Context, org, inst, id, playerID int64) (*repository.ShopDelivery, error)
}

// Members verifies organization membership (repository.OrganizationRepository).
type Members interface {
	VerifyMembership(ctx context.Context, organizationID, userID int64) (role string, ok bool, err error)
}

// Scopes resolves an installation inside an organization (economy.Accounts).
type Scopes interface {
	Scope(ctx context.Context, organizationID, installationID int64) (repository.EconomyScope, error)
}

// Gate is the canary execution lock: closed unless explicitly opened for an installation.
type Gate struct{ installations map[int64]bool }

// NewGate opens the lock for the listed installations only when enabled is true.
func NewGate(enabled bool, installationIDs []int64) Gate {
	g := Gate{installations: map[int64]bool{}}
	if !enabled {
		return g
	}
	for _, id := range installationIDs {
		if id > 0 {
			g.installations[id] = true
		}
	}
	return g
}

// Allows reports whether mutating canary operations may run for the installation.
func (g Gate) Allows(installationID int64) bool { return g.installations[installationID] }

// Actor is the authenticated acting user.
type Actor struct {
	UserID    int64
	DiscordID string
}

// Service is the operator service.
type Service struct {
	ledger     Ledger
	deliveries Deliveries
	members    Members
	scopes     Scopes
	gate       Gate
	now        func() time.Time
}

func New(ledger Ledger, deliveries Deliveries, members Members, scopes Scopes, gate Gate) *Service {
	return &Service{ledger: ledger, deliveries: deliveries, members: members, scopes: scopes, gate: gate, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// authorize: an OWNER/ADMIN of the organization that owns the installation; for mutations also an
// active installation and an open canary lock.
func (s *Service) authorize(ctx context.Context, org, inst int64, a Actor, mutate bool) error {
	if a.UserID <= 0 || strings.TrimSpace(a.DiscordID) == "" || org <= 0 || inst <= 0 {
		return ErrForbidden
	}
	role, ok, err := s.members.VerifyMembership(ctx, org, a.UserID)
	if err != nil {
		return err
	}
	if !ok || (role != "OWNER" && role != "ADMIN") {
		return ErrForbidden
	}
	scope, err := s.scopes.Scope(ctx, org, inst)
	if errors.Is(err, economy.ErrInstallationNotFound) {
		return ErrInstallationUnknown
	}
	if err != nil {
		return err
	}
	if scope.OrganizationID != org || scope.InstallationID != inst {
		return ErrInstallationUnknown
	}
	if !mutate {
		return nil
	}
	if scope.Status == "SUSPENDED" {
		return ErrSuspended
	}
	if !s.gate.Allows(inst) {
		return ErrExecutionLocked
	}
	return nil
}

// --- views ------------------------------------------------------------------------------------------

// NextStep is one transition the operator may request, with the evidence still missing for it.
type NextStep struct {
	To              string   `json:"to"`
	MissingEvidence []string `json:"missingEvidence"`
	NeedsReason     bool     `json:"needsReason"`
}

// AttemptView is an attempt with its evidence, history and the operator's options.
type AttemptView struct {
	Attempt  repository.ShopAttempt
	Evidence []repository.ShopAttemptEvidenceRecord
	Events   []repository.ShopAttemptEvent
	Next     []NextStep
}

// requiredEvidence is what each target state needs, in recorded evidence kinds.
var requiredEvidence = map[string][]string{
	repository.AttemptFileStaged:           {repository.EvidenceStagedFileHash, repository.EvidenceStagingBoot},
	repository.AttemptRestartObserved:      {repository.EvidenceNewBoot},
	repository.AttemptVerificationRequired: {repository.EvidenceUnstagedFileHash},
	repository.AttemptUnstaged:             {repository.EvidenceUnstagedFileHash},
	repository.AttemptFulfilled:            {repository.EvidenceItemObserved, repository.EvidencePickupConfirmed, repository.EvidenceSecondBoot, repository.EvidenceNoAdditionalSpawn},
}

func needsReason(to string) bool {
	return to == repository.AttemptFailedReview || to == repository.AttemptAbandoned
}

func byKind(ev []repository.ShopAttemptEvidenceRecord) map[string]repository.ShopAttemptEvidenceRecord {
	m := map[string]repository.ShopAttemptEvidenceRecord{}
	for _, e := range ev {
		if _, dup := m[e.Kind]; !dup {
			m[e.Kind] = e
		}
	}
	return m
}

func missing(to string, have map[string]repository.ShopAttemptEvidenceRecord) []string {
	out := []string{}
	for _, k := range requiredEvidence[to] {
		if _, ok := have[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

func nextSteps(a repository.ShopAttempt, have map[string]repository.ShopAttemptEvidenceRecord) []NextStep {
	out := []NextStep{}
	for _, to := range repository.ShopAttemptTransitions[a.State] {
		out = append(out, NextStep{To: to, MissingEvidence: missing(to, have), NeedsReason: needsReason(to)})
	}
	return out
}

func (s *Service) view(ctx context.Context, org, inst int64, a repository.ShopAttempt) (AttemptView, error) {
	ev, err := s.ledger.ListEvidence(ctx, org, inst, a.AttemptID)
	if err != nil {
		return AttemptView{}, err
	}
	events, err := s.ledger.Events(ctx, org, inst, a.AttemptID)
	if err != nil {
		return AttemptView{}, err
	}
	return AttemptView{Attempt: a, Evidence: ev, Events: events, Next: nextSteps(a, byKind(ev))}, nil
}

// --- operations -------------------------------------------------------------------------------------

// GetAttempt returns one attempt of the installation with its evidence, history and options.
func (s *Service) GetAttempt(ctx context.Context, org, inst int64, a Actor, attemptID string) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, false); err != nil {
		return AttemptView{}, err
	}
	att, err := s.ledger.Get(ctx, org, inst, attemptID)
	if err != nil {
		return AttemptView{}, err
	}
	return s.view(ctx, org, inst, att)
}

// ListAttempts lists the installation's attempts (open only for reconciliation after a crash).
func (s *Service) ListAttempts(ctx context.Context, org, inst int64, a Actor, openOnly bool, limit int) ([]repository.ShopAttempt, error) {
	if err := s.authorize(ctx, org, inst, a, false); err != nil {
		return nil, err
	}
	return s.ledger.ListAttempts(ctx, org, inst, openOnly, limit)
}

// CreateRequest starts the canary attempt for an existing canary purchase's delivery at a verified
// drop point: the ADM position of the current boot, with its altitude.
type CreateRequest struct {
	DeliveryID       int64
	AltitudeY        float64
	DropSourceFile   string
	DropSourceOffset int64
	DropObservedAt   time.Time
}

// CreateAttempt validates the drop point against the current boot session, builds the plan with the
// production validator (nitradodelivery.NewPlan) and records PLAN_CREATED. It writes no file.
func (s *Service) CreateAttempt(ctx context.Context, org, inst int64, a Actor, req CreateRequest) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, true); err != nil {
		return AttemptView{}, err
	}
	if req.DeliveryID <= 0 {
		return AttemptView{}, invalid("deliveryId is required")
	}
	d, err := s.deliveries.GetDelivery(ctx, org, inst, req.DeliveryID, 0)
	if err != nil {
		return AttemptView{}, err
	}
	if len(d.Items) != 1 || d.Items[0].Quantity != CanaryQuantity {
		return AttemptView{}, invalid("the canary delivery must be exactly one item, quantity 1")
	}
	b, err := s.ledger.CanaryBinding(ctx, org, inst)
	if err != nil {
		return AttemptView{}, err
	}
	sess, err := s.ledger.CurrentBootSession(ctx, b.GameServerID)
	if err != nil {
		return AttemptView{}, err
	}
	now := s.now()
	switch {
	case sess.EndedAt != nil:
		return AttemptView{}, invalid("the current boot session has ended: wait for the new boot to be accepted")
	case strings.TrimSpace(req.DropSourceFile) == "" || req.DropSourceFile != sess.ADMFile:
		return AttemptView{}, invalid("the drop point must come from the current boot's ADM file")
	case req.DropSourceOffset <= 0:
		return AttemptView{}, invalid("the drop point needs its ADM byte offset")
	case req.DropObservedAt.IsZero() || now.Sub(req.DropObservedAt) > repository.CanaryDropPointMaxAge:
		return AttemptView{}, invalid("the drop point observation is older than %s: take a fresh one", repository.CanaryDropPointMaxAge)
	case req.DropObservedAt.Sub(now) > repository.CanaryDropPointFutureSkew:
		return AttemptView{}, invalid("the drop point observation is in the future")
	}
	n, err := s.ledger.NextAttemptNumber(ctx, org, inst, d.ID)
	if err != nil {
		return AttemptView{}, err
	}
	alt := req.AltitudeY
	plan, err := nitradodelivery.NewPlan(nitradodelivery.PlanInput{
		OrganizationID: org, InstallationID: inst,
		Binding:  nitradodelivery.Binding{OrganizationID: org, InstallationID: inst, GameServerID: b.GameServerID, NitradoServiceID: b.NitradoServiceID, MapKey: b.MapKey},
		Delivery: *d, ClassName: CanaryClassName, ApprovedClassNames: []string{CanaryClassName}, AltitudeY: &alt, Attempt: n,
	})
	if err != nil {
		return AttemptView{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	pos := plan.Position()
	att, err := s.ledger.Create(ctx, repository.ShopAttemptCreate{
		OrganizationID: org, InstallationID: inst, DeliveryID: d.ID, Attempt: n, AttemptID: plan.AttemptID(), Fingerprint: plan.Fingerprint(),
		ClassName: plan.ClassName(), Quantity: plan.Quantity(), PosX: pos[0], PosY: pos[1], PosZ: pos[2],
		DropSourceFile: req.DropSourceFile, DropSourceOffset: req.DropSourceOffset,
	}, a.DiscordID)
	if err != nil {
		return AttemptView{}, err
	}
	return s.view(ctx, org, inst, att)
}

// EvidenceRequest is one structured observation.
type EvidenceRequest struct {
	Kind, Source           string
	SHA256, PreviousSHA256 string
	BootFile               string
	BootStartedAt          *time.Time
	ObservedBy             string
	ObservedAt             time.Time
	Detail                 string
}

var evidenceSource = map[string]string{
	repository.EvidenceStagedFileHash:    repository.SourceNitradoReadback,
	repository.EvidenceUnstagedFileHash:  repository.SourceNitradoReadback,
	repository.EvidenceStagingBoot:       repository.SourceBootAuthority,
	repository.EvidenceNewBoot:           repository.SourceBootAuthority,
	repository.EvidenceSecondBoot:        repository.SourceBootAuthority,
	repository.EvidenceItemObserved:      repository.SourceInGameObservation,
	repository.EvidencePickupConfirmed:   repository.SourceInGameObservation,
	repository.EvidenceNoAdditionalSpawn: repository.SourceInGameObservation,
	repository.EvidenceSpawnerLog:        repository.SourceRPTLog,
	repository.EvidenceReviewObservation: repository.SourceInGameObservation,
}

// expectedArtifacts are the SHA-256 of the Champion file with only this attempt staged, and of the
// empty file, rebuilt from the ledger row's immutable plan facts.
func expectedArtifacts(a repository.ShopAttempt) (staged, empty string) {
	s, e := nitradodelivery.SingleAttemptFiles(a.AttemptID, a.ClassName, a.Quantity, [3]float64{a.PosX, a.PosY, a.PosZ})
	return nitradodelivery.SHA256(s), nitradodelivery.SHA256(e)
}

// RecordEvidence appends one observation. The source must be the one that can prove the kind: a
// physical fact needs an in-game observation, never a log line.
func (s *Service) RecordEvidence(ctx context.Context, org, inst int64, a Actor, attemptID string, req EvidenceRequest) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, true); err != nil {
		return AttemptView{}, err
	}
	want, known := evidenceSource[req.Kind]
	if !known {
		return AttemptView{}, invalid("unknown evidence kind %q (review assessments go through ResolveReview)", req.Kind)
	}
	if req.Source != want {
		if want == repository.SourceInGameObservation {
			return AttemptView{}, ErrNotPhysicalProof
		}
		return AttemptView{}, invalid("%s must come from %s", req.Kind, want)
	}
	if req.ObservedAt.IsZero() || req.ObservedAt.Sub(s.now()) > time.Minute {
		return AttemptView{}, invalid("observedAt is required and cannot be in the future")
	}
	if want == repository.SourceInGameObservation && strings.TrimSpace(req.ObservedBy) == "" {
		return AttemptView{}, invalid("an in-game observation must name its observer")
	}
	// A file read-back must be exactly the attempt's artifact: the staged file with only this attempt,
	// or the empty file after the unstage.
	if req.Kind == repository.EvidenceStagedFileHash || req.Kind == repository.EvidenceUnstagedFileHash {
		att, err := s.ledger.Get(ctx, org, inst, attemptID)
		if err != nil {
			return AttemptView{}, err
		}
		staged, empty := expectedArtifacts(att)
		got := strings.ToLower(strings.TrimSpace(req.SHA256))
		switch {
		case req.Kind == repository.EvidenceStagedFileHash && (got != staged || strings.ToLower(strings.TrimSpace(req.PreviousSHA256)) != empty):
			return AttemptView{}, fmt.Errorf("%w: staged read-back %s, expected %s (previous must be the empty file %s)", ErrArtifactHashMismatch, got, staged, empty)
		case req.Kind == repository.EvidenceUnstagedFileHash && got != empty:
			return AttemptView{}, fmt.Errorf("%w: unstaged read-back %s, expected the empty file %s", ErrArtifactHashMismatch, got, empty)
		}
	}
	if _, err := s.ledger.RecordEvidence(ctx, org, inst, attemptID, a.DiscordID, repository.ShopAttemptEvidenceInput{
		Kind: req.Kind, Source: req.Source, SHA256: strings.ToLower(req.SHA256), PreviousSHA256: strings.ToLower(req.PreviousSHA256), BootFile: req.BootFile,
		BootStartedAt: req.BootStartedAt, ObservedBy: req.ObservedBy, ObservedAt: req.ObservedAt, Detail: req.Detail,
	}); err != nil {
		return AttemptView{}, err
	}
	return s.GetAttempt(ctx, org, inst, a, attemptID)
}

// AdvanceAttempt moves the attempt one step, filling the ledger's write-once columns from the recorded
// evidence. It never skips the ledger's compare-and-set: a lost race is repository.ErrShopAttemptStale.
func (s *Service) AdvanceAttempt(ctx context.Context, org, inst int64, a Actor, attemptID, from, to, reason string) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, true); err != nil {
		return AttemptView{}, err
	}
	if to == repository.AttemptFulfilled {
		return AttemptView{}, ErrUseFulfill
	}
	reason = strings.TrimSpace(reason)
	if needsReason(to) && reason == "" {
		return AttemptView{}, invalid("a reason is required for %s", to)
	}
	att, err := s.ledger.Get(ctx, org, inst, attemptID)
	if err != nil {
		return AttemptView{}, err
	}
	evs, err := s.ledger.ListEvidence(ctx, org, inst, attemptID)
	if err != nil {
		return AttemptView{}, err
	}
	have := byKind(evs)
	if m := missing(to, have); len(m) > 0 {
		return AttemptView{}, fmt.Errorf("%w: %s", ErrMissingEvidence, strings.Join(m, ", "))
	}
	ev := repository.ShopAttemptEvidence{Note: reason}
	switch to {
	case repository.AttemptFileStaged:
		h, boot := have[repository.EvidenceStagedFileHash], have[repository.EvidenceStagingBoot]
		at := h.ObservedAt
		ev.BeforeSHA256, ev.StagedSHA256, ev.StagedAt, ev.StagedBootFile = h.PreviousSHA256, h.SHA256, &at, boot.BootFile
	case repository.AttemptRestartObserved:
		nb := have[repository.EvidenceNewBoot]
		if att.StagedAt == nil || nb.BootStartedAt == nil || !nb.BootStartedAt.After(*att.StagedAt) {
			return AttemptView{}, ErrAmbiguousBoot
		}
		at := nb.ObservedAt
		ev.RestartBootFile, ev.RestartObservedAt = nb.BootFile, &at
	case repository.AttemptVerificationRequired, repository.AttemptUnstaged:
		u := have[repository.EvidenceUnstagedFileHash]
		at := u.ObservedAt
		ev.UnstagedSHA256, ev.UnstageVerifiedAt = u.SHA256, &at
	case repository.AttemptFailedReview:
		ev.FailureReason = &reason
	}
	if ev.Note == "" {
		ev.Note = "advanced by operator with recorded evidence"
	}
	next, err := s.ledger.Transition(ctx, org, inst, attemptID, from, to, a.DiscordID, ev)
	if err != nil {
		return AttemptView{}, err
	}
	return s.view(ctx, org, inst, next)
}

// ResolveReview records a human's conclusion about a FAILED_REVIEW attempt. NOT_SPAWNED and SPAWNED
// are one-time resolutions in the ledger; UNCERTAIN is recorded as an assessment and leaves the attempt
// (and its purchase) blocked. No outcome ever creates a new attempt.
func (s *Service) ResolveReview(ctx context.Context, org, inst int64, a Actor, attemptID, outcome, note string) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, true); err != nil {
		return AttemptView{}, err
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return AttemptView{}, invalid("a note explaining the conclusion is required")
	}
	switch outcome {
	case OutcomeNotSpawned, OutcomeSpawned:
		// A resolution needs a recorded in-game observation by a named observer (the database
		// enforces the same rule): what was found at the drop point / on the player.
		evs, err := s.ledger.ListEvidence(ctx, org, inst, attemptID)
		if err != nil {
			return AttemptView{}, err
		}
		proven := false
		for _, e := range evs {
			if e.Source != repository.SourceInGameObservation || e.ObservedBy == nil {
				continue
			}
			if e.Kind == repository.EvidenceReviewObservation ||
				(outcome == OutcomeSpawned && (e.Kind == repository.EvidenceItemObserved || e.Kind == repository.EvidencePickupConfirmed)) {
				proven = true
			}
		}
		if !proven {
			return AttemptView{}, fmt.Errorf("%w: %s", ErrMissingEvidence, repository.EvidenceReviewObservation)
		}
		if _, err := s.ledger.ResolveReview(ctx, org, inst, attemptID, outcome, a.DiscordID, note); err != nil {
			return AttemptView{}, err
		}
	case OutcomeUncertain:
		if _, err := s.ledger.RecordEvidence(ctx, org, inst, attemptID, a.DiscordID, repository.ShopAttemptEvidenceInput{
			Kind: repository.EvidenceReviewUncertain, Source: repository.SourceOperatorAssessment, ObservedAt: s.now(), Detail: note,
		}); err != nil {
			return AttemptView{}, err
		}
	default:
		return AttemptView{}, invalid("outcome must be NOT_SPAWNED, SPAWNED or UNCERTAIN")
	}
	return s.GetAttempt(ctx, org, inst, a, attemptID)
}

// FulfillAttempt is Gate I: attempt, delivery and purchase become FULFILLED together, from the recorded
// physical evidence only (in-game observation and pickup, a second boot after the verified unstage, and
// no additional item). A SPAWNER_LOG record is never used.
func (s *Service) FulfillAttempt(ctx context.Context, org, inst int64, a Actor, attemptID, note string) (AttemptView, error) {
	if err := s.authorize(ctx, org, inst, a, true); err != nil {
		return AttemptView{}, err
	}
	evs, err := s.ledger.ListEvidence(ctx, org, inst, attemptID)
	if err != nil {
		return AttemptView{}, err
	}
	have := byKind(evs)
	if m := missing(repository.AttemptFulfilled, have); len(m) > 0 {
		return AttemptView{}, fmt.Errorf("%w: %s", ErrMissingEvidence, strings.Join(m, ", "))
	}
	item, pick, second, none := have[repository.EvidenceItemObserved], have[repository.EvidencePickupConfirmed], have[repository.EvidenceSecondBoot], have[repository.EvidenceNoAdditionalSpawn]
	for _, e := range []repository.ShopAttemptEvidenceRecord{item, pick, none} {
		if e.Source != repository.SourceInGameObservation || e.ObservedBy == nil {
			return AttemptView{}, ErrNotPhysicalProof
		}
	}
	if second.BootFile == nil || second.BootStartedAt == nil {
		return AttemptView{}, fmt.Errorf("%w: %s", ErrMissingEvidence, repository.EvidenceSecondBoot)
	}
	if _, err := s.ledger.FulfillAttempt(ctx, org, inst, attemptID, a.UserID, a.DiscordID, repository.ShopAttemptPhysicalEvidence{
		ItemObservedBy: *item.ObservedBy, ItemObservedAt: item.ObservedAt, PickedUpBy: *pick.ObservedBy, PickupObservedAt: pick.ObservedAt,
		SecondBootFile: *second.BootFile, SecondBootStartedAt: *second.BootStartedAt, NoRespawnCheckedAt: none.ObservedAt,
		Note: strings.TrimSpace(note),
	}); err != nil {
		return AttemptView{}, err
	}
	return s.GetAttempt(ctx, org, inst, a, attemptID)
}
