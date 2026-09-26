// Package nitradofixture is a read-only stand-in for the Nitrado API that
// serves one synthetic DayZ service and a synthetic ADM log. It exists so an
// isolated staging deployment can run the real ADM pipeline (discovery, stat,
// delta reads, parsing, persistence, feeds) without any Nitrado credential and
// without the ability to change a real server.
//
// It answers only the read endpoints Champion uses. Every write (restart,
// stop, whitelist, ban list, file upload, settings - any non-GET under
// /services or /token) is refused with 403 and counted. Synthetic events are
// injected through the /_fixture control endpoints, which require the
// control token when one is configured.
package nitradofixture

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gamePath is the synthetic server's game_specific.path.
const gamePath = "/games/fixture/noftp/dayzps/"

// ConfigDir is where the synthetic ADM file lives.
const ConfigDir = gamePath + "config"

// Server is the fixture. The zero value is not usable; call New.
type Server struct {
	serviceID    int64
	slots        int
	controlToken string
	now          func() time.Time

	mu             sync.Mutex
	admName        string
	adm            bytes.Buffer
	modified       time.Time
	players        int
	seq            int
	started        bool
	refusedWrites  int
	requestsByPath map[string]int
}

// New returns a fixture for serviceID. controlToken, when non-empty, is
// required (X-Fixture-Token header) on every /_fixture control call.
func New(serviceID int64, controlToken string) *Server {
	s := &Server{serviceID: serviceID, slots: 18, controlToken: controlToken, now: time.Now, started: true, requestsByPath: map[string]int{}}
	s.rotateLocked()
	return s
}

// rotateLocked starts a new ADM file, as a server (re)start does.
func (s *Server) rotateLocked() {
	now := s.now().UTC()
	s.admName = "DayZServer_PS4_x64_" + now.Format("2006-01-02_15-04-05") + ".ADM"
	s.adm.Reset()
	fmt.Fprintf(&s.adm, "AdminLog started on %s at %s\n", now.Format("2006-01-02"), now.Format("15:04:05"))
	s.modified = now
}

func (s *Server) appendLocked(line string) {
	s.adm.WriteString(line)
	s.modified = s.now().UTC()
}

// AddKills appends n distinct PvP kill lines, oldest first, and returns the
// victim names in order.
func (s *Server) AddKills(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var victims []string
	for i := 0; i < n; i++ {
		s.seq++
		at := s.now().UTC().Format("15:04:05")
		v := fmt.Sprintf("FixtureVictim%04d", s.seq)
		k := fmt.Sprintf("FixtureKiller%d", s.seq%7)
		s.appendLocked(fmt.Sprintf(`%s | Player "%s" (DEAD) (id=fv%04d pos=<4500.0, 9800.0, 12.0>) killed by Player "%s" (id=fk%d pos=<4520.0, 9810.0, 12.0>) with M4-A1 from %d.0 meters`+"\n",
			at, v, s.seq, k, s.seq%7, 20+s.seq%80))
		victims = append(victims, v)
	}
	return victims
}

// AddDeaths appends n distinct environmental (non-PvP) death lines.
func (s *Server) AddDeaths(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for i := 0; i < n; i++ {
		s.seq++
		at := s.now().UTC().Format("15:04:05")
		p := fmt.Sprintf("FixtureStarved%04d", s.seq)
		s.appendLocked(fmt.Sprintf(`%s | Player "%s" (DEAD) (id=fs%04d pos=<7000.0, 1200.0, 8.0>) died. Stats> Water: 0 Energy: 0 Bleed sources: 0`+"\n", at, p, s.seq))
		names = append(names, p)
	}
	return names
}

// SetPlayers sets the live query's player_current.
func (s *Server) SetPlayers(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.players = n
}

// Restart begins a new ADM file and clears the player count.
func (s *Server) Restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.players = 0
	s.rotateLocked()
}

// State is the fixture's observable state (GET /_fixture/state).
type State struct {
	ServiceID     int64          `json:"service_id"`
	ADMPath       string         `json:"adm_path"`
	ADMBytes      int            `json:"adm_bytes"`
	Events        int            `json:"events"`
	Players       int            `json:"players"`
	RefusedWrites int            `json:"refused_writes"`
	Requests      map[string]int `json:"requests"`
}

