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
// is genuinely unchanged is reported as an inactive/wrong source rather than
// being treated as healthy.
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
	if snapshot.ProbeClassification != "WRONG_OR_INACTIVE_ADM_SOURCE" {
		t.Fatalf("expected inactive source classification, got %q", snapshot.ProbeClassification)
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
