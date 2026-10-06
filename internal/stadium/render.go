package stadium

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// FileName is the Champion-owned spawner file inside the mission's custom/ folder - the only
// folder Nitrado's console game host receives user files from (docs/SHOP_CUSTOM_RELOCATION.md).
const (
	FileName    = "champion_stadium.json"
	FileDir     = "custom"
	FileRelPath = FileDir + "/" + FileName // relative to the mission folder
)

// File is the spawner file shape (ObjectSpawnerJson in objectspawner.c).
type File struct {
	Objects []Object `json:"Objects"`
}

// Render is the exact file content for a list of objects: two-space indent, a trailing newline,
// objects in the given order. The same objects always render to the same bytes.
func Render(objs []Object) []byte {
	if objs == nil {
		objs = []Object{}
	}
	out, _ := json.MarshalIndent(File{Objects: objs}, "", "  ")
	return append(out, '\n')
}

// EmptyFile is the file with nothing in it: what Remove writes so nothing spawns after the next
// restart while cfggameplay.json keeps referencing the file.
func EmptyFile() []byte { return Render(nil) }

// SHA256 is the lowercase hex digest used in outcomes and the stadium record.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var (
	// ErrForeignFile: the file on the server holds an entry Champion did not write. Champion then
	// refuses to overwrite it rather than destroy somebody's work.
	ErrForeignFile = errors.New("the stadium file on the server contains objects Champion did not write")
	// ErrInvalidFile: the file is not spawner JSON.
	ErrInvalidFile = errors.New("the stadium file on the server is not a valid object-spawner file")
)

// ParseFile parses a file read back from the server. Empty content is an empty file. Every entry
// must carry the stadium tag; anything else is somebody else's and is reported as foreign.
func ParseFile(raw []byte) (File, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return File{Objects: []Object{}}, nil
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("%w: %v", ErrInvalidFile, err)
	}
	if f.Objects == nil {
		f.Objects = []Object{}
	}
	for _, o := range f.Objects {
		if !strings.HasPrefix(o.CustomString, CustomStringPrefix) {
			return File{}, ErrForeignFile
		}
	}
	return f, nil
}
