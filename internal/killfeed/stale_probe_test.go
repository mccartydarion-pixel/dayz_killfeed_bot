package killfeed

import (
	"context"
	"testing"
	"time"
)

// TestStaleProbeReadsUnreadTailWhenMetadataIsStale proves that a selected ADM
// whose directory metadata never changes is still read directly, and that only
// bytes after the checkpoint are processed.
func TestStaleProbeReadsUnreadTailWhenMetadataIsStale(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read
		t.Fatal(err)
	}
	baselineLines := engine.Stats().LinesDiscovered

	// Remote content grows, but the directory listing keeps reporting the old
	// size and modified time (the observed Nitrado staleness).
	grown := baseline + "live line one\nlive line two\n"
	fake.content = []byte(grown)

	engine.lastLogChange = time.Now().Add(-3 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if got := engine.Stats().LinesDiscovered; got != baselineLines+2 {
		t.Fatalf("expected only the unread tail processed, got %d lines (baseline %d)", got, baselineLines)
	}
	if engine.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("expected checkpoint advanced to %d, got %d", len(grown), engine.tracker.LastByteOffset)
	}
	snapshot := engine.diagnostics.Snapshot()
	if snapshot.ProbeClassification != "NITRADO_METADATA_STALE" {
		t.Fatalf("expected stale metadata classification, got %q", snapshot.ProbeClassification)
	}
}

// TestStaleProbeDetectsInactiveSource proves that an ADM whose direct content
// is genuinely unchanged is never treated as healthy. A few minutes without
// new bytes is only SOURCE_QUIET (an empty server writes nothing); the
// WRONG_OR_INACTIVE_ADM_SOURCE verdict is reserved for a source still silent
// past staleGiveUpAfter (see TestEngineDoesNotHealStaleSourceReselectedAlone
// and TestAllCandidatesStaleRetainsCurrentSource).
func TestStaleProbeDetectsInactiveSource(t *testing.T) {
	baseline := "historical line\n"
	engine, _ := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	engine.lastLogChange = time.Now().Add(-3 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	snapshot := engine.diagnostics.Snapshot()
	if snapshot.ProbeResult != "SUCCESS" {
		t.Fatalf("expected a successful probe, got %q", snapshot.ProbeResult)
	}
	if snapshot.ProbeClassification != ProbeSourceQuiet {
		t.Fatalf("expected quiet source classification, got %q", snapshot.ProbeClassification)
	}
	if got := snapshot.Classification(); got == "HEALTHY" {
		t.Fatal("a source with no observed growth must never classify as HEALTHY")
	}
}

// TestSourceGrowthClearsStickyProbeClassification reproduces the production
// symptom "selected ADM source becomes stale before subsequent growth is
// detected": one quiet window used to leave ProbeClassification set forever,
// so the pipeline kept reporting WRONG_OR_INACTIVE_ADM_SOURCE even while the
// same source was growing and being read normally again.
func TestSourceGrowthClearsStickyProbeClassification(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline
		t.Fatal(err)
	}
	// Quiet past the give-up threshold: the verdict is WRONG_OR_INACTIVE.
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := engine.diagnostics.Snapshot().Classification(); got != "WRONG_OR_INACTIVE_ADM_SOURCE" {
		t.Fatalf("setup: expected WRONG_OR_INACTIVE_ADM_SOURCE, got %q", got)
	}
	// The server becomes busy again: the file grows AND the listing reports it.
	grown := baseline + "12:00:00 | Player \"A\" (id=abc pos=<1, 2, 3>) is connected\n"
	fake.content = []byte(grown)
	fake.logs[0].Size = int64(len(grown))
	fake.logs[0].Modified = time.Now()
	engine.lastStaleRediscoveryAt = time.Time{}
	growthBefore := engine.diagnostics.Snapshot().LastSourceGrowthAt
	for i := 0; i < 3 && !engine.diagnostics.Snapshot().LastSourceGrowthAt.After(growthBefore); i++ {
		if err := engine.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	snap := engine.diagnostics.Snapshot()
	if !snap.LastSourceGrowthAt.After(growthBefore) {
		t.Fatal("expected the growth read to advance LastSourceGrowthAt")
	}
	if got := snap.Classification(); got == "WRONG_OR_INACTIVE_ADM_SOURCE" || got == ProbeSourceQuiet {
		t.Fatalf("classification stayed stuck at %q after proven growth", got)
	}
}

// TestStaleProbeRunsPastHardStaleThreshold proves the engine does not give up
// on a genuinely live ADM once directory metadata has looked unchanged for
// more than staleGiveUpAfter. Before this was fixed, pollSelected jumped straight to
// full rediscovery once that threshold passed, which reselected the same (only)
// candidate without ever resetting lastLogChange or calling probeStaleSource
// - so on every following poll the same branch fired again, permanently
// starving the direct-read probe and leaving new kill/connect lines unread
// forever even though the server kept writing them.
func TestStaleProbeRunsPastHardStaleThreshold(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read
		t.Fatal(err)
	}
	baselineLines := engine.Stats().LinesDiscovered

	grown := baseline + "PLAYER CONNECTED\n"
	fake.content = []byte(grown)
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if got := engine.Stats().LinesDiscovered; got != baselineLines+1 {
		t.Fatalf("expected the live line to be read via the stale probe, got %d lines (baseline %d)", got, baselineLines)
	}
	if engine.Stats().State != StatePolling {
		t.Fatalf("expected engine to remain polling the live source, got state %s", engine.Stats().State)
	}
	if time.Since(engine.lastLogChange) > time.Minute {
		t.Fatal("expected lastLogChange to be refreshed by the successful probe")
	}
}

// TestStaleProbeIsRateLimited proves the forced probe cannot become a download
// storm while the source stays stale.
func TestStaleProbeIsRateLimited(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	engine.lastLogChange = time.Now().Add(-3 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	readsAfterFirstProbe := fake.reads

	engine.lastLogChange = time.Now().Add(-3 * time.Minute)
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.reads != readsAfterFirstProbe {
		t.Fatalf("expected the probe to be rate limited, got %d reads after %d", fake.reads, readsAfterFirstProbe)
	}
}

// TestGiveUpBranchReadsListingVisibleGrowthWhileThrottled measures the
// detection latency after a long quiet period: with the probe and the
// rediscovery both inside their 60s throttle windows, growth that the
// listing already reports must still be read on the very next poll - not
// after the next probe window.
func TestGiveUpBranchReadsListingVisibleGrowthWhileThrottled(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	if err := engine.PollOnce(ctx); err != nil { // probe + rediscovery, both now throttled
		t.Fatal(err)
	}
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	if engine.lastStaleProbeAt.IsZero() || engine.lastStaleRediscoveryAt.IsZero() {
		t.Fatal("setup: expected both throttles armed")
	}
	grown := baseline + "new line\n"
	fake.content = []byte(grown)
	fake.logs[0].Size = int64(len(grown))
	fake.logs[0].Modified = time.Now()

	started := time.Now()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if engine.tracker.LastByteOffset != int64(len(grown)) {
		t.Fatalf("growth visible in the listing was not read while throttled: offset %d, want %d", engine.tracker.LastByteOffset, len(grown))
	}
	if time.Since(engine.lastLogChange) > time.Since(started) {
		t.Fatal("expected lastLogChange refreshed by the read, leaving the give-up state")
	}
}
