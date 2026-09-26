package nitradodelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SpawnerObject is one entry of a DayZ object-spawner file, field for field as ITEM_SpawnerObject in
// scripts/3_game/objectspawner.c.
type SpawnerObject struct {
	Name                string     `json:"name"`
	Pos                 [3]float64 `json:"pos"` // x, y (altitude), z
	Ypr                 [3]float64 `json:"ypr"`
	Scale               float64    `json:"scale"`
	EnableCEPersistency bool       `json:"enableCEPersistency"`
	CustomString        string     `json:"customString"`
}

// SpawnerFile is the Champion-owned spawner file (ObjectSpawnerJson).
type SpawnerFile struct {
	Objects []SpawnerObject `json:"Objects"`
}

const championTag = "champion:"

var (
	ErrForeignSpawnerEntry = errors.New("the Champion spawner file contains an entry Champion did not write: refusing to modify it")
	ErrInvalidSpawnerFile  = errors.New("the current spawner file is not valid spawner JSON")
	ErrTooManyStaged       = errors.New("too many staged objects in one file")
)

// ParseSpawnerFile parses the current Champion file. Empty content is an empty file. Every entry must
// carry a Champion attempt tag in customString: an untagged entry means somebody else edited the file,
// and Champion refuses to touch it rather than overwrite their work.
func ParseSpawnerFile(raw []byte) (SpawnerFile, error) {
	var f SpawnerFile
	if len(bytes.TrimSpace(raw)) == 0 {
		return SpawnerFile{Objects: []SpawnerObject{}}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return SpawnerFile{}, fmt.Errorf("%w: %v", ErrInvalidSpawnerFile, err)
	}
	if f.Objects == nil {
		f.Objects = []SpawnerObject{}
	}
	for _, o := range f.Objects {
		if !strings.HasPrefix(o.CustomString, championTag) {
			return SpawnerFile{}, ErrForeignSpawnerEntry
		}
	}
	return f, nil
}

// Render is the deterministic file content (sorted by attempt tag, then position in the attempt).
func (f SpawnerFile) Render() []byte {
	objs := append([]SpawnerObject(nil), f.Objects...)
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].CustomString < objs[j].CustomString })
	if objs == nil {
		objs = []SpawnerObject{}
	}
	out, _ := json.MarshalIndent(SpawnerFile{Objects: objs}, "", "  ")
	return append(out, '\n')
}

// Entries are the spawner objects a plan stages: one per unit, all tagged with the attempt id.
// enableCEPersistency is false, so objectspawner.c creates them with ECE_NOLIFETIME and
// ECE_DYNAMIC_PERSISTENCY: an untouched object is not saved, a picked-up one persists in the
// player's inventory like any item.
func (p Plan) Entries() []SpawnerObject {
	return AttemptEntries(p.AttemptID(), p.className, p.quantity, [3]float64{p.x, p.y, p.z})
}

// AttemptEntries are the spawner objects one attempt stages, rebuilt from the attempt's durable facts
// (the ledger row stores exactly these). Plan.Entries is defined through it, so both always agree.
func AttemptEntries(attemptID, className string, quantity int, pos [3]float64) []SpawnerObject {
	out := make([]SpawnerObject, 0, quantity)
	for i := 0; i < quantity; i++ {
		out = append(out, SpawnerObject{Name: className, Pos: pos, Scale: 1, CustomString: fmt.Sprintf("%s:u%d", attemptID, i+1)})
	}
	return out
}

// SingleAttemptFiles are the exact bytes of the Champion spawner file with only this attempt staged,
// and with nothing staged (the empty file): what a verified read-back must hash to while staged and
// after the unstage.
func SingleAttemptFiles(attemptID, className string, quantity int, pos [3]float64) (staged, empty []byte) {
	empty = SpawnerFile{Objects: []SpawnerObject{}}.Render()
	staged = SpawnerFile{Objects: AttemptEntries(attemptID, className, quantity, pos)}.Render()
	return staged, empty
}

func belongsTo(o SpawnerObject, attemptID string) bool {
	return o.CustomString == attemptID || strings.HasPrefix(o.CustomString, attemptID+":")
}

// Stage returns the file with the plan's entries (replacing any earlier entries of the same attempt,
// so re-staging is idempotent).
func Stage(current SpawnerFile, p Plan) (SpawnerFile, error) {
	next := Unstage(current, p.AttemptID())
	next.Objects = append(next.Objects, p.Entries()...)
	if len(next.Objects) > MaxStagedObjects {
		return SpawnerFile{}, ErrTooManyStaged
	}
	return next, nil
}

// Unstage returns the file without any entry of the attempt. It must be applied as soon as the
// restart that spawned the objects is observed: every entry left in the file spawns again on every
// server start.
func Unstage(current SpawnerFile, attemptID string) SpawnerFile {
	out := SpawnerFile{Objects: []SpawnerObject{}}
	for _, o := range current.Objects {
		if !belongsTo(o, attemptID) {
			out.Objects = append(out.Objects, o)
		}
	}
	return out
}

// SHA256 is the hex digest used to verify backups and uploads.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Diff is a minimal line diff ("  " same, "- " removed, "+ " added) for human review.
func Diff(before, after []byte) string {
	a, b := splitLines(before), splitLines(after)
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			sb.WriteString("  " + a[i] + "\n")
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			sb.WriteString("- " + a[i] + "\n")
			i++
		default:
			sb.WriteString("+ " + b[j] + "\n")
			j++
		}
	}
	for ; i < n; i++ {
		sb.WriteString("- " + a[i] + "\n")
	}
	for ; j < m; j++ {
		sb.WriteString("+ " + b[j] + "\n")
	}
	return sb.String()
}

func splitLines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// RollbackPlan is what an operator (or a future executor) must do to stage and to undo one change
// safely. It is a description only.
type RollbackPlan struct {
	ArtifactPath string
	BeforeSHA256 string // the verified backup's digest; the file is restored to exactly this
	AfterSHA256  string // what the upload must read back as
	BackupName   string
	Steps        []string
}

// PlanRollback describes backup, staged write, verification and restore for a before/after pair.
func PlanRollback(p Plan, before, after []byte) RollbackPlan {
	backup := fmt.Sprintf("%s/backup/%s.%s.bak", ArtifactDir, ArtifactFile, SHA256(before)[:12])
	return RollbackPlan{ArtifactPath: ArtifactRelPath, BeforeSHA256: SHA256(before), AfterSHA256: SHA256(after), BackupName: backup,
		Steps: []string{
			"download " + ArtifactRelPath + " and confirm its SHA-256 is " + SHA256(before),
			"upload that exact content to " + backup + ", download it again and confirm the same SHA-256 (verified backup)",
			"immediately before writing, download " + ArtifactRelPath + " again: if the SHA-256 changed, stop (someone else edited it)",
			"upload the staged content, download it and confirm SHA-256 " + SHA256(after) + "; on mismatch or partial upload, re-upload the backup and verify",
			"only then may the owner perform the " + RequiredActionOwnerConfirmedRestart + " for attempt " + p.AttemptID(),
			"as soon as the restart is observed, upload the content with attempt " + p.AttemptID() + " unstaged and verify it - every remaining entry spawns again on every start",
			"to roll back before any restart: re-upload the backup and verify SHA-256 " + SHA256(before),
		}}
}
