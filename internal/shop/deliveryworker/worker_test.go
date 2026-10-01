package deliveryworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/missionwrite"
	nd "github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// --- a simulated world: server, ledger, journal, buyer ---------------------------------------------

const (
	bootA = "DayZServer_PS4_x64_2026-10-01_08-00-00.ADM"
	bootB = "DayZServer_PS4_x64_2026-10-01_09-08-00.ADM"
	bootC = "DayZServer_PS4_x64_2026-10-01_10-16-00.ADM"
)

var emptyFile = nd.SpawnerFile{Objects: []nd.SpawnerObject{}}.Render()

type fakeServer struct {
	content    []byte
	config     string
	mission    string
	boots      []string
	status     string
	inspectErr error
	// write knobs
	writeStatus   string // "" = write lands and verifies
	storeAnyway   bool   // with UNCERTAIN: the bytes landed
	restartDuring bool
	configChanged bool
	stale         bool
	inspects      int
	writes        [][]byte
}

func (s *fakeServer) Inspect(context.Context) (missionwrite.ArtifactState, error) {
	s.inspects++
	if s.inspectErr != nil {
		return missionwrite.ArtifactState{}, s.inspectErr
	}
	return missionwrite.ArtifactState{MissionPath: s.mission, ConfigSHA256: s.config, Content: append([]byte(nil), s.content...),
		SHA256: missionwrite.SHA256(s.content), Boots: append([]string(nil), s.boots...), GameserverStatus: s.status}, nil
}

func (s *fakeServer) Write(_ context.Context, st missionwrite.ArtifactState, payload []byte) (missionwrite.ArtifactWrite, error) {
	out := missionwrite.ArtifactWrite{Before: st.SHA256}
	if s.stale {
		out.Status, out.Detail = missionwrite.StatusNotWritten, "stale"
		return out, missionwrite.ErrSnapshotStale
	}
	s.writes = append(s.writes, payload)
	switch s.writeStatus {
	case missionwrite.StatusNotWritten:
		out.Status, out.After, out.Detail = missionwrite.StatusNotWritten, st.SHA256, "refused"
		return out, nil
	case missionwrite.StatusUncertain:
		if s.storeAnyway {
			s.content = payload
		}
		out.Status, out.Detail = missionwrite.StatusUncertain, "read-back failed"
		return out, nil
	}
	s.content = payload
	out.Status, out.After, out.Detail = missionwrite.StatusWrittenVerified, missionwrite.SHA256(payload), "read-back matches the payload"
	out.RestartDuring, out.ConfigChanged = s.restartDuring, s.configChanged
	return out, nil
}

type fakeLedger struct {
	attempts map[string]*repository.ShopAttempt
	order    []string
	failNext map[string]error // transition target state -> error returned once
	clock    func() time.Time
	// confirmation states the database would check on fulfilment
	buyers *fakeBuyers
	orders *fakeDeliveries
}

