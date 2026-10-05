//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/maprotation/charwipe"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Map rotation with a player vote over a real PostgreSQL (docs/MAP_ROTATION.md): the API contract
// and the worker from the first boot to a changed map. Nitrado and Discord are in-memory fakes.

const (
	mrRoot    = "/games/ni1_1"
	mrMission = mrRoot + "/noftp/dayzps_missions/dayzOffline.chernarusplus"
	mrCustom  = mrMission + "/custom"
	mrSpawnsA = "<playerspawnpoints><fresh><generator_posbubbles><pos x=\"100\" z=\"100\"/></generator_posbubbles></fresh></playerspawnpoints>"
	mrSpawnsB = "<playerspawnpoints><fresh><generator_posbubbles><pos x=\"200\" z=\"200\"/></generator_posbubbles></fresh></playerspawnpoints>"
	mrSpawnsC = "<playerspawnpoints><fresh><generator_posbubbles><pos x=\"300\" z=\"300\"/></generator_posbubbles></fresh></playerspawnpoints>"
)

// fakeMapRemote is an in-memory Nitrado file server implementing mapRotationRemote. The custom
// folder holds only the map files: spawn files are uploaded on the website and stored in Champion.
type fakeMapRemote struct {
	mu          sync.Mutex
	files       map[string][]byte
	tasks       []nitrado.ScheduledTask
	pending     string // destination of the last upload token
	uploads     []string
	reads       []string // every file downloaded, in order
	refuseAt    int      // >0: exactly the n-th upload is refused
	uploadCount int
	serverCalls []string // stop, restart and delete calls, in order
	status      string   // Nitrado's word for the server ("" = started)
	restartDead bool     // Restart is accepted and the server stays stopped
}

func newFakeMapRemote() *fakeMapRemote {
	return &fakeMapRemote{files: map[string][]byte{
		mrMission + "/cfggameplay.json":         []byte("{\n\t\"version\": 1,\n\t\"WorldsData\": {\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/shop.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"),
		mrMission + "/cfgplayerspawnpoints.xml": []byte(mrSpawnsA),
		mrCustom + "/arena_a.json":              []byte(`{"Objects":[]}`),
		mrCustom + "/arena_b.json":              []byte(`{"Objects":[]}`),
		mrCustom + "/arena_c.json":              []byte(`{"Objects":[]}`),
	}}
}

func (f *fakeMapRemote) GameserverFacts(context.Context, string) (nitrado.GameserverFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.status
	if status == "" {
		status = "started"
	}
	return nitrado.GameserverFacts{Game: "dayzps", Status: status, GamePath: mrRoot + "/noftp/dayzps", Mission: "dayzOffline.chernarusplus"}, nil
}

func (f *fakeMapRemote) ListEntries(_ context.Context, _, dir string) ([]nitrado.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nitrado.DirEntry
	for p, b := range f.files {
		if path.Dir(p) == dir {
			out = append(out, nitrado.DirEntry{Name: path.Base(p), Path: p, Size: int64(len(b))})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("directory not found")
	}
	return out, nil
}

func (f *fakeMapRemote) ReadLog(_ context.Context, _, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, p)
	b, ok := f.files[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte{}, b...), nil
}

func (f *fakeMapRemote) ListScheduledTasks(context.Context, string) ([]nitrado.ScheduledTask, error) {
	return f.tasks, nil
}

func (f *fakeMapRemote) RequestUploadToken(_ context.Context, _, dir, name string) (nitrado.UploadTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = dir + "/" + name
	f.uploads = append(f.uploads, f.pending)
	return nitrado.UploadTarget{}, nil
}

func (f *fakeMapRemote) PostUpload(_ context.Context, _ nitrado.UploadTarget, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploadCount++
	if f.refuseAt > 0 && f.uploadCount == f.refuseAt {
		return errors.New("refused")
	}
	f.files[f.pending] = append([]byte{}, data...)
	return nil
}

// Stop, Restart and DeleteFile are what clearing the saved characters calls ("fresh characters on
// every map switch"). A stop and a start take effect at once.
func (f *fakeMapRemote) Stop(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverCalls = append(f.serverCalls, "stop")
	f.status = "stopped"
	return nil
}

func (f *fakeMapRemote) Restart(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverCalls = append(f.serverCalls, "restart")
	if !f.restartDead {
		f.status = "restarting"
	}
	return nil
}

func (f *fakeMapRemote) DeleteFile(_ context.Context, _, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverCalls = append(f.serverCalls, "delete "+p)
	delete(f.files, p)
	return nil
}

func (f *fakeMapRemote) content(p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.files[p])
}

type mapRotationWorld struct {
	*liveMapWorld
	remote  *fakeMapRemote
	posts   []*discordgo.MessageSend
	postsTo []string
	alerts  []discord.AdminAlert
	target  repository.MapRotationTarget
}

func newMapRotationWorld(t *testing.T) *mapRotationWorld {
	lw := newLiveMapWorld(t)
	w := &mapRotationWorld{liveMapWorld: lw, remote: newFakeMapRemote()}
	a := lw.a
	a.MapRotation = repository.NewMapRotationRepository(a.DB.Pool)
	a.saasAdminActionLimiter, a.saasAdminReadLimiter, a.saasLiveMapLimiter = nil, nil, nil // one actor makes many calls here
	a.mapRotationRemoteFor = func(context.Context, repository.MapRotationTarget) (mapRotationRemote, error) { return w.remote, nil }
	a.mapRotationPost = func(_ repository.MapRotationTarget, channelID string, msg *discordgo.MessageSend) error {
		w.posts, w.postsTo = append(w.posts, msg), append(w.postsTo, channelID)
		return nil
	}
	a.mapRotationAlert = func(alert discord.AdminAlert) { w.alerts = append(w.alerts, alert) }
	target, err := a.MapRotation.Target(context.Background(), lw.f.InstallationID)
	if err != nil || target == nil {
		t.Fatalf("target: %v", err)
	}
	w.target = *target
	return w
}

func (w *mapRotationWorld) flag(on bool) { w.a.Config.MapRotationEnabled = on }

