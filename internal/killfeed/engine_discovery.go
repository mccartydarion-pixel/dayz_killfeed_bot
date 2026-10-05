package killfeed

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// discoverOnce runs directory discovery until a candidate is confirmed as the
// active gameplay log, then transitions to polling only that file.
func (e *Engine) discoverOnce(ctx context.Context) error {
	logs, err := e.client.ListLogs(ctx, e.serviceID)
	if err != nil {
		e.noteTransportError(err)
		var reqErr *nitrado.RequestError
		if !errors.As(err, &reqErr) && strings.Contains(err.Error(), "no log files discovered") {
			e.discoverFails++
			if !e.noSourceLogged {
				e.noSourceLogged = true
				slog.Warn("component=killfeed", "state", string(StateDiscovery), "msg", "no gameplay log discovered yet; backing off", "retry_in", discoveryBackoff(e.discoverFails).String())
			}
			e.reportPoll()
			return nil
		}
		e.noSourceLogged = false
		e.discoverFails++
		return err
	}
	e.noSourceLogged = false
	e.noteTransportSuccess()
	if len(logs) == 0 {
		e.discoverFails++
		e.reportPoll()
		return nil
	}
	// Collapse ftproot/noftp mount aliases of the same logical ADM into one
	// candidate before anything else sees the list - ranking, history, and
	// selection all operate on logical sources from this point on, so a pure
	// mount-representation change can never look like a rotation.
	e.rememberADMDirs(logs)
	// Retain physical aliases for boot-header verification. The normal
	// candidate-ranking and checkpoint pipeline still sees logical files.
	bootAliases := append([]nitrado.LogFile(nil), logs...)
	logs = e.deduplicateCandidates(logs)
	// Boot authority (boot_authority.go): an older boot than the accepted one is never a candidate.
	// When a listing gap leaves nothing admissible, the accepted boot is retained as is.
	logs = e.admissibleCandidates(logs)
	if len(logs) == 0 {
		if e.acceptedFile != nil {
			e.selectionReason = "accepted_boot_retained_listing_gap"
			slog.Info("component=adm_discovery", "event", "accepted_boot_retained", "server_id", e.serverID,
				"file", canonicalADMID(e.acceptedFile.Path), "reason", "no_admissible_candidate_listed")
			e.selectLog(*e.acceptedFile)
			e.reportPoll()
			return nil
		}
		e.discoverFails++
		e.reportPoll()
		return nil
	}
	// A verified newer boot (or, at startup, the newest verified boot) is selected directly: a quiet
	// boot's header never grows, so activity ranking alone would demote it.
	if nb := e.newestVerifiedBoot(ctx, e.admissibleCandidates(bootAliases)); nb != nil {
		e.recordCandidates(logs)
		e.updateCandidateHistory(logs, time.Now())
		e.discoverFails = 0
		previousPath := ""
		if e.selected != nil {
			previousPath = e.selected.Path
			e.drainRotationTail(ctx)
		}
		e.selectionReason = "newer_boot_verified"
		slog.Info("component=adm_discovery", "event", "selection_decision",
			"selected_path", nb.Path, "previous_path", previousPath, "selection_reason", e.selectionReason,
			"source_switched", previousPath != "" && canonicalADMID(previousPath) != canonicalADMID(nb.Path))
		e.selectLog(*nb)
		e.reportPoll()
		return nil
	}
	e.recordCandidates(logs)
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.SelectedADM = nameOfSelected(e)
			s.NewestADM = logs[0].Name
			s.SelectionMatch = nameOfSelected(e) == logs[0].Name
			s.SelectionReason = e.selectionReason
			s.CandidateCount = len(logs)
		})
	}
	// Discovery produced real candidates; clear the backoff counter.
	e.discoverFails = 0

	// Rank candidates by evidence of real activity (growth/modified-advance
	// across discovery passes, demoting anything just proven stale) rather
	// than raw "newest modified timestamp" alone - see source_selection.go.
	// History must be read by ranking BEFORE this pass's observations
	// overwrite it.
	now := time.Now()
	ranked := e.rankCandidates(logs, now)
	previousPath := ""
	if e.selected != nil {
		previousPath = e.selected.Path
	}
	for _, r := range ranked {
		slog.Debug("component=adm_discovery", "event", "candidate_ranked",
			"candidate_path", r.File.Path, "metadata_size", r.File.Size, "modified_at", r.File.Modified.UTC().Format(time.RFC3339),
			"candidate_state", string(r.State), "reason", r.Reason, "score", r.Score)
	}
	e.updateCandidateHistory(logs, now)

	best := ranked[0]
	candidate := best.File
	e.selectionReason = best.Reason

	// Nothing looks alive by listing metadata. That is also what a live file
	// looks like when Nitrado's metadata lags, so before retaining a stale
	// source, direct-read the top alternatives and switch only to one whose
	// real content is growing (adm_alt_probe.go).
	var probed *probeSwitch
	if e.selected != nil && best.State != candidateActive && best.State != candidateUnknown {
		if sw := e.probeAlternatives(ctx, ranked); sw != nil {
			probed = sw
			candidate = sw.File
			best = candidateRank{File: sw.File, State: candidateActive, Reason: "direct_probe_growth"}
			e.selectionReason = best.Reason
			e.seedProbeCheckpoint(sw)
		}
	}

	// No candidate anywhere shows real evidence of life (ACTIVE), and the
	// best alternative is not even a brand-new/never-seen file (UNKNOWN) -
	// it is just another proven-or-passively-stale candidate. Retain the
	// current source rather than walking backward to an equally dead
	// historical file (section 7/9): reselect the SAME path with its freshest
	// known metadata so state correctly returns to POLL_SELECTED_LOG instead
	// of spamming full rediscovery every poll.
	if probed == nil && e.selected != nil && candidate.Path != e.selected.Path && best.State != candidateActive && best.State != candidateUnknown {
		slog.Info("component=adm_discovery", "event", "no_active_adm_candidate",
			"current_path", e.selected.Path, "best_alternative_path", candidate.Path,
			"best_alternative_state", string(best.State), "candidate_count", len(logs))
		for _, r := range ranked {
			if r.File.Path == e.selected.Path {
				candidate = r.File
				break
			}
		}
		e.selectionReason = "no_active_candidate_retain_current"
	} else if probed == nil && best.Reason == "newest_remote_modified" && len(logs) > 1 && logs[0].Modified.Equal(logs[1].Modified) {
		e.selectionReason = "newest_filename_timestamp"
	}

	logicalChanged := previousPath != "" && canonicalADMID(previousPath) != canonicalADMID(candidate.Path)
	slog.Info("component=adm_discovery", "event", "selection_decision",
		"selected_path", candidate.Path, "previous_path", previousPath, "selection_reason", e.selectionReason,
		"source_switched", logicalChanged, "physical_path_changed", previousPath != "" && previousPath != candidate.Path,
		"logical_source_changed", logicalChanged, "candidate_state", string(best.State))
	if e.selectLog(candidate) && probed != nil {
		e.finishProbeSwitch(ctx, probed)
	}
	e.reportPoll()
	return nil
}

