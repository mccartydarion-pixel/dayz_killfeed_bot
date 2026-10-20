package stadiumwrite

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/nitrado/nitradofixture"
	"github.com/yourname/dayz-killfeed/internal/stadium"
)

// Every test drives the real nitrado.Client (discovery, listing, signed download, upload token,
// transfer and mkdir) against the Nitrado fixture with its synthetic mission folder.

const (
	svc          = "90000001"
	liveGameplay = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/The_Lost_City.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"
	wantGameplay = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/The_Lost_City.json\",\n\t\t\t\"custom/champion_stadium.json\"\n\t\t],\n\t\t\"x\": 1.50\n\t}\n}\n"
)

type world struct {
	fx     *nitradofixture.Server
	client *nitrado.Client
}

func newWorld(t *testing.T, gameplay []byte, custom bool) *world {
	t.Helper()
	prev := nitrado.AllowInsecureUploadURL
	nitrado.AllowInsecureUploadURL = true // the fixture is plain http on loopback
	t.Cleanup(func() { nitrado.AllowInsecureUploadURL = prev })
	fx := nitradofixture.New(90000001, "")
	fx.EnableMission(gameplay)
	if custom {
		fx.AddMissionDir("custom")
		fx.SetMissionFile("custom/The_Lost_City.json", []byte(`{"Objects":[]}`))
	}
	fx.AllowMissionWrites(true)
	srv := httptest.NewServer(fx)
	t.Cleanup(srv.Close)
	return &world{fx: fx, client: nitrado.NewClient(srv.URL, "fixture-token-not-a-credential", nil)}
}

func payload(t *testing.T) ([]byte, int) {
	t.Helper()
	p := stadium.Defaults()
	p.MapKey, p.CenterX, p.CenterZ, p.AltitudeY = "chernarusplus", 4618, 10439, 339.2
	l, err := stadium.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	return stadium.Render(l.Objects), len(l.Objects)
}

func (w *world) file(t *testing.T, rel string) string {
	t.Helper()
	b, _ := w.fx.MissionFile(rel)
	return string(b)
}

func stepResult(o Outcome, name string) string {
	for _, s := range o.Steps {
		if s.Name == name {
			return s.Result
		}
	}
	return "missing"
}

func TestApplyBuildsAndIsIdempotent(t *testing.T) {
	w := newWorld(t, []byte(liveGameplay), true)
	file, n := payload(t)
	req := Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n}
	out := Apply(context.Background(), w.client, req)
	if out.Status != StatusBuilt || !out.FileVerified || !out.Referenced || !out.RequiresRestart || out.ObjectCount != n {
		t.Fatalf("outcome: %+v", out)
	}
	if got := w.file(t, "custom/champion_stadium.json"); got != string(file) {
		t.Fatalf("stadium file:\n%s", got)
	}
	if got := w.file(t, "cfggameplay.json"); got != wantGameplay {
		t.Fatalf("cfggameplay.json:\n%s", got)
	}
	if out.SHA256 != stadium.SHA256(file) || out.ConfigSHA256 != stadium.SHA256([]byte(wantGameplay)) || out.File != "custom/champion_stadium.json" {
		t.Fatalf("digests: %+v", out)
	}
	// The verified backup holds the original configuration, outside custom/.
	if out.ConfigBackup != "champion/backup/cfggameplay.json."+stadium.SHA256([]byte(liveGameplay))[:12]+".bak" || w.file(t, out.ConfigBackup) != liveGameplay {
		t.Fatalf("backup %q: %q", out.ConfigBackup, w.file(t, out.ConfigBackup))
	}
	// Exactly these writes, in this order: the stadium file, the two backup folders, the backup,
	// the configuration. The owner's map file is untouched.
	if got := strings.Join(w.fx.MissionWrites(), ", "); got != "upload custom/champion_stadium.json, mkdir champion, mkdir champion/backup, upload champion/backup/"+
		"cfggameplay.json."+stadium.SHA256([]byte(liveGameplay))[:12]+".bak, upload cfggameplay.json" || out.Writes != 5 {
		t.Fatalf("writes (%d): %s", out.Writes, got)
	}
	if w.file(t, "custom/The_Lost_City.json") != `{"Objects":[]}` || w.fx.Snapshot().RefusedWrites != 0 {
		t.Fatal("another file changed or a write was refused")
	}
	for _, name := range []string{"find the mission folder", "read cfggameplay.json", "write the stadium file", "back up cfggameplay.json", "reference the file in cfggameplay.json", "no restart during the build"} {
		if r := stepResult(out, name); r != ResultPass {
			t.Fatalf("step %q: %s (%+v)", name, r, out.Steps)
		}
	}
	// Nothing secret in the outcome.
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"fixture-upload-token", "_upload", "fixture-token-not-a-credential", "/games/fixture", "objectSpawnersArr\": ["} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q leaked into the outcome: %s", secret, raw)
		}
	}

	// A second build changes nothing: no write request at all.
	again := Apply(context.Background(), w.client, req)
	if again.Status != StatusBuilt || again.Writes != 0 || len(w.fx.MissionWrites()) != 5 {
		t.Fatalf("second build: %+v", again)
	}
	if stepResult(again, "write the stadium file") != ResultSkipped || stepResult(again, "reference the file in cfggameplay.json") != ResultSkipped {
		t.Fatalf("second build steps: %+v", again.Steps)
	}
}

