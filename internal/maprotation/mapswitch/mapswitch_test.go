package mapswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// standIn is a disposable Nitrado API over an in-memory file tree, modelled on the one in
// internal/shop/missionwrite: gameserver details, listing, signed download, upload token and
// upload transfer, driven through the real nitrado.Client. Nothing leaves the test process.
type standIn struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	files     map[string][]byte
	dirs      map[string]bool
	sizes     map[string]int64 // listing size override
	tokens    map[string]string
	tokenSeq  int
	uploads   []string // destination of every upload token request, in order
	transfers int
	otherPOST []string

	// transferMode decides what the n-th transfer (1-based) does: "" stores, "refuse" answers 500
	// and stores nothing, "claim" answers 200 and stores nothing, "partial" stores half.
	transferMode map[int]string
	refuseFrom   int            // >0: every transfer from this one on is refused
	reads        map[string]int // downloads per path
	// changeOnRead replaces a file's content just before its n-th download.
	changeOnRead map[string]int
}

const (
	svcID      = "19806451"
	rootDir    = "/games/ni1_1"
	missionDir = rootDir + "/noftp/dayzps_missions/dayzOffline.chernarusplus"
	customDir  = missionDir + "/custom"

	liveGameplay = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/champion_shop_delivery.json\",\n\t\t\t\"custom/arena1.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"
	wantGameplay = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/champion_shop_delivery.json\",\n\t\t\t\"custom/arena2.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"
	spawns1      = "<playerspawnpoints><fresh><generator_posbubbles><pos x=\"100\" z=\"100\"/></generator_posbubbles></fresh></playerspawnpoints>\n"
	spawns2      = "<playerspawnpoints><fresh><generator_posbubbles><pos x=\"2000\" z=\"2000\"/><pos x=\"2010\" z=\"2020\"/></generator_posbubbles></fresh></playerspawnpoints>\n"
)

func newStandIn(t *testing.T) *standIn {
	prev := nitrado.AllowInsecureUploadURL
	nitrado.AllowInsecureUploadURL = true // the stand-in is plain http on loopback
	t.Cleanup(func() { nitrado.AllowInsecureUploadURL = prev })
	s := &standIn{t: t, tokens: map[string]string{}, dirs: map[string]bool{}, sizes: map[string]int64{}, transferMode: map[int]string{}, reads: map[string]int{}, changeOnRead: map[string]int{},
		files: map[string][]byte{
			missionDir + "/cfggameplay.json":           []byte(liveGameplay),
			missionDir + "/cfgplayerspawnpoints.xml":   []byte(spawns1),
			missionDir + "/db/types.xml":               []byte("<types/>"),
			customDir + "/arena1.json":                 []byte(`{"Objects":[{"name":"one"}]}`),
			customDir + "/arena1_spawns.xml":           []byte(spawns1),
			customDir + "/arena2.json":                 []byte(`{"Objects":[{"name":"two"}]}`),
			customDir + "/arena2_spawns.xml":           []byte(spawns2),
			customDir + "/champion_shop_delivery.json": []byte(`{"Objects":[]}`),
		}}
	for p := range s.files {
		for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
			s.dirs[d] = true
		}
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *standIn) client() *nitrado.Client {
	return nitrado.NewClient(s.srv.URL, "api-secret-standin", nil)
}

func (s *standIn) file(p string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.files[p])
}

func (s *standIn) set(p, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = []byte(content)
}

