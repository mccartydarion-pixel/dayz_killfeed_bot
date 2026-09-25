package nitradodelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// fixture is a valid, open MANUAL_COORDINATE delivery on Chernarus with its binding.
func fixture() PlanInput {
	pid := int64(55)
	return PlanInput{
		OrganizationID: 1, InstallationID: 2,
		Binding: Binding{OrganizationID: 1, InstallationID: 2, GameServerID: 7, NitradoServiceID: "1234567", MapKey: "chernarusplus"},
		Delivery: repository.ShopDelivery{ID: 42, PurchaseID: 99, OrganizationID: 1, InstallationID: 2, GameServerID: i64(7), PlayerID: 5,
			DeliveryType: "MANUAL", Policy: repository.DeliveryPolicyManualCoordinate, MapKey: "chernarusplus", X: f64(7500.25), Z: f64(8300.5),
			Status: repository.DeliveryStatusManualReady, PurchaseStatus: repository.ShopStatusPendingFulfillment,
			Items: []repository.ShopPurchaseItem{{ProductID: &pid, ProductName: "M4 Rifle", UnitPricePoints: 500, Quantity: 2, LineTotalPoints: 1000}}},
		ClassName: "M4A1", ApprovedClassNames: []string{"M4A1", "Mag_STANAG_30Rnd"}, AltitudeY: f64(214.5), Attempt: 1,
	}
}