func (e *Engine) recordCandidates(logs []nitrado.LogFile) {
	e.candidateCount = len(logs)
	if len(logs) == 0 {
		return
	}
	e.newestDiscoveredFile = logs[0].Name
	e.newestDiscoveredModified = logs[0].Modified
	// One line per candidate at DEBUG only - at INFO this was 100+ log lines
	// per discovery pass on a server with a long ADM history (section 10).
	for _, candidate := range logs {
		slog.Debug("component=adm_discovery", "event", "candidate", "file", candidate.Name, "modified_at", candidate.Modified.UTC().Format(time.RFC3339), "size", candidate.Size, "filename_timestamp", filenameTimestamp(candidate.Name))
	}
}

func filenameTimestamp(name string) string {
	for _, layout := range []string{"2006-01-02_15-04-05", "2006-01-02_15-04"} {
		for start := 0; start+len(layout) <= len(name); start++ {
			if parsed, err := time.ParseInLocation(layout, name[start:start+len(layout)], time.UTC); err == nil {
				return parsed.Format(time.RFC3339)
			}
		}
	}
	return ""
}

// selectBestCandidate picks the newest-modified ADM with no other evidence to
// go on. ListLogs already sorts candidates newest-first; file size must never
// decide this, since an old, already-rotated-out ADM can be far larger than
// the current one and would otherwise wrongly win. This is now only the
// bottom-tier fallback inside rankCandidates' scoring (source_selection.go) -
// discoverOnce no longer calls it directly, since "newest modified" alone is
// exactly the signal that caused Champion to keep reselecting a known-dead
// file. Kept as its own function because it is still the correct rule once
// no candidate has any stronger activity evidence.
func selectBestCandidate(logs []nitrado.LogFile) nitrado.LogFile {
	if len(logs) == 0 {
		return nitrado.LogFile{}
	}
	return logs[0]
}

