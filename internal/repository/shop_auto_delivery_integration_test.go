//go:build integration

package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Migrations 0091 and 0092 against a real PostgreSQL: the worker's fulfilment path in the ledger,
// the owner's switch and the pause, the lease, the write journal and the reads a pass decides on.
// Nothing contacts a game server.

type autoWorld struct {
	*confirmationWorld
	auto *ShopAutoDeliveryRepository
}

func newAutoWorld(t *testing.T) *autoWorld {
	c := newConfirmationWorld(t)
	return &autoWorld{confirmationWorld: c, auto: NewShopAutoDeliveryRepository(c.db.Pool)}
}

const autoBoot = "DayZServer_PS4_x64_2026-10-01_08-00-00.ADM"

// buyerAttempt creates a worker-mode attempt for the order and walks it to VERIFICATION_REQUIRED.
func (w *autoWorld) buyerAttempt(o *order) ShopAttempt {
	w.t.Helper()
	id := fmt.Sprintf("champion:d%d:a1", o.delivery)
	_, err := w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, DeliveryID: o.delivery, Attempt: 1,
		AttemptID: id, Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1, PosX: 4621.1, PosY: 319.6, PosZ: 8397.2,
		DropSourceFile: autoBoot, DropSourceOffset: 853, ArtifactPath: ShopAttemptCustomArtifactPath, FulfilmentMode: ShopFulfilmentBuyer}, "worker:test")
	must(w.t, err)
	staged, unstaged := strings.Repeat("1", 64), strings.Repeat("2", 64)
	t0 := time.Now().Add(-2 * time.Hour)
	t1, t2 := t0.Add(40*time.Minute), t0.Add(50*time.Minute)
	second := "DayZServer_PS4_x64_2026-10-01_09-08-00.ADM"
	boot := autoBoot
	steps := []struct {
		from, to string
		ev       ShopAttemptEvidence
	}{
		{AttemptPlanCreated, AttemptFilePrepared, ShopAttemptEvidence{}},
		{AttemptFilePrepared, AttemptFileStaged, ShopAttemptEvidence{StagedSHA256: &staged, StagedAt: &t0, StagedBootFile: &boot}},
		{AttemptFileStaged, AttemptAwaitingRestart, ShopAttemptEvidence{}},
		{AttemptAwaitingRestart, AttemptRestartObserved, ShopAttemptEvidence{RestartBootFile: &second, RestartObservedAt: &t1}},
		{AttemptRestartObserved, AttemptUnstageRequired, ShopAttemptEvidence{}},
		{AttemptUnstageRequired, AttemptVerificationRequired, ShopAttemptEvidence{UnstagedSHA256: &unstaged, UnstageVerifiedAt: &t2}},
	}
	var a ShopAttempt
	for _, s := range steps {
		a, err = w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, id, s.from, s.to, "worker:test", s.ev)
		must(w.t, err)
	}
	return a
}

