package missionwrite

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Journal is the local, append-only record of authorized writes (JSON lines, fsync per entry). It
// makes every authorization single-use and blocks further writes to a destination whose last write
// is STARTED (interrupted) or UNCERTAIN until the owner resolves it. It holds plan IDs, digests and
// statuses only - never a token, URL or credential.
type Journal struct{ path string }

// Entry is one journal line.
type Entry struct {
	Time      string `json:"time"`
	PlanID    string `json:"planId"`
	Operation string `json:"operation"`
	Service   string `json:"service"`
	Path      string `json:"path"`
	Status    string `json:"status"` // STARTED, WRITTEN_VERIFIED, NOT_WRITTEN, UNCERTAIN, RESOLVED
	Detail    string `json:"detail,omitempty"`
}

// OpenJournal opens (creating if needed) the journal at path.
func OpenJournal(path string) (*Journal, error) {
	if path == "" {
		return nil, errors.New("missionwrite: a journal path is required")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &Journal{path: path}, nil
}

// Entries returns every entry in order.
func (j *Journal) Entries() ([]Entry, error) {
	f, err := os.Open(j.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, errors.New("missionwrite: the journal is corrupt; refusing to continue")
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Append writes one entry durably.
func (j *Journal) Append(e Entry) error {
	if e.Time == "" {
		e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Used reports whether planID was already started.
func (j *Journal) Used(planID string) (bool, error) {
	es, err := j.Entries()
	if err != nil {
		return false, err
	}
	for _, e := range es {
		if e.PlanID == planID {
			return true, nil
		}
	}
	return false, nil
}

// Outstanding reports whether the last entry for (service, path) is STARTED or UNCERTAIN.
func (j *Journal) Outstanding(service, path string) (bool, error) {
	es, err := j.Entries()
	if err != nil {
		return false, err
	}
	last := ""
	for _, e := range es {
		if e.Service == service && e.Path == path {
			last = e.Status
		}
	}
	return last == "STARTED" || last == StatusUncertain, nil
}

// Resolve records the owner's resolution of an interrupted or UNCERTAIN write (after the owner has
// inspected the destination). It never writes to the server.
func (j *Journal) Resolve(planID, service, path, note string) error {
	open, err := j.Outstanding(service, path)
	if err != nil {
		return err
	}
	if !open {
		return errors.New("missionwrite: nothing outstanding for this destination")
	}
	if note == "" {
		return errors.New("missionwrite: a resolution note is required")
	}
	return j.Append(Entry{PlanID: planID, Service: service, Path: path, Status: "RESOLVED", Detail: note})
}
