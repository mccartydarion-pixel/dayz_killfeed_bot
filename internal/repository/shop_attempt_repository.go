package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Durable delivery-attempt ledger (migration 0054, docs/SHOP_DELIVERY_PHASE2C3.md). An attempt is one
// try at delivering a MANUAL_COORDINATE purchase automatically through the DayZ object spawner. The
// ledger is the database half of the Phase 2B/2C state machine (internal/shop/nitradodelivery): every
// rule is enforced by the table's constraints and triggers, and this repository maps their SQLSTATEs to
// typed errors. Nothing in production creates attempts yet - automatic delivery stays disabled.

// Attempt states (identical to nitradodelivery's; a test keeps them in sync).
const (
	AttemptPlanCreated          = "PLAN_CREATED"
	AttemptFilePrepared         = "FILE_PREPARED"
	AttemptFileStaged           = "FILE_STAGED"
	AttemptAwaitingRestart      = "AWAITING_RESTART"
	AttemptRestartObserved      = "RESTART_OBSERVED"
	AttemptUnstageRequired      = "UNSTAGE_REQUIRED"
	AttemptVerificationRequired = "VERIFICATION_REQUIRED"
	AttemptFulfilled            = "FULFILLED"
	AttemptAbandoned            = "ABANDONED"
	AttemptUnstaged             = "UNSTAGED"
	AttemptFailedReview         = "FAILED_REVIEW"
)

// ShopAttemptTransitions is the state machine the database enforces (trg_shop_delivery_attempt_guard).
var ShopAttemptTransitions = map[string][]string{
	AttemptPlanCreated:          {AttemptFilePrepared, AttemptAbandoned},
	AttemptFilePrepared:         {AttemptFileStaged, AttemptAbandoned},
	AttemptFileStaged:           {AttemptAwaitingRestart, AttemptUnstaged, AttemptFailedReview},
	AttemptAwaitingRestart:      {AttemptRestartObserved, AttemptUnstaged, AttemptFailedReview},
	AttemptRestartObserved:      {AttemptUnstageRequired, AttemptFailedReview},
	AttemptUnstageRequired:      {AttemptVerificationRequired, AttemptFailedReview},
	AttemptVerificationRequired: {AttemptFulfilled, AttemptFailedReview},
}

// Review resolutions a human records for a FAILED_REVIEW attempt.
const (
	ReviewNotSpawned = "NOT_SPAWNED" // verified that the item never reached the game: a refund becomes possible
	ReviewSpawned    = "SPAWNED"     // the item (may have) reached the player: fulfil, never refund automatically
)

var (
	// ErrShopDeliveryAttemptActive: an automatic attempt may have put the item on the server (or a review
	// is unresolved), so the purchase can be neither refunded nor fulfilled by hand. HTTP 409.
	ErrShopDeliveryAttemptActive = errors.New("an automatic delivery attempt may have put the item on the server: resolve it before refunding or fulfilling")
	ErrShopAttemptConflict       = errors.New("the delivery already has an open, fulfilled or unresolved delivery attempt")
	ErrShopAttemptDeliveryClosed = errors.New("the delivery is not open for an automatic delivery attempt")
	ErrShopAttemptSequence       = errors.New("the attempt number does not follow the delivery's history")
	ErrShopAttemptNotFound       = errors.New("delivery attempt not found")
	ErrShopAttemptStale          = errors.New("the delivery attempt is no longer in the expected state")
	ErrShopAttemptRejected       = errors.New("the delivery attempt change is not allowed")
	ErrShopAttemptEvidence       = errors.New("the delivery attempt evidence is missing or inconsistent")
)

