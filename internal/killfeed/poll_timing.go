package killfeed

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Poll timing (docs/NITRADO_POLLING.md): how often Nitrado actually writes each server's ADM
// log, and how long after a write the bot notices it. Both come from the modified time Nitrado
// already returns with every listing, so measuring sends nothing extra.
//
//   - write gap: time between two consecutive modified times seen for the selected log. If Nitrado
//     flushes the log every N seconds, polling faster than N cannot make the feed faster.
//   - detect lag: our clock when a change is noticed minus Nitrado's modified time (1-second
//     resolution; small clock differences between Nitrado and us show up here too).
//
// Samples cover the last hour (at most timingMaxSamples each) per Nitrado service.

const (
	timingWindow     = time.Hour
	timingMaxSamples = 2000
	timingLogEvery   = 5 * time.Minute
	// detectLagMax drops lags longer than this (a backlog after a restart or an outage is not
	// polling delay).
	detectLagMax = 10 * time.Minute
)

type timedSample struct {
	at time.Time
	d  time.Duration
}

type serviceTiming struct {
	lastModified time.Time
	writeGaps    []timedSample
	detectLags   []timedSample
	polls        []time.Time // every metadata check
	changes      []time.Time // checks that found new content
	interval     time.Duration
	lastLog      time.Time
}

type timingRegistry struct {
	mu       sync.Mutex
	services map[string]*serviceTiming
	now      func() time.Time
}

var pollTimings = &timingRegistry{services: map[string]*serviceTiming{}, now: time.Now}

func trimSamples(s []timedSample, cutoff time.Time) []timedSample {
	i := 0
	for i < len(s) && s[i].at.Before(cutoff) {
		i++
	}
	s = s[i:]
	if len(s) > timingMaxSamples {
		s = s[len(s)-timingMaxSamples:]
	}
	return s
}

func trimTimes(s []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(s) && s[i].Before(cutoff) {
		i++
	}
	s = s[i:]
	if len(s) > 4*timingMaxSamples {
		s = s[len(s)-4*timingMaxSamples:]
	}
	return s
}

// observeMetadata records one metadata check of the selected log.
func (r *timingRegistry) observeMetadata(serviceID string, modified time.Time, changed bool, interval time.Duration) {
	if serviceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	s := r.services[serviceID]
	if s == nil {
		s = &serviceTiming{}
		r.services[serviceID] = s
	}
	s.interval = interval
	cutoff := now.Add(-timingWindow)
	s.polls = trimTimes(append(s.polls, now), cutoff)
	if !modified.IsZero() {
		if !s.lastModified.IsZero() && modified.After(s.lastModified) {
			s.writeGaps = trimSamples(append(s.writeGaps, timedSample{now, modified.Sub(s.lastModified)}), cutoff)
		}
		if modified.After(s.lastModified) {
			s.lastModified = modified
		}
	}
	if changed {
		s.changes = trimTimes(append(s.changes, now), cutoff)
		if !modified.IsZero() {
			if lag := now.Sub(modified); lag <= detectLagMax {
				if lag < 0 {
					lag = 0
				}
				s.detectLags = trimSamples(append(s.detectLags, timedSample{now, lag}), cutoff)
			}
		}
	}
	if now.Sub(s.lastLog) >= timingLogEvery {
		s.lastLog = now
		t := s.snapshot(serviceID, now)
		slog.Info("component=adm", "event", "adm_timing", "service_id", serviceID, "poll_interval_ms", t.PollIntervalMs,
			"polls_last_hour", t.PollsLastHour, "changes_last_hour", t.ChangesLastHour,
			"write_gap_p50_ms", t.WriteGap.P50Ms, "write_gap_p90_ms", t.WriteGap.P90Ms,
			"detect_lag_p50_ms", t.DetectLag.P50Ms, "detect_lag_p90_ms", t.DetectLag.P90Ms)
	}
}

// Percentiles summarises a set of durations in milliseconds.
type Percentiles struct {
	Samples int   `json:"samples"`
	P50Ms   int64 `json:"p50Ms"`
	P90Ms   int64 `json:"p90Ms"`
	MaxMs   int64 `json:"maxMs"`
}

func percentiles(s []timedSample) Percentiles {
	if len(s) == 0 {
		return Percentiles{}
	}
	ds := make([]time.Duration, len(s))
	for i, v := range s {
		ds[i] = v.d
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	at := func(p float64) int64 { return ds[int(p*float64(len(ds)-1))].Milliseconds() }
	return Percentiles{Samples: len(ds), P50Ms: at(0.5), P90Ms: at(0.9), MaxMs: ds[len(ds)-1].Milliseconds()}
}

// ServiceTiming is one Nitrado service's poll timing over the last hour.
type ServiceTiming struct {
	ServiceID       string      `json:"serviceId"`
	PollIntervalMs  int64       `json:"pollIntervalMs"` // the rate in effect at the last check
	PollsLastHour   int         `json:"pollsLastHour"`
	ChangesLastHour int         `json:"changesLastHour"`
	WriteGap        Percentiles `json:"writeGap"`  // time between Nitrado writes to the log
	DetectLag       Percentiles `json:"detectLag"` // Nitrado write -> bot noticed
}

func (s *serviceTiming) snapshot(serviceID string, now time.Time) ServiceTiming {
	cutoff := now.Add(-timingWindow)
	s.polls = trimTimes(s.polls, cutoff)
	s.changes = trimTimes(s.changes, cutoff)
	s.writeGaps = trimSamples(s.writeGaps, cutoff)
	s.detectLags = trimSamples(s.detectLags, cutoff)
	return ServiceTiming{ServiceID: serviceID, PollIntervalMs: s.interval.Milliseconds(), PollsLastHour: len(s.polls),
		ChangesLastHour: len(s.changes), WriteGap: percentiles(s.writeGaps), DetectLag: percentiles(s.detectLags)}
}

// PollTimings returns every polled service's timing over the last hour.
func PollTimings() []ServiceTiming { return pollTimingsFrom(pollTimings) }

// pollTimingsFrom is PollTimings over a given registry.
func pollTimingsFrom(r *timingRegistry) []ServiceTiming {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]ServiceTiming, 0, len(r.services))
	for id, s := range r.services {
		out = append(out, s.snapshot(id, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceID < out[j].ServiceID })
	return out
}
