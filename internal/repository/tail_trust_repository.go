package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// tailTrustMaxAge is how long saved tail-read trust stays usable without being refreshed by a
// passed recheck.
const tailTrustMaxAge = "7 days"

// TailTrustRepository stores verified Nitrado tail-read trust (nitrado.TailTrustStore).
type TailTrustRepository struct{ pool *pgxpool.Pool }

func NewTailTrustRepository(pool *pgxpool.Pool) *TailTrustRepository {
	return &TailTrustRepository{pool: pool}
}

var _ nitrado.TailTrustStore = (*TailTrustRepository)(nil)

func (r *TailTrustRepository) LoadTailTrust(ctx context.Context) (map[string]nitrado.TailTrustState, error) {
	rows, err := r.pool.Query(ctx, `SELECT service_id, matches, trusted FROM nitrado_tail_trust WHERE updated_at > NOW() - $1::interval`, tailTrustMaxAge)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]nitrado.TailTrustState{}
	for rows.Next() {
		var id string
		var st nitrado.TailTrustState
		if err := rows.Scan(&id, &st.Matches, &st.Trusted); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

func (r *TailTrustRepository) SaveTailTrust(ctx context.Context, serviceID string, s nitrado.TailTrustState) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO nitrado_tail_trust (service_id, matches, trusted, updated_at) VALUES ($1, $2, $3, NOW())
ON CONFLICT (service_id) DO UPDATE SET matches = EXCLUDED.matches, trusted = EXCLUDED.trusted, updated_at = NOW()`,
		serviceID, s.Matches, s.Trusted && !s.Disabled)
	return err
}