// mapAttemptErr turns the ledger's SQLSTATEs into typed errors; anything else is returned as is.
func mapAttemptErr(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "SA409":
		return ErrShopDeliveryAttemptActive
	case "SA410":
		return ErrShopAttemptConflict
	case "SA411":
		return ErrShopAttemptDeliveryClosed
	case "SA412":
		return ErrShopAttemptSequence
	case "SA422":
		return fmt.Errorf("%w: %s", ErrShopAttemptRejected, pgErr.Message)
	case "23514":
		if strings.HasPrefix(pgErr.ConstraintName, "shop_delivery_attempts") {
			return fmt.Errorf("%w (%s)", ErrShopAttemptEvidence, pgErr.ConstraintName)
		}
	case "23505":
		if strings.HasPrefix(pgErr.ConstraintName, "uq_shop_delivery_attempts") || pgErr.ConstraintName == "shop_delivery_attempts_attempt_id_key" {
			return ErrShopAttemptConflict
		}
	case "23503":
		if strings.HasPrefix(pgErr.TableName, "shop_delivery_attempts") {
			return ErrShopAttemptDeliveryClosed
		}
	}
	return err
}

// ShopAttempt is one ledger row.
type ShopAttempt struct {
	ID, OrganizationID, InstallationID, DeliveryID int64
	Attempt                                        int
	AttemptID, State, Fingerprint, ArtifactPath    string
	ClassName                                      string
	Quantity                                       int
	PosX, PosY, PosZ                               float64
	DropSourceFile                                 string
	DropSourceOffset                               int64
	BeforeSHA256, StagedSHA256, UnstagedSHA256     *string
	StagedAt                                       *time.Time
	StagedBootFile, RestartBootFile                *string
	RestartObservedAt, UnstageVerifiedAt           *time.Time
	ItemObservedBy                                 *string
	ItemObservedAt                                 *time.Time
	PickedUpBy                                     *string
	PickupObservedAt                               *time.Time
	SecondBootFile                                 *string
	SecondBootStartedAt, NoRespawnCheckedAt        *time.Time
	VerifiedBy                                     *string
	FulfilledAt                                    *time.Time
	FailureReason                                  *string
	ReviewResolution, ReviewResolvedBy             *string
	ReviewResolvedAt                               *time.Time
	CreatedAt, UpdatedAt                           time.Time
}

const attemptCols = `id, organization_id, installation_id, delivery_id, attempt, attempt_id, state, fingerprint, artifact_path, class_name, quantity,
 pos_x, pos_y, pos_z, drop_source_file, drop_source_offset, before_sha256, staged_sha256, unstaged_sha256, staged_at, staged_boot_file,
 restart_boot_file, restart_observed_at, unstage_verified_at, item_observed_by, item_observed_at, picked_up_by, pickup_observed_at,
 second_boot_file, second_boot_started_at, no_respawn_checked_at, verified_by, fulfilled_at, failure_reason,
 review_resolution, review_resolved_by, review_resolved_at, created_at, updated_at`

