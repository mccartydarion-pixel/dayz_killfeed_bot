package killfeed

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// pollSelected checks only the selected log for changes and reads new bytes.
// No directory discovery happens here.
func (e *Engine) pollSelected(ctx context.Context) error {
	if e.selected == nil || e.selected.Path == "" {
		e.enterDiscovery()
		return nil
	}
	if e.tracker == nil {
		e.tracker = NewTracker(e.serviceID)
	}

	e.lastPoll = time.Now()
	// Boot authority: a verified newer boot is found on its own cadence, BEFORE the stale-source
	// branch below, so a quiet boot never waits for staleGiveUpAfter (boot_authority.go).
	if time.Since(e.lastBootScan) >= rescanInterval {
		e.lastBootScan = time.Now()
		if e.scanForNewerBoot(ctx) {
			e.reportPoll()
			return nil
		}
	}
	if !e.lastLogChange.IsZero() && time.Since(e.lastLogChange) > staleGiveUpAfter {
		// Directory-listing metadata can lag behind the file Nitrado is actually
		// writing (see internal/killfeed/adm_source_scan.go). Force a direct read
		// before giving up: on success this updates lastLogChange, so a genuinely
		// live file resumes normal polling instead of re-triggering this branch
		// every tick once discovery reselects the same newest file.
		e.probeStaleSource(ctx, e.selected)
		if time.Since(e.lastLogChange) > staleGiveUpAfter {
			// Bound how often a still-stale selection re-runs the (expensive,
			// full-tree) discovery walk: without this, every poll tick while
			// stuck (as often as every couple seconds) would call ListLogs
			// again even though nothing has changed. Reuses staleProbeInterval
			// so there is one consistent cadence for "how often do we check a
			// quiet source again," not a second magic number.
			if e.lastStaleRediscoveryAt.IsZero() || time.Since(e.lastStaleRediscoveryAt) >= staleProbeInterval {
				e.lastStaleRediscoveryAt = time.Now()
				e.markSelectedStale(e.selected)
				// A quiet server re-enters this branch every staleProbeInterval for as long
				// as nobody is playing. Warn once per file; repeats are routine.
				if e.staleWarnedFile != e.selected.Name {
					e.staleWarnedFile = e.selected.Name
					slog.Warn("component=adm", "event", "selected_stale", "file", e.selected.Name)
				} else {
					slog.Debug("component=adm", "event", "selected_stale", "file", e.selected.Name)
				}
				e.state = StateDiscovery
				return e.discoverOnce(ctx)
			}
			// Rediscovery and the direct probe are both throttled. Fall
			// through to the cheap metadata poll below instead of returning:
			// growth the listing already shows is then read at the normal
			// poll cadence rather than waiting up to staleProbeInterval for
			// the next probe window (the latency source after any quiet
			// period longer than staleGiveUpAfter). Unchanged metadata costs
			// one stat and no content read.
		} else {
			// The probe found and processed genuinely new content directly,
			// using its own checkpoint-aware read. Stop here rather than
			// falling through into the metadata-comparison poll below,
			// which would re-derive changed/truncated state from the same
			// (still stale) directory metadata that caused this branch to
			// fire and could misread it as a truncation, double-processing
			// bytes the probe already consumed.
			e.reportPoll()
			return nil
		}
	}

	// Periodically check for a newer ADM file (post-restart) on a slow cadence,
	// separate from the per-2s selected-file poll.
	if time.Since(e.lastRescan) >= rescanInterval {
		e.lastRescan = time.Now()
		e.checkForNewerLog(ctx)
		if e.state != StatePolling {
			e.reportPoll()
			return nil
		}
	}

	current, err := e.currentMeta(ctx)
	if err != nil {
		return e.handleSelectedFailure(ctx, err)
	}
	e.consecFailures = 0
	e.noteTransportSuccess()

	changed := e.tracker.ShouldReadAgain(current.Path, current.Size, current.Modified)
	if !changed && current.Size != e.growthReadSize && e.tracker.GrewSinceRead(current.Path, current.Size) {
		// Nitrado's modified_at has one-second resolution: lines appended in
		// the same second as the last read leave it unchanged. Growth past the
		// size seen at that read is new content; without this it waited for
		// the next write in a later second (minutes on a quiet server).
		changed = true
		e.growthReadSize = current.Size
	}
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastMetadataCheck = time.Now()
			s.LastMetadataChanged = changed
			s.RemoteSize = current.Size
			s.RemoteModified = current.Modified
		})
	}
	slog.Debug("component=adm", "event", "metadata_checked", "changed", changed, "file", current.Name, "size", current.Size)
	pollTimings.observeMetadata(e.serviceID, current.Modified, changed, e.pollingInterval(time.Now()))
	if !changed {
		e.probeStaleSource(ctx, current)
		e.reportPoll()
		return nil
	}

	if current.Path != e.tracker.CurrentLogFile && e.tracker.CurrentLogFile != "" {
		slog.Debug("component=killfeed", "msg", "log rotation detected", "previous_file", e.tracker.CurrentLogFile, "file", current.Path)
		e.tracker.ResetForRotation(current.Path)
	}
	if current.Path == e.tracker.CurrentLogFile && current.Size < e.tracker.LastByteOffset && !e.metadataLagsDirect(current.Path, e.tracker.LastByteOffset) {
		slog.Debug("component=killfeed", "msg", "log truncation detected", "file", current.Path, "old_offset", e.tracker.LastByteOffset, "current_size", current.Size)
		e.tracker.ResetForRotation(current.Path)
	}

	oldOffset := e.tracker.LastByteOffset
	oldSize := e.tracker.CurrentSize
	previousFile := e.previousFileName
	rotation := e.rotationPending
	if !rotation {
		previousFile = ""
	}
	downloadStarted := time.Now()

	// Partial-read attempt (Champion Performance Phase 1.5, docs/NITRADO_DELTA_READS.md). Only
	// tried when there IS a prior offset to resume from (a cold/first read of a file always goes
	// through the full, already-proven ReadLog path - task section 22's cold-start protection) and
	// only for genuine growth (truncation is already handled above, before this point, by resetting
	// oldOffset to 0). On ANY failure this falls straight through to the unchanged full-read code
	// below, exactly as if delta mode were off - ReadLog is never modified or bypassed by this.
	if e.deltaMode != nitrado.DeltaModeOff && oldOffset > 0 && current.Size > oldOffset {
		if handled := e.tryDeltaPoll(ctx, current, oldOffset, previousFile, rotation, downloadStarted); handled {
			e.reportPoll()
			return nil
		}
	}
	// Verified tail reads (docs/NITRADO_POLLING.md): once partial reads have matched full
	// downloads for this Nitrado service, read only the new bytes. Never for a first read, a
	// rotation, or when the checkpoint is not the one the last read ended at.
	verifyTail := false
	tailReader, canTail := e.client.(nitrado.TailReader)
	if e.deltaMode == nitrado.DeltaModeOff && canTail && !rotation && oldOffset > 0 && current.Size > oldOffset &&
		e.admLastPath == current.Path && e.admLastOffset == oldOffset {
		tailOnly, verify := nitrado.TailPlan(e.serviceID)
		verifyTail = verify
		if tailOnly {
			if handled := e.tryVerifiedTail(ctx, tailReader, current, oldOffset, previousFile, rotation, downloadStarted); handled {
				e.reportPoll()
				return nil
			}
		}
	}

	slog.Info("component=adm", "event", "download_started", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size)
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) { s.LastDownloadAttempt = time.Now() })
		e.diagnostics.Event("download started")
	}
	content, err := e.client.ReadLog(ctx, e.serviceID, current.Path)
	if err != nil {
		report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, PreviousOffset: oldOffset, Duration: time.Since(downloadStarted), Result: "failure", ErrorClass: safeDownloadErrorClass(err), At: time.Now()}
		slog.Warn("component=adm", "event", "download_failed", "server_id", e.serverID, "file", current.Name, "error_class", report.ErrorClass, "result", report.Result, "timestamp", report.At.UTC().Format(time.RFC3339))
		if e.onDownload != nil {
			e.onDownload(report)
		}
		return e.handleSelectedFailure(ctx, err)
	}
	e.consecFailures = 0
	e.lastDownloadAt = time.Now()
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastDownloadSuccess = e.lastDownloadAt
			s.DownloadedBytes = int64(len(content))
			s.NewBytes = int64(len(content)) - oldOffset
		})
	}
	downloadDuration := time.Since(downloadStarted)

	readOffset := oldOffset
	if readOffset > int64(len(content)) || readOffset < 0 {
		slog.Warn("component=adm", "event", "file_truncated", "file", current.Name, "old_offset", oldOffset, "size", len(content))
		e.tracker.ResetForRotation(current.Path)
		e.tracker.UpdateCheckpoint(e.serviceID, current.Path, current.Size, current.Modified, int64(len(content)))
		checkpointOK := e.saveDurableCheckpoint(ctx, current, int64(len(content)))
		result := "success_no_new_events"
		if !checkpointOK {
			result = "checkpoint_failed"
		}
		report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: int64(len(content)), NewBytes: int64(len(content)) - oldOffset, Duration: downloadDuration, Result: result, Truncated: true, Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
		e.emitDownloadReport(report)
		slog.Info("component=adm", "event", "download_complete", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size, "downloaded_bytes", len(content), "previous_offset", oldOffset, "new_offset", len(content), "new_bytes", int64(len(content))-oldOffset, "events_parsed", 0, "duration_ms", downloadDuration.Milliseconds(), "result", result, "timestamp", report.At.UTC().Format(time.RFC3339))
		e.rotationPending = false
		e.reportPoll()
		return nil
	}

	e.tracker.LineBuffer = string(content[readOffset:])
	e.markBatchRead()
	lineChunks := e.tracker.DrainCompleteLinesWithOffsets(readOffset)
	eventsParsed := 0
	newOffset := oldOffset
	for _, chunk := range lineChunks {
		parsed, processErr := e.processLineAt(chunk.Text, current.Path, chunk.EndOffset)
		if parsed {
			eventsParsed++
		}
		if processErr != nil {
			e.tracker.LineBuffer = string(content[newOffset:])
			e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, newOffset)
			checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
			report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: newOffset - oldOffset, EventsParsed: eventsParsed, Duration: downloadDuration, Result: "persistence_failed", Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
			e.emitDownloadReport(report)
			e.rotationPending = false
			e.reportPoll()
			return nil
		}
		newOffset = chunk.EndOffset
	}
	if len(lineChunks) == 0 {
		newOffset = int64(len(content)) - int64(len(e.tracker.LineBuffer))
	}
	bytesConsumed := newOffset - oldOffset

	e.bytesProcessed += bytesConsumed
	e.linesDiscovered += int64(len(lineChunks))
	e.lastLogChange = e.lastPoll
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, int64(len(content)), current.Modified, newOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
	e.rememberLastByte(current.Path, newOffset, content, 0)
	e.noteSourceGrowth(bytesConsumed)
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.CheckpointOffset = newOffset
			s.CheckpointRemoteSize = current.Size
			s.CheckpointLastSaved = time.Now()
			s.LastIncrementalParse = time.Now()
			s.CompleteLines = len(lineChunks)
			s.PartialLineBuffered = len(e.tracker.LineBuffer) > 0
			s.TrackerCount = e.players.OnlineCount()
		})
	}
	result := "success"
	if eventsParsed == 0 {
		result = "success_no_new_events"
	}
	if !checkpointOK {
		result = "checkpoint_failed"
	}
	report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: current.Size, DownloadedBytes: int64(len(content)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: int64(len(content)) - oldOffset, EventsParsed: eventsParsed, Duration: downloadDuration, Result: result, Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now()}
	e.emitDownloadReport(report)
	// Verification runs after the new lines are processed, so it never delays a kill post.
	if verifyTail && int64(len(content)) > oldOffset {
		tail, method, ok := nitrado.ReadTail(ctx, tailReader, e.serviceID, current.Path, oldOffset-1, int64(len(content)))
		if ok {
			match, newBytes := nitrado.TailMatches(content, oldOffset-1, tail)
			nitrado.TailVerified(e.serviceID, match, newBytes, method, "adm")
		}
	}
	e.rotationPending = false
	slog.Info("component=adm", "event", "download_complete", "server_id", e.serverID, "file", current.Name, "remote_size", current.Size, "downloaded_bytes", len(content), "previous_offset", oldOffset, "new_offset", newOffset, "new_bytes", int64(len(content))-oldOffset, "events_parsed", eventsParsed, "duration_ms", downloadDuration.Milliseconds(), "result", result, "timestamp", report.At.UTC().Format(time.RFC3339))

	// ADM sample capture is disabled by default in production. Enable with
	// ADM_SAMPLE_DEBUG=true for parser diagnostics; even then it logs at DEBUG.
	if !e.sampleCaptured && len(content) > 0 && admSampleDebugEnabled() {
		sample := SelectSampleLines(string(content), 45)
		if len(sample) > 0 {
			e.sampleCaptured = true
			slog.Debug("component=killfeed", "msg", "ADM sample captured", "file", current.Name, "lines", len(sample))
			for _, line := range sample {
				slog.Debug("component=killfeed_adm_sample", "line", line)
			}
		}
	}

	slog.Debug("component=killfeed", "state", "POLLING",
		"file", current.Name,
		"old_size", oldSize,
		"new_size", int64(len(content)),
		"old_offset", oldOffset,
		"new_offset", newOffset,
		"bytes", bytesConsumed,
		"lines", len(lineChunks),
	)

	e.reportPoll()
	return nil
}