func TestApplyCreatesTheCustomFolder(t *testing.T) {
	w := newWorld(t, []byte(liveGameplay), false)
	file, n := payload(t)
	out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n})
	if out.Status != StatusBuilt {
		t.Fatalf("outcome: %+v", out)
	}
	if !w.fx.MissionDirExists("custom") || w.file(t, "custom/champion_stadium.json") != string(file) {
		t.Fatal("custom/ or the file is missing")
	}
	if got := w.fx.MissionWrites(); got[0] != "mkdir custom" || stepResult(out, "create the custom folder") != ResultPass {
		t.Fatalf("writes: %v", got)
	}
	// A failed mkdir stops everything before any upload.
	w2 := newWorld(t, []byte(liveGameplay), false)
	w2.fx.MkdirStatus(500)
	out = Apply(context.Background(), w2.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n})
	if out.Status != StatusFailed || !strings.Contains(out.Message, "custom folder could not be created") || w2.file(t, "cfggameplay.json") != liveGameplay {
		t.Fatalf("outcome: %+v", out)
	}
	for _, wr := range w2.fx.MissionWrites() {
		if strings.HasPrefix(wr, "upload") {
			t.Fatalf("an upload was sent after a failed mkdir: %v", w2.fx.MissionWrites())
		}
	}
}

func TestApplyFailsClearlyWithoutCfgGameplay(t *testing.T) {
	w := newWorld(t, nil, true)
	file, n := payload(t)
	out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n})
	if out.Status != StatusFailed || !strings.Contains(out.Message, "Enable cfggameplay.json") || out.Writes != 0 || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("outcome: %+v", out)
	}
	// A configuration that cannot be edited safely is refused the same way, before any write.
	w = newWorld(t, []byte(`{"WorldsData": {"objectSpawnersArr": [1, 2]}}`), true)
	out = Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n})
	if out.Status != StatusFailed || !strings.Contains(out.Message, "could not be edited safely") || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestApplyReadBackDecides(t *testing.T) {
	file, n := payload(t)
	req := Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n}

	// A partial transfer: the read-back is neither the old nor the new content -> UNCERTAIN, and
	// cfggameplay.json is never touched.
	w := newWorld(t, []byte(liveGameplay), true)
	w.fx.TransferMode(1, nitradofixture.TransferPartial)
	out := Apply(context.Background(), w.client, req)
	if out.Status != StatusUncertain || out.FileVerified || out.Referenced || !strings.Contains(out.Message, "neither the old nor the new") {
		t.Fatalf("partial: %+v", out)
	}
	if w.file(t, "cfggameplay.json") != liveGameplay || len(w.fx.MissionWrites()) != 1 {
		t.Fatalf("config touched or more writes: %v", w.fx.MissionWrites())
	}

	// A refused transfer: the file is proven unchanged -> FAILED, nothing else written.
	w = newWorld(t, []byte(liveGameplay), true)
	w.fx.TransferMode(1, nitradofixture.TransferRefuse)
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusFailed || !strings.Contains(out.Message, "refused the upload") || out.Writes != 1 {
		t.Fatalf("refused: %+v", out)
	}
	if _, ok := w.fx.MissionFile("custom/champion_stadium.json"); ok {
		t.Fatal("a refused transfer created the file")
	}

	// Reported success without a change -> UNCERTAIN.
	w = newWorld(t, []byte(liveGameplay), true)
	w.fx.TransferMode(1, nitradofixture.TransferClaim)
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusUncertain || !strings.Contains(out.Message, "reported success") {
		t.Fatalf("claim: %+v", out)
	}

	// The configuration write is refused: the stadium file is in place but unreferenced, the
	// outcome says so, and the next build only references it.
	w = newWorld(t, []byte(liveGameplay), true)
	w.fx.TransferMode(3, nitradofixture.TransferRefuse) // 1 = stadium file, 2 = backup, 3 = config
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusFailed || !out.FileVerified || out.Referenced || !strings.Contains(out.Message, "not referenced") {
		t.Fatalf("config refused: %+v", out)
	}
	if w.file(t, "cfggameplay.json") != liveGameplay || w.file(t, "custom/champion_stadium.json") != string(file) {
		t.Fatal("unexpected files after the refused config write")
	}
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusBuilt || out.Writes != 1 || stepResult(out, "write the stadium file") != ResultSkipped || stepResult(out, "back up cfggameplay.json") != ResultPass {
		t.Fatalf("second build: %+v", out)
	}
	if w.file(t, "cfggameplay.json") != wantGameplay {
		t.Fatalf("cfggameplay.json:\n%s", w.file(t, "cfggameplay.json"))
	}

	// A partial configuration write -> UNCERTAIN, naming the backup to restore.
	w = newWorld(t, []byte(liveGameplay), true)
	w.fx.TransferMode(3, nitradofixture.TransferPartial)
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusUncertain || !strings.Contains(out.Message, "Restore it from "+out.ConfigBackup) {
		t.Fatalf("config partial: %+v", out)
	}
	if w.file(t, out.ConfigBackup) != liveGameplay {
		t.Fatal("the backup must hold the original configuration")
	}
}

