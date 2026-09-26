package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Local fixtures only (VERIFIED IN TEST): nothing here reaches a live Nitrado service.

const (
	root       = "/games/ni0000001_1"
	missionDir = root + "/ftproot/dayzps_missions/dayzOffline.chernarusplus"
)

var goodBinding = Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: "19806451"}

type fakeReader struct {
	svc      nitrado.ServiceFacts
	gs       nitrado.GameserverFacts
	tok      nitrado.TokenFacts
	svcErr   error
	dirs     map[string][]nitrado.LogFile
	files    map[string][]byte
	listErr  map[string]error
	readErr  map[string]error
	mu       sync.Mutex
	reads    []string
	listings []string
}

func (f *fakeReader) ServiceFacts(context.Context, string) (nitrado.ServiceFacts, error) {
	return f.svc, f.svcErr
}
func (f *fakeReader) GameserverFacts(context.Context, string) (nitrado.GameserverFacts, error) {
	return f.gs, nil
}
func (f *fakeReader) TokenFacts(context.Context) (nitrado.TokenFacts, error) { return f.tok, nil }
func (f *fakeReader) ListDir(_ context.Context, _ string, dir string) ([]nitrado.LogFile, error) {
	f.mu.Lock()
	f.listings = append(f.listings, dir)
	f.mu.Unlock()
	if err := f.listErr[dir]; err != nil {
		return nil, err
	}
	return f.dirs[dir], nil
}
func (f *fakeReader) ReadLog(_ context.Context, _ string, p string) ([]byte, error) {
	f.mu.Lock()
	f.reads = append(f.reads, p)
	f.mu.Unlock()
	if err := f.readErr[p]; err != nil {
		return nil, err
	}
	b, ok := f.files[p]
	if !ok {
		return nil, &nitrado.RequestError{Kind: nitrado.KindNotFound, StatusCode: 404}
	}
	return b, nil
}

func entry(dir, name string) nitrado.LogFile {
	return nitrado.LogFile{Name: name, Path: dir + "/" + name, Directory: dir}
}

// consoleFixture mirrors the console layout shape (dayzps, ftproot/dayzps_missions/<mission>).
func consoleFixture(gameplay string) *fakeReader {
	return &fakeReader{
		svc: nitrado.ServiceFacts{ID: 19806451, Type: "gameserver", Status: "active", IsOwner: true, Game: "dayzps",
			Roles: []string{"ROLE_WEBINTERFACE_FILEBROWSER_READ", "ROLE_WEBINTERFACE_FILEBROWSER_WRITE", "ROLE_WEBINTERFACE_GENERAL_CONTROL"}},
		gs: nitrado.GameserverFacts{ServiceID: 19806451, Game: "dayzps", GameHuman: "DayZ (PS4)", Status: "started", Slots: 18,
			GamePath: root + "/noftp/dayzps/", HasFileBrowser: true, EnableCfgGameplayFile: "1", Mission: "dayzOffline.chernarusplus"},
		tok: nitrado.TokenFacts{Scopes: []string{"service"}},
		dirs: map[string][]nitrado.LogFile{
			missionDir:             {entry(missionDir, "cfggameplay.json"), entry(missionDir, "init.c"), entry(missionDir, "db")},
			missionDir + "/custom": {entry(missionDir+"/custom", "base.json")},
			root + "/noftp/dayzps": {entry(root+"/noftp/dayzps", "serverDZ.cfg")},
		},
		files: map[string][]byte{
			missionDir + "/cfggameplay.json":    []byte(gameplay),
			root + "/noftp/dayzps/serverDZ.cfg": []byte("hostname = \"Secret Host\";\npassword = \"hunter2-SERVER\";\npasswordAdmin = \"hunter2-ADMIN\";\nenableCfgGameplayFile = 1;\nclass Missions\n{\n  class DayZ\n  {\n    template = \"dayzOffline.chernarusplus\";\n  };\n};\n"),
		},
	}
}

const gameplayWithSpawners = `{"version":123,"WorldsData":{"objectSpawnersArr":["custom/base.json","custom/missing.json"]}}`

func find(t *testing.T, r Report, key string) Finding {
	t.Helper()
	f, ok := r.Get(key)
	if !ok {
		t.Fatalf("missing finding %s in %+v", key, r.Findings)
	}
	return f
}

