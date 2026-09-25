package canaryops

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

const session = "dayzps/config/DayZServer_PS4_x64_2026-09-25_11-40-00.ADM"

// fakeLedger records every call; it applies the transition map so the service's own logic is tested.
type fakeLedger struct {
	mu       sync.Mutex
	attempts map[string]*repository.ShopAttempt
	evidence map[string][]repository.ShopAttemptEvidenceRecord
	calls    []string
	lastEv   repository.ShopAttemptEvidence
	lastPhys repository.ShopAttemptPhysicalEvidence
	resolved string
	ended    bool
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{attempts: map[string]*repository.ShopAttempt{}, evidence: map[string][]repository.ShopAttemptEvidenceRecord{}}
}

func (f *fakeLedger) call(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }

func (f *fakeLedger) mutations() int {
	n := 0
	for _, c := range f.calls {
		switch c {
		case "Create", "Transition", "ResolveReview", "RecordEvidence", "FulfillAttempt":
			n++
		}
	}
	return n
}

func (f *fakeLedger) Create(_ context.Context, in repository.ShopAttemptCreate, actor string) (repository.ShopAttempt, error) {
	f.call("Create")
	a := &repository.ShopAttempt{OrganizationID: in.OrganizationID, InstallationID: in.InstallationID, DeliveryID: in.DeliveryID, Attempt: in.Attempt,
		AttemptID: in.AttemptID, State: repository.AttemptPlanCreated, Fingerprint: in.Fingerprint, ClassName: in.ClassName, Quantity: in.Quantity,
		PosX: in.PosX, PosY: in.PosY, PosZ: in.PosZ, DropSourceFile: in.DropSourceFile, DropSourceOffset: in.DropSourceOffset}
	f.attempts[in.AttemptID] = a
	return *a, nil
}

func (f *fakeLedger) Transition(_ context.Context, org, inst int64, id, from, to, actor string, ev repository.ShopAttemptEvidence) (repository.ShopAttempt, error) {
	f.call("Transition")
	a, ok := f.attempts[id]
	if !ok {
		return repository.ShopAttempt{}, repository.ErrShopAttemptNotFound
	}
	if a.State != from {
		return repository.ShopAttempt{}, repository.ErrShopAttemptStale
	}
	f.lastEv = ev
	a.State = to
	if ev.StagedAt != nil {
		a.StagedAt = ev.StagedAt
	}
	return *a, nil
}

func (f *fakeLedger) ResolveReview(_ context.Context, org, inst int64, id, resolution, actor, note string) (repository.ShopAttempt, error) {
	f.call("ResolveReview")
	f.resolved = resolution
	return *f.attempts[id], nil
}

func (f *fakeLedger) Get(_ context.Context, org, inst int64, id string) (repository.ShopAttempt, error) {
	f.call("Get")
	a, ok := f.attempts[id]
	if !ok {
		return repository.ShopAttempt{}, repository.ErrShopAttemptNotFound
	}
	return *a, nil
}

func (f *fakeLedger) ListAttempts(context.Context, int64, int64, bool, int) ([]repository.ShopAttempt, error) {
	f.call("ListAttempts")
	return nil, nil
}

func (f *fakeLedger) Events(context.Context, int64, int64, string) ([]repository.ShopAttemptEvent, error) {
	return nil, nil
}

func (f *fakeLedger) RecordEvidence(_ context.Context, org, inst int64, id, actor string, in repository.ShopAttemptEvidenceInput) (repository.ShopAttemptEvidenceRecord, error) {
	f.call("RecordEvidence")
	rec := repository.ShopAttemptEvidenceRecord{Kind: in.Kind, Source: in.Source, RecordedBy: actor, ObservedAt: in.ObservedAt, BootStartedAt: in.BootStartedAt, Detail: in.Detail}
	s := func(v string) *string {
		if v == "" {
			return nil
		}
		return &v
	}
	rec.SHA256, rec.PreviousSHA256, rec.BootFile, rec.ObservedBy = s(in.SHA256), s(in.PreviousSHA256), s(in.BootFile), s(in.ObservedBy)
	f.evidence[id] = append(f.evidence[id], rec)
	return rec, nil
}