func (s *standIn) remove(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, p)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	base := "/services/" + svcID
	switch {
	case strings.HasPrefix(r.URL.Path, "/fs/download"):
		p := r.URL.Query().Get("f")
		s.mu.Lock()
		s.reads[p]++
		if n, ok := s.changeOnRead[p]; ok && s.reads[p] == n {
			s.files[p] = append([]byte(" "), s.files[p]...)
		}
		b, ok := s.files[p]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write(b)
	case strings.HasPrefix(r.URL.Path, "/fs/upload"):
		s.transfer(w, r)
	case r.Header.Get("Authorization") != "Bearer api-secret-standin":
		writeJSON(w, 401, map[string]any{"status": "error"})
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers":
		writeJSON(w, 200, map[string]any{"data": map[string]any{"gameserver": map[string]any{
			"service_id": 19806451, "game": "dayzps", "status": "started",
			"game_specific": map[string]any{"path": rootDir + "/noftp/dayzps", "path_available": true, "features": map[string]any{"has_file_browser": true}},
			"settings":      map[string]any{"config": map[string]any{"mission": "dayzOffline.chernarusplus", "enableCfgGameplayFile": "1"}},
		}}})
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers/file_server/list":
		s.list(w, r.URL.Query().Get("dir"))
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers/file_server/download":
		p := r.URL.Query().Get("file")
		s.mu.Lock()
		_, ok := s.files[p]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, 404, map[string]any{"status": "error", "message": "file not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"token": map[string]any{"url": s.srv.URL + "/fs/download?f=" + url.QueryEscape(p) + "&sig=dl-secret", "token": "dl-token"}}})
	case r.Method == http.MethodPost && r.URL.Path == base+"/gameservers/file_server/upload":
		_ = r.ParseForm()
		dest := r.PostForm.Get("path") + "/" + r.PostForm.Get("file")
		s.mu.Lock()
		s.uploads = append(s.uploads, dest)
		s.tokenSeq++
		tok := fmt.Sprintf("upload-secret-token-%d", s.tokenSeq)
		s.tokens[tok] = dest
		seq := s.tokenSeq
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": s.srv.URL + "/fs/upload/secret-path-" + fmt.Sprint(seq), "token": tok}}})
	default:
		if r.Method != http.MethodGet {
			s.mu.Lock()
			s.otherPOST = append(s.otherPOST, r.Method+" "+r.URL.Path)
			s.mu.Unlock()
		}
		writeJSON(w, 404, map[string]any{"status": "error", "message": "no route"})
	}
}

