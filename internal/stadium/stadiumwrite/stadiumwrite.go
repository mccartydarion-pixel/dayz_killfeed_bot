// Package stadiumwrite writes the Stadium to a Nitrado DayZ server (docs/STADIUM.md): the
// Champion-owned spawner file custom/champion_stadium.json and, for a build, the one line of
// cfggameplay.json that references it. It is one of the few places that may call the Nitrado
// write primitives (the isolation test in internal/shop/missionwrite allows exactly this package,
// reachable from internal/app/saas_api_stadium.go alone).
//
// One Apply writes at most these files, all by fixed name inside the mission folder:
//
//   - custom/champion_stadium.json: the rendered layout (or the empty file, for a remove);
//     custom/ is created when the mission has no such folder, since it is the only folder the
//     console game host receives user files from;
//   - champion/backup/cfggameplay.json.<sha12>.bak: a verified copy of the current configuration,
//     the same backup the Shop's gate-b-rollback tool restores (created before the config write,
//     never inside custom/, so it never reaches the game);
//   - cfggameplay.json: only the text of WorldsData.objectSpawnersArr changes
//     (maprotation.EditChampionSpawner), and only when the entry is missing.
//
// It follows the Shop's write rules: everything is inspected before the first write and
// re-verified immediately before each write (the file, the configuration, the server's boot
// identity and status); a changed server means no write. A write is one upload token and one
// transfer, never retried, and only an independent read-back decides what happened: verified,
// proven unchanged, or uncertain - and an uncertain outcome stops the procedure and tells the
// owner what to check. Nothing here restarts a server. Tokens, URLs and file contents never
// appear in an Outcome or a log line.
package stadiumwrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/stadium"
)

// Remote is every Nitrado call a build makes: the three reads of maprotation.Reader and the
// three write primitives.
type Remote interface {
	maprotation.Reader
	RequestUploadToken(ctx context.Context, serviceID, dir, name string) (nitrado.UploadTarget, error)
	PostUpload(ctx context.Context, t nitrado.UploadTarget, data []byte) error
	Mkdir(ctx context.Context, serviceID, parent, name string) error
}

// Outcome statuses.
const (
	StatusBuilt     = "BUILT"     // the file is verified in place and cfggameplay.json references it
	StatusRemoved   = "REMOVED"   // the empty file is verified in place
	StatusFailed    = "FAILED"    // nothing harmful happened: every write is verified or proven unchanged
	StatusUncertain = "UNCERTAIN" // a read-back could not prove the state: the owner must check
)

// Step results.
const (
	ResultPass       = "PASS"
	ResultFail       = "FAIL"
	ResultSkipped    = "SKIPPED"
	ResultUnverified = "UNVERIFIED"
)

// Request is one build or remove.
type Request struct {
	ServiceID string
	// Payload is the exact file to write: the rendered layout, or stadium.EmptyFile() for a remove.
	Payload []byte
	// Reference makes the build ensure cfggameplay.json references the file. A remove leaves the
	// configuration as it is.
	Reference   bool
	ObjectCount int
}

// Step is one verified step of the procedure.
type Step struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail"`
}

// Outcome is what happened, in plain language and digests. It never carries a token, a URL, an
// account path or file content.
type Outcome struct {
	Status          string `json:"status"`
	File            string `json:"file"`
	SHA256          string `json:"sha256"`
	ObjectCount     int    `json:"objectCount"`
	FileVerified    bool   `json:"fileVerified"`
	Referenced      bool   `json:"referenced"`
	Steps           []Step `json:"steps"`
	RequiresRestart bool   `json:"requiresRestart"`
	Message         string `json:"message"`
	// ConfigBackup is the mission-relative path of the verified backup of cfggameplay.json a
	// build took before changing it (empty when nothing was changed).
	ConfigBackup string `json:"configBackup,omitempty"`
	// ConfigSHA256 is the digest of cfggameplay.json after the build.
	ConfigSHA256 string `json:"configSha256,omitempty"`
	// Writes is how many upload or mkdir requests were sent.
	Writes int       `json:"writes"`
	At     time.Time `json:"at"`
}

