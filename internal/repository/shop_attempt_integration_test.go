//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/database"
)

// Migration 0054 against a real PostgreSQL: the durable delivery-attempt ledger, its integration with
// the Shop refund/fulfil paths, and recovery. Nothing contacts a game server.

type attemptWorld struct {
	t        *testing.T
	ctx      context.Context
	db       *database.DB
	shop     *ShopRepository
	attempts *ShopAttemptRepository
	a, b     saasFixture
	playerA  int64
	playerB  int64
	seq      atomic.Int64
}

func newAttemptWorld(t *testing.T) *attemptWorld {
	t.Helper()
	db := saasIntegrationDB(t)
	w := &attemptWorld{t: t, ctx: context.Background(), db: db, shop: NewShopRepository(db.Pool), attempts: NewShopAttemptRepository(db.Pool)}
	w.a, w.b = newSaaSFixture(t, db), newSaaSFixture(t, db)
	players := NewPlayerRepository(db.Pool)
	var err error
	w.playerA, err = players.UpsertPlayer(w.ctx, w.a.GuildRowID, fmt.Sprintf("dz-att-a-%d", time.Now().UnixNano()), "Canary A", time.Now())
	must(t, err)
	w.playerB, err = players.UpsertPlayer(w.ctx, w.b.GuildRowID, fmt.Sprintf("dz-att-b-%d", time.Now().UnixNano()), "Canary B", time.Now())
	must(t, err)
	return w
}

type order struct {
	f                    saasFixture
	purchase, delivery   int64
	attemptN             int
	lastAttemptID, actor string
}

