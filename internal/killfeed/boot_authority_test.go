package killfeed

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Champion Live Sync phase 2.1: ADM source authority regressions, modelled on the production
// incidents of 2026-09-24 (a quiet 124-byte boot selected only after the 8-minute stale path, and a
// noftp listing gap that switched to a days-old ftproot ADM).

const (
	noftpCfg  = "/games/svc_2/noftp/dayzps/config"
	ftpCfg    = "/games/svc_2/ftproot/dayzps/config"
	bootAName = "DayZServer_PS4_x64_2026-09-24_08-08-14.ADM"
	bootBName = "DayZServer_PS4_x64_2026-09-24_09-17-09.ADM"
	oldName   = "DayZServer_PS4_x64_2026-09-21_08-17-52.ADM"
)

// quietADM is the real shape of a boot's ADM with no players: 124 bytes, header only.
func quietADM(date, clock string) string {
	return "\n\n******************************************************************************\nAdminLog started on " + date + " at " + clock + "\n"
}

type bootFile struct {
	content  []byte
	modified time.Time
}

type bootFake struct {
	mu        sync.Mutex
	files     map[string]*bootFile
	hiddenDir map[string]bool // a listing gap: this directory lists nothing
	readFail  map[string]int  // fail the next N reads of a path
	statFail  bool            // every metadata call fails (transport error)
	reads     map[string]int
	listLogs  int
}

func newBootFake() *bootFake {
	return &bootFake{files: map[string]*bootFile{}, hiddenDir: map[string]bool{}, readFail: map[string]int{}, reads: map[string]int{}}
}

func (f *bootFake) put(p, content string, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = &bootFile{content: []byte(content), modified: modified}
}

