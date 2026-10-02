package livesync

import (
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Mount comparison (docs/NITRADO_POLLING.md "Which mount is fresher"): Nitrado exposes each log
// under two mounts, noftp and ftproot. The lister already lists both every pass, so comparing them
// sends nothing extra. For every file present in both, this records which copy shows new bytes
// first and how long the other takes to catch up. It only measures; nothing reads it to decide
// which copy to use.

const (
	mountNoftp   = "noftp"
	mountFtproot = "ftproot"
)

func mountOf(p string) string {
	switch {
	case strings.Contains(p, "/noftp/"):
		return mountNoftp
	case strings.Contains(p, "/ftproot/"):
		return mountFtproot
	}
	return ""
}

// mountLead is one finished measurement: Leader showed Size bytes first, and the other mount
// reached that size Lag later. Leader "tie" means both showed the growth in the same pass.
type mountLead struct {
	ID     string
	Leader string
	Size   int64
	Lag    time.Duration
}

type mountPending struct {
	leader string
	size   int64
	at     time.Time
}

type mountCompare struct {
	size    map[string]map[string]int64 // canonical id -> mount -> last size seen
	pending map[string]mountPending     // canonical id -> the growth being timed
}

func newMountCompare() *mountCompare {
	return &mountCompare{size: map[string]map[string]int64{}, pending: map[string]mountPending{}}
}

// observe takes one pass's listings of both mounts and returns the measurements that finished.
// A pass must have listed both mounts; a file is compared only once it has been seen in both.
func (m *mountCompare) observe(now time.Time, files []nitrado.LogFile) []mountLead {
	seen := map[string]map[string]int64{}
	for _, f := range files {
		mount := mountOf(f.Path)
		if mount == "" {
			continue
		}
		id := CanonicalSourceID(f.Path)
		if seen[id] == nil {
			seen[id] = map[string]int64{}
		}
		seen[id][mount] = f.Size
	}
	var out []mountLead
	for id, now2 := range seen {
		noftp, okN := now2[mountNoftp]
		ftproot, okF := now2[mountFtproot]
		if !okN || !okF {
			continue
		}
		prev, known := m.size[id]
		m.size[id] = map[string]int64{mountNoftp: noftp, mountFtproot: ftproot}
		if !known {
			continue // the first sight of a file says nothing about which copy grew first
		}
		if p, ok := m.pending[id]; ok {
			follower := ftproot
			if p.leader == mountFtproot {
				follower = noftp
			}
			if follower >= p.size {
				out = append(out, mountLead{ID: id, Leader: p.leader, Size: p.size, Lag: now.Sub(p.at)})
				delete(m.pending, id)
			}
			continue
		}
		grewN, grewF := noftp > prev[mountNoftp], ftproot > prev[mountFtproot]
		switch {
		case !grewN && !grewF:
		case noftp == ftproot:
			out = append(out, mountLead{ID: id, Leader: "tie", Size: noftp})
		case noftp > ftproot:
			m.pending[id] = mountPending{leader: mountNoftp, size: noftp, at: now}
		default:
			m.pending[id] = mountPending{leader: mountFtproot, size: ftproot, at: now}
		}
	}
	// Files that left both listings are forgotten, so the maps stay as small as the directory.
	for id := range m.size {
		if _, ok := seen[id]; !ok {
			delete(m.size, id)
			delete(m.pending, id)
		}
	}
	return out
}
