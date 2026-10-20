package killfeed

import (
	"testing"
	"time"
)

func TestPollTimingMeasuresWriteGapsAndDetectLag(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	now := base
	r := &timingRegistry{services: map[string]*serviceTiming{}, now: func() time.Time { return now }}

	// Nitrado writes at :00, :20, :40; we poll every 3s and notice each write 2s later.
	r.observeMetadata("s1", base, true, 3*time.Second) // first sight: no gap yet
	for _, w := range []time.Duration{20 * time.Second, 40 * time.Second} {
		now = base.Add(w - time.Second)
		r.observeMetadata("s1", base.Add(w-20*time.Second), false, 3*time.Second) // unchanged poll
		now = base.Add(w + 2*time.Second)
		r.observeMetadata("s1", base.Add(w), true, 3*time.Second)
	}
	got := pollTimingsFrom(r)
	if len(got) != 1 {
		t.Fatalf("services: %+v", got)
	}
	s := got[0]
	if s.PollsLastHour != 5 || s.ChangesLastHour != 3 || s.PollIntervalMs != 3000 {
		t.Fatalf("counts: %+v", s)
	}
	if s.WriteGap.Samples != 2 || s.WriteGap.P50Ms != 20000 {
		t.Fatalf("write gap: %+v", s.WriteGap)
	}
	if s.DetectLag.Samples != 3 || s.DetectLag.P50Ms != 2000 || s.DetectLag.MaxMs != 2000 {
		t.Fatalf("detect lag (first sight at base counts 0ms, later two 2s; p50 index): %+v", s.DetectLag)
	}

	// Samples older than an hour drop out; a lag beyond 10 minutes is a backlog, not polling delay.
	now = base.Add(2 * time.Hour)
	r.observeMetadata("s1", base.Add(time.Hour), true, 10*time.Second)
	s = pollTimingsFrom(r)[0]
	if s.PollsLastHour != 1 || s.DetectLag.Samples != 0 || s.WriteGap.Samples != 1 {
		t.Fatalf("window/backlog: %+v", s)
	}
}
