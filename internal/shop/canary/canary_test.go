package canary

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// liveShape reproduces the live Champions cfggameplay.json formatting around the spawner array
// (tabs, LF, and the empty array's closing line indented with eight spaces).
const liveShape = "{\n\t\"version\": 123,\n\t\"GeneralData\":\n\t{\n\t\t\"disableBaseDamage\": false,\n\t\t\"disableRespawnDialog\": false\n\t},\n" +
	"\t\"WorldsData\":\n\t{\n\t\t\"lightingConfig\": 0,\n\t\t\"objectSpawnersArr\": [\n        ],\n" +
	"\t\t\"environmentMinTemps\": [-3, -2, 0, 4, 9, 14, 18, 17, 13, 11, 9, 0],\n\t\t\"wetnessWeightModifiers\": [1.0, 1.0, 1.33, 1.66, 2.0]\n\t},\n" +
	"\t\"MapData\":\n\t{\n\t\t\"ignoreMapOwnership\": false\n\t}\n}\n"

const losstShape = "{\n\t\"version\": 123,\n\t\"WorldsData\":\n\t{\n\t\t\"lightingConfig\": 0,\n\t\t\"objectSpawnersArr\": [\"custom/The_Losst_City.json\", \"custom/other.json\"],\n\t\t\"x\": 1.50\n\t}\n}\n"

