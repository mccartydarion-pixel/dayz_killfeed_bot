// Command shop-ledger-verify is the READ-ONLY post-deployment acceptance check for the Shop delivery
// ledger rollout (docs/SHOP_LEDGER_ROLLOUT.md). It opens a READ ONLY transaction, reads aggregates
// only (no player names, no personal ids, no credentials) and compares them with the pre-deployment
// baseline passed as flags. It never writes, never runs migrations and never prints the connection
// string.
//
//	DATABASE_PUBLIC_URL=... go run ./cmd/shop-ledger-verify -phase 0054 \
//	    -migrations 54 -purchases 0 -deliveries 0 -ledger-rows 2 -ledger-sum 10000500 -balance-sum 10000500
//
// Optional: -api https://<host> checks, with the website service secret and a platform-admin id from
// WEBSITE_API_SECRET / CHAMPION_ADMIN_DISCORD_IDS, that the canary operator API reports the execution
// lock closed and that a mutation is refused with HTTP 423 (phase 0055 only). The mutation probe sends
// an attempt request for delivery 0, which cannot create anything even if the lock were open.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type check struct {
	name string
	ok   bool
	got  string
}

func main() {
	phase := flag.String("phase", "0054", "deployed phase: 0054 (PR #97) or 0055 (PR #98)")
	migrations := flag.Int("migrations", -1, "expected number of applied migrations (-1: skip)")
	purchases := flag.Int64("purchases", -1, "baseline shop_purchases rows")
	deliveries := flag.Int64("deliveries", -1, "baseline shop_deliveries rows")
	ledgerRows := flag.Int64("ledger-rows", -1, "baseline point_transactions rows")
	ledgerSum := flag.Int64("ledger-sum", -1<<62, "baseline point_transactions SUM(amount)")
	balanceSum := flag.Int64("balance-sum", -1<<62, "baseline player_points SUM(balance)")
	api := flag.String("api", "", "optional base URL of the deployed service for the canary lock check")
	org := flag.Int64("org", 1, "organization id for the canary lock check")
	inst := flag.Int64("installation", 11, "installation id for the canary lock check")
	flag.Parse()
	if *phase != "0054" && *phase != "0055" {
		fail("phase must be 0054 or 0055")
	}
	dsn := os.Getenv("DATABASE_PUBLIC_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fail("DATABASE_PUBLIC_URL or DATABASE_URL must be set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fail("database connection failed")
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		fail("read-only transaction failed")
	}
	defer tx.Rollback(ctx)

	var checks []check
	add := func(name string, ok bool, got string) { checks = append(checks, check{name, ok, got}) }
	// Each check runs in its own savepoint, so a missing object fails that check only.
	scalar := func(sql string) (string, error) {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return "", err
		}
		defer sp.Rollback(ctx)
		var s *string
		if err := sp.QueryRow(ctx, sql).Scan(&s); err != nil {
			return "", err
		}
		if s == nil {
			return "null", nil
		}
		return *s, nil
	}
	expectSQL := func(name, sql, want string) {
		got, err := scalar(sql)
		if err != nil {
			add(name, false, "error: "+err.Error())
			return
		}
		add(name, got == want, got)
	}

	// Migrations: applied exactly once, in order, nothing unexpected.
	expectSQL("0054_shop_delivery_attempts applied once", `SELECT COUNT(*)::text FROM schema_migrations WHERE name='0054_shop_delivery_attempts'`, "1")
	if *phase == "0055" {
		expectSQL("0055_shop_delivery_attempt_evidence applied once", `SELECT COUNT(*)::text FROM schema_migrations WHERE name='0055_shop_delivery_attempt_evidence'`, "1")
	} else {
		expectSQL("0055 not applied yet", `SELECT COUNT(*)::text FROM schema_migrations WHERE name LIKE '0055\_%'`, "0")
	}
	expectSQL("no duplicate migration names", `SELECT COUNT(*)::text FROM (SELECT name FROM schema_migrations GROUP BY name HAVING COUNT(*) > 1) x`, "0")
	expectSQL("no duplicate migration numbers", `SELECT COUNT(*)::text FROM (SELECT left(name,4) FROM schema_migrations GROUP BY 1 HAVING COUNT(*) > 1) x`, "0")
	expectSQL("0054 applied after 0053", `SELECT ((SELECT applied_at FROM schema_migrations WHERE name='0054_shop_delivery_attempts') >= (SELECT applied_at FROM schema_migrations WHERE name='0053_installation_embed_activation'))::text`, "true")
	if *migrations >= 0 {
		expectSQL("applied migration count", `SELECT COUNT(*)::text FROM schema_migrations`, fmt.Sprint(*migrations))
	}

	// Schema objects.
	expectSQL("shop_delivery_attempts exists", `SELECT (to_regclass('shop_delivery_attempts') IS NOT NULL)::text`, "true")
	expectSQL("shop_delivery_attempt_events exists", `SELECT (to_regclass('shop_delivery_attempt_events') IS NOT NULL)::text`, "true")
	expectSQL("uq_shop_deliveries_id_tenant exists", `SELECT (to_regclass('uq_shop_deliveries_id_tenant') IS NOT NULL)::text`, "true")
	expectSQL("refund/fulfil guard trigger on shop_deliveries", `SELECT COUNT(*)::text FROM pg_trigger WHERE tgrelid='shop_deliveries'::regclass AND tgname='trg_shop_delivery_exposure_guard' AND tgenabled <> 'D'`, "1")
	if *phase == "0055" {
		expectSQL("shop_delivery_attempt_evidence exists", `SELECT (to_regclass('shop_delivery_attempt_evidence') IS NOT NULL)::text`, "true")
		expectSQL("review-evidence trigger present", `SELECT COUNT(*)::text FROM pg_trigger WHERE tgname='trg_shop_attempt_resolution_evidence' AND NOT tgisinternal`, "1")
	}

	// No delivery attempt exists: nothing automatic ran, and the canary has not started.
	expectSQL("no delivery attempts", `SELECT COUNT(*)::text FROM shop_delivery_attempts`, "0")
	expectSQL("no attempt history", `SELECT COUNT(*)::text FROM shop_delivery_attempt_events`, "0")
	if *phase == "0055" {
		expectSQL("no attempt evidence", `SELECT COUNT(*)::text FROM shop_delivery_attempt_evidence`, "0")
	}

	// Existing Shop and economy data unchanged against the baseline.
	if *purchases >= 0 {
		expectSQL("shop_purchases rows unchanged", `SELECT COUNT(*)::text FROM shop_purchases`, fmt.Sprint(*purchases))
	}
	if *deliveries >= 0 {
		expectSQL("shop_deliveries rows unchanged", `SELECT COUNT(*)::text FROM shop_deliveries`, fmt.Sprint(*deliveries))
	}
	if *ledgerRows >= 0 {
		expectSQL("point_transactions rows unchanged", `SELECT COUNT(*)::text FROM point_transactions`, fmt.Sprint(*ledgerRows))
	}
	if *ledgerSum != -1<<62 {
		expectSQL("point_transactions sum unchanged", `SELECT COALESCE(SUM(amount),0)::text FROM point_transactions`, fmt.Sprint(*ledgerSum))
	}
	if *balanceSum != -1<<62 {
		expectSQL("player balances sum unchanged", `SELECT COALESCE(SUM(balance),0)::text FROM player_points`, fmt.Sprint(*balanceSum))
	}
	expectSQL("every shop purchase has one delivery", `SELECT COUNT(*)::text FROM shop_purchases p WHERE NOT EXISTS (SELECT 1 FROM shop_deliveries d WHERE d.purchase_id = p.id)`, "0")

	if *api != "" && *phase == "0055" {
		checks = append(checks, apiChecks(*api, *org, *inst)...)
	}

	failed := 0
	for _, c := range checks {
		mark := "PASS"
		if !c.ok {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-48s %s\n", mark, c.name, c.got)
	}
	if failed > 0 {
		fmt.Printf("%d check(s) failed\n", failed)
		os.Exit(1)
	}
	fmt.Println("all checks passed")
}

