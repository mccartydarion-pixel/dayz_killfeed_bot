package killfeed

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Feed latency (docs/PERFORMANCE.md section 19): how long one kill or death card takes from the
// DayZ server writing its ADM line to Discord accepting the post, split into the stages the bot
// can know. Observability only: nothing here decides, delays or reorders anything, and no player
// name or message content is ever stored or logged.
//
// The stages of one card:
//
//	game log  ->  bot read  ->  queued  ->  Discord accepted
//
//   - game log: the line's own clock (server-local) converted to UTC with the server's learned UTC
//     offset. Unknown offset = unknown, never guessed; the first stage is then simply absent.
//   - bot read: the bot finished downloading the bytes that hold the line.
//   - queued: the event was stored in the database and its card handed to the Discord feed.
//   - Discord accepted: Discord answered the post with success.

const (
	// feedLatencyMaxSamples bounds one server feed's memory: the newest cards only. A feed with
	// more cards than this in 24 hours reports its "last 24 hours" over the newest of them.
	feedLatencyMaxSamples = 4096
	// feedLatencyLogEvery is how often a feed with traffic logs its summary line.
	feedLatencyLogEvery = 5 * time.Minute
	// gameClockTolerance is how far a line's converted time may sit in the future of the read
	// before it is treated as a wrong clock instead of a rounding difference (ADM times have
	// one-second resolution and the game server's clock is not the bot's).
	gameClockTolerance = 30 * time.Second
	// gameClockMaxAge rejects a converted time older than this: a wrong date, not a delay.
	gameClockMaxAge = 24 * time.Hour
)

// FeedTiming carries one card's stage timestamps from the engine to the Discord feed.
type FeedTiming struct {
	ServerID int64
	// LoggedAt is when the game wrote the line, in UTC. Zero when the server's UTC offset is not
	// known or the line carries no usable time.
	LoggedAt time.Time
	// ReadAt is when the bot finished downloading the bytes that hold the line.
	ReadAt time.Time
	// ParsedAt is when the line was parsed (Event.DetectedAt). It stands in for ReadAt on cards
	// restored after a restart, which keep only this.
	ParsedAt time.Time
}

// TimingOf returns ev's stage timestamps.
func TimingOf(ev *Event) FeedTiming {
	if ev == nil {
		return FeedTiming{}
	}
	return FeedTiming{ServerID: ev.ServerID, LoggedAt: ev.LoggedAt, ReadAt: ev.ReadAt, ParsedAt: ev.DetectedAt}
}

// latencySample is one delivered card. Durations are milliseconds; -1 = not known.
type latencySample struct {
	at             time.Time // Discord accepted
	logToRead      int64
	readToQueued   int64
	queuedToPosted int64
	readToPosted   int64
	logToPosted    int64
}

type feedLatencySeries struct {
	ring          []latencySample
	next          int
	delivered     int64
	clockUnknown  int64
	clockRejected int64
	lastAt        time.Time
	sinceLog      int
	lastLog       time.Time
}

type feedLatencyKey struct {
	serverID int64
	feed     string
}

// FeedLatencyRegistry keeps rolling per-server, per-feed latency samples in memory.
type FeedLatencyRegistry struct {
	mu     sync.Mutex
	series map[feedLatencyKey]*feedLatencySeries
	now    func() time.Time
}

// FeedLatency is the process-wide registry.
var FeedLatency = NewFeedLatencyRegistry()

// NewFeedLatencyRegistry returns an empty registry.
func NewFeedLatencyRegistry() *FeedLatencyRegistry {
	return &FeedLatencyRegistry{series: map[feedLatencyKey]*feedLatencySeries{}, now: time.Now}
}

func msBetween(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() {
		return -1
	}
	d := to.Sub(from).Milliseconds()
	if d < 0 {
		d = 0
	}
	return d
}

