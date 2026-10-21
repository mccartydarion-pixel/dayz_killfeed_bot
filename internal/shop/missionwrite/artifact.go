package missionwrite

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// The delivery worker's view of the server (docs/SHOP_DELIVERY_WORKER_DESIGN.md). The manual gates in
// this package take an owner-approved plan and a local journal; the worker has neither a person nor a
// local disk, so it gets two narrower primitives built from the same reads and the same single-token,
// single-transfer, read-back-decides write:
//
//   - InspectArtifact reads everything a worker pass decides on, in one consistent snapshot;
//   - WriteArtifact replaces the Champion spawner file once, refusing if anything in that snapshot
//     changed, and reports the outcome from an independent read-back.
//
// Neither creates a folder or a file, touches cfggameplay.json, or restarts anything. The caller owns
// the journal (the database) and the decision of what to write.

var (
	// ErrArtifactMissing: the Champion file (or its custom/ folder) does not exist. The worker never
	// creates it: that is the owner-approved Gate C.
	ErrArtifactMissing = errors.New("missionwrite: the Champion spawner file does not exist on the server")
	// ErrArtifactNotReferenced: cfggameplay.json does not list the Champion file, so nothing in it
	// would ever spawn. Referencing it is the owner-approved Gate D.
	ErrArtifactNotReferenced = errors.New("missionwrite: cfggameplay.json does not reference the Champion spawner file")
	// ErrSnapshotStale: the server changed between the inspection and the write.
	ErrSnapshotStale = errors.New("missionwrite: the server changed since it was inspected")
)

// ArtifactState is one consistent read of the server for a worker pass.
type ArtifactState struct {
	phys *physical

	MissionPath  string
	ConfigSHA256 string
	Spawners     []string
	// Content is the Champion spawner file exactly as stored; SHA256 is its digest.
	Content []byte
	SHA256  string
	// Boots is every DayZServer_*.ADM file name across the server's log mounts, oldest first. The
	// names carry the boot's server-local start time, so the last one is the current boot.
	Boots            []string
	GameserverStatus string
}

// CurrentBoot is the newest boot's ADM file ("" when no boot is listed).
func (s ArtifactState) CurrentBoot() string {
	if len(s.Boots) == 0 {
		return ""
	}
	return s.Boots[len(s.Boots)-1]
}

// artifactPaths resolves the Champion file's parent folder and its own path.
func artifactPaths(p *physical) (parent, file string, err error) {
	dir, name := path.Split(nitradodelivery.ArtifactRelPath)
	dir = strings.TrimSuffix(dir, "/")
	if parent, err = capability.SafePath(p.fileRoot, p.missionDir+"/"+dir); err != nil {
		return "", "", ErrUnsafePath
	}
	if file, err = capability.SafePath(p.fileRoot, parent+"/"+name); err != nil {
		return "", "", ErrUnsafePath
	}
	return parent, file, nil
}

// readArtifact downloads the Champion file. Existence comes from directory listings, never from a
// download error, which cannot tell "absent" from "failed".
func readArtifact(ctx context.Context, rm Remote, svc string, p *physical) ([]byte, error) {
	dir, name := path.Split(nitradodelivery.ArtifactRelPath)
	dir = strings.TrimSuffix(dir, "/")
	entries, err := rm.ListEntries(ctx, svc, p.missionDir)
	if err != nil {
		return nil, ErrInspection
	}
	found := false
	for _, e := range entries {
		if e.Name == dir {
			if !e.IsDir {
				return nil, ErrUnexpectedState
			}
			found = true
		}
	}
	if !found {
		return nil, ErrArtifactMissing
	}
	parent, file, err := artifactPaths(p)
	if err != nil {
		return nil, err
	}
	children, err := rm.ListEntries(ctx, svc, parent)
	if err != nil {
		return nil, ErrInspection
	}
	for _, e := range children {
		if e.Name != name {
			continue
		}
		if e.IsDir {
			return nil, ErrUnexpectedState
		}
		b, err := rm.ReadLog(ctx, svc, file)
		if err != nil {
			return nil, ErrInspection
		}
		return b, nil
	}
	return nil, ErrArtifactMissing
}