func (w *mapRotationWorld) admin(method string, handler http.HandlerFunc, suffix, actor string, body any) (int, map[string]any) {
	w.t.Helper()
	rr := w.call(handler, method, w.path(suffix), actor, body, nil)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func (w *mapRotationWorld) player(method string, handler http.HandlerFunc, actor string, body any) (int, map[string]any) {
	w.t.Helper()
	rr := w.call(handler, method, "/api/saas/player/servers/x/map/vote", actor, body, map[string]string{"installationID": strconv.FormatInt(w.f.InstallationID, 10)})
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (w *mapRotationWorld) tick(now time.Time) {
	w.a.mapRotationOne(context.Background(), w.target, now)
}

func (w *mapRotationWorld) reboot(at time.Time) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE server_adm_sessions SET adm_file=$2, selected_at=$3 WHERE server_id=$1`,
		w.serverID, fmt.Sprintf("DayZServer_PS4_x64_%d.ADM", standoutSeq.Add(1)), at); err != nil {
		w.t.Fatal(err)
	}
}

func threeMaps() []map[string]any {
	return []map[string]any{
		{"name": "Arena A", "mapFile": "arena_a.json", "spawnFile": "arena_a.xml", "spawnXml": mrSpawnsA, "imageUrl": "https://cdn.example.com/a.png", "enabled": true},
		{"name": "Arena B", "mapFile": "arena_b.json", "spawnFile": "arena_b.xml", "spawnXml": mrSpawnsB, "imageUrl": nil, "enabled": true},
		{"name": "Arena C", "mapFile": "arena_c.json", "spawnFile": "arena_c.xml", "spawnXml": mrSpawnsC, "imageUrl": nil, "enabled": true},
	}
}

// storedSpawns is what Champion holds for a map ("" when nothing is stored).
func (w *mapRotationWorld) storedSpawns(mapID float64) string {
	w.t.Helper()
	var b []byte
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT spawn_xml FROM map_rotation_maps WHERE id=$1 AND installation_id=$2`, int64(mapID), w.f.InstallationID).Scan(&b); err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

// dropSpawns leaves a map as one saved before spawn files were uploaded on the website.
func (w *mapRotationWorld) dropSpawns(mapID float64) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE map_rotation_maps SET spawn_xml=NULL WHERE id=$1 AND installation_id=$2`, int64(mapID), w.f.InstallationID); err != nil {
		w.t.Fatal(err)
	}
}

func errMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func rotationBody(maps []map[string]any, change func(b map[string]any)) map[string]any {
	b := map[string]any{"enabled": true, "everyRestarts": 1, "order": "SEQUENCE", "voteEnabled": true, "voteMinutesBeforeRestart": 30,
		"pingEveryone": true, "announceChannelId": nil, "maps": maps}
	if change != nil {
		change(b)
	}
	return b
}

func TestMapRotationAdminContract(t *testing.T) {
	w := newMapRotationWorld(t)
	owner := w.f.OwnerDiscordID
	a := w.a

	// Off by default: the feature flag is off, so the view says so and nothing can be saved.
	code, view := w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if code != http.StatusOK || view["available"] != false || view["reason"] == nil || view["enabled"] != false {
		t.Fatalf("default view: %d %v", code, view)
	}
	for _, key := range []string{"available", "reason", "enabled", "everyRestarts", "order", "voteEnabled", "voteMinutesBeforeRestart", "pingEveryone", "announceChannelId",
		"wipeCharacters", "maps", "current", "next", "vote", "lastSwitch", "filesCheck"} {
		if _, ok := view[key]; !ok {
			t.Fatalf("the view has no %q", key)
		}
	}
	if view["everyRestarts"].(float64) != 1 || view["order"] != "SEQUENCE" || view["voteMinutesBeforeRestart"].(float64) != 30 || len(view["maps"].([]any)) != 0 ||
		view["vote"] != nil || view["lastSwitch"] != nil || len(view["filesCheck"].([]any)) != 0 || view["wipeCharacters"] != false {
		t.Fatalf("defaults: %v", view)
	}
	cur, next := view["current"].(map[string]any), view["next"].(map[string]any)
	if v, ok := cur["mapId"]; !ok || v != nil || cur["name"] != nil || cur["since"] != nil {
		t.Fatalf("current: %v", cur)
	}
	for _, key := range []string{"mapId", "name", "decidedBy", "switchAt", "restartsUntilSwitch"} {
		if v, ok := next[key]; !ok || v != nil {
			t.Fatalf("next.%s: %v", key, next)
		}
	}
	if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(threeMaps(), nil)); code != http.StatusForbidden || errCode(body) != codeForbidden {
		t.Fatalf("save with the flag off: %d %v", code, body)
	}
	if code, _ := w.admin(http.MethodPost, a.handleCheckMapRotation, "/map/rotation/check", owner, nil); code != http.StatusForbidden {
		t.Fatalf("check with the flag off: %d", code)
	}

	w.flag(true)
	// Owner only: an Administrator is refused on every route.
	adminUser := syncUser(t, a, fmt.Sprintf("mr-admin-%d", standoutSeq.Add(1)), "Admin")
	w.mapRole(adminUser.DiscordUserID, "role-mr-admin", "ADMINISTRATOR")
	for name, call := range map[string]func() (int, map[string]any){
		"GET": func() (int, map[string]any) {
			return w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", adminUser.DiscordUserID, nil)
		},
		"PUT": func() (int, map[string]any) {
			return w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", adminUser.DiscordUserID, rotationBody(threeMaps(), nil))
		},
		"check": func() (int, map[string]any) {
			return w.admin(http.MethodPost, a.handleCheckMapRotation, "/map/rotation/check", adminUser.DiscordUserID, nil)
		},
		"next": func() (int, map[string]any) {
			return w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", adminUser.DiscordUserID, map[string]any{"mapId": nil})
		},
	} {
		if code, body := call(); code != http.StatusForbidden || errCode(body) != codeAdminForbidden {
			t.Fatalf("%s as an Administrator: %d %v", name, code, body)
		}
	}
	code, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if code != http.StatusOK || view["available"] != true || view["reason"] != nil {
		t.Fatalf("available: %d %v", code, view)
	}

	// Validation: each is a 400 VALIDATION_ERROR and saves nothing.
	six := append(threeMaps(), threeMaps()...)
	for i := range six {
		six[i]["mapFile"] = fmt.Sprintf("m%d.json", i)
	}
	for name, body := range map[string]map[string]any{
		"six maps":                rotationBody(six, nil),
		"everyRestarts 4":         rotationBody(threeMaps(), func(b map[string]any) { b["everyRestarts"] = 4 }),
		"order":                   rotationBody(threeMaps(), func(b map[string]any) { b["order"] = "SHUFFLE" }),
		"vote minutes 4":          rotationBody(threeMaps(), func(b map[string]any) { b["voteMinutesBeforeRestart"] = 4 }),
		"vote minutes 121":        rotationBody(threeMaps(), func(b map[string]any) { b["voteMinutesBeforeRestart"] = 121 }),
		"maps missing":            rotationBody(nil, func(b map[string]any) { delete(b, "maps") }),
		"one enabled map":         rotationBody(threeMaps()[:1], nil),
		"path in map file":        rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "../x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"folder in map file":      rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "custom/x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"wrong map extension":     rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.xml", "spawnFile": "x.xml", "enabled": true}), nil),
		"wrong spawn ext":         rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.json", "enabled": true}), nil),
		"http image":              rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "imageUrl": "http://cdn.example.com/x.png", "enabled": true}), nil),
		"long image":              rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "imageUrl": "https://cdn.example.com/" + strings.Repeat("x", 500), "enabled": true}), nil),
		"empty name":              rotationBody(append(threeMaps()[:2], map[string]any{"name": " ", "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"long name":               rotationBody(append(threeMaps()[:2], map[string]any{"name": strings.Repeat("n", 61), "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"unknown map id":          rotationBody(append(threeMaps()[:2], map[string]any{"id": 999999999, "name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"unknown channel":         rotationBody(threeMaps(), func(b map[string]any) { b["announceChannelId"] = "123456789012345678" }),
		"no spawn upload":         rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"empty spawn upload":      rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "spawnXml": "", "enabled": true}), nil),
		"spawn upload not XML":    rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "spawnXml": "<playerspawnpoints><fresh>", "enabled": true}), nil),
		"spawn upload wrong root": rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "spawnXml": "<types><pos x=\"1\" z=\"1\"/></types>", "enabled": true}), nil),
		"spawn upload too large":  rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "spawnXml": mrSpawnsA + strings.Repeat(" ", maprotation.MaxSpawnBytes), "enabled": true}), nil),
		"request too large":       rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "spawnXml": mrSpawnsA + strings.Repeat(" ", mapRotationMaxBody), "enabled": true}), nil),
	} {
		if code, resp := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, body); code != http.StatusBadRequest || errCode(resp) != codeValidationError {
			t.Fatalf("%s: %d %v", name, code, resp)
		}
	}
	if _, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil); len(view["maps"].([]any)) != 0 || view["enabled"] != false {
		t.Fatalf("a refused save stored something: %v", view)
	}
	// A map without a spawn upload is named in plain words.
	_, resp := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner,
		rotationBody(append(threeMaps()[:1], map[string]any{"name": "Dust", "mapFile": "dust.json", "spawnFile": "dust.xml", "enabled": true}), nil))
	if errMessage(resp) != "Map 2 (Dust): upload a spawn file" {
		t.Fatalf("missing spawn upload: %v", resp)
	}

	// A valid save, with a channel of this Discord server.
	ch, err := w.verifier.CreateGuildTextChannel(w.f.DiscordGuildID, "map-vote", "")
	if err != nil {
		t.Fatal(err)
	}
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(threeMaps(), func(b map[string]any) { b["announceChannelId"] = ch.ID; b["everyRestarts"] = 2 }))
	if code != http.StatusOK || view["enabled"] != true || view["announceChannelId"] != ch.ID || view["everyRestarts"].(float64) != 2 || view["pingEveryone"] != true {
		t.Fatalf("save: %d %v", code, view)
	}
	maps := view["maps"].([]any)
	if len(maps) != 3 {
		t.Fatalf("maps: %v", maps)
	}
	ids := make([]float64, 3)
	for i, raw := range maps {
		m := raw.(map[string]any)
		ids[i] = m["id"].(float64)
		if m["position"].(float64) != float64(i) || ids[i] <= 0 || m["enabled"] != true {
			t.Fatalf("map %d: %v", i, m)
		}
	}
	if m := maps[0].(map[string]any); m["name"] != "Arena A" || m["mapFile"] != "arena_a.json" || m["spawnFile"] != "arena_a.xml" || m["imageUrl"] != "https://cdn.example.com/a.png" || maps[1].(map[string]any)["imageUrl"] != nil {
		t.Fatalf("map fields: %v", m)
	}
	// The spawn file is stored in Champion: the view says so and how large, and never returns it.
	for i, want := range []string{mrSpawnsA, mrSpawnsB, mrSpawnsC} {
		m := maps[i].(map[string]any)
		if m["spawnUploaded"] != true || m["spawnBytes"].(float64) != float64(len(want)) || w.storedSpawns(ids[i]) != want {
			t.Fatalf("stored spawns of map %d: %v", i, m)
		}
		if _, has := m["spawnXml"]; has || len(m) != 12 {
			t.Fatalf("a map entry has the contract's twelve fields and no contents: %v", m)
		}
	}
	if raw := w.call(a.handleAdminMapRotation, http.MethodGet, w.path("/map/rotation"), owner, nil, nil).Body.String(); strings.Contains(raw, "generator_posbubbles") {
		t.Fatal("the view returned spawn contents")
	}
	var audited int
	if err := a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM admin_audit_log WHERE action='MAP_ROTATION_SAVE' AND (COALESCE(before_state::text,'') || COALESCE(after_state::text,'')) LIKE '%generator_posbubbles%'`).Scan(&audited); err != nil || audited != 0 {
		t.Fatal("the audit log holds spawn contents")
	}
	if n := view["next"].(map[string]any); n["restartsUntilSwitch"].(float64) != 2 {
		t.Fatalf("restarts until the switch: %v", n)
	}

	// Array order is rotation order: C, A (B removed), ids kept.
	reordered := []map[string]any{
		{"id": ids[2], "name": "Arena C", "mapFile": "arena_c.json", "spawnFile": "arena_c.xml", "imageUrl": nil, "enabled": true},
		{"id": ids[0], "name": "Arena A+", "mapFile": "arena_a.json", "spawnFile": "arena_a.xml", "imageUrl": nil, "enabled": true},
	}
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(reordered, func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false }))
	maps = view["maps"].([]any)
	if code != http.StatusOK || len(maps) != 2 || maps[0].(map[string]any)["id"] != ids[2] || maps[1].(map[string]any)["name"] != "Arena A+" || maps[1].(map[string]any)["position"].(float64) != 1 {
		t.Fatalf("reorder: %d %v", code, maps)
	}
	// spawnXml left out: the stored contents are kept (and the removed map's are gone with it).
	if w.storedSpawns(ids[2]) != mrSpawnsC || w.storedSpawns(ids[0]) != mrSpawnsA || maps[0].(map[string]any)["spawnUploaded"] != true || maps[0].(map[string]any)["spawnBytes"].(float64) != float64(len(mrSpawnsC)) {
		t.Fatalf("a save without spawnXml changed the stored spawns: %v", maps)
	}
	// spawnXml sent: it replaces what was stored, for that map only.
	replaced := []map[string]any{reordered[0], {"id": ids[0], "name": "Arena A+", "mapFile": "arena_a.json", "spawnFile": "arena_a_v2.xml", "spawnXml": mrSpawnsB + "\n", "imageUrl": nil, "enabled": true}}
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(replaced, func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false }))
	if m := view["maps"].([]any)[1].(map[string]any); code != http.StatusOK || m["spawnFile"] != "arena_a_v2.xml" || m["spawnBytes"].(float64) != float64(len(mrSpawnsB)+1) ||
		w.storedSpawns(ids[0]) != mrSpawnsB+"\n" || w.storedSpawns(ids[2]) != mrSpawnsC {
		t.Fatalf("replacing a spawn file: %d %v", code, view["maps"])
	}
	// A map saved before spawn files were uploaded has nothing stored: the view says so, and a
	// save must bring the file. Nothing is saved until it does.
	w.dropSpawns(ids[2])
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if m := view["maps"].([]any)[0].(map[string]any); m["spawnUploaded"] != false || m["spawnBytes"].(float64) != 0 || m["spawnFile"] != "arena_c.xml" {
		t.Fatalf("a map without stored spawns: %v", m)
	}
	code, resp = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(reordered, func(b map[string]any) {
		b["announceChannelId"] = ch.ID
		b["voteEnabled"] = false
		b["everyRestarts"] = 3
	}))
	if code != http.StatusBadRequest || errCode(resp) != codeValidationError || errMessage(resp) != "Map 1 (Arena C): upload a spawn file" {
		t.Fatalf("an old map saved without a spawn file: %d %v", code, resp)
	}
	if _, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil); view["everyRestarts"].(float64) == 3 || w.storedSpawns(ids[0]) != mrSpawnsB+"\n" {
		t.Fatalf("a refused save stored something: %v", view)
	}
	withC := []map[string]any{{"id": ids[2], "name": "Arena C", "mapFile": "arena_c.json", "spawnFile": "arena_c.xml", "spawnXml": mrSpawnsC, "imageUrl": nil, "enabled": true}, reordered[1]}
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(withC, func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false }))
	if code != http.StatusOK || w.storedSpawns(ids[2]) != mrSpawnsC || w.storedSpawns(ids[0]) != mrSpawnsB+"\n" {
		t.Fatalf("uploading the missing spawn file: %d %v", code, view)
	}
	// A sequence without a vote is known in advance.
	if n := view["next"].(map[string]any); n["mapId"] != ids[2] || n["decidedBy"] != "ROTATION" {
		t.Fatalf("next by rotation: %v", n)
	}

	// The staff choice: set, refuse an unknown map, clear.
	code, view = w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", owner, map[string]any{"mapId": ids[0]})
	if n := view["next"].(map[string]any); code != http.StatusOK || n["mapId"] != ids[0] || n["decidedBy"] != "STAFF" || n["name"] != "Arena A+" {
		t.Fatalf("staff next: %d %v", code, view["next"])
	}
	if code, resp := w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", owner, map[string]any{"mapId": ids[1]}); code != http.StatusBadRequest || errCode(resp) != codeValidationError {
		t.Fatalf("staff next for a removed map: %d %v", code, resp)
	}
	code, view = w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", owner, map[string]any{"mapId": nil})
	if n := view["next"].(map[string]any); code != http.StatusOK || n["decidedBy"] != "ROTATION" {
		t.Fatalf("staff next cleared: %d %v", code, view["next"])
	}

	// The file check lists the custom folder for the map files and writes nothing. For the spawn
	// file it reports whether the contents are stored in Champion: nothing on Nitrado is looked up
	// (there is no spawn file in the custom folder at all here).
	delete(w.remote.files, mrCustom+"/arena_a.json")
	w.dropSpawns(ids[2])
	code, view = w.admin(http.MethodPost, a.handleCheckMapRotation, "/map/rotation/check", owner, nil)
	checks := view["filesCheck"].([]any)
	if code != http.StatusOK || len(checks) != 2 {
		t.Fatalf("check: %d %v", code, view)
	}
	for _, raw := range checks {
		c := raw.(map[string]any)
		if c["mapFileFound"] != (c["mapId"] != ids[0]) || c["spawnFileFound"] != (c["mapId"] != ids[2]) || c["checkedAt"] == nil {
			t.Fatalf("check row: %v", c)
		}
	}
	if len(w.remote.uploads) != 0 || len(w.remote.reads) != 0 {
		t.Fatalf("the file check wrote to the server or downloaded a file: %v %v", w.remote.uploads, w.remote.reads)
	}
	// Uploading the spawn file shows at once, without another check.
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(withC, func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false }))
	for _, raw := range view["filesCheck"].([]any) {
		if c := raw.(map[string]any); code != http.StatusOK || c["spawnFileFound"] != true {
			t.Fatalf("check row after the upload: %d %v", code, c)
		}
	}
	w.remote.files[mrCustom+"/arena_a.json"] = []byte(`{"Objects":[]}`)

	// Five maps, each with a spawn file of the maximum size, fit in one save.
	line := "\t\t\t<pos x=\"7500.5\" z=\"7500.5\" />\r\n"
	full := "<playerspawnpoints>\r\n" + strings.Repeat(line, (maprotation.MaxSpawnBytes-48)/len(line)) + "</playerspawnpoints>"
	full += strings.Repeat("\n", maprotation.MaxSpawnBytes-len(full))
	five := []map[string]any{}
	for i := 0; i < maprotation.MaxMaps; i++ {
		five = append(five, map[string]any{"name": fmt.Sprintf("Big %d", i), "mapFile": fmt.Sprintf("big%d.json", i), "spawnFile": "big.xml", "spawnXml": full, "enabled": false})
	}
	// The body is written as a browser writes it (JSON.stringify leaves < and > as they are).
	var fiveBody bytes.Buffer
	enc := json.NewEncoder(&fiveBody)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rotationBody(five, func(b map[string]any) { b["enabled"] = false })); err != nil {
		t.Fatal(err)
	}
	if fiveBody.Len() <= maprotation.MaxMaps*maprotation.MaxSpawnBytes || fiveBody.Len() > mapRotationMaxBody {
		t.Fatalf("test data: a body of %d bytes (limit %d)", fiveBody.Len(), mapRotationMaxBody)
	}
	fiveReq := httptest.NewRequest(http.MethodPut, w.path("/map/rotation"), &fiveBody)
	fiveReq.Header.Set("Authorization", "Bearer test-secret")
	rr := httptest.NewRecorder()
	a.handleSaveMapRotation(rr, withPathValues(withActingUser(fiveReq, owner), w.pathValues()))
	code, view = rr.Code, map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &view)
	if code != http.StatusOK || len(view["maps"].([]any)) != 5 || view["maps"].([]any)[4].(map[string]any)["spawnBytes"].(float64) != float64(maprotation.MaxSpawnBytes) {
		t.Fatalf("five maps at the maximum size: %d %v", code, errMessage(view))
	}
	code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(append(withC[:1:1], map[string]any{"name": "Arena A+", "mapFile": "arena_a.json", "spawnFile": "arena_a.xml", "spawnXml": mrSpawnsA, "enabled": true}), func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false }))
	if code != http.StatusBadRequest {
		// Arena C's id went with the five-map save, so this is refused; put the two maps back as new.
		t.Fatalf("a removed map's id: %d", code)
	}
	back := []map[string]any{threeMaps()[2], threeMaps()[0]}
	if code, view = w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(back, func(b map[string]any) { b["announceChannelId"] = ch.ID; b["voteEnabled"] = false })); code != http.StatusOK {
		t.Fatalf("restore the two maps: %d %v", code, view)
	}
	reordered = []map[string]any{
		{"id": view["maps"].([]any)[0].(map[string]any)["id"], "name": "Arena C", "mapFile": "arena_c.json", "spawnFile": "arena_c.xml", "imageUrl": nil, "enabled": true},
		{"id": view["maps"].([]any)[1].(map[string]any)["id"], "name": "Arena A", "mapFile": "arena_a.json", "spawnFile": "arena_a.xml", "imageUrl": nil, "enabled": true},
	}

	// Plan: Survivor does not include map rotation once plans are enforced.
	setOrgPlan(t, a, w.f.OrgID, entitlements.PlanSurvivor)
	enforcePlanGating(t)
	code, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if code != http.StatusOK || view["available"] != false || !strings.Contains(fmt.Sprint(view["reason"]), "plan") {
		t.Fatalf("Survivor view: %d %v", code, view)
	}
	for name, call := range map[string]func() (int, map[string]any){
		"PUT": func() (int, map[string]any) {
			return w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(reordered, nil))
		},
		"check": func() (int, map[string]any) {
			return w.admin(http.MethodPost, a.handleCheckMapRotation, "/map/rotation/check", owner, nil)
		},
		"next": func() (int, map[string]any) {
			return w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", owner, map[string]any{"mapId": nil})
		},
	} {
		if code, body := call(); code != http.StatusForbidden || errCode(body) != codePlanFeatureRequired {
			t.Fatalf("Survivor %s: %d %v", name, code, body)
		}
	}
	if code, pv := w.player(http.MethodGet, a.handlePlayerMapVote, w.deeloID, nil); code != http.StatusOK || pv["enabled"] != false || pv["vote"] != nil {
		t.Fatalf("Survivor player view: %d %v", code, pv)
	}
	setOrgPlan(t, a, w.f.OrgID, "PREMIUM")
	if code, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil); view["available"] != true {
		t.Fatalf("Champion view: %d %v", code, view)
	}
}

