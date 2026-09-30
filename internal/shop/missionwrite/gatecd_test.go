package missionwrite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

const customChamp = missionDir + "/custom/champion_shop_delivery.json"

// gateBConfig is the live configuration after Gate B: Lost City + the legacy champion/ entry.
func gateBConfig(t *testing.T) []byte { return proposed(t) }

func relocated(t *testing.T) []byte {
	t.Helper()
	p, err := canary.ProposeRelocation(gateBConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return p.Proposed
}

// afterGateB is the stand-in in the state Champions is in now: legacy file, legacy reference, the
// original-config backup, and custom/ with Lost City.
func afterGateB(t *testing.T) *standIn {
	s := afterGateA(t)
	s.files[cfgFile] = gateBConfig(t)
	s.dirs[backupDir] = true
	s.files[backupFile] = []byte(liveConfig)
	return s
}

// afterGateC adds the empty custom/ Champion file.
func afterGateC(t *testing.T) *standIn {
	s := afterGateB(t)
	s.files[customChamp] = []byte(champEmpty)
	return s
}

var legacySpawners = []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"}

func gateC(t *testing.T) Request {
	payload, sha := canary.EmptyArtifact()
	return Request{
		Operation:           OpCreateCustomChampionFile,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "custom/champion_shop_delivery.json",
		ExpectCurrent:       Absent,
		ExpectConfigSHA256:  SHA256(gateBConfig(t)),
		ExpectSpawners:      legacySpawners,
		Payload:             payload,
		ExpectPayloadSHA256: sha,
	}
}

func gateD(t *testing.T) Request {
	return Request{
		Operation:           OpRelocateChampionReference,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "cfggameplay.json",
		ExpectCurrent:       SHA256(gateBConfig(t)),
		ExpectConfigSHA256:  SHA256(gateBConfig(t)),
		ExpectSpawners:      legacySpawners,
		ExpectPayloadSHA256: SHA256(relocated(t)),
	}
}

func TestGateCCreatesInTheExistingCustomFolder(t *testing.T) {
	s := afterGateB(t)
	r, j := gateC(t), journal(t)
	p := plan(t, s, r)
	if !p.Inspection.ParentExists || p.Inspection.Current != Absent || len(p.Steps) == 0 || strings.Contains(strings.Join(p.Steps, "\n"), "mkdir") {
		t.Fatalf("plan: %+v", p)
	}
	o, err := Execute(context.Background(), s.client(), r, p.ID, j)
	if err != nil || o.Status != StatusWrittenVerified || o.After != emptySHA || o.AfterBytes != 20 || o.Directory != DirNotNeeded {
		t.Fatalf("%v %+v", err, o)
	}
	if len(s.mkdirCalls) != 0 || len(s.uploadCalls) != 1 || s.uploadCalls[0].Get("path") != missionDir+"/custom" || s.uploadCalls[0].Get("file") != "champion_shop_delivery.json" {
		t.Fatalf("mkdir=%d tokens=%v", len(s.mkdirCalls), s.uploadCalls)
	}
	if got, _ := s.file(customChamp); string(got) != champEmpty {
		t.Fatalf("stored %q", got)
	}
	if got, _ := s.file(cfgFile); string(got) != string(gateBConfig(t)) {
		t.Fatal("Gate C must not touch the configuration")
	}
	if got, _ := s.file(missionDir + "/custom/The_Lost_City.json"); string(got) != `{"Objects":[]}` {
		t.Fatal("Lost City touched")
	}
	if check(o, "cfggameplay.json unchanged") != "PASS" || check(o, "no restart") != "PASS" {
		t.Fatalf("%+v", o.Checks)
	}
	// Duplicate execution: the ID is dead, and a new plan is refused (the file exists).
	if _, err := Execute(context.Background(), s.client(), r, p.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := Prepare(context.Background(), s.client(), r); !errors.Is(err, ErrUnexpectedState) {
		t.Fatalf("second Gate C: %v", err)
	}
}

func TestGateCNeverCreatesFolders(t *testing.T) {
	s := afterGateB(t)
	delete(s.dirs, missionDir+"/custom")
	delete(s.files, missionDir+"/custom/The_Lost_City.json")
	if _, err := Prepare(context.Background(), s.client(), gateC(t)); !errors.Is(err, ErrParentMissing) {
		t.Fatalf("%v", err)
	}
	if _, err := Execute(context.Background(), s.client(), gateC(t), "mw-x", journal(t)); !errors.Is(err, ErrParentMissing) {
		t.Fatalf("%v", err)
	}
	if len(s.mkdirCalls)+len(s.uploadCalls)+len(s.transfers) != 0 {
		t.Fatal("a write was attempted")
	}
}

func TestGateCDestinationConflicts(t *testing.T) {
	for name, setup := range map[string]func(s *standIn){
		"empty file already there": func(s *standIn) { s.files[customChamp] = []byte(champEmpty) },
		"other content there":      func(s *standIn) { s.files[customChamp] = []byte(`{"Objects":[{}]}`) },
		"a folder in its place":    func(s *standIn) { s.dirs[customChamp] = true },
	} {
		s := afterGateB(t)
		setup(s)
		if _, err := Prepare(context.Background(), s.client(), gateC(t)); !errors.Is(err, ErrUnexpectedState) {
			t.Errorf("%s: %v", name, err)
		}
		if len(s.uploadCalls)+len(s.transfers) != 0 {
			t.Errorf("%s: a write was attempted", name)
		}
	}
	// Stale configuration hash or spawner list.
	s := afterGateB(t)
	r := gateC(t)
	r.ExpectConfigSHA256 = liveSHA
	if _, err := Prepare(context.Background(), s.client(), r); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("stale config: %v", err)
	}
	r = gateC(t)
	r.ExpectSpawners = []string{"custom/The_Lost_City.json"}
	if _, err := Prepare(context.Background(), s.client(), r); !errors.Is(err, ErrSpawnersChanged) {
		t.Fatalf("stale spawners: %v", err)
	}
}

func TestGateCInterruptedUploadAndReadBackMismatch(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		setup  func(s *standIn)
		status string
	}{
		{"partial", func(s *standIn) { s.partialStore = true }, StatusUncertain},
		{"dropped before storing", func(s *standIn) { s.dropBefore = true }, StatusNotWritten},
		{"dropped after storing", func(s *standIn) { s.dropAfter = true }, StatusWrittenVerified},
		{"expired token", func(s *standIn) { s.expireTokens = true }, StatusNotWritten},
		{"read-back mismatch", func(s *standIn) {
			s.afterTransfer = func(s *standIn) { s.corruptSuffix = "/custom/champion_shop_delivery.json" }
		}, StatusUncertain},
	} {
		s := afterGateB(t)
		r, j := gateC(t), journal(t)
		p := plan(t, s, r)
		c.setup(s)
		o, err := Execute(ctx, s.client(), r, p.ID, j)
		if err != nil || o.Status != c.status || len(s.transfers) > 1 {
			t.Errorf("%s: %v %+v transfers=%d", c.name, err, o, len(s.transfers))
			continue
		}
		if c.status == StatusUncertain {
			if _, err := Execute(ctx, s.client(), r, "mw-other", j); !errors.Is(err, ErrUncertainOutstanding) {
				t.Errorf("%s: UNCERTAIN must block: %v", c.name, err)
			}
		}
	}
}

