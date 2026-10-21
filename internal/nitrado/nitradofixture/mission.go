package nitradofixture

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// Mission folder support, opt-in. The fixture stays read-only unless a test turns on
// EnableMission (a synthetic mission folder with files to list and download) and
// AllowMissionWrites (the file-server upload token, transfer and mkdir calls, accepted only
// inside that folder). The staging deployment (cmd/nitrado-fixture) never turns either on.
//
// It exists for the guarded mission-file writes (the Stadium, docs/STADIUM.md): the real
// nitrado.Client runs its real discovery, listing, download, upload and mkdir code against it.

// MissionDir is the synthetic mission folder: the noftp mount of the service's game, as
// maprotation.Locate and capability.Discover find it.
const MissionDir = "/games/fixture/noftp/dayzps_missions/dayzOffline.chernarusplus"

// Transfer modes for TransferMode.
const (
	TransferStore   = ""        // store the bytes and answer 200
	TransferRefuse  = "refuse"  // answer 500 and store nothing
	TransferClaim   = "claim"   // answer 200 and store nothing
	TransferPartial = "partial" // store half the bytes and answer 200
)

type mission struct {
	enabled     bool
	writes      bool
	files       map[string][]byte // mission-relative path -> content
	dirs        map[string]bool   // mission-relative directories ("" is the folder itself)
	tokens      map[string]string // upload token -> mission-relative destination
	tokenSeq    int
	transfers   int
	modes       map[int]string // transfer number (1-based) -> mode
	log         []string
	onDownload  func(rel string)
	mkdirStatus int
}

// SetClock replaces the fixture's clock (boot names and timestamps), for tests that need a
// restart to produce a new ADM file name within the same second.
func (s *Server) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// EnableMission creates the mission folder with cfggameplay.json as given (nil: no such file).
func (s *Server) EnableMission(gameplay []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mission = mission{enabled: true, files: map[string][]byte{}, dirs: map[string]bool{"": true}, tokens: map[string]string{}, modes: map[int]string{}}
	if gameplay != nil {
		s.mission.files["cfggameplay.json"] = append([]byte(nil), gameplay...)
	}
}

// AllowMissionWrites turns the write endpoints on or off (inside the mission folder only).
func (s *Server) AllowMissionWrites(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mission.writes = on
}

// SetMissionFile stores a file at a mission-relative path, creating its folders.
func (s *Server) SetMissionFile(rel string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setFileLocked(rel, content)
}

func (s *Server) setFileLocked(rel string, content []byte) {
	s.mission.files[rel] = append([]byte(nil), content...)
	for d := path.Dir(rel); d != "." && d != "/"; d = path.Dir(d) {
		s.mission.dirs[d] = true
	}
}

// AddMissionDir creates an empty folder (mission-relative, e.g. "custom").
func (s *Server) AddMissionDir(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for d := rel; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		s.mission.dirs[d] = true
	}
}

// RemoveMissionFile deletes a file.
func (s *Server) RemoveMissionFile(rel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mission.files, rel)
}

// MissionFile returns a file's content.
func (s *Server) MissionFile(rel string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.mission.files[rel]
	return append([]byte(nil), b...), ok
}

// MissionDirExists reports whether a mission-relative folder exists.
func (s *Server) MissionDirExists(rel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mission.dirs[rel]
}

// MissionWrites lists every accepted write so far, in order: "upload <rel>", "mkdir <rel>".
func (s *Server) MissionWrites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mission.log...)
}

// Transfers is how many upload transfers were received so far.
func (s *Server) Transfers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mission.transfers
}

// TransferMode makes the n-th upload transfer (1-based) behave as mode.
func (s *Server) TransferMode(n int, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mission.modes[n] = mode
}

// MkdirStatus makes every mkdir answer status (non-zero) without creating anything.
func (s *Server) MkdirStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mission.mkdirStatus = status
}

// OnDownload runs fn (with the mission-relative path) before a mission file is served, so a
// test can change the server between two reads.
func (s *Server) OnDownload(fn func(rel string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mission.onDownload = fn
}

// missionRel maps an absolute file-server path to a mission-relative one ("" for the folder).
func missionRel(p string) (string, bool) {
	p = strings.TrimRight(p, "/")
	if p == MissionDir {
		return "", true
	}
	if strings.HasPrefix(p, MissionDir+"/") {
		return strings.TrimPrefix(p, MissionDir+"/"), true
	}
	return "", false
}

// missionList answers a listing of a mission folder; ok is false when the path is not a mission
// folder (the caller then answers as before).
func (s *Server) missionList(w http.ResponseWriter, dir string) bool {
	rel, ok := missionRel(dir)
	if !ok {
		return false
	}
	s.mu.Lock()
	if !s.mission.enabled || !s.mission.dirs[rel] {
		s.mu.Unlock()
		writeJSON(w, 404, map[string]string{"status": "error", "message": "directory not found"})
		return true
	}
	type entry struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	var entries []entry
	for p, b := range s.mission.files {
		if parentOf(p) == rel {
			entries = append(entries, entry{"file", path.Base(p), MissionDir + "/" + p, len(b)})
		}
	}
	for d := range s.mission.dirs {
		if d != "" && parentOf(d) == rel {
			entries = append(entries, entry{"dir", path.Base(d), MissionDir + "/" + d, 0})
		}
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if entries == nil {
		entries = []entry{}
	}
	writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"entries": entries}})
	return true
}