func (f *bootFake) entries(dir string) []nitrado.LogFile {
	var out []nitrado.LogFile
	for p, bf := range f.files {
		d := path.Dir(p)
		if (dir != "" && d != dir) || f.hiddenDir[d] {
			continue
		}
		out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: d, Size: int64(len(bf.content)), Modified: bf.modified, Type: "ADM", Source: "file_server"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

func (f *bootFake) ListLogs(context.Context, string) ([]nitrado.LogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listLogs++
	if f.statFail {
		return nil, &nitrado.RequestError{Op: "list", Kind: nitrado.KindTemporary, StatusCode: 503}
	}
	return f.entries(""), nil
}

func (f *bootFake) ListLogsInDir(_ context.Context, _ string, dir string) ([]nitrado.LogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statFail {
		return nil, &nitrado.RequestError{Op: "list", Kind: nitrado.KindTemporary, StatusCode: 503}
	}
	return f.entries(dir), nil
}

func (f *bootFake) StatFile(_ context.Context, _ string, p string) (*nitrado.LogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statFail {
		return nil, &nitrado.RequestError{Op: "stat", Kind: nitrado.KindTemporary, StatusCode: 503}
	}
	for _, e := range f.entries(path.Dir(p)) {
		if e.Path == p {
			e := e
			return &e, nil
		}
	}
	return nil, &nitrado.RequestError{Op: "stat", Kind: nitrado.KindNotFound, StatusCode: http.StatusNotFound}
}

func (f *bootFake) ReadLog(_ context.Context, _ string, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[p]++
	if f.readFail[p] > 0 {
		f.readFail[p]--
		return nil, errors.New("transient")
	}
	bf, ok := f.files[p]
	if !ok {
		return nil, &nitrado.RequestError{Op: "read", Kind: nitrado.KindNotFound, StatusCode: http.StatusNotFound}
	}
	return append([]byte(nil), bf.content...), nil
}

func (f *bootFake) readCount(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[p]
}

type countingKillPublisher struct {
	mu    sync.Mutex
	kills int
}

func (c *countingKillPublisher) PublishKill(*Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kills++
	return nil
}

func (c *countingKillPublisher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kills
}

// bootEngine starts an engine on boot A and returns it after its first selection and read.
func bootEngine(t *testing.T, f *bootFake) (*Engine, *countingKillPublisher) {
	t.Helper()
	e := NewEngine(f, "svc", NewADMParser())
	pub := &countingKillPublisher{}
	e.SetKillPublisher(pub)
	ctx := context.Background()
	for i := 0; i < 3 && (e.selected == nil || e.tracker.LastByteOffset == 0); i++ {
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if e.selected == nil || e.selected.Name != bootAName {
		t.Fatalf("boot A selected first: %+v", e.selected)
	}
	return e, pub
}

// forceScan makes the next poll run the boot scan and, optionally, look stale (> staleGiveUpAfter).
func forceScan(e *Engine, stale bool) {
	e.lastBootScan = time.Time{}
	if stale {
		e.lastLogChange = time.Now().Add(-staleGiveUpAfter - time.Minute)
		e.lastStaleRediscoveryAt = time.Time{}
	}
}

func TestQuietNewBootSelectedWithoutStaleRediscovery(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0)
	e, _ := bootEngine(t, f)

	// Boot B appears: a quiet 124-byte header whose size and mtime will never change.
	f.put(noftpCfg+"/"+bootBName, quietADM("2026-09-24", "09:17:09"), t0.Add(69*time.Minute))
	listBefore := f.listLogs
	forceScan(e, false) // the old file is NOT yet stale: no 8-minute give-up path involved
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.selected == nil || e.selected.Name != bootBName || e.selectionReason != "newer_boot_verified" {
		t.Fatalf("a verified newer quiet boot must be selected on the next boot scan: %+v reason=%s", e.selected, e.selectionReason)
	}
	if f.listLogs != listBefore {
		t.Fatal("the boot scan lists only the known ADM directories, never a full discovery walk")
	}
	if b := e.BootAuthority(); b.AcceptedFile != "dayzps/config/"+bootBName || b.AcceptedBoot.Format("15:04:05") != "09:17:09" {
		t.Fatalf("accepted boot: %+v", b)
	}
	if e.tracker.LastByteOffset != int64(len(quietADM("2026-09-24", "09:17:09"))) {
		// the new boot is read from byte 0 (its checkpoint is established on the first poll after the switch)
		if err := e.PollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if e.tracker.CurrentLogFile != noftpCfg+"/"+bootBName {
		t.Fatalf("checkpoint established on boot B: %q", e.tracker.CurrentLogFile)
	}
}

func TestQuietNewBootSelectedEvenWhenOldFileIsStale(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0)
	e, _ := bootEngine(t, f)
	f.put(noftpCfg+"/"+bootBName, quietADM("2026-09-24", "09:17:09"), t0.Add(69*time.Minute))
	forceScan(e, true) // production shape: the quiet old ADM already exceeded staleGiveUpAfter
	if err := e.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.selected.Name != bootBName {
		t.Fatalf("boot scan runs before the stale branch: %s", e.selected.Name)
	}
}

func TestNewBootSeenOnceUnchangedIsNotDemoted(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0)
	e, _ := bootEngine(t, f)
	bPath := noftpCfg + "/" + bootBName
	f.put(bPath, quietADM("2026-09-24", "09:17:09"), t0.Add(69*time.Minute))
	f.readFail[bPath] = 1 // the first verification read fails: not selectable yet
	forceScan(e, false)
	_ = e.PollOnce(context.Background())
	if e.selected.Name != bootAName {
		t.Fatal("an unverified candidate is never selected")
	}
	// Seen before, size and mtime unchanged (the activity model would call it STALE), and a full
	// discovery pass has recorded it in candidate history: it is still the newer boot.
	e.updateCandidateHistory(f.entries(""), time.Now())
	forceScan(e, false)
	_ = e.PollOnce(context.Background())
	if e.selected.Name != bootBName {
		t.Fatalf("a quiet newer boot seen before must still be selected once verified: %s", e.selected.Name)
	}
	if b := e.BootAuthority(); b.UnverifiedCandidates != 1 {
		t.Fatalf("the failed verification is counted: %+v", b)
	}
}

func TestNewerStampWithMismatchedHeaderIsNotPromoted(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 12, 8, 14, 0, time.UTC)
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0)
	e, _ := bootEngine(t, f)
	// A file named like a newer boot whose content states another boot is not boot evidence.
	f.put(noftpCfg+"/"+bootBName, quietADM("2026-09-23", "01:00:00"), t0.Add(time.Hour))
	forceScan(e, false)
	_ = e.PollOnce(context.Background())
	if e.selected.Name != bootAName {
		t.Fatal("a header that contradicts the filename must not be promoted")
	}
}

