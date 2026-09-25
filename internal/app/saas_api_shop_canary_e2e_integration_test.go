//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/canaryops"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// fakeNitrado is a local Nitrado file-server fixture: the download-token endpoint and the signed
// download, serving an in-memory file tree. It records every non-GET request: Champion must never
// send one (the owner's uploads are simulated by changing the fixture directly).
type fakeNitrado struct {
	mu     sync.Mutex
	files  map[string][]byte
	writes []string
	srv    *httptest.Server
}

func newFakeNitrado(t *testing.T) *fakeNitrado {
	f := &fakeNitrado{files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet {
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/gameservers/file_server/download"):
			file := r.URL.Query().Get("file")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"success","data":{"token":{"url":%q}}}`, f.srv.URL+"/signed?f="+url.QueryEscape(file))
		case r.URL.Path == "/signed":
			b, ok := f.files[r.URL.Query().Get("f")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNitrado) put(path string, b []byte) {
	f.mu.Lock()
	f.files[path] = append([]byte(nil), b...)
	f.mu.Unlock()
}

// End-to-end canary simulation (Phase 2C.5): a legitimate 1-point purchase through the real Shop,
// the drop point read from the current boot's ADM through the real Nitrado client against the
// fixture, every gate's evidence from verified read-backs, and the atomic fulfilment. Afterwards the
// Shop ledger, purchase, delivery and attempt must agree, and Champion must have sent no write.
func TestShopCanaryEndToEndSimulation(t *testing.T) {
	w := newFactionWorld(t)
	w.a.saasShopAdminLimiter = newSaaSRateLimiter(time.Minute, 1000)
	ctx := context.Background()
	pool := w.a.DB.Pool
	nit := newFakeNitrado(t)
	client := nitrado.NewClient(nit.srv.URL, "fixture-token", nit.srv.Client())
	const service = "19806451"
	const root = "/games/ni0000000_1"
	mission := root + "/ftproot/dayzps_missions/dayzOffline.chernarusplus"
	artifact := mission + "/champion/champion_shop_delivery.json"

	buyer := w.players[0]
	pid := w.linkPlayer(w.a1, buyer, "Canary Cleo")
	w.grant(w.a1, pid, 10)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	guild, server := w.gameContext(w.a1)
	if _, err := pool.Exec(ctx, `UPDATE game_servers SET provider_service_id=$1 WHERE id=$2`, fmt.Sprint(time.Now().UnixNano()%1e12), server); err != nil {
		t.Fatal(err)
	}
	balance0 := w.balanceOf(w.a1, buyer)

	// 1-2. A legitimate one-point purchase creates the purchase, its ledger debit and the delivery.
	item := w.product(w.a1, "Canary BandageDressing", 1, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 1, "purchaseLimit": 1})
	r := w.expect(w.buyAt(w.a1, buyer, item, 1, idemKeyFor("e2e"), map[string]any{"x": 4621.1, "z": 8397.2}), http.StatusCreated, "purchase").JSON(t)
	purchase := purchaseID(r)
	d := deliveryOf(t, r["purchase"].(map[string]any))
	delivery := int64(d["id"].(float64))
	if d["status"] != "MANUAL_READY" || w.balanceOf(w.a1, buyer) != balance0-1 {
		t.Fatalf("purchase: delivery %v, balance %d -> %d", d["status"], balance0, w.balanceOf(w.a1, buyer))
	}

	// 3. The current boot: its ADM (fixture) carries the buyer's position with altitude; boot
	// authority (server_adm_sessions) names that file.
	boot1 := "dayzps/config/DayZServer_PS4_x64_2026-09-25_05-00-00.ADM"
	adm := "AdminLog started on 2026-09-25 at 05:00:00\n05:05:00 | ##### PlayerList log: 1 players\n" +
		"05:05:00 | Player \"Canary Cleo\" (id=Zm9v= pos=<4621.1, 8397.2, 319.6>)\n05:05:00 | #####\n"
	nit.put(root+"/noftp/"+boot1, []byte(adm))
	if _, err := pool.Exec(ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file, session_local_start) VALUES($1,$2,$3,NOW()::timestamp)
		ON CONFLICT (server_id) DO UPDATE SET adm_file=EXCLUDED.adm_file, ended_at=NULL`, server, guild, boot1); err != nil {
		t.Fatal(err)
	}
	raw, err := client.ReadLog(ctx, service, root+"/noftp/"+boot1)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`Player "Canary Cleo".*pos=<([-0-9.]+), ([-0-9.]+), ([-0-9.]+)>\)\n`).FindSubmatchIndex(raw)
	if m == nil {
		t.Fatal("no position in the ADM fixture")
	}
	x, _ := strconv.ParseFloat(string(raw[m[2]:m[3]]), 64)
	z, _ := strconv.ParseFloat(string(raw[m[4]:m[5]]), 64)
	alt, _ := strconv.ParseFloat(string(raw[m[6]:m[7]]), 64)
	offset := int64(m[1]) // end of the ADM line
	if x != d["coordinates"].(map[string]any)["x"].(float64) || z != d["coordinates"].(map[string]any)["z"].(float64) {
		t.Fatalf("the drop point (%v,%v) is not the purchased delivery position %v", x, z, d["coordinates"])
	}

	// 4. Durable attempt (lock opened for this installation only).
	gate := canaryops.NewGate(true, []int64{w.a1.InstallationID})
	w.a.ShopCanaryGate = gate
	w.a.ShopCanary = canaryops.New(repository.NewShopAttemptRepository(pool), repository.NewShopRepository(pool), w.a.SaaSOrganizations,
		economy.NewAccounts(nil, repository.NewEconomyRepository(pool)), gate)
	base := func(s string) string { return w.shopPath(w.a1, "/canary/attempts"+s) }
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	created := w.expect(w.do(http.MethodPost, base(""), w.admin, map[string]any{"deliveryId": delivery, "altitudeY": alt, "dropSourceFile": boot1,
		"dropSourceOffset": offset, "dropObservedAt": at(-2 * time.Minute)}), http.StatusCreated, "create attempt").JSON(t)
	att := created["attempt"].(map[string]any)
	id := att["attemptId"].(string)
	adv := func(from, to string) {
		t.Helper()
		w.expect(w.do(http.MethodPost, base("/"+id+"/advance"), w.admin, map[string]any{"from": from, "to": to}), http.StatusOK, from+" -> "+to)
	}
	ev := func(body map[string]any) {
		t.Helper()
		w.expect(w.do(http.MethodPost, base("/"+id+"/evidence"), w.admin, body), http.StatusCreated, fmt.Sprint(body["kind"]))
	}
	readback := func() string {
		t.Helper()
		b, err := client.ReadLog(ctx, service, artifact)
		if err != nil {
			t.Fatal(err)
		}
		return nitradodelivery.SHA256(b)
	}
	stagedFile, emptyFile := nitradodelivery.SingleAttemptFiles(id, "BandageDressing", 1, [3]float64{x, alt, z})

	// 5. Gate A's empty file is in place; the owner stages the artifact (fixture write); the operator
	// records the verified read-backs and the staging boot.
	nit.put(artifact, emptyFile)
	before := readback()
	adv("PLAN_CREATED", "FILE_PREPARED")
	nit.put(artifact, stagedFile)
	ev(map[string]any{"kind": "STAGED_FILE_HASH", "source": "NITRADO_READBACK", "sha256": readback(), "previousSha256": before, "observedAt": at(-60 * time.Minute)})
	ev(map[string]any{"kind": "STAGING_BOOT", "source": "BOOT_AUTHORITY", "bootFile": boot1, "observedAt": at(-60 * time.Minute)})
	adv("FILE_PREPARED", "FILE_STAGED")
	adv("FILE_STAGED", "AWAITING_RESTART")

	// 6. First restart: a new boot's ADM appears and boot authority accepts it.
	boot2 := "dayzps/config/DayZServer_PS4_x64_2026-09-25_06-08-00.ADM"
	nit.put(root+"/noftp/"+boot2, []byte("AdminLog started on 2026-09-25 at 06:08:00\n"))
	if _, err := pool.Exec(ctx, `UPDATE server_adm_sessions SET adm_file=$2, ended_at=NULL WHERE server_id=$1`, server, boot2); err != nil {
		t.Fatal(err)
	}
	ev(map[string]any{"kind": "SPAWNER_LOG", "source": "RPT_LOG", "observedAt": at(-50 * time.Minute), "detail": "CE init reached; no [::SpawnObjects] error for the Champion file"})
	ev(map[string]any{"kind": "NEW_BOOT", "source": "BOOT_AUTHORITY", "bootFile": boot2, "bootStartedAt": at(-52 * time.Minute), "observedAt": at(-44 * time.Minute)})
	adv("AWAITING_RESTART", "RESTART_OBSERVED")
	adv("RESTART_OBSERVED", "UNSTAGE_REQUIRED")

	// 7. In-game observation and pickup by named observers.
	ev(map[string]any{"kind": "ITEM_OBSERVED", "source": "IN_GAME_OBSERVATION", "observedBy": "owner-in-game", "observedAt": at(-48 * time.Minute)})
	ev(map[string]any{"kind": "PICKUP_CONFIRMED", "source": "IN_GAME_OBSERVATION", "observedBy": "Canary Cleo", "observedAt": at(-47 * time.Minute)})

	// 8. Verified unstaging: the owner restores the empty file; the read-back must be exactly it.
	nit.put(artifact, emptyFile)
	ev(map[string]any{"kind": "UNSTAGED_FILE_HASH", "source": "NITRADO_READBACK", "sha256": readback(), "observedAt": at(-40 * time.Minute)})
	adv("UNSTAGE_REQUIRED", "VERIFICATION_REQUIRED")

	// 9. Second restart and the in-game no-respawn check.
	boot3 := "dayzps/config/DayZServer_PS4_x64_2026-09-25_07-16-00.ADM"
	ev(map[string]any{"kind": "SECOND_BOOT", "source": "BOOT_AUTHORITY", "bootFile": boot3, "bootStartedAt": at(-20 * time.Minute), "observedAt": at(-12 * time.Minute)})
	ev(map[string]any{"kind": "NO_ADDITIONAL_SPAWN", "source": "IN_GAME_OBSERVATION", "observedBy": "owner-in-game", "observedAt": at(-5 * time.Minute)})

	// 10. Atomic final fulfilment.
	done := w.expect(w.do(http.MethodPost, base("/"+id+"/fulfill"), w.admin, map[string]any{"note": "canary complete"}), http.StatusOK, "fulfil").JSON(t)
	fa := done["attempt"].(map[string]any)
	if fa["state"] != "FULFILLED" || fa["stagedSha256"] != nitradodelivery.SHA256(stagedFile) || fa["unstagedSha256"] != nitradodelivery.SHA256(emptyFile) {
		t.Fatalf("attempt: %v", fa)
	}

	// Consistency: purchase, delivery and attempt fulfilled; exactly one 1-point debit; no refund;
	// the Shop reconciliation finds nothing; Champion sent no write to Nitrado.
	var ps, ds, as string
	if err := pool.QueryRow(ctx, `SELECT sp.status, sd.status, a.state FROM shop_purchases sp JOIN shop_deliveries sd ON sd.purchase_id=sp.id
		JOIN shop_delivery_attempts a ON a.delivery_id=sd.id WHERE sp.id=$1`, purchase).Scan(&ps, &ds, &as); err != nil {
		t.Fatal(err)
	}
	if ps != "FULFILLED" || ds != "FULFILLED" || as != "FULFILLED" {
		t.Fatalf("purchase %s delivery %s attempt %s", ps, ds, as)
	}
	var debits, refunds, sum int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE reason_type='SHOP_PURCHASE'), COUNT(*) FILTER (WHERE reason_type='SHOP_REFUND'), COALESCE(SUM(amount),0)
		FROM point_transactions WHERE source_key=$1`, repository.ShopPurchaseRef(purchase)).Scan(&debits, &refunds, &sum); err != nil {
		t.Fatal(err)
	}
	if debits != 1 || refunds != 0 || sum != -1 || w.balanceOf(w.a1, buyer) != balance0-1 {
		t.Fatalf("ledger: debits %d refunds %d sum %d balance %d (start %d)", debits, refunds, sum, w.balanceOf(w.a1, buyer), balance0)
	}
	mism, err := repository.NewShopRepository(pool).ReconcileShop(ctx, w.a1.OrgID, w.a1.InstallationID, guild, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, mm := range mism {
		if mm.PurchaseID == purchase {
			t.Fatalf("reconciliation mismatch: %+v", mm)
		}
	}
	w.ledgerIntegrity(w.a1)
	if len(done["evidence"].([]any)) != 9 || len(done["history"].([]any)) != 8 {
		t.Fatalf("evidence %d, history %d", len(done["evidence"].([]any)), len(done["history"].([]any)))
	}
	nit.mu.Lock()
	writes := append([]string(nil), nit.writes...)
	nit.mu.Unlock()
	if len(writes) != 0 {
		t.Fatalf("Champion sent writes to Nitrado: %v", writes)
	}
	// The delivered order can no longer be retried: a new attempt is refused.
	w.expect(w.do(http.MethodPost, base(""), w.admin, map[string]any{"deliveryId": delivery, "altitudeY": alt, "dropSourceFile": boot2,
		"dropSourceOffset": offset, "dropObservedAt": at(-time.Minute)}), http.StatusBadRequest, "no retry after fulfilment (the plan validator refuses a closed delivery)")
}

// Security audit over every canary route: service auth, acting user, OWNER/ADMIN, tenant ownership.
func TestShopCanaryRoutesRequireAuthorization(t *testing.T) {
	w := newFactionWorld(t)
	w.a.saasShopAdminLimiter = newSaaSRateLimiter(time.Minute, 1000)
	pool := w.a.DB.Pool
	gate := canaryops.NewGate(true, []int64{w.a1.InstallationID, w.b1.InstallationID})
	w.a.ShopCanaryGate = gate
	w.a.ShopCanary = canaryops.New(repository.NewShopAttemptRepository(pool), repository.NewShopRepository(pool), w.a.SaaSOrganizations,
		economy.NewAccounts(nil, repository.NewEconomyRepository(pool)), gate)
	routes := []struct{ method, suffix string }{
		{http.MethodGet, ""}, {http.MethodPost, ""}, {http.MethodGet, "/champion:d1:a1"}, {http.MethodPost, "/champion:d1:a1/advance"},
		{http.MethodPost, "/champion:d1:a1/evidence"}, {http.MethodPost, "/champion:d1:a1/review"}, {http.MethodPost, "/champion:d1:a1/fulfill"},
	}
	for _, rt := range routes {
		p := w.shopPath(w.a1, "/canary/attempts"+rt.suffix)
		name := rt.method + " " + rt.suffix
		// No service secret.
		req, _ := http.NewRequest(rt.method, w.base+p, strings.NewReader("{}"))
		req.Header.Set(actingUserHeader, w.admin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without service auth: %d", name, resp.StatusCode)
		}
		for who, actor := range map[string]string{"no acting user": "", "member": w.member, "player": w.players[0], "other org owner": w.b1.OwnerDiscordID} {
			res := w.do(rt.method, p, actor, map[string]any{})
			if res.Status != http.StatusUnauthorized && res.Status != http.StatusForbidden {
				t.Errorf("%s as %s: %d %s", name, who, res.Status, res.Body)
			}
		}
		// Another organization's installation id under this organization's path is not reachable.
		cross := w.do(rt.method, fmt.Sprintf("/api/saas/organizations/%d/installations/%d/shop/canary/attempts%s", w.a1.OrgID, w.b1.InstallationID, rt.suffix), w.admin, map[string]any{})
		if cross.Status != http.StatusNotFound && cross.Status != http.StatusForbidden {
			t.Errorf("%s cross-installation: %d %s", name, cross.Status, cross.Body)
		}
	}
}