func TestWorkerAttemptIsFulfilledOnlyByTheBuyersAnswer(t *testing.T) {
	w := newAutoWorld(t)
	o := w.order(w.a)
	a := w.buyerAttempt(o)
	if a.FulfilmentMode != ShopFulfilmentBuyer || a.BuyerAnswer != nil {
		t.Fatalf("%+v", a)
	}
	fulfil := func(answer string) error {
		_, err := w.attempts.FulfillAttemptByBuyer(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, answer, time.Now(), "worker:test")
		return err
	}

	// No confirmation yet, then a confirmation the buyer has not answered: both refused by the database.
	wantIs(t, "fulfil before the buyer was asked", fulfil(BuyerAnswerReceived), ErrShopAttemptRejected)
	must(t, w.conf.OpenForDelivered(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase))
	must(t, w.conf.OpenForDelivered(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase)) // idempotent
	c, err := w.conf.GetConfirmation(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, 0)
	must(t, err)
	if c.State != ConfirmationAwaitingBuyer {
		t.Fatalf("confirmation %+v", c)
	}
	wantIs(t, "fulfil while the buyer has not answered", fulfil(BuyerAnswerReceived), ErrShopAttemptRejected)
	wantIs(t, "an unknown answer", fulfil("YES"), ErrShopAttemptRejected)
	// While the buyer is being asked the item may be on the server: no refund, no manual fulfilment.
	wantIs(t, "refund while awaiting the buyer", w.refund(o), ErrShopDeliveryAttemptActive)
	wantIs(t, "manual fulfil while awaiting the buyer", w.manualFulfill(o), ErrShopDeliveryAttemptActive)

	_, err = w.conf.ConfirmReceived(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, w.playerA, ConfirmationViaDiscord)
	must(t, err)
	// The answer on record is RECEIVED: claiming the deadline passed instead is refused.
	wantIs(t, "an answer the buyer did not give", fulfil(BuyerAnswerAutoCompleted), ErrShopAttemptRejected)
	if p, d := w.statuses(o); p != ShopStatusPendingFulfillment || d != DeliveryStatusManualReady {
		t.Fatalf("a refused fulfilment changed state: %s %s", p, d)
	}
	must(t, fulfil(BuyerAnswerReceived))
	if p, d := w.statuses(o); p != ShopStatusFulfilled || d != DeliveryStatusFulfilled {
		t.Fatalf("after fulfilment: %s %s", p, d)
	}
	got, err := w.attempts.Get(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID)
	must(t, err)
	if got.State != AttemptFulfilled || got.BuyerAnswer == nil || *got.BuyerAnswer != BuyerAnswerReceived || got.BuyerAnsweredAt == nil ||
		got.VerifiedBy == nil || *got.VerifiedBy != "worker:test" || got.ItemObservedBy != nil {
		t.Fatalf("fulfilled attempt %+v", got)
	}
	// The purchase becoming FULFILLED does not reopen or change the buyer's confirmation.
	if state := w.state(o); state != ConfirmationReceived {
		t.Fatalf("confirmation after fulfilment = %s", state)
	}
	wantIs(t, "a second fulfilment", fulfil(BuyerAnswerReceived), ErrShopInvalidStatus)
}

func TestWorkerAttemptIsFulfilledByTheDeadline(t *testing.T) {
	w := newAutoWorld(t)
	o := w.order(w.a)
	a := w.buyerAttempt(o)
	must(t, w.conf.OpenForDelivered(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase))
	w.expire(o)
	_, err := w.conf.AutoCompleteDue(w.ctx, time.Now(), 500)
	must(t, err)
	_, err = w.attempts.FulfillAttemptByBuyer(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, BuyerAnswerAutoCompleted, time.Now(), "worker:test")
	must(t, err)
	// Silence is not consent: the buyer can still report an issue on the fulfilled order.
	_, ticket, err := w.issue(o, "found out late it never arrived")
	must(t, err)
	if ticket.Status != TicketOpen {
		t.Fatalf("ticket %+v", ticket)
	}
}