// order inserts an open 1-point MANUAL_COORDINATE purchase with its delivery, as Purchase() leaves it.
func (w *attemptWorld) order(f saasFixture) *order {
	w.t.Helper()
	player := w.playerA
	if f.OrgID == w.b.OrgID {
		player = w.playerB
	}
	o := &order{f: f, actor: "worker-test"}
	must(w.t, w.db.Pool.QueryRow(w.ctx, `INSERT INTO shop_purchases(organization_id, installation_id, game_server_id, player_id, status, total_points, delivery_type, idempotency_key, paid_at)
VALUES($1,$2,$3,$4,'PENDING_FULFILLMENT',1,'MANUAL',$5,NOW()) RETURNING id`, f.OrgID, f.InstallationID, f.ServerRowID, player, fmt.Sprintf("att-%d-%d", time.Now().UnixNano(), w.seq.Add(1))).Scan(&o.purchase))
	_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO shop_purchase_items(purchase_id, product_name, unit_price_points, quantity, line_total_points) VALUES($1,'Canary BandageDressing',1,1,1)`, o.purchase)
	must(w.t, err)
	must(w.t, w.db.Pool.QueryRow(w.ctx, `INSERT INTO shop_deliveries(purchase_id, organization_id, installation_id, game_server_id, player_id, delivery_policy, map_key, coord_x, coord_z, status)
VALUES($1,$2,$3,$4,$5,'MANUAL_COORDINATE','chernarusplus',4621.1,8397.2,'MANUAL_READY') RETURNING id`, o.purchase, f.OrgID, f.InstallationID, f.ServerRowID, player).Scan(&o.delivery))
	return o
}

func (w *attemptWorld) create(o *order) (ShopAttempt, error) {
	n := o.attemptN + 1
	a, err := w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, DeliveryID: o.delivery, Attempt: n,
		AttemptID: fmt.Sprintf("champion:d%d:a%d", o.delivery, n), Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1,
		PosX: 4621.1, PosY: 319.6, PosZ: 8397.2, DropSourceFile: "dayzps/config/DayZServer_PS4_x64_2026-09-24_20-46-53.ADM", DropSourceOffset: 853}, o.actor)
	if err == nil {
		o.attemptN = n
		o.lastAttemptID = a.AttemptID
	}
	return a, err
}

func (w *attemptWorld) mustCreate(o *order) ShopAttempt {
	w.t.Helper()
	a, err := w.create(o)
	must(w.t, err)
	return a
}

var (
	t0         = time.Date(2026, 9, 25, 2, 0, 0, 0, time.UTC)
	shaEmpty   = strings.Repeat("e", 64)
	shaStaged  = strings.Repeat("5", 64)
	stagedBoot = "dayzps/config/DayZServer_PS4_x64_2026-09-25_01-54-00.ADM"
	firstBoot  = "dayzps/config/DayZServer_PS4_x64_2026-09-25_02-10-00.ADM"
	secondBoot = "dayzps/config/DayZServer_PS4_x64_2026-09-25_03-18-00.ADM"
)

func sp(s string) *string       { return &s }
func tp(t time.Time) *time.Time { return &t }

// evidenceFor is the evidence each target state needs (identical values every time: evidence is write-once).
func evidenceFor(to string) ShopAttemptEvidence {
	switch to {
	case AttemptFileStaged:
		return ShopAttemptEvidence{BeforeSHA256: sp(shaEmpty), StagedSHA256: sp(shaStaged), StagedAt: tp(t0), StagedBootFile: sp(stagedBoot), Note: "staged read-back verified"}
	case AttemptRestartObserved:
		return ShopAttemptEvidence{RestartBootFile: sp(firstBoot), RestartObservedAt: tp(t0.Add(20 * time.Minute)), Note: "boot authority accepted"}
	case AttemptVerificationRequired, AttemptUnstaged:
		return ShopAttemptEvidence{UnstagedSHA256: sp(shaEmpty), UnstageVerifiedAt: tp(t0.Add(25 * time.Minute)), Note: "empty file read back"}
	case AttemptFailedReview:
		return ShopAttemptEvidence{FailureReason: sp("second boot before a verified unstage")}
	}
	return ShopAttemptEvidence{}
}

var legalPath = []string{AttemptPlanCreated, AttemptFilePrepared, AttemptFileStaged, AttemptAwaitingRestart, AttemptRestartObserved, AttemptUnstageRequired, AttemptVerificationRequired}

func (w *attemptWorld) step(o *order, from, to string) (ShopAttempt, error) {
	return w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, from, to, o.actor, evidenceFor(to))
}

// advance drives the order's current attempt along the legal path to `state`.
func (w *attemptWorld) advance(o *order, state string) {
	w.t.Helper()
	switch state {
	case AttemptAbandoned:
		_, err := w.step(o, AttemptPlanCreated, AttemptAbandoned)
		must(w.t, err)
		return
	case AttemptUnstaged, AttemptFailedReview:
		w.advance(o, AttemptFileStaged)
		_, err := w.step(o, AttemptFileStaged, state)
		must(w.t, err)
		return
	}
	for i := 1; i < len(legalPath); i++ {
		if legalPath[i-1] == state {
			return
		}
		if _, err := w.step(o, legalPath[i-1], legalPath[i]); err != nil {
			w.t.Fatalf("%s -> %s: %v", legalPath[i-1], legalPath[i], err)
		}
		if legalPath[i] == state {
			return
		}
	}
}

func physical() ShopAttemptPhysicalEvidence {
	return ShopAttemptPhysicalEvidence{ItemObservedBy: "owner-discord", ItemObservedAt: t0.Add(19 * time.Minute), PickedUpBy: "OwnerCharacter",
		PickupObservedAt: t0.Add(19*time.Minute + 30*time.Second), SecondBootFile: secondBoot, SecondBootStartedAt: t0.Add(88 * time.Minute),
		NoRespawnCheckedAt: t0.Add(98 * time.Minute), Note: "bandage seen and picked up; none after the second start"}
}

func (w *attemptWorld) statuses(o *order) (purchase, delivery string) {
	w.t.Helper()
	must(w.t, w.db.Pool.QueryRow(w.ctx, `SELECT sp.status, sd.status FROM shop_purchases sp JOIN shop_deliveries sd ON sd.purchase_id = sp.id WHERE sp.id=$1`, o.purchase).Scan(&purchase, &delivery))
	return
}

func (w *attemptWorld) refund(o *order) error {
	_, err := w.shop.Refund(w.ctx, RefundParams{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, GuildID: o.f.GuildRowID, ServerID: o.f.ServerRowID,
		PurchaseID: o.purchase, ActorUserID: o.f.OwnerUserID, ActorDiscordID: "admin-discord", Reason: "canary test"})
	return err
}

func (w *attemptWorld) manualFulfill(o *order) error {
	_, err := w.shop.Fulfill(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, o.f.OwnerUserID)
	return err
}

func wantIs(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v, want %v", what, err, want)
	}
}

func TestShopAttemptLifecycleAndFulfillment(t *testing.T) {
	w := newAttemptWorld(t)
	o := w.order(w.a)
	a := w.mustCreate(o)
	if a.State != AttemptPlanCreated || a.AttemptID != fmt.Sprintf("champion:d%d:a1", o.delivery) {
		t.Fatalf("%+v", a)
	}
	w.advance(o, AttemptVerificationRequired)

	// While the item may be on the server, the purchase can be neither refunded nor fulfilled by hand.
	wantIs(t, "refund during verification", w.refund(o), ErrShopDeliveryAttemptActive)
	wantIs(t, "manual fulfil during verification", w.manualFulfill(o), ErrShopDeliveryAttemptActive)
	if p, d := w.statuses(o); p != ShopStatusPendingFulfillment || d != DeliveryStatusManualReady {
		t.Fatalf("a refused refund changed state: %s %s", p, d)
	}
	// FULFILLED only through FulfillAttempt, never a bare transition.
	_, err := w.step(o, AttemptVerificationRequired, AttemptFulfilled)
	wantIs(t, "bare FULFILLED transition", err, ErrShopAttemptRejected)

	// Physical evidence is required and must be consistent.
	bad := map[string]func(*ShopAttemptPhysicalEvidence){
		"no observer":              func(e *ShopAttemptPhysicalEvidence) { e.ItemObservedBy = "" },
		"no pickup":                func(e *ShopAttemptPhysicalEvidence) { e.PickedUpBy = "" },
		"pickup before sighting":   func(e *ShopAttemptPhysicalEvidence) { e.PickupObservedAt = e.ItemObservedAt.Add(-time.Minute) },
		"seen before staging":      func(e *ShopAttemptPhysicalEvidence) { e.ItemObservedAt = t0.Add(-time.Minute) },
		"second boot = first boot": func(e *ShopAttemptPhysicalEvidence) { e.SecondBootFile = firstBoot },
		"second boot before unstage": func(e *ShopAttemptPhysicalEvidence) {
			e.SecondBootStartedAt = t0.Add(24 * time.Minute)
		},
		"check before second boot": func(e *ShopAttemptPhysicalEvidence) { e.NoRespawnCheckedAt = e.SecondBootStartedAt.Add(-time.Minute) },
		"no second boot":           func(e *ShopAttemptPhysicalEvidence) { e.SecondBootStartedAt = time.Time{} },
	}
	for name, mut := range bad {
		ev := physical()
		mut(&ev)
		_, err := w.attempts.FulfillAttempt(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, o.f.OwnerUserID, "owner-discord", ev)
		wantIs(t, name, err, ErrShopAttemptEvidence)
		if p, d := w.statuses(o); p != ShopStatusPendingFulfillment || d != DeliveryStatusManualReady {
			t.Fatalf("%s: a refused fulfilment changed state: %s %s", name, p, d)
		}
	}
	p, err := w.attempts.FulfillAttempt(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, o.f.OwnerUserID, "owner-discord", physical())
	must(t, err)
	if p.Status != ShopStatusFulfilled {
		t.Fatalf("purchase %s", p.Status)
	}
	if pp, d := w.statuses(o); pp != ShopStatusFulfilled || d != DeliveryStatusFulfilled {
		t.Fatalf("attempt, delivery and purchase move together: %s %s", pp, d)
	}
	got, err := w.attempts.Get(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID)
	must(t, err)
	if got.State != AttemptFulfilled || got.VerifiedBy == nil || *got.VerifiedBy != "owner-discord" || got.PickedUpBy == nil {
		t.Fatalf("%+v", got)
	}
	// Once only.
	_, err = w.attempts.FulfillAttempt(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, o.f.OwnerUserID, "owner-discord", physical())
	wantIs(t, "second fulfilment", err, ErrShopInvalidStatus)
	_, err = w.create(o)
	wantIs(t, "attempt after fulfilment", err, ErrShopAttemptDeliveryClosed)

	// History: created + 7 transitions, each with its actor; append-only.
	evs, err := w.attempts.Events(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID)
	must(t, err)
	if len(evs) != 8 || evs[0].ToState != AttemptPlanCreated || evs[7].ToState != AttemptFulfilled || evs[7].Actor != "owner-discord" || evs[1].Actor != "worker-test" {
		t.Fatalf("events: %+v", evs)
	}
	if _, err := w.db.Pool.Exec(w.ctx, `UPDATE shop_delivery_attempt_events SET actor='x' WHERE attempt_row_id=$1`, got.ID); err == nil {
		t.Fatal("history must be append-only")
	}
	// Evidence is write-once, identity immutable, terminal rows frozen (even with an actor).
	for name, sql := range map[string]string{
		"evidence":   `UPDATE shop_delivery_attempts SET staged_sha256=$2 WHERE id=$1`,
		"identity":   `UPDATE shop_delivery_attempts SET pos_y=1 WHERE id=$1 AND $2 <> ''`,
		"terminal":   `UPDATE shop_delivery_attempts SET updated_at=NOW() WHERE id=$1 AND $2 <> ''`,
		"resolution": `UPDATE shop_delivery_attempts SET review_resolution='SPAWNED', review_resolved_by='x', review_resolved_at=NOW() WHERE id=$1 AND $2 <> ''`,
	} {
		tx, err := w.db.Pool.Begin(w.ctx)
		must(t, err)
		must(t, setActor(w.ctx, tx, "tamper", ""))
		_, err = tx.Exec(w.ctx, sql, got.ID, strings.Repeat("f", 64))
		_ = tx.Rollback(w.ctx)
		if err == nil {
			t.Fatalf("%s: tampering accepted", name)
		}
	}
}

// Every (from, to) pair: allowed exactly when the Go state machine allows it.
func TestShopAttemptStateMachineIsExhaustive(t *testing.T) {
	w := newAttemptWorld(t)
	all := []string{AttemptPlanCreated, AttemptFilePrepared, AttemptFileStaged, AttemptAwaitingRestart, AttemptRestartObserved, AttemptUnstageRequired,
		AttemptVerificationRequired, AttemptAbandoned, AttemptUnstaged, AttemptFailedReview}
	allowed := func(from, to string) bool {
		for _, s := range ShopAttemptTransitions[from] {
			if s == to {
				return true
			}
		}
		return false
	}
	for _, from := range all {
		for _, to := range all {
			if from == to {
				continue
			}
			o := w.order(w.a)
			w.mustCreate(o)
			w.advance(o, from)
			_, err := w.step(o, from, to)
			switch {
			case allowed(from, to) && err != nil:
				t.Errorf("%s -> %s refused: %v", from, to, err)
			case !allowed(from, to) && !errors.Is(err, ErrShopAttemptRejected):
				t.Errorf("%s -> %s: got %v, want rejection", from, to, err)
			}
		}
	}
	// Missing evidence is refused on the legal transitions that need it.
	for _, c := range []struct{ from, to string }{{AttemptFilePrepared, AttemptFileStaged}, {AttemptAwaitingRestart, AttemptRestartObserved},
		{AttemptUnstageRequired, AttemptVerificationRequired}, {AttemptFileStaged, AttemptUnstaged}, {AttemptFileStaged, AttemptFailedReview}} {
		o := w.order(w.a)
		w.mustCreate(o)
		w.advance(o, c.from)
		_, err := w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, c.from, c.to, o.actor, ShopAttemptEvidence{})
		wantIs(t, c.from+" -> "+c.to+" without evidence", err, ErrShopAttemptEvidence)
	}
	// The restart must be a different boot from the one staging was verified in.
	o := w.order(w.a)
	w.mustCreate(o)
	w.advance(o, AttemptAwaitingRestart)
	_, err := w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, AttemptAwaitingRestart, AttemptRestartObserved, o.actor,
		ShopAttemptEvidence{RestartBootFile: sp(stagedBoot), RestartObservedAt: tp(t0.Add(time.Minute))})
	wantIs(t, "restart in the staged boot", err, ErrShopAttemptEvidence)
	// No actor, no change.
	_, err = w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, AttemptAwaitingRestart, AttemptUnstaged, " ", evidenceFor(AttemptUnstaged))
	wantIs(t, "no actor", err, ErrShopAttemptRejected)
	// The preview identity and a malformed id can never be stored.
	_, err = w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: w.a.OrgID, InstallationID: w.a.InstallationID, DeliveryID: w.order(w.a).delivery, Attempt: 1,
		AttemptID: "champion:d0:a1", Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1, PosX: 1, PosY: 1, PosZ: 1,
		DropSourceFile: "x.ADM", DropSourceOffset: 1}, "worker-test")
	wantIs(t, "placeholder attempt id", err, ErrShopAttemptEvidence)
}

func TestShopAttemptDuplicatesAndConcurrentTransitions(t *testing.T) {
	w := newAttemptWorld(t)
	o := w.order(w.a)
	var wg sync.WaitGroup
	var wins, conflicts atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			oo := *o
			_, err := w.create(&oo)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrShopAttemptConflict):
				conflicts.Add(1)
			default:
				t.Errorf("concurrent create: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 23 {
		t.Fatalf("concurrent creates: %d won, %d conflicts", wins.Load(), conflicts.Load())
	}
	o.attemptN, o.lastAttemptID = 1, fmt.Sprintf("champion:d%d:a1", o.delivery)
	// Attempt numbers must follow the delivery's history.
	fresh := w.order(w.a)
	fresh.attemptN = 2
	_, err := w.create(fresh)
	wantIs(t, "skipped attempt number", err, ErrShopAttemptSequence)

	var moved, stale atomic.Int64
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.step(o, AttemptPlanCreated, AttemptFilePrepared)
			switch {
			case err == nil:
				moved.Add(1)
			case errors.Is(err, ErrShopAttemptStale):
				stale.Add(1)
			default:
				t.Errorf("concurrent transition: %v", err)
			}
		}()
	}
	wg.Wait()
	if moved.Load() != 1 || stale.Load() != 23 {
		t.Fatalf("compare-and-set: %d moved, %d stale", moved.Load(), stale.Load())
	}
	open, err := w.attempts.ListOpen(w.ctx, w.a.OrgID, w.a.InstallationID)
	must(t, err)
	n := 0
	for _, a := range open {
		if a.DeliveryID == o.delivery {
			n++
			if a.State != AttemptFilePrepared {
				t.Fatalf("%+v", a)
			}
		}
	}
	if n != 1 {
		t.Fatalf("open attempts for the delivery: %d", n)
	}
}

func TestShopAttemptRecovery(t *testing.T) {
	w := newAttemptWorld(t)

	// 1. Worker crash / missing upload response: the attempt stays FILE_PREPARED. It is listed for
	//    reconciliation, blocks refund and manual fulfilment, and blocks a second attempt.
	crash := w.order(w.a)
	w.mustCreate(crash)
	w.advance(crash, AttemptFilePrepared)
	open, err := w.attempts.ListOpen(w.ctx, w.a.OrgID, w.a.InstallationID)
	must(t, err)
	found := false
	for _, a := range open {
		found = found || a.AttemptID == crash.lastAttemptID
	}
	if !found {
		t.Fatal("a crashed attempt must be listed for reconciliation")
	}
	wantIs(t, "refund with an upload in flight", w.refund(crash), ErrShopDeliveryAttemptActive)
	wantIs(t, "manual fulfil with an upload in flight", w.manualFulfill(crash), ErrShopDeliveryAttemptActive)
	_, err = w.create(crash)
	wantIs(t, "second attempt while one is open", err, ErrShopAttemptConflict)
	// Reconcile read the file: the upload landed -> FILE_STAGED with the read-back evidence. There is
	// no way back to FILE_PREPARED (never re-stage).
	w.advance(crash, AttemptFileStaged)
	_, err = w.step(crash, AttemptFileStaged, AttemptFilePrepared)
	wantIs(t, "back to FILE_PREPARED", err, ErrShopAttemptRejected)

	// Reconcile read the file: the upload never landed -> ABANDONED; then refund and a retry are safe.
	lost := w.order(w.a)
	w.mustCreate(lost)
	w.advance(lost, AttemptFilePrepared)
	_, err = w.step(lost, AttemptFilePrepared, AttemptAbandoned)
	must(t, err)
	w.mustCreate(lost) // attempt 2: attempt 1 is proven unwritten
	_, err = w.step(lost, AttemptPlanCreated, AttemptAbandoned)
	must(t, err)
	must(t, w.refund(lost))

	// 2. A second boot before the verified unstage: FAILED_REVIEW. Uncertain: no refund, no manual
	//    fulfilment, no new attempt - until a human resolves it; even then never a new attempt.
	for _, from := range []string{AttemptAwaitingRestart, AttemptUnstageRequired} {
		o := w.order(w.a)
		w.mustCreate(o)
		w.advance(o, from)
		_, err := w.step(o, from, AttemptFailedReview)
		must(t, err)
		_, err = w.step(o, AttemptFailedReview, AttemptVerificationRequired)
		wantIs(t, "leaving FAILED_REVIEW", err, ErrShopAttemptRejected)
		wantIs(t, "refund of an unresolved review", w.refund(o), ErrShopDeliveryAttemptActive)
		wantIs(t, "manual fulfil of an unresolved review", w.manualFulfill(o), ErrShopDeliveryAttemptActive)
		_, err = w.create(o)
		wantIs(t, "retry after an uncertain attempt", err, ErrShopAttemptConflict)
		if from == AttemptAwaitingRestart {
			// Resolved SPAWNED: still never refunded by this path; a manual fulfilment is now allowed.
			_, err = w.attempts.ResolveReview(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, ReviewSpawned, "owner-discord", "player confirmed")
			must(t, err)
			wantIs(t, "refund after SPAWNED", w.refund(o), ErrShopDeliveryAttemptActive)
			must(t, w.manualFulfill(o))
		} else {
			_, err = w.attempts.ResolveReview(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, ReviewNotSpawned, "owner-discord", "file never read: server log shows the error")
			must(t, err)
			must(t, w.refund(o))
			if p, d := w.statuses(o); p != ShopStatusRefunded || d != DeliveryStatusCancelled {
				t.Fatalf("refund after NOT_SPAWNED: %s %s", p, d)
			}
		}
		_, err = w.attempts.ResolveReview(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, ReviewNotSpawned, "owner-discord", "again")
		wantIs(t, "resolving twice", err, ErrShopAttemptStale)
		evs, err := w.attempts.Events(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID)
		must(t, err)
		if last := evs[len(evs)-1]; !strings.HasPrefix(last.Evidence, "review resolved:") || last.Actor != "owner-discord" {
			t.Fatalf("resolution must be in the history: %+v", last)
		}
	}

	// 3. A plan that never touched the server does not block a refund: it is abandoned with it.
	planned := w.order(w.a)
	w.mustCreate(planned)
	must(t, w.refund(planned))
	a, err := w.attempts.Get(w.ctx, w.a.OrgID, w.a.InstallationID, planned.lastAttemptID)
	must(t, err)
	if a.State != AttemptAbandoned {
		t.Fatalf("a refunded plan is abandoned: %s", a.State)
	}
	_, err = w.create(planned)
	wantIs(t, "attempt on a refunded delivery", err, ErrShopAttemptDeliveryClosed)

	// 4. Unstaged before any boot: proven unspawned; refund allowed.
	un := w.order(w.a)
	w.mustCreate(un)
	w.advance(un, AttemptUnstaged)
	must(t, w.refund(un))
}

func TestShopAttemptTenantIsolation(t *testing.T) {
	w := newAttemptWorld(t)
	o := w.order(w.a)
	a := w.mustCreate(o)
	b := w.b
	_, err := w.attempts.Transition(w.ctx, b.OrgID, b.InstallationID, a.AttemptID, AttemptPlanCreated, AttemptFilePrepared, "other", ShopAttemptEvidence{})
	wantIs(t, "cross-tenant transition", err, ErrShopAttemptNotFound)
	_, err = w.attempts.Get(w.ctx, b.OrgID, b.InstallationID, a.AttemptID)
	wantIs(t, "cross-tenant get", err, ErrShopAttemptNotFound)
	_, err = w.attempts.ResolveReview(w.ctx, b.OrgID, b.InstallationID, a.AttemptID, ReviewNotSpawned, "other", "")
	wantIs(t, "cross-tenant resolve", err, ErrShopAttemptNotFound)
	_, err = w.attempts.FulfillAttempt(w.ctx, b.OrgID, b.InstallationID, a.AttemptID, b.OwnerUserID, "other", physical())
	wantIs(t, "cross-tenant fulfil", err, ErrShopAttemptNotFound)
	evs, err := w.attempts.Events(w.ctx, b.OrgID, b.InstallationID, a.AttemptID)
	if err != nil || len(evs) != 0 {
		t.Fatalf("cross-tenant history: %v %v", evs, err)
	}
	open, err := w.attempts.ListOpen(w.ctx, b.OrgID, b.InstallationID)
	must(t, err)
	for _, x := range open {
		if x.OrganizationID != b.OrgID {
			t.Fatalf("another tenant's attempt listed: %+v", x)
		}
	}
	// Another tenant cannot attach an attempt to this delivery.
	_, err = w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: b.OrgID, InstallationID: b.InstallationID, DeliveryID: o.delivery, Attempt: 2,
		AttemptID: fmt.Sprintf("champion:d%d:a2", o.delivery), Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1,
		PosX: 1, PosY: 1, PosZ: 1, DropSourceFile: "x.ADM", DropSourceOffset: 1}, "other")
	wantIs(t, "cross-tenant create", err, ErrShopAttemptDeliveryClosed)
	if got, _ := w.attempts.Get(w.ctx, w.a.OrgID, w.a.InstallationID, a.AttemptID); got.State != AttemptPlanCreated {
		t.Fatalf("the owner's attempt changed: %+v", got)
	}
}

// Refund versus fulfilment/transition races: the outcome is always one consistent history.
func TestShopAttemptRefundRaces(t *testing.T) {
	w := newAttemptWorld(t)

	// PLAN_CREATED: a refund races the worker's PLAN_CREATED -> FILE_PREPARED. Either the refund wins
	// (the attempt is abandoned, the worker loses its compare-and-set) or the worker wins (the refund
	// is refused). Never a cancelled delivery with a prepared attempt.
	refundsWon, workerWon := 0, 0
	for i := 0; i < 12; i++ {
		o := w.order(w.a)
		w.mustCreate(o)
		var wg sync.WaitGroup
		var refundErr, stepErr error
		wg.Add(2)
		go func() { defer wg.Done(); refundErr = w.refund(o) }()
		go func() { defer wg.Done(); _, stepErr = w.step(o, AttemptPlanCreated, AttemptFilePrepared) }()
		wg.Wait()
		a, err := w.attempts.Get(w.ctx, w.a.OrgID, w.a.InstallationID, o.lastAttemptID)
		must(t, err)
		p, d := w.statuses(o)
		switch {
		case refundErr == nil && stepErr != nil:
			refundsWon++
			if a.State != AttemptAbandoned || p != ShopStatusRefunded || d != DeliveryStatusCancelled {
				t.Fatalf("refund won but state is %s/%s/%s", a.State, p, d)
			}
			if !errors.Is(stepErr, ErrShopAttemptStale) && !errors.Is(stepErr, ErrShopAttemptDeliveryClosed) {
				t.Fatalf("worker error: %v", stepErr)
			}
		case stepErr == nil && errors.Is(refundErr, ErrShopDeliveryAttemptActive):
			workerWon++
			if a.State != AttemptFilePrepared || p != ShopStatusPendingFulfillment || d != DeliveryStatusManualReady {
				t.Fatalf("worker won but state is %s/%s/%s", a.State, p, d)
			}
		default:
			t.Fatalf("inconsistent race: refund=%v step=%v state=%s/%s/%s", refundErr, stepErr, a.State, p, d)
		}
	}
	t.Logf("PLAN_CREATED races: refund won %d, worker won %d", refundsWon, workerWon)

	// VERIFICATION_REQUIRED: refund vs FulfillAttempt. The fulfilment always succeeds; the refund is
	// either refused (attempt still open) or, after the fulfilment committed, a refund of a delivered
	// order (existing Shop rule) that keeps the delivery FULFILLED.
	for i := 0; i < 8; i++ {
		o := w.order(w.a)
		w.mustCreate(o)
		w.advance(o, AttemptVerificationRequired)
		var wg sync.WaitGroup
		var refundErr, fulfilErr error
		wg.Add(2)
		go func() { defer wg.Done(); refundErr = w.refund(o) }()
		go func() {
			defer wg.Done()
			_, fulfilErr = w.attempts.FulfillAttempt(w.ctx, o.f.OrgID, o.f.InstallationID, o.lastAttemptID, o.f.OwnerUserID, "owner-discord", physical())
		}()
		wg.Wait()
		must(t, fulfilErr)
		p, d := w.statuses(o)
		if d != DeliveryStatusFulfilled {
			t.Fatalf("delivery must be FULFILLED, is %s", d)
		}
		switch {
		case errors.Is(refundErr, ErrShopDeliveryAttemptActive):
			if p != ShopStatusFulfilled {
				t.Fatalf("purchase %s", p)
			}
		case refundErr == nil:
			if p != ShopStatusRefunded {
				t.Fatalf("purchase %s", p)
			}
		default:
			t.Fatalf("refund: %v", refundErr)
		}
	}

	// Manual fulfil vs worker FILE_PREPARED: same exclusivity as the refund.
	for i := 0; i < 8; i++ {
		o := w.order(w.a)
		w.mustCreate(o)
		var wg sync.WaitGroup
		var fulErr, stepErr error
		wg.Add(2)
		go func() { defer wg.Done(); fulErr = w.manualFulfill(o) }()
		go func() { defer wg.Done(); _, stepErr = w.step(o, AttemptPlanCreated, AttemptFilePrepared) }()
		wg.Wait()
		a, _ := w.attempts.Get(w.ctx, w.a.OrgID, w.a.InstallationID, o.lastAttemptID)
		_, d := w.statuses(o)
		okManual := fulErr == nil && stepErr != nil && a.State == AttemptAbandoned && d == DeliveryStatusFulfilled
		okWorker := stepErr == nil && errors.Is(fulErr, ErrShopDeliveryAttemptActive) && a.State == AttemptFilePrepared && d == DeliveryStatusManualReady
		if !okManual && !okWorker {
			t.Fatalf("inconsistent manual-fulfil race: fulfil=%v step=%v state=%s delivery=%s", fulErr, stepErr, a.State, d)
		}
	}
}
