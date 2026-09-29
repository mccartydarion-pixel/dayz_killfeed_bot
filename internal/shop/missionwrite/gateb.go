package missionwrite

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// Gate B (docs/SHOP_GATE_B_CONFIG.md): the one overwrite of a server-owner file. cfggameplay.json is
// replaced by the byte-preserving patch that appends champion/champion_shop_delivery.json to
// objectSpawnersArr, only after a verified server-side backup of the exact original bytes exists.
// Its rollback restores that backup and is a separate, separately authorized operation.
const (
	OpReferenceChampionFile Operation = "gate-b-reference"
	OpRestoreConfig         Operation = "gate-b-rollback"
)

var (
	ErrChampionNotEmpty  = errors.New("missionwrite: the Champion spawner file is not exactly the verified empty file")
	ErrAlreadyReferenced = errors.New("missionwrite: cfggameplay.json already references the Champion file")
	ErrPatchNotExact     = errors.New("missionwrite: the derived patch is not exactly one appended champion/champion_shop_delivery.json")
	ErrBackupConflict    = errors.New("missionwrite: a different file already exists at the backup path")
	ErrBackupMissing     = errors.New("missionwrite: the verified server-side backup is missing or does not match")
	ErrNothingToRestore  = errors.New("missionwrite: cfggameplay.json already has the backed-up content")
)

// ConfigPlan is a Gate B (or rollback) plan: the exact bytes it writes and the backup it relies on.
type ConfigPlan struct {
	Plan
	Current         []byte   // live cfggameplay.json at planning time
	Payload         []byte   // the bytes the write uploads
	PayloadSHA256   string   `json:"payloadSha256"`
	PayloadBytes    int      `json:"payloadBytes"`
	SpawnersAfter   []string `json:"spawnersAfter"`
	Diff            string   `json:"diff"`
	BackupPath      string   `json:"backupPath"`  // mission-relative
	BackupState     string   `json:"backupState"` // Absent or SHA-256
	BackupDirExists bool     `json:"backupDirExists"`
	ChampionState   string   `json:"championState"` // Absent or SHA-256
}

// fileState is one mission-relative file, located by directory listings.
type fileState struct {
	parentExists bool
	sha          string // Absent or SHA-256
	data         []byte
}

// stateOf reads a mission-relative file of any depth. A missing directory on the way means absent.
func stateOf(ctx context.Context, rm Remote, svc string, in Inspection, rel string) (fileState, error) {
	if err := validateAnyRel(rel); err != nil {
		return fileState{}, err
	}
	dir := in.phys.missionDir
	segs := strings.Split(rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		entries, err := rm.ListEntries(ctx, svc, dir)
		if err != nil {
			return fileState{}, ErrInspection
		}
		found := false
		for _, e := range entries {
			if e.Name == seg {
				if !e.IsDir {
					return fileState{}, ErrUnexpectedState
				}
				found = true
			}
		}
		if !found {
			return fileState{sha: Absent}, nil
		}
		next, err := capability.SafePath(in.phys.fileRoot, dir+"/"+seg)
		if err != nil {
			return fileState{}, ErrUnsafePath
		}
		dir = next
	}
	entries, err := rm.ListEntries(ctx, svc, dir)
	if err != nil {
		return fileState{}, ErrInspection
	}
	fs := fileState{parentExists: true, sha: Absent}
	name := segs[len(segs)-1]
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		if e.IsDir {
			return fileState{}, ErrUnexpectedState
		}
		p, err := capability.SafePath(in.phys.fileRoot, dir+"/"+name)
		if err != nil {
			return fileState{}, ErrUnsafePath
		}
		b, err := rm.ReadLog(ctx, svc, p)
		if err != nil {
			return fileState{}, ErrInspection
		}
		fs.sha, fs.data = SHA256(b), b
	}
	return fs, nil
}

// validateAnyRel is ValidateRelPath without the .json restriction (the backup ends in .bak).
func validateAnyRel(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "\\\x00*?:% ") || path.Clean(p) != p {
		return ErrUnsafePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrUnsafePath
		}
	}
	return nil
}

