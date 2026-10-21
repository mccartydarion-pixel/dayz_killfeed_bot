//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/stadium"
)

// The Stadium API (docs/STADIUM.md) over the real routes and a real PostgreSQL. The game server
// is the Nitrado fixture with its synthetic mission folder, driven through the real Nitrado client:
// the build really uploads the file, the backup and the configuration and reads them back.

const stadiumGameplay = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/The_Lost_City.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"

type stadiumWorld struct {
	*factionWorld
	fx     *nitradofixture.Server
	client *nitrado.Client
	owner  string
}

func newStadiumWorld(t *testing.T) *stadiumWorld {
	t.Helper()
	w := newFactionWorld(t)
	a := w.a
	pool := a.DB.Pool
	a.Stadium = repository.NewStadiumRepository(pool)
	a.ClientAdmin = repository.NewClientAdminRepository(pool)
	a.AdminAudit = repository.NewAuditRepository(pool)
	a.registerStadiumRoutes()

	prev := nitrado.AllowInsecureUploadURL
	nitrado.AllowInsecureUploadURL = true // the fixture is plain http on loopback
	t.Cleanup(func() { nitrado.AllowInsecureUploadURL = prev })
	fx := nitradofixture.New(90000001, "")
	fx.EnableMission([]byte(stadiumGameplay))
	fx.AddMissionDir("custom")
	fx.AllowMissionWrites(true)
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	sw := &stadiumWorld{factionWorld: w, fx: fx, client: nitrado.NewClient(srv.URL, "fixture-token-not-a-credential", nil), owner: w.a1.OwnerDiscordID}
	a.stadiumRemoteFor = func(_ context.Context, t repository.StadiumTarget) (stadiumRemote, error) {
		if t.NitradoServiceID != "90000001" {
			return nil, fmt.Errorf("unexpected service %q", t.NitradoServiceID)
		}
		return sw.client, nil
	}
	// The installation's server is the fixture's service.
	_, server := w.gameContext(w.a1)
	if _, err := pool.Exec(context.Background(), `UPDATE game_servers SET provider_service_id='90000001' WHERE id=$1`, server); err != nil {
		t.Fatal(err)
	}
	return sw
}

func (w *stadiumWorld) path(f installationFixture, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/stadium%s", f.OrgID, f.InstallationID, suffix)
}

func stadiumOf(t *testing.T, r *apiResult) map[string]any {
	t.Helper()
	s, _ := r.JSON(t)["stadium"].(map[string]any)
	if s == nil {
		t.Fatalf("no stadium in the response: %s", r.Body)
	}
	return s
}