// mapImage asks the public route for a map's picture.
func (w *mapRotationWorld) mapImage(installationID int64, mapID float64) *httptest.ResponseRecorder {
	w.t.Helper()
	return w.call(w.a.handlePublicMapImage, http.MethodGet, "/api/saas/network/servers/x/map-images/y", "", nil,
		map[string]string{"installationID": strconv.FormatInt(installationID, 10), "mapID": strconv.FormatInt(int64(mapID), 10)})
}

// A map's picture is uploaded with the save, kept by saves that do not bring one, replaced,
// removed, shown by version in every view, served by the public route and gone with its map.
func TestMapRotationMapImages(t *testing.T) {
	w := newMapRotationWorld(t)
	owner := w.f.OwnerDiscordID
	a := w.a
	inst := w.f.InstallationID
	w.flag(true)
	png, jpeg := mapTestPNG(2000), mapTestJPEG(900)
	save := func(maps []map[string]any) (int, map[string]any) {
		t.Helper()
		return w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(maps, nil))
	}
	entry := func(view map[string]any, i int) map[string]any { return view["maps"].([]any)[i].(map[string]any) }

	// Refused uploads save nothing and name the map.
	for name, data := range map[string][]byte{
		"too large": mapTestPNG(MaxMapImageBytes + 1),
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`),
		"gif":       []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00"),
	} {
		maps := threeMaps()
		maps[1]["imageData"] = base64.StdEncoding.EncodeToString(data)
		if code, body := save(maps); code != http.StatusBadRequest || errCode(body) != codeValidationError || !strings.HasPrefix(errMessage(body), "Map 2 (Arena B): the picture ") {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}

	maps := threeMaps()
	maps[0]["imageData"] = base64.StdEncoding.EncodeToString(png)
	code, view := save(maps)
	if code != http.StatusOK {
		t.Fatalf("save with a picture: %d %v", code, view)
	}
	idA, idB := entry(view, 0)["id"].(float64), entry(view, 1)["id"].(float64)
	if m := entry(view, 0); m["imageUploaded"] != true || m["imageBytes"].(float64) != float64(len(png)) || m["imageVersion"] != imageVersionOf(png) || m["imageUrl"] != "https://cdn.example.com/a.png" {
		t.Fatalf("map with a picture: %v", m)
	}
	if m := entry(view, 1); m["imageUploaded"] != false || m["imageBytes"].(float64) != 0 || m["imageVersion"] != nil {
		t.Fatalf("map without a picture: %v", m)
	}
	if raw := w.call(a.handleAdminMapRotation, http.MethodGet, w.path("/map/rotation"), owner, nil, nil).Body.String(); strings.Contains(raw, "imageData") || strings.Contains(raw, base64.StdEncoding.EncodeToString(png)[:40]) {
		t.Fatal("the view returned the picture")
	}

	// Served as stored.
	rr := w.mapImage(inst, idA)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), png) || rr.Header().Get("Content-Type") != "image/png" || rr.Header().Get("ETag") != `"`+imageVersionOf(png)+`"` ||
		rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("served picture: %d %v", rr.Code, rr.Header())
	}
	if rr := w.mapImage(inst, idB); rr.Code != http.StatusNotFound {
		t.Fatalf("a map without a picture: %d", rr.Code)
	}
	if rr := w.mapImage(inst+987654, idA); rr.Code != http.StatusNotFound {
		t.Fatalf("the picture under another installation: %d", rr.Code)
	}

	// A vote's options carry the version of the picture the map has now.
	w.tick(time.Now().UTC().Truncate(time.Second))
	optionVersions := func() []any {
		t.Helper()
		code, pv := w.player(http.MethodGet, a.handlePlayerMapVote, w.deeloID, nil)
		vote, _ := pv["vote"].(map[string]any)
		if code != http.StatusOK || vote == nil {
			t.Fatalf("player view: %d %v", code, pv)
		}
		out := []any{}
		for _, o := range vote["options"].([]any) {
			v, has := o.(map[string]any)["imageVersion"]
			if !has {
				t.Fatalf("an option has no imageVersion: %v", o)
			}
			out = append(out, v)
		}
		return out
	}
	if got := optionVersions(); len(got) < 2 || !slices.Contains(got, any(imageVersionOf(png))) || slices.Contains(got, any(imageVersionOf(jpeg))) {
		t.Fatalf("option versions: %v", got)
	}
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if vote, _ := view["vote"].(map[string]any); vote == nil || len(vote["options"].([]any)) == 0 {
		t.Fatalf("admin vote: %v", view["vote"])
	} else {
		for _, o := range vote["options"].([]any) {
			if _, has := o.(map[string]any)["imageVersion"]; !has {
				t.Fatalf("an admin option has no imageVersion: %v", o)
			}
		}
	}

	// A save without imageData keeps the picture.
	stored := func() []map[string]any {
		_, view := w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
		out := []map[string]any{}
		for _, raw := range view["maps"].([]any) {
			m := raw.(map[string]any)
			out = append(out, map[string]any{"id": m["id"], "name": m["name"], "mapFile": m["mapFile"], "spawnFile": m["spawnFile"], "imageUrl": m["imageUrl"], "enabled": m["enabled"]})
		}
		return out
	}
	if code, view = save(stored()); code != http.StatusOK || entry(view, 0)["imageVersion"] != imageVersionOf(png) || entry(view, 0)["imageBytes"].(float64) != float64(len(png)) {
		t.Fatalf("a save without imageData: %d %v", code, view["maps"])
	}
	// A new picture replaces it, for that map only; another map gets its own.
	maps = stored()
	maps[0]["imageData"] = base64.StdEncoding.EncodeToString(jpeg)
	maps[0]["removeImage"] = true // a picture sent in the same save wins
	maps[1]["imageData"] = base64.StdEncoding.EncodeToString(mapTestWebP(500))
	if code, view = save(maps); code != http.StatusOK || entry(view, 0)["imageVersion"] != imageVersionOf(jpeg) || entry(view, 1)["imageUploaded"] != true || entry(view, 2)["imageUploaded"] != false {
		t.Fatalf("replace: %d %v", code, view["maps"])
	}
	if rr := w.mapImage(inst, idA); rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), jpeg) || rr.Header().Get("Content-Type") != "image/jpeg" || rr.Header().Get("ETag") != `"`+imageVersionOf(jpeg)+`"` {
		t.Fatalf("replaced picture: %d %v", rr.Code, rr.Header())
	}
	if rr := w.mapImage(inst, idB); rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("second picture: %d", rr.Code)
	}
	if got := optionVersions(); !slices.Contains(got, any(imageVersionOf(jpeg))) || slices.Contains(got, any(imageVersionOf(png))) {
		t.Fatalf("option versions after the replacement: %v", got)
	}

	// The picture does not depend on the feature flag or on the rotation being on.
	w.flag(false)
	if rr := w.mapImage(inst, idA); rr.Code != http.StatusOK {
		t.Fatalf("with the flag off: %d", rr.Code)
	}
	w.flag(true)

	// removeImage clears it; imageUrl stays.
	maps = stored()
	maps[0]["removeImage"] = true
	if code, view = save(maps); code != http.StatusOK || entry(view, 0)["imageUploaded"] != false || entry(view, 0)["imageVersion"] != nil || entry(view, 0)["imageBytes"].(float64) != 0 ||
		entry(view, 0)["imageUrl"] != "https://cdn.example.com/a.png" || entry(view, 1)["imageUploaded"] != true {
		t.Fatalf("removeImage: %d %v", code, view["maps"])
	}
	if rr := w.mapImage(inst, idA); rr.Code != http.StatusNotFound {
		t.Fatalf("a removed picture: %d", rr.Code)
	}
	var left int
	if err := a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM map_rotation_maps WHERE id=$1 AND (image_data IS NOT NULL OR image_type IS NOT NULL OR image_version IS NOT NULL)`, int64(idA)).Scan(&left); err != nil || left != 0 {
		t.Fatalf("columns after removeImage: %d %v", left, err)
	}

	// A map removed from the list loses its picture with its row.
	maps = stored()
	if code, view = save(append(maps[:1:1], maps[2])); code != http.StatusOK || len(view["maps"].([]any)) != 2 {
		t.Fatalf("remove a map: %d %v", code, view)
	}
	if rr := w.mapImage(inst, idB); rr.Code != http.StatusNotFound {
		t.Fatalf("the picture of a removed map: %d", rr.Code)
	}
	var audited int
	if err := a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM admin_audit_log WHERE action='MAP_ROTATION_SAVE' AND (COALESCE(before_state::text,'') || COALESCE(after_state::text,'')) LIKE '%imageData%'`).Scan(&audited); err != nil || audited != 0 {
		t.Fatal("the audit log holds a picture")
	}
}

func TestMapRotationVoteSwitchAndRollback(t *testing.T) {
	w := newMapRotationWorld(t)
	owner := w.f.OwnerDiscordID
	a := w.a
	ctx := context.Background()
	w.flag(true)
	ch, err := w.verifier.CreateGuildTextChannel(w.f.DiscordGuildID, "map-vote", "")
	if err != nil {
		t.Fatal(err)
	}
	code, view := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(threeMaps(), func(b map[string]any) { b["announceChannelId"] = ch.ID }))
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, view)
	}
	var idA, idB, idC = view["maps"].([]any)[0].(map[string]any)["id"].(float64), view["maps"].([]any)[1].(map[string]any)["id"].(float64), view["maps"].([]any)[2].(map[string]any)["id"].(float64)
	gameplay := func() string { return w.remote.content(mrMission + "/cfggameplay.json") }
	spawns := func() string { return w.remote.content(mrMission + "/cfgplayerspawnpoints.xml") }
	originalGameplay := gameplay()

	// A visitor who is signed in but not linked can look, and cannot vote.
	visitor := syncUser(t, a, fmt.Sprintf("mr-visitor-%d", standoutSeq.Add(1)), "Visitor")
	if code, pv := w.player(http.MethodGet, a.handlePlayerMapVote, visitor.DiscordUserID, nil); code != http.StatusOK || pv["enabled"] != true || pv["linked"] != false || pv["vote"] != nil || pv["current"] != nil || pv["next"] != nil {
		t.Fatalf("visitor before the vote: %d %v", code, pv)
	}
	if code, body := w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": idB}); code != http.StatusConflict || errCode(body) != codeVoteClosed {
		t.Fatalf("vote with no vote open: %d %v", code, body)
	}

	// First tick: the boot is remembered and, every restart being a switch, the vote opens at once
	// (no restart schedule is known) for 30 minutes.
	now := time.Now().UTC().Truncate(time.Second)
	w.tick(now)
	if len(w.posts) != 1 || w.postsTo[0] != ch.ID {
		t.Fatalf("vote announcement: %d posts", len(w.posts))
	}
	post := w.posts[0]
	link := fmt.Sprintf("/vote/%d", w.f.InstallationID)
	if !strings.HasPrefix(post.Content, "@everyone") || !strings.Contains(post.Content, link) || len(post.AllowedMentions.Parse) != 1 || post.AllowedMentions.Parse[0] != discordgo.AllowedMentionTypeEveryone {
		t.Fatalf("ping: %q %+v", post.Content, post.AllowedMentions)
	}
	if e := post.Embeds[0]; !strings.Contains(e.Title, "Pick the next map") || !strings.Contains(e.Description, link) || !strings.Contains(e.Fields[0].Value, "Arena B") {
		t.Fatalf("vote embed: %+v", post.Embeds[0])
	}
	w.tick(now.Add(time.Minute))
	if len(w.posts) != 1 {
		t.Fatal("the vote was announced twice")
	}

	code, pv := w.player(http.MethodGet, a.handlePlayerMapVote, w.deeloID, nil)
	vote, _ := pv["vote"].(map[string]any)
	if code != http.StatusOK || pv["linked"] != true || vote == nil || vote["status"] != "OPEN" || len(vote["options"].([]any)) != 3 || vote["myVote"] != nil || vote["totalVotes"].(float64) != 0 || pv["serverName"] == nil {
		t.Fatalf("player view: %d %v", code, pv)
	}
	closes, _ := time.Parse(time.RFC3339, vote["closesAt"].(string))
	if d := closes.Sub(now); d != 30*time.Minute {
		t.Fatalf("the vote is open for %v", d)
	}
	for _, key := range []string{"id", "status", "opensAt", "closesAt", "options", "totalVotes", "myVote"} {
		if _, ok := vote[key]; !ok {
			t.Fatalf("vote has no %q", key)
		}
	}

	if code, body := w.player(http.MethodPost, a.handleCastMapVote, visitor.DiscordUserID, map[string]any{"mapId": idB}); code != http.StatusForbidden || errCode(body) != codeNotLinked {
		t.Fatalf("unlinked vote: %d %v", code, body)
	}
	if code, body := w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": 424242}); code != http.StatusBadRequest || errCode(body) != codeValidationError {
		t.Fatalf("unknown map: %d %v", code, body)
	}
	if code, body := w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{}); code != http.StatusBadRequest || errCode(body) != codeValidationError {
		t.Fatalf("no map: %d %v", code, body)
	}
	// One vote per player; voting again changes it.
	code, pv = w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": idB})
	if v := pv["vote"].(map[string]any); code != http.StatusOK || v["myVote"] != idB || v["totalVotes"].(float64) != 1 {
		t.Fatalf("vote: %d %v", code, pv)
	}
	code, pv = w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": idC})
	if v := pv["vote"].(map[string]any); code != http.StatusOK || v["myVote"] != idC || v["totalVotes"].(float64) != 1 {
		t.Fatalf("changed vote: %d %v", code, pv)
	}
	if code, _ := w.player(http.MethodPost, a.handleCastMapVote, w.mateID, map[string]any{"mapId": idC}); code != http.StatusOK {
		t.Fatalf("second voter: %d", code)
	}

	// Still open: nothing is written.
	w.tick(now.Add(29 * time.Minute))
	if len(w.remote.uploads) != 0 || gameplay() != originalGameplay {
		t.Fatal("files were written while the vote was open")
	}
	// The vote closes: C wins, the files are written and verified, the result is posted.
	w.tick(now.Add(30 * time.Minute))
	if !strings.Contains(gameplay(), `"custom/arena_c.json"`) || !strings.Contains(gameplay(), `"custom/shop.json"`) || spawns() != mrSpawnsC || len(w.remote.uploads) != 2 {
		t.Fatalf("switch: uploads %v\n%s", w.remote.uploads, gameplay())
	}
	if len(w.posts) != 2 || !strings.Contains(w.posts[1].Embeds[0].Title, "Next map: Arena C") || w.posts[1].Content != "" || len(w.posts[1].AllowedMentions.Parse) != 0 ||
		!strings.Contains(w.posts[1].Embeds[0].Fields[0].Value, "Arena C: 2 votes") {
		t.Fatalf("result post: %d %+v", len(w.posts), w.posts[len(w.posts)-1].Embeds[0])
	}
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	last := view["lastSwitch"].(map[string]any)
	if last["ok"] != true || last["mapId"] != idC || last["name"] != "Arena C" || view["next"].(map[string]any)["decidedBy"] != "VOTE" || view["vote"].(map[string]any)["status"] != "CLOSED" {
		t.Fatalf("after the switch: %v", view)
	}
	if view["current"].(map[string]any)["mapId"] != nil {
		t.Fatal("the map is only current once the server restarted")
	}
	if code, body := w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": idB}); code != http.StatusConflict || errCode(body) != codeVoteClosed {
		t.Fatalf("vote after closing: %d %v", code, body)
	}
	// Only the two live files and the map file were ever downloaded.
	for _, p := range w.remote.reads {
		if p != mrMission+"/cfggameplay.json" && p != mrMission+"/cfgplayerspawnpoints.xml" && p != mrCustom+"/arena_c.json" {
			t.Fatalf("the switch downloaded %s", p)
		}
	}
	// More ticks in the same period change nothing (idempotent).
	w.tick(now.Add(31 * time.Minute))
	w.tick(now.Add(45 * time.Minute))
	if len(w.remote.uploads) != 2 || len(w.posts) != 2 {
		t.Fatalf("repeated ticks: uploads %d posts %d", len(w.remote.uploads), len(w.posts))
	}

	// The restart: C is the current map, it is announced, and the next vote opens (C is not an option).
	boot2 := now.Add(time.Hour)
	w.reboot(boot2)
	w.tick(boot2.Add(time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if c := view["current"].(map[string]any); c["mapId"] != idC || c["name"] != "Arena C" || c["since"] == nil {
		t.Fatalf("current after the restart: %v", c)
	}
	if len(w.posts) != 4 || !strings.Contains(w.posts[2].Embeds[0].Title, "Map changed to Arena C") || !strings.Contains(w.posts[3].Embeds[0].Title, "Pick the next map") {
		t.Fatalf("posts after the restart: %d", len(w.posts))
	}
	options := view["vote"].(map[string]any)["options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["mapId"] != idA || options[1].(map[string]any)["mapId"] != idB {
		t.Fatalf("second vote options (rotation order after C): %v", options)
	}

	// A staff choice overrides the vote: B, although A gets the votes.
	if code, _ := w.player(http.MethodPost, a.handleCastMapVote, w.deeloID, map[string]any{"mapId": idA}); code != http.StatusOK {
		t.Fatalf("vote: %d", code)
	}
	if code, _ := w.admin(http.MethodPost, a.handleSetNextMap, "/map/rotation/next", owner, map[string]any{"mapId": idB}); code != http.StatusOK {
		t.Fatal("staff choice")
	}
	// ...and the second write is refused: the first file is restored and staff are told.
	w.remote.refuseAt = w.remote.uploadCount + 2
	beforeGameplay, beforeSpawns := gameplay(), spawns()
	w.tick(boot2.Add(40 * time.Minute))
	if gameplay() != beforeGameplay || spawns() != beforeSpawns {
		t.Fatalf("a failed switch left the files changed:\n%s", gameplay())
	}
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	last = view["lastSwitch"].(map[string]any)
	if last["ok"] != false || last["mapId"] != idB || !strings.Contains(last["message"].(string), "put back") || view["next"].(map[string]any)["decidedBy"] != "STAFF" {
		t.Fatalf("after the failed switch: %v", view)
	}
	if len(w.alerts) != 1 || w.alerts[0].Kind != discord.AlertKindMapRotation || w.alerts[0].ServerID != w.serverID {
		t.Fatalf("staff alert: %+v", w.alerts)
	}
	var status string
	if err := a.DB.Pool.QueryRow(ctx, `SELECT status FROM map_rotation_switches WHERE installation_id=$1 ORDER BY id DESC LIMIT 1`, w.f.InstallationID).Scan(&status); err != nil || status != repository.MapSwitchRolledBack {
		t.Fatalf("switch log: %s %v", status, err)
	}
	// The staff choice was for one switch only; it is carried to the next period and retried there
	// without a new vote.
	w.remote.refuseAt = 0
	boot3 := boot2.Add(2 * time.Hour)
	w.reboot(boot3)
	posts := len(w.posts)
	w.tick(boot3.Add(time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	if view["current"].(map[string]any)["mapId"] != idC || view["vote"] != nil || len(w.posts) != posts {
		t.Fatalf("after a failed switch the map stays and no new vote opens: %v", view)
	}
	w.tick(boot3.Add(31 * time.Minute))
	if !strings.Contains(gameplay(), `"custom/arena_b.json"`) || strings.Contains(gameplay(), "arena_c.json") || spawns() != mrSpawnsB {
		t.Fatalf("retried switch:\n%s", gameplay())
	}

	// With the feature flag off the worker does nothing at all, whatever happens on the server.
	w.flag(false)
	uploads := len(w.remote.uploads)
	posts = len(w.posts)
	boot4 := boot3.Add(2 * time.Hour)
	w.reboot(boot4)
	w.tick(boot4.Add(time.Minute))
	w.tick(boot4.Add(90 * time.Minute))
	var lastBoot string
	_ = a.DB.Pool.QueryRow(ctx, `SELECT last_boot_file FROM map_rotation_settings WHERE installation_id=$1`, w.f.InstallationID).Scan(&lastBoot)
	if len(w.remote.uploads) != uploads || len(w.posts) != posts {
		t.Fatal("the worker acted with the feature flag off")
	}
	// The owner's own switch: off means off as well.
	w.flag(true)
	if code, _ := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(threeMaps()[:0], func(b map[string]any) { b["enabled"] = false })); code != http.StatusOK {
		t.Fatal("switch off")
	}
	w.tick(boot4.Add(2 * time.Hour))
	if len(w.remote.uploads) != uploads || len(w.posts) != posts {
		t.Fatal("the worker acted with the rotation switched off")
	}
	if code, pv := w.player(http.MethodGet, a.handlePlayerMapVote, w.deeloID, nil); code != http.StatusOK || pv["enabled"] != false {
		t.Fatalf("player view when off: %d %v", code, pv)
	}
}

// A switch a crash left PENDING: within the window it is finished without a second backup; too
// old, nothing is written and the rotation stops.
func TestMapRotationInterruptedSwitch(t *testing.T) {
	w := newMapRotationWorld(t)
	a := w.a
	ctx := context.Background()
	w.flag(true)
	code, view := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(threeMaps(), func(b map[string]any) { b["voteEnabled"] = false }))
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, view)
	}
	idB := int64(view["maps"].([]any)[1].(map[string]any)["id"].(float64))
	now := time.Now().UTC().Truncate(time.Second)
	w.tick(now) // baseline
	original := w.remote.content(mrMission + "/cfggameplay.json")

	// The first attempt saved its backup and wrote cfggameplay.json, then the process died.
	sw, created, err := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, idB, "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", now)
	if err != nil || !created {
		t.Fatalf("begin: %v", err)
	}
	if err := a.MapRotation.SaveSwitchBackup(ctx, sw.ID, []byte(original), []byte(mrSpawnsA), now); err != nil {
		t.Fatal(err)
	}
	if err := a.MapRotation.SaveSwitchBackup(ctx, sw.ID, []byte("other"), []byte("other"), now); !errors.Is(err, repository.ErrMapSwitchNotPending) {
		t.Fatalf("a saved backup must never be replaced: %v", err)
	}
	again, created, _ := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, idB, "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", now)
	if created || again.ID != sw.ID || again.Attempts != 2 {
		t.Fatalf("a second switch was started: attempts %d", again.Attempts)
	}
	w.remote.files[mrMission+"/cfggameplay.json"] = []byte(strings.Replace(original, "\"custom/shop.json\"", "\"custom/shop.json\",\n\t\t\t\"custom/arena_b.json\"", 1))
	// The switch took its copy of Arena B's spawn file when it began. The owner uploads another one
	// before the switch is finished: the resumed switch still writes what the first attempt had.
	if string(sw.SpawnXML) != mrSpawnsB || string(again.SpawnXML) != mrSpawnsB {
		t.Fatal("the switch did not take a copy of the map's spawn file when it began")
	}
	maps := threeMaps()
	for i, raw := range view["maps"].([]any) {
		maps[i]["id"] = raw.(map[string]any)["id"]
		delete(maps[i], "spawnXml")
	}
	maps[1]["spawnXml"] = mrSpawnsC
	if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(maps, func(b map[string]any) { b["voteEnabled"] = false })); code != http.StatusOK || w.storedSpawns(float64(idB)) != mrSpawnsC {
		t.Fatalf("upload during the switch: %d %v", code, body)
	}

	w.tick(now.Add(time.Minute))
	if strings.Join(w.remote.uploads, " ") != mrMission+"/cfgplayerspawnpoints.xml" || w.remote.content(mrMission+"/cfgplayerspawnpoints.xml") != mrSpawnsB {
		t.Fatalf("the interrupted switch was not finished by writing only the missing file: %v", w.remote.uploads)
	}
	var status string
	var backup, copyLeft []byte
	_ = a.DB.Pool.QueryRow(ctx, `SELECT status, prev_gameplay, spawn_xml FROM map_rotation_switches WHERE id=$1`, sw.ID).Scan(&status, &backup, &copyLeft)
	if status != repository.MapSwitchApplied || string(backup) != original || copyLeft != nil {
		t.Fatalf("resumed switch: %s (the copy of the spawn file is dropped when the switch finishes: %d bytes left)", status, len(copyLeft))
	}

	// An old one: closed as failed, nothing written, rotation stopped, staff told.
	old, _, err := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, idB, "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.MapRotation.SaveSwitchBackup(ctx, old.ID, []byte(original), []byte(mrSpawnsA), now); err != nil {
		t.Fatal(err)
	}
	uploads := len(w.remote.uploads)
	w.tick(now.Add(2 * time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	last := view["lastSwitch"].(map[string]any)
	if len(w.remote.uploads) != uploads || last["ok"] != false || !strings.Contains(last["message"].(string), "rotation is stopped") || len(w.alerts) != 1 || w.alerts[0].Severity != discord.AlertCritical {
		t.Fatalf("stale switch: uploads %d, %v, alerts %d", len(w.remote.uploads)-uploads, last, len(w.alerts))
	}
	active, _ := a.MapRotation.ActiveInstallations(ctx)
	for _, t2 := range active {
		if t2.InstallationID == w.f.InstallationID {
			t.Fatal("a stopped rotation must not be worked")
		}
	}
	// Saving the settings starts it again.
	if code, _ := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(threeMaps(), func(b map[string]any) { b["voteEnabled"] = false })); code != http.StatusOK {
		t.Fatal("save")
	}
}

// A map with no spawn file stored (saved before spawn files were uploaded on the website): the
// switch fails before anything is sent to the server, with a reason the owner can act on, and
// staff are told. Once the file is uploaded the next period's switch goes through.
func TestMapRotationSwitchWithoutStoredSpawns(t *testing.T) {
	w := newMapRotationWorld(t)
	a := w.a
	ctx := context.Background()
	owner := w.f.OwnerDiscordID
	w.flag(true)
	code, view := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(threeMaps(), func(b map[string]any) { b["voteEnabled"] = false }))
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, view)
	}
	idA, idB := view["maps"].([]any)[0].(map[string]any)["id"].(float64), view["maps"].([]any)[1].(map[string]any)["id"].(float64)
	w.dropSpawns(idA)
	gameplay, spawns := w.remote.content(mrMission+"/cfggameplay.json"), w.remote.content(mrMission+"/cfgplayerspawnpoints.xml")
	// An old spawn file still lying in the custom folder is not used.
	w.remote.files[mrCustom+"/arena_a.xml"] = []byte(mrSpawnsC)

	now := time.Now().UTC().Truncate(time.Second)
	w.tick(now)                       // baseline
	w.tick(now.Add(31 * time.Minute)) // the rotation decides Arena A and tries to switch
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	last, _ := view["lastSwitch"].(map[string]any)
	if last == nil || last["ok"] != false || last["message"] != "Upload a spawn file for Arena A on the Map rotation page. Nothing was changed." {
		t.Fatalf("switch without stored spawns: %v", view["lastSwitch"])
	}
	if len(w.remote.uploads) != 0 || len(w.remote.reads) != 0 || w.remote.content(mrMission+"/cfggameplay.json") != gameplay || w.remote.content(mrMission+"/cfgplayerspawnpoints.xml") != spawns {
		t.Fatalf("the server was touched: uploads %v reads %v", w.remote.uploads, w.remote.reads)
	}
	var status string
	if err := a.DB.Pool.QueryRow(ctx, `SELECT status FROM map_rotation_switches WHERE installation_id=$1 ORDER BY id DESC LIMIT 1`, w.f.InstallationID).Scan(&status); err != nil || status != repository.MapSwitchFailed {
		t.Fatalf("switch log: %s %v", status, err)
	}
	if len(w.alerts) != 1 || w.alerts[0].Kind != discord.AlertKindMapRotation || w.alerts[0].Severity != discord.AlertWarning || !strings.Contains(w.alerts[0].Detail, "Upload a spawn file for Arena A") {
		t.Fatalf("staff alert: %+v", w.alerts)
	}

	// The owner uploads the file; the next period switches to Arena A with the uploaded contents.
	maps := threeMaps()
	for i, raw := range view["maps"].([]any) {
		maps[i]["id"] = raw.(map[string]any)["id"]
		delete(maps[i], "spawnXml")
	}
	maps[0]["spawnXml"] = mrSpawnsB
	if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, rotationBody(maps, func(b map[string]any) { b["voteEnabled"] = false })); code != http.StatusOK {
		t.Fatalf("upload: %d %v", code, body)
	}
	boot2 := now.Add(time.Hour)
	w.reboot(boot2)
	w.tick(boot2.Add(time.Minute))
	w.tick(boot2.Add(32 * time.Minute))
	if !strings.Contains(w.remote.content(mrMission+"/cfggameplay.json"), `"custom/arena_a.json"`) || w.remote.content(mrMission+"/cfgplayerspawnpoints.xml") != mrSpawnsB {
		t.Fatalf("switch after the upload: uploads %v", w.remote.uploads)
	}

	// A switch that began before spawn files were stored, had written a file and was interrupted:
	// there is nothing to finish it with, so nothing more is written and a person must look.
	boot3 := boot2.Add(2 * time.Hour)
	w.reboot(boot3)
	w.tick(boot3.Add(time.Minute))
	w.dropSpawns(idB)
	sw, created, err := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, int64(idB), "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", boot3.Add(2*time.Minute))
	if err != nil || !created || len(sw.SpawnXML) != 0 {
		t.Fatalf("begin: %v", err)
	}
	if err := a.MapRotation.SaveSwitchBackup(ctx, sw.ID, []byte(gameplay), []byte(spawns), boot3.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	uploads := len(w.remote.uploads)
	w.tick(boot3.Add(3 * time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil)
	last = view["lastSwitch"].(map[string]any)
	if len(w.remote.uploads) != uploads || last["ok"] != false || !strings.Contains(last["message"].(string), "rotation is stopped") || w.alerts[len(w.alerts)-1].Severity != discord.AlertCritical {
		t.Fatalf("interrupted switch without a copy of the spawn file: %v", last)
	}
}

// With a restart schedule from Nitrado the vote opens voteMinutes + 5 minutes before the restart
// and closes 5 minutes before it; when Nitrado cannot be asked, nothing is decided.
func TestMapRotationFollowsTheRestartSchedule(t *testing.T) {
	w := newMapRotationWorld(t)
	a := w.a
	w.flag(true)
	if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(threeMaps(), nil)); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, body)
	}
	now := time.Now().UTC().Truncate(time.Second)
	restart := now.Add(2 * time.Hour)
	w.remote.tasks = []nitrado.ScheduledTask{{ID: 1, ActionMethod: "restart", NextRun: restart.Format(time.RFC3339)}}

	// Nitrado unreachable: the boot is remembered, nothing else happens.
	remoteFor := a.mapRotationRemoteFor
	a.mapRotationRemoteFor = func(context.Context, repository.MapRotationTarget) (mapRotationRemote, error) {
		return nil, errors.New("down")
	}
	w.tick(now)
	_, view := w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	if view["vote"] != nil || view["next"].(map[string]any)["switchAt"] != nil {
		t.Fatalf("with Nitrado down nothing is decided: %v", view)
	}
	a.mapRotationRemoteFor = remoteFor

	w.tick(now.Add(time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	if next := view["next"].(map[string]any); view["vote"] != nil || next["switchAt"] != restart.Format(time.RFC3339) || next["restartsUntilSwitch"].(float64) != 1 {
		t.Fatalf("two hours before the restart: %v", view)
	}
	w.tick(restart.Add(-36 * time.Minute))
	if _, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil); view["vote"] != nil {
		t.Fatal("the vote opened too early")
	}
	w.tick(restart.Add(-35 * time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	vote, _ := view["vote"].(map[string]any)
	if vote == nil || vote["status"] != "OPEN" || vote["closesAt"] != restart.Add(-5*time.Minute).Format(time.RFC3339) {
		t.Fatalf("vote window: %v", vote)
	}
	w.tick(restart.Add(-6 * time.Minute))
	if len(w.remote.uploads) != 0 {
		t.Fatal("written before the vote closed")
	}
	// Nobody voted: the rotation decides (the first map) and the files are written 5 minutes before the restart.
	w.tick(restart.Add(-5 * time.Minute))
	_, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	// (Arena A's spawn points are already the live ones here, so only cfggameplay.json is written.)
	if next := view["next"].(map[string]any); len(w.remote.uploads) != 1 || !strings.Contains(w.remote.content(mrMission+"/cfggameplay.json"), "custom/arena_a.json") || next["decidedBy"] != "ROTATION" || next["name"] != "Arena A" || view["lastSwitch"].(map[string]any)["ok"] != true {
		t.Fatalf("switch at the closing time: uploads %v %v", w.remote.uploads, view)
	}
}

// --- fresh characters on every map switch -------------------------------------------------------------

const (
	mrStorage   = mrMission + "/storage_1"
	mrPlayersDB = mrStorage + "/players.db"
)

// wipeWorld is a rotation without a vote on a server that has saved characters and a base, with
// the waits of the clearing cut to nothing and the clearing run inside the tick.
func wipeWorld(t *testing.T, change func(b map[string]any)) (*mapRotationWorld, map[string]any) {
	w := newMapRotationWorld(t)
	w.flag(true)
	clock := time.Now()
	w.a.mapRotationWipeCfg = mapRotationWipeConfig{inline: true, opts: charwipe.Options{
		Now: func() time.Time { return clock }, Sleep: func(_ context.Context, d time.Duration) { clock = clock.Add(d) }}}
	w.remote.files[mrPlayersDB] = []byte("characters")
	w.remote.files[mrStorage+"/vehicles.bin"] = []byte("vehicles and bases")
	code, view := w.admin(http.MethodPut, w.a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(threeMaps(), func(b map[string]any) {
		b["voteEnabled"] = false
		b["wipeCharacters"] = true
		if change != nil {
			change(b)
		}
	}))
	if code != http.StatusOK {
		t.Fatalf("save: %d %v", code, view)
	}
	return w, view
}

func (w *mapRotationWorld) lastSwitch() map[string]any {
	w.t.Helper()
	_, view := w.admin(http.MethodGet, w.a.handleAdminMapRotation, "/map/rotation", w.f.OwnerDiscordID, nil)
	last, _ := view["lastSwitch"].(map[string]any)
	if last == nil {
		w.t.Fatalf("no last switch: %v", view)
	}
	return last
}

func (w *mapRotationWorld) isActive() bool {
	active, _ := w.a.MapRotation.ActiveInstallations(context.Background())
	for _, t := range active {
		if t.InstallationID == w.f.InstallationID {
			return true
		}
	}
	return false
}

// The option end to end: off by default, saved and returned; after a switch the server is stopped,
// players.db (and nothing else) is deleted and the server is started; the boots that follow count
// as one restart; and a server without the file is left running.
func TestMapRotationFreshCharacters(t *testing.T) {
	w, view := wipeWorld(t, func(b map[string]any) { b["everyRestarts"] = 2 })
	a := w.a
	ctx := context.Background()
	if view["wipeCharacters"] != true {
		t.Fatalf("the option was not saved: %v", view["wipeCharacters"])
	}
	// Left out of a save it is off.
	if _, v := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(nil, func(b map[string]any) {
		b["enabled"], b["maps"] = false, view["maps"]
		for _, m := range view["maps"].([]any) {
			delete(m.(map[string]any), "spawnXml")
		}
	})); v["wipeCharacters"] != false {
		t.Fatalf("left out, the option must be off: %v", v)
	}
	maps := threeMaps()
	for i, raw := range view["maps"].([]any) {
		maps[i]["id"] = raw.(map[string]any)["id"]
		delete(maps[i], "spawnXml")
	}
	save := func(change func(b map[string]any)) {
		t.Helper()
		if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(maps, func(b map[string]any) {
			b["voteEnabled"], b["wipeCharacters"], b["everyRestarts"] = false, true, 2
			if change != nil {
				change(b)
			}
		})); code != http.StatusOK {
			t.Fatalf("save: %d %v", code, body)
		}
	}
	save(nil)

	// every 2 restarts: one restart is counted first, then the period before the switch.
	start := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	w.tick(start) // baseline
	boot1 := start.Add(time.Hour)
	w.reboot(boot1)
	w.tick(boot1.Add(time.Minute))
	if len(w.remote.serverCalls) != 0 {
		t.Fatalf("nothing is stopped before a switch: %v", w.remote.serverCalls)
	}
	w.tick(boot1.Add(31 * time.Minute)) // the rotation decides Arena A, writes the files, clears the characters

	if got := strings.Join(w.remote.serverCalls, ", "); got != "stop, delete "+mrPlayersDB+", restart" {
		t.Fatalf("server calls: %s", got)
	}
	if _, there := w.remote.files[mrPlayersDB]; there {
		t.Fatal("players.db is still there")
	}
	if w.remote.content(mrStorage+"/vehicles.bin") != "vehicles and bases" || !strings.Contains(w.remote.content(mrMission+"/cfggameplay.json"), `"custom/arena_a.json"`) {
		t.Fatal("something other than players.db was touched, or the map's files were not written first")
	}
	last := w.lastSwitch()
	if last["ok"] != true || last["charactersCleared"] != true || last["message"] != "Both files were written and verified. Saved characters were cleared, so everyone spawns fresh." {
		t.Fatalf("last switch: %v", last)
	}
	if len(w.alerts) != 0 || !w.isActive() {
		t.Fatalf("a clean clearing alerts nobody and the rotation goes on: %+v", w.alerts)
	}
	var wipeState string
	var wipeRestart, leaseUntil *time.Time
	if err := a.DB.Pool.QueryRow(ctx, `SELECT w.wipe_state, s.wipe_restart_at, s.lease_until FROM map_rotation_switches w JOIN map_rotation_settings s USING (installation_id)