func scanAttempt(row pgx.Row) (ShopAttempt, error) {
	var a ShopAttempt
	err := row.Scan(&a.ID, &a.OrganizationID, &a.InstallationID, &a.DeliveryID, &a.Attempt, &a.AttemptID, &a.State, &a.Fingerprint, &a.ArtifactPath,
		&a.ClassName, &a.Quantity, &a.PosX, &a.PosY, &a.PosZ, &a.DropSourceFile, &a.DropSourceOffset, &a.BeforeSHA256, &a.StagedSHA256,
		&a.UnstagedSHA256, &a.StagedAt, &a.StagedBootFile, &a.RestartBootFile, &a.RestartObservedAt, &a.UnstageVerifiedAt, &a.ItemObservedBy,
		&a.ItemObservedAt, &a.PickedUpBy, &a.PickupObservedAt, &a.SecondBootFile, &a.SecondBootStartedAt, &a.NoRespawnCheckedAt, &a.VerifiedBy,
		&a.FulfilledAt, &a.FailureReason, &a.ReviewResolution, &a.ReviewResolvedBy, &a.ReviewResolvedAt, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// ShopAttemptCreate is a new attempt (the immutable plan facts).
type ShopAttemptCreate struct {
	OrganizationID, InstallationID, DeliveryID int64
	Attempt                                    int
	AttemptID, Fingerprint, ClassName          string
	Quantity                                   int
	PosX, PosY, PosZ                           float64
	DropSourceFile                             string
	DropSourceOffset                           int64
}

// ShopAttemptEvidence is the write-once evidence a transition records. A nil field is left as it is.
type ShopAttemptEvidence struct {
	BeforeSHA256, StagedSHA256, UnstagedSHA256 *string
	StagedAt                                   *time.Time
	StagedBootFile, RestartBootFile            *string
	RestartObservedAt, UnstageVerifiedAt       *time.Time
	FailureReason                              *string
	Note                                       string // free-text evidence for the event log (<= 1024)
}

// ShopAttemptPhysicalEvidence is what Gate I requires: a named in-game observation, the pickup, and a
// second boot after the verified unstage with no new item at the drop point.
type ShopAttemptPhysicalEvidence struct {
	ItemObservedBy      string
	ItemObservedAt      time.Time
	PickedUpBy          string
	PickupObservedAt    time.Time
	SecondBootFile      string
	SecondBootStartedAt time.Time
	NoRespawnCheckedAt  time.Time
	Note                string
}

// ShopAttemptEvent is one history row.
type ShopAttemptEvent struct {
	FromState, ToState, Actor, Evidence string
	CreatedAt                           time.Time
}

// ShopAttemptRepository is the ledger. It shares the Shop pool.
type ShopAttemptRepository struct{ pool pgxBeginner }

type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	shopQuerier
}

func NewShopAttemptRepository(pool pgxBeginner) *ShopAttemptRepository {
	return &ShopAttemptRepository{pool: pool}
}

// setActor binds the acting identity (and optional evidence note) to the transaction; the ledger
// triggers refuse any change without it and write it to the event log.
func setActor(ctx context.Context, tx pgx.Tx, actor, note string) error {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 100 {
		return fmt.Errorf("%w: an actor is required", ErrShopAttemptRejected)
	}
	_, err := tx.Exec(ctx, `SELECT set_config('champion.actor', $1, true), set_config('champion.evidence', $2, true)`, actor, clipText(note, 1024))
	return err
}

func (r *ShopAttemptRepository) inTx(ctx context.Context, actor, note string, fn func(pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := setActor(ctx, tx, actor, note); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return mapAttemptErr(err)
	}
	return mapAttemptErr(tx.Commit(ctx))
}

// Create records PLAN_CREATED. The database refuses a second open attempt, a new attempt after a
// fulfilled or unresolved one (never an automatic re-stage), an attempt on a closed or other-tenant
// delivery, and an attempt number that skips history.
func (r *ShopAttemptRepository) Create(ctx context.Context, in ShopAttemptCreate, actor string) (ShopAttempt, error) {
	var out ShopAttempt
	err := r.inTx(ctx, actor, "plan created", func(tx pgx.Tx) error {
		var err error
		out, err = scanAttempt(tx.QueryRow(ctx, `INSERT INTO shop_delivery_attempts(organization_id, installation_id, delivery_id, attempt, attempt_id,
 fingerprint, artifact_path, class_name, quantity, pos_x, pos_y, pos_z, drop_source_file, drop_source_offset)
VALUES($1,$2,$3,$4,$5,$6,'champion/champion_shop_delivery.json',$7,$8,$9,$10,$11,$12,$13) RETURNING `+attemptCols,
			in.OrganizationID, in.InstallationID, in.DeliveryID, in.Attempt, in.AttemptID, in.Fingerprint, in.ClassName, in.Quantity,
			in.PosX, in.PosY, in.PosZ, in.DropSourceFile, in.DropSourceOffset))
		return err
	})
	return out, err
}

// Transition is the compare-and-set: it moves the attempt from `from` to `to` only if it is still in
// `from` and belongs to the tenant, recording the evidence. A lost race is ErrShopAttemptStale.
func (r *ShopAttemptRepository) Transition(ctx context.Context, org, inst int64, attemptID, from, to, actor string, ev ShopAttemptEvidence) (ShopAttempt, error) {
	if from == to {
		return ShopAttempt{}, fmt.Errorf("%w: a transition must change the state", ErrShopAttemptRejected)
	}
	if to == AttemptFulfilled {
		return ShopAttempt{}, fmt.Errorf("%w: FULFILLED is reached only through FulfillAttempt", ErrShopAttemptRejected)
	}
	var out ShopAttempt
	err := r.inTx(ctx, actor, ev.Note, func(tx pgx.Tx) error {
		var err error
		out, err = scanAttempt(tx.QueryRow(ctx, `UPDATE shop_delivery_attempts SET state=$3,
 before_sha256=COALESCE($6, before_sha256), staged_sha256=COALESCE($7, staged_sha256), unstaged_sha256=COALESCE($8, unstaged_sha256),
 staged_at=COALESCE($9, staged_at), staged_boot_file=COALESCE($10, staged_boot_file), restart_boot_file=COALESCE($11, restart_boot_file),
 restart_observed_at=COALESCE($12, restart_observed_at), unstage_verified_at=COALESCE($13, unstage_verified_at), failure_reason=COALESCE($14, failure_reason)
WHERE attempt_id=$1 AND state=$2 AND organization_id=$4 AND installation_id=$5 RETURNING `+attemptCols,
			attemptID, from, to, org, inst, ev.BeforeSHA256, ev.StagedSHA256, ev.UnstagedSHA256, ev.StagedAt, ev.StagedBootFile, ev.RestartBootFile,
			ev.RestartObservedAt, ev.UnstageVerifiedAt, ev.FailureReason))
		if errors.Is(err, pgx.ErrNoRows) {
			return r.staleOrMissing(ctx, tx, org, inst, attemptID)
		}
		return err
	})
	return out, err
}

func (r *ShopAttemptRepository) staleOrMissing(ctx context.Context, q shopQuerier, org, inst int64, attemptID string) error {
	var one int
	err := q.QueryRow(ctx, `SELECT 1 FROM shop_delivery_attempts WHERE attempt_id=$1 AND organization_id=$2 AND installation_id=$3`, attemptID, org, inst).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrShopAttemptNotFound
	}
	if err != nil {
		return err
	}
	return ErrShopAttemptStale
}