func TestFulfilmentModesDoNotMix(t *testing.T) {
	w := newAutoWorld(t)

	// A manual (OBSERVED) attempt is never fulfilled by a buyer answer.
	manual := w.order(w.a)
	m := w.mustCreate(manual)
	w.advance(manual, AttemptVerificationRequired)
	must(t, w.conf.OpenForDelivered(w.ctx, manual.f.OrgID, manual.f.InstallationID, manual.purchase))
	_, err := w.conf.ConfirmReceived(w.ctx, manual.f.OrgID, manual.f.InstallationID, manual.purchase, w.playerA, ConfirmationViaSite)
	must(t, err)
	_, err = w.attempts.FulfillAttemptByBuyer(w.ctx, manual.f.OrgID, manual.f.InstallationID, m.AttemptID, BuyerAnswerReceived, time.Now(), "worker:test")
	wantIs(t, "buyer fulfilment of a manual attempt", err, ErrShopAttemptStale)

	// A worker (BUYER) attempt is never fulfilled with recorded observations instead of the answer.
	auto := w.order(w.a)
	a := w.buyerAttempt(auto)
	_, err = w.attempts.FulfillAttempt(w.ctx, auto.f.OrgID, auto.f.InstallationID, a.AttemptID, auto.f.OwnerUserID, "owner-discord", physical())
	if err == nil {
		t.Fatal("a worker attempt was fulfilled without the buyer's answer")
	}
	if p, d := w.statuses(auto); p != ShopStatusPendingFulfillment || d != DeliveryStatusManualReady {
		t.Fatalf("a refused fulfilment changed state: %s %s", p, d)
	}

	// Direct SQL cannot change the mode or attach an answer either.
	for what, sql := range map[string]string{
		"switching the mode":                    `UPDATE shop_delivery_attempts SET fulfilment_mode='OBSERVED' WHERE attempt_id=$1`,
		"an answer without fulfilment":          `UPDATE shop_delivery_attempts SET buyer_answer='RECEIVED', buyer_answered_at=NOW() WHERE attempt_id=$1`,
		"fulfilment without the buyer's answer": `UPDATE shop_delivery_attempts SET state='FULFILLED', verified_by='x', fulfilled_at=NOW(), buyer_answer='RECEIVED', buyer_answered_at=NOW() WHERE attempt_id=$1`,
	} {
		tx, err := w.db.Pool.Begin(w.ctx)
		must(t, err)
		_, err = tx.Exec(w.ctx, `SELECT set_config('champion.actor', 'test', true)`)
		must(t, err)
		if _, err = tx.Exec(w.ctx, sql, a.AttemptID); err == nil {
			err = tx.Commit(w.ctx)
		}
		_ = tx.Rollback(w.ctx)
		if err == nil {
			t.Fatalf("%s was accepted by the database", what)
		}
	}
	if _, err := w.attempts.Create(w.ctx, ShopAttemptCreate{FulfilmentMode: "LATER"}, "worker:test"); !errors.Is(err, ErrShopAttemptRejected) {
		t.Fatalf("an unknown fulfilment mode: %v", err)
	}
}

func TestAutoDeliverySettingsPauseAndResume(t *testing.T) {
	w := newAutoWorld(t)
	org, inst := w.a.OrgID, w.a.InstallationID

	s, err := w.auto.Settings(w.ctx, org, inst)
	must(t, err)
	if s.Enabled || s.PausedAt != nil {
		t.Fatalf("default settings %+v", s)
	}
	_, err = w.auto.Settings(w.ctx, w.b.OrgID, inst)
	wantIs(t, "another organization's read", err, ErrShopAutoDeliveryNotFound)

	// A pause before the owner ever enabled it is still recorded, and enabling never clears it.
	must(t, w.auto.Pause(w.ctx, org, inst, "cfggameplay.json changed"))
	must(t, w.auto.Pause(w.ctx, org, inst, "a later consequence"))
	must(t, w.auto.SetEnabled(w.ctx, org, inst, true, w.a.OwnerUserID))
	s, err = w.auto.Settings(w.ctx, org, inst)
	must(t, err)
	if !s.Enabled || s.PausedAt == nil || s.PausedReason != "cfggameplay.json changed" {
		t.Fatalf("after pause + enable: %+v", s)
	}
	must(t, w.auto.Resume(w.ctx, org, inst, w.a.OwnerUserID))
	s, err = w.auto.Settings(w.ctx, org, inst)
	must(t, err)
	if !s.Enabled || s.PausedAt != nil || s.PausedReason != "" {
		t.Fatalf("after resume: %+v", s)
	}
	must(t, w.auto.SetEnabled(w.ctx, org, inst, false, w.a.OwnerUserID))
	if s, _ := w.auto.Settings(w.ctx, org, inst); s.Enabled {
		t.Fatal("still enabled after the owner switched it off")
	}
}

func (w *autoWorld) ready(f saasFixture) {
	w.t.Helper()
	_, err := w.db.Pool.Exec(w.ctx, `UPDATE installations SET status='READY' WHERE id=$1`, f.InstallationID)
	must(w.t, err)
	must(w.t, w.auto.SetEnabled(w.ctx, f.OrgID, f.InstallationID, true, f.OwnerUserID))
}

