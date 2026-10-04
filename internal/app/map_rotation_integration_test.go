//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
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

// fakeMapRemote is an in-memory Nitrado file server implementing mapRotationRemote.
type fakeMapRemote struct {
	mu          sync.Mutex
	files       map[string][]byte
	tasks       []nitrado.ScheduledTask
	pending     string // destination of the last upload token
	uploads     []string
	refuseAt    int // >0: exactly the n-th upload is refused
	uploadCount int
}

func newFakeMapRemote() *fakeMapRemote {
	return &fakeMapRemote{files: map[string][]byte{
		mrMission + "/cfggameplay.json":         []byte("{\n\t\"version\": 1,\n\t\"WorldsData\": {\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/shop.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"),
		mrMission + "/cfgplayerspawnpoints.xml": []byte(mrSpawnsA),
		mrCustom + "/arena_a.json":              []byte(`{"Objects":[]}`),
		mrCustom + "/arena_a.xml":               []byte(mrSpawnsA),
		mrCustom + "/arena_b.json":              []byte(`{"Objects":[]}`),
		mrCustom + "/arena_b.xml":               []byte(mrSpawnsB),
		mrCustom + "/arena_c.json":              []byte(`{"Objects":[]}`),
		mrCustom + "/arena_c.xml":               []byte(mrSpawnsC),
	}}
}

func (f *fakeMapRemote) GameserverFacts(context.Context, string) (nitrado.GameserverFacts, error) {
	return nitrado.GameserverFacts{Game: "dayzps", Status: "started", GamePath: mrRoot + "/noftp/dayzps", Mission: "dayzOffline.chernarusplus"}, nil
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
		{"name": "Arena A", "mapFile": "arena_a.json", "spawnFile": "arena_a.xml", "imageUrl": "https://cdn.example.com/a.png", "enabled": true},
		{"name": "Arena B", "mapFile": "arena_b.json", "spawnFile": "arena_b.xml", "imageUrl": nil, "enabled": true},
		{"name": "Arena C", "mapFile": "arena_c.json", "spawnFile": "arena_c.xml", "imageUrl": nil, "enabled": true},
	}
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
		"maps", "current", "next", "vote", "lastSwitch", "filesCheck"} {
		if _, ok := view[key]; !ok {
			t.Fatalf("the view has no %q", key)
		}
	}
	if view["everyRestarts"].(float64) != 1 || view["order"] != "SEQUENCE" || view["voteMinutesBeforeRestart"].(float64) != 30 || len(view["maps"].([]any)) != 0 ||
		view["vote"] != nil || view["lastSwitch"] != nil || len(view["filesCheck"].([]any)) != 0 {
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
		"six maps":            rotationBody(six, nil),
		"everyRestarts 4":     rotationBody(threeMaps(), func(b map[string]any) { b["everyRestarts"] = 4 }),
		"order":               rotationBody(threeMaps(), func(b map[string]any) { b["order"] = "SHUFFLE" }),
		"vote minutes 4":      rotationBody(threeMaps(), func(b map[string]any) { b["voteMinutesBeforeRestart"] = 4 }),
		"vote minutes 121":    rotationBody(threeMaps(), func(b map[string]any) { b["voteMinutesBeforeRestart"] = 121 }),
		"maps missing":        rotationBody(nil, func(b map[string]any) { delete(b, "maps") }),
		"one enabled map":     rotationBody(threeMaps()[:1], nil),
		"path in map file":    rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "../x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"folder in map file":  rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "custom/x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"wrong map extension": rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.xml", "spawnFile": "x.xml", "enabled": true}), nil),
		"wrong spawn ext":     rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.json", "enabled": true}), nil),
		"http image":          rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "imageUrl": "http://cdn.example.com/x.png", "enabled": true}), nil),
		"long image":          rotationBody(append(threeMaps()[:2], map[string]any{"name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "imageUrl": "https://cdn.example.com/" + strings.Repeat("x", 500), "enabled": true}), nil),
		"empty name":          rotationBody(append(threeMaps()[:2], map[string]any{"name": " ", "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"long name":           rotationBody(append(threeMaps()[:2], map[string]any{"name": strings.Repeat("n", 61), "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"unknown map id":      rotationBody(append(threeMaps()[:2], map[string]any{"id": 999999999, "name": "X", "mapFile": "x.json", "spawnFile": "x.xml", "enabled": true}), nil),
		"unknown channel":     rotationBody(threeMaps(), func(b map[string]any) { b["announceChannelId"] = "123456789012345678" }),
	} {
		if code, resp := w.admin(http.MethodPut, a.handleSaveMapRotation, "/map/rotation", owner, body); code != http.StatusBadRequest || errCode(resp) != codeValidationError {
			t.Fatalf("%s: %d %v", name, code, resp)
		}
	}
	if _, view = w.admin(http.MethodGet, a.handleAdminMapRotation, "/map/rotation", owner, nil); len(view["maps"].([]any)) != 0 || view["enabled"] != false {
		t.Fatalf("a refused save stored something: %v", view)
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

	// The file check lists the custom folder and writes nothing.
	delete(w.remote.files, mrCustom+"/arena_c.xml")
	code, view = w.admin(http.MethodPost, a.handleCheckMapRotation, "/map/rotation/check", owner, nil)
	checks := view["filesCheck"].([]any)
	if code != http.StatusOK || len(checks) != 2 {
		t.Fatalf("check: %d %v", code, view)
	}
	for _, raw := range checks {
		c := raw.(map[string]any)
		wantSpawn := c["mapId"] != ids[2]
		if c["mapFileFound"] != true || c["spawnFileFound"] != wantSpawn || c["checkedAt"] == nil {
			t.Fatalf("check row: %v", c)
		}
	}
	if len(w.remote.uploads) != 0 {
		t.Fatal("the file check wrote to the server")
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
	if again, created, _ := a.MapRotation.BeginSwitch(ctx, w.f.InstallationID, idB, "Arena B", "arena_b.json", "arena_b.xml", "ROTATION", "", now); created || again.ID != sw.ID || again.Attempts != 2 {
		t.Fatalf("a second switch was started: %+v", again)
	}
	w.remote.files[mrMission+"/cfggameplay.json"] = []byte(strings.Replace(original, "\"custom/shop.json\"", "\"custom/shop.json\",\n\t\t\t\"custom/arena_b.json\"", 1))

	w.tick(now.Add(time.Minute))
	if strings.Join(w.remote.uploads, " ") != mrMission+"/cfgplayerspawnpoints.xml" || w.remote.content(mrMission+"/cfgplayerspawnpoints.xml") != mrSpawnsB {
		t.Fatalf("the interrupted switch was not finished by writing only the missing file: %v", w.remote.uploads)
	}
	var status string
	var backup []byte
	_ = a.DB.Pool.QueryRow(ctx, `SELECT status, prev_gameplay FROM map_rotation_switches WHERE id=$1`, sw.ID).Scan(&status, &backup)
	if status != repository.MapSwitchApplied || string(backup) != original {
		t.Fatalf("resumed switch: %s", status)
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
