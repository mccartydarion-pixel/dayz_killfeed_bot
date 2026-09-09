package repository

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

type AnomalyRepository struct{ pool *pgxpool.Pool }

func NewAnomalyRepository(pool *pgxpool.Pool) *AnomalyRepository {
	return &AnomalyRepository{pool: pool}
}
func (r *AnomalyRepository) ObservePair(ctx context.Context, guildID, seasonID, killerID, victimID int64, at time.Time) (bool, error) {
	var suspicious bool
	err := r.pool.QueryRow(ctx, `WITH prior AS (SELECT COALESCE(MAX(window_started_at),$5) AS first_at,COALESCE(MAX(window_ended_at),$5) AS last_at,COALESCE(MAX(occurrences),0) AS count FROM combat_anomaly_flags WHERE guild_id=$1 AND killer_player_id=$3 AND victim_player_id=$4 AND window_ended_at >= $5 - INTERVAL '10 minutes'), inserted AS (INSERT INTO combat_anomaly_flags(guild_id,season_id,killer_player_id,victim_player_id,flag_type,occurrences,window_started_at,window_ended_at) SELECT $1,NULLIF($2,0),$3,$4,'REPEATED_PAIR_KILLS',CASE WHEN prior.last_at >= $5-INTERVAL '10 minutes' THEN prior.count+1 ELSE 1 END,CASE WHEN prior.last_at >= $5-INTERVAL '10 minutes' THEN prior.first_at ELSE $5 END,$5 FROM prior RETURNING occurrences) SELECT occurrences>=5 FROM inserted`, guildID, seasonID, killerID, victimID, at).Scan(&suspicious)
	return suspicious, err
}
