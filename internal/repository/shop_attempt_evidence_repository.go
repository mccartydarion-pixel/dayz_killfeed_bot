package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Structured canary evidence (migration 0055). See docs/SHOP_DELIVERY_PHASE2C4.md.

// Evidence kinds.
const (
	EvidenceStagedFileHash    = "STAGED_FILE_HASH"    // verified read-back of the staged Champion file
	EvidenceStagingBoot       = "STAGING_BOOT"        // the accepted boot while the file was staged
	EvidenceNewBoot           = "NEW_BOOT"            // the first boot accepted after staging
	EvidenceSpawnerLog        = "SPAWNER_LOG"         // RPT note: informational only, never physical proof
	EvidenceItemObserved      = "ITEM_OBSERVED"       // in game: the item at the drop point
	EvidencePickupConfirmed   = "PICKUP_CONFIRMED"    // in game: the item was picked up
	EvidenceUnstagedFileHash  = "UNSTAGED_FILE_HASH"  // verified read-back of the unstaged (empty) file
	EvidenceSecondBoot        = "SECOND_BOOT"         // a boot after the verified unstage
	EvidenceNoAdditionalSpawn = "NO_ADDITIONAL_SPAWN" // in game after the second boot: no new item
	EvidenceReviewUncertain   = "REVIEW_UNCERTAIN"    // a human assessed a FAILED_REVIEW and could not decide
	EvidenceReviewObservation = "REVIEW_OBSERVATION"  // in game, for a FAILED_REVIEW: what a named observer found
)

// Evidence sources.
const (
	SourceNitradoReadback     = "NITRADO_READBACK"
	SourceBootAuthority       = "BOOT_AUTHORITY"
	SourceInGameObservation   = "IN_GAME_OBSERVATION"
	SourceRPTLog              = "RPT_LOG"
	SourceOperatorAssessment  = "OPERATOR_ASSESSMENT"
	CanaryDropPointMaxAge     = 20 * time.Minute
	CanaryDropPointFutureSkew = time.Minute
)

var (
	ErrShopAttemptEvidenceState  = errors.New("this evidence is not acceptable in the attempt's current state")
	ErrShopAttemptEvidenceExists = errors.New("this evidence was already recorded for the attempt (evidence is write-once)")
	ErrShopCanaryBinding         = errors.New("the installation has no bound game server")
	ErrShopCanaryNoSession       = errors.New("the installation has no current boot session")
)

// ShopAttemptEvidenceInput is one observation to record.
type ShopAttemptEvidenceInput struct {
	Kind, Source           string
	SHA256, PreviousSHA256 string
	BootFile               string
	BootStartedAt          *time.Time
	ObservedBy             string
	ObservedAt             time.Time
	Detail                 string
}

// ShopAttemptEvidenceRecord is one stored observation.
type ShopAttemptEvidenceRecord struct {
	ID                             int64
	Kind, Source, RecordedBy       string
	SHA256, PreviousSHA256         *string
	BootFile                       *string
	BootStartedAt                  *time.Time
	ObservedBy                     *string
	ObservedAt                     time.Time
	Detail                         string
	RecordedAt                     time.Time
	AttemptRowID                   int64
	OrganizationID, InstallationID int64
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.TrimSpace(s)
}

func mapEvidenceErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "SA423":
			return ErrShopAttemptEvidenceState
		case pgErr.Code == "23505" && pgErr.ConstraintName == "uq_shop_attempt_evidence_kind":
			return ErrShopAttemptEvidenceExists
		case pgErr.Code == "23514" && strings.HasPrefix(pgErr.ConstraintName, "shop_attempt_evidence"):
			return ErrShopAttemptEvidence
		}
	}
	return mapAttemptErr(err)
}

const evidenceCols = `e.id, e.kind, e.source, e.recorded_by, e.sha256, e.previous_sha256, e.boot_file, e.boot_started_at, e.observed_by,
 e.observed_at, e.detail, e.recorded_at, e.attempt_row_id, e.organization_id, e.installation_id`

func scanEvidence(row pgx.Row) (ShopAttemptEvidenceRecord, error) {
	var e ShopAttemptEvidenceRecord
	err := row.Scan(&e.ID, &e.Kind, &e.Source, &e.RecordedBy, &e.SHA256, &e.PreviousSHA256, &e.BootFile, &e.BootStartedAt, &e.ObservedBy,
		&e.ObservedAt, &e.Detail, &e.RecordedAt, &e.AttemptRowID, &e.OrganizationID, &e.InstallationID)
	return e, err
}