func TestPatchEmptyArrayMinimalAndExact(t *testing.T) {
	p, err := ProposePatch([]byte(liveShape), PatchOptions{CorrectLosstSpelling: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Before) != 0 || len(p.After) != 1 || p.After[0] != nitradodelivery.ArtifactRelPath {
		t.Fatalf("entries: %v -> %v", p.Before, p.After)
	}
	want := strings.Replace(liveShape, "\"objectSpawnersArr\": [\n        ],", "\"objectSpawnersArr\": [\n\t\t\t\"champion/champion_shop_delivery.json\"\n\t\t],", 1)
	if string(p.Proposed) != want {
		t.Fatalf("proposal not byte-exact:\n%s", p.Proposed)
	}
	if p.CurrentSHA256 != nitradodelivery.SHA256([]byte(liveShape)) || p.ProposedSHA256 != nitradodelivery.SHA256([]byte(want)) || p.CurrentSHA256 == p.ProposedSHA256 {
		t.Fatal("hashes")
	}
	// The exact diff touches only the array lines.
	var changed []string
	for _, l := range strings.Split(p.Diff, "\n") {
		if strings.HasPrefix(l, "+ ") || strings.HasPrefix(l, "- ") {
			changed = append(changed, l)
		}
	}
	wantDiff := []string{"- " + "        ],", "+ \t\t\t\"champion/champion_shop_delivery.json\"", "+ \t\t],"}
	if strings.Join(changed, "|") != strings.Join(wantDiff, "|") {
		t.Fatalf("diff: %q", changed)
	}
	if len(p.AffectedFiles) != 2 || p.AffectedFiles[0].Gate != GateA || p.AffectedFiles[1].Change != "CREATE" {
		t.Fatalf("%+v", p.AffectedFiles)
	}
	if !strings.Contains(strings.Join(p.BackupPlan, " "), p.CurrentSHA256) || !strings.Contains(strings.Join(p.RollbackPlan, " "), p.CurrentSHA256) {
		t.Fatal("backup/rollback must name the verified digest")
	}
	// Idempotent: the proposal is already applied.
	if _, err := ProposePatch(p.Proposed, PatchOptions{CorrectLosstSpelling: true}); !errors.Is(err, ErrNoChange) {
		t.Fatalf("second patch: %v", err)
	}
}

func TestPatchPreservesEverythingElse(t *testing.T) {
	p, err := ProposePatch([]byte(losstShape), PatchOptions{CorrectLosstSpelling: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.After, ",") != "custom/The_Lost_City.json,custom/other.json,champion/champion_shop_delivery.json" {
		t.Fatalf("%v", p.After)
	}
	// Bytes outside the array are identical (the "1.50" literal survives, it would be "1.5" after a re-encode).
	i := strings.Index(losstShape, "[")
	j := strings.Index(losstShape, "]") + 1
	if !strings.HasPrefix(string(p.Proposed), losstShape[:i]) || !strings.HasSuffix(string(p.Proposed), losstShape[j:]) {
		t.Fatal("bytes outside the spawner array changed")
	}
	// Without the spelling option the existing entry is preserved as is; Lost City is never re-added by default.
	p2, err := ProposePatch([]byte(losstShape), PatchOptions{})
	if err != nil || p2.After[0] != LosstCityRelPath || len(p2.After) != 3 {
		t.Fatalf("%v %v", p2.After, err)
	}
	p3, _ := ProposePatch([]byte(liveShape), PatchOptions{})
	for _, e := range p3.After {
		if e == LostCityRelPath {
			t.Fatal("Lost City re-added without the owner option")
		}
	}
	p4, _ := ProposePatch([]byte(liveShape), PatchOptions{ReAddLostCity: true})
	if strings.Join(p4.After, ",") != LostCityRelPath+","+nitradodelivery.ArtifactRelPath {
		t.Fatalf("%v", p4.After)
	}
	// CRLF files keep CRLF.
	crlf := strings.ReplaceAll(liveShape, "\n", "\r\n")
	p5, err := ProposePatch([]byte(crlf), PatchOptions{})
	if err != nil || strings.Count(string(p5.Proposed), "\n") != strings.Count(string(p5.Proposed), "\r\n") {
		t.Fatalf("crlf: %v", err)
	}
}

func TestPatchRefusesUnsafeInputs(t *testing.T) {
	for name, in := range map[string]string{
		"invalid json":   "{",
		"no array":       `{"WorldsData":{}}`,
		"two keys":       `{"WorldsData":{"objectSpawnersArr":[]},"x":{"objectSpawnersArr":[]}}`,
		"not strings":    `{"WorldsData":{"objectSpawnersArr":[{"a":1}]}}`,
		"outside worlds": `{"objectSpawnersArr":[],"WorldsData":{}}`,
	} {
		if _, err := ProposePatch([]byte(in), PatchOptions{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func f(v float64) *float64 { return &v }

var now = time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC)

func goodDrop() (DropPoint, *CurrentSession) {
	return DropPoint{OrganizationID: 1, InstallationID: 11, MapKey: "chernarusplus", X: 4621.1, Z: 8397.2, AltitudeY: f(319.6),
			SourceKind: SourceADMPlayerList, SourceFile: "dayzps/config/DayZServer_PS4_x64_2026-09-24_20-46-53.ADM", SourceOffset: 5120,
			ObservedAt: now.Add(-3 * time.Minute)},
		&CurrentSession{ADMFile: "dayzps/config/DayZServer_PS4_x64_2026-09-24_20-46-53.ADM"}
}

func TestDropPointRules(t *testing.T) {
	dp, sess := goodDrop()
	c, err := VerifyDropPoint(dp, 1, 11, "chernarusplus", sess, now, 0)
	if err != nil || c.Status != DropPointVerified || c.Pos != [3]float64{4621.1, 319.6, 8397.2} {
		t.Fatalf("%+v %v", c, err)
	}
	ended := now.Add(-time.Minute)
	cases := map[string]struct {
		mut  func(*DropPoint, *CurrentSession)
		want error
	}{
		"other tenant":    {func(d *DropPoint, _ *CurrentSession) { d.InstallationID = 12 }, ErrDropPointTenant},
		"other map":       {func(d *DropPoint, _ *CurrentSession) { d.MapKey = "enoch" }, ErrDropPointMap},
		"off map":         {func(d *DropPoint, _ *CurrentSession) { d.X = -5 }, ErrDropPointCoordinates},
		"no altitude":     {func(d *DropPoint, _ *CurrentSession) { d.AltitudeY = nil }, ErrDropPointAltitude},
		"absurd altitude": {func(d *DropPoint, _ *CurrentSession) { d.AltitudeY = f(9000) }, ErrDropPointAltitude},
		"not ADM":         {func(d *DropPoint, _ *CurrentSession) { d.SourceKind = "PLAYER_TYPED" }, ErrDropPointSourceKind},
		"no source":       {func(d *DropPoint, _ *CurrentSession) { d.SourceFile, d.SourceOffset = "", 0 }, ErrDropPointNoSource},
		"previous boot": {func(d *DropPoint, _ *CurrentSession) {
			d.SourceFile = "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
		}, ErrDropPointPreviousBoot},
		"session ended":    {func(_ *DropPoint, s *CurrentSession) { s.EndedAt = &ended }, ErrDropPointSessionEnded},
		"stale":            {func(d *DropPoint, _ *CurrentSession) { d.ObservedAt = now.Add(-2 * time.Hour) }, ErrDropPointStale},
		"future":           {func(d *DropPoint, _ *CurrentSession) { d.ObservedAt = now.Add(time.Hour) }, ErrDropPointFutureTime},
		"no session known": {func(_ *DropPoint, s *CurrentSession) { s.ADMFile = "" }, ErrDropPointNoSessionInfo},
	}
	for name, c := range cases {
		d, s := goodDrop()
		c.mut(&d, s)
		got, err := VerifyDropPoint(d, 1, 11, "chernarusplus", s, now, 0)
		if !errors.Is(err, c.want) || got.Status != DropPointRejected {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func binding() nitradodelivery.Binding {
	return nitradodelivery.Binding{OrganizationID: 1, InstallationID: 11, GameServerID: 1, NitradoServiceID: "19806451", MapKey: "chernarusplus"}
}

func TestSingleItemPreview(t *testing.T) {
	dp, sess := goodDrop()
	c, _ := VerifyDropPoint(dp, 1, 11, "chernarusplus", sess, now, 0)
	pv, err := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c})
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Preview || pv.AttemptID != "champion:d0:a1" || pv.ClassName != CanaryClassName || pv.Quantity != 1 || len(pv.Entries) != 1 {
		t.Fatalf("%+v", pv)
	}
	if pv.Entries[0].CustomString != "champion:d0:a1:u1" || pv.Pos != [3]float64{4621.1, 319.6, 8397.2} || pv.Entries[0].EnableCEPersistency {
		t.Fatalf("%+v", pv.Entries[0])
	}
	var parsed nitradodelivery.SpawnerFile
	if err := json.Unmarshal(pv.StagedJSON, &parsed); err != nil || len(parsed.Objects) != 1 || parsed.Objects[0].Name != "BandageDressing" {
		t.Fatalf("staged json: %v", err)
	}
	if pv.StagedSHA256 != nitradodelivery.SHA256(pv.StagedJSON) || pv.UnstagedSHA256 == pv.StagedSHA256 {
		t.Fatal("hashes")
	}
	if strings.TrimSpace(string(pv.UnstagedJSON)) != "{\n  \"Objects\": []\n}" {
		t.Fatalf("unstaged: %s", pv.UnstagedJSON)
	}
	if !strings.Contains(pv.UnstageDiff, "- ") || strings.Contains(pv.StageDiff, "- ") {
		t.Fatal("diffs")
	}
	if pv.Rollback.BeforeSHA256 != pv.UnstagedSHA256 || pv.Rollback.AfterSHA256 != pv.StagedSHA256 {
		t.Fatalf("rollback %+v", pv.Rollback)
	}
	// Deterministic fingerprint; a different attempt number changes identity.
	pv2, _ := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c})
	pv3, _ := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c, Attempt: 2})
	if pv.Fingerprint != pv2.Fingerprint || pv.Fingerprint == pv3.Fingerprint || pv3.AttemptID != "champion:d0:a2" {
		t.Fatal("fingerprint")
	}
	// A real delivery id is not a preview.
	pv4, _ := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c, DeliveryID: 501})
	if pv4.Preview || pv4.AttemptID != "champion:d501:a1" {
		t.Fatalf("%+v", pv4)
	}
	// Unverified drop point, missing altitude and tenant mismatch are refused.
	if _, err := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: DropPointCheck{Status: DropPointRejected}}); !errors.Is(err, ErrDropPointNotVerified) {
		t.Fatal(err)
	}
	other := binding()
	other.InstallationID = 12
	if _, err := PreviewSingleItem(CanaryInput{Binding: other, DropPoint: dp, Check: c}); !errors.Is(err, nitradodelivery.ErrWrongTenant) {
		t.Fatalf("tenant: %v", err)
	}
}