// selectLog locks in the active gameplay log and switches to polling only it. It refuses (returns
// false, keeping the current selection) a file whose boot is older than the accepted boot: an old
// ADM is never selected, so it is never read from byte zero into the live publishers.
func (e *Engine) selectLog(lf nitrado.LogFile) bool {
	if e.isOlderBoot(lf.Path) {
		file := canonicalADMID(lf.Path)
		e.updateBootStats(func(s *BootAuthorityStats) { s.RejectedOlder++; s.LastRejectedFile = file })
		slog.Warn("component=adm_discovery", "event", "older_boot_refused", "server_id", e.serverID, "file", file,
			"accepted_boot", e.acceptedBoot.Format("2006-01-02T15:04:05"))
		if e.selected != nil {
			e.state = StatePolling
		}
		return false
	}
	previousName := ""
	previousPath := ""
	if e.selected != nil {
		previousName = e.selected.Name
		previousPath = e.selected.Path
	}
	candidate := lf
	e.selected = &candidate
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.SelectedADM = candidate.Name
			s.SelectionMatch = e.newestDiscoveredFile == "" || candidate.Name == e.newestDiscoveredFile
			s.SelectionReason = e.selectionReason
		})
	}
	e.confirmPending = nil
	e.state = StatePolling
	e.consecFailures = 0
	e.sampleCaptured = false
	if !e.checkpointLoaded && e.checkpointStore != nil && e.guildID > 0 && e.serverID > 0 {
		checkpoint, err := e.checkpointStore.LoadADMCheckpoint(context.Background(), e.guildID, e.serverID)
		if err != nil {
			slog.Warn("component=adm", "event", "checkpoint_load_failed", "server_id", e.serverID, "err", err.Error())
		} else if checkpoint != nil && checkpoint.Filename == candidate.Path {
			e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, checkpoint.RemoteSize, checkpoint.RemoteModifiedAt, checkpoint.ProcessedOffset)
			e.tracker.LineBuffer = checkpoint.PendingPartialLine
			slog.Info("component=adm", "event", "checkpoint_loaded", "server_id", e.serverID, "offset", checkpoint.ProcessedOffset)
		} else if !e.logSourceFound && !e.startAtTail {
			e.startAtTail = true
			slog.Info("component=adm", "event", "cold_start_baseline_required", "server_id", e.serverID, "file", candidate.Name)
		}
		e.checkpointLoaded = true
	}

	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}
	if e.tracker.CurrentLogFile != "" && e.tracker.CurrentLogFile != candidate.Path {
		// If this exact path was already polled earlier in this process's
		// lifetime (e.g. it grew stale, got demoted, and later became
		// eligible again - see source_selection.go), resume from its own
		// previously tracked offset instead of discarding it. Only a path
		// genuinely new to this tracker gets reset to 0. This is what makes
		// switching sources safe: a re-selected file is never replayed from
		// the start, and its bytes are never confused with another file's.
		if existing, ok := e.tracker.Checkpoints[candidate.Path]; ok {
			e.tracker.CurrentLogFile = candidate.Path
			e.tracker.LastByteOffset = existing.Offset
			e.tracker.LineBuffer = ""
		} else if alias, ok := e.checkpointForCanonicalAlias(canonicalADMID(candidate.Path)); ok {
			// A different mount's copy of this exact logical ADM was already
			// being tracked (e.g. the same file previously read as noftp/X.ADM
			// is now represented as ftproot/X.ADM) - resume from that offset
			// instead of replaying from byte 0 just because the physical
			// representation changed (section 4).
			e.tracker.CurrentLogFile = candidate.Path
			e.tracker.LastByteOffset = alias.Offset
			e.tracker.LineBuffer = ""
			slog.Debug("component=adm", "event", "alias_checkpoint_reused", "canonical_source_id", canonicalADMID(candidate.Path), "physical_path", candidate.Path, "offset", alias.Offset)
		} else {
			e.tracker.ResetForRotation(candidate.Path)
		}
		if e.checkpointStore != nil && e.guildID > 0 && e.serverID > 0 {
			if checkpoint, err := e.checkpointStore.LoadADMCheckpoint(context.Background(), e.guildID, e.serverID); err == nil && checkpoint != nil && checkpoint.Filename == candidate.Path {
				e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, checkpoint.RemoteSize, checkpoint.RemoteModifiedAt, checkpoint.ProcessedOffset)
				e.tracker.LineBuffer = checkpoint.PendingPartialLine
			}
		}
	}

	// Safe first-connect behavior: skip any history already in the log by
	// seeding the checkpoint at the current tail. Only applies to the very
	// first selection of this engine instance (never on rotation/restart).
	if e.startAtTail && !e.logSourceFound {
		e.tracker.UpdateCheckpoint(e.serviceID, candidate.Path, candidate.Size, candidate.Modified, candidate.Size)
		e.saveDurableCheckpoint(context.Background(), &candidate, candidate.Size)
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.ColdStartBaseline = true
				s.ColdStartBaselineOffset = candidate.Size
				s.CheckpointOffset = candidate.Size
				s.CheckpointRemoteSize = candidate.Size
				s.CheckpointLastSaved = time.Now()
			})
		}
		slog.Info("component=killfeed", "msg", "first connect: starting at log tail, existing history skipped", "file", candidate.Name, "size", candidate.Size)
	}

	// ADM session != player session: switching which file Champion reads (first
	// selection or later rotation) must never clear live presence. Only
	// authoritative PLAYER_CONNECT/PLAYER_DISCONNECT events change who is online.
	//
	// Rotation is judged by LOGICAL source identity, not raw path: switching
	// between mount representations of the exact same ADM (e.g.
	// noftp/X.ADM -> ftproot/X.ADM) must never emit a rotation or presence
	// event, reset the staleness clock, or otherwise look like anything
	// happened (section 5). A real rotation still does all of that exactly
	// as before.
	if e.logSourceFound {
		logicalChanged := previousPath != "" && canonicalADMID(previousPath) != canonicalADMID(candidate.Path)
		if logicalChanged {
			e.previousFileName = previousName
			e.rotationPending = true
			e.lastRotationAt = time.Now()
			// A switch to a genuinely different source starts its own fresh
			// staleness clock: without this, a source just proven stale (which
			// is why we are switching away from it at all) would leave
			// lastLogChange already past staleGiveUpAfter, so the very next poll of
			// the newly selected source would immediately re-trigger the
			// give-up path before it ever got a normal read - even though it
			// may be perfectly live.
			e.lastLogChange = time.Now()
			slog.Info("component=adm", "event", "rotation", "previous", previousName, "current", candidate.Name, "presence_retained", true)
			slog.Info("component=presence", "event", "rotation", "presence_retained", true, "online_count", e.players.OnlineCount())
		}
	}
	if !e.logSourceFound {
		e.logSourceFound = true
		e.lastLogChange = time.Now()
		slog.Info("component=adm", "event", "current_selected", "file", candidate.Name)
	}
	slog.Info("component=killfeed", "state", string(StatePolling),
		"msg", "log source selected",
		"file", candidate.Name,
		"size", candidate.Size,
		"modified", candidate.Modified.UTC().Format(time.RFC3339),
	)
	if e.sink != nil {
		e.sink.SetLogSource(candidate.Name, candidate.Path, candidate.Size, candidate.Modified)
		e.sink.SetDiscovery(string(StatePolling), 0, 0)
	}
	e.acceptBoot(candidate)
	e.noteADMSession(candidate.Path)
	e.lastRescan = time.Now()
	return true
}

