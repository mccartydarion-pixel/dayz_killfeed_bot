package killfeed

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Phase 3 (docs/PLAYER_INTELLIGENCE.md): a bounded, batched, off-hot-path location-event
// pipeline. ADM parsing already extracts Position{X,Y,Z} on any event whose metadata block
// includes "pos=<...>" (parser.go's posRe) but it was previously read only transiently (kill
// distance, optional embed coordinates) and never persisted. This file adds that persistence path
// as a fully separate, additive component - internal/killfeed/persistence.go's PersistenceQueue
// (kills/deaths/connect/disconnect durable records) is completely untouched.
//
// The critical difference from PersistenceQueue: that queue's EnqueueAndWait blocks processLine
// until the database write completes (or times out) - acceptable there because a kill/death must
// be durable before Discord publish. A location event has no such ordering requirement, and the
// task's own instruction is explicit: "Do NOT add synchronous DB writes directly into the parser
// loop if it can stall ADM processing." LocationQueue.EnqueueEvent is therefore always
// fire-and-forget - a full queue drops the newest candidate and counts it, exactly like
// PersistenceQueue's own overflow policy, but processLine is never blocked waiting for the result.

// LocationStore is the database surface the location worker needs. UpsertPlayer intentionally
// reuses the exact PersistenceStore method signature (same repository, same idempotent
// ON CONFLICT upsert) - a location event never needs its own player-identity logic.
type LocationStore interface {
	UpsertPlayer(ctx context.Context, guildID int64, dayzID, displayName string, seenAt time.Time) (int64, error)
	// InsertLocationEvents durably inserts a batch in the given order, deduplicating a repeat ADM
	// line via ON CONFLICT DO NOTHING against the (player_id, server_id, event_type, observed_at)
	// constraint (task section 14: "no duplicate location events after ADM replay"). inserted is
	// the count of rows actually written (excluding conflicts), for the events_persisted metric.
	InsertLocationEvents(ctx context.Context, events []repository.LocationEventInput) (inserted int, err error)
}

// Location event type vocabulary (task section 3), independent of killfeed.EventType - a location
// event's own classification is coarser (e.g. PLAYER_DEATH and SUICIDE_ACTION both map to DEATH).
const (
	LocationEventConnect     = "CONNECT"
	LocationEventDisconnect  = "DISCONNECT"
	LocationEventHit         = "HIT"
	LocationEventKill        = "KILL"
	LocationEventDeath       = "DEATH"
	LocationEventRespawn     = "RESPAWN"
	LocationEventUnconscious = "UNCONSCIOUS"
	LocationEventOther       = "OTHER_ADM"
)

// locationEventTypeFor maps an Engine EventType to the location_event_type vocabulary above.
func locationEventTypeFor(t EventType) string {
	switch t {
	case EventPlayerConnect:
		return LocationEventConnect
	case EventPlayerDisconnect:
		return LocationEventDisconnect
	case EventPlayerHit:
		return LocationEventHit
	case EventPlayerKill:
		return LocationEventKill
	case EventPlayerDeath, EventSuicideAction:
		return LocationEventDeath
	case EventPlayerRespawn:
		return LocationEventRespawn
	case EventPlayerUnconscious:
		return LocationEventUnconscious
	default:
		return LocationEventOther
	}
}

type locationCandidate struct {
	player     *PlayerRef
	eventType  string
	observedAt time.Time
}

// maxLocationQueue bounds in-flight location candidates so memory stays flat (task section 4:
// "bounded queue"). Larger than PersistenceQueue's 500: a candidate is produced for every
// position-carrying event (connect/disconnect/hit/kill/death/etc.), not just kill/death/
// connect/disconnect, so throughput is higher per poll.
const maxLocationQueue = 2000

// locationBatchSize/locationBatchInterval bound how long candidates wait before one batched
// insert (task section 4: "batched insert") - whichever limit is hit first triggers a flush, so a
// quiet period never leaves candidates sitting unpersisted indefinitely.
const (
	locationBatchSize     = 100
	locationBatchInterval = 500 * time.Millisecond
)