func apiChecks(base string, org, inst int64) []check {
	secret := os.Getenv("WEBSITE_API_SECRET")
	ids := strings.FieldsFunc(os.Getenv("CHAMPION_ADMIN_DISCORD_IDS"), func(r rune) bool { return r == ',' || r == ' ' })
	actor := os.Getenv("CANARY_CHECK_ACTING_USER") // an OWNER/ADMIN of the organization
	if actor == "" && len(ids) > 0 {
		actor = ids[0]
	}
	if secret == "" || actor == "" {
		return []check{{"canary API check", false, "WEBSITE_API_SECRET and an acting OWNER/ADMIN id are required"}}
	}
	path := fmt.Sprintf("%s/api/saas/organizations/%d/installations/%d/shop/canary/attempts", strings.TrimRight(base, "/"), org, inst)
	do := func(method string, body []byte) (int, []byte, error) {
		req, err := http.NewRequest(method, path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-Champion-Acting-User", actor)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return 0, nil, errors.New("request failed")
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, b, nil
	}
	var out []check
	status, body, err := do(http.MethodGet, nil)
	var list struct {
		Locked   bool  `json:"executionLocked"`
		Attempts []any `json:"attempts"`
	}
	_ = json.Unmarshal(body, &list)
	out = append(out, check{"canary read endpoint answers", err == nil && status == http.StatusOK, fmt.Sprint(status)})
	out = append(out, check{"canary execution locked", status == http.StatusOK && list.Locked, fmt.Sprint(list.Locked)})
	out = append(out, check{"canary lists no attempts", status == http.StatusOK && len(list.Attempts) == 0, fmt.Sprint(len(list.Attempts))})
	// Delivery 0 cannot exist: even an open lock would answer 400 and write nothing.
	status, _, err = do(http.MethodPost, []byte(`{"deliveryId":0,"altitudeY":0,"dropSourceFile":"lock-check.ADM","dropSourceOffset":1,"dropObservedAt":"2000-01-01T00:00:00Z"}`))
	out = append(out, check{"canary mutation refused with 423", err == nil && status == http.StatusLocked, fmt.Sprint(status)})
	return out
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(2)
}