// currentMeta returns fresh metadata for the selected log, preferring the cheap
// per-file stat when the client supports it to avoid full directory scans.
func (e *Engine) currentMeta(ctx context.Context) (*nitrado.LogFile, error) {
	if stat, ok := e.client.(StatSource); ok {
		return stat.StatFile(ctx, e.serviceID, e.selected.Path)
	}

	logs, err := e.client.ListLogs(ctx, e.serviceID)
	if err != nil {
		return nil, err
	}
	for _, lf := range logs {
		if lf.Path == e.selected.Path {
			current := lf
			return &current, nil
		}
	}
	return nil, &nitrado.RequestError{Op: "stat file", Kind: nitrado.KindNotFound, Message: "selected log no longer present", StatusCode: 404}
}

// handleSelectedFailure counts consecutive failures on the selected log and
// re-enters discovery only after the file is confirmed repeatedly unreachable.
func (e *Engine) handleSelectedFailure(ctx context.Context, err error) error {
	e.consecFailures++
	e.noteTransportError(err)

	var reqErr *nitrado.RequestError
	isNotFound := errors.As(err, &reqErr) && reqErr.Kind == nitrado.KindNotFound

	if isNotFound || e.consecFailures >= maxConsecFailures {
		slog.Warn("component=killfeed", "msg", "selected log lost; re-entering discovery", "file", e.selected.Name, "err", err.Error())
		e.enterDiscovery()
		e.reportPoll()
		return nil
	}

	slog.Debug("component=killfeed", "state", "POLLING", "msg", "transient read/stat failure; will retry", "path", e.selected.Path, "failures", e.consecFailures, "err", err.Error())
	e.reportPoll()
	return nil
}

