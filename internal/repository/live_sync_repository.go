package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/dayz-killfeed/internal/livesync"
)

// LiveSyncRepository backs Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md): durable
// per-source checkpoints and the records read from RPT, script, crash and restart logs. It
// implements livesync.Store.
type LiveSyncRepository struct{ pool *pgxpool.Pool }

func NewLiveSyncRepository(pool *pgxpool.Pool) *LiveSyncRepository {
	return &LiveSyncRepository{pool: pool}
}

// LoadSources returns every stored source of one server.
func (r *LiveSyncRepository) LoadSources(ctx context.Context, guildID, serverID int64) ([]livesync.SourceState, error) {
	rows, err := r.pool.Query(ctx, `
SELECT family, source_file, remote_path, file_local_start, checkpoint_offset, backfill_until, read_size, active,
       attached_at, last_read_at, last_growth_at, records
FROM live_sync_sources WHERE guild_id=$1 AND server_id=$2 ORDER BY family, source_file`, guildID, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []livesync.SourceState
	for rows.Next() {
		var s livesync.SourceState
		if err := rows.Scan(&s.Family, &s.SourceFile, &s.RemotePath, &s.FileLocalStart, &s.Checkpoint, &s.BackfillUntil, &s.ReadSize,
			&s.Active, &s.AttachedAt, &s.LastReadAt, &s.LastGrowthAt, &s.Records); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CommitSource inserts the records (replays are no-ops by event id) and writes the source's
// checkpoint in ONE transaction, so the checkpoint never passes a record that is not durable. The
// stored record count is recomputed from what was actually inserted.
func (r *LiveSyncRepository) CommitSource(ctx context.Context, guildID, serverID int64, src livesync.SourceState, recs []livesync.StoredRecord) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted := 0
	if len(recs) > 0 {
		batch := &pgx.Batch{}
		for _, rec := range recs {
			payload, err := json.Marshal(rec.Payload)
			if err != nil {
				return 0, fmt.Errorf("encode payload: %w", err)
			}
			if rec.Payload == nil {
				payload = []byte("{}")
			}
			batch.Queue(`
INSERT INTO live_sync_records(server_id, guild_id, event_id, family, source_file, source_offset, category, status, delivery, boot_id,
    source_local_time, source_utc, visible_after, detected_at, payload, evidence, parser)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::timestamp,$12,$13,$14,$15,$16,$17)
ON CONFLICT (server_id, event_id) DO NOTHING`,
				serverID, guildID, rec.EventID, rec.Family, rec.SourceID, rec.Offset, rec.Category, rec.Status, rec.Delivery, rec.BootID,
				rec.SourceLocalTime, rec.SourceUTC, rec.VisibleAfter, rec.ObservedAt, payload, rec.Evidence, rec.Parser)
		}
		br := tx.SendBatch(ctx, batch)
		for range recs {
			tag, err := br.Exec()
			if err != nil {
				_ = br.Close()
				return 0, fmt.Errorf("insert live sync record: %w", err)
			}
			inserted += int(tag.RowsAffected())
		}
		if err := br.Close(); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO live_sync_sources(server_id, guild_id, family, source_file, remote_path, file_local_start, checkpoint_offset, backfill_until,
    read_size, active, attached_at, last_read_at, last_growth_at, records, updated_at)
VALUES($1,$2,$3,$4,$5,$6::timestamp,$7,$8,$9,$10,$11,$12,$13,$14,NOW())
ON CONFLICT (server_id, family, source_file) DO UPDATE SET
    guild_id=EXCLUDED.guild_id, remote_path=EXCLUDED.remote_path, checkpoint_offset=EXCLUDED.checkpoint_offset,
    backfill_until=EXCLUDED.backfill_until, read_size=EXCLUDED.read_size, active=EXCLUDED.active,
    last_read_at=EXCLUDED.last_read_at, last_growth_at=EXCLUDED.last_growth_at,
    records=live_sync_sources.records+$15, updated_at=NOW()`,
		serverID, guildID, src.Family, src.SourceFile, src.RemotePath, src.FileLocalStart, src.Checkpoint, src.BackfillUntil,
		src.ReadSize, src.Active, attachedAt(src.AttachedAt), src.LastReadAt, src.LastGrowthAt, int64(inserted), int64(inserted)); err != nil {
		return 0, fmt.Errorf("write live sync checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return inserted, nil
}

func attachedAt(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// SetServerUTCOffset records the server's UTC offset as stated by restart.log.
func (r *LiveSyncRepository) SetServerUTCOffset(ctx context.Context, guildID, serverID int64, minutes int, learnedFrom string) error {
	_, err := r.pool.Exec(ctx, `
INSERT INTO live_sync_server_clock(server_id, guild_id, utc_offset_minutes, learned_from, learned_at) VALUES($1,$2,$3,$4,NOW())
ON CONFLICT (server_id) DO UPDATE SET guild_id=EXCLUDED.guild_id, utc_offset_minutes=EXCLUDED.utc_offset_minutes,
    learned_from=EXCLUDED.learned_from, learned_at=NOW()`, serverID, guildID, minutes, learnedFrom)
	return err
}

// LiveSyncRecord is one stored record, for diagnostics and tests.
type LiveSyncRecord struct {
	ID              int64
	EventID         string
	Family          string
	SourceFile      string
	SourceOffset    int64
	Category        string
	Status          string
	Delivery        string
	BootID          string
	SourceLocalTime *time.Time
	SourceUTC       *time.Time
	VisibleAfter    *time.Time
	DetectedAt      time.Time
	PersistedAt     time.Time
	Payload         map[string]string
	Evidence        string
}

// RecentRecords returns a server's newest records, optionally for one family.
func (r *LiveSyncRepository) RecentRecords(ctx context.Context, guildID, serverID int64, family string, limit int) ([]LiveSyncRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.pool.Query(ctx, `
SELECT id, event_id, family, source_file, source_offset, category, status, delivery, boot_id, source_local_time, source_utc,
       visible_after, detected_at, persisted_at, payload, evidence
FROM live_sync_records WHERE guild_id=$1 AND server_id=$2 AND ($3='' OR family=$3)
ORDER BY id DESC LIMIT $4`, guildID, serverID, family, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveSyncRecord
	for rows.Next() {
		var rec LiveSyncRecord
		var payload []byte
		if err := rows.Scan(&rec.ID, &rec.EventID, &rec.Family, &rec.SourceFile, &rec.SourceOffset, &rec.Category, &rec.Status, &rec.Delivery,
			&rec.BootID, &rec.SourceLocalTime, &rec.SourceUTC, &rec.VisibleAfter, &rec.DetectedAt, &rec.PersistedAt, &payload, &rec.Evidence); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(payload, &rec.Payload)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// LiveSyncFamilyStats aggregates one family's stored records.
type LiveSyncFamilyStats struct {
	Family   string
	Records  int64
	Live     int64
	Backfill int64
	Unknown  int64
	// Latency over LIVE records with a known source UTC time, and persistence latency over all.
	EventToDetectP50Sec   *float64
	EventToDetectP95Sec   *float64
	EventSamples          int64
	DetectToPersistP50Ms  *float64
	DetectToPersistP95Ms  *float64
	VisibleWindowP50Sec   *float64
	VisibleWindowSamples  int64
	LastDetectedAt        *time.Time
	LastSourceLocalRecord *time.Time
}

// FamilyStats aggregates stored records per family since a point in time (diagnostics).
func (r *LiveSyncRepository) FamilyStats(ctx context.Context, guildID, serverID int64, since time.Time) ([]LiveSyncFamilyStats, error) {
	rows, err := r.pool.Query(ctx, `
SELECT family, COUNT(*),
       COUNT(*) FILTER (WHERE delivery='LIVE'), COUNT(*) FILTER (WHERE delivery='BACKFILL'), COUNT(*) FILTER (WHERE status='UNKNOWN'),
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM detected_at - source_utc)) FILTER (WHERE delivery='LIVE' AND source_utc IS NOT NULL),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM detected_at - source_utc)) FILTER (WHERE delivery='LIVE' AND source_utc IS NOT NULL),
       COUNT(*) FILTER (WHERE delivery='LIVE' AND source_utc IS NOT NULL),
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM persisted_at - detected_at) * 1000),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM persisted_at - detected_at) * 1000),
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM detected_at - visible_after)) FILTER (WHERE visible_after IS NOT NULL),
       COUNT(*) FILTER (WHERE visible_after IS NOT NULL),
       MAX(detected_at), MAX(source_local_time)
