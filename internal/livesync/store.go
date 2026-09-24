package livesync

import (
	"context"
	"time"
)

// Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md): the durable state behind the per-source
// watchers. *repository.LiveSyncRepository implements Store.

// Delivery separates bytes Champion observed appear from history it read late.
const (
	DeliveryLive     = "LIVE"     // the bytes appeared while the source was watched, and are not late
	DeliveryBackfill = "BACKFILL" // existed before the watcher attached, or older than LateAfter when read
)

// SourceState is one watched source file's durable checkpoint and read history.
type SourceState struct {
	Family         string
	SourceFile     string // canonical identity (CanonicalSourceID)
	RemotePath     string // the physical path last read (either mount)
	FileLocalStart *time.Time
	// Checkpoint is the end of the last complete line whose records are committed.
	Checkpoint int64
	// BackfillUntil: records ending at or before it existed when the source was first attached.
	BackfillUntil int64
	ReadSize      int64 // file size at the last successful read
	Active        bool  // the current file of its family
	AttachedAt    time.Time
	LastReadAt    *time.Time
	LastGrowthAt  *time.Time
	Records       int64
}

// StoredRecord is one envelope with its delivery classification and latency evidence.
type StoredRecord struct {
	Envelope
	Delivery string
	// VisibleAfter is the previous read of the source, which did not yet contain these bytes: the
	// bytes became visible through Nitrado in (VisibleAfter, ObservedAt]. Nil when unknown.
	VisibleAfter *time.Time
}

// Store persists source checkpoints and records. CommitSource must write the records and advance
// the checkpoint in ONE transaction: a failure leaves both unchanged, so the same bytes are read
// again and the deterministic event ids make the retry a no-op.
type Store interface {
	LoadSources(ctx context.Context, guildID, serverID int64) ([]SourceState, error)
	CommitSource(ctx context.Context, guildID, serverID int64, src SourceState, recs []StoredRecord) (inserted int, err error)
	SetServerUTCOffset(ctx context.Context, guildID, serverID int64, minutes int, learnedFrom string) error
}

// SessionEnder ends the server's current ADM boot session on DayZ-written evidence of a later boot
// or a completed shutdown (*repository.LocationRepository implements it).
type SessionEnder interface {
	EndADMSessionBefore(ctx context.Context, guildID, serverID int64, bootLocal time.Time, reason, evidence string) (bool, error)
}