// enterDiscovery resets selection so the next poll re-runs discovery.
func (e *Engine) enterDiscovery() {
	e.state = StateDiscovery
	e.selected = nil
	e.confirmPending = nil
	e.consecFailures = 0
	slog.Info("component=killfeed", "state", string(StateDiscovery))
	if e.sink != nil {
		e.sink.SetDiscovery(string(StateDiscovery), 0, 0)
	}
}

// checkForNewerLog does a lightweight scan of the selected file's own
// directory (DayZ writes a new timestamped ADM after restart) on
// rescanInterval; never rescans the whole ftproot tree. It feeds the result
// through the SAME activity-aware ranking discoverOnce uses
// (rankCandidates/staleMarks/candidateHistory) - there is exactly one
// candidate-selection policy, not a second "newer filename wins" rule here.
// This closes a real production regression: a file just proven stale (see
// markSelectedStale) kept winning this fast path back solely because Nitrado
// still reported it as the newest by modified time, undoing the recovery
// discoverOnce had just performed.
func (e *Engine) checkForNewerLog(ctx context.Context) {
	if e.selected == nil {
		return
	}
	var logs []nitrado.LogFile
	var err error
	if scoped, ok := e.client.(interface {
		ListLogsInDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	}); ok {
		logs, err = scoped.ListLogsInDir(ctx, e.serviceID, e.selected.Directory)
	} else {
		logs, err = e.client.ListLogs(ctx, e.serviceID)
	}
	if err != nil || len(logs) == 0 {
		return
	}
	// Collapse mount aliases before ranking - same reasoning as discoverOnce.
	logs = e.deduplicateCandidates(logs)
	if logs = e.admissibleCandidates(logs); len(logs) == 0 {
		return
	}

	now := time.Now()
	ranked := e.rankCandidates(logs, now)
	// Running this scan far more often than full discovery (rescanInterval vs
	// the staleGiveUpAfter give-up cycle) means genuine new evidence for a demoted
	// source, or a freshly rotated file, is picked up quickly - see section
	// 5/B of the rotation fast-path fix.
	e.updateCandidateHistory(logs, now)

	currentState := candidateUnknown
	for _, r := range ranked {
		if r.File.Path == e.selected.Path {
			currentState = r.State
			break
		}
	}

	best := ranked[0]
	if best.File.Path == e.selected.Path {
		// Nothing beat the current source. Log only when something with a
		// strictly newer Modified timestamp existed but lost on the evidence
		// model, so the rejection (the exact regression this guards against)
		// is still visible without implying a switch happened.
		for _, r := range ranked {
			if r.File.Path != e.selected.Path && r.File.Modified.After(e.selected.Modified) {
				slog.Debug("component=killfeed", "event", "rotation_check",
					"current_path", e.selected.Path, "candidate_path", r.File.Path,
					"current_state", string(currentState), "candidate_state", string(r.State),
					"selection_reason", r.Reason, "switch", false)
				break
			}
		}
		return
	}

	if best.State != candidateActive && best.State != candidateUnknown {
		// Nothing shows real evidence of life; do not hop to an equally dead
		// alternative via the fast path either (section 7).
		slog.Debug("component=killfeed", "event", "no_active_adm_candidate",
			"current_path", e.selected.Path, "best_alternative_path", best.File.Path,
			"best_alternative_state", string(best.State))
		return
	}

	logicalChanged := canonicalADMID(e.selected.Path) != canonicalADMID(best.File.Path)
	slog.Info("component=killfeed", "event", "rotation_check",
		"current_path", e.selected.Path, "candidate_path", best.File.Path,
		"current_state", string(currentState), "candidate_state", string(best.State),
		"selection_reason", best.Reason, "switch", logicalChanged,
		"physical_path_changed", true, "logical_source_changed", logicalChanged)
	e.drainRotationTail(ctx)
	e.selectLog(best.File)
}

