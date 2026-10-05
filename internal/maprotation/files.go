package maprotation

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
)

var (
	ErrSpawnXML     = errors.New("the spawn file is not well-formed XML")
	ErrSpawnRoot    = errors.New("the spawn file does not start with <playerspawnpoints>")
	ErrSpawnNoPos   = errors.New("the spawn file has no <pos x=\"...\" z=\"...\"/> position")
	ErrMapFileJSON  = errors.New("the map file is not a JSON object")
	ErrMissionDir   = errors.New("the server's mission folder could not be found")
	ErrServerLookup = errors.New("the server's details could not be read from Nitrado")
)

// ValidateSpawnXML checks that b has the shape of cfgplayerspawnpoints.xml: well-formed XML whose
// root element is <playerspawnpoints> and which holds at least one <pos> with x and z.
func ValidateSpawnXML(b []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, utf8BOM)))
	// The structure is all that is checked, so a declared single-byte encoding is read as is.
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	root, positions := "", 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrSpawnXML
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if root == "" {
			root = start.Name.Local
			if root != "playerspawnpoints" {
				return ErrSpawnRoot
			}
			continue
		}
		if start.Name.Local == "pos" {
			hasX, hasZ := false, false
			for _, a := range start.Attr {
				hasX = hasX || (a.Name.Local == "x" && strings.TrimSpace(a.Value) != "")
				hasZ = hasZ || (a.Name.Local == "z" && strings.TrimSpace(a.Value) != "")
			}
			if hasX && hasZ {
				positions++
			}
		}
	}
	if root == "" {
		return ErrSpawnXML
	}
	if positions == 0 {
		return ErrSpawnNoPos
	}
	return nil
}

// ValidateMapJSON checks that b is a JSON object (an object-spawner file).
func ValidateMapJSON(b []byte) error {
	body := bytes.TrimPrefix(b, utf8BOM)
	if i := skipSpace(body, 0); !json.Valid(body) || i >= len(body) || body[i] != '{' {
		return ErrMapFileJSON
	}
	return nil
}

// Reader is every Nitrado call this package makes. All three are reads.
type Reader interface {
	GameserverFacts(ctx context.Context, serviceID string) (nitrado.GameserverFacts, error)
	ListEntries(ctx context.Context, serviceID, dir string) ([]nitrado.DirEntry, error)
	ReadLog(ctx context.Context, serviceID, path string) ([]byte, error)
}

// Paths are the server's mission folder and its custom folder, both verified to lie inside the
// service's own file-browser root.
type Paths struct {
	root       string
	MissionDir string
	CustomDir  string
	// missionMounts is the mission folder as it appears under each of the service's mounts
	// ("ftproot", "noftp"). The config files and the saved world can sit under different mounts.
	missionMounts []string
}

// File returns the full path of a file directly inside dir (the mission or the custom folder).
// name must be a plain file name.
func (p Paths) File(dir, name string) (string, error) {
	if (dir != p.MissionDir && dir != p.CustomDir) || name == "" || strings.ContainsAny(name, "/\\\x00") || name == "." || name == ".." {
		return "", capability.ErrUnsafePath
	}
	return capability.SafePath(p.root, dir+"/"+name)
}

// PlayersDBCandidates returns every place the saved-characters file can be: storage_1/players.db
// in the mission folder under each mount, the mount that holds the config files first. Each entry
// is the mission folder, its storage folder and the file's full path, all built from fixed names.
func (p Paths) PlayersDBCandidates() [][3]string {
	dirs := append([]string{p.MissionDir}, p.missionMounts...)
	seen := map[string]bool{}
	var out [][3]string
	for _, mission := range dirs {
		if mission == "" || seen[mission] {
			continue
		}
		seen[mission] = true
		dir, err := capability.SafePath(p.root, mission+"/"+StorageDir)
		if err != nil {
			continue
		}
		file, err := capability.SafePath(p.root, dir+"/"+PlayersDBFile)
		if err != nil {
			continue
		}
		out = append(out, [3]string{mission, dir, file})
	}
	return out
}

// PlayersDB returns the mission's storage folder and the full path of the saved-characters file
// inside it (<mission folder>/storage_1/players.db). Both are fixed names, built here so that no
// caller ever assembles them from anything else.
func (p Paths) PlayersDB() (dir, file string, err error) {
	if p.MissionDir == "" {
		return "", "", capability.ErrUnsafePath
	}
	if dir, err = capability.SafePath(p.root, p.MissionDir+"/"+StorageDir); err != nil {
		return "", "", err
	}
	if file, err = capability.SafePath(p.root, dir+"/"+PlayersDBFile); err != nil {
		return "", "", err
	}
	return dir, file, nil
}

var missionNameRe = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)?$`)
var gameNameRe = regexp.MustCompile(`^[a-z0-9]+$`)

// Locate finds the mission folder the server runs (the one holding cfggameplay.json). Read-only.
func Locate(ctx context.Context, rd Reader, serviceID string) (Paths, error) {
	gs, err := rd.GameserverFacts(ctx, serviceID)
	if err != nil {
		return Paths{}, ErrServerLookup
	}
	root := capability.FileRoot(gs.GamePath)
	if root == "" || !missionNameRe.MatchString(gs.Mission) || !gameNameRe.MatchString(gs.Game) {
		return Paths{}, ErrMissionDir
	}
	for _, mount := range []string{"ftproot", "noftp"} {
		dir, err := capability.SafePath(root, root+"/"+mount+"/"+gs.Game+"_missions/"+gs.Mission)
		if err != nil {
			continue
		}
		entries, err := rd.ListEntries(ctx, serviceID, dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Name == GameplayFile && !e.IsDir {
				custom, err := capability.SafePath(root, dir+"/"+CustomDir)
				if err != nil {
					return Paths{}, ErrMissionDir
				}
				var mounts []string
				for _, m := range []string{"ftproot", "noftp"} {
					if d, err := capability.SafePath(root, root+"/"+m+"/"+gs.Game+"_missions/"+gs.Mission); err == nil {
						mounts = append(mounts, d)
					}
				}
				return Paths{root: root, MissionDir: dir, CustomDir: custom, missionMounts: mounts}, nil
			}
		}
	}
	return Paths{}, ErrMissionDir
}

// ListFiles lists the files (not folders) directly inside dir: name -> size.
func ListFiles(ctx context.Context, rd Reader, serviceID, dir string) (map[string]int64, error) {
	entries, err := rd.ListEntries(ctx, serviceID, dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			out[e.Name] = e.Size
		}
	}
	return out, nil
}