// uploadRel performs the two-step upload of data to a mission-relative file (parent must exist).
func uploadRel(ctx context.Context, rm Remote, svc string, in Inspection, rel string, data []byte) (tokenErr, transferErr error) {
	dir, name := path.Split(rel)
	parent := in.phys.missionDir
	if d := strings.TrimSuffix(dir, "/"); d != "" {
		p, err := capability.SafePath(in.phys.fileRoot, parent+"/"+d)
		if err != nil {
			return ErrUnsafePath, nil
		}
		parent = p
	}
	target, err := rm.RequestUploadToken(ctx, svc, parent, name)
	if err != nil {
		return err, nil
	}
	return nil, rm.PostUpload(ctx, target, data)
}

func equalList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// prepareConfig is Prepare for the Gate B operations. It never writes.
func prepareConfig(ctx context.Context, rm Remote, r Request) (ConfigPlan, error) {
	kind := ops[r.Operation].kind
	in, err := Inspect(ctx, rm, r)
	if err != nil {
		return ConfigPlan{}, err
	}
	cp := ConfigPlan{Plan: Plan{Operation: r.Operation, Path: r.Path, Inspection: in}}
	if err := checkExpectations(r, in); err != nil {
		return cp, err
	}
	svc := r.Binding.NitradoServiceID
	raw, err := rm.ReadLog(ctx, svc, in.phys.configPath)
	if err != nil {
		return cp, ErrInspection
	}
	if SHA256(raw) != in.ConfigSHA256 {
		return cp, ErrConfigChanged // changed during the inspection itself
	}
	cp.Current = raw
	champ, err := stateOf(ctx, rm, svc, in, nitradodelivery.ArtifactRelPath)
	if err != nil {
		return cp, err
	}
	cp.ChampionState = champ.sha

	switch kind {
	case kindConfigPatch:
		if err := canary.CheckReferencePrecondition(champ.data, champ.sha != Absent); err != nil {
			return cp, ErrChampionNotEmpty
		}
		for _, s := range in.Spawners {
			if s == nitradodelivery.ArtifactRelPath {
				return cp, ErrAlreadyReferenced
			}
		}
		p, err := canary.ProposePatch(raw)
		if err != nil {
			return cp, fmt.Errorf("%w: %v", ErrPatchNotExact, err)
		}
		want := append(append([]string{}, r.ExpectSpawners...), nitradodelivery.ArtifactRelPath)
		if !equalList(p.Before, r.ExpectSpawners) || !equalList(p.After, want) || len(p.Changes) != 1 {
			return cp, ErrPatchNotExact
		}
		cp.Payload, cp.SpawnersAfter, cp.Diff = p.Proposed, p.After, p.Diff
		cp.BackupPath = canary.ConfigBackupPath(in.ConfigSHA256)
		b, err := stateOf(ctx, rm, svc, in, cp.BackupPath)
		if err != nil {
			return cp, err
		}
		if b.sha != Absent && b.sha != in.ConfigSHA256 {
			return cp, ErrBackupConflict
		}
		cp.BackupState, cp.BackupDirExists = b.sha, b.parentExists
	case kindConfigRestore:
		if in.ConfigSHA256 == r.ExpectPayloadSHA256 {
			return cp, ErrNothingToRestore
		}
		cp.BackupPath = canary.ConfigBackupPath(r.ExpectPayloadSHA256)
		b, err := stateOf(ctx, rm, svc, in, cp.BackupPath)
		if err != nil {
			return cp, err
		}
		gp := capability.ParseCfgGameplay(b.data)
		if b.sha != r.ExpectPayloadSHA256 || !gp.ValidJSON || !gp.HasSpawnersKey {
			return cp, ErrBackupMissing
		}
		cp.Payload, cp.SpawnersAfter, cp.BackupState, cp.BackupDirExists = b.data, gp.Spawners, b.sha, true
		cp.Diff = nitradodelivery.Diff(raw, b.data)
	}
	cp.PayloadSHA256, cp.PayloadBytes = SHA256(cp.Payload), len(cp.Payload)
	if cp.PayloadSHA256 != r.ExpectPayloadSHA256 {
		return cp, ErrPayload
	}
	rp := r
	rp.Payload = cp.Payload
	// The backup folder's existence is part of the plan: a failed run that created champion/backup must
	// yield a NEW plan ID (the old one is consumed), never the same, already-used ID.
	cp.ID = "mw-" + SHA256([]byte(fmt.Sprintf("%s\x1echampion=%s\x1ebackup=%s=%s\x1ebackup_dir=%t",
		planID(rp, in), cp.ChampionState, cp.BackupPath, cp.BackupState, cp.BackupDirExists)))[:24]
	cp.Steps = configSteps(kind, cp, in)
	return cp, nil
}

