package livesync

import (
	"context"
	"log/slog"
	"sort"
	"time"
)

// Source freshness states. Freshness describes how current Champion's KNOWLEDGE of a source is
// (when it last successfully read it), which is independent of whether DayZ wrote anything new:
// an idle server's RPT can be FRESH with an old LastGrowthAt.
const (
	StateNoSource = "NO_SOURCE" // no file of this family is listed (yet)
	StateFresh    = "FRESH"     // last successful read within 2x the probe interval
	StateLagging  = "LAGGING"   // last successful read older than that (backoff or slow reads)
	StateFailing  = "FAILING"   // three or more consecutive read/commit failures
)

// SourceHealth is one family's independent freshness and diagnostic view. It carries no secret,
// no signed URL and no raw log line.
type SourceHealth struct {
	Family              string     `json:"family"`
	State               string     `json:"state"`
	SourceFile          string     `json:"sourceFile,omitempty"`
	RemotePath          string     `json:"-"` // physical path: diagnostics log only the canonical id
	FileLocalStart      *time.Time `json:"fileLocalStart,omitempty"`
	Checkpoint          int64      `json:"checkpoint"`
	ReadSize            int64      `json:"readSize"`
	ListingSize         int64      `json:"listingSize"`
	ListingModified     *time.Time `json:"listingModified,omitempty"`
	LastReadAt          *time.Time `json:"lastReadAt,omitempty"`
	LastGrowthAt        *time.Time `json:"lastGrowthAt,omitempty"`
	LastRecordLocalTime *time.Time `json:"lastRecordLocalTime,omitempty"`
	LastFailureAt       *time.Time `json:"lastFailureAt,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	LastError           string     `json:"lastError,omitempty"`
	Reads               int64      `json:"reads"`
	ReadBytes           int64      `json:"readBytes"`
	Records             int64      `json:"records"`
	Live                int64      `json:"live"`
	Backfill            int64      `json:"backfill"`
	Unknown             int64      `json:"unknown"`
	Rotations           int64      `json:"rotations"`
	// ListingBehindBytes: how far the listing's size is behind what a direct read found (bytes
	// seen directly but not yet in the listing) - evidence of stale Nitrado metadata.
	ListingBehindBytes int64          `json:"listingBehindBytes"`
	Latency            LatencySummary `json:"latency"`

	latency latencyWindow
}

// LatencySummary: measured, never assumed. EventToDetect needs a known source UTC time (restart.log
// states it; other families use the offset restart.log stated). VisibleWindow is the gap between
// the last read that did not have the bytes and the read that found them - Nitrado made them
// visible somewhere inside it. DetectToPersist is read-complete to commit-complete.
type LatencySummary struct {
	Samples              int     `json:"samples"`
	EventToDetectP50Sec  float64 `json:"eventToDetectP50Sec"`
	EventToDetectP95Sec  float64 `json:"eventToDetectP95Sec"`
	EventToDetectMaxSec  float64 `json:"eventToDetectMaxSec"`
	EventSamples         int     `json:"eventSamples"`
	VisibleWindowP50Sec  float64 `json:"visibleWindowP50Sec"`
	VisibleWindowP95Sec  float64 `json:"visibleWindowP95Sec"`
	VisibleSamples       int     `json:"visibleSamples"`
	DetectToPersistP50Ms float64 `json:"detectToPersistP50Ms"`
	DetectToPersistP95Ms float64 `json:"detectToPersistP95Ms"`
	PersistSamples       int     `json:"persistSamples"`
}

const latencyWindowSize = 256

type latencyWindow struct {
	event, visible, persist []time.Duration
}

func push(buf []time.Duration, v ...time.Duration) []time.Duration {
	buf = append(buf, v...)
	if len(buf) > latencyWindowSize {
		buf = buf[len(buf)-latencyWindowSize:]
	}
	return buf
}

func (l *latencyWindow) add(event, visible []time.Duration, persist time.Duration) {
	l.event = push(l.event, event...)
	l.visible = push(l.visible, visible...)
	l.persist = push(l.persist, persist)
}

func percentile(buf []time.Duration, p float64) time.Duration {
	if len(buf) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), buf...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(p*float64(len(s)-1) + 0.5)
	return s[idx]
}

func (l *latencyWindow) summary() LatencySummary {
	return LatencySummary{
		Samples:              len(l.persist),
		EventToDetectP50Sec:  percentile(l.event, 0.5).Seconds(),
		EventToDetectP95Sec:  percentile(l.event, 0.95).Seconds(),
		EventToDetectMaxSec:  percentile(l.event, 1).Seconds(),
		EventSamples:         len(l.event),
		VisibleWindowP50Sec:  percentile(l.visible, 0.5).Seconds(),
		VisibleWindowP95Sec:  percentile(l.visible, 0.95).Seconds(),
		VisibleSamples:       len(l.visible),
		DetectToPersistP50Ms: float64(percentile(l.persist, 0.5).Microseconds()) / 1000,
		DetectToPersistP95Ms: float64(percentile(l.persist, 0.95).Microseconds()) / 1000,
		PersistSamples:       len(l.persist),
	}
}

// Snapshot is the supervisor's diagnostic view.
type Snapshot struct {
	ServerID         int64             `json:"serverId"`
	StartedAt        time.Time         `json:"startedAt"`
	UTCOffsetMinutes *int              `json:"utcOffsetMinutes,omitempty"`
	Directories      int               `json:"directories"`
	LastListingAt    *time.Time        `json:"lastListingAt,omitempty"`
	ListingError     string            `json:"listingError,omitempty"`
	Sources          []SourceHealth    `json:"sources"`
	SessionEvidence  []SessionEvidence `json:"sessionEvidence"`
}

// Snapshot returns a consistent copy of every family's health.
func (s *Supervisor) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	now := s.cfg.Now()
	s.mu.RLock()
	out := Snapshot{ServerID: s.cfg.ServerID, StartedAt: s.started, SessionEvidence: append([]SessionEvidence{}, s.sessionEv...)}
	if s.offsetMin != nil {
		v := *s.offsetMin
		out.UTCOffsetMinutes = &v
	}
	probe := map[string]time.Duration{}
	for _, p := range s.cfg.Policies {
		probe[p.Family] = p.ProbeEvery
	}
	for _, p := range s.cfg.Policies {
		h := s.health[p.Family]
		if h == nil {
			continue
		}
		c := *h
		c.Latency = h.latency.summary()
		c.latency = latencyWindow{}
		if c.State == StateFresh && (c.LastReadAt == nil || now.Sub(*c.LastReadAt) > 2*probe[p.Family]) {
			c.State = StateLagging
		}
		if c.ReadSize > c.ListingSize && c.ListingSize > 0 {
			c.ListingBehindBytes = c.ReadSize - c.ListingSize
		}
		out.Sources = append(out.Sources, c)
	}
	s.mu.RUnlock()
	s.lister.mu.RLock()
	out.Directories = len(s.lister.dirs)
	if !s.lister.lastOKAt.IsZero() {
		t := s.lister.lastOKAt
		out.LastListingAt = &t
	}
	out.ListingError = s.lister.lastErr
	s.lister.mu.RUnlock()
	return out
}

func (s *Supervisor) logHealthLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.HealthLogEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		snap := s.Snapshot()
		for _, h := range snap.Sources {
			attrs := []any{"event", "source_health", "server_id", s.cfg.ServerID, "family", h.Family, "state", h.State,
				"file", h.SourceFile, "checkpoint", h.Checkpoint, "read_size", h.ReadSize, "listing_size", h.ListingSize,
				"listing_behind_bytes", h.ListingBehindBytes, "failures", h.ConsecutiveFailures, "records", h.Records,
				"live", h.Live, "backfill", h.Backfill, "unknown", h.Unknown, "rotations", h.Rotations,
				"event_to_detect_p50_s", h.Latency.EventToDetectP50Sec, "event_to_detect_p95_s", h.Latency.EventToDetectP95Sec,
				"event_samples", h.Latency.EventSamples, "visible_window_p50_s", h.Latency.VisibleWindowP50Sec,
				"detect_to_persist_p50_ms", h.Latency.DetectToPersistP50Ms, "detect_to_persist_p95_ms", h.Latency.DetectToPersistP95Ms}
			if h.LastReadAt != nil {
				attrs = append(attrs, "last_read_age_s", int(s.cfg.Now().Sub(*h.LastReadAt).Seconds()))
			}
			if h.LastGrowthAt != nil {
				attrs = append(attrs, "last_growth_age_s", int(s.cfg.Now().Sub(*h.LastGrowthAt).Seconds()))
			}
			if h.LastError != "" {
				attrs = append(attrs, "last_error", h.LastError)
			}
			slog.Info("component=livesync", attrs...)
		}
	}
}

// GuildID is the supervisor's tenant guild (diagnostics).
func (s *Supervisor) GuildID() int64 {
	if s == nil {
		return 0
	}
	return s.cfg.GuildID
}
