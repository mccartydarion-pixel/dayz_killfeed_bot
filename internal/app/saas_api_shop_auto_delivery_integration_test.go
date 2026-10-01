//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The routes around the Shop automatic delivery worker, over the real routes and a real PostgreSQL
// (docs/SHOP_DELIVERY_WORKER_DESIGN.md). No worker runs here and nothing contacts a game server.

const autoTestBoot = "DayZServer_PS4_x64_2026-10-01_08-00-00.ADM"

func newAutoWorld(t *testing.T) *factionWorld {
	w := newFactionWorld(t)
	pool := w.a.DB.Pool
	w.a.ShopAuto = repository.NewShopAutoDeliveryRepository(pool)
	w.a.shopAttempts = repository.NewShopAttemptRepository(pool)
	w.a.Config.ShopAutoDelivery = config.ShopAutoDelivery{} // the worker lock starts closed
	return w
}

// openWorkerLock is what CHAMPION_SHOP_AUTO_DELIVERY=enabled plus the installation id does.
func (w *factionWorld) openWorkerLock(f installationFixture, mode string) {
	w.a.Config.ShopAutoDelivery = config.ParseShopAutoDelivery(mode, fmt.Sprint(f.InstallationID), "")
}

func (w *factionWorld) autoState(f installationFixture) map[string]any {
	w.t.Helper()
	return w.getJSON(w.shopPath(f, "/admin/auto-delivery"), w.adminFor(f))["autoDelivery"].(map[string]any)
}

// logPosition records a player-list observation for the player in the installation's current boot.
func (w *factionWorld) logPosition(f installationFixture, player int64, x, z, altitude float64, offset int64, ago time.Duration) {
	w.t.Helper()
	guild, server := w.gameContext(f)
	ctx := context.Background()
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file) VALUES($1,$2,$3) ON CONFLICT (server_id) DO NOTHING`, server, guild, autoTestBoot); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_location_events(guild_id, server_id, player_id, gamertag, x, z, y, event_type, observed_at, source_file, source_offset)
VALUES($1,$2,$3,'P',$4,$5,$6,'PLAYER_LIST',$7,$8,$9)`, guild, server, player, x, z, altitude, time.Now().Add(-ago), autoTestBoot, offset); err != nil {
		w.t.Fatal(err)
	}
}

func (w *factionWorld) setProductAuto(f installationFixture, product int64, body map[string]any) *apiResult {
	w.t.Helper()
	return w.do(http.MethodPut, w.shopPath(f, fmt.Sprintf("/admin/products/%d/auto-delivery", product)), w.adminFor(f), body)
}