func TestClaimLeasesOnlyEnabledUnpausedListedInstallations(t *testing.T) {
	w := newAutoWorld(t)
	a, b := w.a.InstallationID, w.b.InstallationID
	now := time.Now()
	claim := func(owner string, at time.Time, allowed ...int64) []int64 {
		t.Helper()
		got, err := w.auto.ClaimInstallations(w.ctx, at, owner, 4*time.Minute, allowed)
		must(t, err)
		var ids []int64
		for _, g := range got {
			if g.InstallationID == a || g.InstallationID == b {
				ids = append(ids, g.InstallationID)
			}
		}
		return ids
	}

	if got := claim("w1", now, a, b); len(got) != 0 {
		t.Fatalf("claimed %v before any owner enabled automatic delivery", got)
	}
	w.ready(w.a)
	w.ready(w.b)
	if got := claim("w1", now); len(got) != 0 {
		t.Fatalf("claimed %v with an empty allow list", got)
	}
	if got := claim("w1", now, a); len(got) != 1 || got[0] != a {
		t.Fatalf("claimed %v, want only the listed installation", got)
	}
	got, err := w.auto.ClaimInstallations(w.ctx, now, "w1", 4*time.Minute, []int64{a})
	must(t, err)
	if len(got) != 1 || got[0].OrganizationID != w.a.OrgID || got[0].GameServerID != w.a.ServerRowID || got[0].GuildRowID != w.a.GuildRowID ||
		!strings.HasPrefix(got[0].NitradoServiceID, "saas-svc-") || got[0].ConfigSHA256 != "" {
		t.Fatalf("claimed installation %+v", got)
	}
	// Another worker gets nothing while the lease is held, and takes over once it has ended.
	if got := claim("w2", now.Add(time.Minute), a); len(got) != 0 {
		t.Fatalf("a second worker claimed a leased installation: %v", got)
	}
	if got := claim("w2", now.Add(5*time.Minute), a); len(got) != 1 {
		t.Fatalf("the lease never ended: %v", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, a, "w1")) // not the holder any more: no effect
	if got := claim("w1", now.Add(6*time.Minute), a); len(got) != 0 {
		t.Fatalf("a stale release freed another worker's lease: %v", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, a, "w2"))
	if got := claim("w1", now.Add(6*time.Minute), a); len(got) != 1 {
		t.Fatalf("a released lease was not free: %v", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, a, "w1"))

	// The accepted configuration is written once and handed to later passes.
	sha := strings.Repeat("c", 64)
	must(t, w.auto.AcceptConfiguration(w.ctx, w.a.OrgID, a, sha, "dayzps_missions/dayzOffline.chernarusplus"))
	must(t, w.auto.AcceptConfiguration(w.ctx, w.a.OrgID, a, strings.Repeat("d", 64), "other"))
	got, err = w.auto.ClaimInstallations(w.ctx, now.Add(7*time.Minute), "w1", 4*time.Minute, []int64{a})
	must(t, err)
	if len(got) != 1 || got[0].ConfigSHA256 != sha || got[0].MissionPath != "dayzps_missions/dayzOffline.chernarusplus" {
		t.Fatalf("accepted configuration %+v", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, a, "w1"))

	// Paused, switched off, or not READY: not claimed.
	must(t, w.auto.Pause(w.ctx, w.a.OrgID, a, "test"))
	if got := claim("w1", now.Add(8*time.Minute), a, b); len(got) != 1 || got[0] != b {
		t.Fatalf("claimed %v, want only the unpaused installation", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, b, "w1"))
	must(t, w.auto.Resume(w.ctx, w.a.OrgID, a, w.a.OwnerUserID))
	got, err = w.auto.ClaimInstallations(w.ctx, now.Add(9*time.Minute), "w1", 4*time.Minute, []int64{a})
	must(t, err)
	if len(got) != 1 || got[0].ConfigSHA256 != "" {
		t.Fatalf("after a resume the configuration must be accepted again: %+v", got)
	}
	must(t, w.auto.ReleaseLease(w.ctx, a, "w1"))
	_, err = w.db.Pool.Exec(w.ctx, `UPDATE installations SET status='SUSPENDED' WHERE id=$1`, a)
	must(t, err)
	if got := claim("w1", now.Add(10*time.Minute), a); len(got) != 0 {
		t.Fatalf("a suspended installation was claimed: %v", got)
	}
}

