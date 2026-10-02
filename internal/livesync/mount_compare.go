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

// mountSummary says what the two mounts list in one pass: how many files each has alone and how
// many they share, and for the newest file of each family the size each mount reports (-1 when
// that mount does not list it). It answers the question mount_lead cannot: whether the log being
// written right now is in both mounts at all.
type mountSummary struct {
	Both, NoftpOnly, FtprootOnly int
	Newest                       []mountNewest
}

type mountNewest struct {
	Family        string
	ID            string
	Noftp         int64
	Ftproot       int64
	NoftpModified time.Time
	FtprootMod    time.Time
}

func summarizeMounts(files []nitrado.LogFile) mountSummary {
	type pair struct {
		size map[string]int64
		mod  map[string]time.Time
		info SourceInfo
	}
	byID := map[string]*pair{}
	for _, f := range files {
		mount := mountOf(f.Path)
		if mount == "" {
			continue
		}
		info := ClassifySource(f.Path)
		p := byID[info.CanonicalID]
		if p == nil {
			p = &pair{size: map[string]int64{}, mod: map[string]time.Time{}, info: info}
			byID[info.CanonicalID] = p
		}
		p.size[mount], p.mod[mount] = f.Size, f.Modified
	}
	var out mountSummary
	newest := map[string]*pair{}
	for _, p := range byID {
		_, n := p.size[mountNoftp]
		_, f := p.size[mountFtproot]
		switch {
		case n && f:
			out.Both++
		case n:
			out.NoftpOnly++
		default:
			out.FtprootOnly++
		}
		if p.info.Family == FamilyADM || p.info.Family == FamilyRPT {
			if cur := newest[p.info.Family]; cur == nil || newerThan(p.info, cur.info) {
				newest[p.info.Family] = p
			}
		}
	}
	for _, family := range []string{FamilyADM, FamilyRPT} {
		p := newest[family]
		if p == nil {
			continue
		}
		n := mountNewest{Family: family, ID: p.info.CanonicalID, Noftp: -1, Ftproot: -1}
		if size, ok := p.size[mountNoftp]; ok {
			n.Noftp, n.NoftpModified = size, p.mod[mountNoftp]
		}
		if size, ok := p.size[mountFtproot]; ok {
			n.Ftproot, n.FtprootMod = size, p.mod[mountFtproot]
		}
		out.Newest = append(out.Newest, n)
	}
	return out
}