func configSteps(kind opKind, cp ConfigPlan, in Inspection) []string {
	var s []string
	if kind == kindConfigPatch {
		switch {
		case cp.BackupState != Absent:
			s = append(s, "READ  server backup "+cp.BackupPath+" already present with the original SHA-256 (reused, not rewritten)")
		default:
			if !cp.BackupDirExists {
				s = append(s, "WRITE mkdir champion/backup (one directory, inside champion/)")
			}
			s = append(s,
				fmt.Sprintf("WRITE backup: upload the exact original %d bytes to %s", len(cp.Current), cp.BackupPath),
				"READ  read the backup back: SHA-256 must be "+in.ConfigSHA256+" (otherwise stop: cfggameplay.json untouched)")
		}
		s = append(s, "READ  re-verify: cfggameplay.json "+in.ConfigSHA256[:12]+"…, Champion file still the empty file, backup still verified")
	} else {
		s = append(s, "READ  re-verify: cfggameplay.json "+in.ConfigSHA256[:12]+"…, backup "+cp.BackupPath+" still "+cp.PayloadSHA256[:12]+"…")
	}
	return append(s,
		fmt.Sprintf("WRITE upload token for cfggameplay.json, then one POST of %d bytes (SHA-256 %s)", cp.PayloadBytes, cp.PayloadSHA256),
		"READ  read cfggameplay.json back: exact size and SHA-256; objectSpawnersArr = "+fmt.Sprintf("%q", cp.SpawnersAfter),
		"READ  every referenced spawner file still present; Champion file unchanged; no new boot; gameserver status unchanged")
}

