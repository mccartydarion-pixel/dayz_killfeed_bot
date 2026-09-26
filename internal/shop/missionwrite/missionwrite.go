// Package missionwrite is the guarded, single-operation Nitrado file write for the Champion Shop
// canary (docs/SHOP_GATE_A_UPLOAD.md). One call writes at most one file, and only when:
//
//   - the operation is explicitly selected and active (today only Gate A, the empty Champion file);
//   - the service, organization, installation, game server and mission match exactly;
//   - the destination is the operation's one fixed mission-relative path;
//   - a fresh read-only inspection matches the expected current file state, cfggameplay.json SHA-256
//     and objectSpawnersArr;
//   - the payload is exactly the operation's payload with the expected SHA-256;
//   - the caller presents the plan ID computed from that inspection (the owner's single-use
//     authorization), and the local journal shows it unused and no earlier outcome uncertain.
//
// Every write is followed by an independent read-back; the result is WRITTEN_VERIFIED, NOT_WRITTEN
// or UNCERTAIN. Nothing is retried. Nothing here runs at bot startup or in Shop delivery: only
// cmd/shop-mission-write imports this package (enforced by test).
package missionwrite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// Remote is every Nitrado call this package makes: the capability reads plus the three write
// primitives (upload token, transfer, mkdir).
type Remote interface {
	capability.Reader
	ListEntries(ctx context.Context, serviceID, dir string) ([]nitrado.DirEntry, error)
	RequestUploadToken(ctx context.Context, serviceID, dir, name string) (nitrado.UploadTarget, error)
	PostUpload(ctx context.Context, t nitrado.UploadTarget, data []byte) error
	Mkdir(ctx context.Context, serviceID, parent, name string) error
}

// Operation is an explicitly selected write.
type Operation string

const (
	// OpCreateEmptyChampionFile is Gate A: create champion/champion_shop_delivery.json as the empty
	// spawner file. It never overwrites.
	OpCreateEmptyChampionFile Operation = "gate-a-create-empty"
	// OpStageItem (Gate E) and OpUnstageItem (Gate G) are reserved. They need their own
	// authorization and ledger-state checks and are refused until implemented and approved.
	OpStageItem   Operation = "gate-e-stage"
	OpUnstageItem Operation = "gate-g-unstage"
)

// Absent is the expected/observed state of a file that does not exist.
const Absent = "ABSENT"

// Outcome statuses.
const (
	StatusWrittenVerified = "WRITTEN_VERIFIED"
	StatusNotWritten      = "NOT_WRITTEN" // proven by read-back (or no write request was sent)
	StatusUncertain       = "UNCERTAIN"   // a write may have happened and was not verified: stop
)

var (
	ErrOperationNotActive   = errors.New("missionwrite: operation is not active (Gate E and Gate G need their own approval)")
	ErrUnknownOperation     = errors.New("missionwrite: unknown operation")
	ErrWrongDestination     = errors.New("missionwrite: destination is not the operation's fixed path")
	ErrUnsafePath           = errors.New("missionwrite: unsafe or ambiguous path")
	ErrPayload              = errors.New("missionwrite: payload does not match the operation's payload and expected SHA-256")
	ErrExpectation          = errors.New("missionwrite: an expectation is missing or malformed")
	ErrBinding              = errors.New("missionwrite: service, installation or mission does not match the authorized binding")
	ErrInspection           = errors.New("missionwrite: the fresh read-only inspection could not verify the server state")
	ErrUnexpectedState      = errors.New("missionwrite: the destination is not in the expected state (no overwrite)")
	ErrConfigChanged        = errors.New("missionwrite: cfggameplay.json does not have the expected SHA-256")
	ErrSpawnersChanged      = errors.New("missionwrite: objectSpawnersArr is not the expected list")
	ErrAuthorizationMissing = errors.New("missionwrite: an explicit single-use authorization (the plan ID) is required")
	ErrAuthorizationStale   = errors.New("missionwrite: the authorization does not match the fresh plan (state changed since approval)")
	ErrAuthorizationUsed    = errors.New("missionwrite: this authorization was already used")
	ErrUncertainOutstanding = errors.New("missionwrite: an earlier write to this destination is UNCERTAIN or incomplete; resolve it first")
)

// Request is one explicitly authorized write.
type Request struct {
	Operation           Operation
	Binding             capability.Binding
	Mission             string   // canonical mission path, e.g. dayzps_missions/dayzOffline.chernarusplus
	Path                string   // mission-relative destination
	ExpectCurrent       string   // Absent, or the current file's SHA-256
	ExpectConfigSHA256  string   // cfggameplay.json
	ExpectSpawners      []string // exact objectSpawnersArr
	Payload             []byte
	ExpectPayloadSHA256 string
}