func TestShopAutoDeliverySwitchesAreAllOffByDefault(t *testing.T) {
	w := newAutoWorld(t)
	player := w.players[0]

	s := w.autoState(w.a1)
	if s["enabled"] != false || s["paused"] != false || s["worker"] != "off" || s["delivering"] != false {
		t.Fatalf("default state: %v", s)
	}
	// Players and outsiders cannot read or change the switches.
	for _, actor := range []string{player, w.member, w.b1.OwnerDiscordID} {
		if r := w.do(http.MethodGet, w.shopPath(w.a1, "/admin/auto-delivery"), actor, nil); r.Status != http.StatusForbidden && r.Status != http.StatusNotFound {
			t.Fatalf("%s read the switch: HTTP %d", actor, r.Status)
		}
		if r := w.do(http.MethodPut, w.shopPath(w.a1, "/admin/auto-delivery"), actor, map[string]any{"enabled": true}); r.Status != http.StatusForbidden && r.Status != http.StatusNotFound {
			t.Fatalf("%s changed the switch: HTTP %d", actor, r.Status)
		}
	}
	if r := w.do(http.MethodPut, w.shopPath(w.a1, "/admin/auto-delivery"), w.admin, map[string]any{}); r.Status != http.StatusBadRequest {
		t.Fatalf("a switch without a value: HTTP %d", r.Status)
	}

	// The owner's switch alone delivers nothing: the worker lock is set on the server, not here.
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, "/admin/auto-delivery"), w.admin, map[string]any{"enabled": true}), http.StatusOK, "enable")
	if s := w.autoState(w.a1); s["enabled"] != true || s["worker"] != "off" || s["delivering"] != false {
		t.Fatalf("owner switch on, lock closed: %v", s)
	}
	w.openWorkerLock(w.a1, "report")
	if s := w.autoState(w.a1); s["worker"] != "report" || s["delivering"] != false {
		t.Fatalf("report mode must not deliver: %v", s)
	}
	w.openWorkerLock(w.a1, "enabled")
	if s := w.autoState(w.a1); s["worker"] != "enabled" || s["delivering"] != true {
		t.Fatalf("every switch on: %v", s)
	}
	// The lock lists installations: another one stays off.
	if s := w.autoState(w.b1); s["worker"] != "off" || s["delivering"] != false {
		t.Fatalf("an unlisted installation: %v", s)
	}

	// A pause (set by the worker) stops delivery until a person resumes it.
	if err := w.a.ShopAuto.Pause(context.Background(), w.a1.OrgID, w.a1.InstallationID, "cfggameplay.json changed"); err != nil {
		t.Fatal(err)
	}
	if s := w.autoState(w.a1); s["paused"] != true || s["pausedReason"] != "cfggameplay.json changed" || s["delivering"] != false || s["pausedAt"] == nil {
		t.Fatalf("paused: %v", s)
	}
	if r := w.do(http.MethodPost, w.shopPath(w.a1, "/admin/auto-delivery/resume"), player, nil); r.Status != http.StatusForbidden {
		t.Fatalf("a player resumed delivery: HTTP %d", r.Status)
	}
	resumed := w.expect(w.do(http.MethodPost, w.shopPath(w.a1, "/admin/auto-delivery/resume"), w.admin, nil), http.StatusOK, "resume").JSON(t)["autoDelivery"].(map[string]any)
	if resumed["paused"] != false || resumed["delivering"] != true {
		t.Fatalf("resumed: %v", resumed)
	}
}