func TestWriteJournalAllowsOneUnresolvedWrite(t *testing.T) {
	w := newAutoWorld(t)
	w.ready(w.a)
	org, inst := w.a.OrgID, w.a.InstallationID
	before, payload := strings.Repeat("a", 64), strings.Repeat("b", 64)
	entry := ShopDeliveryWrite{OrganizationID: org, InstallationID: inst, Kind: ShopWriteStage, AttemptIDs: []string{"champion:d1:a1"},
		BeforeSHA256: before, PayloadSHA256: payload, BootFile: autoBoot, Worker: "worker:test"}

	first, err := w.auto.BeginWrite(w.ctx, entry)
	must(t, err)
	if first.Outcome != ShopWriteStarted || first.FinishedAt != nil || first.StartedAt.IsZero() || len(first.AttemptIDs) != 1 {
		t.Fatalf("%+v", first)
	}
	_, err = w.auto.BeginWrite(w.ctx, entry)
	wantIs(t, "a second write while one is unfinished", err, ErrShopWriteOutstanding)
	open, err := w.auto.UnresolvedWrite(w.ctx, inst)
	must(t, err)
	if open == nil || open.ID != first.ID {
		t.Fatalf("unresolved = %+v", open)
	}
	must(t, w.auto.FinishWrite(w.ctx, first.ID, ShopWriteWrittenVerified, payload, "read-back matches"))
	if err := w.auto.FinishWrite(w.ctx, first.ID, ShopWriteNotWritten, "", "again"); err == nil {
		t.Fatal("a finished write was finished again")
	}
	if err := w.auto.FinishWrite(w.ctx, first.ID, "DONE", "", ""); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if open, _ := w.auto.UnresolvedWrite(w.ctx, inst); open != nil {
		t.Fatalf("a verified write is still unresolved: %+v", open)
	}

	// An uncertain write blocks the server until a person resumes it.
	second, err := w.auto.BeginWrite(w.ctx, entry)
	must(t, err)
	must(t, w.auto.FinishWrite(w.ctx, second.ID, ShopWriteUncertain, "", "the read-back failed"))
	_, err = w.auto.BeginWrite(w.ctx, entry)
	wantIs(t, "a write after an uncertain one", err, ErrShopWriteOutstanding)
	must(t, w.auto.Resume(w.ctx, org, inst, w.a.OwnerUserID))
	if open, _ := w.auto.UnresolvedWrite(w.ctx, inst); open != nil {
		t.Fatalf("still unresolved after a resume: %+v", open)
	}
	third, err := w.auto.BeginWrite(w.ctx, entry)
	must(t, err)
	must(t, w.auto.FinishWrite(w.ctx, third.ID, ShopWriteNotWritten, before, "refused"))

	// Another installation is independent, and a write that changes nothing is not a write.
	w.ready(w.b)
	other := entry
	other.OrganizationID, other.InstallationID = w.b.OrgID, w.b.InstallationID
	_, err = w.auto.BeginWrite(w.ctx, other)
	must(t, err)
	same := entry
	same.PayloadSHA256 = before
	if _, err := w.auto.BeginWrite(w.ctx, same); err == nil {
		t.Fatal("a write whose payload equals the current file was journaled")
	}
}

// autoProduct makes the order's single item a product with the given automatic-delivery setting.
func (w *autoWorld) autoProduct(o *order, auto bool, class *string) int64 {
	w.t.Helper()
	var id int64
	must(w.t, w.db.Pool.QueryRow(w.ctx, `INSERT INTO shop_products(organization_id, installation_id, name, slug, price_points, product_type, delivery_policy, auto_delivery, class_name)
VALUES($1,$2,'Bandage',$3,1,'ITEM','MANUAL_COORDINATE',$4,$5) RETURNING id`, o.f.OrgID, o.f.InstallationID, fmt.Sprintf("bandage-%d", time.Now().UnixNano()), auto, class).Scan(&id))
	_, err := w.db.Pool.Exec(w.ctx, `UPDATE shop_purchase_items SET product_id=$2 WHERE purchase_id=$1`, o.purchase, id)
	must(w.t, err)
	return id
}

