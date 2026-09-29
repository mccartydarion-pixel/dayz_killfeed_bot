package missionwrite

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
)

const (
	cfgFile    = missionDir + "/cfggameplay.json"
	backupDir  = missionDir + "/champion/backup"
	champEmpty = "{\n  \"Objects\": []\n}\n"
)

var (
	liveSHA    = SHA256([]byte(liveConfig))
	backupFile = missionDir + "/" + canary.ConfigBackupPath(SHA256([]byte(liveConfig)))
)

// afterGateA is the stand-in in the state Gate A left on Champions: champion/ with the empty file.
func afterGateA(t *testing.T) *standIn {
	s := newStandIn(t)
	s.dirs[missionDir+"/champion"] = true
	s.files[champFile] = []byte(champEmpty)
	return s
}

func proposed(t *testing.T) []byte {
	t.Helper()
	p, err := canary.ProposePatch([]byte(liveConfig))
	if err != nil {
		t.Fatal(err)
	}
	return p.Proposed
}

func gateB(t *testing.T) Request {
	return Request{
		Operation:           OpReferenceChampionFile,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "cfggameplay.json",
		ExpectCurrent:       liveSHA,
		ExpectConfigSHA256:  liveSHA,
		ExpectSpawners:      []string{"custom/The_Lost_City.json"},
		ExpectPayloadSHA256: SHA256(proposed(t)),
	}
}

func rollback(t *testing.T, current []byte, spawners []string) Request {
	return Request{
		Operation:           OpRestoreConfig,
		Binding:             capability.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: testService},
		Mission:             testMission,
		Path:                "cfggameplay.json",
		ExpectCurrent:       SHA256(current),
		ExpectConfigSHA256:  SHA256(current),
		ExpectSpawners:      spawners,
		ExpectPayloadSHA256: liveSHA,
	}
}

func prepB(t *testing.T, s *standIn, r Request) ConfigPlan {
	t.Helper()
	cp, err := PrepareConfig(context.Background(), s.client(), r)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return cp
}

func TestGateBBackupThenByteExactPatch(t *testing.T) {
	s := afterGateA(t)
	r, j := gateB(t), journal(t)
	cp := prepB(t, s, r)
	if cp.BackupState != Absent || cp.BackupDirExists || cp.ChampionState != emptySHA || cp.PayloadSHA256 != SHA256(proposed(t)) ||
		cp.BackupPath != "champion/backup/cfggameplay.json."+liveSHA[:12]+".bak" || !strings.HasPrefix(cp.ID, "mw-") {
		t.Fatalf("plan: %+v", cp)
	}
	if len(s.uploadCalls)+len(s.mkdirCalls)+len(s.transfers) != 0 {
		t.Fatal("planning must not write")
	}
	o, err := Execute(context.Background(), s.client(), r, cp.ID, j)
	if err != nil || o.Status != StatusWrittenVerified || o.Before != liveSHA || o.After != cp.PayloadSHA256 {
		t.Fatalf("%v %+v", err, o)
	}
	// Order and exact bytes: mkdir champion/backup; backup token + the ORIGINAL bytes; config token + the patch.
	if len(s.mkdirCalls) != 1 || s.mkdirCalls[0].Get("path") != missionDir+"/champion" || s.mkdirCalls[0].Get("name") != "backup" {
		t.Fatalf("mkdir: %v", s.mkdirCalls)
	}
	if len(s.uploadCalls) != 2 || s.uploadCalls[0].Get("path") != backupDir || s.uploadCalls[1].Get("path") != missionDir || s.uploadCalls[1].Get("file") != "cfggameplay.json" {
		t.Fatalf("tokens: %v", s.uploadCalls)
	}
	if string(s.transfers[0].body) != liveConfig || string(s.transfers[1].body) != string(proposed(t)) {
		t.Fatal("transfer bodies")
	}
	if b, _ := s.file(backupFile); string(b) != liveConfig {
		t.Fatal("backup is not the exact original")
	}
	got, _ := s.file(cfgFile)
	// Byte preservation: only the array changed; everything else (e.g. the "1.50" literal) is identical.
	i := strings.Index(liveConfig, "[")
	k := strings.Index(liveConfig, "]") + 1
	if !strings.HasPrefix(string(got), liveConfig[:i]) || !strings.HasSuffix(string(got), liveConfig[k:]) ||
		!strings.Contains(string(got), "\"custom/The_Lost_City.json\",\n\t\t\t\"champion/champion_shop_delivery.json\"") {
		t.Fatalf("patched config:\n%s", got)
	}
	for _, c := range []string{"backup", "read-back", "objectSpawnersArr", "referenced spawner files present", "Champion file unchanged", "no restart"} {
		if check(o, c) != "PASS" {
			t.Errorf("%s: %s (%+v)", c, check(o, c), o.Checks)
		}
	}
	if c, _ := s.file(champFile); string(c) != champEmpty {
		t.Fatal("Champion file touched")
	}
	// Re-running is refused (already referenced) - and the consumed ID is dead.
	if _, err := PrepareConfig(context.Background(), s.client(), r); err == nil {
		t.Fatal("a second Gate B must be refused")
	}
	if _, err := Execute(context.Background(), s.client(), r, cp.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("reuse: %v", err)
	}
}