// LocationQueue is a bounded, per-server queue of location-event candidates awaiting durable,
// batched persistence, entirely off the ADM parser's hot path. One instance per server (task
// section 4: "per-installation isolation"), mirroring PersistenceQueue's own per-server scoping.
type LocationQueue struct {
	store    LocationStore
	guildID  int64
	serverID int64
	// intrusion is the optional Phase 4 zone/UAV/Base Radar engine (docs/ZONES_UAV_RADAR.md) - nil-
	// safe throughout (Evaluate is a no-op on a nil engine), so a queue that never had one attached
	// behaves exactly as before this feature existed. Runs synchronously, right after this batch's
	// events were durably persisted, on the exact same consumer goroutine - still fully off the ADM
	// parser's hot path (that separation is LocationQueue's own reason to exist), just an additional
	// step of the same already-async pipeline.
	intrusion *IntrusionEngine

	queue  chan locationCandidate
	closed chan struct{}
	done   chan struct{}

	mu          sync.Mutex
	seen        int64
	persisted   int64
	dropped     int64
	highWater   int
	lastWriteMs int64
}

// NewLocationQueue creates the bounded, per-server location-event queue. A nil store is valid
// (Run then does nothing but drain) - matches PersistenceQueue's own defensive-nil style.
func NewLocationQueue(store LocationStore, guildID, serverID int64) *LocationQueue {
	return &LocationQueue{
		store:    store,
		guildID:  guildID,
		serverID: serverID,
		queue:    make(chan locationCandidate, maxLocationQueue),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetIntrusionEngine attaches the Phase 4 zone/UAV/Base Radar intrusion engine. Must be called
// before Run starts consuming (matches NewLocationQueue's other one-shot setup fields) - safe to
// leave unset entirely, in which case persist() never evaluates zones.
func (q *LocationQueue) SetIntrusionEngine(engine *IntrusionEngine) {
	if q == nil {
		return
	}
	q.intrusion = engine
}

// ServerID returns the game_servers row this queue is scoped to (0 if unset).
func (q *LocationQueue) ServerID() int64 {
	if q == nil {
		return 0
	}
	return q.serverID
}

// EnqueueEvent submits every PlayerRef on ev that carries a non-nil Position as a separate
// location-event candidate (a kill can carry both a killer and a victim position - each becomes
// its own row). Never blocks the caller: on a full queue the candidate is dropped and counted.
func (q *LocationQueue) EnqueueEvent(ev *Event) {
	if q == nil || ev == nil {
		return
	}
	eventType := locationEventTypeFor(ev.Type)
	at := eventTime(ev)
	for _, ref := range []*PlayerRef{ev.Player, ev.Victim, ev.Killer, ev.Attacker} {
		if ref == nil || ref.Position == nil || ref.ID == "" {
			continue
		}
		q.enqueue(locationCandidate{player: ref, eventType: eventType, observedAt: at})
	}
}

func (q *LocationQueue) enqueue(c locationCandidate) {
	q.mu.Lock()
	q.seen++
	q.mu.Unlock()
	select {
	case q.queue <- c:
		q.mu.Lock()
		if len(q.queue) > q.highWater {
			q.highWater = len(q.queue)
		}
		q.mu.Unlock()
	default:
		q.mu.Lock()
		q.dropped++
		q.mu.Unlock()
		slog.Debug("component=location", "msg", "location queue full; candidate dropped", "server_id", q.serverID)
	}
}

// Run consumes queued candidates, batching them into bounded, periodic inserts until the queue is
// closed and drained. Within one batch, candidates are inserted in the exact order they were
// enqueued (task section 4: "no event reordering where order matters") - a single goroutine both
// reads the channel and builds the batch, so ordering is structural, not a property that needs
// separate enforcement.
func (q *LocationQueue) Run(ctx context.Context) {
	defer close(q.done)
	ticker := time.NewTicker(locationBatchInterval)
	defer ticker.Stop()
	batch := make([]locationCandidate, 0, locationBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		q.persist(ctx, batch)
		batch = batch[:0]
	}
	for {
		select {
		case c := <-q.queue:
			batch = append(batch, c)
			if len(batch) >= locationBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-q.closed:
			// Drain remaining queued candidates before exit (task section 4: "shutdown flush").
			for {
				select {
				case c := <-q.queue:
					batch = append(batch, c)
					if len(batch) >= locationBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// persist upserts each distinct player in the batch (once per player, not once per candidate -
// cheap since a batch typically has few distinct players across many events) then inserts every
// candidate as one ordered batch. A player-upsert failure drops just that player's candidates,
// never the whole batch.
func (q *LocationQueue) persist(ctx context.Context, batch []locationCandidate) {
	if q.store == nil || len(batch) == 0 {
		return
	}
	start := time.Now()
	records := make([]repository.LocationEventInput, 0, len(batch))
	resolved := make(map[string]int64, len(batch))
	for _, c := range batch {
		playerID, ok := resolved[c.player.ID]
		if !ok {
			id, err := q.store.UpsertPlayer(ctx, q.guildID, c.player.ID, c.player.Name, c.observedAt)
			if err != nil {
				slog.Debug("component=location", "msg", "player upsert failed", "err", err.Error())
				resolved[c.player.ID] = 0
				continue
			}
			playerID = id
			resolved[c.player.ID] = id
		}
		if playerID == 0 {
			continue
		}
		// ADM prints <x, z, altitude> (see Position), so map x/z/y come from the accessors, never
		// the raw fields - the heatmaps and zone distance checks all key on x/z.
		pos := *c.player.Position
		y := pos.Altitude()
		records = append(records, repository.LocationEventInput{
			GuildID: q.guildID, ServerID: q.serverID, PlayerID: playerID, Gamertag: c.player.Name,
			X: pos.MapX(), Z: pos.MapZ(), Y: &y,
			EventType: c.eventType, ObservedAt: c.observedAt, Source: "ADM",
		})
	}
	if len(records) == 0 {
		return
	}
	inserted, err := q.store.InsertLocationEvents(ctx, records)
	elapsed := time.Since(start)
	q.mu.Lock()
	q.lastWriteMs = elapsed.Milliseconds()
	if err != nil {
		q.dropped += int64(len(records))
		q.mu.Unlock()
		slog.Debug("component=location", "msg", "location batch insert failed", "err", err.Error(), "batch_size", len(records))
		return
	}
	q.persisted += int64(inserted)
	q.mu.Unlock()

	// Phase 4 zone/UAV/Base Radar intrusion evaluation (docs/ZONES_UAV_RADAR.md) - only after this
	// batch is confirmed durable, still on this same off-hot-path goroutine. A panic here must never
	// take down the location worker, exactly like every other fire-and-forget consumer in this
	// package (HitPublisher, ConnectionPublisher).
	if q.intrusion != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("component=location", "msg", "intrusion engine panic recovered", "panic", r)
				}
			}()
			locs := make([]locationRecord, 0, len(records))
			for _, rec := range records {
				locs = append(locs, locationRecord{
					GuildID: rec.GuildID, ServerID: rec.ServerID, PlayerID: rec.PlayerID, Gamertag: rec.Gamertag,
					X: rec.X, Z: rec.Z, ObservedAt: rec.ObservedAt,
				})
			}
			q.intrusion.Evaluate(ctx, q.serverID, locs)
		}()
	}
}

// LocationQueueHealth is a point-in-time snapshot for observability (task section 12).
type LocationQueueHealth struct {
	ServerID    int64
	Depth       int
	Capacity    int
	HighWater   int
	Seen        int64
	Persisted   int64
	Dropped     int64
	LastWriteMs int64
}

// Health returns a snapshot of this queue's counters. Safe on a nil queue.
func (q *LocationQueue) Health() LocationQueueHealth {
	if q == nil {
		return LocationQueueHealth{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return LocationQueueHealth{
		ServerID: q.serverID, Depth: len(q.queue), Capacity: cap(q.queue), HighWater: q.highWater,
		Seen: q.seen, Persisted: q.persisted, Dropped: q.dropped, LastWriteMs: q.lastWriteMs,
	}
}

// Close signals the worker to drain and stop, flushing any partially-built or still-queued batch
// first (task section 4: "shutdown flush" - a process restart must never silently lose already-
// enqueued location candidates that haven't been batched yet).
func (q *LocationQueue) Close() {
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
