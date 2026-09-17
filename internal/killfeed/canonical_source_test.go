package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// TestCanonicalADMIDCollapsesMountAliases is scenario A: the same logical
// ADM exposed through ftproot and noftp must canonicalize to the same ID.
func TestCanonicalADMIDCollapsesMountAliases(t *testing.T) {
	noftp := "/games/ni13295416_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-16_13-35-49.ADM"
	ftproot := "/games/ni13295416_2/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-16_13-35-49.ADM"
	if canonicalADMID(noftp) != canonicalADMID(ftproot) {
		t.Fatalf("expected the same canonical ID, got %q vs %q", canonicalADMID(noftp), canonicalADMID(ftproot))
	}

	other := "/games/ni13295416_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-16_19-16-11.ADM"
	if canonicalADMID(noftp) == canonicalADMID(other) {
		t.Fatal("a different filename must not collapse to the same canonical ID")
	}
}

// TestDeduplicateCandidatesCollapsesAliasGroup proves discoverOnce-facing
// deduplication: two mount representations of the same file become exactly
// one candidate.
func TestDeduplicateCandidatesCollapsesAliasGroup(t *testing.T) {
	e := &Engine{}
	noftp := nitrado.LogFile{Name: "X.ADM", Path: "/games/svc/noftp/dayzps/config/X.ADM", Size: 100, Modified: time.Now()}
	ftproot := nitrado.LogFile{Name: "X.ADM", Path: "/games/svc/ftproot/dayzps/config/X.ADM", Size: 100, Modified: noftp.Modified}
	other := nitrado.LogFile{Name: "Y.ADM", Path: "/games/svc/noftp/dayzps/config/Y.ADM", Size: 50, Modified: noftp.Modified.Add(-time.Hour)}

	out := e.deduplicateCandidates([]nitrado.LogFile{noftp, ftproot, other})
	if len(out) != 2 {
		t.Fatalf("expected 2 logical candidates from 3 physical entries, got %d: %+v", len(out), out)
	}
}

// TestChoosePreferredAliasIsSticky proves section 3: once a physical path is
// selected, discovery must not arbitrarily flip to the other mount every
// pass just because it appears first in that pass's list.
func TestChoosePreferredAliasIsSticky(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	e := &Engine{selected: &nitrado.LogFile{Path: noftpPath}}

	members := []nitrado.LogFile{
		{Name: "X.ADM", Path: ftprootPath, Size: 100},
		{Name: "X.ADM", Path: noftpPath, Size: 100},
	}
	chosen := e.choosePreferredAlias(members)
	if chosen.Path != noftpPath {
		t.Fatalf("expected the already-selected path to stay chosen, got %q", chosen.Path)
	}
}

// newAliasFastPathEngine builds an engine already selected on `selected`,
// with a tracker recording the given prior offset for that exact path -
// mirroring newRotationFastPathEngine but seeding checkpoint history too.
func newAliasFastPathEngine(selected nitrado.LogFile, priorOffset int64) (*Engine, *fakeLogSource) {
	fake := &fakeLogSource{}
	e := NewEngine(fake, "svc-1", testParser{})
	e.state = StatePolling
	e.selected = &selected
	e.logSourceFound = true
	e.tracker.UpdateCheckpoint(e.serviceID, selected.Path, selected.Size, selected.Modified, priorOffset)
	return e, fake
}

// TestAliasSwitchDoesNotEmitRotation is scenario B: when discovery's own
// full-tree walk surfaces both mount representations of the currently
// selected ADM, choosing the (sticky) same representation must not look
// like a rotation - presence/rotation state must be untouched.
func TestAliasSwitchDoesNotEmitRotation(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	selected := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Directory: "/games/svc/noftp/dayzps/config", Size: 100, Modified: now}

	e, fake := newAliasFastPathEngine(selected, 100)
	lastLogChangeBefore := time.Now().Add(-90 * time.Second)
	e.lastLogChange = lastLogChangeBefore
	e.rotationPending = false

	fake.logs = []nitrado.LogFile{
		{Name: "X.ADM", Path: ftprootPath, Size: 100, Modified: now},
		selected,
	}
	e.candidateHistory = map[string]candidateObservation{noftpPath: {Size: 100, Modified: now}}

	if err := e.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if e.selected.Path != noftpPath {
		t.Fatalf("expected the sticky noftp representation to remain selected, got %q", e.selected.Path)
	}
	if e.rotationPending {
		t.Fatal("expected no rotation to be flagged for a same-representation reselection")
	}
	if !e.lastLogChange.Equal(lastLogChangeBefore) {
		t.Fatal("expected lastLogChange to be untouched by a non-rotation reselection")
	}
}