// ResolveReview records, once, what a human established about a FAILED_REVIEW attempt. It never
// re-opens the attempt and never allows a new one: NOT_SPAWNED makes a refund possible, SPAWNED makes
// a (manual) fulfilment possible.
func (r *ShopAttemptRepository) ResolveReview(ctx context.Context, org, inst int64, attemptID, resolution, actor, note string) (ShopAttempt, error) {
	if resolution != ReviewNotSpawned && resolution != ReviewSpawned {
		return ShopAttempt{}, fmt.Errorf("%w: unknown review resolution", ErrShopAttemptRejected)
	}
	var out ShopAttempt
	err := r.inTx(ctx, actor, note, func(tx pgx.Tx) error {
		var err error
		out, err = scanAttempt(tx.QueryRow(ctx, `UPDATE shop_delivery_attempts SET review_resolution=$4, review_resolved_by=$5, review_resolved_at=NOW()
WHERE attempt_id=$1 AND organization_id=$2 AND installation_id=$3 AND state='FAILED_REVIEW' AND review_resolution IS NULL RETURNING `+attemptCols,
			attemptID, org, inst, resolution, strings.TrimSpace(actor)))
		if errors.Is(err, pgx.ErrNoRows) {
			return r.staleOrMissing(ctx, tx, org, inst, attemptID)
		}
		return err
	})
	return out, err
}

// Get loads one attempt of the tenant.
func (r *ShopAttemptRepository) Get(ctx context.Context, org, inst int64, attemptID string) (ShopAttempt, error) {
	a, err := scanAttempt(r.pool.QueryRow(ctx, `SELECT `+attemptCols+` FROM shop_delivery_attempts WHERE attempt_id=$1 AND organization_id=$2 AND installation_id=$3`, attemptID, org, inst))
	if errors.Is(err, pgx.ErrNoRows) {
		return ShopAttempt{}, ErrShopAttemptNotFound
	}
	return a, err
}