// executeConfig is Execute for the Gate B operations.
func executeConfig(ctx context.Context, rm Remote, r Request, authorizedID string, j *Journal) (Outcome, error) {
	kind := ops[r.Operation].kind
	out := Outcome{PlanID: authorizedID, Status: StatusNotWritten, Transfer: "not attempted", Directory: DirNotNeeded}
	unlock, err := j.Lock()
	if err != nil {
		return out, err
	}
	defer unlock()
	svc := r.Binding.NitradoServiceID
	if open, err := j.Outstanding(svc, r.Path); err != nil {
		return out, err
	} else if open {
		return out, ErrUncertainOutstanding
	}
	if used, err := j.Used(authorizedID); err != nil {
		return out, err
	} else if used {
		return out, ErrAuthorizationUsed
	}
	cp, err := prepareConfig(ctx, rm, r)
	if err != nil {
		return out, err
	}
	if cp.ID != authorizedID {
		return out, ErrAuthorizationStale
	}
	in := cp.Inspection
	out.Before = in.ConfigSHA256
	if err := j.Append(Entry{PlanID: cp.ID, Operation: string(r.Operation), Service: svc, Path: r.Path, Status: "STARTED", Detail: "before=" + in.ConfigSHA256 + " backup=" + cp.BackupPath}); err != nil {
		return out, fmt.Errorf("missionwrite: journal: %w", err)
	}
	finish := func(o Outcome, detail string) (Outcome, error) {
		if err := j.Append(Entry{PlanID: cp.ID, Operation: string(r.Operation), Service: svc, Path: r.Path, Status: o.Status, Detail: detail + "; directory: " + o.Directory}); err != nil {
			return o, fmt.Errorf("missionwrite: outcome %s could not be journaled (the STARTED entry keeps the destination blocked): %w", o.Status, err)
		}
		return o, nil
	}

	// 1. Verified server-side backup of the exact original bytes (Gate B only).
	if kind == kindConfigPatch {
		if cp.BackupState == Absent {
			if !cp.BackupDirExists {
				champDir, perr := capability.SafePath(in.phys.fileRoot, in.phys.missionDir+"/"+nitradodelivery.ArtifactDir)
				if perr != nil {
					return finish(out, "unsafe backup folder path; cfggameplay.json untouched")
				}
				mkErr := rm.Mkdir(ctx, svc, champDir, "backup")
				now, lerr := stateOf(ctx, rm, svc, in, cp.BackupPath)
				switch {
				case mkErr == nil && lerr == nil && now.parentExists:
					out.Directory = "champion/backup created"
				case mkErr != nil && lerr == nil && now.parentExists:
					out.Directory = "champion/backup " + DirCreatedDespiteErr
				case lerr != nil:
					out.Directory = "champion/backup " + DirUnknown
				default:
					out.Directory = "champion/backup " + DirNotCreated
				}
				if mkErr != nil || lerr != nil || !now.parentExists {
					out.Checks = append(out.Checks, Check{"backup folder", "FAIL", errText(mkErr)})
					return finish(out, "backup folder not created; cfggameplay.json untouched")
				}
			}
			tokErr, trErr := uploadRel(ctx, rm, svc, in, cp.BackupPath, cp.Current)
			b, berr := stateOf(ctx, rm, svc, in, cp.BackupPath)
			switch {
			case berr == nil && b.sha == in.ConfigSHA256:
				out.Checks = append(out.Checks, Check{"backup", "PASS", fmt.Sprintf("%s: %d bytes, SHA-256 %s", cp.BackupPath, len(b.data), b.sha)})
			default:
				detail := "backup not verified (token: " + errText(tokErr) + ", transfer: " + errText(trErr) + ")"
				if berr == nil && b.sha != Absent {
					detail += "; a backup file with unexpected content exists (SHA-256 " + b.sha + ") - the owner must inspect it"
				}
				out.Checks = append(out.Checks, Check{"backup", "FAIL", detail})
				return finish(out, "backup failed; cfggameplay.json untouched")
			}
		} else {
			out.Checks = append(out.Checks, Check{"backup", "PASS", cp.BackupPath + " already present with the original SHA-256"})
		}
	}

	// 2. Re-verify immediately before the overwrite.
	now, err := stateOf(ctx, rm, svc, in, canary.ConfigRelPath)
	if err != nil || now.sha != r.ExpectCurrent {
		out.Checks = append(out.Checks, Check{"pre-write cfggameplay.json", "FAIL", "changed or unreadable"})
		return finish(out, "aborted before the upload request")
	}
	if kind == kindConfigPatch {
		if c, err := stateOf(ctx, rm, svc, in, nitradodelivery.ArtifactRelPath); err != nil || canary.CheckReferencePrecondition(c.data, c.sha != Absent) != nil {
			out.Checks = append(out.Checks, Check{"pre-write Champion file", "FAIL", "no longer exactly the empty file"})
			return finish(out, "aborted before the upload request")
		}
	}
	// The backup must still hold the original bytes: for the patch that is the current config, for the
	// rollback it is the payload being restored.
	wantBackup := cp.PayloadSHA256
	if kind == kindConfigPatch {
		wantBackup = in.ConfigSHA256
	}
	if b, err := stateOf(ctx, rm, svc, in, cp.BackupPath); err != nil || b.sha != wantBackup {
		out.Checks = append(out.Checks, Check{"pre-write backup", "FAIL", "the backup changed or is unreadable"})
		return finish(out, "aborted before the upload request")
	}

	// 3. The overwrite: one token, one transfer.
	tokErr, trErr := uploadRel(ctx, rm, svc, in, canary.ConfigRelPath, cp.Payload)
	switch {
	case tokErr != nil:
		out.Checks = append(out.Checks, Check{"upload token", "FAIL", errText(tokErr)})
	case trErr != nil:
		out.Transfer = errText(trErr)
	default:
		out.Transfer = "2xx"
	}

	// 4. Independent read-back decides the outcome.
	after, err := stateOf(ctx, rm, svc, in, canary.ConfigRelPath)
	if err != nil {
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "UNVERIFIED", "cfggameplay.json could not be read back"})
		return finish(out, "read-back failed after a write request")
	}
	out.After, out.AfterBytes = after.sha, len(after.data)
	refused := tokErr != nil || trErr != nil
	switch {
	case after.sha == cp.PayloadSHA256 && len(after.data) == cp.PayloadBytes && tokErr == nil:
		out.Status = StatusWrittenVerified
		out.Checks = append(out.Checks, Check{"read-back", "PASS", fmt.Sprintf("%d bytes, SHA-256 %s", len(after.data), after.sha)})
	case after.sha == r.ExpectCurrent && refused:
		out.Status = StatusNotWritten
		out.Checks = append(out.Checks, Check{"read-back", "PASS", "cfggameplay.json verified unchanged after the refused request"})
		return finish(out, "request refused; cfggameplay.json verified unchanged")
	case after.sha == r.ExpectCurrent:
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "FAIL", "the file server reported success but cfggameplay.json is unchanged"})
		return finish(out, "reported success, file unchanged")
	default:
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "FAIL", fmt.Sprintf("unexpected content: %d bytes, SHA-256 %s (rollback: %s from %s, separately authorized)", len(after.data), after.sha, OpRestoreConfig, cp.BackupPath)})
		return finish(out, "read-back mismatch")
	}

	// 5. Post-write checks.
	gp := capability.ParseCfgGameplay(after.data)
	if gp.ValidJSON && equalList(gp.Spawners, cp.SpawnersAfter) {
		out.Checks = append(out.Checks, Check{"objectSpawnersArr", "PASS", fmt.Sprintf("%q", gp.Spawners)})
	} else {
		out.Checks = append(out.Checks, Check{"objectSpawnersArr", "FAIL", fmt.Sprintf("%q", gp.Spawners)})
	}
	missing := []string{}
	for _, s := range gp.Spawners {
		if f, err := stateOf(ctx, rm, svc, in, s); err != nil || f.sha == Absent {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		out.Checks = append(out.Checks, Check{"referenced spawner files present", "PASS", fmt.Sprintf("%d/%d", len(gp.Spawners), len(gp.Spawners))})
	} else {
		out.Checks = append(out.Checks, Check{"referenced spawner files present", "FAIL", "missing: " + strings.Join(missing, ", ")})
	}
	if c, err := stateOf(ctx, rm, svc, in, nitradodelivery.ArtifactRelPath); err == nil && c.sha == cp.ChampionState {
		out.Checks = append(out.Checks, Check{"Champion file unchanged", "PASS", c.sha})
	} else {
		out.Checks = append(out.Checks, Check{"Champion file unchanged", "FAIL", "changed or unreadable"})
	}
	out.Checks = append(out.Checks, restartCheck(ctx, rm, svc, in))
	return finish(out, "after="+out.After)
}

// PrepareConfig exposes the Gate B plan with its payload and backup facts (for the dry run).
func PrepareConfig(ctx context.Context, rm Remote, r Request) (ConfigPlan, error) {
	if err := r.Validate(); err != nil {
		return ConfigPlan{}, err
	}
	if ops[r.Operation].kind == kindCreate {
		return ConfigPlan{}, ErrUnknownOperation
	}
	return prepareConfig(ctx, rm, r)
}