// TestAliasSwitchPreservesCheckpoint is scenario C: if the chosen physical
// representation of a logical source DOES change (its previous
// representation vanished from this pass's listing), the logical source's
// already-tracked checkpoint must be reused, never replayed from 0.
func TestAliasSwitchPreservesCheckpoint(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	selected := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Directory: "/games/svc/noftp/dayzps/config", Size: 500, Modified: now}

	e, _ := newAliasFastPathEngine(selected, 500)

	// The noftp representation is gone from this discovery pass entirely
	// (e.g. that mount briefly failed to enumerate it) - only the ftproot
	// alias appears, with growth evidence so it is a legitimate pick.
	ftprootGrown := nitrado.LogFile{Name: "X.ADM", Path: ftprootPath, Size: 700, Modified: now.Add(time.Minute)}
	e.candidateHistory = map[string]candidateObservation{ftprootPath: {Size: 500, Modified: now}}

	e.selectLog(ftprootGrown)

	if e.selected.Path != ftprootPath {
		t.Fatalf("expected ftproot representation selected, got %q", e.selected.Path)
	}
	if e.tracker.LastByteOffset != 500 {
		t.Fatalf("expected the logical source's prior offset (500) reused via canonical alias, got %d", e.tracker.LastByteOffset)
	}
}

// TestAliasSwitchDoesNotDuplicatePublish is scenario D: switching between
// mount representations of the same logical file must never reprocess (and
// republish) content already consumed under the other representation.
func TestAliasSwitchDoesNotDuplicatePublish(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	killLine := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	content := killLine + "\n"

	noftpFile := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Directory: "/games/svc/noftp/dayzps/config", Size: int64(len(content)), Modified: now}
	fake := &fakeLogSource{contentByPath: map[string][]byte{noftpPath: []byte(content), ftprootPath: []byte(content)}}
	e := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	e.SetKillPublisher(pub)
	e.state = StatePolling
	e.selected = &noftpFile
	e.logSourceFound = true

	fake.logs = []nitrado.LogFile{noftpFile}
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected 1 published kill, got %d", len(pub.kills))
	}

	// Now the SAME file appears as its ftproot alias too (grown, so it is a
	// legitimate winner over the static noftp entry) - switching to it must
	// resume past the already-published kill, not replay it.
	grownContent := content + "\n"
	fake.contentByPath[ftprootPath] = []byte(grownContent)
	ftprootFile := nitrado.LogFile{Name: "X.ADM", Path: ftprootPath, Size: int64(len(grownContent)), Modified: now.Add(time.Minute)}
	fake.logs = []nitrado.LogFile{ftprootFile, noftpFile}
	e.candidateHistory = map[string]candidateObservation{
		noftpPath:   {Size: noftpFile.Size, Modified: noftpFile.Modified},
		ftprootPath: {Size: 0, Modified: now.Add(-time.Hour)}, // smaller/older prior observation -> registers growth
	}
	if err := e.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(pub.kills) != 1 {
		t.Fatalf("expected the kill to never be republished across the alias switch, got %d total publishes", len(pub.kills))
	}
}

// TestOldUnchangedFileIsNotRecentlyActive is scenario F: a candidate that
// has been observed before with no growth must classify STALE, never a
// falsely-optimistic "recently active" state.
func TestOldUnchangedFileIsNotRecentlyActive(t *testing.T) {
	e := &Engine{}
	now := time.Now()
	old := nitrado.LogFile{Path: "/old.ADM", Size: 124, Modified: now.Add(-6 * time.Hour)}
	e.candidateHistory = map[string]candidateObservation{old.Path: {Size: 124, Modified: old.Modified}}

	rank := e.rankOneCandidate(old, 1, now)
	if rank.State != candidateStale {
		t.Fatalf("expected an unchanged previously-seen file to classify STALE, got %s (reason=%s)", rank.State, rank.Reason)
	}
	if rank.Reason != "no_growth_since_last_discovery" {
		t.Fatalf("expected reason no_growth_since_last_discovery, got %q", rank.Reason)
	}
}