func (f *fakeLedger) ListEvidence(_ context.Context, org, inst int64, id string) ([]repository.ShopAttemptEvidenceRecord, error) {
	return f.evidence[id], nil
}

func (f *fakeLedger) FulfillAttempt(_ context.Context, org, inst int64, id string, uid int64, did string, ev repository.ShopAttemptPhysicalEvidence) (*repository.ShopPurchase, error) {
	f.call("FulfillAttempt")
	f.lastPhys = ev
	f.attempts[id].State = repository.AttemptFulfilled
	return &repository.ShopPurchase{}, nil
}

func (f *fakeLedger) NextAttemptNumber(context.Context, int64, int64, int64) (int, error) {
	return 1, nil
}

func (f *fakeLedger) CanaryBinding(context.Context, int64, int64) (repository.ShopCanaryBinding, error) {
	return repository.ShopCanaryBinding{GameServerID: 1, NitradoServiceID: "19806451", MapKey: "chernarusplus"}, nil
}

func (f *fakeLedger) CurrentBootSession(context.Context, int64) (repository.ShopCanarySession, error) {
	s := repository.ShopCanarySession{ADMFile: session}
	if f.ended {
		t := now
		s.EndedAt = &t
	}
	return s, nil
}

type fakeDeliveries struct{ qty int }

func (d fakeDeliveries) GetDelivery(_ context.Context, org, inst, id, _ int64) (*repository.ShopDelivery, error) {
	if org != 1 || inst != 11 {
		return nil, repository.ErrShopDeliveryNotFound
	}
	gs, x, z := int64(1), 4621.1, 8397.2
	q := d.qty
	if q == 0 {
		q = 1
	}
	return &repository.ShopDelivery{ID: id, PurchaseID: 900, OrganizationID: 1, InstallationID: 11, GameServerID: &gs, PlayerID: 5, DeliveryType: "MANUAL",
		Policy: repository.DeliveryPolicyManualCoordinate, MapKey: "chernarusplus", X: &x, Z: &z, Status: repository.DeliveryStatusManualReady,
		PurchaseStatus: repository.ShopStatusPendingFulfillment, Items: []repository.ShopPurchaseItem{{ProductName: "Canary", UnitPricePoints: 1, Quantity: q, LineTotalPoints: int64(q)}}}, nil
}

type fakeMembers map[int64]string

func (m fakeMembers) VerifyMembership(_ context.Context, org, user int64) (string, bool, error) {
	if org != 1 {
		return "", false, nil
	}
	r, ok := m[user]
	return r, ok, nil
}

type fakeScopes struct{ status string }

func (s fakeScopes) Scope(_ context.Context, org, inst int64) (repository.EconomyScope, error) {
	if org != 1 || inst != 11 {
		return repository.EconomyScope{}, economy.ErrInstallationNotFound
	}
	st := s.status
	if st == "" {
		st = "ACTIVE"
	}
	return repository.EconomyScope{OrganizationID: 1, InstallationID: 11, Status: st}, nil
}

var (
	owner  = Actor{UserID: 1, DiscordID: "100000001"}
	admin  = Actor{UserID: 2, DiscordID: "100000002"}
	member = Actor{UserID: 3, DiscordID: "100000003"}
	nobody = Actor{UserID: 4, DiscordID: "100000004"}
)

func newSvc(gate Gate) (*Service, *fakeLedger) {
	l := newFakeLedger()
	s := New(l, fakeDeliveries{}, fakeMembers{1: "OWNER", 2: "ADMIN", 3: "MEMBER"}, fakeScopes{}, gate)
	s.SetClock(func() time.Time { return now })
	return s, l
}

func openGate() Gate { return NewGate(true, []int64{11}) }

