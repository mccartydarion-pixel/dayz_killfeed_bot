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
	"github.com/yourname/dayz-killfeed/internal/shop"
	"github.com/yourname/dayz-killfeed/internal/shop/deliveryworker"
	"github.com/yourname/dayz-killfeed/internal/shop/missionwrite"
	nd "github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// One automatic order from purchase to fulfilment, and one that ends in a staff review, with the
// REAL worker, the real ledger, journal, settings, confirmation and ticket tables, and the real API
// routes for everything a person does (buying, confirming, reviewing, refunding). Only the game
// server is simulated: an in-memory Champion file, configuration and boot list. The worker's own
// reads and writes against the real Nitrado client are covered by missionwrite's artifact tests.

type simulatedServer struct {
	content []byte
	config  string
	mission string
	boots   []string
	writes  int
}

func (s *simulatedServer) Inspect(context.Context) (missionwrite.ArtifactState, error) {
	return missionwrite.ArtifactState{MissionPath: s.mission, ConfigSHA256: s.config, Content: append([]byte(nil), s.content...),
		SHA256: missionwrite.SHA256(s.content), Boots: append([]string(nil), s.boots...), GameserverStatus: "started"}, nil
}

func (s *simulatedServer) Write(_ context.Context, st missionwrite.ArtifactState, payload []byte) (missionwrite.ArtifactWrite, error) {
	s.writes++
	s.content = payload
	return missionwrite.ArtifactWrite{Status: missionwrite.StatusWrittenVerified, Before: st.SHA256, After: missionwrite.SHA256(payload), Detail: "read-back matches the payload"}, nil
}

// bootNamed is the ADM file of a boot that started at the given UTC time on a UTC-4 server.
func bootNamed(startUTC time.Time) string {
	return "DayZServer_PS4_x64_" + startUTC.Add(-4*time.Hour).Format("2006-01-02_15-04-05") + ".ADM"
}

