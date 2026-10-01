package repository

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CaseShadowVerdictRepository stores staff verdicts on test-run findings and
// summarizes them. A verdict never releases a detector or acts on a player.
type CaseShadowVerdictRepository struct{ pool *pgxpool.Pool }

func NewCaseShadowVerdictRepository(pool *pgxpool.Pool) *CaseShadowVerdictRepository {
	return &CaseShadowVerdictRepository{pool: pool}
}

const (
	VerdictFalseAlarm = "FALSE_ALARM"
	VerdictSuspicious = "SUSPICIOUS"
)

var (
	ErrInvalidVerdict  = errors.New("invalid verdict")
	verdictIncidentKey = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ShadowVerdictDetector reports whether staff can review a detector's test run.
func ShadowVerdictDetector(id string) bool { return id == "CASE-LOGIN-001" || id == "CASE-BASE-001" }

type CaseVerdictScope struct{ InstallationID, GuildID, ServerID int64 }

func (s CaseVerdictScope) valid() bool { return s.InstallationID > 0 && s.GuildID > 0 && s.ServerID > 0 }

type CaseShadowVerdict struct {
	IncidentKey string    `json:"incidentKey"`
	PlayerID    int64     `json:"playerId"`
	Verdict     string    `json:"verdict"`
	Note        string    `json:"note,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CaseVerdictSummary is the review score for one detector on one server.
type CaseVerdictSummary struct {
	DetectorID    string     `json:"detectorId"`
	Reviewed      int        `json:"reviewed"`
	FalseAlarms   int        `json:"falseAlarms"`
	Suspicious    int        `json:"suspicious"`
	FirstReviewAt *time.Time `json:"firstReviewAt,omitempty"`
	LastReviewAt  *time.Time `json:"lastReviewAt,omitempty"`
}

// Save records (or changes) a verdict on one finding.
func (r *CaseShadowVerdictRepository) Save(ctx context.Context, s CaseVerdictScope, detectorID, incidentKey string, playerID int64, verdict, note string, reviewerUserID *int64) (CaseShadowVerdict, error) {
	note = strings.TrimSpace(note)
	if r == nil || r.pool == nil || !s.valid() || !ShadowVerdictDetector(detectorID) || !verdictIncidentKey.MatchString(incidentKey) ||
		playerID <= 0 || (verdict != VerdictFalseAlarm && verdict != VerdictSuspicious) || len([]rune(note)) > 300 {
		return CaseShadowVerdict{}, ErrInvalidVerdict
	}
	out := CaseShadowVerdict{IncidentKey: incidentKey}
	err := r.pool.QueryRow(ctx, `INSERT INTO case_shadow_verdicts
 (installation_id,guild_id,server_id,detector_id,incident_key,player_id,verdict,note,reviewer_user_id)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT (installation_id,detector_id,incident_key) DO UPDATE SET verdict=EXCLUDED.verdict,
  note=EXCLUDED.note,reviewer_user_id=EXCLUDED.reviewer_user_id,updated_at=NOW()
 WHERE case_shadow_verdicts.server_id=EXCLUDED.server_id AND case_shadow_verdicts.player_id=EXCLUDED.player_id
 RETURNING player_id,verdict,note,updated_at`,
		s.InstallationID, s.GuildID, s.ServerID, detectorID, incidentKey, playerID, verdict, note, reviewerUserID).
		Scan(&out.PlayerID, &out.Verdict, &out.Note, &out.UpdatedAt)
	if err != nil {
		return CaseShadowVerdict{}, err
	}
	return out, nil
}

// Summary returns the review score and the newest verdicts for one detector.
func (r *CaseShadowVerdictRepository) Summary(ctx context.Context, s CaseVerdictScope, detectorID string, limit int) (CaseVerdictSummary, []CaseShadowVerdict, error) {
	sum := CaseVerdictSummary{DetectorID: detectorID}
	if r == nil || r.pool == nil || !s.valid() || !ShadowVerdictDetector(detectorID) {
		return sum, nil, ErrInvalidVerdict
	}
	if limit < 1 || limit > 500 {
		limit = 200
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(*) FILTER (WHERE verdict='FALSE_ALARM'),
  COUNT(*) FILTER (WHERE verdict='SUSPICIOUS'),MIN(created_at),MAX(updated_at)
 FROM case_shadow_verdicts WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND detector_id=$4`,
		s.InstallationID, s.GuildID, s.ServerID, detectorID).
		Scan(&sum.Reviewed, &sum.FalseAlarms, &sum.Suspicious, &sum.FirstReviewAt, &sum.LastReviewAt); err != nil {
		return sum, nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT incident_key,player_id,verdict,note,updated_at FROM case_shadow_verdicts
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND detector_id=$4
 ORDER BY updated_at DESC,id DESC LIMIT $5`, s.InstallationID, s.GuildID, s.ServerID, detectorID, limit)
	if err != nil {
		return sum, nil, err
	}
	defer rows.Close()
	out := make([]CaseShadowVerdict, 0)
	for rows.Next() {
		var v CaseShadowVerdict
		if err := rows.Scan(&v.IncidentKey, &v.PlayerID, &v.Verdict, &v.Note, &v.UpdatedAt); err != nil {
			return sum, nil, err
		}
		out = append(out, v)
	}
	return sum, out, rows.Err()
}
