// Package canary is the Champion Shop Phase 2C.2 live-server canary PREPARATION
// (docs/SHOP_DELIVERY_PHASE2C2.md). It is NON-EXECUTING: it computes the proposed cfggameplay.json
// patch, validates a drop point, previews the single-item canary plan, decides restart/unstage steps
// from evidence, and describes the upload sequence and the owner gates. It has no Nitrado client, no
// HTTP code and no database code - nothing here can upload a file, restart a server or change a
// delivery.
package canary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// ConfigRelPath is the mission-relative gameplay configuration the patch targets.
const ConfigRelPath = "cfggameplay.json"

// Spawner paths. LostCityRelPath is the spawner file that exists in custom/; LosstCityRelPath is the
// misspelled reference that used to be configured (it never loaded: "does not exist" on every boot).
const (
	LostCityRelPath  = "custom/The_Lost_City.json"
	LosstCityRelPath = "custom/The_Losst_City.json"
)

var (
	ErrConfigInvalid     = errors.New("cfggameplay.json is not valid JSON")
	ErrNoSpawnerArray    = errors.New("cfggameplay.json has no single WorldsData.objectSpawnersArr array")
	ErrSpawnerArrayShape = errors.New("objectSpawnersArr is not an array of strings")
	ErrNoChange          = errors.New("the configuration already matches the proposal: nothing to change")
	ErrPatchNotMinimal   = errors.New("internal: the patch changed something other than objectSpawnersArr")
)

// ConfigPatch is the proposal for one exact current file. It holds file content (cfggameplay.json has
// no secret) but never a path outside the mission folder.
type ConfigPatch struct {
	Purpose        string // SHOP_REFERENCE or LOST_CITY_RESTORE: never both in one change
	Path           string
	CurrentSHA256  string
	ProposedSHA256 string
	CurrentBytes   int
	ProposedBytes  int
	Before, After  []string // objectSpawnersArr entries
	Changes        []string
	Diff           string
	Proposed       []byte
	AffectedFiles  []AffectedFile
	Preconditions  []string
	BackupPlan     []string
	RollbackPlan   []string
}

// Patch purposes. Shop activation and the Lost City map are separate owner decisions: a patch never
// carries both.
const (
	PurposeShopReference   = "SHOP_REFERENCE"
	PurposeLostCityRestore = "LOST_CITY_RESTORE"
)

// AffectedFile is one file a gate touches.
type AffectedFile struct {
	Path   string
	Change string // MODIFY / CREATE / REPLACE
	Gate   string
}

// ProposePatch is the Shop change: reference the Champion spawner file. Every existing entry
// (including any Lost City reference, spelled right or wrong) is kept exactly; nothing is removed
// or corrected. The Champion file must already exist, empty, on the server (Gate A) before this patch
// is applied (Gate B), so no restart can ever meet a reference to a missing file.
func ProposePatch(current []byte) (ConfigPatch, error) {
	p, err := proposeArray(current, func(before []string) ([]string, []string) {
		for _, e := range before {
			if e == nitradodelivery.ArtifactRelPath {
				return before, nil
			}
		}
		return append(append([]string{}, before...), nitradodelivery.ArtifactRelPath), []string{"add " + nitradodelivery.ArtifactRelPath}
	})
	if err != nil {
		return p, err
	}
	empty, emptySHA := EmptyArtifact()
	p.Purpose = PurposeShopReference
	p.AffectedFiles = []AffectedFile{
		{Path: nitradodelivery.ArtifactRelPath, Change: "CREATE", Gate: GateA},
		{Path: ConfigRelPath, Change: "MODIFY", Gate: GateB},
	}
	p.Preconditions = []string{
		"Gate A verified: " + nitradodelivery.ArtifactRelPath + " exists and reads back as the empty spawner file (" + fmt.Sprint(len(empty)) + " bytes, SHA-256 " + emptySHA + ")",
		"re-read the Champion file immediately before this write: if it is missing or not the empty file, stop",
	}
	return p, nil
}

