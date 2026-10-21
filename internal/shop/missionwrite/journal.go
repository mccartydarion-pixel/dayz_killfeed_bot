package missionwrite

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Journal is the local, append-only, hash-chained record of authorized writes (JSON lines, fsync per
// entry). It makes every authorization single-use and blocks further writes to a destination whose
// last write is STARTED (interrupted) or UNCERTAIN until the owner resolves it. It holds plan IDs,
// digests and statuses only - never a token, URL or credential.
//
// A journal must not be resettable by accident, so:
//   - its path must be absolute (a different working directory cannot select a different journal);
//   - a separate ANCHOR file (another directory) records the journal's identity, entry count and
//     last hash. A missing, replaced, truncated or edited journal no longer matches its anchor and
//     every operation is refused;
//   - a journal is created only with an explicit init, and only when no anchor exists;
//   - each line carries its sequence number and the hash of the previous line (tamper evident);
//   - an exclusive lock file serializes runs; a lock left by a crash is never removed automatically.
type Journal struct {
	path, anchor string
}

// Entry is one journal line.
type Entry struct {
	Seq       int    `json:"seq"`
	Prev      string `json:"prev"` // SHA-256 of the previous line ("" for the genesis line)
	Time      string `json:"time"`
	JournalID string `json:"journalId,omitempty"` // genesis only
	PlanID    string `json:"planId,omitempty"`
	Operation string `json:"operation,omitempty"`
	Service   string `json:"service,omitempty"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status"` // GENESIS, STARTED, WRITTEN_VERIFIED, NOT_WRITTEN, UNCERTAIN, RESOLVED
	Detail    string `json:"detail,omitempty"`
}

type anchorFile struct {
	JournalID string `json:"journalId"`
	Journal   string `json:"journal"`
	Seq       int    `json:"seq"`
	Last      string `json:"last"`
}

var (
	ErrJournalPath    = errors.New("missionwrite: the journal and anchor paths must be absolute and in different directories")
	ErrJournalMissing = errors.New("missionwrite: the journal does not exist; create it once with -init-journal")
	ErrJournalExists  = errors.New("missionwrite: a journal or anchor already exists; refusing to initialize over it")
	ErrJournalAnchor  = errors.New("missionwrite: the journal does not match its anchor (missing, replaced, truncated or edited); refusing")
	ErrJournalCorrupt = errors.New("missionwrite: the journal is corrupt; refusing")
	ErrJournalLocked  = errors.New("missionwrite: the journal is locked by another run (or a crashed run); inspect, then remove the lock file by hand")
	ErrNothingToAdopt = errors.New("missionwrite: nothing to adopt")
)

// DefaultPaths returns the default journal (user config dir) and anchor (user home) locations.
func DefaultPaths() (journal, anchor string, err error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(cfg, "champion-shop", "mission-write-journal.jsonl"), filepath.Join(home, ".champion-shop-mission-write.anchor"), nil
}

func checkPaths(journal, anchor string) error {
	if !filepath.IsAbs(journal) || !filepath.IsAbs(anchor) || filepath.Clean(filepath.Dir(journal)) == filepath.Clean(filepath.Dir(anchor)) {
		return ErrJournalPath
	}
	return nil
}

// InitJournal creates a new journal and its anchor. It refuses if either already exists.
func InitJournal(journal, anchor string) (*Journal, error) {
	if err := checkPaths(journal, anchor); err != nil {
		return nil, err
	}
	for _, p := range []string{journal, anchor} {
		if _, err := os.Stat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			return nil, ErrJournalExists
		}
	}
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(anchor), 0o700); err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	j := &Journal{path: journal, anchor: anchor}
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, ErrJournalExists
	}
	line, _ := json.Marshal(Entry{Seq: 0, Time: now(), JournalID: hex.EncodeToString(id), Status: "GENESIS"})
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	if err := j.writeAnchor(anchorFile{JournalID: hex.EncodeToString(id), Journal: journal, Seq: 0, Last: SHA256(line)}); err != nil {
		return nil, err
	}
	return j, nil
}

// OpenJournal opens an existing journal and verifies it against its anchor.
func OpenJournal(journal, anchor string) (*Journal, error) {
	if err := checkPaths(journal, anchor); err != nil {
		return nil, err
	}
	j := &Journal{path: journal, anchor: anchor}
	if _, err := j.verified(); err != nil {
		return nil, err
	}
	return j, nil
}

// AdoptJournal re-anchors an existing, internally consistent journal whose anchor was lost. It is an
// explicit owner action. It changes no entry: an outstanding write stays outstanding.
func AdoptJournal(journal, anchor string) (*Journal, error) {
	if err := checkPaths(journal, anchor); err != nil {
		return nil, err
	}
	if _, err := os.Stat(anchor); err == nil {
		return nil, ErrJournalExists
	}
	j := &Journal{path: journal, anchor: anchor}
	es, lines, err := j.readChain()
	if err != nil {
		return nil, err
	}
	if len(es) == 0 {
		return nil, ErrNothingToAdopt
	}
	return j, j.writeAnchor(anchorFile{JournalID: es[0].JournalID, Journal: journal, Seq: es[len(es)-1].Seq, Last: SHA256(lines[len(lines)-1])})
}