func goodCreate() CreateRequest {
	return CreateRequest{DeliveryID: 42, AltitudeY: 319.6, DropSourceFile: session, DropSourceOffset: 853, DropObservedAt: now.Add(-3 * time.Minute)}
}

func TestAuthorizationAndTenantIsolation(t *testing.T) {
	s, l := newSvc(openGate())
	ctx := context.Background()
	for name, c := range map[string]struct {
		org, inst int64
		a         Actor
		want      error
	}{
		"no actor":           {1, 11, Actor{}, ErrForbidden},
		"member":             {1, 11, member, ErrForbidden},
		"not a member":       {1, 11, nobody, ErrForbidden},
		"other organization": {2, 11, owner, ErrForbidden},
		"other installation": {1, 12, owner, ErrInstallationUnknown},
	} {
		if _, err := s.CreateAttempt(ctx, c.org, c.inst, c.a, goodCreate()); !errors.Is(err, c.want) {
			t.Errorf("%s create: %v", name, err)
		}
		if _, err := s.GetAttempt(ctx, c.org, c.inst, c.a, "champion:d42:a1"); !errors.Is(err, c.want) {
			t.Errorf("%s get: %v", name, err)
		}
		if _, err := s.ListAttempts(ctx, c.org, c.inst, c.a, true, 10); !errors.Is(err, c.want) {
			t.Errorf("%s list: %v", name, err)
		}
	}
	if l.mutations() != 0 {
		t.Fatalf("an unauthorized request reached the ledger: %v", l.calls)
	}
	for _, a := range []Actor{owner, admin} {
		if _, err := s.ListAttempts(ctx, 1, 11, a, true, 10); err != nil {
			t.Fatal(err)
		}
	}
}

// The execution lock: closed by default, per installation, and it blocks every mutation before the ledger.
func TestExecutionLockPreventsProductionOperations(t *testing.T) {
	ctx := context.Background()
	for name, g := range map[string]Gate{
		"zero value":             {},
		"disabled":               NewGate(false, []int64{11}),
		"other installation":     NewGate(true, []int64{12}),
		"enabled without a list": NewGate(true, nil),
	} {
		s, l := newSvc(g)
		l.attempts["champion:d42:a1"] = &repository.ShopAttempt{AttemptID: "champion:d42:a1", State: repository.AttemptFailedReview}
		ops := map[string]error{}
		_, ops["create"] = s.CreateAttempt(ctx, 1, 11, owner, goodCreate())
		_, ops["advance"] = s.AdvanceAttempt(ctx, 1, 11, owner, "champion:d42:a1", repository.AttemptPlanCreated, repository.AttemptFilePrepared, "")
		_, ops["evidence"] = s.RecordEvidence(ctx, 1, 11, owner, "champion:d42:a1", EvidenceRequest{Kind: repository.EvidenceSpawnerLog, Source: repository.SourceRPTLog, ObservedAt: now, Detail: "x"})
		_, ops["review"] = s.ResolveReview(ctx, 1, 11, owner, "champion:d42:a1", OutcomeNotSpawned, "checked")
		_, ops["fulfil"] = s.FulfillAttempt(ctx, 1, 11, owner, "champion:d42:a1", "")
		for op, err := range ops {
			if !errors.Is(err, ErrExecutionLocked) {
				t.Errorf("%s %s: %v", name, op, err)
			}
		}
		if l.mutations() != 0 {
			t.Errorf("%s: the lock let a mutation through: %v", name, l.calls)
		}
		// Reads stay available to OWNER/ADMIN while locked.
		if _, err := s.GetAttempt(ctx, 1, 11, admin, "champion:d42:a1"); err != nil {
			t.Errorf("%s read: %v", name, err)
		}
	}
	// Suspended installation: locked too.
	l := newFakeLedger()
	s := New(l, fakeDeliveries{}, fakeMembers{1: "OWNER"}, fakeScopes{status: "SUSPENDED"}, openGate())
	if _, err := s.CreateAttempt(ctx, 1, 11, owner, goodCreate()); !errors.Is(err, ErrSuspended) {
		t.Fatal(err)
	}
}

