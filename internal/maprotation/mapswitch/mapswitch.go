// Package mapswitch makes one map of a rotation the active map of a Nitrado DayZ server
// (docs/MAP_ROTATION.md). It is the only code outside internal/shop/missionwrite that calls the
// Nitrado write primitives, and the isolation test in missionwrite allows exactly this package.
// Only internal/app/map_rotation_worker.go may import it.
//
// One Apply writes at most two files, both in the mission folder and both by fixed name:
//
//   - cfggameplay.json: only the text of WorldsData.objectSpawnersArr changes (maprotation.EditSpawners);
//   - cfgplayerspawnpoints.xml: replaced by the chosen map's spawn file from custom/.
//
// It never creates a folder, never deletes and never writes anywhere else. The rules it follows:
//
//   - everything is downloaded and validated before the first write; any doubt means no write;
//   - the previous contents of both files are handed to the caller's saveBackup (a database row)
//     before the first write, and Apply refuses to write if that fails;
//   - a write is attempted once and is judged only by reading the file back;
//   - if the second file cannot be written and verified, the first is restored from the backup and
//     the restore is verified the same way;
//   - a file that already has the wanted content is not written again, so repeating an Apply after
//     a crash finishes the switch instead of applying it twice.
//
// File contents, upload tokens and upload URLs are never logged and never appear in a Result.
package mapswitch

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Remote is every Nitrado call a switch makes: the three reads and the two write primitives. There
// is no mkdir: a switch never creates a folder.
type Remote interface {
	maprotation.Reader
	RequestUploadToken(ctx context.Context, serviceID, dir, name string) (nitrado.UploadTarget, error)
	PostUpload(ctx context.Context, t nitrado.UploadTarget, data []byte) error
}

// Switch statuses (the PENDING state belongs to the caller's switch log).
const (
	StatusApplied    = "APPLIED"
	StatusFailed     = "FAILED"
	StatusRolledBack = "ROLLED_BACK"
)

// Backup is the previous content of the two live files.
type Backup struct {
	Gameplay []byte
	Spawns   []byte
}

// Request is one switch.
type Request struct {
	ServiceID string
	MapFile   string   // the chosen map's file in custom/
	SpawnFile string   // the chosen map's spawn file in custom/
	Owned     []string // the map files of every configured map of this installation
	// Prior is the backup saved by an earlier, interrupted Apply of this same switch. When set it
	// is kept (never replaced) and used for any restore.
	Prior *Backup
}

// Result is what happened, in plain language.
type Result struct {
	Status  string
	Message string
	// NeedsAttention: a file may have been left in a state Champion could not verify or restore.
	// The caller stops the rotation until a person has looked.
	NeedsAttention bool
	// Writes is how many upload requests were sent (restores included).
	Writes int
}

func failed(msg string) Result { return Result{Status: StatusFailed, Message: msg} }

// restoreTimeout bounds a restore that has to run after the caller's context ended.
const restoreTimeout = 90 * time.Second

type outcome int

const (
	written   outcome = iota // read-back equals the payload
	unchanged                // read-back equals the previous content: nothing was written
	uncertain                // anything else, or the read-back failed
)

type session struct {
	rm     Remote
	svc    string
	paths  maprotation.Paths
	writes int
}

// put uploads data as name in the mission folder, once, and judges the result by reading it back.
func (s *session) put(ctx context.Context, name string, before, data []byte) outcome {
	full, err := s.paths.File(s.paths.MissionDir, name)
	if err != nil {
		return unchanged
	}
	// Re-read immediately before the write (Nitrado has no conditional write): if the file is no
	// longer what was validated, nothing is sent.
	if cur, err := s.rm.ReadLog(ctx, s.svc, full); err != nil || !bytes.Equal(cur, before) {
		return unchanged
	}
	s.writes++
	target, err := s.rm.RequestUploadToken(ctx, s.svc, s.paths.MissionDir, name)
	if err == nil {
		_ = s.rm.PostUpload(ctx, target, data) // the read-back decides, not this answer
	}
	after, err := s.rm.ReadLog(context.WithoutCancel(ctx), s.svc, full)
	switch {
	case err != nil:
		return uncertain
	case bytes.Equal(after, data):
		return written
	case bytes.Equal(after, before):
		return unchanged
	}
	return uncertain
}

// restore puts want back as name and verifies it. It reports whether the file now equals want.
func (s *session) restore(ctx context.Context, name string, want []byte) bool {
	full, err := s.paths.File(s.paths.MissionDir, name)
	if err != nil {
		return false
	}
	if cur, err := s.rm.ReadLog(ctx, s.svc, full); err == nil && bytes.Equal(cur, want) {
		return true
	}
	s.writes++
	target, err := s.rm.RequestUploadToken(ctx, s.svc, s.paths.MissionDir, name)
	if err == nil {
		_ = s.rm.PostUpload(ctx, target, want)
	}
	after, err := s.rm.ReadLog(ctx, s.svc, full)
	return err == nil && bytes.Equal(after, want)
}