// tryDeltaPoll attempts a partial read for the current poll cycle, processing it through the exact
// same tracker/parser/checkpoint machinery pollSelected's full-read path uses (task section 4: no
// Nitrado-specific logic in the parser - this is the killfeed/nitrado boundary, not a parallel
// implementation of line parsing). handled=false means nothing was consumed or checkpointed and the
// caller must fall through to the unchanged full-read path below it - a delta failure can never
// leave the tracker in a half-applied state (task section 13).
func (e *Engine) tryDeltaPoll(ctx context.Context, current *nitrado.LogFile, oldOffset int64, previousFile string, rotation bool, downloadStarted time.Time) (handled bool) {
	deltaSource, ok := e.client.(DeltaSource)
	if !ok {
		return false
	}
	targetSize := current.Size // captured once (task section 19) - a still-growing remote file during this read is not chased within this cycle
	result, ok := deltaSource.ReadDelta(ctx, e.serviceID, current.Path, oldOffset, targetSize, e.deltaMode)
	if !ok {
		return false
	}
	return e.applyTail(ctx, current, oldOffset, targetSize, result.Data, result.Method, previousFile, rotation, downloadStarted)
}

// tryVerifiedTail reads oldOffset-1..current.Size from a trusted service. The first byte must be
// the one the last read ended with, so a replaced file is never stitched onto this checkpoint.
// handled=false: nothing was consumed; the caller does a full download.
func (e *Engine) tryVerifiedTail(ctx context.Context, tr nitrado.TailReader, current *nitrado.LogFile, oldOffset int64, previousFile string, rotation bool, downloadStarted time.Time) bool {
	data, method, ok := nitrado.ReadTail(ctx, tr, e.serviceID, current.Path, oldOffset-1, current.Size)
	if !ok || len(data) == 0 || data[0] != e.admLastByte {
		nitrado.TailFailed(e.serviceID)
		return false
	}
	return e.applyTail(ctx, current, oldOffset, oldOffset+int64(len(data)-1), data[1:], method, previousFile, rotation, downloadStarted)
}

