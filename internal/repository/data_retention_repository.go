package repository

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DataRetentionRepository prunes append-only diagnostics tables (docs/PERFORMANCE.md "Data
// growth and retention"). It only ever deletes rows older than a cutoff, in bounded batches, from
// a table the caller names from its compile-time list - never kills, deaths, evidence, ledgers or
// anything else that feeds a case, a leaderboard or a payment.
type DataRetentionRepository struct{ pool *pgxpool.Pool }

func NewDataRetentionRepository(pool *pgxpool.Pool) *DataRetentionRepository {
	return &DataRetentionRepository{pool: pool}
}

// RetentionTable names one table and the timestamp column its age is measured by.
type RetentionTable struct {
	Table      string
	TimeColumn string
}

// retentionIdentifier is the only shape a table or column name may take: the names are
// interpolated into the statement (identifiers cannot be bound), so anything else is refused.
var retentionIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// MaxRetentionBatch bounds one DELETE so a sweep never holds a long lock or a huge transaction.
const MaxRetentionBatch = 5000

// PruneOlderThan deletes at most batch rows of t whose time column is before cutoff, oldest
// first, and returns how many it removed. The caller loops until it returns 0.
func (r *DataRetentionRepository) PruneOlderThan(ctx context.Context, t RetentionTable, cutoff time.Time, batch int) (int64, error) {
	if !retentionIdentifier.MatchString(t.Table) || !retentionIdentifier.MatchString(t.TimeColumn) {
		return 0, fmt.Errorf("data retention: refusing table %q column %q", t.Table, t.TimeColumn)
	}
	if batch <= 0 || batch > MaxRetentionBatch {
		batch = MaxRetentionBatch
	}
	q := fmt.Sprintf(`DELETE FROM %[1]s WHERE id IN (
  SELECT id FROM %[1]s WHERE %[2]s < $1 ORDER BY %[2]s LIMIT $2)`, t.Table, t.TimeColumn)
	tag, err := r.pool.Exec(ctx, q, cutoff, batch)
	if err != nil {
		return 0, fmt.Errorf("data retention %s: %w", t.Table, err)
	}
	return tag.RowsAffected(), nil
}