// Record stores one delivered card. feed is the feed's name (KILLFEED, DEATH_FEED); queuedAt is
// when the card entered the Discord feed's queue; postedAt is when Discord accepted it.
func (r *FeedLatencyRegistry) Record(feed string, t FeedTiming, queuedAt, postedAt time.Time) {
	if r == nil || feed == "" || postedAt.IsZero() {
		return
	}
	readAt := t.ReadAt
	if readAt.IsZero() {
		readAt = t.ParsedAt
	}
	s := latencySample{at: postedAt, logToRead: -1, logToPosted: -1,
		readToQueued: msBetween(readAt, queuedAt), queuedToPosted: msBetween(queuedAt, postedAt), readToPosted: msBetween(readAt, postedAt)}
	clockUnknown, clockRejected := t.LoggedAt.IsZero(), false
	if !clockUnknown && !readAt.IsZero() {
		if lag := readAt.Sub(t.LoggedAt); lag < -gameClockTolerance || lag > gameClockMaxAge {
			clockRejected = true
		} else {
			s.logToRead = msBetween(t.LoggedAt, readAt)
			s.logToPosted = msBetween(t.LoggedAt, postedAt)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	key := feedLatencyKey{t.ServerID, feed}
	fs := r.series[key]
	if fs == nil {
		fs = &feedLatencySeries{lastLog: r.now()}
		r.series[key] = fs
	}
	if len(fs.ring) < feedLatencyMaxSamples {
		fs.ring = append(fs.ring, s)
	} else {
		fs.ring[fs.next] = s
		fs.next = (fs.next + 1) % feedLatencyMaxSamples
	}
	fs.delivered++
	fs.sinceLog++
	fs.lastAt = postedAt
	switch {
	case clockRejected:
		fs.clockRejected++
	case clockUnknown:
		fs.clockUnknown++
	}
}

// StageStats summarises one stage over a window, in milliseconds.
type StageStats struct {
	Samples int   `json:"samples"`
	P50Ms   int64 `json:"p50Ms"`
	P90Ms   int64 `json:"p90Ms"`
	P99Ms   int64 `json:"p99Ms"`
	MaxMs   int64 `json:"maxMs"`
}

func stageStats(values []int64) StageStats {
	if len(values) == 0 {
		return StageStats{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	// Nearest rank: the smallest value with at least p of the samples at or below it.
	at := func(p float64) int64 {
		i := int(p*float64(len(values))+0.999999) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(values) {
			i = len(values) - 1
		}
		return values[i]
	}
	return StageStats{Samples: len(values), P50Ms: at(0.50), P90Ms: at(0.90), P99Ms: at(0.99), MaxMs: values[len(values)-1]}
}

// FeedLatencyWindow is every stage of one feed over one time window.
type FeedLatencyWindow struct {
	// Cards is how many delivered cards the window holds.
	Cards int `json:"cards"`
	// GameLogToBotReadMs: the game wrote the line -> the bot had downloaded it. Nitrado's own
	// delay in exposing the log plus the bot's polling. Only cards whose server clock is known.
	GameLogToBotReadMs StageStats `json:"gameLogToBotReadMs"`
	// BotReadToQueuedMs: downloaded -> stored in the database and queued for Discord (parsing,
	// earlier lines of the same download, database work).
	BotReadToQueuedMs StageStats `json:"botReadToQueuedMs"`
	// QueuedToDiscordAcceptedMs: queued -> Discord accepted the post (the feed's own wait - up to a
	// whole cycle in rotating mode - plus the Discord request).
	QueuedToDiscordAcceptedMs StageStats `json:"queuedToDiscordAcceptedMs"`
	// BotReadToDiscordAcceptedMs: everything the bot controls.
	BotReadToDiscordAcceptedMs StageStats `json:"botReadToDiscordAcceptedMs"`
	// GameLogToDiscordAcceptedMs: the whole trip. Only cards whose server clock is known.
	GameLogToDiscordAcceptedMs StageStats `json:"gameLogToDiscordAcceptedMs"`
}

// FeedLatencySnapshot is one server feed's latency.
type FeedLatencySnapshot struct {
	ServerID int64  `json:"serverId"`
	Feed     string `json:"feed"`
	// DeliveredSinceStart counts every card recorded since this process started.
	DeliveredSinceStart int64 `json:"deliveredSinceStart"`
	// CardsWithoutServerClock were delivered while the server's UTC offset was unknown: they have
	// no game-log stage. CardsWithImplausibleServerClock had a converted time in the future of the
	// read or more than a day old and were left out of the game-log stages the same way.
	CardsWithoutServerClock         int64             `json:"cardsWithoutServerClock"`
	CardsWithImplausibleServerClock int64             `json:"cardsWithImplausibleServerClock"`
	LastDeliveredAt                 *time.Time        `json:"lastDeliveredAt,omitempty"`
	LastHour                        FeedLatencyWindow `json:"lastHour"`
	Last24Hours                     FeedLatencyWindow `json:"last24Hours"`
}

func (fs *feedLatencySeries) window(cutoff time.Time) FeedLatencyWindow {
	var logRead, readQueued, queuedPosted, readPosted, logPosted []int64
	add := func(dst *[]int64, v int64) {
		if v >= 0 {
			*dst = append(*dst, v)
		}
	}
	cards := 0
	for _, s := range fs.ring {
		if s.at.Before(cutoff) {
			continue
		}
		cards++
		add(&logRead, s.logToRead)
		add(&readQueued, s.readToQueued)
		add(&queuedPosted, s.queuedToPosted)
		add(&readPosted, s.readToPosted)
		add(&logPosted, s.logToPosted)
	}
	return FeedLatencyWindow{Cards: cards, GameLogToBotReadMs: stageStats(logRead), BotReadToQueuedMs: stageStats(readQueued),
		QueuedToDiscordAcceptedMs: stageStats(queuedPosted), BotReadToDiscordAcceptedMs: stageStats(readPosted),
		GameLogToDiscordAcceptedMs: stageStats(logPosted)}
}

func (fs *feedLatencySeries) snapshot(key feedLatencyKey, now time.Time) FeedLatencySnapshot {
	out := FeedLatencySnapshot{ServerID: key.serverID, Feed: key.feed, DeliveredSinceStart: fs.delivered,
		CardsWithoutServerClock: fs.clockUnknown, CardsWithImplausibleServerClock: fs.clockRejected,
		LastHour: fs.window(now.Add(-time.Hour)), Last24Hours: fs.window(now.Add(-24 * time.Hour))}
	if !fs.lastAt.IsZero() {
		at := fs.lastAt.UTC()
		out.LastDeliveredAt = &at
	}
	return out
}

// Snapshot returns every server feed's latency, sorted by server then feed.
func (r *FeedLatencyRegistry) Snapshot() []FeedLatencySnapshot {
	return r.snapshot(func(feedLatencyKey) bool { return true })
}

// SnapshotServer returns one server's feeds.
func (r *FeedLatencyRegistry) SnapshotServer(serverID int64) []FeedLatencySnapshot {
	return r.snapshot(func(k feedLatencyKey) bool { return k.serverID == serverID })
}

func (r *FeedLatencyRegistry) snapshot(keep func(feedLatencyKey) bool) []FeedLatencySnapshot {
	out := []FeedLatencySnapshot{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for key, fs := range r.series {
		if keep(key) {
			out = append(out, fs.snapshot(key, now))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ServerID != out[j].ServerID {
			return out[i].ServerID < out[j].ServerID
		}
		return out[i].Feed < out[j].Feed
	})
	return out
}

// LogIfDue writes one summary line per feed of serverID that delivered cards since its last line,
// at most every feedLatencyLogEvery. The engine calls it after each poll cycle. Numbers only.
func (r *FeedLatencyRegistry) LogIfDue(serverID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	now := r.now()
	var due []FeedLatencySnapshot
	for key, fs := range r.series {
		if key.serverID != serverID || fs.sinceLog == 0 || now.Sub(fs.lastLog) < feedLatencyLogEvery {
			continue
		}
		fs.sinceLog, fs.lastLog = 0, now
		due = append(due, fs.snapshot(key, now))
	}
	r.mu.Unlock()
	for _, s := range due {
		h := s.LastHour
		slog.Info("component=killfeed", "event", "feed_latency", "server_id", s.ServerID, "feed", s.Feed, "window", "1h",
			"cards", h.Cards, "cards_24h", s.Last24Hours.Cards,
			"game_log_to_discord_samples", h.GameLogToDiscordAcceptedMs.Samples,
			"game_log_to_discord_p50_ms", h.GameLogToDiscordAcceptedMs.P50Ms, "game_log_to_discord_p90_ms", h.GameLogToDiscordAcceptedMs.P90Ms,
			"game_log_to_discord_p99_ms", h.GameLogToDiscordAcceptedMs.P99Ms, "game_log_to_discord_max_ms", h.GameLogToDiscordAcceptedMs.MaxMs,
			"game_log_to_bot_read_p50_ms", h.GameLogToBotReadMs.P50Ms, "game_log_to_bot_read_p90_ms", h.GameLogToBotReadMs.P90Ms,
			"bot_read_to_queued_p50_ms", h.BotReadToQueuedMs.P50Ms, "bot_read_to_queued_p90_ms", h.BotReadToQueuedMs.P90Ms,
			"queued_to_discord_p50_ms", h.QueuedToDiscordAcceptedMs.P50Ms, "queued_to_discord_p90_ms", h.QueuedToDiscordAcceptedMs.P90Ms,
			"bot_read_to_discord_p50_ms", h.BotReadToDiscordAcceptedMs.P50Ms, "bot_read_to_discord_p90_ms", h.BotReadToDiscordAcceptedMs.P90Ms,
			"bot_read_to_discord_p99_ms", h.BotReadToDiscordAcceptedMs.P99Ms, "bot_read_to_discord_max_ms", h.BotReadToDiscordAcceptedMs.MaxMs,
			"cards_without_server_clock", s.CardsWithoutServerClock)
	}
}