// The 2026-09-24 13:19 incident: the noftp listing lost the files and ftproot held a days-old ADM.
func TestListingGapNeverSwitchesBackwardOrReplaysHistory(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 13, 17, 9, 0, time.UTC)
	bPath := noftpCfg + "/" + bootBName
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0.Add(-69*time.Minute))
	e, pub := bootEngine(t, f)
	f.put(bPath, quietADM("2026-09-24", "09:17:09"), t0)
	forceScan(e, false)
	_ = e.PollOnce(context.Background())
	if e.selected.Name != bootBName {
		t.Fatal("setup: boot B accepted")
	}
	bOffset := e.tracker.LastByteOffset
	_ = e.PollOnce(context.Background())
	bOffset = e.tracker.LastByteOffset

	// A days-old ADM, never ingested, full of kills - and ftproot lists it with a NEWER mtime than
	// anything noftp shows, while noftp lists nothing at all.
	oldPath := ftpCfg + "/" + oldName
	kill := `16:40:12 | Player "victim1" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "killer1" (id=k1 pos=<4.0, 5.0, 6.0>) with M4-A1 from 62.1978 meters` + "\n"
	f.put(oldPath, quietADM("2026-09-21", "08:17:52")+kill+kill, time.Now())
	f.mu.Lock()
	f.hiddenDir[noftpCfg] = true
	f.mu.Unlock()
	e.rememberADMDirs([]nitrado.LogFile{{Path: oldPath, Directory: ftpCfg}})

	for i := 0; i < 4; i++ {
		forceScan(e, true) // stale branch -> full rediscovery, the path that switched backward in production
		_ = e.PollOnce(context.Background())
	}
	if e.selected == nil || canonicalADMID(e.selected.Path) != "dayzps/config/"+bootBName {
		t.Fatalf("the accepted boot must be retained across a listing gap: %+v", e.selected)
	}
	if f.readCount(oldPath) != 0 {
		t.Fatalf("a historical ADM must never be read into the pipeline: %d reads", f.readCount(oldPath))
	}
	if pub.count() != 0 {
		t.Fatalf("no historical kill may be republished: %d", pub.count())
	}
	if _, tracked := e.tracker.Checkpoints[oldPath]; tracked {
		t.Fatal("no checkpoint may be created for the historical file")
	}
	if b := e.BootAuthority(); b.RejectedOlder == 0 || b.AcceptedFile != "dayzps/config/"+bootBName {
		t.Fatalf("rejections counted, accepted boot unchanged: %+v", b)
	}

	// The gap closes: boot B resumes at its own checkpoint, not from byte 0.
	f.mu.Lock()
	f.hiddenDir[noftpCfg] = false
	f.mu.Unlock()
	forceScan(e, false)
	_ = e.PollOnce(context.Background())
	if e.tracker.Checkpoints[e.selected.Path].Offset < bOffset && e.tracker.LastByteOffset < bOffset {
		t.Fatalf("checkpoint preserved: %d < %d", e.tracker.LastByteOffset, bOffset)
	}
}

func TestSelectLogRefusesAnOlderBoot(t *testing.T) {
	f := newBootFake()
	t0 := time.Date(2026, 9, 24, 13, 17, 9, 0, time.UTC)
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), t0)
	e, _ := bootEngine(t, f)
	before := *e.selected
	if e.selectLog(nitrado.LogFile{Name: oldName, Path: ftpCfg + "/" + oldName, Directory: ftpCfg, Size: 124}) {
		t.Fatal("selectLog must refuse an older boot")
	}
	if e.selected.Path != before.Path || e.state != StatePolling {
		t.Fatalf("the current selection is kept: %+v %s", e.selected, e.state)
	}
	// The same boot through the other mount is not older: aliases stay one source.
	if !e.selectLog(nitrado.LogFile{Name: bootAName, Path: ftpCfg + "/" + bootAName, Directory: ftpCfg, Size: 124}) {
		t.Fatal("a mount alias of the accepted boot is admissible")
	}
}

