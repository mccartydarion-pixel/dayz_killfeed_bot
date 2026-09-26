//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Migration 0054 over the real Shop routes: a purchase whose automatic delivery attempt may have put
// the item on the server is neither refunded nor fulfilled by hand - the API answers 409
// DELIVERY_ATTEMPT_ACTIVE, never a 500 - until the attempt is resolved. Nothing contacts a game server.
func TestShopRefundAndFulfilRespectDeliveryAttempts(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Canary Cara")
	w.grant(w.a1, pid, 1_000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	item := w.product(w.a1, "Canary BandageDressing", 1, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 50})
	attempts := repository.NewShopAttemptRepository(w.a.DB.Pool)

	type bought struct{ purchase, delivery int64 }
	buy := func() bought {
		r := w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("canary"), map[string]any{"x": 4621.1, "z": 8397.2}), http.StatusCreated, "buy").JSON(t)
		d := deliveryOf(t, r["purchase"].(map[string]any))
		return bought{purchase: purchaseID(r), delivery: int64(d["id"].(float64))}
	}
	refund := func(id int64) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", id)), w.admin, map[string]any{"reason": "canary"})
	}
	fulfill := func(id int64) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", id)), w.admin, nil)
	}
	blocked := func(r *apiResult, what string) {
		t.Helper()
		if r.Status != http.StatusConflict || r.errCode(t) != "DELIVERY_ATTEMPT_ACTIVE" {
			t.Fatalf("%s: want 409 DELIVERY_ATTEMPT_ACTIVE, got %d %s", what, r.Status, r.Body)
		}
	}
	start := func(b bought) string {
		a, err := attempts.Create(ctx, repository.ShopAttemptCreate{OrganizationID: w.a1.OrgID, InstallationID: w.a1.InstallationID, DeliveryID: b.delivery, Attempt: 1,
			AttemptID: fmt.Sprintf("champion:d%d:a1", b.delivery), Fingerprint: strings.Repeat("cd", 32), ClassName: "BandageDressing", Quantity: 1,
			PosX: 4621.1, PosY: 319.6, PosZ: 8397.2, DropSourceFile: "dayzps/config/x.ADM", DropSourceOffset: 853}, "worker-test")
		if err != nil {
			t.Fatal(err)
		}
		return a.AttemptID
	}
	move := func(id, from, to string, ev repository.ShopAttemptEvidence) {
		t.Helper()
		if _, err := attempts.Transition(ctx, w.a1.OrgID, w.a1.InstallationID, id, from, to, "worker-test", ev); err != nil {
			t.Fatalf("%s -> %s: %v", from, to, err)
		}
	}
	s := func(v string) *string { return &v }
	observe := func(id string) {
		t.Helper()
		if _, err := attempts.RecordEvidence(ctx, w.a1.OrgID, w.a1.InstallationID, id, w.admin, repository.ShopAttemptEvidenceInput{Kind: repository.EvidenceReviewObservation,
			Source: repository.SourceInGameObservation, ObservedBy: "admin-in-game", ObservedAt: time.Now(), Detail: "checked in game"}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	staged := repository.ShopAttemptEvidence{BeforeSHA256: s(strings.Repeat("e", 64)), StagedSHA256: s(strings.Repeat("5", 64)), StagedAt: &now, StagedBootFile: s("boot-1.ADM")}

	// An upload in flight (FILE_PREPARED): refund and manual fulfil are 409, and nothing changed.
	b1 := buy()
	a1 := start(b1)
	move(a1, repository.AttemptPlanCreated, repository.AttemptFilePrepared, repository.ShopAttemptEvidence{})
	balance := w.balanceOf(w.a1, player)
	blocked(refund(b1.purchase), "refund with an upload in flight")
	blocked(fulfill(b1.purchase), "manual fulfil with an upload in flight")
	if w.balanceOf(w.a1, player) != balance {
		t.Fatal("a refused refund must not credit the player")
	}
	// Proven unwritten -> ABANDONED: the ordinary refund works again.
	move(a1, repository.AttemptFilePrepared, repository.AttemptAbandoned, repository.ShopAttemptEvidence{})
	w.expect(refund(b1.purchase), http.StatusOK, "refund after the attempt was abandoned")

	// Staged, then a second boot before the unstage was verified: FAILED_REVIEW. Uncertain -> 409
	// until a human resolves it; NOT_SPAWNED makes the refund possible.
	b2 := buy()
	a2 := start(b2)
	move(a2, repository.AttemptPlanCreated, repository.AttemptFilePrepared, repository.ShopAttemptEvidence{})
	move(a2, repository.AttemptFilePrepared, repository.AttemptFileStaged, staged)
	move(a2, repository.AttemptFileStaged, repository.AttemptFailedReview, repository.ShopAttemptEvidence{FailureReason: s("second boot before a verified unstage")})
	blocked(refund(b2.purchase), "refund of an unresolved review")
	blocked(fulfill(b2.purchase), "manual fulfil of an unresolved review")
	observe(a2)
	if _, err := attempts.ResolveReview(ctx, w.a1.OrgID, w.a1.InstallationID, a2, repository.ReviewNotSpawned, w.admin, "verified in game"); err != nil {
		t.Fatal(err)
	}
	w.expect(refund(b2.purchase), http.StatusOK, "refund after NOT_SPAWNED")

	// Resolved SPAWNED: the refund stays refused; the manual fulfilment records the delivery.
	b3 := buy()
	a3 := start(b3)
	move(a3, repository.AttemptPlanCreated, repository.AttemptFilePrepared, repository.ShopAttemptEvidence{})
	move(a3, repository.AttemptFilePrepared, repository.AttemptFileStaged, staged)
	move(a3, repository.AttemptFileStaged, repository.AttemptFailedReview, repository.ShopAttemptEvidence{FailureReason: s("unknown")})
	observe(a3)
	if _, err := attempts.ResolveReview(ctx, w.a1.OrgID, w.a1.InstallationID, a3, repository.ReviewSpawned, w.admin, "player has it"); err != nil {
		t.Fatal(err)
	}
	blocked(refund(b3.purchase), "refund after SPAWNED")
	w.expect(fulfill(b3.purchase), http.StatusOK, "manual fulfil after SPAWNED")

	// A plan that never touched the server does not block anything: the refund abandons it.
	b4 := buy()
	a4 := start(b4)
	w.expect(refund(b4.purchase), http.StatusOK, "refund at PLAN_CREATED")
	if got, err := attempts.Get(ctx, w.a1.OrgID, w.a1.InstallationID, a4); err != nil || got.State != repository.AttemptAbandoned {
		t.Fatalf("the plan is abandoned with the refund: %+v %v", got, err)
	}
	// Purchases without attempts behave exactly as before.
	b5 := buy()
	w.expect(fulfill(b5.purchase), http.StatusOK, "plain manual fulfil")
	w.expect(refund(buy().purchase), http.StatusOK, "plain refund")
	w.ledgerIntegrity(w.a1)
}
