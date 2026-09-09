package killfeed

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// PersistenceStore is the database surface the persistence worker needs.
// Implemented by the real repositories; faked in tests.
type PersistenceStore interface {
	UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error)
	InsertKill(ctx context.Context, k repository.KillRecord) error
	InsertDeath(ctx context.Context, d repository.DeathRecord) error
}

type KillAttributionResolver interface {
	ResolveKillAttribution(ctx context.Context, guildID, killerID, victimID int64, at time.Time) (killerFactionID, victimFactionID, seasonID, warID *int64)
}

type DeathSeasonResolver interface {
	ResolveDeathSeason(ctx context.Context, guildID int64, at time.Time) *int64
}

type KillIDStore interface {
	InsertKillReturning(ctx context.Context, k repository.KillRecord) (int64, error)
}

type KillPostProcessor interface {
	ProcessPersistedKill(ctx context.Context, killID int64, record repository.KillRecord, event *Event)
}
type ActivityRecorder interface {
	RecordConnect(context.Context, int64, int64, int64, time.Time) error
	RecordDisconnect(context.Context, int64, int64, int64, time.Time) error
}
type ActivityCheckpointer interface {
	CheckpointConnected(context.Context, int64, int64, time.Time) error
}

// PersistenceQueue is a bounded, ordered queue of events awaiting durable
// persistence before Discord publish. Overflow drops the oldest-eligible policy
// is deterministic: when full, the newest event is dropped and counted.
type PersistenceQueue struct {
	store    PersistenceStore
	guildID  int64
	serverID int64
	session  string

	mu        sync.Mutex
	queue     chan *Event
	dropped   int64
	persisted int64
	closed    chan struct{}
	done      chan struct{}

	// onKillPersisted is invoked after a kill is durably inserted. If the insert
	// is a duplicate (already persisted), it is NOT invoked — preventing reposts.
	onKillPersisted func(ev *Event)
	postProcessor   KillPostProcessor
	enqueued        int64
	highWater       int
	oldestAt        time.Time
}

func (q *PersistenceQueue) SetKillPostProcessor(processor KillPostProcessor) {
	q.postProcessor = processor
}

// maxPersistenceQueue bounds in-flight events so memory stays flat.
const maxPersistenceQueue = 500

// NewPersistenceQueue creates the bounded ordered queue.
func NewPersistenceQueue(store PersistenceStore, guildID int64, sessionID string) *PersistenceQueue {
	return &PersistenceQueue{
		store:   store,
		guildID: guildID,
		session: sessionID,
		queue:   make(chan *Event, maxPersistenceQueue),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
}
func NewPersistenceQueueWithServerID(store PersistenceStore, guildID, serverID int64, sessionID string) *PersistenceQueue {
	q := NewPersistenceQueue(store, guildID, sessionID)
	q.serverID = serverID
	return q
}

// SetKillPersistedHook registers the callback fired after a kill is durably
// persisted (and not a duplicate). This is where Discord publish happens.
func (q *PersistenceQueue) SetKillPersistedHook(fn func(ev *Event)) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onKillPersisted = fn
}
func (q *PersistenceQueue) Enqueue(ev *Event) bool {
	if q == nil || ev == nil {
		return false
	}
	// Stamp before sending to the channel. Assigning after enqueue creates a
	// race where the worker can consume the event before its tenant context is set.
	ev.GuildID = q.guildID
	ev.ServerID = q.serverID
	ev.SessionID = q.session
	select {
	case q.queue <- ev:
		q.mu.Lock()
		q.enqueued++
		if len(q.queue) > q.highWater {
			q.highWater = len(q.queue)
		}
		if q.oldestAt.IsZero() {
			q.oldestAt = time.Now()
		}
		q.mu.Unlock()
		return true
	default:
		q.mu.Lock()
		q.dropped++
		q.mu.Unlock()
		slog.Warn("component=killfeed", "msg", "persistence queue full; event dropped", "type", string(ev.Type))
		return false
	}
}

// Dropped returns the count of events dropped due to a full queue.
func (q *PersistenceQueue) Dropped() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Depth returns the current queue depth.
func (q *PersistenceQueue) Depth() int {
	return len(q.queue)
}

func (q *PersistenceQueue) QueueHealth() (depth, capacity, highWater int, dropped int64, oldestAge time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	depth = len(q.queue)
	capacity = cap(q.queue)
	highWater = q.highWater
	dropped = q.dropped
	if !q.oldestAt.IsZero() {
		oldestAge = time.Since(q.oldestAt)
	}
	return
}