// captureLogs swaps the default logger for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestSessionLogOnlyClaimsCurrentWhenDatabaseAccepts(t *testing.T) {
	buf := captureLogs(t)
	e := NewEngine(&fakeLogSource{}, "svc", NewADMParser())
	e.guildID, e.serverID = 7, 9
	store := &recordingSessionStore{reject: true}
	e.SetADMSessionStore(store)
	e.noteADMSession(ftpCfg + "/" + oldName)
	out := buf.String()
	if strings.Contains(out, "adm_session_current") || !strings.Contains(out, "event=session_rejected") || !strings.Contains(out, "reason=older_than_recorded_session") {
		t.Fatalf("a refused session must be logged as rejected, never as current:\n%s", out)
	}
	if strings.Contains(out, "/games/") {
		t.Fatalf("only the canonical identity is logged:\n%s", out)
	}
	buf.Reset()
	store.reject = false
	e.noteADMSession(noftpCfg + "/" + bootBName)
	if !strings.Contains(buf.String(), "adm_session_current") {
		t.Fatalf("an accepted session is logged as current:\n%s", buf.String())
	}
}

func TestPollCycleHeartbeatAndSourceHealth(t *testing.T) {
	f := newBootFake()
	f.put(noftpCfg+"/"+bootAName, quietADM("2026-09-24", "08:08:14"), time.Now())
	e, _ := bootEngine(t, f)
	var cycles []PollOutcome
	e.OnPollCycle(func(o PollOutcome) { cycles = append(cycles, o) })

	// A quiet, working worker: every cycle reports, and the source is QUIET - not dead, not failing.
	for i := 0; i < 3; i++ {
		_ = e.PollOnce(context.Background())
		e.firePollCycle(nil)
	}
	if len(cycles) != 3 || cycles[2].TransportStreak != 0 {
		t.Fatalf("every completed cycle reports (heartbeat): %+v", cycles)
	}
	h := e.SourceHealth()
	now := time.Now()
	h.LastChangeAt = now.Add(-10 * time.Minute)
	if st, _ := ClassifyADMSourceHealth(h, now); st != ADMQuiet {
		t.Fatalf("quiet but healthy: %s", st)
	}

	// A real transport failure is reported as one, never as quiet.
	f.mu.Lock()
	f.statFail = true
	f.mu.Unlock()
	for i := 0; i < 3; i++ {
		_ = e.PollOnce(context.Background())
		e.firePollCycle(nil)
	}
	h = e.SourceHealth()
	if st, reason := ClassifyADMSourceHealth(h, time.Now()); st != ADMTransportError || !strings.Contains(reason, "temporary") {
		t.Fatalf("transport failures: %s %s (streak %d)", st, reason, h.TransportStreak)
	}
}