// applyTail processes bytes oldOffset..targetSize through the same tracker/parser/checkpoint
// machinery as a full read.
func (e *Engine) applyTail(ctx context.Context, current *nitrado.LogFile, oldOffset, targetSize int64, tail []byte, method, previousFile string, rotation bool, downloadStarted time.Time) (handled bool) {
	downloadDuration := time.Since(downloadStarted)
	saved := targetSize - int64(len(tail))
	if saved < 0 {
		saved = 0
	}
	e.deltaBytesReceived += int64(len(tail))
	e.fullReadBytesAvoided += saved
	slog.Debug("component=adm", "event", "partial_read_complete", "server_id", e.serverID, "file", current.Name,
		"download_mode", method, "requested_offset", oldOffset, "requested_bytes", targetSize-oldOffset,
		"received_bytes", len(tail), "remote_size", targetSize, "saved_bytes", saved, "duration_ms", downloadDuration.Milliseconds())

	e.tracker.LineBuffer = string(tail)
	e.markBatchRead()
	lineChunks := e.tracker.DrainCompleteLinesWithOffsets(oldOffset)
	eventsParsed := 0
	newOffset := oldOffset
	for _, chunk := range lineChunks {
		parsed, processErr := e.processLineAt(chunk.Text, current.Path, chunk.EndOffset)
		if parsed {
			eventsParsed++
		}
		if processErr != nil {
			e.tracker.LineBuffer = string(tail[newOffset-oldOffset:])
			e.tracker.UpdateCheckpoint(e.serviceID, current.Path, targetSize, current.Modified, newOffset)
			checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
			report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: targetSize, DownloadedBytes: int64(len(tail)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: newOffset - oldOffset, EventsParsed: eventsParsed, Duration: downloadDuration, Result: "persistence_failed", Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now(), Mode: method}
			e.emitDownloadReport(report)
			e.rotationPending = false
			return true
		}
		newOffset = chunk.EndOffset
	}
	if len(lineChunks) == 0 {
		newOffset = oldOffset + int64(len(tail)) - int64(len(e.tracker.LineBuffer))
	}
	bytesConsumed := newOffset - oldOffset

	e.bytesProcessed += bytesConsumed
	e.linesDiscovered += int64(len(lineChunks))
	e.lastLogChange = e.lastPoll
	e.tracker.UpdateCheckpoint(e.serviceID, current.Path, targetSize, current.Modified, newOffset)
	checkpointOK := e.saveDurableCheckpoint(ctx, current, newOffset)
	e.rememberLastByte(current.Path, newOffset, tail, oldOffset)
	e.noteSourceGrowth(bytesConsumed)
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.CheckpointOffset = newOffset
			s.CheckpointRemoteSize = targetSize
			s.CheckpointLastSaved = time.Now()
			s.LastIncrementalParse = time.Now()
			s.CompleteLines = len(lineChunks)
			s.PartialLineBuffered = len(e.tracker.LineBuffer) > 0
			s.TrackerCount = e.players.OnlineCount()
		})
	}
	resultStr := "success"
	if eventsParsed == 0 {
		resultStr = "success_no_new_events"
	}
	if !checkpointOK {
		resultStr = "checkpoint_failed"
	}
	report := DownloadReport{ServerID: e.serverID, File: current.Name, PreviousFile: previousFile, RemoteSize: targetSize, DownloadedBytes: int64(len(tail)), PreviousOffset: oldOffset, NewOffset: newOffset, NewBytes: bytesConsumed, EventsParsed: eventsParsed, Duration: downloadDuration, Result: resultStr, Rotation: rotation, CheckpointCurrent: checkpointOK, At: time.Now(), Mode: method}
	e.emitDownloadReport(report)
	e.rotationPending = false
	slog.Info("component=adm", "event", "download_complete", "server_id", e.serverID, "file", current.Name, "download_mode", method, "remote_size", targetSize, "downloaded_bytes", len(tail), "previous_offset", oldOffset, "new_offset", newOffset, "new_bytes", bytesConsumed, "events_parsed", eventsParsed, "duration_ms", downloadDuration.Milliseconds(), "result", resultStr, "timestamp", report.At.UTC().Format(time.RFC3339))
	return true
}