WHERE w.installation_id=$1 ORDER BY w.id DESC LIMIT 1`, w.f.InstallationID).Scan(&wipeState, &wipeRestart, &leaseUntil); err != nil {
		t.Fatal(err)
	}
	if wipeState != repository.MapWipeDone || wipeRestart == nil || leaseUntil != nil {
		t.Fatalf("after the clearing: state %s, restart recorded %v, lease released %v", wipeState, wipeRestart != nil, leaseUntil == nil)
	}

	// Restart counting. Champion's own restart boots the server at +2 minutes: the map is active.
	// The scheduled restart follows at +6 minutes: the same restart, not counted.
	count := func() (n int, current string) {
		t.Helper()
		var name *string
		if err := a.DB.Pool.QueryRow(ctx, `SELECT restarts_since_switch, current_map_name FROM map_rotation_settings WHERE installation_id=$1`, w.f.InstallationID).Scan(&n, &name); err != nil {
			t.Fatal(err)
		}
		if name != nil {
			current = *name
		}
		return n, current
	}
	wr := *wipeRestart
	w.reboot(wr.Add(2 * time.Minute))
	w.tick(wr.Add(3 * time.Minute))
	if n, cur := count(); n != 0 || cur != "Arena A" {
		t.Fatalf("after Champion's own restart: count %d, current %q", n, cur)
	}
	w.reboot(wr.Add(6 * time.Minute))
	w.tick(wr.Add(7 * time.Minute))
	w.tick(wr.Add(8 * time.Minute))
	if n, cur := count(); n != 0 || cur != "Arena A" {
		t.Fatalf("the scheduled restart inside the window was counted: count %d, current %q", n, cur)
	}
	var lastBoot, bootNow string
	_ = a.DB.Pool.QueryRow(ctx, `SELECT s.last_boot_file, b.adm_file FROM map_rotation_settings s, server_adm_sessions b WHERE s.installation_id=$1 AND b.server_id=$2`, w.f.InstallationID, w.serverID).Scan(&lastBoot, &bootNow)
	if lastBoot == "" || lastBoot != bootNow {
		t.Fatalf("the boot inside the window must still be remembered: %q vs %q", lastBoot, bootNow)
	}
	// A boot at +25 minutes is an ordinary restart.
	boot4 := wr.Add(25 * time.Minute)
	w.reboot(boot4)
	w.tick(boot4.Add(time.Minute))
	if n, _ := count(); n != 1 {
		t.Fatalf("a restart after the window: count %d, want 1", n)
	}

	// The next switch finds no players.db (nobody has joined): the server is not stopped, the
	// switch stays applied, the rotation goes on and staff are told.
	calls := len(w.remote.serverCalls)
	w.tick(boot4.Add(32 * time.Minute))
	last = w.lastSwitch()
	if len(w.remote.serverCalls) != calls {
		t.Fatalf("a server without the file must not be stopped: %v", w.remote.serverCalls[calls:])
	}
	if last["ok"] != true || last["charactersCleared"] != false || last["name"] != "Arena B" ||
		last["message"] != "Both files were written and verified. Saved characters could not be cleared: the saved-characters file was not found." {
		t.Fatalf("last switch without the file: %v", last)
	}
	if len(w.alerts) != 1 || w.alerts[0].Severity != discord.AlertWarning || w.alerts[0].Headline != "Saved characters not cleared" || !w.isActive() {
		t.Fatalf("alerts %+v, active %v", w.alerts, w.isActive())
	}

	// With the option off nothing is ever stopped or deleted.
	save(func(b map[string]any) { b["wipeCharacters"], b["everyRestarts"] = false, 1 })
	w.remote.files[mrPlayersDB] = []byte("characters")
	boot5 := boot4.Add(2 * time.Hour)
	w.reboot(boot5)
	w.tick(boot5.Add(time.Minute))
	w.tick(boot5.Add(32 * time.Minute))
	last = w.lastSwitch()
	if len(w.remote.serverCalls) != calls || last["name"] != "Arena C" || last["ok"] != true || last["charactersCleared"] != nil || last["message"] != "Both files were written and verified." {
		t.Fatalf("option off: calls %v, last %v", w.remote.serverCalls[calls:], last)
	}
	if _, there := w.remote.files[mrPlayersDB]; !there {
		t.Fatal("players.db was deleted with the option off")
	}
}

// The server was stopped and does not start: the rotation stops and staff get a critical alert.
// And after a crash in the middle, the next tick starts the server without stopping or deleting
// again, even when the rotation was switched off in the meantime.
func TestMapRotationFreshCharactersServerDownAndCrash(t *testing.T) {
	w, view := wipeWorld(t, nil)
	a := w.a
	ctx := context.Background()
	idB := int64(view["maps"].([]any)[1].(map[string]any)["id"].(float64))
	w.remote.restartDead = true
	start := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	w.tick(start)
	w.tick(start.Add(31 * time.Minute))

	last := w.lastSwitch()
	msg, _ := last["message"].(string)
	if last["ok"] != true || last["charactersCleared"] != true || !strings.Contains(msg, charwipe.ServerDownMessage) || !strings.Contains(msg, "rotation is stopped") {
		t.Fatalf("last switch: %v", last)
	}
	if w.remote.serverCalls[0] != "stop" || w.remote.serverCalls[1] != "delete "+mrPlayersDB || len(w.remote.serverCalls) != 5 || w.remote.serverCalls[4] != "restart" {
		t.Fatalf("the server is asked to start three times: %v", w.remote.serverCalls)
	}
	if len(w.alerts) != 1 || w.alerts[0].Severity != discord.AlertCritical || !strings.Contains(w.alerts[0].Detail, "Start it in Nitrado") || w.isActive() {
		t.Fatalf("alerts %+v, still active %v", w.alerts, w.isActive())
	}
	var halted string
	_ = a.DB.Pool.QueryRow(ctx, `SELECT halted_reason FROM map_rotation_settings WHERE installation_id=$1`, w.f.InstallationID).Scan(&halted)
	if halted != charwipe.ServerDownMessage {
		t.Fatalf("halted reason: %q", halted)
	}

	// --- a crash right after the stop was requested -------------------------------------------
	maps := threeMaps()
	for i, raw := range view["maps"].([]any) {
		maps[i]["id"] = raw.(map[string]any)["id"]
		delete(maps[i], "spawnXml")
	}
	save := func(enabled bool) {
		t.Helper()
		if code, body := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", w.f.OwnerDiscordID, rotationBody(maps, func(b map[string]any) {
			b["voteEnabled"], b["wipeCharacters"], b["enabled"] = false, true, enabled
		})); code != http.StatusOK {
			t.Fatalf("save: %d %v", code, body)
		}
	}
	crash := func(states ...string) int64 {
		t.Helper()
		now := time.Now().UTC()
		sw, created, err := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, idB, "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", now)
		if err != nil || !created || sw.WipeState != repository.MapWipeNone {
			t.Fatalf("begin: %v %+v", err, sw)
		}
		for _, state := range states {
			if err := a.MapRotation.SetWipeState(ctx, w.f.InstallationID, sw.ID, state, now); err != nil {
				t.Fatalf("state %s: %v", state, err)
			}
		}
		return sw.ID
	}
	switchRow := func(id int64) (status, state, message string, cleared *bool) {
		t.Helper()
		if err := a.DB.Pool.QueryRow(ctx, `SELECT status, wipe_state, message, characters_cleared FROM map_rotation_switches WHERE id=$1`, id).Scan(&status, &state, &message, &cleared); err != nil {
			t.Fatal(err)
		}
		return
	}
	save(true) // clears the stop
	w.remote.restartDead = false
	w.remote.files[mrPlayersDB] = []byte("characters")
	id := crash(repository.MapWipeStopRequested)
	// The server is stopped at most once per switch.
	if err := a.MapRotation.SetWipeState(ctx, w.f.InstallationID, id, repository.MapWipeStopRequested, time.Now().UTC()); !errors.Is(err, repository.ErrMapSwitchNotPending) {
		t.Fatalf("a second stop of the same switch must be refused: %v", err)
	}
	w.remote.status = "stopped"
	save(false) // the owner switches the rotation off while the server is down
	calls, alerts := len(w.remote.serverCalls), len(w.alerts)

	// The ordinary pass does nothing for it (the rotation is off, and the switch is not its to finish).
	w.tick(time.Now().UTC())
	if status, state, _, _ := switchRow(id); status != repository.MapSwitchPending || state != repository.MapWipeStopRequested || len(w.remote.serverCalls) != calls {
		t.Fatalf("the ordinary pass touched an interrupted clearing: %s %s %v", status, state, w.remote.serverCalls[calls:])
	}
	a.mapRotationWipeRecover(ctx, time.Now().UTC())
	if got := strings.Join(w.remote.serverCalls[calls:], ", "); got != "restart" || w.remote.status != "restarting" {
		t.Fatalf("recovery must only start the server: %q, status %s", got, w.remote.status)
	}
	status, state, message, cleared := switchRow(id)
	if status != repository.MapSwitchApplied || state != repository.MapWipeFailed || cleared == nil || *cleared ||
		message != "The map's files were written and verified. Saved characters could not be cleared: "+charwipe.ReasonInterrupted+"." {
		t.Fatalf("recovered switch: %s %s %q", status, state, message)
	}
	if _, there := w.remote.files[mrPlayersDB]; !there {
		t.Fatal("recovery deleted the file")
	}
	if len(w.alerts) != alerts+1 || w.alerts[alerts].Severity != discord.AlertWarning {
		t.Fatalf("recovery alert: %+v", w.alerts[alerts:])
	}
	var leaseUntil *time.Time
	_ = a.DB.Pool.QueryRow(ctx, `SELECT lease_until FROM map_rotation_settings WHERE installation_id=$1`, w.f.InstallationID).Scan(&leaseUntil)
	if leaseUntil != nil {
		t.Fatal("recovery kept the lease")
	}

	// --- a crash after the file was verified deleted ------------------------------------------
	id = crash(repository.MapWipeStopRequested, repository.MapWipeDeleted)
	w.remote.status = "stopped"
	calls, alerts = len(w.remote.serverCalls), len(w.alerts)
	a.mapRotationWipeRecover(ctx, time.Now().UTC())
	status, state, message, cleared = switchRow(id)
	if got := strings.Join(w.remote.serverCalls[calls:], ", "); got != "restart" || status != repository.MapSwitchApplied || state != repository.MapWipeDone || cleared == nil || !*cleared ||
		message != "The map's files were written and verified. Saved characters were cleared, so everyone spawns fresh." || len(w.alerts) != alerts {
		t.Fatalf("crash after the delete: calls %q, %s %s %q", got, status, state, message)
	}

	// --- a crash after the restart was requested, and the server never comes up ------------------
	id = crash(repository.MapWipeStopRequested, repository.MapWipeDeleted, repository.MapWipeStartRequested)
	w.remote.status, w.remote.restartDead = "stopped", true
	alerts = len(w.alerts)
	a.mapRotationWipeRecover(ctx, time.Now().UTC())
	status, state, message, _ = switchRow(id)
	_ = a.DB.Pool.QueryRow(ctx, `SELECT halted_reason FROM map_rotation_settings WHERE installation_id=$1`, w.f.InstallationID).Scan(&halted)
	if status != repository.MapSwitchApplied || state != repository.MapWipeFailed || !strings.Contains(message, charwipe.ServerDownMessage) || halted != charwipe.ServerDownMessage ||
		len(w.alerts) != alerts+1 || w.alerts[alerts].Severity != discord.AlertCritical {
		t.Fatalf("recovery with the server down: %s %s %q halted %q alerts %+v", status, state, message, halted, w.alerts[alerts:])
	}
	// Nothing is left to recover.
	if left, err := a.MapRotation.WipesInProgress(ctx); err != nil || len(left) != 0 {
		t.Fatalf("clearings still in progress: %v %v", left, err)
	}
}
