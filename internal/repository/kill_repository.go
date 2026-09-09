package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// KillRepository persists authoritative PvP kills with durable dedupe.
type KillRepository struct {
	pool *pgxpool.Pool
}

// NewKillRepository creates a kill repository.
func NewKillRepository(pool *pgxpool.Pool) *KillRepository {
	return &KillRepository{pool: pool}
}

// KillRecord is one normalized, persisted PvP kill. No coordinates, raw ADM
// lines, tokens, or credentials are stored.
type KillRecord struct {
	GuildID         int64
	SessionID       string
	Fingerprint     string
	KillerPlayerID  int64
	VictimPlayerID  int64
	KillerFactionID *int64
	VictimFactionID *int64
	SeasonID        *int64
	WarID           *int64
	WeaponRaw       string
	WeaponDisplay   string
	Distance        *float64
	Headshot        bool
	KillStyle       string
	EventTime       *time.Time
}

// InsertKill persists a kill. Returns ErrDuplicate if the (guild, fingerprint)
// pair already exists — the durable dedupe that prevents reposts after restart.
func (r *KillRepository) InsertKill(ctx context.Context, k KillRecord) error {
	_, err := r.InsertKillReturning(ctx, k)
	return err
}

func (r *KillRepository) InsertKillReturning(ctx context.Context, k KillRecord) (int64, error) {
	const q = `
	INSERT INTO kills (guild_id, session_id, event_fingerprint, killer_player_id, victim_player_id,
	killer_faction_id, victim_faction_id, season_id, war_id, weapon_raw, weapon_display, distance, headshot, kill_style, event_time)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	RETURNING id`

	var id int64
	err := r.pool.QueryRow(ctx, q, k.GuildID, k.SessionID, k.Fingerprint,
		nilIfZero(k.KillerPlayerID), nilIfZero(k.VictimPlayerID), k.KillerFactionID, k.VictimFactionID, k.SeasonID, k.WarID,
		k.WeaponRaw, k.WeaponDisplay, k.Distance, k.Headshot, k.KillStyle, k.EventTime).Scan(&id)
	if isUniqueViolation(err) {
		return 0, ErrDuplicate
	}
	if err != nil {
		return 0, fmt.Errorf("insert kill: %w", err)
	}
	return id, nil
}

func nilIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