func TestGateBReusesAnExistingVerifiedBackup(t *testing.T) {
	s := afterGateA(t)
	s.dirs[backupDir] = true
	s.files[backupFile] = []byte(liveConfig)
	r := gateB(t)
	cp := prepB(t, s, r)
	o, err := Execute(context.Background(), s.client(), r, cp.ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified || len(s.uploadCalls) != 1 || len(s.mkdirCalls) != 0 {
		t.Fatalf("%v %+v tokens=%d", err, o, len(s.uploadCalls))
	}
}

func TestGateBStaleStateRefusal(t *testing.T) {
	ctx := context.Background()
	changed := strings.Replace(liveConfig, "1.50", "1.51", 1)
	for name, c := range map[string]struct {
		setup func(s *standIn)
		want  error
	}{
		"config changed":          {func(s *standIn) { s.files[cfgFile] = []byte(changed) }, ErrConfigChanged},
		"Champion file absent":    {func(s *standIn) { delete(s.files, champFile) }, ErrChampionNotEmpty},
		"Champion file not empty": {func(s *standIn) { s.files[champFile] = []byte(`{"Objects":[{}]}`) }, ErrChampionNotEmpty},
		"Lost City reference gone": {func(s *standIn) {
			s.files[cfgFile] = []byte(strings.Replace(liveConfig, "\t\t\t\"custom/The_Lost_City.json\"\n", "", 1))
		}, nil},
		"already referenced":         {func(s *standIn) { s.files[cfgFile] = proposed(t) }, nil},
		"conflicting backup present": {func(s *standIn) { s.dirs[backupDir] = true; s.files[backupFile] = []byte("x") }, ErrBackupConflict},
	} {
		s := afterGateA(t)
		c.setup(s)
		r := gateB(t)
		_, err := PrepareConfig(ctx, s.client(), r)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: %v", name, err)
		}
		if len(s.uploadCalls)+len(s.mkdirCalls)+len(s.transfers) != 0 {
			t.Errorf("%s: a write was attempted", name)
		}
	}
	// A caller-supplied payload is refused (the patch is always derived from the live bytes).
	r := gateB(t)
	r.Payload = proposed(t)
	if r.Validate() == nil {
		t.Error("custom payload accepted")
	}
	// The config changes after the owner approved the plan: stale.
	s := afterGateA(t)
	cp := prepB(t, s, gateB(t))
	s.files[cfgFile] = []byte(changed)
	r2 := gateB(t)
	r2.ExpectCurrent, r2.ExpectConfigSHA256 = SHA256([]byte(changed)), SHA256([]byte(changed))
	p2, _ := canary.ProposePatch([]byte(changed))
	r2.ExpectPayloadSHA256 = SHA256(p2.Proposed)
	if _, err := Execute(ctx, s.client(), r2, cp.ID, journal(t)); !errors.Is(err, ErrAuthorizationStale) {
		t.Fatalf("stale: %v", err)
	}
	// The config changes AFTER the backup and before the overwrite: aborted, config untouched.
	s = afterGateA(t)
	cp = prepB(t, s, gateB(t))
	s.afterTransfer = func(s *standIn) { s.files[cfgFile] = []byte(changed) } // runs after the backup transfer
	o, err := Execute(ctx, s.client(), gateB(t), cp.ID, journal(t))
	if err != nil || o.Status != StatusNotWritten || len(s.uploadCalls) != 1 || check(o, "backup") != "PASS" || check(o, "pre-write cfggameplay.json") != "FAIL" {
		t.Fatalf("mid-run change: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != changed {
		t.Fatal("the tool must not have written the config")
	}
}

func TestGateBBackupFailureLeavesConfigUntouched(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(s *standIn){
		"backup folder refused":  func(s *standIn) { s.mkdirStatus = 403 },
		"backup token refused":   func(s *standIn) { s.tokenStatus = 403 },
		"backup transfer 507":    func(s *standIn) { s.onlyTransfer = 1; s.transferStatus = 507 },
		"backup partial":         func(s *standIn) { s.onlyTransfer = 1; s.partialStore = true },
		"backup reads back diff": func(s *standIn) { s.corruptSuffix = ".bak" },
	} {
		s := afterGateA(t)
		setup(s)
		r := gateB(t)
		o, err := Execute(ctx, s.client(), r, prepB(t, s, r).ID, journal(t))
		if err != nil || o.Status != StatusNotWritten {
			t.Errorf("%s: %v %+v", name, err, o)
		}
		if got, _ := s.file(cfgFile); string(got) != liveConfig {
			t.Errorf("%s: config touched", name)
		}
		for _, u := range s.uploadCalls {
			if u.Get("file") == "cfggameplay.json" {
				t.Errorf("%s: a config upload was requested", name)
			}
		}
	}
}

func TestGateBInterruptedUploadAndReadBackMismatch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		setup  func(s *standIn)
		status string
	}{
		{"partial config upload", func(s *standIn) { s.onlyTransfer = 2; s.partialStore = true }, StatusUncertain},
		{"dropped before storing", func(s *standIn) { s.onlyTransfer = 2; s.dropBefore = true }, StatusNotWritten},
		{"dropped after storing", func(s *standIn) { s.onlyTransfer = 2; s.dropAfter = true }, StatusWrittenVerified},
		{"success claimed, unchanged", func(s *standIn) { s.onlyTransfer = 2; s.claimNoStore = true }, StatusUncertain},
		{"read-back mismatch", func(s *standIn) {
			// Downloads of the config diverge only after the config upload (the 2nd transfer).
			s.afterTransfer = func(s *standIn) {
				if len(s.transfers) == 2 {
					s.corruptSuffix = "/cfggameplay.json"
				}
			}
		}, StatusUncertain},
	}
	for _, c := range cases {
		s := afterGateA(t)
		r, j := gateB(t), journal(t)
		cp := prepB(t, s, r)
		c.setup(s)
		o, err := Execute(ctx, s.client(), r, cp.ID, j)
		if err != nil || o.Status != c.status {
			t.Errorf("%s: %v %+v", c.name, err, o)
			continue
		}
		if c.status == StatusUncertain {
			if open, _ := j.Outstanding(testService, r.Path); !open {
				t.Errorf("%s: UNCERTAIN must block further writes", c.name)
			}
			if _, err := Execute(ctx, s.client(), r, "mw-other", j); !errors.Is(err, ErrUncertainOutstanding) {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		if n := len(s.transfers); n > 2 {
			t.Errorf("%s: %d transfers (no retry allowed)", c.name, n)
		}
	}
}

// Rollback: a separate, separately authorized restore from the verified server backup - after a
// verified Gate B, and after a partial upload that left an invalid configuration.
func TestGateBRollbackProcedures(t *testing.T) {
	ctx := context.Background()
	s := afterGateA(t)
	j := journal(t)
	cp := prepB(t, s, gateB(t))
	if o, err := Execute(ctx, s.client(), gateB(t), cp.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("gate B: %v %+v", err, o)
	}
	rb := rollback(t, proposed(t), []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"})
	if _, err := Execute(ctx, s.client(), rb, cp.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("Gate B's plan ID must never authorize the rollback: %v", err)
	}
	rp, err := PrepareConfig(ctx, s.client(), rb)
	if err != nil || rp.ID == cp.ID || rp.PayloadSHA256 != liveSHA {
		t.Fatalf("rollback plan: %v %+v", err, rp)
	}
	o, err := Execute(ctx, s.client(), rb, rp.ID, j)
	if err != nil || o.Status != StatusWrittenVerified || check(o, "objectSpawnersArr") != "PASS" {
		t.Fatalf("rollback: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != liveConfig {
		t.Fatal("rollback must restore the exact original bytes")
	}
	if _, err := PrepareConfig(ctx, s.client(), rb); err == nil {
		t.Fatal("nothing left to restore")
	}

	// After a partial upload: resolve the UNCERTAIN outcome, then roll back the damaged file.
	s = afterGateA(t)
	j = journal(t)
	cp = prepB(t, s, gateB(t))
	s.onlyTransfer, s.partialStore = 2, true
	if o, _ := Execute(ctx, s.client(), gateB(t), cp.ID, j); o.Status != StatusUncertain {
		t.Fatalf("partial: %+v", o)
	}
	damaged, _ := s.file(cfgFile)
	rb = rollback(t, damaged, []string{})
	if _, err := Execute(ctx, s.client(), rb, "mw-x", j); !errors.Is(err, ErrUncertainOutstanding) {
		t.Fatalf("rollback before resolution: %v", err)
	}
	if err := j.Resolve(cp.ID, testService, "cfggameplay.json", "owner inspected: partial config; rolling back"); err != nil {
		t.Fatal(err)
	}
	rp = prepB(t, s, rb)
	o, err = Execute(ctx, s.client(), rb, rp.ID, j)
	if err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("rollback of the damaged file: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != liveConfig {
		t.Fatal("not restored")
	}
	// A rollback without a verified backup is refused.
	s = afterGateA(t)
	s.files[cfgFile] = proposed(t)
	if _, err := PrepareConfig(ctx, s.client(), rollback(t, proposed(t), []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"})); !errors.Is(err, ErrBackupMissing) {
		t.Fatalf("no backup: %v", err)
	}
	s.dirs[backupDir] = true
	s.files[backupFile] = []byte("tampered")
	if _, err := PrepareConfig(ctx, s.client(), rollback(t, proposed(t), []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"})); !errors.Is(err, ErrBackupMissing) {
		t.Fatalf("tampered backup: %v", err)
	}
}

func TestGateANeverAuthorizesGateB(t *testing.T) {
	s := newStandIn(t)
	j := journal(t)
	a := gateA(t)
	pa := plan(t, s, a)
	if o, err := Execute(context.Background(), s.client(), a, pa.ID, j); err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("gate A: %v %+v", err, o)
	}
	if _, err := Execute(context.Background(), s.client(), gateB(t), pa.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("Gate A's consumed plan ID: %v", err)
	}
	cp := prepB(t, s, gateB(t))
	if cp.ID == pa.ID {
		t.Fatal("distinct operations must have distinct plan IDs")
	}
}

// A run that creates champion/backup and then fails leaves the config untouched, consumes its plan ID,
// and the next dry run yields a NEW plan ID that can proceed (the folder is reused, not recreated).
func TestGateBRetryAfterBackupFolderCreated(t *testing.T) {
	ctx := context.Background()
	for name, fail := range map[string]func(s *standIn){
		"mkdir error but folder created": func(s *standIn) { s.mkdirThenFail = true },
		"folder created, backup refused": func(s *standIn) { s.tokenStatus = 403 },
		"folder created, backup dropped": func(s *standIn) { s.onlyTransfer = 1; s.dropBefore = true },
		"folder created, backup 507":     func(s *standIn) { s.onlyTransfer = 1; s.transferStatus = 507 },
	} {
		s := afterGateA(t)
		j := journal(t)
		first := prepB(t, s, gateB(t))
		fail(s)
		o, err := Execute(ctx, s.client(), gateB(t), first.ID, j)
		if err != nil || o.Status != StatusNotWritten || !s.dirs[backupDir] {
			t.Errorf("%s: %v %+v folder=%t", name, err, o, s.dirs[backupDir])
			continue
		}
		if got, _ := s.file(cfgFile); string(got) != liveConfig {
			t.Errorf("%s: config touched", name)
		}
		*s = standIn{t: s.t, srv: s.srv, serviceID: s.serviceID, root: s.root, files: s.files, dirs: s.dirs, status: s.status, secret: s.secret, tokens: map[string]string{}}
		second := prepB(t, s, gateB(t))
		if second.ID == first.ID || !second.BackupDirExists {
			t.Errorf("%s: retry plan %s (first %s) dirExists=%t", name, second.ID, first.ID, second.BackupDirExists)
			continue
		}
		if _, err := Execute(ctx, s.client(), gateB(t), first.ID, j); !errors.Is(err, ErrAuthorizationUsed) {
			t.Errorf("%s: the consumed ID must stay dead: %v", name, err)
		}
		o, err = Execute(ctx, s.client(), gateB(t), second.ID, j)
		if err != nil || o.Status != StatusWrittenVerified || len(s.mkdirCalls) != 0 {
			t.Errorf("%s: retry %v %+v mkdir=%d", name, err, o, len(s.mkdirCalls))
		}
	}
}

// The server backup is read back (listed and downloaded) after its upload and BEFORE the config
// upload token is requested; a backup that does not read back never lets the overwrite happen.
func TestGateBBackupReadBackPrecedesOverwrite(t *testing.T) {
	s := afterGateA(t)
	var seq []string
	s.onRequest = func(r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/fs/upload"):
			seq = append(seq, "transfer")
		case strings.HasPrefix(r.URL.Path, "/fs/download") && strings.HasSuffix(r.URL.Query().Get("f"), ".bak"):
			seq = append(seq, "backup-read")
		case strings.HasSuffix(r.URL.Path, "/file_server/upload"):
			seq = append(seq, "token")
		}
	}
	r := gateB(t)
	o, err := Execute(context.Background(), s.client(), r, prepB(t, s, r).ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified {
		t.Fatalf("%v %+v", err, o)
	}
	got := strings.Join(seq, ",")
	// backup token, backup transfer, backup read-back (x2: verification and pre-write re-check), config token, config transfer
	if !strings.HasPrefix(got, "token,transfer,backup-read,backup-read,token,transfer") {
		t.Fatalf("order: %s", got)
	}
}

func TestGateBInterruptedBackupUpload(t *testing.T) {
	ctx := context.Background()
	// Dropped after the server stored it: the read-back proves the backup, Gate B proceeds.
	s := afterGateA(t)
	r := gateB(t)
	cp := prepB(t, s, r)
	s.onlyTransfer, s.dropAfter = 1, true
	if o, err := Execute(ctx, s.client(), r, cp.ID, journal(t)); err != nil || o.Status != StatusWrittenVerified || check(o, "backup") != "PASS" {
		t.Fatalf("drop after store: %v %+v", err, o)
	}
	// Dropped before storing: no backup, no overwrite.
	s = afterGateA(t)
	cp = prepB(t, s, r)
	s.onlyTransfer, s.dropBefore = 1, true
	o, err := Execute(ctx, s.client(), r, cp.ID, journal(t))
	if err != nil || o.Status != StatusNotWritten || check(o, "backup") != "FAIL" || len(s.uploadCalls) != 1 {
		t.Fatalf("drop before store: %v %+v", err, o)
	}
	if got, _ := s.file(cfgFile); string(got) != liveConfig {
		t.Fatal("config touched")
	}
	// A partial backup left behind blocks the next plan until the owner inspects it.
	s = afterGateA(t)
	cp = prepB(t, s, r)
	s.onlyTransfer, s.partialStore = 1, true
	if o, _ := Execute(ctx, s.client(), r, cp.ID, journal(t)); o.Status != StatusNotWritten || !strings.Contains(o.Checks[len(o.Checks)-1].Detail, "unexpected content") {
		t.Fatalf("partial backup: %+v", o)
	}
	if _, err := PrepareConfig(ctx, s.client(), r); !errors.Is(err, ErrBackupConflict) {
		t.Fatalf("partial backup must block the next plan: %v", err)
	}
}

func TestGateBRollbackRefusals(t *testing.T) {
	ctx := context.Background()
	s := afterGateA(t)
	j := journal(t)
	cp := prepB(t, s, gateB(t))
	if o, _ := Execute(ctx, s.client(), gateB(t), cp.ID, j); o.Status != StatusWrittenVerified {
		t.Fatalf("gate B: %+v", o)
	}
	both := []string{"custom/The_Lost_City.json", "champion/champion_shop_delivery.json"}
	// Stale current hash (the config changed after the rollback was planned).
	rb := rollback(t, proposed(t), both)
	rp := prepB(t, s, rb)
	s.files[cfgFile] = []byte(strings.Replace(string(proposed(t)), "1.50", "1.52", 1))
	if _, err := Execute(ctx, s.client(), rb, rp.ID, j); err == nil {
		t.Fatal("stale rollback executed")
	}
	// Wrong expected spawners, custom payload, rollback to a different digest.
	s.files[cfgFile] = proposed(t)
	bad := rollback(t, proposed(t), []string{"custom/The_Lost_City.json"})
	if _, err := PrepareConfig(ctx, s.client(), bad); !errors.Is(err, ErrSpawnersChanged) {
		t.Fatalf("spawners: %v", err)
	}
	bad = rollback(t, proposed(t), both)
	bad.Payload = []byte(liveConfig)
	if bad.Validate() == nil {
		t.Fatal("custom rollback payload accepted")
	}
	bad = rollback(t, proposed(t), both)
	bad.ExpectPayloadSHA256 = strings.Repeat("1", 64)
	if _, err := PrepareConfig(ctx, s.client(), bad); !errors.Is(err, ErrBackupMissing) {
		t.Fatalf("other digest: %v", err)
	}
}

// Dual mount (Nitrado): the server's own directory (game_specific.path) shows the newest boot while
// the mission's mount lags. The boot identity must come from the newest across both, so a restart
// that only appears in the server's mount is still detected.
func TestBootIdentityUsesNewestAcrossMounts(t *testing.T) {
	s := afterGateA(t)
	serverCfg := testRoot + "/ftproot/dayzps/config"
	s.gameMount = "ftproot"
	for d := serverCfg; d != testRoot; d = d[:strings.LastIndex(d, "/")] {
		s.dirs[d] = true
	}
	s.files[serverCfg+"/DayZServer_PS4_x64_2026-09-29_04-59-38.ADM"] = []byte("AdminLog started\n") // newer than the mission mount's
	r := gateB(t)
	cp := prepB(t, s, r)
	if cp.Inspection.BootFile != "DayZServer_PS4_x64_2026-09-29_04-59-38.ADM" {
		t.Fatalf("boot identity from the lagging mount: %s", cp.Inspection.BootFile)
	}
	s.afterTransfer = func(s *standIn) {
		if len(s.transfers) == 2 { // the config upload: a restart visible only in the server's mount
			s.files[serverCfg+"/DayZServer_PS4_x64_2026-09-29_06-07-38.ADM"] = []byte("AdminLog started\n")
		}
	}
	o, err := Execute(context.Background(), s.client(), r, cp.ID, journal(t))
	if err != nil || o.Status != StatusWrittenVerified || check(o, "no restart") != "FAIL" {
		t.Fatalf("%v %+v", err, o)
	}
}