func (w *stadiumWorld) params(extra map[string]any) map[string]any {
	p := map[string]any{"centerX": 4618, "centerZ": 10439, "altitudeY": 339.2, "altitudeSource": "MANUAL"}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func (w *stadiumWorld) insertPosition(f installationFixture, player int64, name string, x, z float64, y *float64, at time.Time) {
	w.t.Helper()
	guild, server := w.gameContext(f)
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_location_events(guild_id, server_id, player_id, gamertag, x, z, y, event_type, observed_at, source)
VALUES($1,$2,$3,$4,$5,$6,$7,'PLAYER_LIST',$8,'ADM')`, guild, server, player, name, x, z, y, at); err != nil {
		w.t.Fatal(err)
	}
}

func TestStadiumAPIAuthorizationAndDraft(t *testing.T) {
	w := newStadiumWorld(t)
	f := w.a1

	// The chain: service auth, acting user, OWNER/ADMIN of the organization, the installation.
	r := w.do(http.MethodGet, w.path(f, ""), "", nil)
	if r.Status != http.StatusUnauthorized {
		t.Fatalf("no acting user: %d %s", r.Status, r.Body)
	}
	for who, actor := range map[string]string{"member": w.member, "player": w.players[0], "owner of another organization": w.b1.OwnerDiscordID} {
		if r := w.do(http.MethodGet, w.path(f, ""), actor, nil); r.Status != http.StatusForbidden {
			t.Fatalf("%s: %d %s", who, r.Status, r.Body)
		}
	}
	if r := w.do(http.MethodGet, w.path(installationFixture{OrgID: f.OrgID, InstallationID: w.b1.InstallationID}, ""), w.owner, nil); r.Status != http.StatusNotFound {
		t.Fatalf("another organization's installation: %d %s", r.Status, r.Body)
	}
	s := stadiumOf(t, w.expect(w.do(http.MethodGet, w.path(f, ""), w.owner, nil), http.StatusOK, "get"))
	if s["status"] != "NONE" || s["params"] != nil || s["preview"] != nil || s["build"] != nil {
		t.Fatalf("empty stadium: %v", s)
	}
	if r := w.do(http.MethodGet, w.path(f, "/file"), w.owner, nil); r.Status != http.StatusNotFound {
		t.Fatalf("file without a configuration: %d", r.Status)
	}
	if r := w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil); r.Status != http.StatusConflict {
		t.Fatalf("build without a configuration: %d %s", r.Status, r.Body)
	}

	// Validation errors are 400 with a plain message.
	for name, body := range map[string]any{
		"no centre":     map[string]any{"altitudeY": 1},
		"unknown field": w.params(map[string]any{"y": 3}),
		"bad size":      w.params(map[string]any{"size": "HUGE"}),
		"outside map":   w.params(map[string]any{"centerX": 10}),
		"not json":      "{",
	} {
		r := w.do(http.MethodPut, w.path(f, ""), w.owner, body)
		if r.Status != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, r.Status, r.Body)
		}
		if msg, _ := r.JSON(t)["error"].(map[string]any)["message"].(string); msg == "" || strings.Contains(msg, "invalid stadium parameters:") {
			t.Fatalf("%s: message %q", name, msg)
		}
	}
	// The admin (ADMIN role) may save; the preview comes back with the defaults filled in.
	r = w.expect(w.do(http.MethodPut, w.path(f, ""), w.admin, w.params(nil)), http.StatusOK, "put")
	s = stadiumOf(t, r)
	params, _ := s["params"].(map[string]any)
	preview, _ := s["preview"].(map[string]any)
	if s["status"] != "DRAFT" || params["size"] != "MEDIUM" || params["cover"] != "LIGHT" || params["lockerKit"] != "M4_ONLY" || params["clothingKit"] != "RED_BLUE_CORNERS" || params["mapKey"] != "" {
		t.Fatalf("draft: %v", s)
	}
	if preview["objectCount"] != float64(115) || len(preview["zones"].([]any)) < 10 || len(preview["footprint"].([]any)) != 4 {
		t.Fatalf("preview: %v", preview)
	}
	if warnings := preview["warnings"].([]any); len(warnings) != 1 || !strings.Contains(warnings[0].(string), "no map is configured") {
		t.Fatalf("warnings: %v", warnings)
	}
	if s["build"] != nil || s["dirty"] != false {
		t.Fatalf("draft build block: %v", s)
	}
	// With the installation's map configured the key is filled in and the warning goes.
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO shop_delivery_settings(installation_id, organization_id, map_key) VALUES($1,$2,'chernarusplus')`, f.InstallationID, f.OrgID); err != nil {
		t.Fatal(err)
	}
	s = stadiumOf(t, w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"size": "small", "extras": map[string]any{"flags": false}})), http.StatusOK, "put 2"))
	params, preview = s["params"].(map[string]any), s["preview"].(map[string]any)
	if params["mapKey"] != "chernarusplus" || params["size"] != "SMALL" || len(preview["warnings"].([]any)) != 0 || preview["objectCount"] != float64(113) {
		t.Fatalf("second draft: %v", s)
	}
	if extras := params["extras"].(map[string]any); extras["flags"] != false || extras["stairs"] != true {
		t.Fatalf("extras: %v", extras)
	}

	// The file download is the rendered layout.
	r = w.expect(w.do(http.MethodGet, w.path(f, "/file"), w.owner, nil), http.StatusOK, "file")
	if !strings.HasPrefix(r.ContentType, "application/json") {
		t.Fatalf("content type %q", r.ContentType)
	}
	var file stadium.File
	if err := json.Unmarshal(r.Body, &file); err != nil || len(file.Objects) != 113 || file.Objects[0].Name != stadium.ClassWall {
		t.Fatalf("file: %v %d", err, len(file.Objects))
	}
	req, _ := http.NewRequest(http.MethodGet, w.base+w.path(f, "/file"), nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set(actingUserHeader, w.owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="champion_stadium.json"` {
		t.Fatalf("content disposition %q", cd)
	}
}

func TestStadiumPositionLookup(t *testing.T) {
	w := newStadiumWorld(t)
	f := w.a1
	now := time.Now().UTC().Truncate(time.Second)
	// The owner has no linked player yet.
	r := w.do(http.MethodPost, w.path(f, "/position"), w.owner, nil)
	if r.Status != http.StatusNotFound || r.errCode(t) != codeNotFound || !strings.Contains(r.JSON(t)["error"].(map[string]any)["message"].(string), "no recent position") {
		t.Fatalf("unlinked: %d %s", r.Status, r.Body)
	}
	player := w.linkPlayer(f, w.owner, "Arena Owner")
	if r := w.do(http.MethodPost, w.path(f, "/position"), w.owner, nil); r.Status != http.StatusNotFound {
		t.Fatalf("no position yet: %d %s", r.Status, r.Body)
	}
	// An old position and one without altitude never count; the newest with an altitude wins.
	alt1, alt2 := 338.7, 339.2
	w.insertPosition(f, player, "Arena Owner", 100, 200, &alt1, now.Add(-25*time.Hour))
	if r := w.do(http.MethodPost, w.path(f, "/position"), w.owner, nil); r.Status != http.StatusNotFound {
		t.Fatalf("stale: %d %s", r.Status, r.Body)
	}
	w.insertPosition(f, player, "Arena Owner", 4618.5, 10439.25, &alt2, now.Add(-10*time.Minute))
	w.insertPosition(f, player, "Arena Owner", 4700, 10500, nil, now.Add(-5*time.Minute))
	r = w.expect(w.do(http.MethodPost, w.path(f, "/position"), w.owner, nil), http.StatusOK, "position")
	pos := r.JSON(t)["position"].(map[string]any)
	if pos["x"] != 4618.5 || pos["z"] != 10439.25 || pos["altitudeY"] != 339.2 || pos["playerName"] != "Arena Owner" || pos["source"] != "ADM" || pos["observedAt"] != now.Add(-10*time.Minute).Format(time.RFC3339) {
		t.Fatalf("position: %v", pos)
	}
	// A named player of this server, case-insensitively; a player of another server is not found.
	other := w.linkPlayer(f, w.players[1], "Ref Rita")
	alt3 := 12.5
	w.insertPosition(f, other, "Ref Rita", 1000, 2000, &alt3, now.Add(-time.Minute))
	r = w.expect(w.do(http.MethodPost, w.path(f, "/position"), w.owner, map[string]any{"playerName": "ref rita"}), http.StatusOK, "named")
	if pos := r.JSON(t)["position"].(map[string]any); pos["altitudeY"] != 12.5 || pos["playerName"] != "Ref Rita" {
		t.Fatalf("named position: %v", pos)
	}
	if r := w.do(http.MethodPost, w.path(f, "/position"), w.owner, map[string]any{"playerName": "Nobody"}); r.Status != http.StatusNotFound {
		t.Fatalf("unknown name: %d", r.Status)
	}
	stranger := w.linkPlayer(w.b1, w.players[2], "Elsewhere Eve")
	w.insertPosition(w.b1, stranger, "Elsewhere Eve", 10, 20, &alt3, now)
	if r := w.do(http.MethodPost, w.path(f, "/position"), w.owner, map[string]any{"playerName": "Elsewhere Eve"}); r.Status != http.StatusNotFound {
		t.Fatalf("player of another server: %d", r.Status)
	}
	if r := w.do(http.MethodPost, w.path(f, "/position"), w.member, nil); r.Status != http.StatusForbidden {
		t.Fatalf("member: %d", r.Status)
	}
}

func TestStadiumBuildWritesVerifiesAndRemoves(t *testing.T) {
	w := newStadiumWorld(t)
	f := w.a1
	w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"mapKey": "chernarusplus"})), http.StatusOK, "put")

	// A member cannot build; neither can a platform admin viewing as the owner (read-only).
	if r := w.do(http.MethodPost, w.path(f, "/build"), w.member, nil); r.Status != http.StatusForbidden {
		t.Fatalf("member build: %d %s", r.Status, r.Body)
	}
	viewAs := func(method, suffix string) *apiResult {
		req, _ := http.NewRequest(method, w.base+w.path(f, suffix), nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set(actingUserHeader, w.owner)
		req.Header.Set(impersonatorHeader, adminFounderID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := &apiResult{Status: resp.StatusCode}
		_ = json.NewDecoder(resp.Body).Decode(&out.m)
		return out
	}
	if r := viewAs(http.MethodPost, "/build"); r.Status != http.StatusForbidden {
		t.Fatalf("view-as build: %d", r.Status)
	}
	if r := viewAs(http.MethodGet, ""); r.Status != http.StatusOK {
		t.Fatalf("view-as read: %d", r.Status)
	}
	if len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("writes before any approved build: %v", w.fx.MissionWrites())
	}

	// The owner's Build is the approval: the file, the verified backup and the reference.
	r := w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "build")
	outcome := r.JSON(t)["outcome"].(map[string]any)
	if outcome["status"] != "BUILT" || outcome["referenced"] != true || outcome["fileVerified"] != true || outcome["requiresRestart"] != true || outcome["objectCount"] != float64(115) || outcome["file"] != "custom/champion_stadium.json" {
		t.Fatalf("outcome: %v", outcome)
	}
	got, ok := w.fx.MissionFile("custom/champion_stadium.json")
	if !ok || stadium.SHA256(got) != outcome["sha256"] {
		t.Fatalf("file on the server: %v", ok)
	}
	var file stadium.File
	if err := json.Unmarshal(got, &file); err != nil || len(file.Objects) != 115 {
		t.Fatalf("server file: %v %d", err, len(file.Objects))
	}
	cfg, _ := w.fx.MissionFile("cfggameplay.json")
	if !strings.Contains(string(cfg), `"custom/The_Lost_City.json",`+"\n\t\t\t"+`"custom/champion_stadium.json"`) {
		t.Fatalf("cfggameplay.json:\n%s", cfg)
	}
	backup, _ := w.fx.MissionFile(outcome["configBackup"].(string))
	if string(backup) != stadiumGameplay {
		t.Fatalf("backup: %q", backup)
	}
	s := stadiumOf(t, r)
	build := s["build"].(map[string]any)
	if s["status"] != "BUILT" || s["dirty"] != false || build["referenced"] != true || build["fileSha256"] != outcome["sha256"] || build["objectCount"] != float64(115) || build["builtAt"] == nil || build["lastOutcome"].(map[string]any)["status"] != "BUILT" {
		t.Fatalf("stadium after the build: %v", s)
	}
	// It is recorded for the next GET too, and audited.
	s = stadiumOf(t, w.expect(w.do(http.MethodGet, w.path(f, ""), w.owner, nil), http.StatusOK, "get"))
	if s["status"] != "BUILT" || s["build"].(map[string]any)["lastOutcome"] == nil {
		t.Fatalf("persisted: %v", s)
	}
	var audits int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='STADIUM_BUILD' AND result='built'`, f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows: %d %v", audits, err)
	}

	// Changed params keep the stadium BUILT but dirty; the download is the new layout.
	s = stadiumOf(t, w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"mapKey": "chernarusplus", "size": "LARGE"})), http.StatusOK, "put 2"))
	if s["status"] != "BUILT" || s["dirty"] != true || s["preview"].(map[string]any)["objectCount"] != float64(125) || s["build"].(map[string]any)["objectCount"] != float64(115) {
		t.Fatalf("dirty: %v", s)
	}
	r = w.expect(w.do(http.MethodGet, w.path(f, "/file"), w.owner, nil), http.StatusOK, "file")
	if err := json.Unmarshal(r.Body, &file); err != nil || len(file.Objects) != 125 {
		t.Fatalf("download: %v %d", err, len(file.Objects))
	}
	// A rebuild writes only the file (the reference is already there) and clears dirty.
	writes := len(w.fx.MissionWrites())
	r = w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "rebuild")
	if o := r.JSON(t)["outcome"].(map[string]any); o["status"] != "BUILT" || o["writes"] != float64(1) || len(w.fx.MissionWrites()) != writes+1 {
		t.Fatalf("rebuild: %v", o)
	}
	if s := stadiumOf(t, r); s["dirty"] != false || s["build"].(map[string]any)["objectCount"] != float64(125) {
		t.Fatalf("after the rebuild: %v", s)
	}

	// A read-back mismatch is refused as UNCERTAIN and recorded; the stadium's status is kept.
	w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"mapKey": "chernarusplus", "size": "SMALL"})), http.StatusOK, "put 3")
	w.fx.TransferMode(w.fx.Transfers()+1, nitradofixture.TransferPartial)
	r = w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "mismatch")
	if o := r.JSON(t)["outcome"].(map[string]any); o["status"] != "UNCERTAIN" || o["fileVerified"] != false || !strings.Contains(o["message"].(string), "neither the old nor the new") {
		t.Fatalf("mismatch outcome: %v", o)
	}
	if s := stadiumOf(t, r); s["status"] != "BUILT" || s["build"].(map[string]any)["lastOutcome"].(map[string]any)["status"] != "UNCERTAIN" {
		t.Fatalf("after the mismatch: %v", s)
	}
	// The next build replaces the half-written file.
	r = w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "repair")
	if o := r.JSON(t)["outcome"].(map[string]any); o["status"] != "BUILT" || o["objectCount"] != float64(117) {
		t.Fatalf("repair: %v", o)
	}

	// Remove empties the file and keeps the reference.
	r = w.expect(w.do(http.MethodPost, w.path(f, "/remove"), w.owner, nil), http.StatusOK, "remove")
	if o := r.JSON(t)["outcome"].(map[string]any); o["status"] != "REMOVED" || o["referenced"] != true || o["objectCount"] != float64(0) {
		t.Fatalf("remove: %v", o)
	}
	if got, _ := w.fx.MissionFile("custom/champion_stadium.json"); string(got) != string(stadium.EmptyFile()) {
		t.Fatalf("file after remove: %q", got)
	}
	if cfg, _ := w.fx.MissionFile("cfggameplay.json"); !strings.Contains(string(cfg), "custom/champion_stadium.json") {
		t.Fatal("remove must keep the reference")
	}
	s = stadiumOf(t, r)
	if s["status"] != "REMOVED" || s["removedAt"] == nil || s["build"].(map[string]any)["objectCount"] != float64(0) {
		t.Fatalf("after remove: %v", s)
	}
	// Saving again after a remove is a draft; building again brings the arena back.
	s = stadiumOf(t, w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"mapKey": "chernarusplus"})), http.StatusOK, "put 4"))
	if s["status"] != "DRAFT" {
		t.Fatalf("after remove + put: %v", s)
	}
	if o := w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "build again").JSON(t)["outcome"].(map[string]any); o["status"] != "BUILT" {
		t.Fatalf("build again: %v", o)
	}

	// Nitrado unavailable is a 503, not an outcome.
	w.a.stadiumRemoteFor = func(context.Context, repository.StadiumTarget) (stadiumRemote, error) { return nil, fmt.Errorf("down") }
	if r := w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil); r.Status != http.StatusServiceUnavailable || r.errCode(t) != codeNitradoUnavailable {
		t.Fatalf("nitrado down: %d %s", r.Status, r.Body)
	}
}

func TestStadiumBuildFailsClearlyWithoutCfgGameplay(t *testing.T) {
	w := newStadiumWorld(t)
	f := w.a1
	w.fx.RemoveMissionFile("cfggameplay.json")
	w.expect(w.do(http.MethodPut, w.path(f, ""), w.owner, w.params(map[string]any{"mapKey": "chernarusplus"})), http.StatusOK, "put")
	r := w.expect(w.do(http.MethodPost, w.path(f, "/build"), w.owner, nil), http.StatusOK, "build")
	o := r.JSON(t)["outcome"].(map[string]any)
	if o["status"] != "FAILED" || !strings.Contains(o["message"].(string), `tick "Enable cfggameplay.json"`) || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("outcome: %v", o)
	}
	if s := stadiumOf(t, r); s["status"] != "DRAFT" || s["build"].(map[string]any)["lastOutcome"].(map[string]any)["status"] != "FAILED" {
		t.Fatalf("stadium: %v", s)
	}
}