// ProposeLostCityRestore is the SEPARATE, optional owner change that re-enables the Lost City map:
// a misspelled custom/The_Losst_City.json reference is corrected in place, otherwise
// custom/The_Lost_City.json is appended. It never touches the Champion entry.
func ProposeLostCityRestore(current []byte) (ConfigPatch, error) {
	p, err := proposeArray(current, func(before []string) ([]string, []string) {
		var after, changes []string
		has := false
		for _, e := range before {
			switch e {
			case LosstCityRelPath:
				if has {
					changes = append(changes, "drop duplicate "+LosstCityRelPath)
					continue
				}
				changes = append(changes, "correct "+LosstCityRelPath+" -> "+LostCityRelPath)
				e = LostCityRelPath
				has = true
			case LostCityRelPath:
				if has {
					continue
				}
				has = true
			}
			after = append(after, e)
		}
		if !has {
			after = append(after, LostCityRelPath)
			changes = append(changes, "add "+LostCityRelPath)
		}
		return after, changes
	})
	if err != nil {
		return p, err
	}
	p.Purpose = PurposeLostCityRestore
	p.AffectedFiles = []AffectedFile{{Path: ConfigRelPath, Change: "MODIFY", Gate: GateLostCity}}
	p.Preconditions = []string{
		LostCityRelPath + " still exists and parses as a spawner file (re-verify its SHA-256 on the day)",
		"an owner decision independent of Shop activation: it re-creates every Lost City object at every start",
	}
	return p, nil
}

// EmptyArtifact is the Champion spawner file with no objects - the content Gate A creates and Gate G
// restores. The server spawns nothing from it.
func EmptyArtifact() ([]byte, string) {
	b := nitradodelivery.SpawnerFile{Objects: []nitradodelivery.SpawnerObject{}}.Render()
	return b, nitradodelivery.SHA256(b)
}

var (
	ErrArtifactMissing  = errors.New("the Champion spawner file does not exist: create the empty file first (Gate A)")
	ErrArtifactNotEmpty = errors.New("the Champion spawner file is not the verified empty file")
)

// CheckReferencePrecondition is Gate B's guard: the configuration may only reference a Champion file
// that exists and is exactly the empty spawner file.
func CheckReferencePrecondition(artifact []byte, present bool) error {
	if !present {
		return ErrArtifactMissing
	}
	f, err := nitradodelivery.ParseSpawnerFile(artifact)
	if err != nil {
		return err
	}
	if _, sha := EmptyArtifact(); len(f.Objects) != 0 || nitradodelivery.SHA256(artifact) != sha {
		return ErrArtifactNotEmpty
	}
	return nil
}

func proposeArray(current []byte, edit func([]string) ([]string, []string)) (ConfigPatch, error) {
	var cur map[string]any
	if err := json.Unmarshal(current, &cur); err != nil {
		return ConfigPatch{}, fmt.Errorf("%w: %v", ErrConfigInvalid, err)
	}
	start, end, indent, err := spawnerArraySpan(current)
	if err != nil {
		return ConfigPatch{}, err
	}
	var before []string
	if err := json.Unmarshal(current[start:end], &before); err != nil {
		return ConfigPatch{}, ErrSpawnerArrayShape
	}
	if before == nil {
		before = []string{}
	}
	after, changes := edit(before)
	if len(changes) == 0 {
		return ConfigPatch{}, ErrNoChange
	}
	nl := "\n"
	if bytes.Contains(current, []byte("\r\n")) {
		nl = "\r\n"
	}
	var arr strings.Builder
	arr.WriteString("[")
	for i, e := range after {
		q, _ := json.Marshal(e)
		arr.WriteString(nl + indent + "\t" + string(q))
		if i < len(after)-1 {
			arr.WriteString(",")
		}
	}
	arr.WriteString(nl + indent + "]")
	proposed := make([]byte, 0, len(current)+len(arr.String()))
	proposed = append(proposed, current[:start]...)
	proposed = append(proposed, arr.String()...)
	proposed = append(proposed, current[end:]...)
	if err := verifyMinimal(cur, proposed, after); err != nil {
		return ConfigPatch{}, err
	}
	p := ConfigPatch{
		Path: ConfigRelPath, CurrentSHA256: nitradodelivery.SHA256(current), ProposedSHA256: nitradodelivery.SHA256(proposed),
		CurrentBytes: len(current), ProposedBytes: len(proposed), Before: before, After: after, Changes: changes,
		Diff: nitradodelivery.Diff(current, proposed), Proposed: proposed,
	}
	backup := ConfigBackupPath(p.CurrentSHA256)
	p.BackupPlan = []string{
		"download " + ConfigRelPath + " and confirm SHA-256 " + p.CurrentSHA256 + " (" + fmt.Sprint(p.CurrentBytes) + " bytes); any other digest: stop, recompute the proposal",
		"upload that exact content to " + backup + ", download it again and confirm SHA-256 " + p.CurrentSHA256,
		"keep a local copy of the same bytes outside the server (second backup)",
	}
	p.RollbackPlan = []string{
		"upload the verified backup " + backup + " content to " + ConfigRelPath + " and confirm SHA-256 " + p.CurrentSHA256,
		"the rollback takes effect at the next server start; until then the running server keeps the configuration it booted with",
	}
	return p, nil
}