func (e *Engine) drainRotationTail(ctx context.Context) {
	if e == nil || e.selected == nil || e.tracker == nil {
		return
	}
	old := *e.selected
	meta, err := e.currentMeta(ctx)
	if err != nil || meta.Size <= e.tracker.LastByteOffset {
		return
	}
	started := time.Now()
	content, err := e.client.ReadLog(ctx, e.serviceID, old.Path)
	if err != nil {
		slog.Warn("component=adm", "event", "rotation_tail_incomplete", "server_id", e.serverID, "file", old.Name, "error_class", safeDownloadErrorClass(err))
		return
	}
	readOffset := e.tracker.LastByteOffset
	if readOffset < 0 || readOffset > int64(len(content)) {
		return
	}
	e.tracker.LineBuffer = string(content[readOffset:])
	e.markBatchRead()
	chunks := e.tracker.DrainCompleteLinesWithOffsets(readOffset)
	safeOffset := readOffset
	parsed := 0
	for _, chunk := range chunks {
		ok, processErr := e.processLineAt(chunk.Text, old.Path, chunk.EndOffset)
		if processErr != nil {
			slog.Warn("component=adm", "event", "rotation_tail_incomplete", "server_id", e.serverID, "file", old.Name, "error_class", safeDownloadErrorClass(processErr))
			break
		}
		if ok {
			parsed++
		}
		safeOffset = chunk.EndOffset
	}
	e.tracker.LineBuffer = string(content[safeOffset:])
	e.tracker.UpdateCheckpoint(e.serviceID, old.Path, int64(len(content)), old.Modified, safeOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, &old, safeOffset)
	e.emitDownloadReport(DownloadReport{ServerID: e.serverID, File: old.Name, DownloadedBytes: int64(len(content)), RemoteSize: meta.Size, PreviousOffset: readOffset, NewOffset: safeOffset, NewBytes: safeOffset - readOffset, EventsParsed: parsed, Duration: time.Since(started), Result: "success", CheckpointCurrent: checkpointOK, At: time.Now()})
}