// logged records a player-list observation of installation A's buyer.
func (w *autoWorld) logged(f saasFixture, x, z float64, y *float64, offset int64, at time.Time) {
	w.t.Helper()
	_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO player_location_events(guild_id, server_id, player_id, gamertag, x, z, y, event_type, observed_at, source_file, source_offset)
VALUES($1,$2,$3,'Canary A',$4,$5,$6,'PLAYER_LIST',$7,$8,$9)`, f.GuildRowID, f.ServerRowID, w.playerA, x, z, y, at, autoBoot, offset)
	must(w.t, err)
}

func TestCandidatesAreOpenAutomaticOrdersAtALoggedPosition(t *testing.T) {
	w := newAutoWorld(t)
	class, alt := "BandageDressing", 319.6
	find := func(o *order) *ShopAutoCandidate {
		t.Helper()
		got, err := w.auto.Candidates(w.ctx, o.f.OrgID, o.f.InstallationID, o.f.ServerRowID, 50)
		must(t, err)
		for i := range got {
			if got[i].DeliveryID == o.delivery {
				return &got[i]
			}
		}
		return nil
	}

	o := w.order(w.a)
	product := w.autoProduct(o, true, &class)
	if find(o) != nil {
		t.Fatal("an order with no logged position qualified")
	}
	// A position far from the delivery point, and one without an altitude, do not make a drop point.
	w.logged(w.a, 4700, 8397.2, &alt, 100, time.Now().Add(-30*time.Minute))
	w.logged(w.a, 4621.1, 8397.2, nil, 200, time.Now().Add(-20*time.Minute))
	if find(o) != nil {
		t.Fatal("an order qualified without a matching position that has an altitude")
	}
	older := 318.0
	w.logged(w.a, 4621.1, 8397.2, &older, 300, time.Now().Add(-15*time.Minute))
	w.logged(w.a, 4621.1, 8397.2, &alt, 853, time.Now().Add(-10*time.Minute))
	c := find(o)
	if c == nil || c.ClassName != class || c.AltitudeY != alt || c.DropSourceFile != autoBoot || c.DropSourceOffset != 853 || c.PurchaseID != o.purchase || c.PlayerID != w.playerA {
		t.Fatalf("candidate = %+v, want the newest matching observation", c)
	}
	// Another tenant never sees it.
	if got, err := w.auto.Candidates(w.ctx, w.b.OrgID, w.b.InstallationID, w.b.ServerRowID, 50); err != nil || len(got) != 0 {
		t.Fatalf("another tenant's candidates: %v %v", got, err)
	}

	// The owner switches the product off: the order goes back to the manual queue.
	_, err := w.db.Pool.Exec(w.ctx, `UPDATE shop_products SET auto_delivery=FALSE WHERE id=$1`, product)
	must(t, err)
	if find(o) != nil {
		t.Fatal("an order of a product that is no longer automatic qualified")
	}
	_, err = w.db.Pool.Exec(w.ctx, `UPDATE shop_products SET auto_delivery=TRUE WHERE id=$1`, product)
	must(t, err)

	// An open attempt takes the order out; an attempt proven to have spawned nothing puts it back.
	a, err := w.attempts.Create(w.ctx, ShopAttemptCreate{OrganizationID: o.f.OrgID, InstallationID: o.f.InstallationID, DeliveryID: o.delivery, Attempt: 1,
		AttemptID: fmt.Sprintf("champion:d%d:a1", o.delivery), Fingerprint: strings.Repeat("ab", 32), ClassName: class, Quantity: 1, PosX: 4621.1, PosY: alt, PosZ: 8397.2,
		DropSourceFile: autoBoot, DropSourceOffset: 853, ArtifactPath: ShopAttemptCustomArtifactPath, FulfilmentMode: ShopFulfilmentBuyer}, "worker:test")
	must(t, err)
	if find(o) != nil {
		t.Fatal("an order with an open attempt qualified")
	}
	_, err = w.attempts.Transition(w.ctx, o.f.OrgID, o.f.InstallationID, a.AttemptID, AttemptPlanCreated, AttemptAbandoned, "worker:test", ShopAttemptEvidence{Note: "never written"})
	must(t, err)
	if find(o) == nil {
		t.Fatal("an order whose only attempt was abandoned did not qualify again")
	}

	// A refunded order never qualifies.
	must(t, w.refund(o))
	if find(o) != nil {
		t.Fatal("a refunded order qualified")
	}

	// An order with two item lines is not automatic.
	two := w.order(w.a)
	w.autoProduct(two, true, &class)
	_, err = w.db.Pool.Exec(w.ctx, `INSERT INTO shop_purchase_items(purchase_id, product_name, unit_price_points, quantity, line_total_points) VALUES($1,'Extra',1,1,1)`, two.purchase)
	must(t, err)
	if find(two) != nil {
		t.Fatal("an order with two item lines qualified")
	}

	// The database refuses an automatic product without a class name, or one that is not a coordinate product.
	for what, sql := range map[string]string{
		"automatic without a class name": `UPDATE shop_products SET class_name=NULL WHERE id=$1`,
		"automatic pickup product":       `UPDATE shop_products SET delivery_policy='MANUAL_PICKUP' WHERE id=$1`,
		"a class name with a path":       `UPDATE shop_products SET class_name='dz/structures/castle.p3d' WHERE id=$1`,
	} {
		if _, err := w.db.Pool.Exec(w.ctx, sql, product); err == nil {
			t.Fatalf("%s was accepted by the database", what)
		}
	}
}

func TestLatestPositionIsFromTheCurrentBootOnly(t *testing.T) {
	w := newAutoWorld(t)
	alt := 319.6
	if _, ok, err := w.auto.LatestPosition(w.ctx, w.a.ServerRowID, w.playerA); err != nil || ok {
		t.Fatalf("a position without any observation: %v %v", ok, err)
	}
	w.logged(w.a, 4621.1, 8397.2, &alt, 853, time.Now().Add(-4*time.Minute))
	if _, ok, _ := w.auto.LatestPosition(w.ctx, w.a.ServerRowID, w.playerA); ok {
		t.Fatal("a position was returned although the server has no current boot session")
	}
	_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file) VALUES($1,$2,$3)`, w.a.ServerRowID, w.a.GuildRowID, autoBoot)
	must(t, err)
	p, ok, err := w.auto.LatestPosition(w.ctx, w.a.ServerRowID, w.playerA)
	must(t, err)
	if !ok || p.X != 4621.1 || p.Z != 8397.2 || p.AltitudeY != alt || p.SourceFile != autoBoot || time.Since(p.ObservedAt) > 10*time.Minute {
		t.Fatalf("position = %+v ok=%v", p, ok)
	}
	// The session moved on to a new boot: the old boot's position is no longer current.
	_, err = w.db.Pool.Exec(w.ctx, `UPDATE server_adm_sessions SET adm_file='DayZServer_PS4_x64_2026-10-01_09-08-00.ADM' WHERE server_id=$1`, w.a.ServerRowID)
	must(t, err)
	if _, ok, _ := w.auto.LatestPosition(w.ctx, w.a.ServerRowID, w.playerA); ok {
		t.Fatal("a previous boot's position was returned as current")
	}
}