func TestDiscoverConsoleLayout(t *testing.T) {
	rd := consoleFixture(gameplayWithSpawners)
	rep, err := Discover(context.Background(), rd, goodBinding)
	if err != nil {
		t.Fatal(err)
	}
	if rep.MissionPath != "dayzps_missions/dayzOffline.chernarusplus" || rep.MissionDir != missionDir {
		t.Fatalf("mission path: %q %q", rep.MissionPath, rep.MissionDir)
	}
	if f := find(t, rep, "gameserver_platform"); f.Value != "dayzps / PlayStation" {
		t.Fatalf("platform: %+v", f)
	}
	if f := find(t, rep, "filebrowser_write_role"); f.Class != Documentation {
		t.Fatalf("a listed write role is never classed as a verified capability: %+v", f)
	}
	if f := find(t, rep, "objectSpawnersArr"); f.Value != "2" || f.Class != VerifiedLive {
		t.Fatalf("spawners: %+v", f)
	}
	if f := find(t, rep, "spawner_files_exist"); f.Value != "1/2" || !strings.Contains(f.Evidence, "custom/missing.json") {
		t.Fatalf("missing spawner file reported: %+v", f)
	}
	if f := find(t, rep, "champion_referenced"); f.Value != "false" {
		t.Fatalf("champion reference: %+v", f)
	}
	if f := find(t, rep, "serverdz_cfg"); f.Class != VerifiedLive || !strings.Contains(f.Evidence, `enableCfgGameplayFile="1"`) {
		t.Fatalf("serverDZ.cfg: %+v", f)
	}
	// Credential redaction: nothing in the report carries a secret from serverDZ.cfg.
	raw, _ := json.Marshal(rep)
	for _, secret := range []string{"hunter2", "Secret Host", "ni0000001_1"} {
		if strings.Contains(string(raw), secret) && secret != "ni0000001_1" {
			t.Fatalf("report leaks %q: %s", secret, raw)
		}
	}
	for _, f := range rep.Findings {
		if strings.Contains(f.Evidence, "ni0000001") || strings.Contains(f.Value, "ni0000001") {
			t.Fatalf("findings carry the account path, not only canonical paths: %+v", f)
		}
	}
}

func TestDiscoverRefusesAForeignOrMalformedBinding(t *testing.T) {
	rd := consoleFixture(gameplayWithSpawners)
	for name, b := range map[string]Binding{
		"another service":     {OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: "123"},
		"no organization":     {InstallationID: 11, GameServerID: 1, NitradoServiceID: "19806451"},
		"no installation":     {OrganizationID: 1, GameServerID: 1, NitradoServiceID: "19806451"},
		"non-numeric service": {OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: "19806451/../x"},
	} {
		if _, err := Discover(context.Background(), rd, b); !errors.Is(err, ErrBinding) {
			t.Errorf("%s: want ErrBinding, got %v", name, err)
		}
	}
	// The gameserver details must be for the same service too.
	rd.gs.ServiceID = 999
	if _, err := Discover(context.Background(), rd, goodBinding); !errors.Is(err, ErrBinding) {
		t.Fatalf("gameserver of another service: %v", err)
	}
}

func TestSafePathAndFileRoot(t *testing.T) {
	if FileRoot("/games/ni0000001_1/noftp/dayzps/") != root || FileRoot("/etc/passwd") != "" || FileRoot("/games/../etc") != "" {
		t.Fatal("file root derivation")
	}
	for _, bad := range []string{"/etc/passwd", root + "/../other/x", root + "_evil/x", root + "\\x", "/games/other_2/x", root + "/a\x00b"} {
		if _, err := SafePath(root, bad); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q must be refused", bad)
		}
	}
	if p, err := SafePath(root, root+"/ftproot//x/./y.json"); err != nil || p != root+"/ftproot/x/y.json" {
		t.Fatalf("clean path: %q %v", p, err)
	}
	if Canonical(root+"/noftp/dayzps/config/a.ADM") != Canonical(root+"/ftproot/dayzps/config/a.ADM") {
		t.Fatal("ftproot/noftp aliases are one location")
	}
}

func TestMissingAndInvalidConfiguration(t *testing.T) {
	// No mission folder listed.
	rd := consoleFixture(gameplayWithSpawners)
	delete(rd.dirs, missionDir)
	rep, _ := Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "mission_dir"); f.Class != Unverified {
		t.Fatalf("missing mission folder: %+v", f)
	}
	// No cfggameplay.json.
	rd = consoleFixture(gameplayWithSpawners)
	rd.dirs[missionDir] = []nitrado.LogFile{entry(missionDir, "init.c")}
	rep, _ = Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "cfggameplay"); f.Class != Unverified || f.Value != "absent" {
		t.Fatalf("missing cfggameplay: %+v", f)
	}
	// Invalid JSON.
	rep, _ = Discover(context.Background(), consoleFixture(`{"WorldsData":`), goodBinding)
	if f := find(t, rep, "cfggameplay"); !strings.Contains(f.Value, "INVALID") {
		t.Fatalf("invalid cfggameplay: %+v", f)
	}
	// No objectSpawnersArr key; unsafe spawner entries are flagged.
	gp := ParseCfgGameplay([]byte(`{"version":1,"WorldsData":{"objectSpawnersArr":["../../etc/x.json","ok/a.json"]}}`))
	if len(gp.InvalidSpawnPath) != 1 || gp.InvalidSpawnPath[0] != "../../etc/x.json" {
		t.Fatalf("unsafe spawner path: %+v", gp)
	}
	if gp := ParseCfgGameplay([]byte(`{"version":1}`)); gp.HasSpawnersKey || !gp.ValidJSON {
		t.Fatalf("no spawner key: %+v", gp)
	}
	// Mission setting that is not a plain name is never used as a path.
	rd = consoleFixture(gameplayWithSpawners)
	rd.gs.Mission = "../../other"
	rep, _ = Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "mission_dir"); f.Class != Unverified {
		t.Fatalf("unsafe mission name: %+v", f)
	}
	for _, d := range rd.listings {
		if strings.Contains(d, "..") {
			t.Fatalf("an unsafe path was listed: %s", d)
		}
	}
}

