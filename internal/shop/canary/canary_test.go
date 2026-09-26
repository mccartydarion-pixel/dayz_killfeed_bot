package canary

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
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
	p, err := ProposePatch([]byte(liveShape))
	if err != nil {
		t.Fatal(err)
	}
	if p.Purpose != PurposeShopReference || len(p.Before) != 0 || len(p.After) != 1 || p.After[0] != nitradodelivery.ArtifactRelPath {
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
	// Sequencing: the empty Champion file is created (Gate A) before the configuration references it (Gate B).
	if len(p.AffectedFiles) != 2 || p.AffectedFiles[0].Gate != GateA || p.AffectedFiles[0].Change != "CREATE" ||
		p.AffectedFiles[1].Gate != GateB || p.AffectedFiles[1].Path != ConfigRelPath {
		t.Fatalf("%+v", p.AffectedFiles)
	}
	_, emptySHA := EmptyArtifact()
	if !strings.Contains(strings.Join(p.Preconditions, " "), emptySHA) {
		t.Fatal("the reference patch must require the verified empty Champion file")
	}
	if !strings.Contains(strings.Join(p.BackupPlan, " "), p.CurrentSHA256) || !strings.Contains(strings.Join(p.RollbackPlan, " "), p.CurrentSHA256) {
		t.Fatal("backup/rollback must name the verified digest")
	}
	// Idempotent: the proposal is already applied.
	if _, err := ProposePatch(p.Proposed); !errors.Is(err, ErrNoChange) {
		t.Fatalf("second patch: %v", err)
	}
}

func TestReferenceRequiresTheEmptyChampionFile(t *testing.T) {
	empty, sha := EmptyArtifact()
	if strings.TrimSpace(string(empty)) != "{\n  \"Objects\": []\n}" || sha != nitradodelivery.SHA256(empty) {
		t.Fatalf("empty artifact: %q", empty)
	}
	if err := CheckReferencePrecondition(nil, false); !errors.Is(err, ErrArtifactMissing) {
		t.Fatalf("missing file: %v", err)
	}
	if err := CheckReferencePrecondition(empty, true); err != nil {
		t.Fatal(err)
	}
	dp, sess := goodDrop()
	c, _ := VerifyDropPoint(dp, 1, 11, "chernarusplus", sess, now, 0)
	pv, _ := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c})
	if err := CheckReferencePrecondition(pv.StagedJSON, true); !errors.Is(err, ErrArtifactNotEmpty) {
		t.Fatalf("a staged file is not the empty file: %v", err)
	}
	if err := CheckReferencePrecondition([]byte(`{"Objects":[{"name":"X","pos":[1,2,3],"ypr":[0,0,0],"scale":1,"enableCEPersistency":false,"customString":"someone"}]}`), true); err == nil {
		t.Fatal("a foreign file was accepted")
	}
	// Same objects, different bytes: only the exact rendered empty file qualifies.
	if err := CheckReferencePrecondition([]byte(`{"Objects":[]}`), true); !errors.Is(err, ErrArtifactNotEmpty) {
		t.Fatalf("non-canonical empty file: %v", err)
	}
}

func TestShopPatchPreservesEverythingElse(t *testing.T) {
	p, err := ProposePatch([]byte(losstShape))
	if err != nil {
		t.Fatal(err)
	}
	// Existing entries - even a misspelled one - are kept exactly; Shop activation never edits Lost City.
	if strings.Join(p.After, ",") != LosstCityRelPath+",custom/other.json,"+nitradodelivery.ArtifactRelPath {
		t.Fatalf("%v", p.After)
	}
	// Bytes outside the array are identical (the "1.50" literal survives; a re-encode would print 1.5).
	i := strings.Index(losstShape, "[")
	j := strings.Index(losstShape, "]") + 1
	if !strings.HasPrefix(string(p.Proposed), losstShape[:i]) || !strings.HasSuffix(string(p.Proposed), losstShape[j:]) {
		t.Fatal("bytes outside the spawner array changed")
	}
	p3, _ := ProposePatch([]byte(liveShape))
	for _, e := range p3.After {
		if e == LostCityRelPath || e == LosstCityRelPath {
			t.Fatal("Shop activation restored Lost City")
		}
	}
	// CRLF files keep CRLF.
	crlf := strings.ReplaceAll(liveShape, "\n", "\r\n")
	p5, err := ProposePatch([]byte(crlf))
	if err != nil || strings.Count(string(p5.Proposed), "\n") != strings.Count(string(p5.Proposed), "\r\n") {
		t.Fatalf("crlf: %v", err)
	}
}