type opSpec struct {
	path          string
	payload       func() []byte
	mustBeAbsent  bool
	createsParent bool
}

var ops = map[Operation]*opSpec{
	OpCreateEmptyChampionFile: {
		path:          nitradodelivery.ArtifactRelPath,
		payload:       func() []byte { b, _ := canary.EmptyArtifact(); return b },
		mustBeAbsent:  true,
		createsParent: true,
	},
	OpStageItem:   nil,
	OpUnstageItem: nil,
}

var (
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	relPathRe  = regexp.MustCompile(`^([A-Za-z0-9_-]+/)*[A-Za-z0-9_-]+\.json$`) // spawner JSON files only
	admFileRe  = regexp.MustCompile(`^DayZServer_[A-Za-z0-9_]+_\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}\.ADM$`)
	missionRe  = regexp.MustCompile(`^[a-z0-9]+_missions/[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)?$`)
	parentName = nitradodelivery.ArtifactDir
)

// SHA256 is the lowercase hex digest.
func SHA256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// ValidateRelPath refuses anything but a plain mission-relative file path: no absolute path, no "."
// or ".." segment, no empty segment, no backslash, NUL, space, glob or scheme, and only a .json file.
func ValidateRelPath(p string) error {
	if p == "" || len(p) > 200 || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, "\\\x00*?:% ") {
		return ErrUnsafePath
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrUnsafePath
		}
	}
	if path.Clean(p) != p || !relPathRe.MatchString(p) {
		return ErrUnsafePath
	}
	return nil
}

// Validate checks the request without any network call.
func (r Request) Validate() error {
	spec, known := ops[r.Operation]
	if !known {
		return ErrUnknownOperation
	}
	if spec == nil {
		return ErrOperationNotActive
	}
	if err := ValidateRelPath(r.Path); err != nil {
		return err
	}
	if r.Path != spec.path {
		return ErrWrongDestination
	}
	b := r.Binding
	if b.OrganizationID <= 0 || b.InstallationID <= 0 || b.GameServerID <= 0 || b.NitradoServiceID == "" || !missionRe.MatchString(r.Mission) {
		return ErrBinding
	}
	if !sha256Re.MatchString(r.ExpectConfigSHA256) || !sha256Re.MatchString(r.ExpectPayloadSHA256) || r.ExpectSpawners == nil {
		return ErrExpectation
	}
	if r.ExpectCurrent != Absent && !sha256Re.MatchString(r.ExpectCurrent) {
		return ErrExpectation
	}
	if spec.mustBeAbsent && r.ExpectCurrent != Absent {
		return ErrUnexpectedState
	}
	want := spec.payload()
	if !bytes.Equal(r.Payload, want) || SHA256(r.Payload) != r.ExpectPayloadSHA256 ||
		bytes.HasPrefix(r.Payload, []byte{0xEF, 0xBB, 0xBF}) || bytes.ContainsRune(r.Payload, '\r') {
		return ErrPayload
	}
	return nil
}

// Inspection is the fresh read-only server state a plan is computed from. MissionDir is the physical
// path and is never printed (it names the Nitrado account); MissionPath is the canonical path.
type Inspection struct {
	phys *physical // a pointer, so fmt prints an address, never the account path

	MissionPath      string
	ConfigSHA256     string
	ConfigBytes      int
	Spawners         []string
	ParentExists     bool
	Current          string // Absent or SHA-256
	CurrentBytes     int
	GameserverStatus string
	BootFile         string // newest DayZServer_*.ADM, "" if unverifiable
}

type physical struct{ missionDir, fileRoot, configPath, bootDir string }

func (*physical) String() string { return "<physical paths redacted>" }