func bools(b bool) *bool { return &b }

func TestRestartRules(t *testing.T) {
	staged := now
	cur := "adm/2026-09-24_20-46-53.ADM"
	boot := func(name string, at time.Time) BootObservation {
		return BootObservation{BootFile: name, StartUTC: at, SourceAuthoritative: true, CEInitSeen: true}
	}
	pre := staged.Add(40 * time.Minute)
	b1 := boot("adm/b1.ADM", staged.Add(60*time.Minute))
	b2 := boot("adm/b2.ADM", staged.Add(125*time.Minute))
	unstagedAt := staged.Add(70 * time.Minute)
	late := staged.Add(130 * time.Minute)

	// Pre-start alone never unstages.
	d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptAwaitingRestart, StagedAt: staged, StagedBootFile: cur, PreStartSeenAt: &pre,
		Boots: []BootObservation{boot(cur, staged.Add(-time.Hour))}})
	if d.Next != nitradodelivery.AttemptAwaitingRestart || !strings.Contains(d.Action, "keep the entry staged") {
		t.Fatalf("pre-start: %+v", d)
	}
	// An unaccepted (non-authoritative) boot does not count.
	unacc := b1
	unacc.SourceAuthoritative = false
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptAwaitingRestart, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{unacc}}); d.Next != nitradodelivery.AttemptAwaitingRestart {
		t.Fatalf("unaccepted boot: %+v", d)
	}
	// First accepted new boot -> unstage required.
	d = DecideRestart(RestartEvidence{State: nitradodelivery.AttemptAwaitingRestart, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b1}})
	if d.Next != nitradodelivery.AttemptUnstageRequired || d.NewBoots != 1 || d.SpawnerOutcome != SpawnerNoErrorReported {
		t.Fatalf("first boot: %+v", d)
	}
	// Spawner error for the Champion file is reported.
	bad := b1
	bad.SpawnerErrorArtifact = true
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptAwaitingRestart, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{bad}}); d.SpawnerOutcome != SpawnerFailed || d.Next != nitradodelivery.AttemptUnstageRequired {
		t.Fatalf("spawner failed: %+v", d)
	}
	// Two new boots before a verified unstage -> review.
	for _, st := range []string{nitradodelivery.AttemptAwaitingRestart, nitradodelivery.AttemptUnstageRequired} {
		if d := DecideRestart(RestartEvidence{State: st, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b2, b1}}); d.Next != nitradodelivery.AttemptFailedReview {
			t.Fatalf("%s two boots: %+v", st, d)
		}
	}
	// Unstage verified only after the second boot -> review.
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptUnstageRequired, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b1, b2},
		UnstageVerifiedAt: &late, FileContainsAttempt: bools(false)}); d.Next != nitradodelivery.AttemptFailedReview {
		t.Fatalf("late unstage: %+v", d)
	}
	// Unstage verified in time but file not read back -> still unstage required.
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptUnstageRequired, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b1},
		UnstageVerifiedAt: &unstagedAt}); d.Next != nitradodelivery.AttemptUnstageRequired {
		t.Fatalf("unread: %+v", d)
	}
	// Verified unstage -> verification; a clean second start -> ready for Gate F.
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptUnstageRequired, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b1},
		UnstageVerifiedAt: &unstagedAt, FileContainsAttempt: bools(false)}); d.Next != nitradodelivery.AttemptVerificationRequired {
		t.Fatalf("verified: %+v", d)
	}
	d = DecideRestart(RestartEvidence{State: nitradodelivery.AttemptVerificationRequired, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{b1, b2},
		UnstageVerifiedAt: &unstagedAt, FileContainsAttempt: bools(false)})
	if !d.SecondStartClean || d.Next != nitradodelivery.AttemptVerificationRequired {
		t.Fatalf("second start: %+v", d)
	}
	// A boot inside the staging quiet period is ambiguous.
	near := boot("adm/near.ADM", staged.Add(-2*time.Minute))
	if d := DecideRestart(RestartEvidence{State: nitradodelivery.AttemptFileStaged, StagedAt: staged, StagedBootFile: cur, Boots: []BootObservation{near}}); d.Next != nitradodelivery.AttemptFailedReview {
		t.Fatalf("quiet period: %+v", d)
	}
}