func TestUnsupportedCapabilitiesAndFailures(t *testing.T) {
	rd := consoleFixture(gameplayWithSpawners)
	rd.gs.HasFileBrowser = false
	rd.svc.Roles = []string{"ROLE_WEBINTERFACE_GENERAL_CONTROL"}
	rep, _ := Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "file_browser"); f.Class != Unsupported {
		t.Fatalf("no file browser: %+v", f)
	}
	if f := find(t, rep, "filebrowser_write_role"); f.Class != Unverified || f.Value != "false" {
		t.Fatalf("no write role: %+v", f)
	}
	// Listing failure of the mission folder on both mounts.
	rd = consoleFixture(gameplayWithSpawners)
	rd.listErr = map[string]error{missionDir: &nitrado.RequestError{Kind: nitrado.KindTemporary, StatusCode: 503}}
	rep, _ = Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "mission_dir"); f.Class != Unverified {
		t.Fatalf("listing failure: %+v", f)
	}
	// Download failure of cfggameplay.json.
	rd = consoleFixture(gameplayWithSpawners)
	rd.readErr = map[string]error{missionDir + "/cfggameplay.json": &nitrado.RequestError{Kind: nitrado.KindTemporary, StatusCode: 502}}
	rep, _ = Discover(context.Background(), rd, goodBinding)
	if f := find(t, rep, "cfggameplay"); f.Class != Unverified || !strings.Contains(f.Evidence, "nitrado_temporary_502") {
		t.Fatalf("download failure: %+v", f)
	}
	// Service details failure stops cleanly.
	rd = consoleFixture(gameplayWithSpawners)
	rd.svcErr = errors.New("boom")
	rep, err := Discover(context.Background(), rd, goodBinding)
	if err != nil || find(t, rep, "service_identity").Class != Unverified {
		t.Fatalf("service failure: %+v %v", rep, err)
	}
}

func TestServerCfgKeepsOnlyWhitelistedKeys(t *testing.T) {
	raw := []byte("hostname = \"H\";\npassword = \"pw1\";\npasswordAdmin = \"pw2\";\nenableCfgGameplayFile = 0;\nclass Missions { class DayZ { template = \"dayzOffline.enoch\"; }; };\n")
	f := ParseServerCfg(raw)
	if f.EnableCfgGameplayFile != "0" || f.MissionTemplate != "dayzOffline.enoch" {
		t.Fatalf("keys: %+v", f)
	}
	b, _ := json.Marshal(f)
	if strings.Contains(string(b), "pw") || strings.Contains(string(b), `"H"`) {
		t.Fatalf("secrets kept: %s", b)
	}
	if g := ParseServerCfg([]byte("password = \"x\";")); g.EnableCfgGameplayFile != "" || g.MissionTemplate != "" {
		t.Fatalf("absent keys stay absent: %+v", g)
	}
}