// ConfigBackupPath is the mission-relative backup name for a configuration digest.
func ConfigBackupPath(sha string) string {
	return nitradodelivery.ArtifactDir + "/backup/" + ConfigRelPath + "." + sha[:12] + ".bak"
}

// spawnerArraySpan finds the byte span of the objectSpawnersArr array value inside WorldsData and the
// indentation of its key line. The key must occur exactly once.
func spawnerArraySpan(b []byte) (start, end int, indent string, err error) {
	key := []byte(`"objectSpawnersArr"`)
	if bytes.Count(b, key) != 1 {
		return 0, 0, "", ErrNoSpawnerArray
	}
	k := bytes.Index(b, key)
	i := k + len(key)
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != ':' {
		return 0, 0, "", ErrNoSpawnerArray
	}
	i++
	for i < len(b) && isSpace(b[i]) {
		i++
	}
	if i >= len(b) || b[i] != '[' {
		return 0, 0, "", ErrSpawnerArrayShape
	}
	start = i
	inStr, esc := false, false
	for j := start + 1; j < len(b); j++ {
		c := b[j]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && (c == '[' || c == '{'):
			return 0, 0, "", ErrSpawnerArrayShape
		case !inStr && c == ']':
			ls := bytes.LastIndexByte(b[:k], '\n') + 1
			ind := b[ls:k]
			if len(bytes.TrimLeft(ind, " \t")) != 0 {
				ind = nil
			}
			return start, j + 1, string(ind), nil
		}
	}
	return 0, 0, "", ErrSpawnerArrayShape
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// verifyMinimal proves the proposal parses, carries exactly the planned entries, keeps
// objectSpawnersArr under WorldsData, and is deep-equal to the current file in every other key.
func verifyMinimal(cur map[string]any, proposed []byte, want []string) error {
	var next map[string]any
	if err := json.Unmarshal(proposed, &next); err != nil {
		return fmt.Errorf("%w: proposal does not parse: %v", ErrPatchNotMinimal, err)
	}
	wdN, ok := next["WorldsData"].(map[string]any)
	if !ok {
		return ErrNoSpawnerArray
	}
	wdC, ok := cur["WorldsData"].(map[string]any)
	if !ok {
		return ErrNoSpawnerArray
	}
	got, _ := json.Marshal(wdN["objectSpawnersArr"])
	exp, _ := json.Marshal(want)
	if !bytes.Equal(got, exp) {
		return fmt.Errorf("%w: spawner entries differ", ErrPatchNotMinimal)
	}
	strip := func(m map[string]any, wd map[string]any) map[string]any {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		w := map[string]any{}
		for k, v := range wd {
			if k != "objectSpawnersArr" {
				w[k] = v
			}
		}
		c["WorldsData"] = w
		return c
	}
	if !reflect.DeepEqual(strip(cur, wdC), strip(next, wdN)) {
		return ErrPatchNotMinimal
	}
	return nil
}