// TestAllCandidatesStaleRetainsCurrentSource is scenario E/I: with the
// current source proven stale and every other candidate equally
// unproven/historical, discovery must retain the current source rather than
// walking backward through old files, and the pipeline classification must
// remain truthfully WRONG_OR_INACTIVE_ADM_SOURCE.
func TestAllCandidatesStaleRetainsCurrentSource(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()

	if err := engine.PollOnce(ctx); err != nil { // discovery
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil { // baseline read
		t.Fatal(err)
	}
	selectedPath := engine.Stats().SelectedPath

	// Populate history for several older, genuinely static candidates across
	// a first pass, then present them again unchanged alongside the now
	// current-and-stale selected file.
	now := time.Now()
	older := make([]nitrado.LogFile, 0, 5)
	for i := 1; i <= 5; i++ {
		older = append(older, nitrado.LogFile{
			Name:     "old.ADM",
			Path:     "/logs/old" + string(rune('0'+i)) + ".ADM",
			Size:     124,
			Modified: now.Add(-time.Duration(i) * time.Hour),
		})
	}
	engine.candidateHistory = map[string]candidateObservation{}
	for _, o := range older {
		engine.candidateHistory[o.Path] = candidateObservation{Size: o.Size, Modified: o.Modified}
	}

	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	fake.logs = append([]nitrado.LogFile{fake.logs[0]}, older...)
	if err := engine.PollOnce(ctx); err != nil { // forces give-up + discovery
		t.Fatal(err)
	}

	if got := engine.Stats().SelectedPath; got != selectedPath {
		t.Fatalf("expected to retain the current stale source instead of walking backward through history, got %q", got)
	}
	snapshot := engine.diagnostics.Snapshot()
	if snapshot.ProbeClassification != "WRONG_OR_INACTIVE_ADM_SOURCE" {
		t.Fatalf("expected the pipeline to keep truthfully reporting inactive-source classification, got %q", snapshot.ProbeClassification)
	}
}

// TestNewRotatedFileStillBecomesEligible is scenario G/H: the no-active-source
// guard must never block a genuinely new candidate from being picked up, nor
// from being selected promptly once it shows growth.
func TestNewRotatedFileStillBecomesEligible(t *testing.T) {
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	pathA, pathNew := "/A.ADM", "/new.ADM"
	a := nitrado.LogFile{Name: "A.ADM", Path: pathA, Size: 100, Modified: now}

	e, fake := newRotationFastPathEngine(a)
	e.staleMarks = map[string]staleMark{pathA: {At: now, Size: a.Size, Modified: a.Modified}}
	e.candidateHistory = map[string]candidateObservation{pathA: {Size: a.Size, Modified: a.Modified}}

	// A genuinely new, never-before-seen file appears (no growth evidence
	// yet - this is its first appearance).
	newFile := nitrado.LogFile{Name: "new.ADM", Path: pathNew, Size: 10, Modified: now.Add(time.Minute)}
	fake.logs = []nitrado.LogFile{newFile, a}
	e.checkForNewerLog(context.Background())
	if e.selected.Path != pathNew {
		t.Fatalf("expected the brand-new file to become eligible over known-stale A, got %q", e.selected.Path)
	}
}

// TestNoActiveCandidateEventLogged simply exercises the no-active-source
// path via discoverOnce without panicking or selecting a historical file,
// covering scenario I from the discovery side (checkForNewerLog side is
// covered in rotation_fastpath_test.go's throttle/no-flap tests).
func TestNoActiveCandidateEventLogged(t *testing.T) {
	baseline := "historical line\n"
	engine, fake := newFakeEngine(baseline, int64(len(baseline)))
	ctx := context.Background()
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	selectedPath := engine.Stats().SelectedPath

	older := nitrado.LogFile{Name: "old.ADM", Path: "/logs/old.ADM", Size: 124, Modified: time.Now().Add(-6 * time.Hour)}
	engine.candidateHistory = map[string]candidateObservation{older.Path: {Size: older.Size, Modified: older.Modified}}
	engine.lastLogChange = time.Now().Add(-(staleGiveUpAfter + time.Minute))
	fake.logs = append(fake.logs, older)

	if err := engine.PollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := engine.Stats().SelectedPath; got != selectedPath {
		t.Fatalf("expected to retain current source, got %q", got)
	}
	if engine.Stats().State != StatePolling {
		t.Fatalf("expected engine to remain in POLL_SELECTED_LOG after retaining current source, got %s", engine.Stats().State)
	}
}

// TestChoosePreferredAliasPrefersNoftpOverSelectedCheckpointedFtproot is
// scenario A of the noftp API audit: noftp must be preferred over ftproot
// even when ftproot is the currently selected path AND already has a
// tracked checkpoint - neither signal may make ftproot win once a noftp
// alias is present in the listing (section 2).
func TestChoosePreferredAliasPrefersNoftpOverSelectedCheckpointedFtproot(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"

	e := NewEngine(&fakeLogSource{}, "svc-1", testParser{})
	e.selected = &nitrado.LogFile{Path: ftprootPath}
	e.tracker.Checkpoints[ftprootPath] = LogCheckpoint{Offset: 500}

	members := []nitrado.LogFile{
		// ftproot listed first and with a larger size, on top of already
		// being selected and checkpointed - every old tie-break signal
		// favors it, and it must still lose to noftp.
		{Name: "X.ADM", Path: ftprootPath, Size: 900, Modified: time.Now()},
		{Name: "X.ADM", Path: noftpPath, Size: 100, Modified: time.Now().Add(-time.Hour)},
	}
	chosen := e.choosePreferredAlias(members)
	if chosen.Path != noftpPath {
		t.Fatalf("expected noftp to win despite ftproot being selected, checkpointed, larger, and listed first; got %q", chosen.Path)
	}
}

// TestNoftpMissingRetriesBeforeFtprootFallback is scenario B/section 4: a
// canonical source that has previously shown a noftp alias must not flap
// straight to ftproot the moment a single pass's listing lacks noftp - it
// keeps returning the last known noftp representation for a short bounded
// retry window, only falling back once that window is exhausted.
func TestNoftpMissingRetriesBeforeFtprootFallback(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	noftpFile := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Size: 100, Modified: time.Now()}
	ftprootFile := nitrado.LogFile{Name: "X.ADM", Path: ftprootPath, Size: 100, Modified: noftpFile.Modified}

	e := NewEngine(&fakeLogSource{}, "svc-1", testParser{})

	// Pass 1: both aliases present - noftp wins and is remembered.
	chosen := e.choosePreferredAlias([]nitrado.LogFile{ftprootFile, noftpFile})
	if chosen.Path != noftpPath {
		t.Fatalf("pass 1: expected noftp, got %q", chosen.Path)
	}

	// Passes 2 and 3: noftp vanishes from the listing entirely (only ftproot
	// remains a candidate for this canonical group). Within the bounded
	// retry window, Champion must keep returning the remembered noftp
	// representation rather than switching mounts.
	for i := 2; i <= 1+noftpFallbackRetryPasses; i++ {
		chosen = e.choosePreferredAlias([]nitrado.LogFile{ftprootFile})
		if chosen.Path != noftpPath {
			t.Fatalf("pass %d: expected the bounded retry to keep returning noftp, got %q", i, chosen.Path)
		}
	}

	// The next pass exhausts the retry window - only now is ftproot allowed
	// to become the representative.
	chosen = e.choosePreferredAlias([]nitrado.LogFile{ftprootFile})
	if chosen.Path != ftprootPath {
		t.Fatalf("expected ftproot fallback once the retry window is exhausted, got %q", chosen.Path)
	}
}