func TestFulfillOnlyOnPhysicalConfirmation(t *testing.T) {
	ok := PhysicalConfirmation{ConfirmedBy: "123", Method: "IN_GAME_OBSERVED", ObservedAt: now, SecondStartClean: true}
	if s, err := Fulfill(nitradodelivery.AttemptVerificationRequired, ok); err != nil || s != nitradodelivery.AttemptFulfilled {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		state string
		conf  PhysicalConfirmation
	}{
		"from restart":   {nitradodelivery.AttemptRestartObserved, ok},
		"from unstage":   {nitradodelivery.AttemptUnstageRequired, ok},
		"anonymous":      {nitradodelivery.AttemptVerificationRequired, PhysicalConfirmation{Method: "IN_GAME_OBSERVED", ObservedAt: now, SecondStartClean: true}},
		"log evidence":   {nitradodelivery.AttemptVerificationRequired, PhysicalConfirmation{ConfirmedBy: "1", Method: "RPT_NO_ERROR", ObservedAt: now, SecondStartClean: true}},
		"no second boot": {nitradodelivery.AttemptVerificationRequired, PhysicalConfirmation{ConfirmedBy: "1", Method: "IN_GAME_OBSERVED", ObservedAt: now}},
	} {
		if _, err := Fulfill(c.state, c.conf); !errors.Is(err, ErrFulfillmentNotAllowed) {
			t.Errorf("%s: fulfilled", name)
		}
	}
}