func TestGateDRelocatesOnlyTheReference(t *testing.T) {
	s := afterGateC(t)
	r, j := gateD(t), journal(t)
	cp := prepB(t, s, r)
	freshBackup := "champion/backup/cfggameplay.json." + SHA256(gateBConfig(t))[:12] + ".bak"
	if cp.BackupPath != freshBackup || cp.BackupState != Absent || !cp.BackupDirExists || cp.ChampionState != emptySHA ||
		strings.Join(cp.SpawnersAfter, ",") != "custom/The_Lost_City.json,custom/champion_shop_delivery.json" {
		t.Fatalf("plan: %+v", cp)
	}
	o, err := Execute(context.Background(), s.client(), r, cp.ID, j)
	if err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("%v %+v", err, o)
	}
	// A fresh backup of the CURRENT (post-Gate-B) config first; the folder exists, so no mkdir.
	if len(s.mkdirCalls) != 0 || len(s.uploadCalls) != 2 || s.uploadCalls[0].Get("path") != backupDir || s.uploadCalls[1].Get("file") != "cfggameplay.json" {
		t.Fatalf("mkdir=%d tokens=%v", len(s.mkdirCalls), s.uploadCalls)
	}
	if b, _ := s.file(missionDir + "/" + freshBackup); string(b) != string(gateBConfig(t)) {
		t.Fatal("fresh backup is not the exact current config")
	}
	if b, _ := s.file(backupFile); string(b) != liveConfig {
		t.Fatal("the original-config backup must be preserved")
	}
	// Only the one entry changed.
	got, _ := s.file(cfgFile)
	if string(got) != strings.Replace(string(gateBConfig(t)), "\"champion/champion_shop_delivery.json\"", "\"custom/champion_shop_delivery.json\"", 1) {
		t.Fatalf("config:\n%s", got)
	}
	for _, c := range []string{"backup", "read-back", "objectSpawnersArr", "referenced spawner files present", "Champion file unchanged", "no restart"} {
		if check(o, c) != "PASS" {
			t.Errorf("%s: %s", c, check(o, c))
		}
	}
	if l, _ := s.file(champFile); string(l) != champEmpty {
		t.Fatal("the legacy file is left alone")
	}
	// Duplicate: the ID is dead, and a second relocation is refused.
	if _, err := Execute(context.Background(), s.client(), r, cp.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("reuse: %v", err)
	}
	r2 := gateD(t)
	r2.ExpectCurrent, r2.ExpectConfigSHA256 = SHA256(got), SHA256(got)
	r2.ExpectSpawners = []string{"custom/The_Lost_City.json", "custom/champion_shop_delivery.json"}
	if _, err := PrepareConfig(context.Background(), s.client(), r2); err == nil {
		t.Fatal("a second relocation must be refused")
	}
}