// TestNoftpRecoversWithoutTreatingAliasSwitchAsRotation is scenario E: once
// Champion has fallen back to ftproot, noftp reappearing in the listing must
// immediately become the representative again (never a permanent
// blacklist), and - driven through the real discoverOnce pipeline - that
// recovery must not look like an ADM rotation or disturb presence state,
// exactly like any other alias-only change (also covers scenario J).
func TestNoftpRecoversWithoutTreatingAliasSwitchAsRotation(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	noftpFile := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Directory: "/games/svc/noftp/dayzps/config", Size: 100, Modified: now}
	ftprootFile := nitrado.LogFile{Name: "X.ADM", Path: ftprootPath, Size: 100, Modified: now}

	e, fake := newAliasFastPathEngine(noftpFile, 100)

	// Force the fallback state directly (equivalent to noftpFallbackRetryPasses
	// consecutive misses already having happened).
	e.rememberNoftpAlias(canonicalADMID(noftpPath), noftpFile)
	e.noftpMemory[canonicalADMID(noftpPath)].missStreak = noftpFallbackRetryPasses + 1
	e.noftpMemory[canonicalADMID(noftpPath)].usingFallback = true
	e.selected = &ftprootFile
	e.candidateHistory = map[string]candidateObservation{ftprootPath: {Size: 100, Modified: now}}
	lastLogChangeBefore := time.Now().Add(-90 * time.Second)
	e.lastLogChange = lastLogChangeBefore
	e.rotationPending = false

	// noftp reappears in the listing.
	fake.logs = []nitrado.LogFile{ftprootFile, noftpFile}

	if err := e.discoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if e.selected.Path != noftpPath {
		t.Fatalf("expected recovery back to noftp, got %q", e.selected.Path)
	}
	if e.rotationPending {
		t.Fatal("expected the mount recovery to never be flagged as a rotation")
	}
	if !e.lastLogChange.Equal(lastLogChangeBefore) {
		t.Fatal("expected lastLogChange to be untouched by a mount-only recovery")
	}
	if mem := e.noftpMemory[canonicalADMID(noftpPath)]; mem.usingFallback || mem.missStreak != 0 {
		t.Fatalf("expected fallback state cleared on recovery, got usingFallback=%v missStreak=%d", mem.usingFallback, mem.missStreak)
	}
}

