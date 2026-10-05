package killfeed

import (
	"context"
	"log/slog"
	"time"
)

// processLines runs the ordered pipeline for complete ADM lines:
// parse -> dedupe -> (PLAYER_KILL only) publish. Runs sequentially on the
// polling goroutine; line order is preserved and no per-line goroutines spawn.
func (e *Engine) processLines(lines []string) int {
	if e == nil || len(lines) == 0 {
		return 0
	}
	if e.dedupe == nil {
		e.dedupe = NewDeduplicator(90*time.Second, 8192)
	}
	parsedCount := 0
	for _, line := range lines {
		parsed, err := e.processLine(line)
		if parsed {
			parsedCount++
		}
		if err != nil {
			break
		}
	}
	return parsedCount
}

func (e *Engine) processLine(line string) (bool, error) {
	return e.processLineAt(line, "", -1)
}

// processLineAt is the real file-backed path. Only complete line chunks with
// an authoritative end offset may be persisted as C.A.S.E. evidence.
func (e *Engine) processLineAt(line, sourcePath string, endOffset int64) (bool, error) {
	e.metrics.ADMLinesProcessed++
	lineFile := ""
	if sourcePath != "" {
		lineFile = canonicalADMID(sourcePath)
	}
	prevFile, prevEnd := e.noteLine(lineFile, endOffset)
	ev, err := e.parser.ParseLine(line)
	if err != nil {
		e.metrics.EventsIgnored++
		return false, nil
	}
	if ev == nil {
		e.metrics.EventsIgnored++
		return false, nil
	}
	ev.DetectedAt = time.Now()
	e.metrics.EventsParsed++
	if e.diagnostics != nil {
		e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
			s.LastParsedEventType = string(ev.Type)
			s.LastParsedEventAt = time.Now()
			if ev.Type == EventPlayerKill {
				s.LastKillParsedAt = time.Now()
			}
		})
		e.diagnostics.Event("parser " + string(ev.Type))
	}
	if e.handleObservationLine(ev, sourcePath, endOffset) {
		return true, nil
	}
	// The line's physical source travels with the event, so kills/deaths and the location rows
	// written from this same line share one identity (heatmap join, Live Sync phase 2).
	if src := e.locationSource(ev, sourcePath, endOffset, ""); src.File != "" {
		ev.SourceFile, ev.SourceOffset, ev.SourceLocalTime = src.File, src.Offset, src.LocalTime
	}
	e.correlateFinalHit(ev, lineFile, endOffset, prevFile, prevEnd)
	if ev.Type == EventPlayerDisconnect {
		slog.Info("component=presence", "event", "disconnect_parsed", "matched", true)
	}
	switch ev.Type {
	case EventPlayerHit:
		e.metrics.HitsParsed++
	case EventPlayerKill:
		e.metrics.ExplicitKillsParsed++
	case EventPlayerDeath:
		e.metrics.DeathsParsed++
	case EventPlayerConnect:
		e.metrics.ConnectsParsed++
	case EventPlayerDisconnect:
		e.metrics.DisconnectsParsed++
	}
	// Evidence is addressed by physical ADM line, not semantic event fingerprint.
	// Record before the legacy hitfeed deduplicator so equal-looking hits at
	// separate offsets remain independent evidence observations.
	if e.evidenceStore != nil && sourcePath != "" {
		if err := e.observeEvidence(ev, sourcePath, endOffset); err != nil {
			slog.Warn("component=case", "event", "evidence_write_failed",
				"server_id", e.serverID, "event_type", string(ev.Type), "err", err.Error())
			return true, err
		}
	}
	if e.dedupe == nil {
		e.dedupe = NewDeduplicator(90*time.Second, 8192)
	}
	if e.dedupe.Contains(ev) {
		e.metrics.DuplicateEventsDropped++
		return true, nil
	}
	if e.persistence != nil && (ev.Type == EventPlayerConnect || ev.Type == EventPlayerDisconnect || ev.Type == EventPlayerDeath || ev.Type == EventSuicideAction || ev.Type == EventPlayerKill) {
		// Bounded, not context.Background(): a single slow/hung downstream
		// call (DB, Discord) inside the persistence queue's consumer must
		// never freeze this engine's entire poll loop indefinitely - observed
		// live as a ~40 minute stall with no logged error, blocking every
		// later ADM poll and kill/death publish behind it. On timeout the
		// checkpoint does not advance past this event, so it is retried on
		// the next poll (existing persistence-failure path); durable dedupe
		// (kill/death fingerprints, connect/disconnect upserts) makes a
		// retry safe even if the original call eventually completes.
		persistCtx, cancel := context.WithTimeout(context.Background(), persistEnqueueTimeout)
		err := e.persistence.EnqueueAndWait(persistCtx, ev)
		cancel()
		if err != nil {
			e.presenceMu.Lock()
			e.lastPersistenceResult = "FAILURE"
			e.presenceMu.Unlock()
			if e.diagnostics != nil {
				e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
					s.LastPersistenceEvent = string(ev.Type)
					s.LastPersistenceResult = "FAILURE"
					s.LastPersistenceAt = time.Now()
					s.LastErrorStage = "PERSISTENCE_FAILURE"
					s.LastErrorAt = time.Now()
				})
			}
			return true, err
		}
		e.presenceMu.Lock()
		e.lastPersistenceResult = "SUCCESS"
		e.presenceMu.Unlock()
		if e.diagnostics != nil {
			e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
				s.LastPersistenceEvent = string(ev.Type)
				s.LastPersistenceResult = "SUCCESS"
				s.LastPersistenceAt = time.Now()
				if ev.Type == EventPlayerKill {
					s.LastKillPersistedAt = time.Now()
				}
			})
			e.diagnostics.Event("persistence success")
		}
	} else if ev.Type == EventPlayerKill && e.publisher != nil {
		if err := e.publisher.PublishKill(ev); err != nil {
			e.metrics.DiscordPublishErrors++
		} else {
			e.metrics.DiscordKillsPublished++
			e.metrics.LastKillTime = time.Now()
		}
	}
	e.dedupe.Remember(ev)
	// Location candidates (Phase 3, docs/PLAYER_INTELLIGENCE.md): only a non-duplicate event
	// reaches here, matching the hit-publish guard immediately below - a replayed line never
	// produces a duplicate location candidate from this call site (the DB-level UNIQUE
	// constraint is still the authoritative backstop, per task section 14, but this avoids
	// manufacturing the duplicate in the first place). EnqueueEvent is non-blocking and a no-op
	// on a nil queue, so this never affects the hot path whether or not Phase 3 is wired up.
	e.locationQueue.EnqueueEventAt(ev, e.locationSource(ev, sourcePath, endOffset, ""))
	if ev.Type == EventPlayerHit {
		// Hits are not persisted. Only a non-duplicate hit reaches here, so a
		// replayed line (retry after a later persistence failure, rotation
		// overlap) is never fed to the HITFEED twice.
		e.publishHit(ev)
	}
	if ev.Type == EventBuildAction {
		// Not persisted; only a non-duplicate line reaches here.
		e.publishBuild(ev)
	}
	if e.players != nil {
		switch ev.Type {
		case EventPlayerConnect:
			if e.players.PlayerConnected(ev.Player) {
				connectAt := time.Now()
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_CONNECT"
				e.lastConnectAt = connectAt
				e.presenceMu.Unlock()
				if e.diagnostics != nil {
					e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
						s.LastConnectAt = connectAt
						s.TrackerCount = e.players.OnlineCount()
					})
				}
				slog.Info("component=presence", "event", "connect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.notePresenceEvent(connectAt)
				e.firePlayersChanged()
				// Published only here: after dedupe, after durable persistence, and
				// only when the player was genuinely not online yet - a repeated
				// "is connected" for an online player is a refresh, not a new
				// connection.
				e.publishConnection(ConnectionNotice{Kind: ConnectionConnected, Name: ev.Player.Name})
			}
		case EventPlayerDisconnect:
			if session, removed := e.players.DisconnectSession(ev.Player); removed {
				disconnectAt := time.Now()
				e.presenceMu.Lock()
				e.lastPresenceEvent = "PLAYER_DISCONNECT"
				e.lastDisconnectAt = disconnectAt
				e.presenceMu.Unlock()
				if e.diagnostics != nil {
					e.diagnostics.Update(func(s *RuntimeDiagnosticSnapshot) {
						s.LastDisconnectAt = disconnectAt
						s.TrackerCount = e.players.OnlineCount()
					})
				}
				slog.Info("component=presence", "event", "disconnect_committed", "server_id", e.serverID, "online_count", e.players.OnlineCount())
				e.notePresenceEvent(disconnectAt)
				e.firePlayersChanged()
				e.publishConnection(ConnectionNotice{Kind: ConnectionDisconnected, Name: ev.Player.Name, Session: session})
			}
		}
	}
	return true, nil
}

// firePlayersChanged invokes the registered hook after the online set changes.
func (e *Engine) firePlayersChanged() {
	if e == nil || e.onPlayers == nil || e.players == nil {
		return
	}
	e.onPlayers(e.players.OnlineCount())
}