func TestClassifyADMSourceHealth(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 30, 0, 0, time.UTC)
	base := ADMSourceHealth{LastCycleAt: now.Add(-2 * time.Second), LastChangeAt: now.Add(-30 * time.Second),
		AcceptedFile: "dayzps/config/B.ADM", NewestListedFile: "dayzps/config/B.ADM", NewestListedSince: now.Add(-time.Hour)}
	cases := []struct {
		name string
		mod  func(*ADMSourceHealth)
		want string
	}{
		{"healthy", func(*ADMSourceHealth) {}, ADMHealthy},
		{"quiet", func(h *ADMSourceHealth) { h.LastChangeAt = now.Add(-time.Hour) }, ADMQuiet},
		{"stalled worker", func(h *ADMSourceHealth) { h.LastCycleAt = now.Add(-5 * time.Minute) }, ADMWorkerStalled},
		{"never ran", func(h *ADMSourceHealth) { h.LastCycleAt = time.Time{} }, ADMWorkerStalled},
		{"transport", func(h *ADMSourceHealth) { h.TransportStreak = 3; h.LastErrorClass = "nitrado_temporary_503" }, ADMTransportError},
		{"newer boot not accepted", func(h *ADMSourceHealth) {
			h.NewestListedFile, h.NewestListedSince = "dayzps/config/C.ADM", now.Add(-time.Minute)
		}, ADMSourceLagging},
		{"newer boot within grace", func(h *ADMSourceHealth) {
			h.NewestListedFile, h.NewestListedSince = "dayzps/config/C.ADM", now.Add(-10*time.Second)
		}, ADMHealthy},
		{"players online, ADM not advancing", func(h *ADMSourceHealth) { h.OnlinePlayers = 2; h.LastChangeAt = now.Add(-6 * time.Minute) }, ADMSourceLagging},
		{"transport beats quiet", func(h *ADMSourceHealth) { h.TransportStreak = 5; h.LastChangeAt = now.Add(-time.Hour) }, ADMTransportError},
	}
	for _, c := range cases {
		h := base
		c.mod(&h)
		if got, _ := ClassifyADMSourceHealth(h, now); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestNewerBootFallbackToReadableMountAlias(t *testing.T) {
 f:=newBootFake()
 now:=time.Date(2026,9,24,12,8,14,0,time.UTC)
 f.put(noftpCfg+"/"+bootAName,quietADM("2026-09-24","08:08:14"),now)
 e,_:=bootEngine(t,f)
 mainPath:=noftpCfg+"/"+bootBName
 backupPath:=ftpCfg+"/"+bootBName
 header:=quietADM("2026-09-24","09:17:09")
 f.put(mainPath,header,now.Add(time.Hour))
 f.put(backupPath,header,now.Add(time.Hour))
 f.readFail[mainPath]=2
 // The newer boot is still authoritative if the noftp representation is
 // listed but temporarily cannot be read. A verified ftproot alias is safe.
 forceScan(e,false)
 if err:=e.PollOnce(context.Background());err!=nil {t.Fatal(err)}
 if e.selected==nil||canonicalADMID(e.selected.Path)!=canonicalADMID(backupPath)||
  e.selected.Path!=backupPath {t.Fatalf("verified fallback alias not promoted: %+v",e.selected)}
 if got:=e.BootAuthority();got.LastCandidateReason!=""||got.AcceptedFile!=canonicalADMID(backupPath){
  t.Fatalf("wrong verification/authority diagnostics: %+v",got)
 }
 if f.readCount(mainPath)!=1||f.readCount(backupPath)!=1 {
  t.Fatalf("unexpected mount probe count: primary=%d secondary=%d",f.readCount(mainPath),f.readCount(backupPath))
 }
 if e.selectLog(nitrado.LogFile{Name:oldName,Path:ftpCfg+"/"+oldName}) {
  t.Fatal("fallback must not permit regression to an older boot")
 }
}

func TestNewerBootUnverifiedAliasesNeverPromote(t *testing.T) {
 f:=newBootFake()
 now:=time.Date(2026,9,24,12,8,14,0,time.UTC)
 f.put(noftpCfg+"/"+bootAName,quietADM("2026-09-24","08:08:14"),now)
 e,_:=bootEngine(t,f)
 primary:=noftpCfg+"/"+bootBName
 secondary:=ftpCfg+"/"+bootBName
 f.put(primary,quietADM("2026-09-23","01:00:00"),now.Add(time.Hour))
 f.put(secondary,quietADM("2026-09-23","01:00:00"),now.Add(time.Hour))
 forceScan(e,false)
 if err:=e.PollOnce(context.Background());err!=nil {t.Fatal(err)}
 if e.selected.Name!=bootAName {t.Fatalf("unverified boot promoted: %+v",e.selected)}
 st:=e.BootAuthority()
 if st.LastCandidateFile!=canonicalADMID(primary)||st.LastCandidateReason!="header_does_not_match_filename"||st.LastCandidateCheckAt.IsZero(){
  t.Fatalf("missing failure provenance: %+v",st)
 }
}