// Run processes queued events in order until the queue is closed and drained.
func (q *PersistenceQueue) Run(ctx context.Context) {
	defer close(q.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev := <-q.queue:
			if ev == nil {
				continue
			}
			q.persistOne(ctx, ev)
			q.mu.Lock()
			if len(q.queue) == 0 {
				q.oldestAt = time.Time{}
			}
			q.mu.Unlock()
		case <-ticker.C:
			if checkpointer, ok := q.store.(ActivityCheckpointer); ok && q.serverID > 0 {
				_ = checkpointer.CheckpointConnected(ctx, q.guildID, q.serverID, time.Now().UTC())
			}
		case <-q.closed:
			// Drain remaining queued events before exit.
			for {
				select {
				case ev := <-q.queue:
					if ev != nil {
						q.persistOne(ctx, ev)
					}
				default:
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// persistOne persists a single event. Kills and deaths are persisted; other
// event types only upsert the player identity.
func (q *PersistenceQueue) persistOne(ctx context.Context, ev *Event) {
	if q.store == nil {
		return
	}

	// Persist based on event type. Only kills/deaths become durable records here;
	// all events upsert their player identities for last_seen tracking.
	switch ev.Type {
	case EventPlayerKill:
		killerID := q.upsertPlayer(ctx, ev.Killer)
		victimID := q.upsertPlayer(ctx, ev.Victim)
		var killerFactionID, victimFactionID, seasonID, warID *int64
		if resolver, ok := q.store.(KillAttributionResolver); ok {
			killerFactionID, victimFactionID, seasonID, warID = resolver.ResolveKillAttribution(ctx, q.guildID, killerID, victimID, eventTime(ev))
		}
		rec := repository.KillRecord{
			GuildID:         q.guildID,
			ServerID:        q.serverID,
			SessionID:       q.session,
			Fingerprint:     eventFingerprint(ev),
			KillerPlayerID:  killerID,
			VictimPlayerID:  victimID,
			KillerFactionID: killerFactionID,
			VictimFactionID: victimFactionID,
			SeasonID:        seasonID,
			WarID:           warID,
			WeaponRaw:       ev.Weapon,
			WeaponDisplay:   ev.Weapon,
			Distance:        ev.Distance,
			Headshot:        isHeadshotEvent(ev),
			KillStyle:       "",
			EventTime:       eventTimePtr(ev),
		}
		var killID int64
		var err error
		if inserter, ok := q.store.(KillIDStore); ok {
			killID, err = inserter.InsertKillReturning(ctx, rec)
		} else {
			err = q.store.InsertKill(ctx, rec)
		}
		if errors.Is(err, repository.ErrDuplicate) {
			// Durable dedupe: already persisted — do NOT publish again.
			slog.Debug("component=killfeed", "msg", "kill already persisted; skipping publish", "fingerprint", rec.Fingerprint)
			return
		}
		if err != nil {
			slog.Warn("component=killfeed", "msg", "kill persistence failed; not published", "err", err.Error())
			return
		}
		if q.postProcessor != nil && killID > 0 {
			q.postProcessor.ProcessPersistedKill(ctx, killID, rec, ev)
		}
		// Publish only after a successful, non-duplicate durable insert.
		q.mu.Lock()
		hook := q.onKillPersisted
		q.mu.Unlock()
		if hook != nil {
			hook(ev)
		}
		q.mu.Lock()
		q.persisted++
		q.mu.Unlock()
		return
	case EventPlayerDeath, EventSuicideAction:
		playerID := q.upsertPlayer(ctx, ev.Player)
		var seasonID *int64
		if resolver, ok := q.store.(DeathSeasonResolver); ok {
			seasonID = resolver.ResolveDeathSeason(ctx, q.guildID, eventTime(ev))
		}
		deathType := repository.DeathTypeUnknown
		if ev.Type == EventSuicideAction {
			deathType = repository.DeathTypeSuicide
		}
		rec := repository.DeathRecord{
			GuildID:     q.guildID,
			ServerID:    q.serverID,
			SessionID:   q.session,
			Fingerprint: eventFingerprint(ev),
			PlayerID:    playerID,
			SeasonID:    seasonID,
			DeathType:   deathType,
			EventTime:   eventTimePtr(ev),
		}
		if err := q.store.InsertDeath(ctx, rec); err != nil && !errors.Is(err, repository.ErrDuplicate) {
			slog.Warn("component=killfeed", "msg", "death persistence failed", "err", err.Error())
			return
		}
	default:
		// connect/disconnect/hit/etc: just track the player's identity/last_seen.
		q.upsertPlayer(ctx, ev.Player)
		q.upsertPlayer(ctx, ev.Victim)
		q.upsertPlayer(ctx, ev.Attacker)
		if recorder, ok := q.store.(ActivityRecorder); ok && (ev.Type == EventPlayerConnect || ev.Type == EventPlayerDisconnect) {
			playerID := q.upsertPlayer(ctx, ev.Player)
			at := eventTime(ev)
			if ev.Type == EventPlayerConnect {
				if q.serverID > 0 {
					_ = recorder.RecordConnect(ctx, q.guildID, q.serverID, playerID, at)
				}
			} else {
				if q.serverID > 0 {
					_ = recorder.RecordDisconnect(ctx, q.guildID, q.serverID, playerID, at)
				}
			}
		}
	}

	q.mu.Lock()
	q.persisted++
	q.mu.Unlock()
}

func (q *PersistenceQueue) upsertPlayer(ctx context.Context, p *PlayerRef) int64 {
	if p == nil || p.ID == "" {
		return 0
	}
	seen := time.Now()
	if p != nil && !q.sessionStart().IsZero() {
		seen = q.sessionStart()
	}
	id, err := q.store.UpsertPlayer(ctx, q.guildID, p.ID, p.Name, seen)
	if err != nil {
		slog.Debug("component=killfeed", "msg", "player upsert failed", "err", err.Error())
		return 0
	}
	return id
}

func (q *PersistenceQueue) sessionStart() time.Time { return time.Time{} }

// Close signals the worker to drain and stop.
func (q *PersistenceQueue) Close() {
	if q == nil {
		return
	}
	select {
	case <-q.closed:
	default:
		close(q.closed)
	}
	<-q.done
}

// eventFingerprint reuses the dedupe fingerprint as the durable dedupe key.
func eventFingerprint(ev *Event) string {
	return fingerprint(ev)
}

// isHeadshotEvent reports whether the event is a confirmed headshot.
func isHeadshotEvent(ev *Event) bool {
	return ev != nil && ev.HitZone == "Head"
}

func eventTimePtr(ev *Event) *time.Time {
	if ev == nil || ev.Timestamp.IsZero() {
		return nil
	}
	t := ev.Timestamp.UTC()
	return &t
}

func eventTime(ev *Event) time.Time {
	if ev == nil || ev.Timestamp.IsZero() {
		return time.Now().UTC()
	}
	return ev.Timestamp.UTC()
}