func (s *standIn) list(w http.ResponseWriter, dir string) {
	type entry struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	s.mu.Lock()
	exists := s.dirs[dir]
	var entries []entry
	for p, b := range s.files {
		if path.Dir(p) == dir {
			size := int64(len(b))
			if o, ok := s.sizes[p]; ok {
				size = o
			}
			entries = append(entries, entry{"file", p, path.Base(p), size})
		}
	}
	for d := range s.dirs {
		if path.Dir(d) == dir {
			entries = append(entries, entry{"dir", d, path.Base(d), 0})
		}
	}
	s.mu.Unlock()
	if !exists {
		writeJSON(w, 404, map[string]any{"status": "error", "message": "directory not found"})
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSON(w, 200, map[string]any{"data": map[string]any{"entries": entries}})
}

func (s *standIn) transfer(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tok := r.Header.Get("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transfers++
	dest, ok := s.tokens[tok]
	delete(s.tokens, tok)
	mode := s.transferMode[s.transfers]
	if s.refuseFrom > 0 && s.transfers >= s.refuseFrom {
		mode = "refuse"
	}
	switch {
	case !ok:
		writeJSON(w, 401, map[string]any{"status": "error"})
	case mode == "refuse":
		writeJSON(w, 500, map[string]any{"status": "error", "message": "refused"})
	case mode == "claim":
		writeJSON(w, 200, map[string]any{"status": "success"})
	case mode == "partial":
		s.files[dest] = body[:len(body)/2]
		writeJSON(w, 200, map[string]any{"status": "success"})
	default:
		s.files[dest] = body
		writeJSON(w, 200, map[string]any{"status": "success"})
	}
}

type backups struct {
	saved []Backup
	err   error
}

func (b *backups) save(_ context.Context, bk Backup) error {
	if b.err != nil {
		return b.err
	}
	b.saved = append(b.saved, bk)
	return nil
}

func request() Request {
	return Request{ServiceID: svcID, MapFile: "arena2.json", SpawnFile: "arena2_spawns.xml", Owned: []string{"arena1.json", "arena2.json"}}
}

// unchangedOnServer asserts the two live files are exactly what they were.
func (s *standIn) unchangedOnServer(t *testing.T) {
	t.Helper()
	if s.file(missionDir+"/cfggameplay.json") != liveGameplay || s.file(missionDir+"/cfgplayerspawnpoints.xml") != spawns1 {
		t.Fatalf("the server's files are not what they were:\n%s\n%s", s.file(missionDir+"/cfggameplay.json"), s.file(missionDir+"/cfgplayerspawnpoints.xml"))
	}
}

func TestApplyWritesBothFilesVerifiesAndIsIdempotent(t *testing.T) {
	s := newStandIn(t)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bk := &backups{}
	res := Apply(context.Background(), s.client(), request(), bk.save)
	if res.Status != StatusApplied || res.NeedsAttention || res.Writes != 2 {
		t.Fatalf("apply: %+v", res)
	}
	if got := s.file(missionDir + "/cfggameplay.json"); got != wantGameplay {
		t.Fatalf("cfggameplay.json:\n%s", got)
	}
	if got := s.file(missionDir + "/cfgplayerspawnpoints.xml"); got != spawns2 {
		t.Fatalf("spawn points: %s", got)
	}
	// The backup was saved once, before the writes, with the previous contents.
	if len(bk.saved) != 1 || string(bk.saved[0].Gameplay) != liveGameplay || string(bk.saved[0].Spawns) != spawns1 {
		t.Fatalf("backup: %d saved", len(bk.saved))
	}
	// Only the two target files were ever written, both in the mission folder; no other request
	// that changes anything was sent.
	if strings.Join(s.uploads, " ") != missionDir+"/cfggameplay.json "+missionDir+"/cfgplayerspawnpoints.xml" || len(s.otherPOST) != 0 {
		t.Fatalf("uploads: %v, other writes: %v", s.uploads, s.otherPOST)
	}
	// The owner's files and every other file are untouched.
	if s.file(customDir+"/arena2.json") != `{"Objects":[{"name":"two"}]}` || s.file(missionDir+"/db/types.xml") != "<types/>" {
		t.Fatal("another file changed")
	}

	// A second Apply (a retry after a crash that happened after the writes) changes nothing.
	again := Apply(context.Background(), s.client(), request(), bk.save)
	if again.Status != StatusApplied || again.Writes != 0 || len(bk.saved) != 1 || len(s.uploads) != 2 {
		t.Fatalf("second apply: %+v uploads %v", again, s.uploads)
	}

	// Nothing secret or bulky was logged or reported.
	for _, secret := range []string{"upload-secret-token", "secret-path", "dl-secret", "api-secret-standin", "generator_posbubbles", "objectSpawnersArr"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(res.Message, secret) {
			t.Fatalf("%q leaked into a log line or the result", secret)
		}
	}
}

func TestApplyWritesNothingWhenValidationFails(t *testing.T) {
	cases := map[string]func(s *standIn, r *Request, b *backups){
		"map file missing":   func(s *standIn, _ *Request, _ *backups) { s.remove(customDir + "/arena2.json") },
		"spawn file missing": func(s *standIn, _ *Request, _ *backups) { s.remove(customDir + "/arena2_spawns.xml") },
		"map file empty":     func(s *standIn, _ *Request, _ *backups) { s.set(customDir+"/arena2.json", "") },
		"spawn file empty":   func(s *standIn, _ *Request, _ *backups) { s.set(customDir+"/arena2_spawns.xml", "") },
		"map file not JSON":  func(s *standIn, _ *Request, _ *backups) { s.set(customDir+"/arena2.json", "{oops") },
		"spawn file malformed": func(s *standIn, _ *Request, _ *backups) {
			s.set(customDir+"/arena2_spawns.xml", "<playerspawnpoints><fresh>")
		},
		"spawn file wrong root": func(s *standIn, _ *Request, _ *backups) {
			s.set(customDir+"/arena2_spawns.xml", "<types><pos x=\"1\" z=\"1\"/></types>")
		},
		"spawn file no position": func(s *standIn, _ *Request, _ *backups) {
			s.set(customDir+"/arena2_spawns.xml", "<playerspawnpoints></playerspawnpoints>")
		},
		"map file too large": func(s *standIn, _ *Request, _ *backups) {
			s.sizes[customDir+"/arena2.json"] = maprotation.MaxMapFileBytes + 1
		},
		"spawn file too large": func(s *standIn, _ *Request, _ *backups) {
			s.sizes[customDir+"/arena2_spawns.xml"] = maprotation.MaxSpawnBytes + 1
		},
		"cfggameplay malformed":    func(s *standIn, _ *Request, _ *backups) { s.set(missionDir+"/cfggameplay.json", `{"WorldsData": {`) },
		"cfggameplay no worlds":    func(s *standIn, _ *Request, _ *backups) { s.set(missionDir+"/cfggameplay.json", `{"version": 1}`) },
		"live spawn file missing":  func(s *standIn, _ *Request, _ *backups) { s.remove(missionDir + "/cfgplayerspawnpoints.xml") },
		"map name with a folder":   func(_ *standIn, r *Request, _ *backups) { r.MapFile = "../arena2.json" },
		"spawn name not xml":       func(_ *standIn, r *Request, _ *backups) { r.SpawnFile = "arena2.json" },
		"spawn name with a folder": func(_ *standIn, r *Request, _ *backups) { r.SpawnFile = "custom/arena2_spawns.xml" },
		"backup cannot be saved":   func(_ *standIn, _ *Request, b *backups) { b.err = errors.New("database down") },
		"no service":               func(_ *standIn, r *Request, _ *backups) { r.ServiceID = "" },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStandIn(t)
			before := map[string]string{}
			req, bk := request(), &backups{}
			breakIt(s, &req, bk)
			for p, b := range s.files {
				before[p] = string(b)
			}
			res := Apply(context.Background(), s.client(), req, bk.save)
			if res.Status != StatusFailed || res.NeedsAttention || res.Writes != 0 || res.Message == "" {
				t.Fatalf("result: %+v", res)
			}
			if len(s.uploads) != 0 || s.transfers != 0 || len(s.otherPOST) != 0 {
				t.Fatalf("a write was attempted: %v", s.uploads)
			}
			for p, b := range s.files {
				if before[p] != string(b) {
					t.Fatalf("%s changed", p)
				}
			}
			if !strings.Contains(res.Message, "Nothing was changed") && !strings.Contains(res.Message, "nothing was changed") && !strings.Contains(res.Message, "not started") {
				t.Fatalf("the reason is not plain: %q", res.Message)
			}
		})
	}
}

