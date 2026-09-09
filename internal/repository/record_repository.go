package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

const RecordLongestKill = "LONGEST_KILL"
const RecordMostKills = "MOST_KILLS"

// ServerRecord is the current guild record. NumericValue is the comparable value.
type ServerRecord struct {
	RecordType   string
	PlayerID     int64
	KillID       int64
	NumericValue float64
	TextValue    string
}

type RecordRepository struct{ pool *pgxpool.Pool }

func NewRecordRepository(pool *pgxpool.Pool) *RecordRepository { return &RecordRepository{pool: pool} }

// TryUpdateLongestKill atomically updates only when the new distance is greater.
// It returns the previous record, whether it changed, and the new record.
func (r *RecordRepository) TryUpdateLongestKill(ctx context.Context, guildID, playerID, killID int64, distance float64, text string) (*ServerRecord, bool, error) {
	if distance <= 0 {
		return nil, false, nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	var old ServerRecord
	err = tx.QueryRow(ctx, `SELECT COALESCE(player_id,0),COALESCE(kill_id,0),COALESCE(numeric_value,0),COALESCE(text_value,'') FROM server_records WHERE guild_id=$1 AND record_type=$2 FOR UPDATE`, guildID, RecordLongestKill).Scan(&old.PlayerID, &old.KillID, &old.NumericValue, &old.TextValue)
	if err != nil && err.Error() != "no rows in result set" {
		return nil, false, err
	}
	if err == nil && old.NumericValue >= distance {
		return &old, false, tx.Commit(ctx)
	}

	_, err = tx.Exec(ctx, `INSERT INTO server_records(guild_id,record_type,player_id,kill_id,numeric_value,text_value) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(guild_id,record_type) DO UPDATE SET player_id=EXCLUDED.player_id,kill_id=EXCLUDED.kill_id,numeric_value=EXCLUDED.numeric_value,text_value=EXCLUDED.text_value,updated_at=NOW()`, guildID, RecordLongestKill, playerID, killID, distance, text)
	if err != nil {
		return nil, false, fmt.Errorf("update longest record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return &old, true, nil
}