func mustPlan(t *testing.T, in PlanInput) Plan {
	t.Helper()
	p, err := NewPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanIsValidatedAndImmutable(t *testing.T) {
	in := fixture()
	p := mustPlan(t, in)
	if p.DeliveryID() != 42 || p.PurchaseID() != 99 || p.InstallationID() != 2 || p.NitradoServiceID() != "1234567" || p.MapKey() != "chernarusplus" ||
		p.Quantity() != 2 || p.ClassName() != "M4A1" || p.Position() != [3]float64{7500.25, 214.5, 8300.5} || p.RequiredAction() != RequiredActionOwnerConfirmedRestart ||
		p.ArtifactPath() != "champion/champion_shop_delivery.json" || p.AttemptID() != "champion:d42:a1" {
		t.Fatalf("%+v", p)
	}
	// Mutating the inputs or a returned slice never changes the plan.
	fp := p.Fingerprint()
	in.Delivery.Items[0].ProductName = "changed"
	*in.Delivery.X = 1
	items := p.Items()
	items[0].Quantity = 99
	if p.Items()[0].ProductName != "M4 Rifle" || p.Items()[0].Quantity != 2 || p.Position()[0] != 7500.25 || p.Fingerprint() != fp {
		t.Fatal("the plan must be immutable")
	}
	// No credential can be carried: the plan has no exported fields and the binding has no secret fields.
	raw, _ := json.Marshal(p)
	if string(raw) != "{}" {
		t.Fatalf("plan serializes nothing implicitly: %s", raw)
	}
	for _, banned := range []string{"password", "token", "secret", "credential"} {
		if strings.Contains(strings.ToLower(string(mustJSON(t, fixture().Binding))), banned) {
			t.Fatalf("binding carries %q", banned)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlanRejections(t *testing.T) {
	cases := map[string]struct {
		mut  func(*PlanInput)
		want error
	}{
		"other organization":              {func(in *PlanInput) { in.OrganizationID = 9 }, ErrWrongTenant},
		"other installation":              {func(in *PlanInput) { in.Delivery.InstallationID = 3 }, ErrWrongTenant},
		"binding of another installation": {func(in *PlanInput) { in.Binding.InstallationID = 3 }, ErrWrongTenant},
		"other game server":               {func(in *PlanInput) { in.Delivery.GameServerID = i64(8) }, ErrServiceMismatch},
		"no game server":                  {func(in *PlanInput) { in.Delivery.GameServerID = nil }, ErrServiceMismatch},
		"bad service id":                  {func(in *PlanInput) { in.Binding.NitradoServiceID = "../1" }, ErrInvalidServiceID},
		"pickup":                          {func(in *PlanInput) { in.Delivery.Policy = repository.DeliveryPolicyManualPickup }, ErrNotCoordinate},
		"refunded (cancelled)":            {func(in *PlanInput) { in.Delivery.Status = repository.DeliveryStatusCancelled }, ErrNotDeliverable},
		"refunded purchase":               {func(in *PlanInput) { in.Delivery.PurchaseStatus = repository.ShopStatusRefunded }, ErrNotDeliverable},
		"already fulfilled":               {func(in *PlanInput) { in.Delivery.Status = repository.DeliveryStatusFulfilled }, ErrNotDeliverable},
		"unknown purchase state":          {func(in *PlanInput) { in.Delivery.PurchaseStatus = "" }, ErrNotDeliverable},
		"unsupported map":                 {func(in *PlanInput) { in.Delivery.MapKey, in.Binding.MapKey = "sakhal", "sakhal" }, ErrUnsupportedMap},
		"map changed since":               {func(in *PlanInput) { in.Binding.MapKey = "enoch" }, ErrUnsupportedMap},
		"off map":                         {func(in *PlanInput) { in.Delivery.X = f64(15361) }, ErrInvalidCoordinates},
		"negative z":                      {func(in *PlanInput) { in.Delivery.Z = f64(-1) }, ErrInvalidCoordinates},
		"nan x":                           {func(in *PlanInput) { in.Delivery.X = f64(math.NaN()) }, ErrInvalidCoordinates},
		"missing coordinates":             {func(in *PlanInput) { in.Delivery.X = nil }, ErrInvalidCoordinates},
		"no altitude":                     {func(in *PlanInput) { in.AltitudeY = nil }, ErrAltitudeRequired},
		"absurd altitude":                 {func(in *PlanInput) { in.AltitudeY = f64(9000) }, ErrAltitudeRequired},
		"inf altitude":                    {func(in *PlanInput) { in.AltitudeY = f64(math.Inf(1)) }, ErrAltitudeRequired},
		"p3d path classname":              {func(in *PlanInput) { in.ClassName = `DZ\plants\tree.p3d` }, ErrInvalidClassName},
		"slash classname":                 {func(in *PlanInput) { in.ClassName = "DZ/rocks/stone" }, ErrInvalidClassName},
		"json injection":                  {func(in *PlanInput) { in.ClassName = `M4A1","pos":[0,0,0]` }, ErrInvalidClassName},
		"empty classname":                 {func(in *PlanInput) { in.ClassName = "" }, ErrInvalidClassName},
		"unapproved classname":            {func(in *PlanInput) { in.ClassName = "AKM" }, ErrClassNameNotApproved},
		"too many units":                  {func(in *PlanInput) { in.Delivery.Items[0].Quantity = 11 }, ErrInvalidQuantity},
		"no items":                        {func(in *PlanInput) { in.Delivery.Items = nil }, ErrInvalidQuantity},
		"attempt zero":                    {func(in *PlanInput) { in.Attempt = 0 }, ErrInvalidAttempt},
	}
	for name, c := range cases {
		in := fixture()
		c.mut(&in)
		if _, err := NewPlan(in); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	for _, ok := range []string{"M4A1", "Mag_STANAG_30Rnd", "AmmoBox_556x45_20Rnd", "BandageDressing"} {
		if ValidateClassName(ok) != nil {
			t.Errorf("%s is a valid classname", ok)
		}
	}
	// Livonia coordinates are checked against Livonia's own bounds.
	in := fixture()
	in.Delivery.MapKey, in.Binding.MapKey, in.Delivery.X = "enoch", "enoch", f64(13000)
	if _, err := NewPlan(in); !errors.Is(err, ErrInvalidCoordinates) {
		t.Fatalf("livonia bounds: %v", err)
	}
}

func TestSpawnerFileGenerationAndDiff(t *testing.T) {
	p := mustPlan(t, fixture())
	prop, err := PrototypeAdapter{}.Propose(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	var f SpawnerFile
	if err := json.Unmarshal(prop.ProposedFile, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Objects) != 2 {
		t.Fatalf("one spawner entry per unit: %+v", f.Objects)
	}
	for i, o := range f.Objects {
		if o.Name != "M4A1" || o.Pos != [3]float64{7500.25, 214.5, 8300.5} || o.Scale != 1 || o.EnableCEPersistency || o.CustomString != "champion:d42:a1:u"+string(rune('1'+i)) {
			t.Fatalf("entry %d: %+v", i, o)
		}
	}
	// The exact field names objectspawner.c reads.
	for _, k := range []string{`"Objects"`, `"name"`, `"pos"`, `"ypr"`, `"scale"`, `"enableCEPersistency"`, `"customString"`} {
		if !strings.Contains(string(prop.ProposedFile), k) {
			t.Fatalf("missing %s in %s", k, prop.ProposedFile)
		}
	}
	if !strings.Contains(prop.Diff, `"name": "M4A1",`) || !strings.HasPrefix(prop.Diff, "+ ") || strings.Contains(prop.Diff, "\n- ") || prop.ProposedSHA256 != SHA256(prop.ProposedFile) {
		t.Fatalf("diff:\n%s", prop.Diff)
	}
	// Re-staging the same attempt is idempotent; a second delivery is added beside it.
	again, err := PrototypeAdapter{}.Propose(p, prop.ProposedFile)
	if err != nil || string(again.ProposedFile) != string(prop.ProposedFile) || strings.Contains(again.Diff, "+ ") {
		t.Fatalf("idempotent restage: %v\n%s", err, again.Diff)
	}
	in2 := fixture()
	in2.Delivery.ID, in2.Delivery.Items[0].Quantity, in2.ClassName = 43, 1, "Mag_STANAG_30Rnd"
	both, err := PrototypeAdapter{}.Propose(mustPlan(t, in2), prop.ProposedFile)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := ParseSpawnerFile(both.ProposedFile)
	if len(cur.Objects) != 3 {
		t.Fatalf("%d", len(cur.Objects))
	}
	// Unstaging removes exactly one attempt and nothing else.
	left := Unstage(cur, p.AttemptID())
	if len(left.Objects) != 1 || left.Objects[0].Name != "Mag_STANAG_30Rnd" {
		t.Fatalf("%+v", left)
	}
	if d := Diff(cur.Render(), left.Render()); strings.Count(d, "\n- ") < 2 || strings.Contains(d, "\n+ ") {
		t.Fatalf("unstage diff:\n%s", d)
	}
	// Prefix safety: unstaging attempt a1 of delivery 4 must not touch delivery 42.
	if n := len(Unstage(cur, "champion:d4:a1").Objects); n != 3 {
		t.Fatalf("prefix collision removed entries: %d", n)
	}
}

func TestSpawnerFileRefusesForeignOrBrokenContent(t *testing.T) {
	p := mustPlan(t, fixture())
	for name, raw := range map[string]string{
		"owner edited":  `{"Objects":[{"name":"Land_Wall","pos":[1,2,3],"ypr":[0,0,0],"scale":1,"enableCEPersistency":false,"customString":""}]}`,
		"not json":      `{"Objects":[`,
		"unknown field": `{"Objects":[],"Extra":1}`,
	} {
		if _, err := (PrototypeAdapter{}).Propose(p, []byte(raw)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// Too many staged objects.
	f := SpawnerFile{}
	for i := 0; i < MaxStagedObjects; i++ {
		f.Objects = append(f.Objects, SpawnerObject{Name: "Apple", CustomString: "champion:d1:a1:u1"})
	}
	if _, err := Stage(f, p); !errors.Is(err, ErrTooManyStaged) {
		t.Fatalf("%v", err)
	}
}

func TestRollbackPlanning(t *testing.T) {
	p := mustPlan(t, fixture())
	before := []byte("{\n  \"Objects\": []\n}\n")
	prop, err := PrototypeAdapter{}.Propose(p, before)
	if err != nil {
		t.Fatal(err)
	}
	rb := prop.Rollback
	if rb.BeforeSHA256 != SHA256(before) || rb.AfterSHA256 != SHA256(prop.ProposedFile) || rb.ArtifactPath != ArtifactRelPath ||
		!strings.HasPrefix(rb.BackupName, "champion/backup/") || len(rb.Steps) < 6 {
		t.Fatalf("%+v", rb)
	}
	joined := strings.Join(rb.Steps, "\n")
	for _, must := range []string{"backup", "confirm", "someone else edited", "partial upload", "unstaged", "re-upload the backup"} {
		if !strings.Contains(joined, must) {
			t.Errorf("rollback plan misses %q", must)
		}
	}
	// Restoring the backup content restores the exact digest.
	if SHA256(before) != rb.BeforeSHA256 {
		t.Fatal("digest")
	}
	// Every path stays inside the Champion directory - never a caller-supplied path.
	for _, path := range []string{rb.ArtifactPath, rb.BackupName} {
		if !strings.HasPrefix(path, ArtifactDir+"/") || strings.Contains(path, "..") {
			t.Fatalf("path %q", path)
		}
	}
}

func TestAdapterStaysDisabledAndNeverExecutes(t *testing.T) {
	var a shop.DeliveryAdapter = PrototypeAdapter{}
	if a.Enabled() {
		t.Fatal("the prototype must be disabled")
	}
	if err := a.Deliver(context.Background(), shop.DeliveryPlan{DeliveryID: 1}); !errors.Is(err, shop.ErrAutomaticDeliveryDisabled) {
		t.Fatal(err)
	}
	r := a.DryRun(context.Background(), shop.DeliveryPlan{DeliveryID: 1, MapKey: "chernarusplus"})
	if r.Enabled || len(r.Blockers) == 0 {
		t.Fatalf("%+v", r)
	}
	prop, err := PrototypeAdapter{}.Propose(mustPlan(t, fixture()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if prop.Executed || prop.RequiredAction != RequiredActionOwnerConfirmedRestart || len(prop.Blockers) == 0 {
		t.Fatalf("%+v", prop)
	}
	for _, s := range prop.Steps {
		if !strings.HasPrefix(s, "NOT EXECUTED:") {
			t.Fatalf("a dry run never claims execution: %q", s)
		}
	}
}

func TestAttemptLedgerPreventsDuplicates(t *testing.T) {
	l := NewMemoryLedger()
	p1 := mustPlan(t, fixture())
	rec, err := l.Begin(p1)
	if err != nil || rec.State != AttemptPlanCreated {
		t.Fatal(err)
	}
	if _, err := l.Begin(p1); !errors.Is(err, ErrAttemptInProgress) {
		t.Fatalf("duplicate attempt: %v", err)
	}
	step := func(from, to string) {
		t.Helper()
		if _, err := l.Advance(42, p1.AttemptID(), from, to); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
	}
	step(AttemptPlanCreated, AttemptFilePrepared)
	step(AttemptFilePrepared, AttemptFileStaged)
	step(AttemptFileStaged, AttemptAwaitingRestart)
	step(AttemptAwaitingRestart, AttemptRestartObserved)
	// Shortcuts are refused: a restart is not proof of delivery.
	if _, err := l.Advance(42, p1.AttemptID(), AttemptRestartObserved, AttemptFulfilled); !errors.Is(err, ErrAttemptTransition) {
		t.Fatalf("restart -> fulfilled must be refused: %v", err)
	}
	// Verification never skips the unstage step (Phase 2C.2).
	if _, err := l.Advance(42, p1.AttemptID(), AttemptRestartObserved, AttemptVerificationRequired); !errors.Is(err, ErrAttemptTransition) {
		t.Fatalf("restart -> verification without unstaging must be refused: %v", err)
	}
	// A crashed worker never re-stages after a possible spawn.
	in2 := fixture()
	in2.Attempt = 2
	if _, err := l.Begin(mustPlan(t, in2)); !errors.Is(err, ErrAttemptInProgress) {
		t.Fatalf("%v", err)
	}
	step(AttemptRestartObserved, AttemptFailedReview)
	if _, err := l.Begin(mustPlan(t, in2)); !errors.Is(err, ErrAttemptUncertain) {
		t.Fatalf("after an uncertain attempt no automatic retry: %v", err)
	}

	// A different delivery: unstaged before any restart -> a new attempt is allowed; fulfilled -> never again.
	in3 := fixture()
	in3.Delivery.ID = 77
	a1 := mustPlan(t, in3)
	l.Begin(a1)
	l.Advance(77, a1.AttemptID(), AttemptPlanCreated, AttemptFilePrepared)
	l.Advance(77, a1.AttemptID(), AttemptFilePrepared, AttemptFileStaged)
	if _, err := l.Advance(77, a1.AttemptID(), AttemptFileStaged, AttemptUnstaged); err != nil {
		t.Fatal(err)
	}
	in3.Attempt = l.NextAttempt(77)
	a2 := mustPlan(t, in3)
	if _, err := l.Begin(a2); err != nil {
		t.Fatalf("a proven-unspawned attempt allows a retry: %v", err)
	}
	for _, s := range [][2]string{{AttemptPlanCreated, AttemptFilePrepared}, {AttemptFilePrepared, AttemptFileStaged}, {AttemptFileStaged, AttemptAwaitingRestart},
		{AttemptAwaitingRestart, AttemptRestartObserved}, {AttemptRestartObserved, AttemptUnstageRequired}, {AttemptUnstageRequired, AttemptVerificationRequired},
		{AttemptVerificationRequired, AttemptFulfilled}} {
		if _, err := l.Advance(77, a2.AttemptID(), s[0], s[1]); err != nil {
			t.Fatal(err)
		}
	}
	in3.Attempt = 3
	if _, err := l.Begin(mustPlan(t, in3)); !errors.Is(err, ErrAttemptFulfilled) {
		t.Fatalf("%v", err)
	}
	// Attempt numbers must follow history.
	in4 := fixture()
	in4.Delivery.ID, in4.Attempt = 88, 5
	if _, err := l.Begin(mustPlan(t, in4)); !errors.Is(err, ErrAttemptPlanMismatch) {
		t.Fatalf("%v", err)
	}
}

func TestConcurrentAttemptsOnlyOneWins(t *testing.T) {
	l := NewMemoryLedger()
	p := mustPlan(t, fixture())
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Begin(p); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent attempts: %d winners", wins)
	}
	// Concurrent compare-and-set advances: exactly one moves the state.
	moved := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Advance(42, p.AttemptID(), AttemptPlanCreated, AttemptFilePrepared); err == nil {
				mu.Lock()
				moved++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if moved != 1 {
		t.Fatalf("CAS advance: %d", moved)
	}
}

func TestReconcileAfterCrash(t *testing.T) {
	for name, c := range map[string]struct {
		state         string
		inFile, start bool
		want          string
	}{
		"never uploaded":              {AttemptFilePrepared, false, false, AttemptAbandoned},
		"upload landed unrecorded":    {AttemptFilePrepared, true, false, AttemptFileStaged},
		"staged, restart happened":    {AttemptFileStaged, true, true, AttemptUnstageRequired},
		"awaiting, restart happened":  {AttemptAwaitingRestart, false, true, AttemptVerificationRequired},
		"staged, removed, no restart": {AttemptFileStaged, false, false, AttemptUnstaged},
		"still staged, no restart":    {AttemptAwaitingRestart, true, false, AttemptAwaitingRestart},
		"restart observed, removed":   {AttemptRestartObserved, false, true, AttemptVerificationRequired},
		"restart observed, in file":   {AttemptRestartObserved, true, true, AttemptUnstageRequired},
		"unstage pending":             {AttemptUnstageRequired, true, true, AttemptUnstageRequired},
		"unstage done":                {AttemptUnstageRequired, false, true, AttemptVerificationRequired},
	} {
		if got, action := Reconcile(c.state, c.inFile, c.start); got != c.want || action == "" {
			t.Errorf("%s: %s (%s)", name, got, action)
		}
	}
}

// The in-memory prototype and the durable ledger (migration 0054) enforce the same state machine.
func TestAttemptStatesMatchDurableLedger(t *testing.T) {
	if len(attemptTransitions) != len(repository.ShopAttemptTransitions) {
		t.Fatalf("state count: %d vs %d", len(attemptTransitions), len(repository.ShopAttemptTransitions))
	}
	for from, tos := range attemptTransitions {
		got := repository.ShopAttemptTransitions[from]
		if fmt.Sprint(got) != fmt.Sprint(tos) {
			t.Errorf("%s: prototype %v, ledger %v", from, tos, got)
		}
	}
	for _, s := range [][2]string{{AttemptPlanCreated, repository.AttemptPlanCreated}, {AttemptFilePrepared, repository.AttemptFilePrepared},
		{AttemptFileStaged, repository.AttemptFileStaged}, {AttemptAwaitingRestart, repository.AttemptAwaitingRestart}, {AttemptRestartObserved, repository.AttemptRestartObserved},
		{AttemptUnstageRequired, repository.AttemptUnstageRequired}, {AttemptVerificationRequired, repository.AttemptVerificationRequired},
		{AttemptFulfilled, repository.AttemptFulfilled}, {AttemptAbandoned, repository.AttemptAbandoned}, {AttemptUnstaged, repository.AttemptUnstaged},
		{AttemptFailedReview, repository.AttemptFailedReview}} {
		if s[0] != s[1] {
			t.Errorf("%s != %s", s[0], s[1])
		}
	}
}