func TestShopDeliveryWorkerEndToEnd(t *testing.T) {
	w := newAutoWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.shopConfirmationRepo = repository.NewShopConfirmationRepository(pool)
	w.a.ShopConfirmations = shop.NewConfirmations(w.a.shopConfirmationRepo, w.a.EconomyAccounts)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	f := w.a1
	guild, server := w.gameContext(f)
	player := w.players[0]
	pid := w.linkPlayer(f, player, "Drop Dana")
	w.grant(f, pid, 1000)
	w.expect(w.setMap(f, "chernarusplus"), http.StatusOK, "set map")
	item := w.product(f, "Bandage", 5, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	w.expect(w.setProductAuto(f, item, map[string]any{"autoDelivery": true, "className": "BandageDressing"}), http.StatusOK, "auto on")
	w.openWorkerLock(f, "enabled")
	w.expect(w.do(http.MethodPut, w.shopPath(f, "/admin/auto-delivery"), w.admin, map[string]any{"enabled": true}), http.StatusOK, "enable")
	// What a finished setup looks like: a READY installation bound to a numeric Nitrado service, and
	// a server clock Live Sync has learned (UTC-4).
	exec(`UPDATE installations SET status='READY' WHERE id=$1`, f.InstallationID)
	exec(`UPDATE game_servers SET provider_service_id=$2 WHERE id=$1`, server, fmt.Sprint(900000000+server))
	exec(`INSERT INTO live_sync_server_clock(server_id, guild_id, utc_offset_minutes, learned_from) VALUES($1,$2,-240,'test') ON CONFLICT (server_id) DO NOTHING`, server, guild)

	start := time.Now().UTC().Truncate(time.Second)
	bootA := bootNamed(start.Add(-40 * time.Minute))
	srv := &simulatedServer{content: nd.SpawnerFile{Objects: []nd.SpawnerObject{}}.Render(), config: strings.Repeat("c", 64),
		mission: "dayzps_missions/dayzOffline.chernarusplus", boots: []string{bootA}}
	empty := string(srv.content)
	attempts := repository.NewShopAttemptRepository(pool)
	worker := deliveryworker.New(deliveryworker.Config{Name: "e2e"}, attempts, w.a.ShopAuto, repository.NewShopRepository(pool), w.a.shopConfirmationRepo)
	clock := start
	worker.SetClock(func() time.Time { return clock })
	var tickets []int64
	worker.OnTicket = func(id int64) { tickets = append(tickets, id) }
	pass := func() deliveryworker.Report {
		t.Helper()
		claimed, err := w.a.ShopAuto.ClaimInstallations(ctx, time.Now(), "worker:e2e", 4*time.Minute, []int64{f.InstallationID})
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v %+v", err, claimed)
		}
		rep, err := worker.Pass(ctx, claimed[0], srv)
		if err != nil {
			t.Fatalf("pass: %v", err)
		}
		if err := w.a.ShopAuto.ReleaseLease(ctx, f.InstallationID, "worker:e2e"); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	stateOf := func(attemptID string) string {
		t.Helper()
		a, err := attempts.Get(ctx, f.OrgID, f.InstallationID, attemptID)
		if err != nil {
			t.Fatal(err)
		}
		return a.State
	}
	purchaseStatus := func(id int64) string {
		t.Helper()
		return w.getJSON(w.shopPath(f, fmt.Sprintf("/me/purchases/%d", id)), player)["purchase"].(map[string]any)["status"].(string)
	}
	bootRecord := func(category string, at time.Time) {
		t.Helper()
		exec(`INSERT INTO live_sync_records(server_id, guild_id, event_id, family, source_file, source_offset, category, status, delivery, source_utc, detected_at, payload, parser)
VALUES($1,$2,$3,'RPT','DayZServer_x64.RPT',$4,$5,'PARSED','LIVE',$6,$6,'{}'::jsonb,'test')`, server, guild, idemKeyFor("rec"), buySeq.Add(1), category, at)
	}

	// --- a pass with nothing to deliver touches nothing -------------------------------------------
	if rep := pass(); rep.Inspected || srv.writes != 0 {
		t.Fatalf("idle pass: %+v", rep)
	}

	// --- order 1: delivered and confirmed ----------------------------------------------------------
	w.logPosition(f, pid, 7000.5, 9000.5, 210.4, 853, 2*time.Minute)
	bought := w.expect(w.buyAt(f, player, item, 2, idemKeyFor("e2e"), map[string]any{"x": 7000.5, "z": 9000.5}), http.StatusCreated, "purchase").JSON(t)
	purchase := purchaseID(bought)
	delivery := int64(deliveryOf(t, bought["purchase"].(map[string]any))["id"].(float64))
	attemptID := fmt.Sprintf("champion:d%d:a1", delivery)

	rep := pass()
	if len(rep.Staged) != 1 || rep.Staged[0] != attemptID || srv.writes != 1 {
		t.Fatalf("stage pass: %+v", rep)
	}
	wantFile := string(nd.SpawnerFile{Objects: nd.AttemptEntries(attemptID, "BandageDressing", 2, [3]float64{7000.5, 210.4, 9000.5})}.Render())
	if string(srv.content) != wantFile {
		t.Fatalf("staged file:\n%s\nwant:\n%s", srv.content, wantFile)
	}
	if accepted := w.count(`SELECT COUNT(*) FROM shop_auto_delivery_settings WHERE installation_id=$1 AND config_sha256=$2 AND mission_path=$3`, f.InstallationID, srv.config, srv.mission); accepted != 1 {
		t.Fatal("the first pass did not record the configuration it accepted")
	}
	refund := func(id int64) *apiResult {
		return w.do(http.MethodPost, w.shopPath(f, fmt.Sprintf("/purchases/%d/refund", id)), w.admin, map[string]any{"reason": "test"})
	}
	if r := refund(purchase); r.Status != http.StatusConflict {
		t.Fatalf("refund of a staged order: HTTP %d %s", r.Status, r.Body)
	}
	if rep := pass(); srv.writes != 1 || stateOf(attemptID) != repository.AttemptAwaitingRestart {
		t.Fatalf("waiting pass: %+v state=%s", rep, stateOf(attemptID))
	}

	// The scheduled restart, ten minutes after the staging.
	restart := start.Add(10 * time.Minute)
	srv.boots = append(srv.boots, bootNamed(restart))
	clock = start.Add(19 * time.Minute)
	rep = pass()
	if len(rep.Unstaged) != 1 || string(srv.content) != empty || srv.writes != 2 || stateOf(attemptID) != repository.AttemptVerificationRequired {
		t.Fatalf("unstage pass: %+v state=%s", rep, stateOf(attemptID))
	}
	// The server's log has not shown the boot finishing yet: the buyer is not asked.
	if rep := pass(); len(rep.Delivered) != 0 {
		t.Fatalf("the buyer was asked before the log showed the boot: %+v", rep)
	}
	confirmation := w.shopPath(f, fmt.Sprintf("/me/purchases/%d/confirmation", purchase))
	if r := w.do(http.MethodGet, confirmation, player, nil); r.Status != http.StatusNotFound {
		t.Fatalf("a confirmation before delivery: HTTP %d", r.Status)
	}
	bootRecord("CENTRAL_ECONOMY", restart.Add(time.Minute))
	if rep := pass(); len(rep.Delivered) != 1 {
		t.Fatalf("deliver pass: %+v", rep)
	}
	c := w.getJSON(confirmation, player)["confirmation"].(map[string]any)
	if c["state"] != "AWAITING_BUYER" || c["canConfirmReceived"] != true || purchaseStatus(purchase) != "PENDING_FULFILLMENT" {
		t.Fatalf("after delivery: confirmation=%v purchase=%s", c, purchaseStatus(purchase))
	}
	// The DM notice is queued for the Discord side like any delivered order.
	if n := w.count(`SELECT COUNT(*) FROM shop_order_confirmations WHERE purchase_id=$1 AND notify_state='PENDING'`, purchase); n != 1 {
		t.Fatal("no delivered-order notice is queued")
	}
	if rep := pass(); len(rep.Fulfilled) != 0 || stateOf(attemptID) != repository.AttemptVerificationRequired {
		t.Fatalf("fulfilled before the buyer answered: %+v", rep)
	}

	// The buyer presses "Received order" on the website.
	w.expect(w.do(http.MethodPost, w.shopPath(f, fmt.Sprintf("/me/purchases/%d/confirm-received", purchase)), player, map[string]any{}), http.StatusOK, "confirm received")
	rep = pass()
	if len(rep.Fulfilled) != 1 || stateOf(attemptID) != repository.AttemptFulfilled || purchaseStatus(purchase) != "FULFILLED" {
		t.Fatalf("fulfil pass: %+v state=%s purchase=%s", rep, stateOf(attemptID), purchaseStatus(purchase))
	}
	done, err := attempts.Get(ctx, f.OrgID, f.InstallationID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if done.BuyerAnswer == nil || *done.BuyerAnswer != repository.BuyerAnswerReceived || *done.VerifiedBy != "worker:e2e" || done.ItemObservedBy != nil {
		t.Fatalf("fulfilled attempt: %+v", done)
	}
	history, err := attempts.Events(ctx, f.OrgID, f.InstallationID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	var path []string
	for _, e := range history {
		path = append(path, e.ToState)
		if e.Actor != "worker:e2e" {
			t.Fatalf("history actor %q", e.Actor)
		}
	}
	if got := strings.Join(path, ">"); got != "PLAN_CREATED>FILE_PREPARED>FILE_STAGED>AWAITING_RESTART>RESTART_OBSERVED>UNSTAGE_REQUIRED>VERIFICATION_REQUIRED>FULFILLED" {
		t.Fatalf("attempt history: %s", got)
	}
	if n := w.count(`SELECT COUNT(*) FROM shop_delivery_writes WHERE installation_id=$1 AND outcome='WRITTEN_VERIFIED' AND finished_at IS NOT NULL`, f.InstallationID); n != 2 {
		t.Fatalf("%d verified journal rows, want the stage and the unstage", n)
	}
	if len(tickets) != 0 {
		t.Fatalf("a clean delivery opened tickets: %v", tickets)
	}

	// --- order 2: two restarts before the removal -> review, ticket, staff decision, refund -------
	clock = start.Add(30 * time.Minute)
	w.logPosition(f, pid, 7100.5, 9100.5, 205.0, 1900, time.Minute)
	second := w.expect(w.buyAt(f, player, item, 1, idemKeyFor("e2e"), map[string]any{"x": 7100.5, "z": 9100.5}), http.StatusCreated, "second purchase").JSON(t)
	purchase2 := purchaseID(second)
	attempt2 := fmt.Sprintf("champion:d%d:a1", int64(deliveryOf(t, second["purchase"].(map[string]any))["id"].(float64)))
	if rep := pass(); len(rep.Staged) != 1 || rep.Staged[0] != attempt2 {
		t.Fatalf("second stage pass: %+v", rep)
	}
	srv.boots = append(srv.boots, bootNamed(start.Add(60*time.Minute)), bootNamed(start.Add(128*time.Minute)))
	clock = start.Add(140 * time.Minute)
	rep = pass()
	if len(rep.Failed) != 1 || rep.Paused != "" || string(srv.content) != empty || stateOf(attempt2) != repository.AttemptFailedReview {
		t.Fatalf("double restart pass: %+v state=%s", rep, stateOf(attempt2))
	}
	if len(tickets) != 1 {
		t.Fatalf("tickets = %v, want one system ticket", tickets)
	}
	ticket := w.getJSON(w.shopPath(f, fmt.Sprintf("/admin/tickets/%d", tickets[0])), w.admin)["ticket"].(map[string]any)
	if ticket["openedVia"] != "SYSTEM" || int64(ticket["purchaseId"].(float64)) != purchase2 || !strings.Contains(ticket["reason"].(string), "spawned twice") || ticket["status"] != "OPEN" {
		t.Fatalf("system ticket: %v", ticket)
	}
	if r := w.do(http.MethodGet, w.shopPath(f, fmt.Sprintf("/me/purchases/%d/confirmation", purchase2)), player, nil); r.Status != http.StatusNotFound {
		t.Fatalf("the buyer was asked to confirm an order under review: HTTP %d", r.Status)
	}
	// The worker never tries that order again, and keeps working the installation.
	if rep := pass(); srv.writes != 4 || rep.Paused != "" {
		t.Fatalf("after the review: %+v writes=%d", rep, srv.writes)
	}
	if r := refund(purchase2); r.Status != http.StatusConflict {
		t.Fatalf("refund before the review was resolved: HTTP %d", r.Status)
	}
	w.expect(w.do(http.MethodPost, w.shopPath(f, "/admin/delivery-attempts/"+attempt2+"/review"), w.admin,
		map[string]any{"outcome": "NOT_SPAWNED", "observer": "ceiyxe", "detail": "Nothing at the drop point after either restart."}), http.StatusOK, "review")
	balance := w.balanceOf(f, player)
	w.expect(refund(purchase2), http.StatusOK, "refund after the review")
	if got := w.balanceOf(f, player); got != balance+5 {
		t.Fatalf("balance after the refund = %d, want %d", got, balance+5)
	}
	if s := w.autoState(f); s["paused"] != false || s["delivering"] != true {
		t.Fatalf("installation state at the end: %v", s)
	}
	w.allIntegrity(f)
}
