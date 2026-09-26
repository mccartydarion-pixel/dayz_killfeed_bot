package missionwrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
)

const (
	emptySHA  = "328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f"
	champFile = missionDir + "/champion/champion_shop_delivery.json"
)

func init() { nitrado.AllowInsecureUploadURL = true } // the stand-in serves plain http

func gateA(t *testing.T) Request {
	t.Helper()
	payload, sha := canary.EmptyArtifact()
	if sha != emptySHA || len(payload) != 20 {
		t.Fatalf("empty artifact changed: %d %s", len(payload), sha)
	}
	return Request{
		Operation:           OpCreateEmptyChampionFile,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "champion/champion_shop_delivery.json",
		ExpectCurrent:       Absent,
		ExpectConfigSHA256:  SHA256([]byte(liveConfig)),
		ExpectSpawners:      []string{"custom/The_Lost_City.json"},
		Payload:             payload,
		ExpectPayloadSHA256: emptySHA,
	}
}

func journal(t *testing.T) *Journal {
	t.Helper()
	j, err := OpenJournal(filepath.Join(t.TempDir(), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func plan(t *testing.T, s *standIn, r Request) Plan {
	t.Helper()
	p, err := Prepare(context.Background(), s.client(), r)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return p
}

func check(o Outcome, name string) string {
	for _, c := range o.Checks {
		if c.Name == name {
			return c.Result
		}
	}
	return "missing"
}

func TestGateASuccessfulTwoStepUpload(t *testing.T) {
	s := newStandIn(t)
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	if p.Inspection.ParentExists || p.Inspection.Current != Absent || p.Inspection.MissionPath != testMission ||
		p.Inspection.BootFile != "DayZServer_PS4_x64_2026-09-26_04-17-25.ADM" || !strings.HasPrefix(p.ID, "mw-") {
		t.Fatalf("plan: %+v", p)
	}
	if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
		t.Fatal("Prepare must not write")
	}
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusWrittenVerified || o.After != emptySHA || o.AfterBytes != 20 || !o.DirectoryMade {
		t.Fatalf("%v %+v", err, o)
	}
	if got, _ := s.file(champFile); string(got) != "{\n  \"Objects\": []\n}\n" {
		t.Fatalf("stored %q", got)
	}
	// Exact protocol: mkdir(parent=mission, name=champion); token(path=<champion dir>, file=<name>);
	// one transfer with the token header, application/binary and the raw 20 bytes.
	if len(s.mkdirCalls) != 1 || s.mkdirCalls[0].Get("path") != missionDir || s.mkdirCalls[0].Get("name") != "champion" {
		t.Fatalf("mkdir: %v", s.mkdirCalls)
	}
	if len(s.uploadCalls) != 1 || s.uploadCalls[0].Get("path") != missionDir+"/champion" || s.uploadCalls[0].Get("file") != "champion_shop_delivery.json" {
		t.Fatalf("token: %v", s.uploadCalls)
	}
	if len(s.transfers) != 1 || s.transfers[0].contentType != "application/binary" || string(s.transfers[0].body) != "{\n  \"Objects\": []\n}\n" || s.transfers[0].token == "" {
		t.Fatalf("transfer: %+v", s.transfers)
	}
	if check(o, "cfggameplay.json unchanged") != "PASS" || check(o, "no restart") != "PASS" || check(o, "read-back") != "PASS" {
		t.Fatalf("checks: %+v", o.Checks)
	}
	if cfg, _ := s.file(missionDir + "/cfggameplay.json"); string(cfg) != liveConfig {
		t.Fatal("cfggameplay.json was touched")
	}
	es, _ := j.Entries()
	if len(es) != 2 || es[0].Status != "STARTED" || es[1].Status != StatusWrittenVerified {
		t.Fatalf("journal: %+v", es)
	}
}

func TestExistingParentDirectoryIsNotRecreated(t *testing.T) {
	s := newStandIn(t)
	s.dirs[missionDir+"/champion"] = true
	r := gateA(t)
	p := plan(t, s, r)
	o, err := Execute(context.Background(), s.client(), r, p.ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified || o.DirectoryMade || len(s.mkdirCalls) != 0 {
		t.Fatalf("%v %+v mkdir=%d", err, o, len(s.mkdirCalls))
	}
}

func TestPermissionDenied(t *testing.T) {
	s := newStandIn(t)
	s.tokenStatus = 403
	r, j := gateA(t), journal(t)
	o, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, j)
	if err != nil || o.Status != StatusNotWritten || len(s.transfers) != 0 || !strings.Contains(o.Checks[len(o.Checks)-1].Detail, "kind=permission") {
		t.Fatalf("%v %+v", err, o)
	}
	if _, ok := s.file(champFile); ok {
		t.Fatal("nothing may be written")
	}
	if open, _ := j.Outstanding(testService, r.Path); open {
		t.Fatal("a proven NOT_WRITTEN is not outstanding")
	}
}

func TestExpiredUploadToken(t *testing.T) {
	s := newStandIn(t)
	s.expireTokens = true // issued, but no longer valid when the bytes arrive
	r, j := gateA(t), journal(t)
	o, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, j)
	if err != nil || o.Status != StatusNotWritten || !strings.Contains(o.Transfer, "status=401") || check(o, "read-back") != "PASS" {
		t.Fatalf("%v %+v", err, o)
	}
	if len(s.transfers) != 1 {
		t.Fatalf("no automatic retry with a new token: %d transfers", len(s.transfers))
	}
}

func TestIncorrectDestinationAndBinding(t *testing.T) {
	s := newStandIn(t)
	ctx := context.Background()
	for name, mut := range map[string]func(*Request){
		"other file":         func(r *Request) { r.Path = "champion/other.json" },
		"config file":        func(r *Request) { r.Path = "cfggameplay.json" },
		"other mission":      func(r *Request) { r.Mission = "dayzps_missions/dayzOffline.enoch" },
		"other service":      func(r *Request) { r.Binding.NitradoServiceID = "19806452" },
		"missing binding":    func(r *Request) { r.Binding.InstallationID = 0 },
		"reserved op gate E": func(r *Request) { r.Operation = OpStageItem },
		"reserved op gate G": func(r *Request) { r.Operation = OpUnstageItem },
		"unknown op":         func(r *Request) { r.Operation = "upload-anything" },
		"overwrite expected": func(r *Request) { r.ExpectCurrent = emptySHA },
	} {
		r := gateA(t)
		mut(&r)
		if _, err := Prepare(ctx, s.client(), r); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := Execute(ctx, s.client(), r, "mw-anything", journal(t)); err == nil {
			t.Errorf("%s: executed", name)
		}
	}
	for _, op := range []Operation{OpStageItem, OpUnstageItem} {
		r := gateA(t)
		r.Operation = op
		if err := r.Validate(); !errors.Is(err, ErrOperationNotActive) {
			t.Errorf("%s: %v", op, err)
		}
	}
	// An upload destination that is not https (or has credentials in it) never receives the token.
	nitrado.AllowInsecureUploadURL = false
	defer func() { nitrado.AllowInsecureUploadURL = true }()
	r := gateA(t)
	o, err := Execute(ctx, s.client(), r, plan(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusNotWritten || len(s.transfers) != 0 {
		t.Fatalf("insecure destination: %v %+v", err, o)
	}
	if len(s.uploadCalls)+len(s.mkdirCalls) == 0 {
		t.Fatal("expected the token request to be made and refused locally")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	for _, p := range []string{
		"", "/champion/champion_shop_delivery.json", "../champion/champion_shop_delivery.json", "champion/../cfggameplay.json",
		"champion/./champion_shop_delivery.json", "champion//champion_shop_delivery.json", "champion\\champion_shop_delivery.json",
		"champion/champion_shop_delivery.json/", "champion/champion_shop_delivery.json\x00", "champion/*.json", "champion",
		"champion/champion shop.json", "https://x/y.json", "champion/%2e%2e/x.json", "C:/champion/x.json",
	} {
		if ValidateRelPath(p) == nil {
			t.Errorf("accepted %q", p)
		}
		r := gateA(t)
		r.Path = p
		if r.Validate() == nil {
			t.Errorf("request accepted %q", p)
		}
	}
	if ValidateRelPath("champion/champion_shop_delivery.json") != nil {
		t.Fatal("the Gate A path must be valid")
	}
}

func TestFileAlreadyExists(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(s *standIn){
		"empty file present": func(s *standIn) {
			s.dirs[missionDir+"/champion"] = true
			s.setFile(champFile, []byte("{\n  \"Objects\": []\n}\n"))
		},
		"other file present":  func(s *standIn) { s.dirs[missionDir+"/champion"] = true; s.setFile(champFile, []byte("x")) },
		"directory in place":  func(s *standIn) { s.dirs[missionDir+"/champion"] = true; s.dirs[champFile] = true },
		"file named champion": func(s *standIn) { s.setFile(missionDir+"/champion", []byte("x")) },
	} {
		s := newStandIn(t)
		setup(s)
		r := gateA(t)
		if _, err := Prepare(ctx, s.client(), r); !errors.Is(err, ErrUnexpectedState) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := Execute(ctx, s.client(), r, "mw-x", journal(t)); !errors.Is(err, ErrUnexpectedState) {
			t.Errorf("%s execute: %v", name, err)
		}
		if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
			t.Errorf("%s: a write was attempted", name)
		}
	}
}

func TestChangedConfigurationHash(t *testing.T) {
	ctx := context.Background()
	// 1. Expected hash wrong at preparation.
	s := newStandIn(t)
	r := gateA(t)
	r.ExpectConfigSHA256 = strings.Repeat("0", 64)
	if _, err := Prepare(ctx, s.client(), r); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("stale expectation: %v", err)
	}
	// 2. Spawner list different (Lost City reference removed).
	r = gateA(t)
	r.ExpectSpawners = []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"}
	if _, err := Prepare(ctx, s.client(), r); !errors.Is(err, ErrSpawnersChanged) {
		t.Fatalf("spawners: %v", err)
	}
	// 3. Changed between the owner's approval and execution: the plan ID no longer matches.
	r = gateA(t)
	p := plan(t, s, r)
	changed := strings.Replace(liveConfig, "1.50", "1.51", 1)
	s.setFile(missionDir+"/cfggameplay.json", []byte(changed))
	r2 := r
	r2.ExpectConfigSHA256 = SHA256([]byte(changed))
	if _, err := Execute(ctx, s.client(), r2, p.ID, journal(t)); !errors.Is(err, ErrAuthorizationStale) {
		t.Fatalf("stale authorization: %v", err)
	}
	// 4. Changed during execution (after mkdir, before the token): aborted, nothing uploaded.
	s = newStandIn(t)
	r = gateA(t)
	p = plan(t, s, r)
	s.afterMkdir = func(s *standIn) { s.setFile(missionDir+"/cfggameplay.json", []byte(changed)) }
	o, err := Execute(ctx, s.client(), r, p.ID, journal(t))
	if err != nil || o.Status != StatusNotWritten || len(s.uploadCalls) != 0 || check(o, "pre-write cfggameplay.json") != "FAIL" {
		t.Fatalf("mid-run change: %v %+v", err, o)
	}
}

func TestPartialUploadIsUncertainAndBlocks(t *testing.T) {
	s := newStandIn(t)
	s.partialStore = true
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusUncertain || o.AfterBytes != 10 || check(o, "read-back") != "FAIL" {
		t.Fatalf("%v %+v", err, o)
	}
	// No retry happened, and every further write to this destination is refused until resolved.
	if len(s.transfers) != 1 {
		t.Fatalf("transfers: %d", len(s.transfers))
	}
	s.partialStore = false
	if _, err := Execute(context.Background(), s.client(), r, "mw-new", j); !errors.Is(err, ErrUncertainOutstanding) {
		t.Fatalf("uncertain must block: %v", err)
	}
	if err := j.Resolve(p.ID, testService, r.Path, ""); err == nil {
		t.Fatal("a resolution needs a note")
	}
	if err := j.Resolve(p.ID, testService, r.Path, "owner inspected: 10-byte partial file, deleted in the file browser"); err != nil {
		t.Fatal(err)
	}
	if open, _ := j.Outstanding(testService, r.Path); open {
		t.Fatal("resolved")
	}
}

func TestReadBackMismatch(t *testing.T) {
	s := newStandIn(t)
	s.corruptRead = true
	r, j := gateA(t), journal(t)
	o, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, j)
	if err != nil || o.Status != StatusUncertain || o.After == emptySHA {
		t.Fatalf("%v %+v", err, o)
	}
	if open, _ := j.Outstanding(testService, r.Path); !open {
		t.Fatal("a mismatch must stay outstanding")
	}
}

func TestReportedSuccessButAbsentIsUncertain(t *testing.T) {
	s := newStandIn(t)
	s.claimNoStore = true
	r := gateA(t)
	o, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusUncertain || o.After != Absent {
		t.Fatalf("%v %+v", err, o)
	}
}

func TestNetworkInterruption(t *testing.T) {
	ctx := context.Background()
	// Connection dropped after the server stored the file: the read-back proves the write.
	s := newStandIn(t)
	s.dropAfter = true
	r := gateA(t)
	o, err := Execute(ctx, s.client(), r, plan(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified || !strings.Contains(o.Transfer, "network") {
		t.Fatalf("drop after store: %v %+v", err, o)
	}
	// Dropped before storing: the read-back proves nothing was written.
	s = newStandIn(t)
	s.dropBefore = true
	o, err = Execute(ctx, s.client(), r, plan(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusNotWritten || !strings.Contains(o.Transfer, "network") {
		t.Fatalf("drop before store: %v %+v", err, o)
	}
	// The read-back itself fails after the transfer: UNCERTAIN, never assumed, and outstanding.
	s = newStandIn(t)
	j := journal(t)
	p := plan(t, s, r)
	s.afterTransfer = func(s *standIn) { s.failLists = true }
	o, err = Execute(ctx, s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusUncertain || check(o, "read-back") != "UNVERIFIED" {
		t.Fatalf("read-back failure: %v %+v", err, o)
	}
	if open, _ := j.Outstanding(testService, r.Path); !open {
		t.Fatal("an unverified write must stay outstanding")
	}
}

func TestDuplicateExecution(t *testing.T) {
	s := newStandIn(t)
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	if o, err := Execute(context.Background(), s.client(), r, p.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("%v %+v", err, o)
	}
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("same authorization: %v", err)
	}
	// A fresh plan against the new state is refused too: Gate A never overwrites.
	if _, err := Prepare(context.Background(), s.client(), r); !errors.Is(err, ErrUnexpectedState) {
		t.Fatalf("second run: %v", err)
	}
	if len(s.transfers) != 1 {
		t.Fatalf("transfers: %d", len(s.transfers))
	}
}

func TestInterruptedRunBlocksUntilResolved(t *testing.T) {
	s := newStandIn(t)
	r, j := gateA(t), journal(t)
	// A crash after the authorization was consumed leaves only STARTED.
	_ = j.Append(Entry{PlanID: "mw-crashed", Service: testService, Path: r.Path, Status: "STARTED"})
	if _, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, j); !errors.Is(err, ErrUncertainOutstanding) {
		t.Fatalf("%v", err)
	}
	if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
		t.Fatal("no write after an interrupted run")
	}
}

func TestAuthorizationRequired(t *testing.T) {
	s := newStandIn(t)
	r := gateA(t)
	if _, err := Execute(context.Background(), s.client(), r, " ", journal(t)); !errors.Is(err, ErrAuthorizationMissing) {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), s.client(), r, "mw-000000000000000000000000", journal(t)); !errors.Is(err, ErrAuthorizationStale) {
		t.Fatal(err)
	}
	if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
		t.Fatal("no write without the exact authorization")
	}
}

func TestPayloadIsExactlyTheEmptyFile(t *testing.T) {
	for name, mut := range map[string]func(*Request){
		"crlf": func(r *Request) {
			r.Payload = []byte("{\r\n  \"Objects\": []\r\n}\r\n")
			r.ExpectPayloadSHA256 = SHA256(r.Payload)
		},
		"bom": func(r *Request) {
			r.Payload = append([]byte{0xEF, 0xBB, 0xBF}, r.Payload...)
			r.ExpectPayloadSHA256 = SHA256(r.Payload)
		},
		"one object": func(r *Request) {
			r.Payload = []byte(`{"Objects":[{"name":"BandageDressing"}]}`)
			r.ExpectPayloadSHA256 = SHA256(r.Payload)
		},
		"wrong sha": func(r *Request) { r.ExpectPayloadSHA256 = strings.Repeat("a", 64) },
		"no sha":    func(r *Request) { r.ExpectPayloadSHA256 = "" },
	} {
		r := gateA(t)
		mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRestartDuringWriteIsReported(t *testing.T) {
	s := newStandIn(t)
	s.afterTransfer = func(s *standIn) {
		s.files[configDir+"/DayZServer_PS4_x64_2026-09-26_05-25-00.ADM"] = []byte("AdminLog started\n")
	}
	r := gateA(t)
	o, err := Execute(context.Background(), s.client(), r, plan(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified || check(o, "no restart") != "FAIL" {
		t.Fatalf("%v %+v", err, o)
	}
}

func TestNoSecretInAnyOutput(t *testing.T) {
	s := newStandIn(t)
	r, j := gateA(t), journal(t)
	p := plan(t, s, r)
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(j.path)
	pj, _ := json.Marshal(p)
	oj, _ := json.Marshal(o)
	all := string(raw) + string(pj) + string(oj) + fmt.Sprintf("%v %+v %#v", p, o, o)
	for _, secret := range []string{"upload-secret-token", "secret-path", "api-secret-standin", "dl-secret", testRoot, "/noftp/"} {
		if strings.Contains(all, secret) {
			t.Errorf("output contains %q", secret)
		}
	}
	// Failure paths too.
	s2 := newStandIn(t)
	s2.transferStatus = 500
	o2, _ := Execute(context.Background(), s2.client(), r, plan(t, s2, r).ID, journal(t))
	if b, _ := json.Marshal(o2); strings.Contains(string(b), "secret") {
		t.Errorf("failure output leaks: %s", b)
	}
}