func TestGatesAndUploadSequence(t *testing.T) {
	gs := Gates()
	ids := ""
	for _, g := range gs {
		ids += g.ID
		if g.Action == "" || len(g.Evidence) == 0 || g.Rollback == "" {
			t.Errorf("gate %s incomplete", g.ID)
		}
	}
	if ids != "ABCDEF" || !gs[0].Writes || !gs[1].Writes || !gs[2].Restarts || !gs[3].Writes || !gs[4].Restarts || gs[5].Writes || gs[5].Restarts {
		t.Fatalf("gates %s", ids)
	}
	if WriteCapability != "UNVERIFIED" {
		t.Fatal("write capability must stay UNVERIFIED until a live upload is verified")
	}
	seq := UploadSequence("champion/champion_shop_delivery.json", "aa", "bb", "champion/backup/x.bak")
	if len(seq) != 7 || seq[3].Kind != "READ" || seq[4].Kind != "WRITE" || seq[5].Kind != "READ" || !strings.Contains(seq[5].AbortIf, "restore") {
		t.Fatalf("%+v", seq)
	}
}

func TestMigrationIsProposalOnly(t *testing.T) {
	for _, s := range []string{nitradodelivery.AttemptPlanCreated, nitradodelivery.AttemptFilePrepared, nitradodelivery.AttemptFileStaged, nitradodelivery.AttemptAwaitingRestart,
		nitradodelivery.AttemptRestartObserved, nitradodelivery.AttemptUnstageRequired, nitradodelivery.AttemptVerificationRequired, nitradodelivery.AttemptFulfilled,
		nitradodelivery.AttemptAbandoned, nitradodelivery.AttemptUnstaged, nitradodelivery.AttemptFailedReview} {
		if !strings.Contains(ProposedAttemptMigrationSQL, "'"+s+"'") {
			t.Errorf("state %s missing from the proposed CHECK", s)
		}
	}
	if !strings.Contains(ProposedAttemptMigrationSQL, "WHERE state NOT IN ('FULFILLED','ABANDONED','UNSTAGED','FAILED_REVIEW')") {
		t.Fatal("open-attempt uniqueness")
	}
	reg, err := os.ReadFile(filepath.Join("..", "..", "database", "migrations.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(reg), "shop_delivery_attempts") || strings.Contains(string(reg), strconv.Quote(ProposedAttemptMigrationName)) {
		t.Fatal("the attempt migration must not be registered in this phase")
	}
}

// The canary package cannot write: it imports no network, database or Nitrado client package.
func TestPackageIsNonExecuting(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, fn := range files {
		if strings.HasSuffix(fn, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), fn, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			for _, bad := range []string{"net", "net/http", "os/exec", "database/sql", "github.com/jackc/pgx", "internal/nitrado", "internal/database", "internal/shop/capability"} {
				if p == bad || strings.HasPrefix(p, bad+"/") || strings.HasSuffix(p, "/"+bad) {
					t.Errorf("%s imports %s", fn, p)
				}
			}
		}
	}
}

// Nothing the preparation produces carries a credential, a signed URL or a physical account path.
func TestOutputsCarryNoCredentials(t *testing.T) {
	dp, sess := goodDrop()
	c, _ := VerifyDropPoint(dp, 1, 11, "chernarusplus", sess, now, 0)
	pv, _ := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c})
	p, _ := ProposePatch([]byte(liveShape), PatchOptions{})
	all, _ := json.Marshal([]any{pv, p, Gates(), UploadSequence(ConfigRelPath, p.CurrentSHA256, p.ProposedSHA256, ConfigBackupPath(p.CurrentSHA256))})
	for _, bad := range []string{"token=", "Bearer", "password", "X-Amz", "Signature=", "/games/", "ftproot", "http://", "https://"} {
		if strings.Contains(string(all), bad) {
			t.Errorf("output contains %q", bad)
		}
	}
}