// allBoots lists every ADM file name over the given directories, sorted and without duplicates.
// ok is false when no directory could be listed (the boot identity is then unknown, not "none").
func allBoots(ctx context.Context, rm Remote, svc string, dirs []string) (names []string, ok bool) {
	seen := map[string]bool{}
	for _, d := range dirs {
		files, err := rm.ListDir(ctx, svc, d)
		if err != nil {
			continue
		}
		ok = true
		for _, f := range files {
			if admFileRe.MatchString(f.Name) && !seen[f.Name] {
				seen[f.Name] = true
				names = append(names, f.Name)
			}
		}
	}
	sort.Strings(names)
	return names, ok
}

// bootDirs is every mount's <game>/config directory, the server's own directory first (see Inspect).
func bootDirs(p *physical, missionPath string, gs nitrado.GameserverFacts) []string {
	var dirs []string
	if gs.GamePath != "" {
		if d, err := capability.SafePath(p.fileRoot, gs.GamePath+"/config"); err == nil {
			dirs = append(dirs, d)
		}
	}
	prefix := strings.TrimSuffix(p.missionDir, missionPath)
	if prefix != p.missionDir && gs.Game != "" {
		if d, err := capability.SafePath(p.fileRoot, prefix+gs.Game+"/config"); err == nil && (len(dirs) == 0 || dirs[0] != d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// InspectArtifact reads the configuration, the Champion file, the boot list and the server status.
// It fails closed: an unreadable configuration, a missing or unreferenced Champion file, or an
// unknown boot identity is an error, never an empty state.
func InspectArtifact(ctx context.Context, rm Remote, b capability.Binding) (ArtifactState, error) {
	var st ArtifactState
	rep, err := capability.Discover(ctx, rm, b)
	if errors.Is(err, capability.ErrBinding) {
		return st, ErrBinding
	}
	if err != nil || rep.MissionDir == "" || rep.FileRoot == "" || rep.MissionPath == "" {
		return st, ErrInspection
	}
	st.phys = &physical{missionDir: rep.MissionDir, fileRoot: rep.FileRoot}
	st.MissionPath = rep.MissionPath
	if st.phys.configPath, err = capability.SafePath(st.phys.fileRoot, st.phys.missionDir+"/"+canary.ConfigRelPath); err != nil {
		return st, ErrUnsafePath
	}
	svc := b.NitradoServiceID
	cfg, err := rm.ReadLog(ctx, svc, st.phys.configPath)
	if err != nil {
		return st, ErrInspection
	}
	gp := capability.ParseCfgGameplay(cfg)
	if !gp.ValidJSON || !gp.HasSpawnersKey {
		return st, ErrInspection
	}
	st.ConfigSHA256, st.Spawners = SHA256(cfg), append([]string{}, gp.Spawners...)
	referenced := false
	for _, s := range st.Spawners {
		if s == nitradodelivery.ArtifactRelPath {
			referenced = true
		}
	}
	if !referenced {
		return st, ErrArtifactNotReferenced
	}
	if st.Content, err = readArtifact(ctx, rm, svc, st.phys); err != nil {
		return st, err
	}
	st.SHA256 = SHA256(st.Content)

	gs, err := rm.GameserverFacts(ctx, svc)
	if err != nil {
		return st, ErrInspection
	}
	st.GameserverStatus = gs.Status
	st.phys.bootDirs = bootDirs(st.phys, st.MissionPath, gs)
	boots, ok := allBoots(ctx, rm, svc, st.phys.bootDirs)
	if !ok || len(boots) == 0 {
		return st, ErrInspection
	}
	st.Boots = boots
	return st, nil
}

// ArtifactWrite is the verified result of WriteArtifact.
type ArtifactWrite struct {
	Status string // StatusWrittenVerified, StatusNotWritten or StatusUncertain
	Before string // digest the write started from
	After  string // digest read back ("" when the read-back failed)
	Detail string // sanitized; never a URL, token or account path
	// ConfigChanged and RestartDuring report the two post-write checks: the configuration is no
	// longer what the snapshot saw, and a new boot (or a status change) appeared during the write.
	ConfigChanged bool
	RestartDuring bool
	// Unverified is true when a post-write check could not be read at all.
	Unverified bool
}

// WriteArtifact replaces the Champion spawner file with payload, once. It refuses (StatusNotWritten,
// ErrSnapshotStale) if the file, the configuration, the boot or the server status differ from st -
// the snapshot the caller decided on. After any write request the outcome is decided by an
// independent read-back; a result that cannot be proven either way is StatusUncertain, and the
// caller must stop writing to this server.
func WriteArtifact(ctx context.Context, rm Remote, b capability.Binding, st ArtifactState, payload []byte) (ArtifactWrite, error) {
	out := ArtifactWrite{Status: StatusNotWritten, Before: st.SHA256, Detail: "no write request was sent"}
	if st.phys == nil || st.SHA256 == "" || len(st.Boots) == 0 {
		return out, ErrInspection
	}
	if _, err := nitradodelivery.ParseSpawnerFile(payload); err != nil {
		return out, err
	}
	svc := b.NitradoServiceID
	parent, _, err := artifactPaths(st.phys)
	if err != nil {
		return out, err
	}
	_, name := path.Split(nitradodelivery.ArtifactRelPath)

	// Re-verify immediately before the write (narrows the race: Nitrado has no conditional write).
	cur, err := readArtifact(ctx, rm, svc, st.phys)
	if err != nil || SHA256(cur) != st.SHA256 {
		out.Detail = "the Champion file changed or could not be read before the write"
		return out, ErrSnapshotStale
	}
	if cfg, err := rm.ReadLog(ctx, svc, st.phys.configPath); err != nil || SHA256(cfg) != st.ConfigSHA256 {
		out.Detail = "cfggameplay.json changed or could not be read before the write"
		return out, ErrSnapshotStale
	}
	if boots, ok := allBoots(ctx, rm, svc, st.phys.bootDirs); !ok || len(boots) == 0 || boots[len(boots)-1] != st.CurrentBoot() {
		out.Detail = "a new boot appeared, or the boot list could not be read, before the write"
		return out, ErrSnapshotStale
	}
	if gs, err := rm.GameserverFacts(ctx, svc); err != nil || gs.Status != st.GameserverStatus {
		out.Detail = "the server status changed or could not be read before the write"
		return out, ErrSnapshotStale
	}

	target, tokenErr := rm.RequestUploadToken(ctx, svc, parent, name)
	var transferErr error
	if tokenErr == nil {
		transferErr = rm.PostUpload(ctx, target, payload)
	}
	refused := tokenErr != nil || transferErr != nil

	// The read-back decides - also after a failed token request, whose side effects are undocumented.
	after, err := readArtifact(ctx, rm, svc, st.phys)
	if err != nil {
		out.Status, out.After, out.Detail = StatusUncertain, "", "the Champion file could not be read back after a write request"
		return out, nil
	}
	out.After = SHA256(after)
	switch {
	case out.After == SHA256(payload) && len(after) == len(payload) && tokenErr == nil:
		out.Status, out.Detail = StatusWrittenVerified, "read-back matches the payload"
	case out.After == st.SHA256 && refused:
		out.Status, out.Detail = StatusNotWritten, "request refused ("+errText(firstErr(tokenErr, transferErr))+"); file verified unchanged"
		return out, nil
	case out.After == st.SHA256:
		out.Status, out.Detail = StatusUncertain, "the file server reported success but the file is unchanged"
		return out, nil
	default:
		out.Status, out.Detail = StatusUncertain, "the read-back is neither the payload nor the previous file"
		return out, nil
	}

	// Post-write checks: the configuration and the server are untouched.
	if cfg, err := rm.ReadLog(ctx, svc, st.phys.configPath); err != nil {
		out.Unverified = true
	} else if SHA256(cfg) != st.ConfigSHA256 {
		out.ConfigChanged = true
	}
	boots, ok := allBoots(ctx, rm, svc, st.phys.bootDirs)
	gs, gerr := rm.GameserverFacts(ctx, svc)
	switch {
	case !ok || len(boots) == 0 || gerr != nil:
		out.Unverified = true
	case boots[len(boots)-1] != st.CurrentBoot() || gs.Status != st.GameserverStatus:
		out.RestartDuring = true
	}
	return out, nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