func TestCreateAttemptValidatesTheDropPoint(t *testing.T) {
	ctx := context.Background()
	s, l := newSvc(openGate())
	v, err := s.CreateAttempt(ctx, 1, 11, owner, goodCreate())
	if err != nil {
		t.Fatal(err)
	}
	a := v.Attempt
	if a.AttemptID != "champion:d42:a1" || len(a.Fingerprint) != 64 || a.ClassName != CanaryClassName || a.Quantity != 1 ||
		a.PosX != 4621.1 || a.PosY != 319.6 || a.PosZ != 8397.2 || a.DropSourceOffset != 853 {
		t.Fatalf("%+v", a)
	}
	if len(v.Next) != 2 || v.Next[0].To != repository.AttemptFilePrepared || !v.Next[1].NeedsReason {
		t.Fatalf("next steps: %+v", v.Next)
	}
	for name, mut := range map[string]func(*CreateRequest){
		"previous boot":   func(r *CreateRequest) { r.DropSourceFile = "dayzps/config/DayZServer_PS4_x64_2026-09-24_20-46-53.ADM" },
		"no source":       func(r *CreateRequest) { r.DropSourceFile = "" },
		"no offset":       func(r *CreateRequest) { r.DropSourceOffset = 0 },
		"stale":           func(r *CreateRequest) { r.DropObservedAt = now.Add(-25 * time.Minute) },
		"future":          func(r *CreateRequest) { r.DropObservedAt = now.Add(5 * time.Minute) },
		"no delivery":     func(r *CreateRequest) { r.DeliveryID = 0 },
		"absurd altitude": func(r *CreateRequest) { r.AltitudeY = 9000 },
	} {
		r := goodCreate()
		mut(&r)
		if _, err := s.CreateAttempt(ctx, 1, 11, owner, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	l.ended = true
	if _, err := s.CreateAttempt(ctx, 1, 11, owner, goodCreate()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ended session: %v", err)
	}
	s2 := New(newFakeLedger(), fakeDeliveries{qty: 2}, fakeMembers{1: "OWNER"}, fakeScopes{}, openGate())
	s2.SetClock(func() time.Time { return now })
	if _, err := s2.CreateAttempt(ctx, 1, 11, owner, goodCreate()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("two units: %v", err)
	}
}

func sha(c string) string { return strings.Repeat(c, 64) }

func TestEvidenceRulesAndAdvance(t *testing.T) {
	ctx := context.Background()
	s, l := newSvc(openGate())
	v, _ := s.CreateAttempt(ctx, 1, 11, owner, goodCreate())
	id := v.Attempt.AttemptID
	rec := func(r EvidenceRequest) error { _, err := s.RecordEvidence(ctx, 1, 11, admin, id, r); return err }
	adv := func(from, to, reason string) error {
		_, err := s.AdvanceAttempt(ctx, 1, 11, owner, id, from, to, reason)
		return err
	}
	// A physical fact from a log is refused; wrong sources and unknown kinds too.
	if err := rec(EvidenceRequest{Kind: repository.EvidenceItemObserved, Source: repository.SourceRPTLog, ObservedBy: "x", ObservedAt: now}); !errors.Is(err, ErrNotPhysicalProof) {
		t.Fatalf("RPT as physical proof: %v", err)
	}
	for name, r := range map[string]EvidenceRequest{
		"wrong source":       {Kind: repository.EvidenceStagedFileHash, Source: repository.SourceInGameObservation, SHA256: sha("5"), ObservedAt: now},
		"unknown kind":       {Kind: "RESTART_LOG", Source: repository.SourceRPTLog, ObservedAt: now},
		"assessment by hand": {Kind: repository.EvidenceReviewUncertain, Source: repository.SourceOperatorAssessment, ObservedAt: now, Detail: "x"},
		"no time":            {Kind: repository.EvidenceSpawnerLog, Source: repository.SourceRPTLog, Detail: "x"},
		"future":             {Kind: repository.EvidenceSpawnerLog, Source: repository.SourceRPTLog, ObservedAt: now.Add(time.Hour), Detail: "x"},
	} {
		if err := rec(r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := adv(repository.AttemptPlanCreated, repository.AttemptFilePrepared, ""); err != nil {
		t.Fatal(err)
	}
	// Missing evidence is named.
	err := adv(repository.AttemptFilePrepared, repository.AttemptFileStaged, "")
	if !errors.Is(err, ErrMissingEvidence) || !strings.Contains(err.Error(), repository.EvidenceStagedFileHash) || !strings.Contains(err.Error(), repository.EvidenceStagingBoot) {
		t.Fatalf("missing: %v", err)
	}
	stagedAt := now.Add(-time.Minute)
	if err := rec(EvidenceRequest{Kind: repository.EvidenceStagedFileHash, Source: repository.SourceNitradoReadback, SHA256: sha("5"), PreviousSHA256: sha("e"), ObservedAt: stagedAt}); err != nil {
		t.Fatal(err)
	}
	if err := rec(EvidenceRequest{Kind: repository.EvidenceStagingBoot, Source: repository.SourceBootAuthority, BootFile: session, ObservedAt: stagedAt}); err != nil {
		t.Fatal(err)
	}
	if err := adv(repository.AttemptFilePrepared, repository.AttemptFileStaged, ""); err != nil {
		t.Fatal(err)
	}
	if ev := l.lastEv; *ev.StagedSHA256 != sha("5") || *ev.BeforeSHA256 != sha("e") || !ev.StagedAt.Equal(stagedAt) || *ev.StagedBootFile != session {
		t.Fatalf("staged evidence not carried: %+v", ev)
	}
	if err := adv(repository.AttemptFileStaged, repository.AttemptAwaitingRestart, ""); err != nil {
		t.Fatal(err)
	}
	// A boot that started before the verified staging is ambiguous.
	early := stagedAt.Add(-time.Minute)
	if err := rec(EvidenceRequest{Kind: repository.EvidenceNewBoot, Source: repository.SourceBootAuthority, BootFile: "b2.ADM", BootStartedAt: &early, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := adv(repository.AttemptAwaitingRestart, repository.AttemptRestartObserved, ""); !errors.Is(err, ErrAmbiguousBoot) {
		t.Fatalf("ambiguous boot: %v", err)
	}
	// FULFILLED only through FulfillAttempt; FAILED_REVIEW needs a reason.
	if err := adv(repository.AttemptVerificationRequired, repository.AttemptFulfilled, ""); !errors.Is(err, ErrUseFulfill) {
		t.Fatal(err)
	}
	if err := adv(repository.AttemptAwaitingRestart, repository.AttemptFailedReview, " "); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := adv(repository.AttemptAwaitingRestart, repository.AttemptFailedReview, "boot started before staging was verified"); err != nil {
		t.Fatal(err)
	}
	if l.lastEv.FailureReason == nil {
		t.Fatal("the reason is recorded")
	}
}

func TestReviewOutcomesNeverCreateAttempts(t *testing.T) {
	ctx := context.Background()
	s, l := newSvc(openGate())
	l.attempts["champion:d42:a1"] = &repository.ShopAttempt{AttemptID: "champion:d42:a1", State: repository.AttemptFailedReview}
	if _, err := s.ResolveReview(ctx, 1, 11, owner, "champion:d42:a1", OutcomeUncertain, "cannot tell from the evidence"); err != nil {
		t.Fatal(err)
	}
	if l.resolved != "" || len(l.evidence["champion:d42:a1"]) != 1 || l.evidence["champion:d42:a1"][0].Kind != repository.EvidenceReviewUncertain {
		t.Fatalf("UNCERTAIN must be an assessment, not a resolution: %q %+v", l.resolved, l.evidence)
	}
	for _, bad := range []struct{ outcome, note string }{{"MAYBE", "x"}, {OutcomeNotSpawned, " "}} {
		if _, err := s.ResolveReview(ctx, 1, 11, owner, "champion:d42:a1", bad.outcome, bad.note); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	if _, err := s.ResolveReview(ctx, 1, 11, admin, "champion:d42:a1", OutcomeNotSpawned, "verified in game: nothing there"); err != nil || l.resolved != OutcomeNotSpawned {
		t.Fatal(err)
	}
	for _, c := range l.calls {
		if c == "Create" {
			t.Fatal("a review resolution created an attempt")
		}
	}
}

func TestFulfillUsesOnlyPhysicalEvidence(t *testing.T) {
	ctx := context.Background()
	s, l := newSvc(openGate())
	id := "champion:d42:a1"
	l.attempts[id] = &repository.ShopAttempt{AttemptID: id, State: repository.AttemptVerificationRequired}
	add := func(r repository.ShopAttemptEvidenceInput) { l.RecordEvidence(ctx, 1, 11, id, "100000001", r) }
	// A clean RPT is not proof: with only a log record the fulfilment is refused.
	add(repository.ShopAttemptEvidenceInput{Kind: repository.EvidenceSpawnerLog, Source: repository.SourceRPTLog, ObservedAt: now, Detail: "no [::SpawnObjects] error"})
	if _, err := s.FulfillAttempt(ctx, 1, 11, owner, id, ""); !errors.Is(err, ErrMissingEvidence) || !strings.Contains(err.Error(), repository.EvidenceItemObserved) {
		t.Fatalf("log-only fulfilment: %v", err)
	}
	b := now.Add(-30 * time.Minute)
	add(repository.ShopAttemptEvidenceInput{Kind: repository.EvidenceItemObserved, Source: repository.SourceInGameObservation, ObservedBy: "owner", ObservedAt: now.Add(-50 * time.Minute)})
	add(repository.ShopAttemptEvidenceInput{Kind: repository.EvidencePickupConfirmed, Source: repository.SourceInGameObservation, ObservedBy: "OwnerCharacter", ObservedAt: now.Add(-49 * time.Minute)})
	add(repository.ShopAttemptEvidenceInput{Kind: repository.EvidenceSecondBoot, Source: repository.SourceBootAuthority, BootFile: "b3.ADM", BootStartedAt: &b, ObservedAt: now.Add(-20 * time.Minute)})
	if _, err := s.FulfillAttempt(ctx, 1, 11, owner, id, ""); !errors.Is(err, ErrMissingEvidence) || !strings.Contains(err.Error(), repository.EvidenceNoAdditionalSpawn) {
		t.Fatalf("no respawn check: %v", err)
	}
	add(repository.ShopAttemptEvidenceInput{Kind: repository.EvidenceNoAdditionalSpawn, Source: repository.SourceInGameObservation, ObservedBy: "owner", ObservedAt: now.Add(-10 * time.Minute)})
	if _, err := s.FulfillAttempt(ctx, 1, 11, owner, id, "canary complete"); err != nil {
		t.Fatal(err)
	}
	p := l.lastPhys
	if p.ItemObservedBy != "owner" || p.PickedUpBy != "OwnerCharacter" || p.SecondBootFile != "b3.ADM" || !p.SecondBootStartedAt.Equal(b) || !p.NoRespawnCheckedAt.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("physical evidence: %+v", p)
	}
}

// The operator service is a recorder: it cannot reach Nitrado, HTTP, processes or the capability probe.
func TestPackageCannotExecute(t *testing.T) {
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
			for _, bad := range []string{"net", "net/http", "os/exec", "internal/nitrado", "internal/shop/capability", "internal/shop/canary"} {
				if p == bad || strings.HasSuffix(p, "/"+bad) {
					t.Errorf("%s imports %s", fn, p)
				}
			}
		}
	}
}