func (l *fakeLedger) ListOpen(_ context.Context, org, inst int64) ([]repository.ShopAttempt, error) {
	var out []repository.ShopAttempt
	for _, id := range l.order {
		a := l.attempts[id]
		switch a.State {
		case repository.AttemptFulfilled, repository.AttemptAbandoned, repository.AttemptUnstaged, repository.AttemptFailedReview:
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

func (l *fakeLedger) NextAttemptNumber(_ context.Context, _, _, deliveryID int64) (int, error) {
	n := 1
	for _, a := range l.attempts {
		if a.DeliveryID == deliveryID {
			n++
		}
	}
	return n, nil
}

func (l *fakeLedger) Create(_ context.Context, in repository.ShopAttemptCreate, actor string) (repository.ShopAttempt, error) {
	if !strings.HasPrefix(actor, "worker:") {
		return repository.ShopAttempt{}, errors.New("no actor")
	}
	for _, a := range l.attempts {
		if a.DeliveryID == in.DeliveryID && a.State != repository.AttemptAbandoned && a.State != repository.AttemptUnstaged {
			return repository.ShopAttempt{}, repository.ErrShopAttemptConflict
		}
	}
	a := &repository.ShopAttempt{OrganizationID: in.OrganizationID, InstallationID: in.InstallationID, DeliveryID: in.DeliveryID, Attempt: in.Attempt,
		AttemptID: in.AttemptID, State: repository.AttemptPlanCreated, Fingerprint: in.Fingerprint, ArtifactPath: in.ArtifactPath, ClassName: in.ClassName,
		Quantity: in.Quantity, PosX: in.PosX, PosY: in.PosY, PosZ: in.PosZ, DropSourceFile: in.DropSourceFile, DropSourceOffset: in.DropSourceOffset,
		FulfilmentMode: in.FulfilmentMode}
	l.attempts[a.AttemptID] = a
	l.order = append(l.order, a.AttemptID)
	return *a, nil
}

func (l *fakeLedger) Transition(_ context.Context, _, _ int64, attemptID, from, to, actor string, ev repository.ShopAttemptEvidence) (repository.ShopAttempt, error) {
	a := l.attempts[attemptID]
	if a == nil {
		return repository.ShopAttempt{}, repository.ErrShopAttemptNotFound
	}
	if err := l.failNext[to]; err != nil {
		delete(l.failNext, to)
		return *a, err
	}
	if a.State != from {
		return *a, repository.ErrShopAttemptStale
	}
	allowed := false
	for _, s := range repository.ShopAttemptTransitions[from] {
		allowed = allowed || s == to
	}
	if !allowed || to == repository.AttemptFulfilled {
		return *a, fmt.Errorf("%w: %s -> %s", repository.ErrShopAttemptRejected, from, to)
	}
	set := func(dst **string, v *string) {
		if v != nil {
			*dst = v
		}
	}
	setT := func(dst **time.Time, v *time.Time) {
		if v != nil {
			*dst = v
		}
	}
	set(&a.BeforeSHA256, ev.BeforeSHA256)
	set(&a.StagedSHA256, ev.StagedSHA256)
	set(&a.UnstagedSHA256, ev.UnstagedSHA256)
	set(&a.StagedBootFile, ev.StagedBootFile)
	set(&a.RestartBootFile, ev.RestartBootFile)
	set(&a.FailureReason, ev.FailureReason)
	setT(&a.StagedAt, ev.StagedAt)
	setT(&a.RestartObservedAt, ev.RestartObservedAt)
	setT(&a.UnstageVerifiedAt, ev.UnstageVerifiedAt)
	// The ledger's own CHECKs.
	switch to {
	case repository.AttemptFileStaged:
		if a.StagedSHA256 == nil || a.StagedAt == nil || a.StagedBootFile == nil {
			return *a, repository.ErrShopAttemptEvidence
		}
	case repository.AttemptRestartObserved:
		if a.RestartBootFile == nil || a.RestartObservedAt == nil || *a.RestartBootFile == *a.StagedBootFile {
			return *a, repository.ErrShopAttemptEvidence
		}
	case repository.AttemptVerificationRequired:
		if a.UnstageVerifiedAt == nil || a.UnstagedSHA256 == nil || *a.UnstagedSHA256 == *a.StagedSHA256 || a.UnstageVerifiedAt.Before(*a.StagedAt) {
			return *a, repository.ErrShopAttemptEvidence
		}
	case repository.AttemptFailedReview:
		if a.FailureReason == nil {
			return *a, repository.ErrShopAttemptEvidence
		}
	}
	a.State = to
	return *a, nil
}

func (l *fakeLedger) FulfillAttemptByBuyer(_ context.Context, org, inst int64, attemptID, answer string, answeredAt time.Time, actor string) (*repository.ShopPurchase, error) {
	a := l.attempts[attemptID]
	if a == nil || a.State != repository.AttemptVerificationRequired || a.FulfilmentMode != repository.ShopFulfilmentBuyer {
		return nil, repository.ErrShopAttemptStale
	}
	// Migration 0100: the buyer's confirmation must hold exactly this answer.
	purchase := l.orders.byID[a.DeliveryID].PurchaseID
	if c, ok := l.buyers.confirmations[purchase]; !ok || c.State != answer || answeredAt.Before(*a.UnstageVerifiedAt) {
		return nil, repository.ErrShopAttemptRejected
	}
	a.State, a.BuyerAnswer, a.BuyerAnsweredAt = repository.AttemptFulfilled, &answer, &answeredAt
	return &repository.ShopPurchase{ID: purchase, Status: repository.ShopStatusFulfilled}, nil
}

type fakeStore struct {
	paused      []string
	accepted    []string
	writes      []*repository.ShopDeliveryWrite
	candidates  []repository.ShopAutoCandidate
	bootLog     repository.ShopBootLog
	clock       func() time.Time
	finishErr   error
	candidateN  int
	bootLogFrom []time.Time
}

func (s *fakeStore) Pause(_ context.Context, _, _ int64, reason string) error {
	s.paused = append(s.paused, reason)
	return nil
}
func (s *fakeStore) AcceptConfiguration(_ context.Context, _, _ int64, sha, mission string) error {
	s.accepted = append(s.accepted, sha+"|"+mission)
	return nil
}
func (s *fakeStore) BeginWrite(_ context.Context, w repository.ShopDeliveryWrite) (repository.ShopDeliveryWrite, error) {
	if u, _ := s.UnresolvedWrite(context.Background(), w.InstallationID); u != nil {
		return w, repository.ErrShopWriteOutstanding
	}
	w.ID, w.Outcome, w.StartedAt = int64(len(s.writes)+1), repository.ShopWriteStarted, s.clock()
	s.writes = append(s.writes, &w)
	return w, nil
}
func (s *fakeStore) FinishWrite(_ context.Context, id int64, outcome, after, detail string) error {
	if s.finishErr != nil {
		return s.finishErr
	}
	w := s.writes[id-1]
	if w.Outcome != repository.ShopWriteStarted {
		return errors.New("not open")
	}
	now := s.clock()
	w.Outcome, w.Detail, w.FinishedAt = outcome, detail, &now
	if after != "" {
		w.AfterSHA256 = &after
	}
	return nil
}
func (s *fakeStore) UnresolvedWrite(context.Context, int64) (*repository.ShopDeliveryWrite, error) {
	for _, w := range s.writes {
		if w.Outcome == repository.ShopWriteStarted || w.Outcome == repository.ShopWriteUncertain {
			c := *w
			return &c, nil
		}
	}
	return nil, nil
}
func (s *fakeStore) Candidates(_ context.Context, _, _, _ int64, limit int) ([]repository.ShopAutoCandidate, error) {
	s.candidateN++
	if limit > len(s.candidates) {
		limit = len(s.candidates)
	}
	return append([]repository.ShopAutoCandidate(nil), s.candidates[:limit]...), nil
}
func (s *fakeStore) BootLog(_ context.Context, _ int64, from, _ time.Time) (repository.ShopBootLog, error) {
	s.bootLogFrom = append(s.bootLogFrom, from)
	return s.bootLog, nil
}

type fakeDeliveries struct {
	byID map[int64]*repository.ShopDelivery
}

func (d *fakeDeliveries) GetDelivery(_ context.Context, _, _, id, _ int64) (*repository.ShopDelivery, error) {
	if v, ok := d.byID[id]; ok {
		c := *v
		return &c, nil
	}
	return nil, repository.ErrShopDeliveryNotFound
}

type fakeBuyers struct {
	confirmations map[int64]*repository.ShopOrderConfirmation
	tickets       []string
}

func (b *fakeBuyers) OpenForDelivered(_ context.Context, _, _, purchaseID int64) error {
	if _, ok := b.confirmations[purchaseID]; !ok {
		b.confirmations[purchaseID] = &repository.ShopOrderConfirmation{PurchaseID: purchaseID, State: repository.ConfirmationAwaitingBuyer}
	}
	return nil
}
func (b *fakeBuyers) GetConfirmation(_ context.Context, _, _, purchaseID, _ int64) (repository.ShopOrderConfirmation, error) {
	if c, ok := b.confirmations[purchaseID]; ok {
		return *c, nil
	}
	return repository.ShopOrderConfirmation{}, repository.ErrShopConfirmationNotFound
}
func (b *fakeBuyers) OpenSystemTicket(_ context.Context, _, _, purchaseID int64, reason string) (repository.ShopOrderTicket, error) {
	b.tickets = append(b.tickets, fmt.Sprintf("%d: %s", purchaseID, reason))
	return repository.ShopOrderTicket{ID: int64(len(b.tickets)), PurchaseID: purchaseID}, nil
}

// world wires a worker to the simulation. Server-local time is UTC-4, like Champions.
type world struct {
	t       *testing.T
	now     time.Time
	srv     *fakeServer
	ledger  *fakeLedger
	store   *fakeStore
	orders  *fakeDeliveries
	buyers  *fakeBuyers
	w       *Worker
	inst    repository.ShopAutoInstallation
	tickets []int64
}

const utcMinus4 = -240

func utc(local string) time.Time {
	t, _ := time.Parse("2006-01-02 15:04:05", local)
	return t.Add(4 * time.Hour)
}

func newWorld(t *testing.T, cfg Config) *world {
	offset := utcMinus4
	wd := &world{t: t, now: utc("2026-10-01 08:30:00"),
		srv:    &fakeServer{content: emptyFile, config: strings.Repeat("c", 64), mission: "dayzps_missions/dayzOffline.chernarusplus", boots: []string{bootA}, status: "started"},
		orders: &fakeDeliveries{byID: map[int64]*repository.ShopDelivery{}},
		buyers: &fakeBuyers{confirmations: map[int64]*repository.ShopOrderConfirmation{}},
		inst: repository.ShopAutoInstallation{OrganizationID: 1, InstallationID: 11, GameServerID: 1, GuildRowID: 5, NitradoServiceID: "19806451",
			MapKey: "chernarusplus", ConfigSHA256: strings.Repeat("c", 64), MissionPath: "dayzps_missions/dayzOffline.chernarusplus", UTCOffsetMinutes: &offset},
	}
	clock := func() time.Time { return wd.now }
	wd.ledger = &fakeLedger{attempts: map[string]*repository.ShopAttempt{}, failNext: map[string]error{}, clock: clock, buyers: wd.buyers, orders: wd.orders}
	wd.store = &fakeStore{clock: clock, bootLog: repository.ShopBootLog{CentralEconomySeen: true}}
	wd.w = New(cfg, wd.ledger, wd.store, wd.orders, wd.buyers)
	wd.w.SetClock(clock)
	wd.w.OnTicket = func(id int64) { wd.tickets = append(wd.tickets, id) }
	return wd
}

// order adds an open coordinate delivery of one item and makes it a candidate.
func (wd *world) order(deliveryID int64, class string, quantity int) {
	server, x, z := int64(1), 4621.1, 8397.2
	wd.orders.byID[deliveryID] = &repository.ShopDelivery{ID: deliveryID, PurchaseID: deliveryID + 100, OrganizationID: 1, InstallationID: 11, GameServerID: &server,
		PlayerID: 7, Policy: repository.DeliveryPolicyManualCoordinate, MapKey: "chernarusplus", X: &x, Z: &z, Status: repository.DeliveryStatusManualReady,
		PurchaseStatus: repository.ShopStatusPendingFulfillment, Items: []repository.ShopPurchaseItem{{ProductName: class, Quantity: quantity}}}
	wd.store.candidates = append(wd.store.candidates, repository.ShopAutoCandidate{DeliveryID: deliveryID, PurchaseID: deliveryID + 100, PlayerID: 7,
		ClassName: class, AltitudeY: 319.6, DropSourceFile: bootA, DropSourceOffset: 853})
}

// pass runs one pass and drops candidates that now have an attempt (as the real query would).
func (wd *world) pass() Report {
	wd.t.Helper()
	rep, err := wd.w.Pass(context.Background(), wd.inst, wd.srv)
	if err != nil {
		wd.t.Fatalf("pass: %v", err)
	}
	var left []repository.ShopAutoCandidate
	for _, c := range wd.store.candidates {
		taken := false
		for _, a := range wd.ledger.attempts {
			if a.DeliveryID == c.DeliveryID && a.State != repository.AttemptAbandoned && a.State != repository.AttemptUnstaged {
				taken = true
			}
		}
		if !taken {
			left = append(left, c)
		}
	}
	wd.store.candidates = left
	return rep
}

func (wd *world) at(local string) { wd.now = utc(local) }

func (wd *world) state(attemptID string) string {
	wd.t.Helper()
	a := wd.ledger.attempts[attemptID]
	if a == nil {
		wd.t.Fatalf("no attempt %s (have %v)", attemptID, wd.ledger.order)
	}
	return a.State
}

func (wd *world) fileIsEmpty() bool { return string(wd.srv.content) == string(emptyFile) }

// stagedOrder runs the world up to one order staged and waiting for a restart.
func stagedOrder(t *testing.T) *world {
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	if rep := wd.pass(); len(rep.Staged) != 1 {
		t.Fatalf("not staged: %+v", rep)
	}
	wd.at("2026-10-01 08:31:00")
	wd.pass()
	if got := wd.state("champion:d1:a1"); got != repository.AttemptAwaitingRestart {
		t.Fatalf("state = %s", got)
	}
	return wd
}

// --- tests ---------------------------------------------------------------------------------------

func TestOneOrderEndToEnd(t *testing.T) {
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 2)

	rep := wd.pass()
	if len(rep.Staged) != 1 || rep.Staged[0] != "champion:d1:a1" || rep.WriteOutcome != missionwrite.StatusWrittenVerified {
		t.Fatalf("stage pass: %+v", rep)
	}
	a := wd.ledger.attempts["champion:d1:a1"]
	want := nd.SpawnerFile{Objects: nd.AttemptEntries("champion:d1:a1", "BandageDressing", 2, [3]float64{4621.1, 319.6, 8397.2})}.Render()
	if string(wd.srv.content) != string(want) || a.FulfilmentMode != repository.ShopFulfilmentBuyer || a.ArtifactPath != repository.ShopAttemptCustomArtifactPath ||
		*a.StagedBootFile != bootA || *a.BeforeSHA256 != missionwrite.SHA256(emptyFile) || *a.StagedSHA256 != missionwrite.SHA256(want) || !a.StagedAt.Equal(wd.now) {
		t.Fatalf("staged attempt %+v\nfile %s", a, wd.srv.content)
	}

	// No restart yet: the worker waits and writes nothing.
	wd.at("2026-10-01 08:45:00")
	if rep := wd.pass(); rep.WriteOutcome != "" || wd.state("champion:d1:a1") != repository.AttemptAwaitingRestart || len(wd.srv.writes) != 1 {
		t.Fatalf("waiting pass: %+v writes=%d", rep, len(wd.srv.writes))
	}

	// The scheduled restart happens at 09:08 local; its ADM is listed nine minutes later.
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.at("2026-10-01 09:17:00")
	rep = wd.pass()
	if len(rep.Unstaged) != 1 || !wd.fileIsEmpty() || wd.state("champion:d1:a1") != repository.AttemptVerificationRequired || len(wd.srv.writes) != 2 {
		t.Fatalf("unstage pass: %+v file=%s", rep, wd.srv.content)
	}
	if *a.RestartBootFile != bootB || !a.RestartObservedAt.Equal(utc("2026-10-01 09:08:00")) || *a.UnstagedSHA256 != missionwrite.SHA256(emptyFile) {
		t.Fatalf("restart evidence %+v", a)
	}

	// The server's log shows the boot finished: the buyer is asked. The order is not fulfilled yet.
	rep = wd.pass()
	if len(rep.Delivered) != 1 || wd.buyers.confirmations[101].State != repository.ConfirmationAwaitingBuyer || wd.state("champion:d1:a1") != repository.AttemptVerificationRequired {
		t.Fatalf("deliver pass: %+v", rep)
	}
	if rep := wd.pass(); len(rep.Fulfilled)+len(rep.Delivered) != 0 || rep.Inspected {
		t.Fatalf("a pass while the buyer has not answered touched something: %+v", rep)
	}

	// The buyer presses Received.
	answered := utc("2026-10-01 09:40:00")
	wd.buyers.confirmations[101].State, wd.buyers.confirmations[101].RespondedAt = repository.ConfirmationReceived, &answered
	wd.at("2026-10-01 09:41:00")
	rep = wd.pass()
	if len(rep.Fulfilled) != 1 || wd.state("champion:d1:a1") != repository.AttemptFulfilled || *a.BuyerAnswer != repository.BuyerAnswerReceived || !a.BuyerAnsweredAt.Equal(answered) {
		t.Fatalf("fulfil pass: %+v attempt=%+v", rep, a)
	}
	if len(wd.srv.writes) != 2 || len(wd.store.paused) != 0 || len(wd.buyers.tickets) != 0 {
		t.Fatalf("writes=%d paused=%v tickets=%v", len(wd.srv.writes), wd.store.paused, wd.buyers.tickets)
	}
	for _, w := range wd.store.writes {
		if w.Outcome != repository.ShopWriteWrittenVerified || w.Worker != "worker:t" {
			t.Fatalf("journal row %+v", w)
		}
	}
}

func TestNoAnswerByTheDeadlineFulfils(t *testing.T) {
	wd := stagedOrder(t)
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.at("2026-10-01 09:17:00")
	wd.pass()
	wd.pass()
	wd.buyers.confirmations[101].State = repository.ConfirmationAutoCompleted
	if rep := wd.pass(); len(rep.Fulfilled) != 1 || *wd.ledger.attempts["champion:d1:a1"].BuyerAnswer != repository.BuyerAnswerAutoCompleted {
		t.Fatalf("%+v", rep)
	}
}

func TestBuyerIssueSendsTheAttemptToReview(t *testing.T) {
	wd := stagedOrder(t)
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.at("2026-10-01 09:17:00")
	wd.pass()
	wd.pass()
	wd.buyers.confirmations[101].State = repository.ConfirmationIssueReported
	rep := wd.pass()
	a := wd.ledger.attempts["champion:d1:a1"]
	if len(rep.Failed) != 1 || a.State != repository.AttemptFailedReview || !strings.Contains(*a.FailureReason, "buyer reported") {
		t.Fatalf("%+v %+v", rep, a)
	}
	// The buyer's own ticket is the ticket: the worker opens no second one.
	if len(wd.buyers.tickets) != 0 {
		t.Fatalf("tickets = %v", wd.buyers.tickets)
	}
}

func TestStagingPreconditions(t *testing.T) {
	cases := map[string]func(wd *world){
		"the current boot is younger than the quiet period": func(wd *world) { wd.at("2026-10-01 08:09:00") },
		"the server clock offset is unknown":                func(wd *world) { wd.inst.UTCOffsetMinutes = nil },
		"the server is not running":                         func(wd *world) { wd.srv.status = "stopped" },
		"Nitrado cannot be read":                            func(wd *world) { wd.srv.inspectErr = missionwrite.ErrInspection },
	}
	for name, prepare := range cases {
		wd := newWorld(t, Config{Name: "t"})
		wd.order(1, "BandageDressing", 1)
		prepare(wd)
		rep := wd.pass()
		if len(wd.srv.writes) != 0 || len(wd.ledger.attempts) != 0 || len(wd.store.writes) != 0 || len(wd.store.paused) != 0 || rep.Skipped == "" {
			t.Errorf("%s: writes=%d attempts=%d journal=%d paused=%v skipped=%q", name, len(wd.srv.writes), len(wd.ledger.attempts), len(wd.store.writes), wd.store.paused, rep.Skipped)
		}
	}
}

func TestPlanRefusalsLeaveTheOrderInTheManualQueue(t *testing.T) {
	for name, spoil := range map[string]func(d *repository.ShopDelivery, c *repository.ShopAutoCandidate){
		"refunded meanwhile": func(d *repository.ShopDelivery, _ *repository.ShopAutoCandidate) {
			d.PurchaseStatus = repository.ShopStatusRefunded
		},
		"another server": func(d *repository.ShopDelivery, _ *repository.ShopAutoCandidate) {
			other := int64(2)
			d.GameServerID = &other
		},
		"another map": func(d *repository.ShopDelivery, _ *repository.ShopAutoCandidate) { d.MapKey = "enoch" },
		"a class name with a path": func(_ *repository.ShopDelivery, c *repository.ShopAutoCandidate) {
			c.ClassName = "dz/structures/castle.p3d"
		},
		"no altitude":  func(_ *repository.ShopDelivery, c *repository.ShopAutoCandidate) { c.AltitudeY = 9000 },
		"eleven units": func(d *repository.ShopDelivery, _ *repository.ShopAutoCandidate) { d.Items[0].Quantity = 11 },
	} {
		wd := newWorld(t, Config{Name: "t"})
		wd.order(1, "BandageDressing", 1)
		spoil(wd.orders.byID[1], &wd.store.candidates[0])
		wd.pass()
		if len(wd.srv.writes) != 0 || len(wd.ledger.attempts) != 0 || len(wd.store.writes) != 0 || len(wd.store.paused) != 0 {
			t.Errorf("%s: writes=%d attempts=%d journal=%d paused=%v", name, len(wd.srv.writes), len(wd.ledger.attempts), len(wd.store.writes), wd.store.paused)
		}
	}
}

func TestUnclearRestartsGoToReviewAndAreUnstaged(t *testing.T) {
	cases := map[string]struct {
		boots  []string
		reason string
	}{
		// Staged at 08:30 local. A boot named 08:25 that was not listed yet had already begun.
		"the restart began before the staging":  {[]string{"DayZServer_PS4_x64_2026-10-01_08-25-00.ADM"}, "already begun"},
		"staged two minutes before the restart": {[]string{"DayZServer_PS4_x64_2026-10-01_08-32-00.ADM"}, "less than three minutes"},
		"two restarts before the removal":       {[]string{bootB, bootC}, "spawned twice"},
	}
	for name, tc := range cases {
		wd := stagedOrder(t)
		wd.srv.boots = append(wd.srv.boots, tc.boots...)
		wd.at("2026-10-01 10:30:00")
		rep := wd.pass()
		a := wd.ledger.attempts["champion:d1:a1"]
		if len(rep.Failed) != 1 || a.State != repository.AttemptFailedReview || !strings.Contains(*a.FailureReason, tc.reason) || !wd.fileIsEmpty() {
			t.Errorf("%s: %+v state=%s reason=%v file=%s", name, rep, a.State, a.FailureReason, wd.srv.content)
			continue
		}
		if len(wd.buyers.tickets) != 1 || !strings.Contains(wd.buyers.tickets[0], "101: Automatic delivery stopped") || len(wd.tickets) != 1 {
			t.Errorf("%s: tickets=%v notified=%v", name, wd.buyers.tickets, wd.tickets)
		}
		if len(wd.buyers.confirmations) != 0 {
			t.Errorf("%s: the buyer was asked about an order under review", name)
		}
		// The attempt is terminal: later passes never stage the order again.
		wd.order(1, "BandageDressing", 1)
		wd.store.candidates = nil // the real query excludes a delivery with a FAILED_REVIEW attempt
		wd.pass()
		if len(wd.srv.writes) != 2 {
			t.Errorf("%s: %d writes, want stage + unstage only", name, len(wd.srv.writes))
		}
	}
}

func TestARestartWhileRemovingIsADoubleSpawnRisk(t *testing.T) {
	wd := stagedOrder(t)
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.srv.restartDuring = true
	wd.at("2026-10-01 09:17:00")
	rep := wd.pass()
	if len(rep.Failed) != 1 || !wd.fileIsEmpty() || !strings.Contains(*wd.ledger.attempts["champion:d1:a1"].FailureReason, "while the item was being removed") {
		t.Fatalf("%+v", rep)
	}
}

func TestLogEvidenceDecidesWhetherTheBuyerIsAsked(t *testing.T) {
	removed := func(t *testing.T) *world {
		wd := stagedOrder(t)
		wd.srv.boots = append(wd.srv.boots, bootB)
		wd.at("2026-10-01 09:17:00")
		wd.pass()
		return wd
	}

	wd := removed(t)
	wd.store.bootLog = repository.ShopBootLog{CentralEconomySeen: true, SpawnerError: true}
	rep := wd.pass()
	if len(rep.Failed) != 1 || len(wd.buyers.confirmations) != 0 || !strings.Contains(wd.buyers.tickets[0], "missing or invalid") {
		t.Fatalf("spawner error: %+v tickets=%v", rep, wd.buyers.tickets)
	}
	if from := wd.store.bootLogFrom[0]; !from.Equal(utc("2026-10-01 09:07:00")) {
		t.Fatalf("the log was read from %v, want one minute before the restart", from)
	}

	wd = removed(t)
	wd.store.bootLog = repository.ShopBootLog{}
	wd.at("2026-10-01 09:40:00")
	if rep := wd.pass(); len(rep.Failed)+len(rep.Delivered) != 0 {
		t.Fatalf("no log yet, inside the wait: %+v", rep)
	}
	wd.at("2026-10-01 09:48:00")
	if rep := wd.pass(); len(rep.Failed) != 1 || len(wd.buyers.confirmations) != 0 || !strings.Contains(wd.buyers.tickets[0], "never showed") {
		t.Fatalf("no log after the wait: %+v tickets=%v", rep, wd.buyers.tickets)
	}
}

func TestWriteOutcomes(t *testing.T) {
	// Refused and verified unchanged: the attempt is abandoned and the order can be planned again.
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.srv.writeStatus = missionwrite.StatusNotWritten
	rep := wd.pass()
	if len(rep.Abandoned) != 1 || wd.state("champion:d1:a1") != repository.AttemptAbandoned || wd.store.writes[0].Outcome != repository.ShopWriteNotWritten || len(wd.store.paused) != 0 {
		t.Fatalf("not written: %+v journal=%+v", rep, wd.store.writes[0])
	}
	wd.srv.writeStatus = ""
	wd.at("2026-10-01 08:32:00")
	if rep := wd.pass(); len(rep.Staged) != 1 || rep.Staged[0] != "champion:d1:a2" {
		t.Fatalf("second attempt: %+v", rep)
	}

	// Uncertain: the installation is paused, and nothing more is written until a person resumes.
	wd = newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.srv.writeStatus, wd.srv.storeAnyway = missionwrite.StatusUncertain, true
	rep = wd.pass()
	if rep.Paused == "" || len(wd.store.paused) != 1 || wd.store.writes[0].Outcome != repository.ShopWriteUncertain || wd.state("champion:d1:a1") != repository.AttemptFilePrepared {
		t.Fatalf("uncertain: %+v", rep)
	}
	wd.srv.writeStatus = ""
	inspects := wd.srv.inspects
	rep = wd.pass()
	if rep.Paused == "" || len(wd.srv.writes) != 1 || wd.srv.inspects != inspects {
		t.Fatalf("after an uncertain write the worker acted again: %+v writes=%d", rep, len(wd.srv.writes))
	}

	// The snapshot went stale between the inspection and the write: nothing sent, tried again later.
	wd = newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.srv.stale = true
	rep = wd.pass()
	if len(wd.srv.writes) != 0 || wd.state("champion:d1:a1") != repository.AttemptAbandoned || len(wd.store.paused) != 0 {
		t.Fatalf("stale: %+v", rep)
	}

	// The configuration changed during the write: recorded, then paused.
	wd = newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.srv.configChanged = true
	rep = wd.pass()
	if len(rep.Staged) != 1 || rep.Paused == "" || wd.state("champion:d1:a1") != repository.AttemptFileStaged || wd.store.writes[0].Outcome != repository.ShopWriteWrittenVerified {
		t.Fatalf("config changed during the write: %+v", rep)
	}
}

func TestTheWorkerPausesWhenTheServerIsNotWhatItExpects(t *testing.T) {
	cases := map[string]func(wd *world){
		"the Champion file is missing":     func(wd *world) { wd.srv.inspectErr = missionwrite.ErrArtifactMissing },
		"the file is no longer referenced": func(wd *world) { wd.srv.inspectErr = missionwrite.ErrArtifactNotReferenced },
		"another Nitrado service":          func(wd *world) { wd.srv.inspectErr = missionwrite.ErrBinding },
		"cfggameplay.json changed":         func(wd *world) { wd.srv.config = strings.Repeat("d", 64) },
		"the mission folder changed":       func(wd *world) { wd.srv.mission = "dayzps_missions/dayzOffline.enoch" },
		"the file holds a foreign entry": func(wd *world) {
			wd.srv.content = []byte(`{"Objects":[{"name":"Land_Castle","pos":[1,2,3],"ypr":[0,0,0],"scale":1,"enableCEPersistency":false,"customString":"mine"}]}`)
		},
		"the file holds an unknown attempt": func(wd *world) {
			wd.srv.content = nd.SpawnerFile{Objects: nd.AttemptEntries("champion:d9:a1", "BandageDressing", 1, [3]float64{1, 2, 3})}.Render()
		},
		"the file is not valid spawner JSON": func(wd *world) { wd.srv.content = []byte("{") },
	}
	for name, spoil := range cases {
		wd := newWorld(t, Config{Name: "t"})
		wd.order(1, "BandageDressing", 1)
		spoil(wd)
		rep := wd.pass()
		if rep.Paused == "" || len(wd.store.paused) != 1 || len(wd.srv.writes) != 0 || len(wd.store.writes) != 0 {
			t.Errorf("%s: %+v paused=%v writes=%d journal=%d", name, rep, wd.store.paused, len(wd.srv.writes), len(wd.store.writes))
		}
	}

	// A staged item that vanished from the file (someone emptied it) is also a mismatch.
	wd := stagedOrder(t)
	wd.srv.content = emptyFile
	if rep := wd.pass(); rep.Paused == "" || !strings.Contains(rep.Paused, "does not match") || len(wd.srv.writes) != 1 {
		t.Fatalf("emptied file: %+v", rep)
	}
}

func TestFirstPassAcceptsTheConfigurationOnce(t *testing.T) {
	wd := newWorld(t, Config{Name: "t"})
	wd.inst.ConfigSHA256, wd.inst.MissionPath = "", ""
	wd.order(1, "BandageDressing", 1)
	if rep := wd.pass(); len(rep.Staged) != 1 || len(wd.store.accepted) != 1 || wd.store.accepted[0] != strings.Repeat("c", 64)+"|dayzps_missions/dayzOffline.chernarusplus" {
		t.Fatalf("%+v accepted=%v", rep, wd.store.accepted)
	}
}

func TestRecoveryAfterTheWorkerStoppedMidWrite(t *testing.T) {
	// The upload landed but nothing was recorded: the ledger is completed from the journal's facts.
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.ledger.failNext[repository.AttemptFileStaged] = errors.New("database unavailable")
	if _, err := wd.w.Pass(context.Background(), wd.inst, wd.srv); err == nil {
		t.Fatal("the pass hid a ledger failure")
	}
	if wd.store.writes[0].Outcome != repository.ShopWriteStarted || wd.state("champion:d1:a1") != repository.AttemptFilePrepared || wd.fileIsEmpty() {
		t.Fatalf("after the failure: journal=%s state=%s", wd.store.writes[0].Outcome, wd.state("champion:d1:a1"))
	}
	stagedAt := wd.now
	wd.store.candidates = nil
	wd.at("2026-10-01 08:50:00")
	rep := wd.pass()
	a := wd.ledger.attempts["champion:d1:a1"]
	if len(rep.Staged) != 1 || a.State != repository.AttemptFileStaged || wd.store.writes[0].Outcome != repository.ShopWriteWrittenVerified || len(wd.srv.writes) != 1 {
		t.Fatalf("recovery: %+v state=%s journal=%s writes=%d", rep, a.State, wd.store.writes[0].Outcome, len(wd.srv.writes))
	}
	// staged_at is when the write BEGAN, and the boot is the one from before it: a restart during the
	// outage is then counted, never missed.
	if !a.StagedAt.Equal(stagedAt) || *a.StagedBootFile != bootA {
		t.Fatalf("recovered staging evidence: at=%v boot=%s", a.StagedAt, *a.StagedBootFile)
	}

	// The upload never landed: the journal row closes as NOT_WRITTEN and the attempt is abandoned.
	wd = newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.pass()
	wd.srv.content = emptyFile
	wd.ledger.attempts["champion:d1:a1"].State = repository.AttemptFilePrepared
	wd.store.writes[0].Outcome, wd.store.writes[0].FinishedAt = repository.ShopWriteStarted, nil
	rep = wd.pass()
	if wd.store.writes[0].Outcome != repository.ShopWriteNotWritten || wd.state("champion:d1:a1") != repository.AttemptAbandoned || len(wd.store.paused) != 0 {
		t.Fatalf("not landed: %+v journal=%s", rep, wd.store.writes[0].Outcome)
	}

	// The file is neither: uncertain, paused.
	wd = newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.pass()
	wd.srv.content = []byte(`{"Objects":[]}`)
	wd.ledger.attempts["champion:d1:a1"].State = repository.AttemptFilePrepared
	wd.store.writes[0].Outcome, wd.store.writes[0].FinishedAt = repository.ShopWriteStarted, nil
	rep = wd.pass()
	if rep.Paused == "" || wd.store.writes[0].Outcome != repository.ShopWriteUncertain || len(wd.srv.writes) != 1 {
		t.Fatalf("unexpected file: %+v journal=%s", rep, wd.store.writes[0].Outcome)
	}
}

func TestRecoveryAfterTheWorkerStoppedMidRemoval(t *testing.T) {
	wd := stagedOrder(t)
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.ledger.failNext[repository.AttemptVerificationRequired] = errors.New("database unavailable")
	wd.at("2026-10-01 09:17:00")
	if _, err := wd.w.Pass(context.Background(), wd.inst, wd.srv); err == nil {
		t.Fatal("the pass hid a ledger failure")
	}
	if !wd.fileIsEmpty() || wd.state("champion:d1:a1") != repository.AttemptUnstageRequired || wd.store.writes[1].Outcome != repository.ShopWriteStarted {
		t.Fatalf("after the failure: state=%s journal=%s", wd.state("champion:d1:a1"), wd.store.writes[1].Outcome)
	}
	rep := wd.pass()
	if len(rep.Unstaged) != 1 || wd.state("champion:d1:a1") != repository.AttemptVerificationRequired || wd.store.writes[1].Outcome != repository.ShopWriteWrittenVerified || len(wd.srv.writes) != 2 {
		t.Fatalf("recovery: %+v state=%s writes=%d", rep, wd.state("champion:d1:a1"), len(wd.srv.writes))
	}
}

func TestOneOrderAtATimeUntilBatchingIsApproved(t *testing.T) {
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.order(2, "Canteen", 1)
	if rep := wd.pass(); len(rep.Staged) != 1 || rep.Staged[0] != "champion:d1:a1" {
		t.Fatalf("%+v", rep)
	}
	wd.at("2026-10-01 08:40:00")
	if rep := wd.pass(); len(rep.Staged) != 0 || len(wd.srv.writes) != 1 {
		t.Fatalf("a second order was staged beside the first: %+v", rep)
	}

	// With batching allowed, the second order joins the file and both leave it after the restart.
	wd = newWorld(t, Config{Name: "t", MaxStaged: 3})
	wd.order(1, "BandageDressing", 1)
	wd.pass()
	wd.order(2, "Canteen", 2)
	wd.at("2026-10-01 08:40:00")
	if rep := wd.pass(); len(rep.Staged) != 1 || rep.Staged[0] != "champion:d2:a1" {
		t.Fatalf("%+v", rep)
	}
	file, err := nd.ParseSpawnerFile(wd.srv.content)
	if err != nil || len(file.Objects) != 3 {
		t.Fatalf("file has %d objects (%v): %s", len(file.Objects), err, wd.srv.content)
	}
	wd.srv.boots = append(wd.srv.boots, bootB)
	wd.at("2026-10-01 09:17:00")
	if rep := wd.pass(); len(rep.Unstaged) != 2 || !wd.fileIsEmpty() || len(wd.srv.writes) != 3 {
		t.Fatalf("%+v writes=%d", rep, len(wd.srv.writes))
	}
	if New(Config{MaxStaged: 999}, nil, nil, nil, nil).cfg.MaxStaged != nd.MaxStagedObjects/nd.MaxUnitsPerOrder {
		t.Fatal("MaxStaged is not capped by the file's object limit")
	}
}

func TestReportOnlyWritesNothingAnywhere(t *testing.T) {
	wd := newWorld(t, Config{Name: "t", ReportOnly: true})
	wd.inst.ConfigSHA256 = ""
	wd.order(1, "BandageDressing", 1)
	rep := wd.pass()
	if len(rep.WouldStage) != 1 || rep.WouldStage[0] != 1 || !rep.Inspected || rep.CurrentBoot != bootA {
		t.Fatalf("%+v", rep)
	}
	wd.srv.config = strings.Repeat("d", 64)
	wd.inst.ConfigSHA256 = strings.Repeat("c", 64)
	if rep := wd.pass(); rep.Paused == "" {
		t.Fatalf("report-only did not report the pause it would make: %+v", rep)
	}
	if len(wd.srv.writes)+len(wd.ledger.attempts)+len(wd.store.writes)+len(wd.store.paused)+len(wd.store.accepted)+len(wd.buyers.confirmations)+len(wd.buyers.tickets) != 0 {
		t.Fatalf("report-only wrote something: server=%d ledger=%d journal=%d paused=%v accepted=%v", len(wd.srv.writes), len(wd.ledger.attempts), len(wd.store.writes), wd.store.paused, wd.store.accepted)
	}
}

func TestAManualAttemptKeepsTheWorkerAway(t *testing.T) {
	wd := newWorld(t, Config{Name: "t"})
	wd.order(1, "BandageDressing", 1)
	wd.ledger.attempts["champion:d5:a1"] = &repository.ShopAttempt{AttemptID: "champion:d5:a1", DeliveryID: 5, State: repository.AttemptFileStaged, FulfilmentMode: repository.ShopFulfilmentObserved}
	wd.ledger.order = append(wd.ledger.order, "champion:d5:a1")
	rep := wd.pass()
	if !strings.Contains(rep.Skipped, "manual") || wd.srv.inspects != 0 || len(wd.srv.writes) != 0 || len(wd.ledger.attempts) != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestBootStart(t *testing.T) {
	offset := utcMinus4
	got, ok := BootStart(bootB, &offset)
	if !ok || !got.Equal(time.Date(2026, 10, 1, 13, 8, 0, 0, time.UTC)) {
		t.Fatalf("%v %v", got, ok)
	}
	if _, ok := BootStart(bootB, nil); ok {
		t.Fatal("a boot start without a clock offset")
	}
	if _, ok := BootStart("DayZServer_PS4_x64.ADM", &offset); ok {
		t.Fatal("a boot start from a name without a time")
	}
}
