package killfeed

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Champion Live Sync phase 2.1 (docs/CHAMPION_LIVE_SYNC.md section 7.7): ADM source authority.
//
// DayZ names every boot's ADM after the server-local time the boot started
// (DayZServer_PS4_x64_2026-09-24_09-17-09.ADM) and writes the same time into its first line
// ("AdminLog started on 2026-09-24 at 09:17:09"). When both agree, that stamp is the boot's
// identity, and it is authoritative over listing activity:
//
//   - a verified NEWER boot is selected on the next boot scan (every rescanInterval), even when its
//     124-byte header never grows - a quiet boot is still the current boot. The scan runs before the
//     stale-source branch, so it never waits for the old file's staleGiveUpAfter rediscovery;
//   - an OLDER boot is never selected: every candidate list is filtered before ranking, and
//     selectLog refuses one as a last line of defense. A listing that temporarily loses the current
//     file therefore retains the accepted boot instead of walking backward through history - and a
//     historical file is never read from byte zero into the live publishers.
//
// Activity ranking (source_selection.go) still decides between candidates with no stamp and between
// mount aliases of one boot, which deduplicateCandidates already collapses to one logical source.

// bootVerifyWindow: the header and the filename are written by the same boot within seconds.
const bootVerifyWindow = 2 * time.Minute

// bootHeaderReadLimit bounds the bytes inspected for the header (it is the first non-blank line).
const bootHeaderReadLimit = 4096

var admHeaderRe = regexp.MustCompile(`AdminLog started on (\d{4}-\d{2}-\d{2}) at (\d{2}:\d{2}:\d{2})`)

// admBootStamp is the server-local boot time in an ADM file name.
func admBootStamp(path string) (time.Time, bool) {
	c, ok := newADMClock(path)
	if !ok {
		return time.Time{}, false
	}
	return c.base, true
}

// BootAuthorityStats are the boot-authority counters exposed for diagnostics.
type BootAuthorityStats struct {
	AcceptedBoot         time.Time // filename stamp of the accepted current boot (server-local)
	AcceptedFile         string    // canonical ADM identity
	AcceptedAt           time.Time // when Champion accepted it
	LastNewBootSeenAt    time.Time // first listing of the most recent newer boot
	LastNewBootFile      string
	RejectedOlder        int64 // older-boot candidates filtered out or refused
	UnverifiedCandidates int64 // newer-stamped candidates whose header did not confirm the stamp (yet)
	LastRejectedFile     string
	LastCandidateFile string // canonical newer boot being verified
	LastCandidateReason string // reason a newer candidate could not be verified; empty on acceptance
	LastCandidateCheckAt time.Time
}

// BootAuthority returns a snapshot of the boot-authority counters.
func (e *Engine) BootAuthority() BootAuthorityStats {
	e.presenceMu.RLock()
	defer e.presenceMu.RUnlock()
	return e.bootStats
}

func (e *Engine) updateBootStats(fn func(*BootAuthorityStats)) {
	e.presenceMu.Lock()
	fn(&e.bootStats)
	e.presenceMu.Unlock()
}

// rememberADMDirs records every directory discovery listed an ADM in (both mounts), so the boot
// scan lists exactly those - no directory is guessed.
func (e *Engine) rememberADMDirs(logs []nitrado.LogFile) {
	if e.admDirs == nil {
		e.admDirs = map[string]bool{}
	}
	for _, lf := range logs {
		if dir := lf.Directory; dir != "" {
			e.admDirs[dir] = true
		} else if dir := parentPath(lf.Path); dir != "" {
			e.admDirs[dir] = true
		}
	}
}

func parentPath(p string) string {
	for i := len(p) - 1; i > 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return ""
}

// isOlderBoot reports whether a candidate's filename boot stamp is older than the accepted boot.
// Unstamped candidates and the accepted boot's own aliases are never "older".
func (e *Engine) isOlderBoot(path string) bool {
	if e.acceptedBoot.IsZero() {
		return false
	}
	st, ok := admBootStamp(path)
	return ok && st.Before(e.acceptedBoot)
}

// admissibleCandidates removes every candidate whose boot is older than the accepted boot.
func (e *Engine) admissibleCandidates(logs []nitrado.LogFile) []nitrado.LogFile {
	if e.acceptedBoot.IsZero() {
		return logs
	}
	kept := logs[:0:0]
	rejected := 0
	for _, lf := range logs {
		if e.isOlderBoot(lf.Path) {
			rejected++
			continue
		}
		kept = append(kept, lf)
	}
	if rejected > 0 {
		e.updateBootStats(func(s *BootAuthorityStats) { s.RejectedOlder += int64(rejected) })
		slog.Debug("component=adm_discovery", "event", "older_boot_candidates_filtered", "server_id", e.serverID, "count", rejected)
	}
	return kept
}