FROM live_sync_records WHERE guild_id=$1 AND server_id=$2 AND detected_at >= $3
GROUP BY family ORDER BY family`, guildID, serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveSyncFamilyStats
	for rows.Next() {
		var s LiveSyncFamilyStats
		if err := rows.Scan(&s.Family, &s.Records, &s.Live, &s.Backfill, &s.Unknown, &s.EventToDetectP50Sec, &s.EventToDetectP95Sec, &s.EventSamples,
			&s.DetectToPersistP50Ms, &s.DetectToPersistP95Ms, &s.VisibleWindowP50Sec, &s.VisibleWindowSamples, &s.LastDetectedAt, &s.LastSourceLocalRecord); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ADMLatencyStats measures the ADM path from stored location rows that carry a source time:
// DayZ time (source_local_time converted with the restart.log offset) -> Champion detection
// (observed_at, the ingestion time) -> persistence (created_at).
type ADMLatencyStats struct {
	Samples              int64
	EventToDetectP50Sec  *float64
	EventToDetectP95Sec  *float64
	EventToDetectMaxSec  *float64
	DetectToPersistP50Ms *float64
	DetectToPersistP95Ms *float64
}

func (r *LiveSyncRepository) ADMLatency(ctx context.Context, guildID, serverID int64, since time.Time) (ADMLatencyStats, error) {
	var s ADMLatencyStats
	err := r.pool.QueryRow(ctx, `