// Messages the owner sees.
const (
	msgEnableGameplay = "The server's mission folder with cfggameplay.json could not be found. On Nitrado, open the server's General Settings, tick \"Enable cfggameplay.json\", restart the server once and try again. Nothing was changed."
	msgNitrado        = "Nitrado could not be reached to read the server. Nothing was changed."
	msgChanged        = "The server changed while the stadium was being prepared (a file was edited or the server restarted). Nothing was written. Try again in a few minutes."
	msgBuilt          = "The stadium file was written and verified, and cfggameplay.json references it. The arena appears at the next server restart."
	msgRemoved        = "The stadium file is now empty and verified. Nothing spawns after the next server restart; cfggameplay.json still references the empty file, which is harmless."
)

var admFileRe = regexp.MustCompile(`^DayZServer_[A-Za-z0-9_]+_\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}\.ADM$`)

// readBackTimeout bounds a read-back that must still run after the caller's deadline passed: a
// write request was sent, and only the read-back can say what it did.
const readBackTimeout = 60 * time.Second

type session struct {
	ctx    context.Context
	rm     Remote
	svc    string
	paths  maprotation.Paths
	root   string
	out    *Outcome
	boots  []string
	status string
}

func (s *session) step(name, result, detail string) {
	s.out.Steps = append(s.out.Steps, Step{Name: name, Result: result, Detail: detail})
}

func (s *session) fail(message string) Outcome {
	s.out.Status, s.out.Message = StatusFailed, message
	return *s.out
}

func (s *session) uncertain(message string) Outcome {
	s.out.Status, s.out.Message = StatusUncertain, message
	return *s.out
}

// afterWrite is the context a read-back runs under: the caller's, or a fresh one when that has
// already ended.
func (s *session) afterWrite() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), readBackTimeout)
}

// fileState is one mission-relative file, located by directory listings (never by a download
// error, which cannot tell "absent" from "failed"). present is false when the file or a folder
// on the way does not exist.
type fileState struct {
	present bool
	data    []byte
}

func (f fileState) sha() string {
	if !f.present {
		return "ABSENT"
	}
	return stadium.SHA256(f.data)
}

func (f fileState) equal(g fileState) bool {
	return f.present == g.present && bytes.Equal(f.data, g.data)
}

// listDir lists a mission-relative folder ("" is the mission folder itself).
func (s *session) listDir(ctx context.Context, rel string) ([]nitrado.DirEntry, error) {
	dir := s.paths.MissionDir
	if rel != "" {
		p, err := capability.SafePath(s.root, s.paths.MissionDir+"/"+rel)
		if err != nil {
			return nil, err
		}
		dir = p
	}
	return s.rm.ListEntries(ctx, s.svc, dir)
}

// dirExists reports whether a mission-relative folder exists, from its parent's listing.
func (s *session) dirExists(ctx context.Context, rel string) (bool, error) {
	parent, name := path.Split(rel)
	parent = strings.TrimSuffix(parent, "/")
	if parent != "" {
		ok, err := s.dirExists(ctx, parent)
		if err != nil || !ok {
			return false, err
		}
	}
	entries, err := s.listDir(ctx, parent)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == name {
			if !e.IsDir {
				return false, fmt.Errorf("%s is a file, not a folder", rel)
			}
			return true, nil
		}
	}
	return false, nil
}

// stateOf reads a mission-relative file.
func (s *session) stateOf(ctx context.Context, rel string) (fileState, error) {
	dir, name := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir != "" {
		ok, err := s.dirExists(ctx, dir)
		if err != nil || !ok {
			return fileState{}, err
		}
	}
	entries, err := s.listDir(ctx, dir)
	if err != nil {
		return fileState{}, err
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if e.IsDir {
			return fileState{}, fmt.Errorf("%s is a folder, not a file", rel)
		}
		full, err := capability.SafePath(s.root, s.paths.MissionDir+"/"+rel)
		if err != nil {
			return fileState{}, err
		}
		b, err := s.rm.ReadLog(ctx, s.svc, full)
		if err != nil {
			return fileState{}, err
		}
		return fileState{present: true, data: b}, nil
	}
	return fileState{}, nil
}