func TestShopProductAutoDeliverySetting(t *testing.T) {
	w := newAutoWorld(t)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "set map")
	coordinate := w.product(w.a1, "Bandage", 5, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	pickup := w.product(w.a1, "Crate", 5, nil)
	path := w.shopPath(w.a1, fmt.Sprintf("/admin/products/%d/auto-delivery", coordinate))

	got := w.getJSON(path, w.admin)["product"].(map[string]any)
	if got["autoDelivery"] != false || got["className"] != "" || got["deliveryPolicy"] != "MANUAL_COORDINATE" {
		t.Fatalf("default: %v", got)
	}
	for name, body := range map[string]map[string]any{
		"no switch":                {"className": "BandageDressing"},
		"automatic without a name": {"autoDelivery": true},
		"a path as class name":     {"autoDelivery": true, "className": "dz/structures/castle.p3d"},
		"a name with a space":      {"autoDelivery": true, "className": "Bandage Dressing"},
	} {
		if r := w.setProductAuto(w.a1, coordinate, body); r.Status != http.StatusBadRequest {
			t.Fatalf("%s: HTTP %d %s", name, r.Status, r.Body)
		}
	}
	if r := w.setProductAuto(w.a1, pickup, map[string]any{"autoDelivery": true, "className": "BandageDressing"}); r.Status != http.StatusConflict || r.errCode(t) != "AUTO_DELIVERY_NOT_ALLOWED" {
		t.Fatalf("a pickup product: HTTP %d %s", r.Status, r.Body)
	}
	set := w.expect(w.setProductAuto(w.a1, coordinate, map[string]any{"autoDelivery": true, "className": " BandageDressing "}), http.StatusOK, "set").JSON(t)["product"].(map[string]any)
	if set["autoDelivery"] != true || set["className"] != "BandageDressing" {
		t.Fatalf("set: %v", set)
	}
	// Another organization cannot see or change it; a player cannot either.
	if r := w.do(http.MethodPut, w.shopPath(w.b1, fmt.Sprintf("/admin/products/%d/auto-delivery", coordinate)), w.b1.OwnerDiscordID, map[string]any{"autoDelivery": false}); r.Status != http.StatusNotFound {
		t.Fatalf("another tenant changed the product: HTTP %d", r.Status)
	}
	if r := w.do(http.MethodPut, path, w.players[0], map[string]any{"autoDelivery": false}); r.Status != http.StatusForbidden {
		t.Fatalf("a player changed the product: HTTP %d", r.Status)
	}
	// Switching it off keeps the class name unless it is cleared.
	off := w.expect(w.setProductAuto(w.a1, coordinate, map[string]any{"autoDelivery": false, "className": "BandageDressing"}), http.StatusOK, "off").JSON(t)["product"].(map[string]any)
	if off["autoDelivery"] != false || off["className"] != "BandageDressing" {
		t.Fatalf("off: %v", off)
	}
	// The public product never exposes the class name.
	public := w.expect(w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/products/%d", coordinate)), w.players[0], nil), http.StatusOK, "public product")
	if strings.Contains(string(public.Body), "BandageDressing") || strings.Contains(string(public.Body), "className") {
		t.Fatalf("the public product exposes the class name: %s", public.Body)
	}
}

func TestShopPurchaseOfAnAutomaticProductUsesTheBuyersLoggedPosition(t *testing.T) {
	w := newAutoWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Drop Dana")
	w.grant(w.a1, pid, 1000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "set map")
	item := w.product(w.a1, "Bandage", 5, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	w.expect(w.setProductAuto(w.a1, item, map[string]any{"autoDelivery": true, "className": "BandageDressing"}), http.StatusOK, "auto on")
	position := func() map[string]any {
		t.Helper()
		return w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/me/delivery-position?productId=%d", item)), player)
	}
	typed := map[string]any{"x": 4621.1, "z": 8397.2}

	// With any switch off the product behaves as before: the buyer types coordinates, staff deliver.
	if p := position(); p["automatic"] != false || p["position"] != nil {
		t.Fatalf("lock closed: %v", p)
	}
	manual := w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("manual"), typed), http.StatusCreated, "manual purchase").JSON(t)
	if deliveryOf(t, manual["purchase"].(map[string]any))["status"] != "MANUAL_READY" {
		t.Fatalf("manual purchase: %v", manual)
	}

	w.openWorkerLock(w.a1, "enabled")
	w.expect(w.do(http.MethodPut, w.shopPath(w.a1, "/admin/auto-delivery"), w.admin, map[string]any{"enabled": true}), http.StatusOK, "enable")

	// No logged position: the purchase is refused and nothing is written.
	if p := position(); p["automatic"] != true || p["position"] != nil {
		t.Fatalf("no position yet: %v", p)
	}
	before := w.snapshot(w.a1, player)
	refuse := func(what string, delivery any) {
		t.Helper()
		r := w.buyAt(w.a1, player, item, 1, idemKeyFor("auto"), delivery)
		if r.Status != http.StatusConflict || r.errCode(t) != "DELIVERY_POSITION_REQUIRED" {
			t.Fatalf("%s: HTTP %d %s", what, r.Status, r.Body)
		}
		if after := w.snapshot(w.a1, player); after != before {
			t.Fatalf("%s: a refused purchase wrote something: %+v -> %+v", what, before, after)
		}
	}
	refuse("no logged position", typed)

	// A position older than twenty minutes is shown but cannot be bought with.
	w.logPosition(w.a1, pid, 4621.1, 8397.2, 319.6, 100, 31*time.Minute)
	if p := position()["position"].(map[string]any); p["fresh"] != false || p["x"].(float64) != 4621.1 {
		t.Fatalf("stale position: %v", p)
	}
	refuse("a stale position", typed)

	// A fresh position: only exactly that position is accepted.
	w.logPosition(w.a1, pid, 7000.5, 9000.5, 210.4, 853, 2*time.Minute)
	p := position()
	pos := p["position"].(map[string]any)
	if pos["fresh"] != true || pos["x"].(float64) != 7000.5 || pos["z"].(float64) != 9000.5 || p["maxAgeSeconds"].(float64) != 1200 {
		t.Fatalf("fresh position: %v", p)
	}
	if _, leaked := pos["y"]; leaked || strings.Contains(fmt.Sprint(p), "210.4") {
		t.Fatalf("the altitude is exposed to the buyer: %v", p)
	}
	refuse("typed coordinates elsewhere", typed)
	refuse("no coordinates", nil)
	bought := w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("auto"), map[string]any{"x": 7000.5, "z": 9000.5}), http.StatusCreated, "automatic purchase").JSON(t)
	d := deliveryOf(t, bought["purchase"].(map[string]any))
	coords := d["coordinates"].(map[string]any)
	if d["status"] != "MANUAL_READY" || coords["x"].(float64) != 7000.5 || coords["z"].(float64) != 9000.5 {
		t.Fatalf("automatic purchase delivery: %v", d)
	}

	// That order is exactly what the worker would pick up, with the altitude from the same log line.
	_, server := w.gameContext(w.a1)
	candidates, err := w.a.ShopAuto.Candidates(context.Background(), w.a1.OrgID, w.a1.InstallationID, server, 50)
	if err != nil {
		t.Fatal(err)
	}
	var mine []repository.ShopAutoCandidate
	for _, c := range candidates {
		if c.PlayerID == pid {
			mine = append(mine, c)
		}
	}
	// The earlier manual order at 4621.1/8397.2 also matches a logged position, so both qualify.
	if len(mine) != 2 {
		t.Fatalf("candidates = %+v, want the manual-at-a-logged-position order and the automatic one", mine)
	}
	last := mine[len(mine)-1]
	if last.PurchaseID != purchaseID(bought) || last.AltitudeY != 210.4 || last.ClassName != "BandageDressing" || last.DropSourceFile != autoTestBoot || last.DropSourceOffset != 853 {
		t.Fatalf("worker candidate = %+v", last)
	}

	// A product that is not automatic is untouched by all of this.
	plain := w.product(w.a1, "Canteen", 5, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	w.expect(w.buyAt(w.a1, player, plain, 1, idemKeyFor("plain"), typed), http.StatusCreated, "non-automatic purchase")

	// A paused installation falls back to the manual flow.
	if err := w.a.ShopAuto.Pause(context.Background(), w.a1.OrgID, w.a1.InstallationID, "test"); err != nil {
		t.Fatal(err)
	}
	if p := position(); p["automatic"] != false {
		t.Fatalf("paused: %v", p)
	}
	w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("paused"), typed), http.StatusCreated, "purchase while paused")
	w.allIntegrity(w.a1)
}