func TestApplyRefusesWhenTheServerChangesBeforeTheWrite(t *testing.T) {
	file, n := payload(t)
	req := Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n}
	// cfggameplay.json is edited by somebody between the inspection and the pre-write re-read.
	w := newWorld(t, []byte(liveGameplay), true)
	reads := 0
	w.fx.OnDownload(func(rel string) {
		if rel == "cfggameplay.json" {
			reads++
			if reads == 2 {
				w.fx.SetMissionFile("cfggameplay.json", []byte(strings.Replace(liveGameplay, "1.50", "1.75", 1)))
			}
		}
	})
	out := Apply(context.Background(), w.client, req)
	if out.Status != StatusFailed || out.Message != msgChanged || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("changed config: %+v (writes %v)", out, w.fx.MissionWrites())
	}
	// The server restarts (a new boot log appears) between the inspection and the write.
	w = newWorld(t, []byte(liveGameplay), true)
	reads = 0
	w.fx.OnDownload(func(rel string) {
		if rel == "cfggameplay.json" {
			reads++
			if reads == 2 {
				w.fx.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
				w.fx.Restart()
			}
		}
	})
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusFailed || out.Message != msgChanged || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("restart: %+v (writes %v)", out, w.fx.MissionWrites())
	}
	// A file somebody else wrote at the stadium's path is never overwritten.
	w = newWorld(t, []byte(liveGameplay), true)
	w.fx.SetMissionFile("custom/champion_stadium.json", []byte(`{"Objects":[{"name":"Land_Wreck_Uaz","pos":[1,2,3],"ypr":[0,0,0],"scale":1,"enableCEPersistency":false,"customString":"theirs"}]}`))
	out = Apply(context.Background(), w.client, req)
	if out.Status != StatusFailed || !strings.Contains(out.Message, "did not write") || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("foreign: %+v", out)
	}
}

func TestApplyRemoveWritesTheEmptyFileAndKeepsTheReference(t *testing.T) {
	w := newWorld(t, []byte(liveGameplay), true)
	file, n := payload(t)
	if out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n}); out.Status != StatusBuilt {
		t.Fatalf("build: %+v", out)
	}
	out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: stadium.EmptyFile(), Reference: false})
	if out.Status != StatusRemoved || !out.FileVerified || !out.Referenced || out.Writes != 1 || out.ObjectCount != 0 {
		t.Fatalf("remove: %+v", out)
	}
	if !bytes.Equal([]byte(w.file(t, "custom/champion_stadium.json")), stadium.EmptyFile()) || w.file(t, "cfggameplay.json") != wantGameplay {
		t.Fatal("remove must empty the file and keep the configuration")
	}
	// Removing with no custom folder at all is nothing to do.
	w2 := newWorld(t, []byte(liveGameplay), false)
	out = Apply(context.Background(), w2.client, Request{ServiceID: svc, Payload: stadium.EmptyFile(), Reference: false})
	if out.Status != StatusRemoved || out.Writes != 0 || out.Referenced || w2.fx.MissionDirExists("custom") {
		t.Fatalf("remove without custom: %+v", out)
	}
}

func TestApplyWithAnAlreadyReferencedFileTakesNoBackup(t *testing.T) {
	w := newWorld(t, []byte(wantGameplay), true)
	file, n := payload(t)
	out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: file, Reference: true, ObjectCount: n})
	if out.Status != StatusBuilt || out.Writes != 1 || out.ConfigBackup != "" || out.ConfigSHA256 != stadium.SHA256([]byte(wantGameplay)) {
		t.Fatalf("outcome: %+v", out)
	}
	if w.fx.MissionDirExists("champion") {
		t.Fatal("no backup folder is needed when nothing is written to the configuration")
	}
}

func TestApplyRefusesBadInput(t *testing.T) {
	w := newWorld(t, []byte(liveGameplay), true)
	if out := Apply(context.Background(), nil, Request{ServiceID: svc, Payload: stadium.EmptyFile()}); out.Status != StatusFailed {
		t.Fatalf("nil remote: %+v", out)
	}
	if out := Apply(context.Background(), w.client, Request{ServiceID: svc, Payload: []byte("{oops")}); out.Status != StatusFailed || len(w.fx.MissionWrites()) != 0 {
		t.Fatalf("bad payload: %+v", out)
	}
	if out := Apply(context.Background(), w.client, Request{ServiceID: "", Payload: stadium.EmptyFile()}); out.Status != StatusFailed {
		t.Fatalf("no service: %+v", out)
	}
}