// Inspect performs the fresh read-only inspection for r (Validate must pass first).
func Inspect(ctx context.Context, rm Remote, r Request) (Inspection, error) {
	var in Inspection
	rep, err := capability.Discover(ctx, rm, r.Binding)
	if errors.Is(err, capability.ErrBinding) {
		return in, ErrBinding
	}
	if err != nil || rep.MissionDir == "" || rep.FileRoot == "" {
		return in, ErrInspection
	}
	if rep.MissionPath != r.Mission {
		return in, ErrBinding
	}
	in.phys = &physical{missionDir: rep.MissionDir, fileRoot: rep.FileRoot}
	in.MissionPath = rep.MissionPath

	if in.phys.configPath, err = capability.SafePath(in.phys.fileRoot, in.phys.missionDir+"/"+canary.ConfigRelPath); err != nil {
		return in, ErrUnsafePath
	}
	cfg, err := rm.ReadLog(ctx, r.Binding.NitradoServiceID, in.phys.configPath)
	if err != nil {
		return in, ErrInspection
	}
	in.ConfigSHA256, in.ConfigBytes = SHA256(cfg), len(cfg)
	gp := capability.ParseCfgGameplay(cfg)
	if !gp.ValidJSON || !gp.HasSpawnersKey {
		return in, ErrInspection
	}
	in.Spawners = append([]string{}, gp.Spawners...)

	if err := readTarget(ctx, rm, r, &in); err != nil {
		return in, err
	}

	gs, err := rm.GameserverFacts(ctx, r.Binding.NitradoServiceID)
	if err == nil {
		in.GameserverStatus = gs.Status
		prefix := strings.TrimSuffix(in.phys.missionDir, in.MissionPath)
		if prefix != in.phys.missionDir && gs.Game != "" {
			if d, perr := capability.SafePath(in.phys.fileRoot, prefix+gs.Game+"/config"); perr == nil {
				in.phys.bootDir = d
				in.BootFile = newestADM(ctx, rm, r.Binding.NitradoServiceID, d)
			}
		}
	}
	return in, nil
}

// readTarget fills ParentExists and Current for the destination, from directory listings (never
// from a download error, which cannot tell "absent" from "failed").
func readTarget(ctx context.Context, rm Remote, r Request, in *Inspection) error {
	svc := r.Binding.NitradoServiceID
	dir, name := path.Split(r.Path)
	dir = strings.TrimSuffix(dir, "/")
	entries, err := rm.ListEntries(ctx, svc, in.phys.missionDir)
	if err != nil {
		return ErrInspection
	}
	in.ParentExists, in.Current, in.CurrentBytes = false, Absent, 0
	for _, e := range entries {
		if e.Name == dir {
			if !e.IsDir {
				return ErrUnexpectedState // a file where the directory should be
			}
			in.ParentExists = true
		}
	}
	if !in.ParentExists {
		return nil
	}
	parent, err := capability.SafePath(in.phys.fileRoot, in.phys.missionDir+"/"+dir)
	if err != nil {
		return ErrUnsafePath
	}
	children, err := rm.ListEntries(ctx, svc, parent)
	if err != nil {
		return ErrInspection
	}
	for _, e := range children {
		if e.Name != name {
			continue
		}
		if e.IsDir {
			return ErrUnexpectedState
		}
		p, err := capability.SafePath(in.phys.fileRoot, parent+"/"+name)
		if err != nil {
			return ErrUnsafePath
		}
		b, err := rm.ReadLog(ctx, svc, p)
		if err != nil {
			return ErrInspection
		}
		in.Current, in.CurrentBytes = SHA256(b), len(b)
	}
	return nil
}

