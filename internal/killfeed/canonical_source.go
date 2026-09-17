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
// Preference order: the path already selected (never arbitrarily flip
// mounts on every discovery pass), then a path with an existing tracked
// checkpoint (proven read history), then the largest reported size (most
// complete read), then simply the first alias encountered.
func (e *Engine) choosePreferredAlias(members []nitrado.LogFile) nitrado.LogFile {
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