// Snapshot returns the current State.
func (s *Server) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := make(map[string]int, len(s.requestsByPath))
	for k, v := range s.requestsByPath {
		req[k] = v
	}
	return State{ServiceID: s.serviceID, ADMPath: ConfigDir + "/" + s.admName, ADMBytes: s.adm.Len(), Events: s.seq,
		Players: s.players, RefusedWrites: s.refusedWrites, Requests: req}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/_fixture/") {
		s.control(w, r)
		return
	}
	if path == "/_files" {
		s.serveFile(w, r)
		return
	}
	s.mu.Lock()
	s.requestsByPath[r.Method+" "+routeLabel(path)]++
	if r.Method != http.MethodGet {
		s.refusedWrites++
		s.mu.Unlock()
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": "nitrado fixture is read-only"})
		return
	}
	s.mu.Unlock()

	id := strconv.FormatInt(s.serviceID, 10)
	switch path {
	case "/token":
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"id": 1, "scopes": []string{"service"}}}})
	case "/services":
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"services": []any{s.serviceJSON()}}})
	case "/services/" + id:
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"service": s.serviceJSON()}})
	case "/services/" + id + "/gameservers":
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"gameserver": s.gameserverJSON()}})
	case "/services/" + id + "/gameservers/file_server/list":
		s.list(w, r.URL.Query().Get("dir"))
	case "/services/" + id + "/gameservers/file_server/download":
		file := r.URL.Query().Get("file")
		if !s.isADM(file) {
			writeJSON(w, 404, map[string]string{"status": "error", "message": "file not found"})
			return
		}
		signed := baseURL(r) + "/_files?" + url.Values{"file": {file}}.Encode()
		writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"token": map[string]any{"url": signed}}})
	default:
		// Includes file_server/seek: absent here, as on accounts without it;
		// the client falls back to its validated Range read.
		writeJSON(w, 404, map[string]string{"status": "error", "message": "not found"})
	}
}

// routeLabel collapses query-free paths for the request counter.
func routeLabel(p string) string {
	if len(p) > 80 {
		return p[:80]
	}
	return p
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) serviceJSON() map[string]any {
	return map[string]any{"id": s.serviceID, "type": "gameserver", "status": "active", "is_owner": true, "readonly": true,
		"username": "champion-staging-fixture", "location": "fixture", "roles": []string{"ROLE_WEBINTERFACE_FILEBROWSER_READ"},
		"details": map[string]any{"game": "DayZ (PS4)", "name": "Champion staging fixture"}}
}

func (s *Server) gameserverJSON() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "started"
	if !s.started {
		status = "stopped"
	}
	return map[string]any{"service_id": s.serviceID, "game": "dayzps", "game_human": "DayZ (PS4)", "status": status, "type": "gameserver", "slots": s.slots,
		"game_specific": map[string]any{"path": gamePath, "path_available": true, "log_files": []string{}, "features": map[string]any{"has_file_browser": true}},
		"query":         map[string]any{"server_name": "Champion staging fixture", "map": "chernarusplus", "version": "fixture", "player_current": s.players, "player_max": s.slots}}
}

func (s *Server) isADM(file string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return file == ConfigDir+"/"+s.admName
}

func (s *Server) list(w http.ResponseWriter, dir string) {
	dir = strings.TrimRight(dir, "/")
	entries := []any{}
	s.mu.Lock()
	switch dir {
	case "", "/games", "/games/fixture", "/games/fixture/noftp", "/games/fixture/noftp/dayzps":
		// Only the documented path is exposed; the tree walk from "/" finds
		// nothing and discovery uses game_specific.path, as on a noftp mount.
	case ConfigDir:
		entries = append(entries, map[string]any{"type": "file", "name": s.admName, "path": ConfigDir + "/" + s.admName,
			"size": s.adm.Len(), "modified_at": s.modified.Unix()})
	default:
		s.mu.Unlock()
		writeJSON(w, 404, map[string]string{"status": "error", "message": "directory not found"})
		return
	}
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "success", "data": map[string]any{"entries": entries}})
}

// serveFile is the "signed URL" target. http.ServeContent answers Range
// requests with 206 + Content-Range and ignores offset/count, like Nitrado.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusForbidden, map[string]string{"status": "error", "message": "nitrado fixture is read-only"})
		return
	}
	if !s.isADM(r.URL.Query().Get("file")) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	body := append([]byte(nil), s.adm.Bytes()...)
	mod := s.modified
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", mod, bytes.NewReader(body))
}

// control handles /_fixture/{kills,deaths,players,restart,state}.
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	if s.controlToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Fixture-Token")), []byte(s.controlToken)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "fixture control token required"})
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/_fixture/")
	if action == "state" {
		writeJSON(w, 200, s.Snapshot())
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	switch action {
	case "kills":
		if n <= 0 || n > 1000 {
			writeJSON(w, 400, map[string]string{"error": "n must be 1..1000"})
			return
		}
		writeJSON(w, 200, map[string]any{"added": s.AddKills(n)})
	case "deaths":
		if n <= 0 || n > 1000 {
			writeJSON(w, 400, map[string]string{"error": "n must be 1..1000"})
			return
		}
		writeJSON(w, 200, map[string]any{"added": s.AddDeaths(n)})
	case "players":
		if n < 0 || n > s.slots {
			writeJSON(w, 400, map[string]string{"error": "n out of range"})
			return
		}
		s.SetPlayers(n)
		writeJSON(w, 200, map[string]any{"players": n})
	case "restart":
		s.Restart()
		writeJSON(w, 200, s.Snapshot())
	default:
		writeJSON(w, 404, map[string]string{"error": "unknown fixture action"})
	}
}