// Apply performs the switch. saveBackup must store the backup durably and return nil only when it
// is stored; it is called at most once, before the first write, and not at all when req.Prior is
// set or nothing needs writing.
func Apply(ctx context.Context, rm Remote, req Request, saveBackup func(context.Context, Backup) error) Result {
	if rm == nil || req.ServiceID == "" || saveBackup == nil {
		return failed("The switch was not started: the server connection is missing.")
	}
	if maprotation.ValidateMapFile(req.MapFile) != nil || maprotation.ValidateSpawnFile(req.SpawnFile) != nil {
		return failed("The map's file names are not allowed. Nothing was changed.")
	}
	paths, err := maprotation.Locate(ctx, rm, req.ServiceID)
	if err != nil {
		if errors.Is(err, maprotation.ErrServerLookup) {
			return failed("Nitrado could not be reached to read the server. Nothing was changed.")
		}
		return failed("The server's mission folder (with cfggameplay.json) could not be found. Nothing was changed.")
	}
	s := &session{rm: rm, svc: req.ServiceID, paths: paths}

	// 1. Everything exists, as a file, within the size limits.
	mission, err := maprotation.ListFiles(ctx, rm, req.ServiceID, paths.MissionDir)
	if err != nil {
		return failed("The mission folder could not be listed. Nothing was changed.")
	}
	custom, err := maprotation.ListFiles(ctx, rm, req.ServiceID, paths.CustomDir)
	if err != nil {
		return failed("The custom folder could not be listed. Nothing was changed.")
	}
	check := func(files map[string]int64, name string, max int64, what string) string {
		size, ok := files[name]
		switch {
		case !ok:
			return what + " (" + name + ") was not found."
		case size == 0:
			return what + " (" + name + ") is empty."
		case size > max:
			return what + " (" + name + ") is too large."
		}
		return ""
	}
	for _, problem := range []string{
		check(mission, maprotation.GameplayFile, maprotation.MaxGameplayBytes, "The server's cfggameplay.json"),
		check(mission, maprotation.SpawnPointsFile, maprotation.MaxSpawnBytes, "The server's cfgplayerspawnpoints.xml"),
		check(custom, req.MapFile, maprotation.MaxMapFileBytes, "The map file in the custom folder"),
		check(custom, req.SpawnFile, maprotation.MaxSpawnBytes, "The spawn file in the custom folder"),
	} {
		if problem != "" {
			return failed(problem + " Nothing was changed.")
		}
	}

	// 2. Download all four and validate.
	read := func(dir, name string, max int) ([]byte, bool) {
		full, err := paths.File(dir, name)
		if err != nil {
			return nil, false
		}
		b, err := rm.ReadLog(ctx, req.ServiceID, full)
		return b, err == nil && len(b) > 0 && len(b) <= max
	}
	liveGameplay, ok1 := read(paths.MissionDir, maprotation.GameplayFile, maprotation.MaxGameplayBytes)
	liveSpawns, ok2 := read(paths.MissionDir, maprotation.SpawnPointsFile, maprotation.MaxSpawnBytes)
	mapBytes, ok3 := read(paths.CustomDir, req.MapFile, maprotation.MaxMapFileBytes)
	newSpawns, ok4 := read(paths.CustomDir, req.SpawnFile, maprotation.MaxSpawnBytes)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return failed("One of the files could not be downloaded from the server. Nothing was changed.")
	}
	if err := maprotation.ValidateMapJSON(mapBytes); err != nil {
		return failed("The map file (" + req.MapFile + ") is not a valid JSON file. Nothing was changed.")
	}
	if err := maprotation.ValidateSpawnXML(newSpawns); err != nil {
		return failed("The spawn file (" + req.SpawnFile + ") is not a valid spawn point file: " + err.Error() + ". Nothing was changed.")
	}
	edit, err := maprotation.EditSpawners(liveGameplay, req.Owned, req.MapFile)
	if err != nil {
		return failed("The server's cfggameplay.json could not be edited safely: " + err.Error() + ". Nothing was changed.")
	}
	needGameplay, needSpawns := edit.Changed, !bytes.Equal(liveSpawns, newSpawns)
	if !needGameplay && !needSpawns {
		return Result{Status: StatusApplied, Message: "The map's files were already in place."}
	}

	// 3. The backup is durable before the first write.
	backup := req.Prior
	if backup == nil {
		backup = &Backup{Gameplay: liveGameplay, Spawns: liveSpawns}
		if err := saveBackup(ctx, *backup); err != nil {
			return failed("The backup of the server's files could not be saved, so nothing was changed.")
		}
	}

	// A restore must still run when the caller's deadline has passed.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	rollback := func(spawnsToo bool, why string) Result {
		okG := s.restore(rctx, maprotation.GameplayFile, backup.Gameplay)
		okS := !spawnsToo || s.restore(rctx, maprotation.SpawnPointsFile, backup.Spawns)
		if okG && okS {
			return Result{Status: StatusRolledBack, Message: why + " The server's files were put back as they were.", Writes: s.writes}
		}
		return Result{Status: StatusFailed, NeedsAttention: true, Writes: s.writes,
			Message: why + " The server's files could NOT be put back and verified. Check cfggameplay.json and cfgplayerspawnpoints.xml before the next restart; Champion keeps a backup of both."}
	}

	// 4. cfggameplay.json first.
	if needGameplay {
		switch s.put(ctx, maprotation.GameplayFile, liveGameplay, edit.Out) {
		case unchanged:
			if req.Prior != nil {
				// An earlier attempt of this switch may have written the spawn file already.
				return rollback(true, "cfggameplay.json could not be written.")
			}
			return Result{Status: StatusFailed, Message: "cfggameplay.json could not be written. Nothing was changed.", Writes: s.writes}
		case uncertain:
			return rollback(req.Prior != nil, "cfggameplay.json could not be written and verified.")
		}
	}
	// 5. Then the spawn points.
	if needSpawns {
		switch s.put(ctx, maprotation.SpawnPointsFile, liveSpawns, newSpawns) {
		case unchanged:
			return rollback(false, "cfgplayerspawnpoints.xml could not be written.")
		case uncertain:
			return rollback(true, "cfgplayerspawnpoints.xml could not be written and verified.")
		}
	}
	return Result{Status: StatusApplied, Message: "Both files were written and verified.", Writes: s.writes}
}