// upload is one token and one transfer of data to a mission-relative file whose folder exists.
// The result is decided by the caller's read-back; refused reports that a request failed.
func (s *session) upload(ctx context.Context, rel string, data []byte) (refused bool, why string) {
	dir, name := path.Split(rel)
	parent := s.paths.MissionDir
	if d := strings.TrimSuffix(dir, "/"); d != "" {
		p, err := capability.SafePath(s.root, parent+"/"+d)
		if err != nil {
			return true, "unsafe path"
		}
		parent = p
	}
	s.out.Writes++
	target, err := s.rm.RequestUploadToken(ctx, s.svc, parent, name)
	if err != nil {
		return true, "upload token: " + errText(err)
	}
	if err := s.rm.PostUpload(ctx, target, data); err != nil {
		return true, "transfer: " + errText(err)
	}
	return false, ""
}

// mkdir creates one mission-relative folder (its parent must exist) and verifies it by listing.
func (s *session) mkdir(ctx context.Context, rel string) error {
	parent, name := path.Split(rel)
	parent = strings.TrimSuffix(parent, "/")
	dir := s.paths.MissionDir
	if parent != "" {
		p, err := capability.SafePath(s.root, dir+"/"+parent)
		if err != nil {
			return err
		}
		dir = p
	}
	s.out.Writes++
	mkErr := s.rm.Mkdir(ctx, s.svc, dir, name)
	rctx, cancel := s.afterWrite()
	defer cancel()
	ok, lerr := s.dirExists(rctx, rel)
	switch {
	case lerr != nil:
		return fmt.Errorf("the folder could not be listed after the request (%s)", errText(mkErr))
	case !ok:
		return fmt.Errorf("the folder was not created (%s)", errText(mkErr))
	}
	return nil
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	var we *nitrado.WriteError
	if errors.As(err, &we) {
		return we.Error()
	}
	if errors.Is(err, nitrado.ErrInsecureUploadURL) {
		return err.Error()
	}
	return "error"
}

