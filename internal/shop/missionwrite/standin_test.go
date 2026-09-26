package missionwrite

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// standIn is a disposable Nitrado API: the endpoints the real nitrado.Client uses for discovery,
// listing, signed download, upload token, upload transfer and mkdir, over an in-memory file tree.
// Nothing leaves the test process.
type standIn struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex

	serviceID string
	root      string // /games/ni1_1
	files     map[string][]byte
	dirs      map[string]bool
	status    string

	secret      string            // the API bearer the client must present
	tokens      map[string]string // upload token -> destination path
	tokenSeq    int
	uploadCalls []url.Values
	transfers   []transfer
	mkdirCalls  []url.Values

	// Failure knobs.
	tokenStatus    int                   // non-zero: the token request answers this status
	transferStatus int                   // non-zero: the transfer answers this status (after storing nothing)
	partialStore   bool                  // store only half the bytes, answer 200
	dropBefore     bool                  // hijack and close the transfer connection before storing
	dropAfter      bool                  // store, then hijack and close without a response
	claimNoStore   bool                  // answer 200 but store nothing
	corruptRead    bool                  // downloads of the destination return other bytes
	mkdirStatus    int                   // non-zero: mkdir answers this status and creates nothing
	insecureURL    string                // token response URL override
	beforeToken    func(s *standIn)      // hook run when the token is requested
	afterMkdir     func(s *standIn)      // hook run after a successful mkdir
	expireTokens   bool                  // tokens are issued but have expired by the transfer
	afterTransfer  func(s *standIn)      // hook run after a transfer
	failLists      bool                  // every directory listing fails with 500
	onRequest      func(r *http.Request) // observation hook
}

type transfer struct {
	contentType string
	body        []byte
	token       string
}

const (
	testService = "19806451"
	testRoot    = "/games/ni1_1"
	missionDir  = testRoot + "/noftp/dayzps_missions/dayzOffline.chernarusplus"
	testMission = "dayzps_missions/dayzOffline.chernarusplus"
	configDir   = testRoot + "/noftp/dayzps/config"
	liveConfig  = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/The_Lost_City.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"
)

func newStandIn(t *testing.T) *standIn {
	s := &standIn{
		t: t, serviceID: testService, root: testRoot, status: "started", secret: "api-secret-standin",
		files: map[string][]byte{
			missionDir + "/cfggameplay.json":                          []byte(liveConfig),
			missionDir + "/custom/The_Lost_City.json":                 []byte(`{"Objects":[]}`),
			missionDir + "/db/types.xml":                              []byte("<types/>"),
			configDir + "/DayZServer_PS4_x64_2026-09-26_04-17-25.ADM": []byte("AdminLog started\n"),
		},
		dirs:   map[string]bool{},
		tokens: map[string]string{},
	}
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
	return nitrado.NewClient(s.srv.URL, s.secret, nil)
}

func (s *standIn) file(p string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[p]
	return b, ok
}

func (s *standIn) setFile(p string, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *standIn) serve(w http.ResponseWriter, r *http.Request) {
	if s.onRequest != nil {
		s.onRequest(r)
	}
	// File-server endpoints (signed download and upload transfer) authenticate by URL/token, not bearer.
	switch {
	case strings.HasPrefix(r.URL.Path, "/fs/download"):
		s.serveDownload(w, r)
		return
	case strings.HasPrefix(r.URL.Path, "/fs/upload"):
		s.serveTransfer(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.secret {
		writeJSON(w, 401, map[string]any{"status": "error", "message": "unauthorized"})
		return
	}
	base := "/services/" + s.serviceID
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/token":
		writeJSON(w, 200, map[string]any{"data": map[string]any{"token": map[string]any{"scopes": []string{"service"}}}})
	case r.Method == http.MethodGet && r.URL.Path == base:
		writeJSON(w, 200, map[string]any{"data": map[string]any{"service": map[string]any{
			"id": 19806451, "type": "gameserver", "status": "active", "is_owner": true,
			"roles":   []string{"ROLE_WEBINTERFACE_FILEBROWSER_READ", "ROLE_WEBINTERFACE_FILEBROWSER_WRITE"},
			"details": map[string]any{"game": "dayzps"},
		}}})
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers":
		s.mu.Lock()
		st := s.status
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"data": map[string]any{"gameserver": map[string]any{
			"service_id": 19806451, "game": "dayzps", "status": st,
			"game_specific": map[string]any{"path": s.root + "/noftp/dayzps", "path_available": true, "features": map[string]any{"has_file_browser": true}},
			"settings":      map[string]any{"config": map[string]any{"mission": "dayzOffline.chernarusplus", "enableCfgGameplayFile": "1"}},
		}}})
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers/file_server/list":
		s.serveList(w, r)
	case r.Method == http.MethodGet && r.URL.Path == base+"/gameservers/file_server/download":
		p := r.URL.Query().Get("file")
		if _, ok := s.file(p); !ok {
			writeJSON(w, 404, map[string]any{"status": "error", "message": "file not found"})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]any{"token": map[string]any{"url": s.srv.URL + "/fs/download?f=" + url.QueryEscape(p) + "&sig=dl-secret", "token": "dl-token"}}})
	case r.Method == http.MethodPost && r.URL.Path == base+"/gameservers/file_server/upload":
		s.serveUploadToken(w, r)
	case r.Method == http.MethodPost && r.URL.Path == base+"/gameservers/file_server/mkdir":
		_ = r.ParseForm()
		s.mu.Lock()
		s.mkdirCalls = append(s.mkdirCalls, r.PostForm)
		st := s.mkdirStatus
		parent, name := r.PostForm.Get("path"), r.PostForm.Get("name")
		if st == 0 {
			if !s.dirs[parent] {
				st = 404
			} else {
				s.dirs[parent+"/"+name] = true
			}
		}
		s.mu.Unlock()
		if st != 0 {
			writeJSON(w, st, map[string]any{"status": "error", "message": "mkdir refused"})
			return
		}
		if s.afterMkdir != nil {
			s.afterMkdir(s)
		}
		writeJSON(w, 200, map[string]any{"status": "success"})
	default:
		writeJSON(w, 404, map[string]any{"status": "error", "message": "no route"})
	}
}

