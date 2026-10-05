package discord

import (
	"sort"
	"time"

	"github.com/bwmarrin/discordgo"
)

// In-memory timing per command, button and form, for the status endpoint.
// Names are the same masked labels the logs use (command and subcommand, or a
// custom ID with its numbers replaced), never user IDs, arguments or content.

// timingSamples is how many recent samples the percentiles are computed over.
const timingSamples = 512

// maxTimedNames bounds the table; anything beyond it is counted as "other".
const maxTimedNames = 256

type timingSeries struct {
	count   uint64
	max     time.Duration
	samples []time.Duration // ring of the most recent timingSamples
	next    int
}

func (t *timingSeries) add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	t.count++
	if d > t.max {
		t.max = d
	}
	if len(t.samples) < timingSamples {
		t.samples = append(t.samples, d)
		return
	}
	t.samples[t.next] = d
	t.next = (t.next + 1) % timingSamples
}

// percentiles returns the nearest-rank p50, p90 and p99 of the recent samples.
func (t *timingSeries) percentiles() (p50, p90, p99 time.Duration) {
	if len(t.samples) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), t.samples...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	rank := func(p int) time.Duration {
		idx := (len(sorted)*p + 99) / 100
		if idx < 1 {
			idx = 1
		}
		return sorted[idx-1]
	}
	return rank(50), rank(90), rank(99)
}

type interactionStat struct {
	ack        timingSeries // received -> first response reached Discord
	total      timingSeries // received -> handler finished
	lateAcks   uint64
	unanswered uint64
}

func (t *interactionTimer) statLocked(label string) *interactionStat {
	stat, ok := t.stats[label]
	if !ok {
		if len(t.stats) >= maxTimedNames {
			label = "other"
			if stat, ok = t.stats[label]; ok {
				return stat
			}
		}
		stat = &interactionStat{}
		t.stats[label] = stat
	}
	return stat
}

// finish records how long the handler ran, and whether it ended without any
// acknowledgement reaching Discord.
func (t *interactionTimer) finish(i *discordgo.InteractionCreate, total time.Duration, answered bool) {
	if t == nil || i == nil || i.Interaction == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.byID[i.ID]
	if entry == nil || entry.finished {
		return
	}
	entry.finished = true
	stat := t.statLocked(entry.label)
	stat.total.add(total)
	if !answered && !entry.answered {
		stat.unanswered++
	}
}

// InteractionTiming is the timing summary of one command, button or form
// since the process started. Percentiles cover the most recent 512 samples;
// counts and maximums cover everything.
type InteractionTiming struct {
	Name string `json:"name"`
	// Time to first response: from the bot receiving the interaction to its
	// acknowledgement reaching Discord.
	Acknowledged int64 `json:"acknowledged"`
	AckP50Ms     int64 `json:"ack_p50_ms"`
	AckP90Ms     int64 `json:"ack_p90_ms"`
	AckP99Ms     int64 `json:"ack_p99_ms"`
	AckMaxMs     int64 `json:"ack_max_ms"`
	// LateAcks counts acknowledgements that took over 2.5 s.
	LateAcks int64 `json:"late_acks"`
	// Unanswered counts interactions whose handler ended with no acknowledgement.
	Unanswered int64 `json:"unanswered"`
	// Total handling time: from receiving the interaction to the handler returning.
	Handled    int64 `json:"handled"`
	TotalP50Ms int64 `json:"total_p50_ms"`
	TotalP90Ms int64 `json:"total_p90_ms"`
	TotalP99Ms int64 `json:"total_p99_ms"`
	TotalMaxMs int64 `json:"total_max_ms"`
}

func (t *interactionTimer) summary() []InteractionTiming {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]InteractionTiming, 0, len(t.stats))
	for name, stat := range t.stats {
		a50, a90, a99 := stat.ack.percentiles()
		t50, t90, t99 := stat.total.percentiles()
		out = append(out, InteractionTiming{
			Name:         name,
			Acknowledged: int64(stat.ack.count), AckP50Ms: a50.Milliseconds(), AckP90Ms: a90.Milliseconds(), AckP99Ms: a99.Milliseconds(), AckMaxMs: stat.ack.max.Milliseconds(),
			LateAcks: int64(stat.lateAcks), Unanswered: int64(stat.unanswered),
			Handled: int64(stat.total.count), TotalP50Ms: t50.Milliseconds(), TotalP90Ms: t90.Milliseconds(), TotalP99Ms: t99.Milliseconds(), TotalMaxMs: stat.total.max.Milliseconds(),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// InteractionTimings returns the per-name timing summary, sorted by name. It
// is safe to call from any goroutine and holds no user data.
func (c *Client) InteractionTimings() []InteractionTiming {
	if c == nil {
		return nil
	}
	return c.timer.summary()
}