func TestShopStaffReviewOfAnOrderTheWorkerStoppedOn(t *testing.T) {
	w := newAutoWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Review Rae")
	w.grant(w.a1, pid, 1000)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "set map")
	item := w.product(w.a1, "Bandage", 5, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE"})
	bought := w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("rev"), map[string]any{"x": 4621.1, "z": 8397.2}), http.StatusCreated, "purchase").JSON(t)
	purchase := purchaseID(bought)
	delivery := int64(deliveryOf(t, bought["purchase"].(map[string]any))["id"].(float64))
	ctx := context.Background()
	attemptID := fmt.Sprintf("champion:d%d:a1", delivery)
	ledger := w.a.shopAttempts
	if _, err := ledger.Create(ctx, repository.ShopAttemptCreate{OrganizationID: w.a1.OrgID, InstallationID: w.a1.InstallationID, DeliveryID: delivery, Attempt: 1,
		AttemptID: attemptID, Fingerprint: strings.Repeat("ab", 32), ClassName: "BandageDressing", Quantity: 1, PosX: 4621.1, PosY: 319.6, PosZ: 8397.2,
		DropSourceFile: autoTestBoot, DropSourceOffset: 853, ArtifactPath: repository.ShopAttemptCustomArtifactPath, FulfilmentMode: repository.ShopFulfilmentBuyer}, "worker:test"); err != nil {
		t.Fatal(err)
	}
	staged, at, boot, reason := strings.Repeat("1", 64), time.Now().Add(-time.Hour), autoTestBoot, "two restarts happened before the item could be removed"
	for _, step := range []struct {
		from, to string
		ev       repository.ShopAttemptEvidence
	}{
		{repository.AttemptPlanCreated, repository.AttemptFilePrepared, repository.ShopAttemptEvidence{}},
		{repository.AttemptFilePrepared, repository.AttemptFileStaged, repository.ShopAttemptEvidence{StagedSHA256: &staged, StagedAt: &at, StagedBootFile: &boot}},
		{repository.AttemptFileStaged, repository.AttemptFailedReview, repository.ShopAttemptEvidence{FailureReason: &reason}},
	} {
		if _, err := ledger.Transition(ctx, w.a1.OrgID, w.a1.InstallationID, attemptID, step.from, step.to, "worker:test", step.ev); err != nil {
			t.Fatal(err)
		}
	}

	attempts := w.getJSON(w.shopPath(w.a1, fmt.Sprintf("/admin/purchases/%d/delivery-attempts", purchase)), w.admin)["items"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("attempts: %v", attempts)
	}
	first := attempts[0].(map[string]any)
	if first["attemptId"] != attemptID || first["state"] != "FAILED_REVIEW" || first["canReview"] != true || first["automatic"] != true || first["failureReason"] != reason {
		t.Fatalf("attempt: %v", first)
	}
	if r := w.do(http.MethodGet, w.shopPath(w.a1, fmt.Sprintf("/admin/purchases/%d/delivery-attempts", purchase)), player, nil); r.Status != http.StatusForbidden {
		t.Fatalf("a player listed delivery attempts: HTTP %d", r.Status)
	}

	// While the review is open the order can be neither refunded nor fulfilled by hand.
	refund := func() *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", purchase)), w.admin, map[string]any{"reason": "not delivered"})
	}
	if r := refund(); r.Status != http.StatusConflict {
		t.Fatalf("refund during an open review: HTTP %d %s", r.Status, r.Body)
	}
	review := func(actor string, body map[string]any) *apiResult {
		return w.do(http.MethodPost, w.shopPath(w.a1, "/admin/delivery-attempts/"+attemptID+"/review"), actor, body)
	}
	good := map[string]any{"outcome": "NOT_SPAWNED", "observer": "ceiyxe", "detail": "Checked the drop point after both restarts: nothing there."}
	for name, body := range map[string]map[string]any{
		"no observer":        {"outcome": "NOT_SPAWNED", "detail": "x"},
		"no detail":          {"outcome": "NOT_SPAWNED", "observer": "ceiyxe"},
		"an unknown outcome": {"outcome": "MAYBE", "observer": "ceiyxe", "detail": "x"},
	} {
		if r := review(w.admin, body); r.Status != http.StatusBadRequest {
			t.Fatalf("%s: HTTP %d %s", name, r.Status, r.Body)
		}
	}
	if r := review(player, good); r.Status != http.StatusForbidden {
		t.Fatalf("a player reviewed an attempt: HTTP %d", r.Status)
	}
	if r := w.do(http.MethodPost, w.shopPath(w.b1, "/admin/delivery-attempts/"+attemptID+"/review"), w.b1.OwnerDiscordID, good); r.Status != http.StatusNotFound {
		t.Fatalf("another tenant reviewed the attempt: HTTP %d", r.Status)
	}
	done := w.expect(review(w.admin, good), http.StatusOK, "review").JSON(t)["attempt"].(map[string]any)
	if done["reviewResolution"] != "NOT_SPAWNED" || done["canReview"] != false {
		t.Fatalf("reviewed: %v", done)
	}
	if r := review(w.admin, good); r.Status != http.StatusConflict || r.errCode(t) != "ATTEMPT_NOT_REVIEWABLE" {
		t.Fatalf("a second review: HTTP %d %s", r.Status, r.Body)
	}
	// The item was verified not spawned: the refund is now possible, and the points come back.
	balance := w.balanceOf(w.a1, player)
	w.expect(refund(), http.StatusOK, "refund after NOT_SPAWNED")
	if got := w.balanceOf(w.a1, player); got != balance+5 {
		t.Fatalf("balance after the refund = %d, want %d", got, balance+5)
	}
	w.allIntegrity(w.a1)
}