// rememberLastByte records the byte at newOffset-1 for the next verified tail read. data holds the
// file from dataStart onward. If that byte is not in data, the old record stays valid only when
// the checkpoint did not move.
func (e *Engine) rememberLastByte(path string, newOffset int64, data []byte, dataStart int64) {
	i := newOffset - 1 - dataStart
	switch {
	case newOffset <= 0:
		e.admLastPath, e.admLastOffset = "", 0
	case i >= 0 && i < int64(len(data)):
		e.admLastPath, e.admLastOffset, e.admLastByte = path, newOffset, data[i]
	case e.admLastPath == path && e.admLastOffset == newOffset:
		// unchanged checkpoint: keep the byte already known
	default:
		e.admLastPath, e.admLastOffset = "", 0
	}
}

func (e *Engine) saveDurableCheckpoint(ctx context.Context, current *nitrado.LogFile, offset int64) bool {
	if e == nil || e.checkpointStore == nil || current == nil || e.guildID == 0 || e.serverID == 0 {
		return true
	}
	checkpoint := DurableCheckpoint{Filename: current.Path, RemoteModifiedAt: current.Modified, RemoteSize: current.Size, ProcessedOffset: offset}
	if e.tracker != nil {
		checkpoint.PendingPartialLine = e.tracker.LineBuffer
	}
	if err := e.checkpointStore.SaveADMCheckpoint(ctx, e.guildID, e.serverID, e.serviceID, checkpoint); err != nil {
		slog.Warn("component=adm", "event", "checkpoint_failed", "server_id", e.serverID, "err", err.Error())
		return false
	}
	slog.Debug("component=adm", "event", "checkpoint_saved", "server_id", e.serverID, "offset", offset)
	return true
}

func (e *Engine) emitDownloadReport(report DownloadReport) {
	if e != nil && e.onDownload != nil {
		e.onDownload(report)
	}
}

func safeDownloadErrorClass(err error) string {
	var reqErr *nitrado.RequestError
	if errors.As(err, &reqErr) {
		return string(reqErr.Kind)
	}
	return "download_error"
}