// Read-only enforcement: the Reader surface has read methods only, and Discover through the REAL
// Nitrado client issues only GET requests, never to a write, restart or stop endpoint.
func TestReaderIsReadOnly(t *testing.T) {
	allowed := map[string]bool{"ServiceFacts": true, "GameserverFacts": true, "TokenFacts": true, "ListDir": true, "ReadLog": true}
	rt := reflect.TypeOf((*Reader)(nil)).Elem()
	for i := 0; i < rt.NumMethod(); i++ {
		if !allowed[rt.Method(i).Name] {
			t.Fatalf("Reader exposes a non-read method %s", rt.Method(i).Name)
		}
	}
	// internal/nitrado's only file-write calls are the three reviewed Gate A primitives
	// (docs/SHOP_GATE_A_UPLOAD.md). They are not part of Reader, and only internal/shop/missionwrite
	// may call them (missionwrite.TestWriteCapabilityIsIsolated). Any other write method fails here.
	reviewedWrites := map[string]bool{"RequestUploadToken": true, "PostUpload": true, "Mkdir": true}
	ct := reflect.TypeOf(&nitrado.Client{})
	for i := 0; i < ct.NumMethod(); i++ {
		// (Non-file control calls such as ban-list edits and restarts exist for Client Admin; they are
		// simply not part of Reader. The guard here is about the FILE SERVER.)
		if reviewedWrites[ct.Method(i).Name] {
			continue
		}
		name := strings.ToLower(ct.Method(i).Name)
		fileish := strings.Contains(name, "file") || strings.Contains(name, "dir") || strings.Contains(name, "mission")
		if strings.Contains(name, "upload") {
			t.Fatalf("nitrado.Client gained an upload method: %s", ct.Method(i).Name)
		}
		for _, w := range []string{"write", "delete", "remove", "move", "copy", "mkdir", "rename", "put", "save"} {
			if fileish && strings.Contains(name, w) {
				t.Fatalf("nitrado.Client gained a file-write method: %s", ct.Method(i).Name)
			}
		}
	}

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/services/19806451/gameservers":
			// A realistic body: secrets are present and must never come out.
			fmt.Fprintf(w, `{"status":"success","data":{"gameserver":{"service_id":19806451,"game":"dayzps","status":"started","credentials":{"ftp":{"hostname":"ftp.host","username":"ni0000001_1","password":"FTP-SECRET"},"mysql":{"password":"SQL-SECRET"}},"websocket_token":"WS-SECRET","settings":{"config":{"enableCfgGameplayFile":"1","mission":"dayzOffline.chernarusplus","password":"SERVER-SECRET"},"general":{"admin-password":"ADMIN-SECRET","rcon-password":"RCON-SECRET","expertMode":"false"}},"game_specific":{"path":"%s/noftp/dayzps/","features":{"has_file_browser":true}}}}}`, root)
		case r.URL.Path == "/services/19806451":
			fmt.Fprint(w, `{"status":"success","data":{"service":{"id":19806451,"type":"gameserver","status":"active","roles":["ROLE_WEBINTERFACE_FILEBROWSER_READ"],"websocket_token":"WS2-SECRET","details":{"game":"dayzps"}}}}`)
		case r.URL.Path == "/token":
			fmt.Fprint(w, `{"status":"success","data":{"token":{"id":5,"scopes":["service"],"user":{"id":1,"username":"owner-name"}}}}`)
		case strings.HasSuffix(r.URL.Path, "/file_server/list"):
			dir := r.URL.Query().Get("dir")
			if dir == missionDir {
				fmt.Fprintf(w, `{"status":"success","data":{"entries":[{"type":"file","path":"%s/cfggameplay.json","name":"cfggameplay.json","size":10,"modified_at":1}]}}`, missionDir)
				return
			}
			fmt.Fprint(w, `{"status":"success","data":{"entries":[]}}`)
		case strings.HasSuffix(r.URL.Path, "/file_server/download"):
			fmt.Fprintf(w, `{"status":"success","data":{"token":{"url":"http://%s/signed/%s"}}}`, r.Host, path.Base(r.URL.Query().Get("file")))
		case strings.HasPrefix(r.URL.Path, "/signed/"):
			fmt.Fprint(w, gameplayWithSpawners)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := nitrado.NewClient(srv.URL, "TOKEN-SECRET", srv.Client())
	rep, err := Discover(context.Background(), client, goodBinding)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("a non-GET request was made: %s", s)
		}
		for _, bad := range []string{"upload", "delete", "restart", "stop", "move", "copy"} {
			if strings.Contains(strings.ToLower(s), bad) {
				t.Fatalf("a write/control endpoint was called: %s", s)
			}
		}
	}
	gs, _ := client.GameserverFacts(context.Background(), "19806451")
	svc, _ := client.ServiceFacts(context.Background(), "19806451")
	tok, _ := client.TokenFacts(context.Background())
	all, _ := json.Marshal([]any{rep, gs, svc, tok})
	for _, secret := range []string{"FTP-SECRET", "SQL-SECRET", "WS-SECRET", "WS2-SECRET", "SERVER-SECRET", "ADMIN-SECRET", "RCON-SECRET", "TOKEN-SECRET", "owner-name", "ftp.host"} {
		if strings.Contains(string(all), secret) {
			t.Fatalf("secret %s reached discovery output", secret)
		}
	}
	if gs.EnableCfgGameplayFile != "1" || gs.Mission != "dayzOffline.chernarusplus" || !gs.HasFileBrowser || svc.Game != "dayzps" || len(tok.Scopes) != 1 {
		t.Fatalf("whitelisted facts decoded: %+v %+v %+v", gs, svc, tok)
	}
}