// bootIdentity is the newest DayZServer_*.ADM across the server's log mounts ("" when none can
// be listed) and the gameserver status: a new boot or a changed status between the inspection
// and a write means the server is not what was inspected.
func (s *session) bootIdentity(ctx context.Context) (newest, status string) {
	gs, err := s.rm.GameserverFacts(ctx, s.svc)
	if err != nil {
		return "", ""
	}
	var dirs []string
	if gs.GamePath != "" {
		if d, err := capability.SafePath(s.root, gs.GamePath+"/config"); err == nil {
			dirs = append(dirs, d)
		}
	}
	if gs.Game != "" {
		for _, mount := range []string{"ftproot", "noftp"} {
			if d, err := capability.SafePath(s.root, s.root+"/"+mount+"/"+gs.Game+"/config"); err == nil && (len(dirs) == 0 || dirs[0] != d) {
				dirs = append(dirs, d)
			}
		}
	}
	names := []string{}
	for _, d := range dirs {
		entries, err := s.rm.ListEntries(ctx, s.svc, d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir && admFileRe.MatchString(e.Name) {
				names = append(names, e.Name)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", gs.Status
	}
	return names[len(names)-1], gs.Status
}

// serverUnchanged re-reads the boot identity and status and compares them with the inspection.
func (s *session) serverUnchanged(ctx context.Context) bool {
	newest, status := s.bootIdentity(ctx)
	if s.boots == nil {
		return status == s.status
	}
	return newest == s.boots[0] && status == s.status
}

// Apply performs the build or remove. It never returns an error: everything it found is in the
// Outcome, whose Status says whether the server now holds the stadium.
func Apply(ctx context.Context, rm Remote, req Request) Outcome {
	out := &Outcome{Status: StatusFailed, File: stadium.FileRelPath, ObjectCount: req.ObjectCount, RequiresRestart: true, Steps: []Step{}, At: time.Now().UTC()}
	s := &session{ctx: ctx, rm: rm, svc: req.ServiceID, out: out}
	if rm == nil || req.ServiceID == "" {
		return s.fail("The server connection is missing. Nothing was changed.")
	}
	if _, err := stadium.ParseFile(req.Payload); err != nil || len(req.Payload) == 0 {
		return s.fail("The stadium file could not be prepared. Nothing was changed.")
	}
	out.SHA256 = stadium.SHA256(req.Payload)

	// 1. The mission folder, found by its cfggameplay.json.
	paths, err := maprotation.Locate(ctx, rm, req.ServiceID)
	if err != nil {
		if errors.Is(err, maprotation.ErrServerLookup) {
			return s.fail(msgNitrado)
		}
		s.step("find the mission folder", ResultFail, "no mission folder with cfggameplay.json under the server's file browser")
		return s.fail(msgEnableGameplay)
	}
	s.paths, s.root = paths, paths.Root()
	if s.root == "" {
		return s.fail(msgNitrado)
	}
	s.step("find the mission folder", ResultPass, capability.Canonical(paths.MissionDir))

	// 2. The configuration and the edit that references the file.
	cfg, err := s.stateOf(ctx, maprotation.GameplayFile)
	if err != nil || !cfg.present || len(cfg.data) == 0 || len(cfg.data) > maprotation.MaxGameplayBytes {
		s.step("read cfggameplay.json", ResultFail, "the file could not be downloaded")
		return s.fail(msgEnableGameplay)
	}
	edit, err := maprotation.EditChampionSpawner(cfg.data, stadium.FileName)
	if err != nil {
		s.step("read cfggameplay.json", ResultFail, err.Error())
		return s.fail("The server's cfggameplay.json could not be edited safely: " + err.Error() + ". Nothing was changed.")
	}
	referencedNow := !edit.Changed
	s.step("read cfggameplay.json", ResultPass, fmt.Sprintf("%d bytes, SHA-256 %s, references the stadium file: %t", len(cfg.data), cfg.sha(), referencedNow))

	// 3. The custom/ folder and the current stadium file.
	customExists, err := s.dirExists(ctx, stadium.FileDir)
	if err != nil {
		s.step("list the custom folder", ResultFail, "the mission folder could not be listed")
		return s.fail(msgNitrado)
	}
	before := fileState{}
	if customExists {
		if before, err = s.stateOf(ctx, stadium.FileRelPath); err != nil {
			s.step("read the stadium file", ResultFail, "the file could not be read")
			return s.fail(msgNitrado)
		}
	}
	if before.present {
		if _, perr := stadium.ParseFile(before.data); errors.Is(perr, stadium.ErrForeignFile) {
			s.step("read the stadium file", ResultFail, "the file holds objects Champion did not write")
			return s.fail("custom/champion_stadium.json on the server contains objects Champion did not write. Remove or rename it in the Nitrado file browser first. Nothing was changed.")
		} else if perr != nil {
			s.step("read the stadium file", ResultPass, fmt.Sprintf("%d bytes, SHA-256 %s; not valid spawner JSON (an interrupted write?), it will be replaced", len(before.data), before.sha()))
		} else {
			s.step("read the stadium file", ResultPass, fmt.Sprintf("%d bytes, SHA-256 %s", len(before.data), before.sha()))
		}
	} else {
		s.step("read the stadium file", ResultPass, "absent")
	}

	// 4. The boot identity and status the writes are checked against.
	if newest, status := s.bootIdentity(ctx); newest != "" {
		s.boots, s.status = []string{newest}, status
		s.step("record the server's boot", ResultPass, "boot "+newest+", status "+status)
	} else {
		s.status = status
		s.step("record the server's boot", ResultUnverified, "no boot log could be listed; only the status ("+status+") is compared")
	}

	// 5. The stadium file.
	want := fileState{present: true, data: req.Payload}
	switch {
	case before.equal(want):
		s.step("write the stadium file", ResultSkipped, "the file already has exactly this content")
		out.FileVerified = true
	case !customExists && !req.Reference:
		s.step("write the stadium file", ResultSkipped, "there is no custom folder, so no stadium file exists")
		out.FileVerified = true
	default:
		if !customExists {
			if err := s.mkdir(ctx, stadium.FileDir); err != nil {
				s.step("create the custom folder", ResultFail, err.Error())
				return s.fail("The custom folder could not be created in the mission folder: " + err.Error() + ". Create it in the Nitrado file browser and try again. Nothing else was changed.")
			}
			s.step("create the custom folder", ResultPass, "custom/ created and listed")
		}
		// Re-verify immediately before the write (Nitrado has no conditional write).
		if now, err := s.stateOf(ctx, stadium.FileRelPath); err != nil || !now.equal(before) {
			s.step("write the stadium file", ResultFail, "the file changed since it was read")
			return s.fail(msgChanged)
		}
		if now, err := s.stateOf(ctx, maprotation.GameplayFile); err != nil || !now.equal(cfg) {
			s.step("write the stadium file", ResultFail, "cfggameplay.json changed since it was read")
			return s.fail(msgChanged)
		}
		if !s.serverUnchanged(ctx) {
			s.step("write the stadium file", ResultFail, "a new boot or a changed server status appeared")
			return s.fail(msgChanged)
		}
		refused, why := s.upload(ctx, stadium.FileRelPath, req.Payload)
		rctx, cancel := s.afterWrite()
		after, err := s.stateOf(rctx, stadium.FileRelPath)
		cancel()
		switch {
		case err != nil:
			s.step("write the stadium file", ResultUnverified, "the file could not be read back after the request")
			return s.uncertain("The stadium file could not be read back after the upload. Check custom/champion_stadium.json in the Nitrado file browser before the next restart, then try again.")
		case after.equal(want):
			s.step("write the stadium file", ResultPass, fmt.Sprintf("read back %d bytes, SHA-256 %s", len(after.data), after.sha()))
			out.FileVerified = true
		case after.equal(before) && refused:
			s.step("write the stadium file", ResultFail, "refused ("+why+"); the file is verified unchanged")
			return s.fail("The file server refused the upload (" + why + "). Nothing was changed. Try again later.")
		case after.equal(before):
			s.step("write the stadium file", ResultFail, "the file server reported success but the file is unchanged")
			return s.uncertain("The file server reported success but custom/champion_stadium.json is unchanged. Check it in the Nitrado file browser before the next restart.")
		default:
			s.step("write the stadium file", ResultFail, fmt.Sprintf("read back %d bytes, SHA-256 %s: neither the old nor the new content", len(after.data), after.sha()))
			return s.uncertain("custom/champion_stadium.json on the server is neither the old nor the new content. Check it in the Nitrado file browser before the next restart (upload the file from \"Download file\" by hand if needed).")
		}
	}

	// 6. The reference in cfggameplay.json.
	switch {
	case !req.Reference:
		out.Referenced = referencedNow
		s.step("reference the file in cfggameplay.json", ResultSkipped, fmt.Sprintf("a remove keeps the configuration (referenced: %t)", referencedNow))
	case referencedNow:
		out.Referenced = true
		out.ConfigSHA256 = cfg.sha()
		s.step("reference the file in cfggameplay.json", ResultSkipped, "objectSpawnersArr already lists custom/champion_stadium.json")
	default:
		if o, done := s.reference(cfg, edit); done {
			return o
		}
	}

	// 7. Did the server restart meanwhile? Informational: the files are verified either way.
	if s.boots != nil {
		if newest, status := s.bootIdentity(ctx); newest == "" {
			s.step("no restart during the build", ResultUnverified, "the boot log could not be listed again")
		} else if newest != s.boots[0] || status != s.status {
			s.step("no restart during the build", ResultFail, "the server restarted while the files were being written; the written files are verified, the arena appears at the restart after this one at the latest")
		} else {
			s.step("no restart during the build", ResultPass, "boot "+newest+", status "+status)
		}
	}
	if !req.Reference {
		out.Status, out.Message = StatusRemoved, msgRemoved
		return *out
	}
	out.Status, out.Message = StatusBuilt, msgBuilt
	return *out
}

// reference takes the verified backup and writes the edited configuration. done is true when the
// procedure ends here (with the returned outcome).
func (s *session) reference(cfg fileState, edit maprotation.GameplayEdit) (Outcome, bool) {
	ctx := s.ctx
	out := s.out
	backup := canary.ConfigBackupPath(cfg.sha())
	out.ConfigBackup = backup
	state, err := s.stateOf(ctx, backup)
	if err != nil {
		s.step("back up cfggameplay.json", ResultFail, "the backup folder could not be listed")
		return s.fail(msgNitrado), true
	}
	switch {
	case state.present && bytes.Equal(state.data, cfg.data):
		s.step("back up cfggameplay.json", ResultPass, backup+" already holds the current configuration")
	case state.present:
		s.step("back up cfggameplay.json", ResultFail, backup+" exists with different content")
		return s.fail("A file already exists at " + backup + " with different content. Move it away in the Nitrado file browser and try again. The stadium file is in place; cfggameplay.json is untouched."), true
	default:
		for _, dir := range []string{path.Dir(path.Dir(backup)), path.Dir(backup)} {
			ok, err := s.dirExists(ctx, dir)
			if err != nil {
				s.step("back up cfggameplay.json", ResultFail, "the backup folder could not be listed")
				return s.fail(msgNitrado), true
			}
			if ok {
				continue
			}
			if err := s.mkdir(ctx, dir); err != nil {
				s.step("back up cfggameplay.json", ResultFail, dir+": "+err.Error())
				return s.fail("The backup folder " + dir + " could not be created: " + err.Error() + ". The stadium file is in place; cfggameplay.json is untouched."), true
			}
		}
		refused, why := s.upload(ctx, backup, cfg.data)
		rctx, cancel := s.afterWrite()
		got, err := s.stateOf(rctx, backup)
		cancel()
		if err != nil || !got.present || !bytes.Equal(got.data, cfg.data) {
			detail := "the backup could not be verified"
			if refused {
				detail += " (" + why + ")"
			}
			s.step("back up cfggameplay.json", ResultFail, detail)
			return s.fail("The backup of cfggameplay.json could not be written and verified. The stadium file is in place; cfggameplay.json is untouched. Try again later."), true
		}
		s.step("back up cfggameplay.json", ResultPass, fmt.Sprintf("%s: %d bytes, SHA-256 %s", backup, len(got.data), got.sha()))
	}

	// Re-verify immediately before the overwrite.
	if now, err := s.stateOf(ctx, maprotation.GameplayFile); err != nil || !now.equal(cfg) {
		s.step("reference the file in cfggameplay.json", ResultFail, "cfggameplay.json changed since it was read")
		return s.fail("cfggameplay.json changed while the stadium was being built. The stadium file is in place but not referenced yet; try again."), true
	}
	if !s.serverUnchanged(ctx) {
		s.step("reference the file in cfggameplay.json", ResultFail, "a new boot or a changed server status appeared")
		return s.fail("The server restarted while the stadium was being built. The stadium file is in place but not referenced yet; try again in a few minutes."), true
	}
	refused, why := s.upload(ctx, maprotation.GameplayFile, edit.Out)
	rctx, cancel := s.afterWrite()
	after, err := s.stateOf(rctx, maprotation.GameplayFile)
	cancel()
	want := fileState{present: true, data: edit.Out}
	switch {
	case err != nil:
		s.step("reference the file in cfggameplay.json", ResultUnverified, "cfggameplay.json could not be read back after the request")
		return s.uncertain("cfggameplay.json could not be read back after the write. Check it in the Nitrado file browser before the next restart; a verified backup is at " + backup + "."), true
	case after.equal(want):
		out.Referenced, out.ConfigSHA256 = true, after.sha()
		s.step("reference the file in cfggameplay.json", ResultPass, fmt.Sprintf("read back %d bytes, SHA-256 %s; objectSpawnersArr = %q", len(after.data), after.sha(), edit.After))
		return Outcome{}, false
	case after.equal(cfg) && refused:
		s.step("reference the file in cfggameplay.json", ResultFail, "refused ("+why+"); cfggameplay.json is verified unchanged")
		return s.fail("The file server refused the write to cfggameplay.json (" + why + "). The stadium file is in place but not referenced; try again later."), true
	case after.equal(cfg):
		s.step("reference the file in cfggameplay.json", ResultFail, "the file server reported success but cfggameplay.json is unchanged")
		return s.uncertain("The file server reported success but cfggameplay.json is unchanged. Check it in the Nitrado file browser before the next restart."), true
	default:
		s.step("reference the file in cfggameplay.json", ResultFail, fmt.Sprintf("read back %d bytes, SHA-256 %s: neither the old nor the new content", len(after.data), after.sha()))
		return s.uncertain("cfggameplay.json on the server is neither the old nor the new content. Restore it from " + backup + " in the Nitrado file browser before the next restart."), true
	}
}