// TestShopPatchKeepsEnabledLostCity covers the live configuration since 2026-09-25: the owner
// re-enabled The Lost City, so the array already holds its (correctly spelled) reference.
func TestShopPatchKeepsEnabledLostCity(t *testing.T) {
	enabled := strings.Replace(liveShape, "[\n        ]", "[\n\t\t\t\""+LostCityRelPath+"\"\n\t\t]", 1)
	p, err := ProposePatch([]byte(enabled))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Before, ",") != LostCityRelPath || strings.Join(p.After, ",") != LostCityRelPath+","+nitradodelivery.ArtifactRelPath {
		t.Fatalf("%v -> %v", p.Before, p.After)
	}
	i := strings.Index(enabled, "\"objectSpawnersArr\": [") + len("\"objectSpawnersArr\": ")
	j := i + strings.Index(enabled[i:], "]") + 1
	if !strings.HasPrefix(string(p.Proposed), enabled[:i]) || !strings.HasSuffix(string(p.Proposed), enabled[j:]) {
		t.Fatal("bytes outside the spawner array changed")
	}
	// Lost City needs no change, before or after the Shop reference.
	if _, err := ProposeLostCityRestore([]byte(enabled)); !errors.Is(err, ErrNoChange) {
		t.Fatalf("before: %v", err)
	}
	if _, err := ProposeLostCityRestore(p.Proposed); !errors.Is(err, ErrNoChange) {
		t.Fatalf("after: %v", err)
	}
	// Re-proposing on the patched file changes nothing.
	if _, err := ProposePatch(p.Proposed); !errors.Is(err, ErrNoChange) {
		t.Fatalf("idempotent: %v", err)
	}
}