// RecordEvidence appends one observation to the tenant's attempt. The table refuses a wrong source for
// the kind, missing fields, a kind the attempt's state cannot have, and a second record of a proof kind.
func (r *ShopAttemptRepository) RecordEvidence(ctx context.Context, org, inst int64, attemptID, actor string, in ShopAttemptEvidenceInput) (ShopAttemptEvidenceRecord, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > 100 {
		return ShopAttemptEvidenceRecord{}, ErrShopAttemptRejected
	}
	e, err := scanEvidence(r.pool.QueryRow(ctx, `WITH a AS (
  SELECT id FROM shop_delivery_attempts WHERE attempt_id=$1 AND organization_id=$2 AND installation_id=$3)
INSERT INTO shop_delivery_attempt_evidence AS e (attempt_row_id, organization_id, installation_id, kind, source, recorded_by, sha256, previous_sha256,
  boot_file, boot_started_at, observed_by, observed_at, detail)
SELECT a.id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13 FROM a
RETURNING `+evidenceCols,
		attemptID, org, inst, in.Kind, in.Source, actor, nullStr(in.SHA256), nullStr(in.PreviousSHA256), nullStr(in.BootFile), in.BootStartedAt,
		nullStr(in.ObservedBy), in.ObservedAt, clipText(strings.TrimSpace(in.Detail), 500)))
	if errors.Is(err, pgx.ErrNoRows) {
		return ShopAttemptEvidenceRecord{}, ErrShopAttemptNotFound
	}
	if err != nil {
		return ShopAttemptEvidenceRecord{}, mapEvidenceErr(err)
	}
	return e, nil
}

// ListEvidence is the attempt's evidence, oldest first (tenant-scoped).
func (r *ShopAttemptRepository) ListEvidence(ctx context.Context, org, inst int64, attemptID string) ([]ShopAttemptEvidenceRecord, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+evidenceCols+` FROM shop_delivery_attempt_evidence e
 JOIN shop_delivery_attempts a ON a.id = e.attempt_row_id AND a.organization_id = e.organization_id AND a.installation_id = e.installation_id
 WHERE a.attempt_id=$1 AND a.organization_id=$2 AND a.installation_id=$3 ORDER BY e.id`, attemptID, org, inst)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ShopAttemptEvidenceRecord
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAttempts lists the tenant's attempts, newest first; openOnly limits it to non-terminal ones.
func (r *ShopAttemptRepository) ListAttempts(ctx context.Context, org, inst int64, openOnly bool, limit int) ([]ShopAttempt, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return r.list(ctx, `SELECT `+attemptCols+` FROM shop_delivery_attempts WHERE organization_id=$1 AND installation_id=$2
 AND (NOT $3 OR state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW')) ORDER BY id DESC LIMIT $4`, org, inst, openOnly, limit)
}

// NextAttemptNumber is the number the delivery's next attempt must carry (its history + 1).
func (r *ShopAttemptRepository) NextAttemptNumber(ctx context.Context, org, inst, deliveryID int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) + 1 FROM shop_delivery_attempts WHERE delivery_id=$1 AND organization_id=$2 AND installation_id=$3`, deliveryID, org, inst).Scan(&n)
	return n, err
}

// ShopCanaryBinding is the installation's bound game server as the plan validator needs it.
type ShopCanaryBinding struct {
	GameServerID     int64
	NitradoServiceID string
	MapKey           string
}

// CanaryBinding resolves installation -> game server -> Nitrado service id and the delivery map.
func (r *ShopAttemptRepository) CanaryBinding(ctx context.Context, org, inst int64) (ShopCanaryBinding, error) {
	var b ShopCanaryBinding
	err := r.pool.QueryRow(ctx, `SELECT gs.id, gs.provider_service_id, COALESCE(ds.map_key,'')
 FROM installations i JOIN game_servers gs ON gs.id = i.game_server_id
 LEFT JOIN shop_delivery_settings ds ON ds.installation_id = i.id AND ds.organization_id = i.organization_id
 WHERE i.id=$2 AND i.organization_id=$1`, org, inst).Scan(&b.GameServerID, &b.NitradoServiceID, &b.MapKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrShopCanaryBinding
	}
	return b, err
}

// ShopCanarySession is the game server's current boot session (boot authority, Live Sync).
type ShopCanarySession struct {
	ADMFile string
	EndedAt *time.Time
}

// CurrentBootSession reads server_adm_sessions for the installation's game server.
func (r *ShopAttemptRepository) CurrentBootSession(ctx context.Context, gameServerID int64) (ShopCanarySession, error) {
	var s ShopCanarySession
	err := r.pool.QueryRow(ctx, `SELECT adm_file, ended_at FROM server_adm_sessions WHERE server_id=$1`, gameServerID).Scan(&s.ADMFile, &s.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, ErrShopCanaryNoSession
	}
	return s, err
}