func newestADM(ctx context.Context, rm Remote, svc, dir string) string {
	files, err := rm.ListDir(ctx, svc, dir)
	if err != nil {
		return ""
	}
	names := []string{}
	for _, f := range files {
		if admFileRe.MatchString(f.Name) {
			names = append(names, f.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[len(names)-1]
}

// Plan is the exact operation an owner authorizes by its ID.
type Plan struct {
	ID         string
	Operation  Operation
	Path       string
	Inspection Inspection
	Steps      []string
}

// planID binds the authorization to every fact the write depends on. Anything that changes after
// the owner's approval (the file, the configuration, the spawner list, the parent directory, the
// binding or the payload) produces a different ID and the write is refused.
func planID(r Request, in Inspection) string {
	lines := []string{
		"op=" + string(r.Operation),
		"service=" + r.Binding.NitradoServiceID,
		fmt.Sprintf("org=%d installation=%d game_server=%d", r.Binding.OrganizationID, r.Binding.InstallationID, r.Binding.GameServerID),
		"mission=" + in.MissionPath,
		"path=" + r.Path,
		"current=" + in.Current,
		fmt.Sprintf("parent_exists=%t", in.ParentExists),
		"config_sha256=" + in.ConfigSHA256,
		"spawners=" + strings.Join(in.Spawners, "\n"),
		fmt.Sprintf("payload_sha256=%s payload_bytes=%d", SHA256(r.Payload), len(r.Payload)),
	}
	return "mw-" + SHA256([]byte(strings.Join(lines, "\x1e")))[:24]
}

// Prepare validates r, inspects the server and checks every expectation. It never writes.
func Prepare(ctx context.Context, rm Remote, r Request) (Plan, error) {
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	in, err := Inspect(ctx, rm, r)
	if err != nil {
		return Plan{}, err
	}
	if err := checkExpectations(r, in); err != nil {
		return Plan{Inspection: in}, err
	}
	p := Plan{ID: planID(r, in), Operation: r.Operation, Path: r.Path, Inspection: in}
	if !in.ParentExists {
		p.Steps = append(p.Steps, "WRITE mkdir "+parentName+" in the mission folder (one directory, not recursive)")
	}
	p.Steps = append(p.Steps,
		"READ  re-verify: destination "+Absent+", cfggameplay.json "+in.ConfigSHA256[:12]+"…",
		fmt.Sprintf("WRITE upload token for %s, then one POST of %d bytes (SHA-256 %s)", r.Path, len(r.Payload), r.ExpectPayloadSHA256),
		"READ  read the file back: exact size and SHA-256",
		"READ  cfggameplay.json unchanged; no new boot; gameserver status unchanged",
	)
	return p, nil
}

func checkExpectations(r Request, in Inspection) error {
	if in.ConfigSHA256 != r.ExpectConfigSHA256 {
		return ErrConfigChanged
	}
	if strings.Join(in.Spawners, "\n") != strings.Join(r.ExpectSpawners, "\n") || len(in.Spawners) != len(r.ExpectSpawners) {
		return ErrSpawnersChanged
	}
	if in.Current != r.ExpectCurrent {
		return ErrUnexpectedState
	}
	return nil
}

// Check is one post-write verification.
type Check struct {
	Name   string `json:"name"`
	Result string `json:"result"` // PASS / FAIL / UNVERIFIED
	Detail string `json:"detail"`
}

// Outcome is the verified result of Execute. It carries digests and sizes only.
type Outcome struct {
	PlanID        string  `json:"planId"`
	Status        string  `json:"status"`
	Before        string  `json:"before"`
	After         string  `json:"after"`
	AfterBytes    int     `json:"afterBytes"`
	DirectoryMade bool    `json:"directoryCreated"`
	Transfer      string  `json:"transfer"` // "not attempted", "2xx" or a sanitized error
	Checks        []Check `json:"checks"`
}

// Execute performs the authorized write. authorizedID must be the plan ID the owner approved; it is
// recomputed from a fresh inspection, so any change since approval refuses the write.
func Execute(ctx context.Context, rm Remote, r Request, authorizedID string, j *Journal) (Outcome, error) {
	out := Outcome{PlanID: authorizedID, Status: StatusNotWritten, Transfer: "not attempted"}
	if strings.TrimSpace(authorizedID) == "" {
		return out, ErrAuthorizationMissing
	}
	if j == nil {
		return out, errors.New("missionwrite: a journal is required")
	}
	if err := r.Validate(); err != nil {
		return out, err
	}
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
	plan, err := Prepare(ctx, rm, r)
	if err != nil {
		return out, err
	}
	if plan.ID != authorizedID {
		return out, ErrAuthorizationStale
	}
	in := plan.Inspection
	out.Before = in.Current
	// The authorization is consumed BEFORE any write: a crash from here on leaves a STARTED entry,
	// which blocks further writes to this destination until resolved.
	if err := j.Append(Entry{PlanID: plan.ID, Operation: string(r.Operation), Service: svc, Path: r.Path, Status: "STARTED", Detail: "before=" + in.Current}); err != nil {
		return out, fmt.Errorf("missionwrite: journal: %w", err)
	}
	finish := func(o Outcome, detail string) (Outcome, error) {
		if err := j.Append(Entry{PlanID: plan.ID, Operation: string(r.Operation), Service: svc, Path: r.Path, Status: o.Status, Detail: detail}); err != nil {
			return o, fmt.Errorf("missionwrite: outcome %s could not be journaled: %w", o.Status, err)
		}
		return o, nil
	}

	dir, name := path.Split(r.Path)
	dir = strings.TrimSuffix(dir, "/")
	if !in.ParentExists {
		mkErr := rm.Mkdir(ctx, svc, in.phys.missionDir, dir)
		var now Inspection = in
		if err := readTarget(ctx, rm, r, &now); err != nil || !now.ParentExists {
			// The directory is not there: no file can have been written.
			out.Checks = append(out.Checks, Check{"mkdir", "FAIL", errText(mkErr)})
			return finish(out, "mkdir failed; no file write attempted")
		}
		out.DirectoryMade = true
		out.Checks = append(out.Checks, Check{"mkdir", "PASS", "directory present after mkdir (" + errText(mkErr) + ")"})
	}

	// Re-verify immediately before the write (narrows the race: Nitrado has no conditional write).
	var pre Inspection = in
	if err := readTarget(ctx, rm, r, &pre); err != nil || pre.Current != r.ExpectCurrent {
		out.Checks = append(out.Checks, Check{"pre-write destination", "FAIL", "destination changed or unreadable"})
		return finish(out, "aborted before the upload request")
	}
	if cfg, err := rm.ReadLog(ctx, svc, in.phys.configPath); err != nil || SHA256(cfg) != r.ExpectConfigSHA256 {
		out.Checks = append(out.Checks, Check{"pre-write cfggameplay.json", "FAIL", "changed or unreadable"})
		return finish(out, "aborted before the upload request")
	}
	parentPath, err := capability.SafePath(in.phys.fileRoot, in.phys.missionDir+"/"+dir)
	if err != nil {
		return finish(out, "unsafe parent path")
	}

	target, err := rm.RequestUploadToken(ctx, svc, parentPath, name)
	if err != nil {
		out.Checks = append(out.Checks, Check{"upload token", "FAIL", errText(err)})
		return finish(out, "no upload token; nothing sent")
	}
	transferErr := rm.PostUpload(ctx, target, r.Payload)
	out.Transfer = "2xx"
	if transferErr != nil {
		out.Transfer = errText(transferErr)
	}

	// Independent read-back decides the outcome.
	var after Inspection = in
	after.ParentExists = true
	if err := readTarget(ctx, rm, r, &after); err != nil {
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "UNVERIFIED", "the destination could not be read back"})
		return finish(out, "read-back failed after a transfer attempt")
	}
	out.After, out.AfterBytes = after.Current, after.CurrentBytes
	switch {
	case after.Current == r.ExpectPayloadSHA256 && after.CurrentBytes == len(r.Payload):
		out.Status = StatusWrittenVerified
		out.Checks = append(out.Checks, Check{"read-back", "PASS", fmt.Sprintf("%d bytes, SHA-256 %s", after.CurrentBytes, after.Current)})
	case after.Current == Absent && transferErr != nil:
		out.Status = StatusNotWritten
		out.Checks = append(out.Checks, Check{"read-back", "PASS", "destination still absent after a refused transfer"})
		return finish(out, "transfer refused; destination verified absent")
	case after.Current == Absent:
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "FAIL", "the file server reported success but the file is absent"})
		return finish(out, "reported success, file absent")
	default:
		out.Status = StatusUncertain
		out.Checks = append(out.Checks, Check{"read-back", "FAIL", fmt.Sprintf("unexpected content: %d bytes, SHA-256 %s", after.CurrentBytes, after.Current)})
		return finish(out, "read-back mismatch")
	}

	// Post-write checks: the configuration and the server are untouched.
	if cfg, err := rm.ReadLog(ctx, svc, in.phys.configPath); err != nil {
		out.Checks = append(out.Checks, Check{"cfggameplay.json unchanged", "UNVERIFIED", "could not be read"})
	} else if SHA256(cfg) != r.ExpectConfigSHA256 {
		out.Checks = append(out.Checks, Check{"cfggameplay.json unchanged", "FAIL", "SHA-256 is now " + SHA256(cfg)})
	} else {
		out.Checks = append(out.Checks, Check{"cfggameplay.json unchanged", "PASS", r.ExpectConfigSHA256})
	}
	out.Checks = append(out.Checks, restartCheck(ctx, rm, svc, in))
	return finish(out, "after="+out.After)
}

func restartCheck(ctx context.Context, rm Remote, svc string, in Inspection) Check {
	if in.BootFile == "" || in.phys.bootDir == "" {
		return Check{"no restart", "UNVERIFIED", "the boot identity was not readable before the write"}
	}
	now := newestADM(ctx, rm, svc, in.phys.bootDir)
	gs, err := rm.GameserverFacts(ctx, svc)
	switch {
	case now == "" || err != nil:
		return Check{"no restart", "UNVERIFIED", "the boot identity could not be re-read"}
	case now != in.BootFile:
		return Check{"no restart", "FAIL", "a new boot appeared: " + now}
	case gs.Status != in.GameserverStatus:
		return Check{"no restart", "FAIL", "gameserver status changed: " + in.GameserverStatus + " -> " + gs.Status}
	}
	return Check{"no restart", "PASS", "boot " + now + ", status " + gs.Status}
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