func TestGateDStaleStateRefusal(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		setup func(s *standIn)
		want  error
	}{
		"Gate C not done":       {func(s *standIn) { delete(s.files, customChamp) }, ErrChampionNotEmpty},
		"custom file not empty": {func(s *standIn) { s.files[customChamp] = []byte(`{"Objects":[{}]}`) }, ErrChampionNotEmpty},
		"config changed": {func(s *standIn) {
			s.files[cfgFile] = []byte(strings.Replace(string(gateBConfig(t)), "1.50", "1.51", 1))
		}, ErrConfigChanged},
		"legacy reference missing": {func(s *standIn) { s.files[cfgFile] = []byte(liveConfig) }, nil},
		"already relocated":        {func(s *standIn) { s.files[cfgFile] = relocated(t) }, nil},
		"conflicting fresh backup": {func(s *standIn) {
			s.files[missionDir+"/champion/backup/cfggameplay.json."+SHA256(gateBConfig(t))[:12]+".bak"] = []byte("x")
		}, ErrBackupConflict},
	} {
		s := afterGateC(t)
		c.setup(s)
		_, err := PrepareConfig(ctx, s.client(), gateD(t))
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: %v", name, err)
		}
		if len(s.uploadCalls)+len(s.transfers)+len(s.mkdirCalls) != 0 {
			t.Errorf("%s: a write was attempted", name)
		}
	}
	r := gateD(t)
	r.Payload = relocated(t)
	if r.Validate() == nil {
		t.Fatal("custom payload accepted")
	}
}

func TestGateDInterruptionsAndRollback(t *testing.T) {
	ctx := context.Background()
	// Backup failure: config untouched.
	s := afterGateC(t)
	cp := prepB(t, s, gateD(t))
	s.onlyTransfer, s.transferStatus = 1, 507
	if o, err := Execute(ctx, s.client(), gateD(t), cp.ID, journal(t)); err != nil || o.Status != StatusNotWritten {
		t.Fatalf("backup failure: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != string(gateBConfig(t)) {
		t.Fatal("config touched")
	}
	// Partial config upload: UNCERTAIN, blocked; resolved; then restored from the FRESH backup.
	s = afterGateC(t)
	j := journal(t)
	cp = prepB(t, s, gateD(t))
	s.onlyTransfer, s.partialStore = 2, true
	if o, _ := Execute(ctx, s.client(), gateD(t), cp.ID, j); o.Status != StatusUncertain {
		t.Fatalf("partial: %+v", o)
	}
	damaged, _ := s.file(cfgFile)
	rb := rollback(t, damaged, []string{})
	rb.ExpectPayloadSHA256 = SHA256(gateBConfig(t))
	if _, err := Execute(ctx, s.client(), rb, "mw-x", j); !errors.Is(err, ErrUncertainOutstanding) {
		t.Fatalf("rollback before resolution: %v", err)
	}
	if err := j.Resolve(cp.ID, testService, "cfggameplay.json", "owner inspected: partial config"); err != nil {
		t.Fatal(err)
	}
	s.partialStore = false
	rp := prepB(t, s, rb)
	if o, err := Execute(ctx, s.client(), rb, rp.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("rollback: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != string(gateBConfig(t)) {
		t.Fatal("not restored to the post-Gate-B config")
	}
	// After a verified relocation, the ORIGINAL pre-Gate-B config is still restorable.
	s = afterGateC(t)
	j = journal(t)
	cp = prepB(t, s, gateD(t))
	if o, _ := Execute(ctx, s.client(), gateD(t), cp.ID, j); o.Status != StatusWrittenVerified {
		t.Fatal("relocation")
	}
	full := rollback(t, relocated(t), []string{"custom/The_Lost_City.json", "custom/champion_shop_delivery.json"})
	fp := prepB(t, s, full)
	if o, err := Execute(ctx, s.client(), full, fp.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("full rollback: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != liveConfig {
		t.Fatal("not restored to the original config")
	}
}

func TestRelocationPlanIDsAreDistinctAndRetirement(t *testing.T) {
	s := afterGateB(t)
	j := journal(t)
	pc := plan(t, s, gateC(t))
	if o, err := Execute(context.Background(), s.client(), gateC(t), pc.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("gate C: %v %+v", err, o)
	}
	if _, err := Execute(context.Background(), s.client(), gateD(t), pc.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("Gate C's ID must never authorize Gate D: %v", err)
	}
	pd := prepB(t, s, gateD(t))
	if pd.ID == pc.ID {
		t.Fatal("distinct plan IDs")
	}
	if !Retired(OpCreateEmptyChampionFile) || !Retired(OpReferenceChampionFile) || Retired(OpCreateCustomChampionFile) || Retired(OpRelocateChampionReference) || Retired(OpRestoreConfig) {
		t.Fatal("retirement flags")
	}
	if nitradodelivery.ArtifactRelPath != "custom/champion_shop_delivery.json" || nitradodelivery.LegacyArtifactRelPath != "champion/champion_shop_delivery.json" {
		t.Fatal("paths")
	}
}