func parentOf(rel string) string {
	d := path.Dir(rel)
	if d == "." {
		return ""
	}
	return d
}

// missionDownload answers the signed-URL step for a mission file; ok is false when the path is
// not a mission file.
func (s *Server) missionDownload(w http.ResponseWriter, r *http.Request, file string) bool {
	rel, ok := missionRel(file)
	if !ok {
		return false
	}
	s.mu.Lock()
	_, exists := s.mission.files[rel]
	enabled := s.mission.enabled
	s.mu.Unlock()
	if !enabled || !exists {
		writeJSON(w, 404, map[string]string{"status": "error", "message": "file not found"})
		return true
	}
	signed := baseURL(r) + "/_files?" + url.Values{"file": {file}}.Encode()
	writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": signed}}})
	return true
}

// missionServe serves a mission file's bytes (the /_files target); ok is false otherwise.
func (s *Server) missionServe(w http.ResponseWriter, file string) bool {
	rel, ok := missionRel(file)
	if !ok {
		return false
	}
	s.mu.Lock()
	hook := s.mission.onDownload
	s.mu.Unlock()
	if hook != nil {
		hook(rel)
	}
	s.mu.Lock()
	b, exists := s.mission.files[rel]
	body := append([]byte(nil), b...)
	s.mu.Unlock()
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return true
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
	return true
}

// missionWrite handles the three write endpoints when writes are allowed; ok is false when the
// request is not one of them (the caller then refuses it as before).
func (s *Server) missionWrite(w http.ResponseWriter, r *http.Request) bool {
	id := fmt.Sprint(s.serviceID)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/services/"+id+"/gameservers/file_server/upload":
		_ = r.ParseForm()
		rel, ok := missionRel(r.PostForm.Get("path"))
		name := r.PostForm.Get("file")
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.mission.enabled || !s.mission.writes || !ok || !s.mission.dirs[rel] || name == "" || strings.ContainsAny(name, "/\\") {
			s.refusedWrites++
			writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": "nitrado fixture refuses this write"})
			return true
		}
		dest := name
		if rel != "" {
			dest = rel + "/" + name
		}
		s.mission.tokenSeq++
		tok := fmt.Sprintf("fixture-upload-token-%d", s.mission.tokenSeq)
		s.mission.tokens[tok] = dest
		s.mission.log = append(s.mission.log, "upload "+dest)
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": baseURL(r) + "/_upload/" + fmt.Sprint(s.mission.tokenSeq), "token": tok}}})
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/services/"+id+"/gameservers/file_server/mkdir":
		_ = r.ParseForm()
		rel, ok := missionRel(r.PostForm.Get("path"))
		name := r.PostForm.Get("name")
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.mission.enabled || !s.mission.writes || !ok || !s.mission.dirs[rel] || name == "" || strings.ContainsAny(name, "/\\") {
			s.refusedWrites++
			writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": "nitrado fixture refuses this write"})
			return true
		}
		if s.mission.mkdirStatus != 0 {
			writeJSON(w, s.mission.mkdirStatus, map[string]string{"status": "error", "message": "mkdir failed"})
			return true
		}
		dir := name
		if rel != "" {
			dir = rel + "/" + name
		}
		s.mission.dirs[dir] = true
		s.mission.log = append(s.mission.log, "mkdir "+dir)
		writeJSON(w, 200, map[string]any{"status": "success"})
		return true
	}
	return false
}

// missionTransfer is the upload target (/_upload/<n>): the single-use token names the file.
func (s *Server) missionTransfer(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	tok := r.Header.Get("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	dest, ok := s.mission.tokens[tok]
	delete(s.mission.tokens, tok)
	if r.Method != http.MethodPost || !ok || !s.mission.writes {
		s.refusedWrites++
		writeJSON(w, http.StatusUnauthorized, map[string]string{"status": "error", "message": "bad upload token"})
		return
	}
	s.mission.transfers++
	switch s.mission.modes[s.mission.transfers] {
	case TransferRefuse:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "error", "message": "refused"})
	case TransferClaim:
		writeJSON(w, 200, map[string]string{"status": "success"})
	case TransferPartial:
		s.setFileLocked(dest, body[:len(body)/2])
		writeJSON(w, 200, map[string]string{"status": "success"})
	default:
		s.setFileLocked(dest, body)
		writeJSON(w, 200, map[string]string{"status": "success"})
	}
}