func (s *standIn) serveList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	s.mu.Lock()
	fail := s.failLists
	exists := s.dirs[dir]
	type entry struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	var entries []entry
	for p, b := range s.files {
		if path.Dir(p) == dir {
			entries = append(entries, entry{"file", p, path.Base(p), int64(len(b))})
		}
	}
	for d := range s.dirs {
		if path.Dir(d) == dir {
			entries = append(entries, entry{"dir", d, path.Base(d), 0})
		}
	}
	s.mu.Unlock()
	if fail {
		writeJSON(w, 500, map[string]any{"status": "error"})
		return
	}
	if !exists {
		writeJSON(w, 404, map[string]any{"status": "error", "message": "directory not found"})
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSON(w, 200, map[string]any{"data": map[string]any{"entries": entries}})
}

func (s *standIn) serveDownload(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("f")
	b, ok := s.file(p)
	if !ok {
		w.WriteHeader(404)
		return
	}
	if s.corruptRead && strings.HasSuffix(p, "/champion_shop_delivery.json") {
		b = []byte("{\n  \"Objects\": [1]\n}\n")
	}
	_, _ = w.Write(b)
}

func (s *standIn) serveUploadToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	s.uploadCalls = append(s.uploadCalls, r.PostForm)
	s.mu.Unlock()
	if s.beforeToken != nil {
		s.beforeToken(s)
	}
	if s.tokenStatus != 0 {
		writeJSON(w, s.tokenStatus, map[string]any{"status": "error", "message": "refused"})
		return
	}
	dir, name := r.PostForm.Get("path"), r.PostForm.Get("file")
	s.mu.Lock()
	s.tokenSeq++
	tok := fmt.Sprintf("upload-secret-token-%d", s.tokenSeq)
	s.tokens[tok] = dir + "/" + name
	s.mu.Unlock()
	u := s.srv.URL + "/fs/upload/secret-path-" + fmt.Sprint(s.tokenSeq)
	if s.insecureURL != "" {
		u = s.insecureURL
	}
	writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": u, "token": tok}}})
}

func (s *standIn) serveTransfer(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tok := r.Header.Get("token")
	s.mu.Lock()
	s.transfers = append(s.transfers, transfer{contentType: r.Header.Get("Content-Type"), body: body, token: tok})
	dest, ok := s.tokens[tok]
	delete(s.tokens, tok) // single use
	if s.expireTokens {
		ok = false
	}
	s.mu.Unlock()
	if s.dropBefore {
		hijackClose(w)
		return
	}
	if !ok {
		writeJSON(w, 401, map[string]any{"status": "error", "message": "invalid or expired token"})
		return
	}
	if s.transferStatus != 0 {
		writeJSON(w, s.transferStatus, map[string]any{"status": "error", "message": "refused"})
		return
	}
	s.mu.Lock()
	if !s.dirs[path.Dir(dest)] {
		s.mu.Unlock()
		writeJSON(w, 404, map[string]any{"status": "error", "message": "directory not found"})
		return
	}
	switch {
	case s.claimNoStore:
	case s.partialStore:
		s.files[dest] = body[:len(body)/2]
	default:
		s.files[dest] = body
	}
	s.mu.Unlock()
	if s.afterTransfer != nil {
		s.afterTransfer(s)
	}
	if s.dropAfter {
		hijackClose(w)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "success"})
}

func hijackClose(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("no hijacker")
	}
	c, _, err := hj.Hijack()
	if err == nil {
		c.Close()
	}
}
