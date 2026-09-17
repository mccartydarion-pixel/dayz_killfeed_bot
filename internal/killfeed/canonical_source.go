package killfeed

import (
	"log/slog"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// admMountMarkers are the known Nitrado filesystem mount segments that can
// each independently expose the identical underlying ADM file for the same
// DayZ service (see the dual-mount discovery in internal/nitrado/logs.go:
// "ftproot" is enumerable from a directory walk, "noftp" is only reachable
// via the gameserver config path - both can list the same physical file).
// Stripping up to and including one of these markers turns two different
// absolute paths for the SAME physical file into the same stable suffix.
// Deliberately narrow: only these two known markers are recognized, so
// unrelated files are never accidentally collapsed together.
var admMountMarkers = []string{"/ftproot/", "/noftp/"}

// canonicalADMID derives a stable logical identity for a discovered ADM
// candidate from the path segment that survives across known Nitrado mount
// aliases (e.g. ".../noftp/dayzps/config/X.ADM" and
// ".../ftproot/dayzps/config/X.ADM" both canonicalize to
// "dayzps/config/X.ADM" - the same relative config filename). A path with no
// recognized mount marker canonicalizes to itself, so it never collides with
// anything else.
func canonicalADMID(path string) string {
	for _, marker := range admMountMarkers {
		if idx := strings.Index(path, marker); idx >= 0 {
			return path[idx+len(marker):]
		}
	}
	return path
}

// deduplicateCandidates groups discovered candidates by canonical ADM
// identity and returns one representative nitrado.LogFile per logical
// source, in the same relative order they first appeared. This is the only
// place candidate identity changes from "physical path" to "logical
// source" - everything downstream (ranking, history, selection) keeps
// working on ordinary nitrado.LogFile values, just deduplicated ones.
//
// Do NOT globally collapse unrelated files: only paths that canonicalize to
// the exact same suffix (same relative config filename under a known mount
// marker) are ever grouped together.
func (e *Engine) deduplicateCandidates(logs []nitrado.LogFile) []nitrado.LogFile {
	type group struct {
		members []nitrado.LogFile
	}
	groups := make(map[string]*group, len(logs))
	order := make([]string, 0, len(logs))
	for _, lf := range logs {
		id := canonicalADMID(lf.Path)
		g, ok := groups[id]
		if !ok {
			g = &group{}
			groups[id] = g
			order = append(order, id)
		}
		g.members = append(g.members, lf)
	}

	out := make([]nitrado.LogFile, 0, len(order))
	for _, id := range order {
		g := groups[id]
		chosen := e.choosePreferredAlias(g.members)
		if len(g.members) > 1 {
			slog.Debug("component=adm_discovery", "event", "alias_group_resolved",
				"canonical_source_id", id, "alias_count", len(g.members), "selected_representation", chosen.Path)
		}
		out = append(out, chosen)
	}
	return out
}

// choosePreferredAlias deterministically picks ONE physical path to
// represent a logical ADM source that currently has multiple mount aliases.
//
// A trusted DayZ operator advised that Nitrado's noftp mount should always be
// preferred over ftproot (ftproot is a slower, secondary representation of
// the same file). So the top-level preference is simply "a noftp alias, if
// one is present in this pass's listing" - never ftproot merely because it
// appeared first, already has a checkpoint, or looks newer by metadata
// (section 2 of the noftp API audit). Only when NO noftp alias is present at
// all - and, per the bounded retry below, has stayed absent for more than a
// couple of passes - does ftproot become the fallback representative.
//
// Within whichever mount's alias set is actually chosen from, the original
// tie-break still applies (pickAlias): the already-selected path, then an
// existing tracked checkpoint, then the largest reported size, then simply
// the first alias encountered. In practice a mount's alias set has exactly
// one member (one file, one representation per known mount), so this only
// matters in the rare case Nitrado reports more than one candidate for the
// same mount under the same canonical ID.
func (e *Engine) choosePreferredAlias(members []nitrado.LogFile) nitrado.LogFile {
	if len(members) == 1 && !isNoftpAlias(members[0].Path) && !isFtprootAlias(members[0].Path) {
		// No recognized mount marker at all - an ordinary single candidate,
		// nothing to prefer between mounts. A lone noftp or ftproot member
		// still needs the logic below: that is exactly what "noftp missing
		// from this pass's listing" looks like once a mount-alias group
		// collapses back down to one member (section 4).
		return members[0]
	}

	var noftpMembers, ftprootMembers []nitrado.LogFile
	for _, m := range members {
		if isNoftpAlias(m.Path) {
			noftpMembers = append(noftpMembers, m)
		} else {
			ftprootMembers = append(ftprootMembers, m)
		}
	}
	id := canonicalADMID(members[0].Path)

	if len(noftpMembers) > 0 {
		chosen := e.pickAlias(noftpMembers)
		mem := e.rememberNoftpAlias(id, chosen)
		if mem.usingFallback {
			slog.Info("component=adm_discovery", "event", "adm_mount_recovered", "canonical_source_id", id, "mount", "noftp")
		}
		mem.lastSeen = chosen
		mem.missStreak = 0
		mem.usingFallback = false
		return chosen
	}

	// No noftp alias in this pass's listing for this canonical source. Only
	// treat this as a fallback-worthy gap if noftp has actually been seen for
	// this source before - a canonical group that has simply never had a
	// noftp copy (e.g. an old rotated ADM that predates this server's noftp
	// mount) is not "noftp missing," it just never existed.
	if mem, ok := e.noftpMemory[id]; ok {
		mem.missStreak++
		if mem.missStreak <= noftpFallbackRetryPasses {
			// Short bounded retry: keep returning the last known noftp
			// representation rather than immediately switching mounts, since
			// noftp's own listing path (a separate Gameserver Details call -
			// see internal/nitrado/logs.go) can occasionally miss a single
			// pass even while the file itself is fine.
			return mem.lastSeen
		}
		if !mem.usingFallback {
			mem.usingFallback = true
			slog.Info("component=adm_discovery", "event", "adm_mount_fallback",
				"canonical_source_id", id, "from", "noftp", "to", "ftproot", "reason", "noftp_missing_after_retry")
		}
	}

	fallback := ftprootMembers
	if len(fallback) == 0 {
		fallback = members
	}
	return e.pickAlias(fallback)
}

// rememberNoftpAlias returns (creating if needed) this canonical source's
// noftp alias memory entry.
func (e *Engine) rememberNoftpAlias(id string, chosen nitrado.LogFile) *noftpAliasMemory {
	if e.noftpMemory == nil {
		e.noftpMemory = make(map[string]*noftpAliasMemory)
	}
	mem, ok := e.noftpMemory[id]
	if !ok {
		mem = &noftpAliasMemory{}
		e.noftpMemory[id] = mem
	}
	return mem
}

// pickAlias is the original single-mount tie-break: the already-selected
// path (never arbitrarily flip aliases every pass), then a path with an
// existing tracked checkpoint, then the largest reported size, then simply
// the first alias encountered.
func (e *Engine) pickAlias(members []nitrado.LogFile) nitrado.LogFile {
	if len(members) == 1 {
		return members[0]
	}
	if e.selected != nil {
		for _, m := range members {
			if m.Path == e.selected.Path {
				return m
			}
		}
	}
	if e.tracker != nil {
		for _, m := range members {
			if _, ok := e.tracker.Checkpoints[m.Path]; ok {
				return m
			}
		}
	}
	best := members[0]
	for _, m := range members[1:] {
		if m.Size > best.Size {
			best = m
		}
	}
	return best
}

// isNoftpAlias/isFtprootAlias report which known Nitrado mount a discovered
// candidate's path belongs to (see admMountMarkers).
func isNoftpAlias(path string) bool   { return strings.Contains(path, "/noftp/") }
func isFtprootAlias(path string) bool { return strings.Contains(path, "/ftproot/") }

// noftpFallbackRetryPasses bounds how many consecutive discovery/rotation-
// check passes a canonical ADM source may go without a noftp alias in the
// live Nitrado listing before Champion falls back to its ftproot
// representation. A short, bounded retry tolerates a single transient
// listing gap (noftp is only reachable via a separate Gameserver Details
// call - see internal/nitrado/logs.go - so it can occasionally miss a pass
// even while the underlying file is fine) without immediately hopping onto
// the slower mount.
const noftpFallbackRetryPasses = 2

// noftpAliasMemory remembers, per canonical ADM source, the last known noftp
// representation and how many consecutive discovery passes it has been
// missing from the live listing - the state backing the bounded retry above.
type noftpAliasMemory struct {
	lastSeen      nitrado.LogFile
	missStreak    int
	usingFallback bool
}

// checkpointForCanonicalAlias looks for a tracked checkpoint belonging to any
// physical path that canonicalizes to id - i.e. a different mount's copy of
// the exact same logical ADM already has read history. Used as a fallback
// when the exact target path has no checkpoint of its own, so switching
// between mount representations of the same logical file never replays
// already-processed bytes (section 4).
func (e *Engine) checkpointForCanonicalAlias(id string) (LogCheckpoint, bool) {
	if e.tracker == nil {
		return LogCheckpoint{}, false
	}
	for path, checkpoint := range e.tracker.Checkpoints {
		if canonicalADMID(path) == id {
			return checkpoint, true
		}
	}
	return LogCheckpoint{}, false
}