func TestLostCityRestoreIsSeparate(t *testing.T) {
	// Live shape (empty array): append.
	p, err := ProposeLostCityRestore([]byte(liveShape))
	if err != nil {
		t.Fatal(err)
	}
	if p.Purpose != PurposeLostCityRestore || strings.Join(p.After, ",") != LostCityRelPath || len(p.AffectedFiles) != 1 || p.AffectedFiles[0].Gate != GateLostCity {
		t.Fatalf("%+v", p)
	}
	// After the Shop reference: Lost City is appended and the Champion entry is untouched.
	shop, _ := ProposePatch([]byte(liveShape))
	p2, err := ProposeLostCityRestore(shop.Proposed)
	if err != nil || strings.Join(p2.After, ",") != nitradodelivery.ArtifactRelPath+","+LostCityRelPath {
		t.Fatalf("%v %v", p2.After, err)
	}
	// A misspelled reference is corrected in place.
	p3, err := ProposeLostCityRestore([]byte(losstShape))
	if err != nil || strings.Join(p3.After, ",") != LostCityRelPath+",custom/other.json" {
		t.Fatalf("%v %v", p3.After, err)
	}
	if _, err := ProposeLostCityRestore(p.Proposed); !errors.Is(err, ErrNoChange) {
		t.Fatalf("already enabled: %v", err)
	}
	if g := LostCityGate(); g.ID != GateLostCity {
		t.Fatal("lost city gate")
	}
	for _, g := range Gates() {
		if g.ID == GateLostCity {
			t.Fatal("the Lost City decision must not be a canary gate")
		}
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
		if _, err := ProposePatch([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := ProposeLostCityRestore([]byte(in)); err == nil {
			t.Errorf("%s: lost city accepted", name)
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
	// Stage: from the Gate A empty file only the empty list line goes; unstage: back to it.
	var removed []string
	for _, l := range strings.Split(pv.StageDiff, "\n") {
		if strings.HasPrefix(l, "- ") {
			removed = append(removed, l)
		}
	}
	if len(removed) != 1 || removed[0] != "-   \"Objects\": []" || !strings.Contains(pv.UnstageDiff, "+   \"Objects\": []") {
		t.Fatalf("diffs: %q", removed)
	}
	if empty, sha := EmptyArtifact(); string(pv.UnstagedJSON) != string(empty) || pv.UnstagedSHA256 != sha {
		t.Fatal("unstaging restores exactly the Gate A empty file")
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
	// The preview identity can never be used in production.
	if err := ValidateProductionAttemptID(pv.AttemptID); !errors.Is(err, ErrPlaceholderAttempt) {
		t.Fatalf("placeholder accepted: %v", err)
	}
	// A real canary delivery (existing purchase flow) is not a preview.
	real := canaryDelivery(501, 4621.3, 8397.0)
	pv4, err := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c, Delivery: &real})
	if err != nil || pv4.Preview || pv4.AttemptID != "champion:d501:a1" || ValidateProductionAttemptID(pv4.AttemptID) != nil {
		t.Fatalf("%+v %v", pv4, err)
	}
	if pv4.Pos != [3]float64{4621.3, 319.6, 8397.0} || pv4.Fingerprint == pv.Fingerprint || pv4.StagedSHA256 == pv.StagedSHA256 {
		t.Fatal("the real plan must be recomputed from the real delivery")
	}
	for name, d := range map[string]repository.ShopDelivery{
		"id 0":     canaryDelivery(0, 4621.1, 8397.2),
		"far away": canaryDelivery(502, 4623.0, 8397.2),
		"two units": func() repository.ShopDelivery {
			d := canaryDelivery(503, 4621.1, 8397.2)
			d.Items[0].Quantity = 2
			return d
		}(),
		"refunded": func() repository.ShopDelivery {
			d := canaryDelivery(504, 4621.1, 8397.2)
			d.Status = repository.DeliveryStatusCancelled
			return d
		}(),
		"other tenant": func() repository.ShopDelivery {
			d := canaryDelivery(505, 4621.1, 8397.2)
			d.InstallationID = 12
			return d
		}(),
	} {
		d := d
		if _, err := PreviewSingleItem(CanaryInput{Binding: binding(), DropPoint: dp, Check: c, Delivery: &d}); err == nil {
			t.Errorf("%s: accepted", name)
		}
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

func fullMilestones() Milestones {
	at := func(m int) time.Time { return now.Add(time.Duration(m) * time.Minute) }
	return Milestones{
		RestartInitiated: &Evidence{At: at(1), Detail: "restart.log pre-start"},
		NewBootAccepted:  &Evidence{At: at(10), Detail: "boot authority"},
		SpawnerAttempted: &Evidence{At: at(10), Detail: "CE init, no Champion spawner error"},
		ItemObserved:     &Evidence{At: at(12), By: "owner", Detail: "seen at drop point, picked up"},
		EntryRemoved:     &Evidence{At: at(15), Detail: "empty file read back"},
		SecondBootStart:  at(40),
		NoSecondSpawn:    &Evidence{At: at(50), By: "owner", Detail: "no new bandage at the drop point"},
	}
}

func TestMilestonesAreDistinct(t *testing.T) {
	m := fullMilestones()
	if !m.Complete() {
		t.Fatalf("%+v", m.Check())
	}
	names := ""
	for _, s := range m.Check() {
		names += s.Name + ","
	}
	if names != "RESTART_INITIATED,NEW_BOOT_ACCEPTED,SPAWNER_ATTEMPTED,ITEM_OBSERVED,ENTRY_REMOVED,NO_SECOND_SPAWN," {
		t.Fatal(names)
	}
	for name, mut := range map[string]func(*Milestones){
		"restart without accepted boot": func(m *Milestones) { m.NewBootAccepted = nil },
		"no spawner evidence":           func(m *Milestones) { m.SpawnerAttempted = nil },
		"rpt clean but item not seen":   func(m *Milestones) { m.ItemObserved = nil },
		"anonymous observation":         func(m *Milestones) { m.ItemObserved = &Evidence{At: now, Detail: "seen"} },
		"removed after second boot":     func(m *Milestones) { m.EntryRemoved = &Evidence{At: m.SecondBootStart.Add(time.Minute)} },
		"no second boot":                func(m *Milestones) { m.SecondBootStart = time.Time{} },
		"check before second boot":      func(m *Milestones) { m.NoSecondSpawn = &Evidence{At: m.SecondBootStart.Add(-time.Minute), By: "owner"} },
	} {
		mm := fullMilestones()
		mut(&mm)
		if mm.Complete() {
			t.Errorf("%s: complete", name)
		}
	}
}

func TestFulfillOnlyOnPhysicalConfirmation(t *testing.T) {
	ok := PhysicalConfirmation{ConfirmedBy: "123", Method: "IN_GAME_OBSERVED", ObservedAt: now.Add(12 * time.Minute), PickedUpBy: "OwnerCharacter",
		PickupObservedAt: now.Add(13 * time.Minute), NoRespawnCheckedAt: now.Add(50 * time.Minute), Milestones: fullMilestones()}
	if s, err := Fulfill(nitradodelivery.AttemptVerificationRequired, ok); err != nil || s != nitradodelivery.AttemptFulfilled {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*PhysicalConfirmation) *string{
		"from restart":     func(c *PhysicalConfirmation) *string { s := nitradodelivery.AttemptRestartObserved; return &s },
		"from unstage":     func(c *PhysicalConfirmation) *string { s := nitradodelivery.AttemptUnstageRequired; return &s },
		"anonymous":        func(c *PhysicalConfirmation) *string { c.ConfirmedBy = ""; return nil },
		"log evidence":     func(c *PhysicalConfirmation) *string { c.Method = "RPT_NO_ERROR"; return nil },
		"no pickup":        func(c *PhysicalConfirmation) *string { c.PickedUpBy = ""; return nil },
		"pickup before":    func(c *PhysicalConfirmation) *string { c.PickupObservedAt = now; return nil },
		"no respawn check": func(c *PhysicalConfirmation) *string { c.NoRespawnCheckedAt = time.Time{}; return nil },
		"milestone gap":    func(c *PhysicalConfirmation) *string { c.Milestones.EntryRemoved = nil; return nil },
	} {
		c := ok
		state := nitradodelivery.AttemptVerificationRequired
		if s := mut(&c); s != nil {
			state = *s
		}
		if _, err := Fulfill(state, c); !errors.Is(err, ErrFulfillmentNotAllowed) {
			t.Errorf("%s: fulfilled", name)
		}
	}
}

func TestStagingReadiness(t *testing.T) {
	good := StagingReadiness{Now: now, Operator: OperatorWindow{Operator: "123", ConfirmedAt: now.Add(-time.Minute), From: now.Add(-time.Minute), Until: now.Add(3 * time.Hour)},
		CurrentBootStart: now.Add(-30 * time.Minute), BootAccepted: true, DropPointObserved: now.Add(-4 * time.Minute)}
	if err := CheckStagingReadiness(good); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		mut  func(*StagingReadiness)
		want error
	}{
		"no operator":          {func(r *StagingReadiness) { r.Operator.Operator = "" }, ErrNoOperator},
		"unconfirmed operator": {func(r *StagingReadiness) { r.Operator.ConfirmedAt = time.Time{} }, ErrNoOperator},
		"window too short":     {func(r *StagingReadiness) { r.Operator.Until = now.Add(70 * time.Minute) }, ErrOperatorWindow},
		"window starts later":  {func(r *StagingReadiness) { r.Operator.From = now.Add(time.Minute) }, ErrOperatorWindow},
		"boot not accepted":    {func(r *StagingReadiness) { r.BootAccepted = false }, ErrBootNotAccepted},
		"boot too recent":      {func(r *StagingReadiness) { r.CurrentBootStart = now.Add(-3 * time.Minute) }, ErrBootTooRecent},
		"restart beginning":    {func(r *StagingReadiness) { r.PreStartPending = true }, ErrPreStartPending},
		"stale drop point":     {func(r *StagingReadiness) { r.DropPointObserved = now.Add(-25 * time.Minute) }, ErrDropPointTooStale},
	} {
		r := good
		c.mut(&r)
		if err := CheckStagingReadiness(r); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if MinOperatorWindow < 2*ObservedRestartInterval {
		t.Fatal("the operator window must cover a scheduled restart and the unstage after it")
	}
}

func TestGatesAndUploadSequence(t *testing.T) {
	gs := Gates()
	ids := ""
	for _, g := range gs {
		ids += g.ID
		if g.Action == "" || len(g.Evidence) == 0 || g.Rollback == "" || len(g.Preconditions) == 0 {
			t.Errorf("gate %s incomplete", g.ID)
		}
	}
	if ids != "ABCDEFGHI" {
		t.Fatalf("gates %s", ids)
	}
	by := map[string]Gate{}
	for _, g := range gs {
		by[g.ID] = g
	}
	// Writes, records and restarts are separate gates; the item is exposed only from E to G.
	for id, want := range map[string][4]bool{ // writes, records, restarts, exposure
		GateA: {true, false, false, false}, GateB: {true, false, false, false}, GateC: {false, true, false, false},
		GateD: {false, true, false, false}, GateE: {true, false, false, true}, GateF: {false, false, true, true},
		GateG: {true, false, false, true}, GateH: {false, false, true, false}, GateI: {false, true, false, false},
	} {
		g := by[id]
		if got := [4]bool{g.Writes, g.Records, g.Restarts, g.ItemExposure}; got != want {
			t.Errorf("gate %s: %v", id, got)
		}
	}
	// The empty file is created before the configuration references it.
	if by[GateA].Paths[0] != nitradodelivery.ArtifactRelPath || by[GateB].Paths[0] != ConfigRelPath {
		t.Fatal("A creates the Champion file, B edits the configuration")
	}
	if !strings.Contains(strings.Join(by[GateE].Preconditions, " "), "operator") {
		t.Fatal("staging requires a confirmed operator")
	}
	if WriteCapability != "UNVERIFIED" {
		t.Fatal("write capability must stay UNVERIFIED until a live upload is verified")
	}
	seq := UploadSequence("champion/champion_shop_delivery.json", "aa", "bb", "champion/backup/x.bak")
	if len(seq) != 7 || seq[3].Kind != "READ" || seq[4].Kind != "WRITE" || seq[5].Kind != "READ" || !strings.Contains(seq[5].AbortIf, "restore") {
		t.Fatalf("%+v", seq)
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
	p, _ := ProposePatch([]byte(liveShape))
	lc, _ := ProposeLostCityRestore([]byte(liveShape))
	all, _ := json.Marshal([]any{pv, p, lc, LostCityGate(), Gates(), UploadSequence(ConfigRelPath, p.CurrentSHA256, p.ProposedSHA256, ConfigBackupPath(p.CurrentSHA256))})
	for _, bad := range []string{"token=", "Bearer", "password", "X-Amz", "Signature=", "/games/", "ftproot", "http://", "https://"} {
		if strings.Contains(string(all), bad) {
			t.Errorf("output contains %q", bad)
		}
	}
}

func canaryDelivery(id int64, x, z float64) repository.ShopDelivery {
	gs := int64(1)
	return repository.ShopDelivery{ID: id, PurchaseID: id + 1000, OrganizationID: 1, InstallationID: 11, GameServerID: &gs, PlayerID: 7,
		DeliveryType: "MANUAL", Policy: repository.DeliveryPolicyManualCoordinate, MapKey: "chernarusplus", X: &x, Z: &z,
		Status: repository.DeliveryStatusManualReady, PurchaseStatus: repository.ShopStatusPendingFulfillment,
		Items: []repository.ShopPurchaseItem{{ProductName: "Canary BandageDressing", UnitPricePoints: 1, Quantity: 1, LineTotalPoints: 1}}}
}