// TestNoftpFallbackDoesNotDuplicatePublish is scenario I through the new
// mount-fallback path specifically: once the bounded retry is exhausted and
// Champion switches representation to ftproot, it must resume from the
// logical source's own checkpoint and never reprocess (and republish)
// content already consumed while reading it as noftp.
func TestNoftpFallbackDoesNotDuplicatePublish(t *testing.T) {
	noftpPath := "/games/svc/noftp/dayzps/config/X.ADM"
	ftprootPath := "/games/svc/ftproot/dayzps/config/X.ADM"
	now := time.Date(2026, 9, 17, 1, 0, 0, 0, time.UTC)
	killLine := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters`
	content := killLine + "\n"

	noftpFile := nitrado.LogFile{Name: "X.ADM", Path: noftpPath, Directory: "/games/svc/noftp/dayzps/config", Size: int64(len(content)), Modified: now}
	fake := &fakeLogSource{contentByPath: map[string][]byte{noftpPath: []byte(content)}}
	e := NewEngine(fake, "svc-1", NewADMParser())
	pub := &recordingPublisher{}
	e.SetKillPublisher(pub)
	e.state = StatePolling
	e.selected = &noftpFile
	e.logSourceFound = true

	fake.logs = []nitrado.LogFile{noftpFile}
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected 1 published kill from noftp, got %d", len(pub.kills))
	}

	// Establish noftp alias memory as if a prior discovery pass had already
	// observed both mounts together - the realistic precondition for "noftp
	// temporarily disappears" (section 4 only bounds a retry for a source
	// that has actually been seen as noftp before).
	mem := e.rememberNoftpAlias(canonicalADMID(noftpPath), noftpFile)
	mem.lastSeen = noftpFile

	// noftp vanishes from discovery for more than the retry window; ftproot
	// (the SAME underlying kill line re-appended, exactly like
	// TestCheckForNewerLogPreservesCheckpointAndDedupe's "again" fixture)
	// is the only candidate each pass.
	grownContent := content + killLine + " again\n"
	fake.contentByPath[ftprootPath] = []byte(grownContent)
	ftprootFile := nitrado.LogFile{Name: "X.ADM", Path: ftprootPath, Size: int64(len(grownContent)), Modified: now.Add(time.Minute)}
	e.candidateHistory = map[string]candidateObservation{ftprootPath: {Size: 0, Modified: now.Add(-time.Hour)}}

	for i := 0; i < noftpFallbackRetryPasses+1; i++ {
		fake.logs = []nitrado.LogFile{ftprootFile}
		if err := e.discoverOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if e.selected.Path != ftprootPath {
		t.Fatalf("expected the fallback to have switched to ftproot after %d misses, got %q", noftpFallbackRetryPasses+1, e.selected.Path)
	}
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.kills) != 1 {
		t.Fatalf("expected the original kill to never be republished across the mount fallback, got %d total publishes", len(pub.kills))
	}
	if e.tracker.LastByteOffset != int64(len(grownContent)) {
		t.Fatalf("expected the checkpoint to resume from its reused offset and consume the full tail, got %d", e.tracker.LastByteOffset)
	}
}