func TestApplyRollsBackWhenTheSecondWriteFails(t *testing.T) {
	for name, mode := range map[string]string{"refused": "refuse", "reported success, stored nothing": "claim", "stored half": "partial"} {
		t.Run(name, func(t *testing.T) {
			s := newStandIn(t)
			s.transferMode[2] = mode // the spawn points write
			bk := &backups{}
			res := Apply(context.Background(), s.client(), request(), bk.save)
			if res.Status != StatusRolledBack || res.NeedsAttention {
				t.Fatalf("result: %+v", res)
			}
			s.unchangedOnServer(t)
			// cfggameplay.json was written, then restored from the backup and verified.
			if s.uploads[0] != missionDir+"/cfggameplay.json" || s.uploads[len(s.uploads)-1] == customDir {
				t.Fatalf("uploads: %v", s.uploads)
			}
			for _, u := range s.uploads {
				if u != missionDir+"/cfggameplay.json" && u != missionDir+"/cfgplayerspawnpoints.xml" {
					t.Fatalf("wrote outside the two target files: %s", u)
				}
			}
			if len(bk.saved) != 1 {
				t.Fatalf("backup saved %d times", len(bk.saved))
			}
		})
	}
}

func TestApplyReportsWhenTheRestoreFailsToo(t *testing.T) {
	s := newStandIn(t)
	s.refuseFrom = 2 // the spawn write and every restore are refused
	res := Apply(context.Background(), s.client(), request(), (&backups{}).save)
	if res.Status != StatusFailed || !res.NeedsAttention || !strings.Contains(res.Message, "could NOT be put back") {
		t.Fatalf("result: %+v", res)
	}
	// The state is exactly what the message says: the first file changed, the second did not.
	if s.file(missionDir+"/cfggameplay.json") != wantGameplay || s.file(missionDir+"/cfgplayerspawnpoints.xml") != spawns1 {
		t.Fatal("unexpected server state")
	}
	// One attempt per write: the failed spawn write and the failed restore, never a loop.
	if s.transfers != 3 {
		t.Fatalf("transfers: %d", s.transfers)
	}
}