// ListOpen is what a restarted worker reconciles: the tenant's attempts that are not terminal.
func (r *ShopAttemptRepository) ListOpen(ctx context.Context, org, inst int64) ([]ShopAttempt, error) {
	return r.list(ctx, `SELECT `+attemptCols+` FROM shop_delivery_attempts WHERE organization_id=$1 AND installation_id=$2
 AND state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW') ORDER BY id`, org, inst)
}

// ListUnresolvedReviews lists FAILED_REVIEW attempts no human has resolved yet.
func (r *ShopAttemptRepository) ListUnresolvedReviews(ctx context.Context, org, inst int64) ([]ShopAttempt, error) {
	return r.list(ctx, `SELECT `+attemptCols+` FROM shop_delivery_attempts WHERE organization_id=$1 AND installation_id=$2
 AND state='FAILED_REVIEW' AND review_resolution IS NULL ORDER BY id`, org, inst)
}

func (r *ShopAttemptRepository) list(ctx context.Context, sql string, args ...any) ([]ShopAttempt, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Events is the attempt's append-only history, oldest first.
func (r *ShopAttemptRepository) Events(ctx context.Context, org, inst int64, attemptID string) ([]ShopAttemptEvent, error) {
	rows, err := r.pool.Query(ctx, `SELECT e.from_state, e.to_state, e.actor, e.evidence, e.created_at FROM shop_delivery_attempt_events e
 JOIN shop_delivery_attempts a ON a.id = e.attempt_row_id WHERE a.attempt_id=$1 AND a.organization_id=$2 AND a.installation_id=$3 ORDER BY e.id`, attemptID, org, inst)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopAttemptEvent
	for rows.Next() {
		var e ShopAttemptEvent
		if err := rows.Scan(&e.FromState, &e.ToState, &e.Actor, &e.Evidence, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FulfillAttempt is Gate I: in ONE transaction it moves the attempt VERIFICATION_REQUIRED -> FULFILLED
// with the physical evidence, and the delivery and the purchase to FULFILLED. Lock order is the Shop's:
// the purchase row first, so it serializes with Refund and the manual Fulfill.
func (r *ShopAttemptRepository) FulfillAttempt(ctx context.Context, org, inst int64, attemptID string, actorUserID int64, actorDiscordID string, ev ShopAttemptPhysicalEvidence) (*ShopPurchase, error) {
	if strings.TrimSpace(ev.ItemObservedBy) == "" || strings.TrimSpace(ev.PickedUpBy) == "" || strings.TrimSpace(ev.SecondBootFile) == "" ||
		ev.ItemObservedAt.IsZero() || ev.PickupObservedAt.IsZero() || ev.SecondBootStartedAt.IsZero() || ev.NoRespawnCheckedAt.IsZero() {
		return nil, ErrShopAttemptEvidence
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := setActor(ctx, tx, actorDiscordID, ev.Note); err != nil {
		return nil, err
	}
	var purchaseID int64
	err = tx.QueryRow(ctx, `SELECT sd.purchase_id FROM shop_delivery_attempts a JOIN shop_deliveries sd ON sd.id = a.delivery_id
 WHERE a.attempt_id=$1 AND a.organization_id=$2 AND a.installation_id=$3`, attemptID, org, inst).Scan(&purchaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrShopAttemptNotFound
	}
	if err != nil {
		return nil, err
	}
	cur, err := getPurchase(ctx, tx, org, inst, purchaseID, 0, true)
	if err != nil {
		return nil, err
	}
	if cur.Status != ShopStatusPendingFulfillment {
		return nil, ErrShopInvalidStatus
	}
	tag, err := tx.Exec(ctx, `UPDATE shop_delivery_attempts SET state='FULFILLED', item_observed_by=$4, item_observed_at=$5, picked_up_by=$6, pickup_observed_at=$7,
 second_boot_file=$8, second_boot_started_at=$9, no_respawn_checked_at=$10, verified_by=$11, fulfilled_at=NOW()
WHERE attempt_id=$1 AND organization_id=$2 AND installation_id=$3 AND state='VERIFICATION_REQUIRED'`,
		attemptID, org, inst, strings.TrimSpace(ev.ItemObservedBy), ev.ItemObservedAt, strings.TrimSpace(ev.PickedUpBy), ev.PickupObservedAt,
		strings.TrimSpace(ev.SecondBootFile), ev.SecondBootStartedAt, ev.NoRespawnCheckedAt, strings.TrimSpace(actorDiscordID))
	if err != nil {
		return nil, mapAttemptErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrShopAttemptStale
	}
	tag, err = tx.Exec(ctx, `UPDATE shop_deliveries SET status='FULFILLED', fulfilled_at=NOW(), fulfilled_by_user_id=$4, updated_at=NOW()
WHERE purchase_id=$3 AND organization_id=$1 AND installation_id=$2 AND status='MANUAL_READY'`, org, inst, purchaseID, nullID(actorUserID))
	if err != nil {
		return nil, mapAttemptErr(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrShopInvalidStatus
	}
	if _, err := tx.Exec(ctx, `UPDATE shop_purchases SET status='FULFILLED', fulfilled_at=NOW(), fulfilled_by_user_id=$4, updated_at=NOW()
WHERE id=$3 AND organization_id=$1 AND installation_id=$2 AND status='PENDING_FULFILLMENT'`, org, inst, purchaseID, nullID(actorUserID)); err != nil {
		return nil, err
	}
	p, err := getPurchase(ctx, tx, org, inst, purchaseID, 0, false)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, mapAttemptErr(err)
	}
	return p, nil
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// attemptBlocksClose is the Go side of trg_shop_delivery_exposure_guard, evaluated under the purchase
// lock before the Shop writes anything, so the caller gets ErrShopDeliveryAttemptActive (HTTP 409)
// instead of a database error. refund=true applies the stricter refund rule for FAILED_REVIEW.
// PLAN_CREATED attempts never touched the server: they are abandoned in the same transaction.
func attemptBlocksClose(ctx context.Context, tx pgx.Tx, org, inst, purchaseID int64, refund bool, actor string) error {
	var blocked, planned int
	err := tx.QueryRow(ctx, `SELECT
 COUNT(*) FILTER (WHERE a.state IN ('FILE_PREPARED','FILE_STAGED','AWAITING_RESTART','RESTART_OBSERVED','UNSTAGE_REQUIRED','VERIFICATION_REQUIRED')
   OR (a.state = 'FAILED_REVIEW' AND $4 AND a.review_resolution IS DISTINCT FROM 'NOT_SPAWNED')
   OR (a.state = 'FAILED_REVIEW' AND NOT $4 AND a.review_resolution IS NULL)),
 COUNT(*) FILTER (WHERE a.state = 'PLAN_CREATED')
FROM shop_delivery_attempts a JOIN shop_deliveries sd ON sd.id = a.delivery_id
WHERE sd.purchase_id=$3 AND a.organization_id=$1 AND a.installation_id=$2`, org, inst, purchaseID, refund).Scan(&blocked, &planned)
	if err != nil {
		return err
	}
	if blocked > 0 {
		return ErrShopDeliveryAttemptActive
	}
	if planned == 0 {
		return nil
	}
	if err := setActor(ctx, tx, actor, "delivery closed before any file was prepared"); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE shop_delivery_attempts a SET state='ABANDONED' FROM shop_deliveries sd
WHERE sd.id = a.delivery_id AND sd.purchase_id=$3 AND a.organization_id=$1 AND a.installation_id=$2 AND a.state='PLAN_CREATED'`, org, inst, purchaseID)
	return mapAttemptErr(err)
}