WITH c AS (SELECT utc_offset_minutes FROM live_sync_server_clock WHERE server_id=$2),
     l AS (SELECT e.observed_at, e.created_at,
                  (e.source_local_time - make_interval(mins => c.utc_offset_minutes)) AT TIME ZONE 'UTC' AS src
           FROM player_location_events e CROSS JOIN c
           WHERE e.guild_id=$1 AND e.server_id=$2 AND e.observed_at >= $3 AND e.source_local_time IS NOT NULL)
SELECT COUNT(*),
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM observed_at - src)),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM observed_at - src)),
       MAX(EXTRACT(EPOCH FROM observed_at - src))::float8,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM created_at - observed_at) * 1000),
       percentile_cont(0.95) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM created_at - observed_at) * 1000)
FROM l`, guildID, serverID, since).Scan(&s.Samples, &s.EventToDetectP50Sec, &s.EventToDetectP95Sec, &s.EventToDetectMaxSec,
		&s.DetectToPersistP50Ms, &s.DetectToPersistP95Ms)
	return s, err
}

// LiveSyncNoiseCategories are high-volume, low-value categories (engine start-up chatter, model
// warnings, unparsed lines) kept for a short window; every other record is kept longer.
var LiveSyncNoiseCategories = []string{
	livesync.CategoryUnknown, livesync.CategoryModelWarning, livesync.CategoryEngineStartup, livesync.CategoryLocalization,
	livesync.CategoryMissingModel, livesync.CategoryQuery, livesync.CategoryConfigWarning, livesync.CategoryLogHeader,
	livesync.CategoryScriptModule, livesync.CategorySpawnerConfig, livesync.CategoryCentralEconomy,
}

// PruneRecords deletes at most batch records older than their category's retention: noise
// categories before noiseBefore, everything else before allBefore. Returns the rows deleted.
func (r *LiveSyncRepository) PruneRecords(ctx context.Context, noiseBefore, allBefore time.Time, batch int) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM live_sync_records WHERE id IN (
    SELECT id FROM live_sync_records
    WHERE (category = ANY($1) AND detected_at < $2) OR detected_at < $3
    LIMIT $4)`, LiveSyncNoiseCategories, noiseBefore, allBefore, batch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CommandLineHeaderRecords counts stored RPT header records that still carry command-line evidence
// (diagnostics: must be 0 once migration 0052 has run and parser cls-1.2 is live).
func (r *LiveSyncRepository) CommandLineHeaderRecords(ctx context.Context, guildID, serverID int64) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM live_sync_records
WHERE guild_id=$1 AND server_id=$2 AND family='RPT' AND category='LOG_HEADER'
  AND (evidence LIKE '%-port=%' OR evidence LIKE '%-config=%' OR evidence LIKE '%-profiles=%' OR evidence LIKE '%.exe%')`, guildID, serverID).Scan(&n)
	return n, err
}