// newestVerifiedBoot returns the newest-stamped candidate strictly newer than the accepted boot whose
// first line confirms its filename stamp, or nil. Candidates are tried newest first; an unverifiable
// one is skipped for this pass only (it is re-checked on the next scan).
func (e *Engine) newestVerifiedBoot(ctx context.Context, logs []nitrado.LogFile) *nitrado.LogFile {
	type stamped struct {
		lf nitrado.LogFile
		at time.Time
	}
	var cands []stamped
	for _, lf := range logs {
		st, ok := admBootStamp(lf.Path)
		if !ok || !st.After(e.acceptedBoot) {
			continue
		}
		cands = append(cands, stamped{lf, st})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].at.Equal(cands[j].at) {
			// When both mount aliases are listed, try the primary noftp
			// representation first but retain ftproot as a bounded read
			// fallback if noftp lists successfully yet cannot be read.
			return isNoftpAlias(cands[i].lf.Path) && !isNoftpAlias(cands[j].lf.Path)
		}
		return cands[i].at.After(cands[j].at)
	})
	if len(cands) > 0 && e.lastNewBootFile != canonicalADMID(cands[0].lf.Path) {
		// The newest listed boot, recorded once when first seen (diagnostics and SOURCE_LAGGING).
		e.lastNewBootFile = canonicalADMID(cands[0].lf.Path)
		now := time.Now()
		e.updateBootStats(func(s *BootAuthorityStats) { s.LastNewBootSeenAt, s.LastNewBootFile = now, e.lastNewBootFile })
		slog.Info("component=adm_discovery", "event", "newer_boot_listed", "server_id", e.serverID,
			"file", e.lastNewBootFile, "boot_local", cands[0].at.Format("2006-01-02T15:04:05"))
	}
	for _, c := range cands {
		if ok, reason := e.verifyBootHeader(ctx, c.lf, c.at); ok {
			e.updateBootStats(func(s *BootAuthorityStats) {
				s.LastCandidateFile = canonicalADMID(c.lf.Path)
				s.LastCandidateReason = ""
				s.LastCandidateCheckAt = time.Now()
			})
			lf := c.lf
			return &lf
		} else {
			e.updateBootStats(func(s *BootAuthorityStats) {
				s.UnverifiedCandidates++
				s.LastCandidateFile = canonicalADMID(c.lf.Path)
				s.LastCandidateReason = reason
				s.LastCandidateCheckAt = time.Now()
			})
			slog.Info("component=adm_discovery", "event", "boot_candidate_unverified", "server_id", e.serverID,
				"file", canonicalADMID(c.lf.Path), "reason", reason)
		}
	}
	return nil
}

// verifyBootHeader reads the candidate and requires its "AdminLog started" line to state the same
// boot as its filename (within bootVerifyWindow). A verified canonical file is cached.
func (e *Engine) verifyBootHeader(ctx context.Context, lf nitrado.LogFile, stamp time.Time) (bool, string) {
	id := canonicalADMID(lf.Path)
	if e.verifiedBoots[id] {
		return true, ""
	}
	content, err := e.client.ReadLog(ctx, e.serviceID, lf.Path)
	if err != nil {
		return false, "read_failed:" + safeDownloadErrorClass(err)
	}
	if len(content) > bootHeaderReadLimit {
		content = content[:bootHeaderReadLimit]
	}
	m := admHeaderRe.FindSubmatch(content)
	if m == nil {
		return false, "no_adminlog_header"
	}
	header, err := time.Parse("2006-01-02 15:04:05", string(m[1])+" "+string(m[2]))
	if err != nil {
		return false, "unparseable_header"
	}
	if d := header.Sub(stamp); d > bootVerifyWindow || d < -bootVerifyWindow {
		return false, "header_does_not_match_filename"
	}
	if e.verifiedBoots == nil {
		e.verifiedBoots = map[string]bool{}
	}
	if len(e.verifiedBoots) > 64 {
		e.verifiedBoots = map[string]bool{}
	}
	e.verifiedBoots[id] = true
	return true, ""
}

// scanForNewerBoot lists the known ADM directories and, when a verified newer boot exists, drains
// the current file's remaining complete lines and switches to it. Returns true on a switch.
func (e *Engine) scanForNewerBoot(ctx context.Context) bool {
	if e.selected == nil || len(e.admDirs) == 0 {
		return false
	}
	lister, ok := e.client.(interface {
		ListLogsInDir(ctx context.Context, serviceID, dir string) ([]nitrado.LogFile, error)
	})
	if !ok {
		return false
	}
	var logs []nitrado.LogFile
	dirs := make([]string, 0, len(e.admDirs))
	for d := range e.admDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		found, err := lister.ListLogsInDir(ctx, e.serviceID, d)
		if err != nil {
			continue // one mount failing to list never forces a decision
		}
		logs = append(logs, found...)
	}
	if len(logs) == 0 {
		return false
	}
	// Keep aliases for boot-header verification. Collapsing them before
	// reading the header would discard the secondary mount when the
	// preferred noftp representation is listed but temporarily unreadable.
	// The returned selection is still one canonical boot.
	best := e.newestVerifiedBoot(ctx, logs)
	if best == nil {
		return false
	}
	slog.Info("component=adm_discovery", "event", "selection_decision", "selected_path", best.Path, "previous_path", e.selected.Path,
		"selection_reason", "newer_boot_verified", "source_switched", true, "logical_source_changed", true)
	e.selectionReason = "newer_boot_verified"
	e.drainRotationTail(ctx)
	return e.selectLog(*best)
}

// acceptBoot records a selected file's boot as the accepted boot (only ever forward).
func (e *Engine) acceptBoot(lf nitrado.LogFile) {
	st, ok := admBootStamp(lf.Path)
	if !ok {
		return
	}
	file := canonicalADMID(lf.Path)
	// A boot strictly newer than an already-accepted one is a server
	// restart observed live: the restart disconnected everyone.
	restart := !e.acceptedBoot.IsZero() && st.After(e.acceptedBoot)
	if st.After(e.acceptedBoot) || e.acceptedFile == nil {
		e.acceptedBoot = st
		now := time.Now()
		e.updateBootStats(func(s *BootAuthorityStats) { s.AcceptedBoot, s.AcceptedFile, s.AcceptedAt = st, file, now })
	}
	if !st.Before(e.acceptedBoot) {
		c := lf
		e.acceptedFile = &c
	}
	if restart {
		e.resetPresenceForNewBoot(st)
	}
}