func TestBootLogReadsTheServersOwnRecords(t *testing.T) {
	w := newAutoWorld(t)
	server, guild := w.a.ServerRowID, w.a.GuildRowID
	boot := time.Now().Add(-time.Hour)
	record := func(category, payload string, at time.Time) {
		t.Helper()
		_, err := w.db.Pool.Exec(w.ctx, `INSERT INTO live_sync_records(server_id, guild_id, event_id, family, source_file, source_offset, category, status, delivery, source_utc, detected_at, payload, parser)
VALUES($1,$2,$3,'RPT','DayZServer_x64.RPT',$4,$5,'PARSED','LIVE',$6,$6,$7::jsonb,'test')`, server, guild, fmt.Sprintf("e-%d-%d", time.Now().UnixNano(), w.seq.Add(1)), w.seq.Add(1), category, at, payload)
		must(t, err)
	}
	got, err := w.auto.BootLog(w.ctx, server, boot, time.Time{})
	must(t, err)
	if got.CentralEconomySeen || got.SpawnerError {
		t.Fatalf("a boot with no records: %+v", got)
	}
	record("CENTRAL_ECONOMY", `{}`, boot.Add(-10*time.Minute)) // the previous boot
	record("OBJECT_SPAWNER_ERROR", `{"missingFile":"custom/The_Lost_City.json"}`, boot.Add(time.Minute))
	if got, _ := w.auto.BootLog(w.ctx, server, boot, time.Time{}); got.CentralEconomySeen || got.SpawnerError {
		t.Fatalf("an earlier boot's record, or another file's error, counted: %+v", got)
	}
	record("CENTRAL_ECONOMY", `{}`, boot.Add(2*time.Minute))
	if got, _ := w.auto.BootLog(w.ctx, server, boot, time.Time{}); !got.CentralEconomySeen || got.SpawnerError {
		t.Fatalf("after the economy started: %+v", got)
	}
	record("OBJECT_SPAWNER_ERROR", `{"missingFile":"custom/champion_shop_delivery.json"}`, boot.Add(3*time.Minute))
	if got, _ := w.auto.BootLog(w.ctx, server, boot, time.Time{}); !got.SpawnerError {
		t.Fatalf("the Champion file's spawner error was missed: %+v", got)
	}
	if got, _ := w.auto.BootLog(w.ctx, server, boot, boot.Add(150*time.Second)); got.SpawnerError || !got.CentralEconomySeen {
		t.Fatalf("the upper bound was ignored: %+v", got)
	}
	if got, _ := w.auto.BootLog(w.ctx, w.b.ServerRowID, boot, time.Time{}); got.CentralEconomySeen || got.SpawnerError {
		t.Fatalf("another server's records were read: %+v", got)
	}
}

