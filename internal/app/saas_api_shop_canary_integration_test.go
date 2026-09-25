//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/canaryops"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
)

// The canary operator API over the real routes and PostgreSQL: OWNER/ADMIN only, installation-scoped,
// locked by default (423), and only a recorder - nothing here contacts a game server.
func TestShopCanaryOperatorAPI(t *testing.T) {
	w := newFactionWorld(t)
	w.a.saasShopAdminLimiter = newSaaSRateLimiter(time.Minute, 1000) // this test makes many admin calls
	ctx := context.Background()
	pool := w.a.DB.Pool
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Canary Cleo")
	w.grant(w.a1, pid, 100)
	w.expect(w.setMap(w.a1, "chernarusplus"), http.StatusOK, "map")
	guild, server := w.gameContext(w.a1)
	// The plan validator needs a numeric Nitrado service id and a current boot session.
	if _, err := pool.Exec(ctx, `UPDATE game_servers SET provider_service_id=$1 WHERE id=$2`, fmt.Sprint(time.Now().UnixNano()%1e12), server); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("dayzps/config/DayZServer_PS4_x64_%d.ADM", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO server_adm_sessions(server_id, guild_id, adm_file, session_local_start) VALUES($1,$2,$3,NOW()::timestamp)
		ON CONFLICT (server_id) DO UPDATE SET adm_file=EXCLUDED.adm_file, ended_at=NULL`, server, guild, session); err != nil {
		t.Fatal(err)
	}
	item := w.product(w.a1, "Canary BandageDressing", 1, map[string]any{"deliveryPolicy": "MANUAL_COORDINATE", "stockMode": "FINITE", "stockQuantity": 1, "purchaseLimit": 1})
	r := w.expect(w.buyAt(w.a1, player, item, 1, idemKeyFor("canary"), map[string]any{"x": 4621.1, "z": 8397.2}), http.StatusCreated, "canary purchase").JSON(t)
	purchase := purchaseID(r)
	delivery := int64(deliveryOf(t, r["purchase"].(map[string]any))["id"].(float64))

	build := func(gate canaryops.Gate) {
		w.a.ShopCanaryGate = gate
		w.a.ShopCanary = canaryops.New(repository.NewShopAttemptRepository(pool), repository.NewShopRepository(pool), w.a.SaaSOrganizations,
			economy.NewAccounts(nil, repository.NewEconomyRepository(pool)), gate)
	}
	path := func(f installationFixture, suffix string) string { return w.shopPath(f, "/canary/attempts"+suffix) }
	create := map[string]any{"deliveryId": delivery, "altitudeY": 319.6, "dropSourceFile": session, "dropSourceOffset": 853,
		"dropObservedAt": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)}
	code := func(res *apiResult, status int, want, what string) {
		t.Helper()
		if res.Status != status || (want != "" && res.errCode(t) != want) {
			t.Fatalf("%s: want %d %s, got %d %s", what, status, want, res.Status, res.Body)
		}
	}

	// Locked by default: reads work for OWNER/ADMIN, every mutation is 423 and writes nothing.
	build(canaryops.Gate{})
	code(w.do(http.MethodGet, path(w.a1, ""), w.admin, nil), http.StatusOK, "", "list while locked")
	code(w.do(http.MethodPost, path(w.a1, ""), w.admin, create), http.StatusLocked, "CANARY_EXECUTION_LOCKED", "create while locked")
	// Opening the lock for another installation does not open this one.
	build(canaryops.NewGate(true, []int64{w.b1.InstallationID}))
	code(w.do(http.MethodPost, path(w.a1, ""), w.admin, create), http.StatusLocked, "CANARY_EXECUTION_LOCKED", "create with another installation's lock")
	var n int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM shop_delivery_attempts WHERE installation_id=$1`, w.a1.InstallationID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a locked request wrote %d attempts (%v)", n, err)
	}

	build(canaryops.NewGate(true, []int64{w.a1.InstallationID}))
	// Authorization: MEMBER and a player are refused; another organization's admin cannot reach it.
	code(w.do(http.MethodPost, path(w.a1, ""), w.member, create), http.StatusForbidden, "SHOP_FORBIDDEN", "member")
	code(w.do(http.MethodPost, path(w.a1, ""), player, create), http.StatusForbidden, "SHOP_FORBIDDEN", "player")
	code(w.do(http.MethodGet, path(w.a1, ""), w.b1.OwnerDiscordID, nil), http.StatusForbidden, "SHOP_FORBIDDEN", "other org owner")
	// A missing altitude is never invented.
	noAlt := map[string]any{}
	for k, v := range create {
		noAlt[k] = v
	}
	delete(noAlt, "altitudeY")
	code(w.do(http.MethodPost, path(w.a1, ""), w.admin, noAlt), http.StatusBadRequest, "INVALID_REQUEST", "no altitude")

	res := w.expect(w.do(http.MethodPost, path(w.a1, ""), w.admin, create), http.StatusCreated, "create").JSON(t)
	att := res["attempt"].(map[string]any)
	id := att["attemptId"].(string)
	if id != fmt.Sprintf("champion:d%d:a1", delivery) || att["state"] != "PLAN_CREATED" || res["executionLocked"] != false {
		t.Fatalf("%v", res)
	}
	code(w.do(http.MethodPost, path(w.a1, ""), w.admin, create), http.StatusConflict, "ATTEMPT_CONFLICT", "duplicate")
	adv := func(from, to, reason string) *apiResult {
		return w.do(http.MethodPost, path(w.a1, "/"+id+"/advance"), w.admin, map[string]any{"from": from, "to": to, "reason": reason})
	}
	w.expect(adv("PLAN_CREATED", "FILE_PREPARED", ""), http.StatusOK, "prepare")
	code(adv("PLAN_CREATED", "FILE_PREPARED", ""), http.StatusConflict, "ATTEMPT_STATE_CHANGED", "stale advance")
	code(adv("FILE_PREPARED", "FILE_STAGED", ""), http.StatusConflict, "EVIDENCE_REQUIRED", "staging without evidence")
	// The refund path answers 409 while an upload may be in flight.
	code(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", purchase)), w.admin, map[string]any{"reason": "x"}),
		http.StatusConflict, "DELIVERY_ATTEMPT_ACTIVE", "refund while prepared")
	ev := func(body map[string]any) *apiResult {
		return w.do(http.MethodPost, path(w.a1, "/"+id+"/evidence"), w.admin, body)
	}
	at := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	code(ev(map[string]any{"kind": "ITEM_OBSERVED", "source": "RPT_LOG", "observedBy": "x", "observedAt": at}), http.StatusBadRequest, "INVALID_REQUEST", "RPT as proof")
	code(ev(map[string]any{"kind": "ITEM_OBSERVED", "source": "IN_GAME_OBSERVATION", "observedBy": "x", "observedAt": at}), http.StatusConflict, "EVIDENCE_NOT_ACCEPTED", "item before staging")
	pos := att["position"].([]any)
	stagedFile, emptyFile := nitradodelivery.SingleAttemptFiles(id, att["className"].(string), int(att["quantity"].(float64)),
		[3]float64{pos[0].(float64), pos[1].(float64), pos[2].(float64)})
	stagedSHA, emptySHA := nitradodelivery.SHA256(stagedFile), nitradodelivery.SHA256(emptyFile)
	code(ev(map[string]any{"kind": "STAGED_FILE_HASH", "source": "NITRADO_READBACK", "sha256": strings.Repeat("5", 64), "previousSha256": emptySHA, "observedAt": at}), http.StatusConflict, "ARTIFACT_HASH_MISMATCH", "wrong artifact")
	w.expect(ev(map[string]any{"kind": "STAGED_FILE_HASH", "source": "NITRADO_READBACK", "sha256": stagedSHA, "previousSha256": emptySHA, "observedAt": at}), http.StatusCreated, "staged hash")
	code(ev(map[string]any{"kind": "STAGED_FILE_HASH", "source": "NITRADO_READBACK", "sha256": stagedSHA, "previousSha256": emptySHA, "observedAt": at}), http.StatusConflict, "EVIDENCE_ALREADY_RECORDED", "write-once")
	w.expect(ev(map[string]any{"kind": "STAGING_BOOT", "source": "BOOT_AUTHORITY", "bootFile": session, "observedAt": at}), http.StatusCreated, "staging boot")
	staged := w.expect(adv("FILE_PREPARED", "FILE_STAGED", ""), http.StatusOK, "stage").JSON(t)
	if a := staged["attempt"].(map[string]any); a["stagedSha256"] != stagedSHA || a["stagedBootFile"] != session || a["refundBlocked"] != true {
		t.Fatalf("%v", a)
	}
	w.expect(adv("FILE_STAGED", "AWAITING_RESTART", ""), http.StatusOK, "await")
	// A second boot before the verified unstage -> FAILED_REVIEW; UNCERTAIN keeps it blocked.
	code(adv("AWAITING_RESTART", "FAILED_REVIEW", ""), http.StatusBadRequest, "INVALID_REQUEST", "review without reason")
	w.expect(adv("AWAITING_RESTART", "FAILED_REVIEW", "two boots before the unstage was verified"), http.StatusOK, "review")
	rv := w.expect(w.do(http.MethodPost, path(w.a1, "/"+id+"/review"), w.admin, map[string]any{"outcome": "UNCERTAIN", "note": "cannot confirm"}), http.StatusOK, "uncertain").JSON(t)
	if a := rv["attempt"].(map[string]any); a["reviewResolution"] != nil || a["refundBlocked"] != true || a["manualFulfilBlocked"] != true {
		t.Fatalf("%v", a)
	}
	code(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/fulfill", purchase)), w.admin, nil), http.StatusConflict, "DELIVERY_ATTEMPT_ACTIVE", "manual fulfil while uncertain")
	code(w.do(http.MethodPost, path(w.a1, "/"+id+"/review"), w.admin, map[string]any{"outcome": "NOT_SPAWNED", "note": "checked in game"}), http.StatusConflict, "EVIDENCE_REQUIRED", "resolution without observation")
	w.expect(ev(map[string]any{"kind": "REVIEW_OBSERVATION", "source": "IN_GAME_OBSERVATION", "observedBy": "admin-in-game", "observedAt": at, "detail": "drop point empty"}), http.StatusCreated, "review observation")
	w.expect(w.do(http.MethodPost, path(w.a1, "/"+id+"/review"), w.admin, map[string]any{"outcome": "NOT_SPAWNED", "note": "checked in game"}), http.StatusOK, "not spawned")
	w.expect(w.do(http.MethodPost, w.shopPath(w.a1, fmt.Sprintf("/purchases/%d/refund", purchase)), w.admin, map[string]any{"reason": "canary aborted"}), http.StatusOK, "refund after NOT_SPAWNED")
	// The full history is readable and every entry names its actor.
	got := w.expect(w.do(http.MethodGet, path(w.a1, "/"+id), w.admin, nil), http.StatusOK, "get").JSON(t)
	for _, h := range got["history"].([]any) {
		if h.(map[string]any)["actor"] == "" {
			t.Fatalf("%v", h)
		}
	}
	for _, e := range got["evidence"].([]any) {
		if e.(map[string]any)["recordedBy"] == "" || e.(map[string]any)["source"] == "" {
			t.Fatalf("%v", e)
		}
	}
	code(w.do(http.MethodGet, path(w.a1, "/champion:d999999999:a1"), w.admin, nil), http.StatusNotFound, "ATTEMPT_NOT_FOUND", "unknown attempt")
	w.ledgerIntegrity(w.a1)
}