// readChain parses the journal and verifies its internal hash chain.
func (j *Journal) readChain() ([]Entry, [][]byte, error) {
	raw, err := os.ReadFile(j.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrJournalMissing
	}
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return nil, nil, ErrJournalCorrupt // a torn final line
	}
	var es []Entry
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	prev := ""
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var e Entry
		if len(line) == 0 || json.Unmarshal(line, &e) != nil || e.Seq != len(es) || e.Prev != prev {
			return nil, nil, ErrJournalCorrupt
		}
		if (e.Seq == 0) != (e.Status == "GENESIS") || (e.Seq == 0 && e.JournalID == "") {
			return nil, nil, ErrJournalCorrupt
		}
		es, lines = append(es, e), append(lines, line)
		prev = SHA256(line)
	}
	if sc.Err() != nil || len(es) == 0 {
		return nil, nil, ErrJournalCorrupt
	}
	return es, lines, nil
}

// verified returns the entries after checking the chain against the anchor. A journal exactly one
// valid, chained entry ahead of its anchor (a crash between the journal fsync and the anchor
// update) is accepted and the anchor rolled forward; anything else is refused.
func (j *Journal) verified() ([]Entry, error) {
	es, lines, err := j.readChain()
	if errors.Is(err, ErrJournalMissing) {
		if _, aerr := os.Stat(j.anchor); aerr == nil {
			return nil, ErrJournalAnchor // the anchor proves a journal existed
		}
		return nil, ErrJournalMissing
	}
	if err != nil {
		return nil, err
	}
	ab, err := os.ReadFile(j.anchor)
	if err != nil {
		return nil, ErrJournalAnchor
	}
	var a anchorFile
	if json.Unmarshal(ab, &a) != nil || a.JournalID != es[0].JournalID || a.Journal != j.path {
		return nil, ErrJournalAnchor
	}
	last := len(es) - 1
	switch {
	case a.Seq == last && a.Last == SHA256(lines[last]):
	case a.Seq == last-1 && a.Last == SHA256(lines[last-1]):
		if err := j.writeAnchor(anchorFile{JournalID: a.JournalID, Journal: j.path, Seq: last, Last: SHA256(lines[last])}); err != nil {
			return nil, err
		}
	default:
		return nil, ErrJournalAnchor
	}
	return es, nil
}

func (j *Journal) writeAnchor(a anchorFile) error {
	b, _ := json.Marshal(a)
	tmp := j.anchor + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, j.anchor)
}

// Entries returns every verified entry in order.
func (j *Journal) Entries() ([]Entry, error) { return j.verified() }

// Append adds one entry durably (journal fsync, then anchor).
func (j *Journal) Append(e Entry) error {
	es, err := j.verified()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(j.path)
	if err != nil {
		return err
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	e.Seq, e.Prev, e.JournalID = len(es), SHA256(lines[len(lines)-1]), ""
	if e.Time == "" {
		e.Time = now()
	}
	if e.Status == "GENESIS" {
		return ErrJournalCorrupt
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_WRONLY, 0o600)
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
	if err := f.Close(); err != nil {
		return err
	}
	return j.writeAnchor(anchorFile{JournalID: es[0].JournalID, Journal: j.path, Seq: e.Seq, Last: SHA256(b)})
}

// Lock takes the exclusive run lock. The returned func releases it. A lock that already exists is
// never taken over or removed here.
func (j *Journal) Lock() (func(), error) {
	p := j.path + ".lock"
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, ErrJournalLocked
	}
	fmt.Fprintf(f, "pid=%d time=%s\n", os.Getpid(), now())
	_ = f.Sync()
	f.Close()
	return func() { _ = os.Remove(p) }, nil
}

// Used reports whether planID was ever started (a used authorization is never reusable, even after
// a resolution).
func (j *Journal) Used(planID string) (bool, error) {
	es, err := j.verified()
	if err != nil {
		return false, err
	}
	for _, e := range es {
		if e.PlanID == planID && e.Status != "RESOLVED" {
			return true, nil
		}
	}
	return false, nil
}

// Outstanding reports whether the last entry for (service, path) is STARTED or UNCERTAIN.
func (j *Journal) Outstanding(service, path string) (bool, error) {
	es, err := j.verified()
	if err != nil {
		return false, err
	}
	for _, k := range outstandingKeys(es) {
		if k == service+"\x00"+path {
			return true, nil
		}
	}
	return false, nil
}

func outstandingKeys(es []Entry) []string {
	last := map[string]string{}
	var order []string
	for _, e := range es {
		if e.Service == "" && e.Path == "" {
			continue
		}
		k := e.Service + "\x00" + e.Path
		if _, ok := last[k]; !ok {
			order = append(order, k)
		}
		last[k] = e.Status
	}
	var out []string
	for _, k := range order {
		if last[k] == "STARTED" || last[k] == StatusUncertain {
			out = append(out, k)
		}
	}
	return out
}

// Resolve records the owner's resolution of an interrupted or UNCERTAIN write (after the owner has
// inspected the destination). It never writes to the server, and the plan ID stays used.
func (j *Journal) Resolve(planID, service, path, note string) error {
	unlock, err := j.Lock()
	if err != nil {
		return err
	}
	defer unlock()
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

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