func TestApplyFirstWriteFailures(t *testing.T) {
	// Refused: nothing changed, the second file is never attempted.
	s := newStandIn(t)
	s.transferMode[1] = "refuse"
	res := Apply(context.Background(), s.client(), request(), (&backups{}).save)
	if res.Status != StatusFailed || res.NeedsAttention || len(s.uploads) != 1 {
		t.Fatalf("refused: %+v uploads %v", res, s.uploads)
	}
	s.unchangedOnServer(t)

	// Stored half: the read-back catches it, and the file is restored and verified.
	s = newStandIn(t)
	s.transferMode[1] = "partial"
	res = Apply(context.Background(), s.client(), request(), (&backups{}).save)
	if res.Status != StatusRolledBack || res.NeedsAttention {
		t.Fatalf("partial: %+v", res)
	}
	s.unchangedOnServer(t)
	for _, u := range s.uploads {
		if u != missionDir+"/cfggameplay.json" {
			t.Fatalf("the spawn file must not be written after a failed first write: %v", s.uploads)
		}
	}
}

// The file changed between validation and the write (someone edited it): nothing is sent.
func TestApplyStopsWhenAFileChangesBeforeTheWrite(t *testing.T) {
	s := newStandIn(t)
	s.changeOnRead[missionDir+"/cfggameplay.json"] = 2 // 1 = validation, 2 = the re-read before the write
	res := Apply(context.Background(), s.client(), request(), (&backups{}).save)
	if res.Status != StatusFailed || res.NeedsAttention || len(s.uploads) != 0 {
		t.Fatalf("result: %+v uploads %v", res, s.uploads)
	}
}

// A crash after the first write: the next attempt carries the saved backup, writes only the file
// that is still missing and never replaces the backup.
func TestApplyResumesAfterACrash(t *testing.T) {
	s := newStandIn(t)
	s.set(missionDir+"/cfggameplay.json", wantGameplay) // the first attempt got this far
	bk := &backups{}
	req := request()
	req.Prior = &Backup{Gameplay: []byte(liveGameplay), Spawns: []byte(spawns1)}
	res := Apply(context.Background(), s.client(), req, bk.save)
	if res.Status != StatusApplied || res.Writes != 1 || len(bk.saved) != 0 {
		t.Fatalf("resume: %+v, backups saved %d", res, len(bk.saved))
	}
	if strings.Join(s.uploads, " ") != missionDir+"/cfgplayerspawnpoints.xml" || s.file(missionDir+"/cfgplayerspawnpoints.xml") != spawns2 || s.file(missionDir+"/cfggameplay.json") != wantGameplay {
		t.Fatalf("resume wrote %v", s.uploads)
	}

	// The same, but the remaining write fails: the first file goes back to the ORIGINAL backup.
	s = newStandIn(t)
	s.set(missionDir+"/cfggameplay.json", wantGameplay)
	s.transferMode[1] = "refuse"
	res = Apply(context.Background(), s.client(), req, bk.save)
	if res.Status != StatusRolledBack || len(bk.saved) != 0 {
		t.Fatalf("resume with failure: %+v", res)
	}
	s.unchangedOnServer(t)
}

// Switching to a map whose files are already live but whose entry is one of several own entries.
func TestApplyRemovesEveryOwnMapEntry(t *testing.T) {
	s := newStandIn(t)
	s.set(missionDir+"/cfggameplay.json", `{"WorldsData": {"objectSpawnersArr": ["custom/arena1.json", "custom/other.json", "custom/old_removed.json"]}}`)
	req := request()
	req.Owned = append(req.Owned, "old_removed.json")
	if res := Apply(context.Background(), s.client(), req, (&backups{}).save); res.Status != StatusApplied {
		t.Fatalf("apply: %+v", res)
	}
	if got := s.file(missionDir + "/cfggameplay.json"); got != `{"WorldsData": {"objectSpawnersArr": ["custom/arena2.json", "custom/other.json"]}}` {
		t.Fatalf("cfggameplay.json: %s", got)
	}
}