func TestSystemTicketForAnOrderTheWorkerCouldNotConfirm(t *testing.T) {
	w := newAutoWorld(t)

	// No verified Discord account: the ticket carries a placeholder opener, never a guessed id.
	o := w.order(w.a)
	first, err := w.conf.OpenSystemTicket(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, "  two restarts happened  ")
	must(t, err)
	if first.OpenedVia != TicketViaSystem || first.OpenedByDiscordID != "system" || first.Reason != "two restarts happened" || first.Status != TicketOpen || first.PlayerID != w.playerA {
		t.Fatalf("ticket %+v", first)
	}
	again, err := w.conf.OpenSystemTicket(w.ctx, o.f.OrgID, o.f.InstallationID, o.purchase, "something else")
	must(t, err)
	if again.ID != first.ID || again.Reason != "two restarts happened" {
		t.Fatalf("a second ticket was opened for the same order: %+v", again)
	}
	// It needs a channel like any other ticket.
	if job := w.claimTicket(first.ID, time.Now(), first.ID); job == nil {
		t.Fatal("a system ticket was not offered for a Discord channel")
	}

	// With a verified link the buyer is the opener, so they are added to the ticket channel.
	discordID := w.link("VERIFIED")
	linked := w.order(w.a)
	t2, err := w.conf.OpenSystemTicket(w.ctx, linked.f.OrgID, linked.f.InstallationID, linked.purchase, "")
	must(t, err)
	if t2.OpenedByDiscordID != discordID || t2.Reason == "" {
		t.Fatalf("ticket %+v", t2)
	}
	_, err = w.conf.OpenSystemTicket(w.ctx, w.b.OrgID, w.b.InstallationID, o.purchase, "x")
	wantIs(t, "another tenant's order", err, ErrShopTicketNotFound)
}
