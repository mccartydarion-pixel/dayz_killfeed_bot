//go:build integration

package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// End-to-end Champion Points economy web API tests over the real routes and a real PostgreSQL
// (docs/ECONOMY.md): the player balance and history, the admin lookup/grant/debit, identity,
// authorization, tenant and server isolation, idempotency, concurrency, reconciliation, the ECONOMY
// notification hook and audit logging.

func (w *factionWorld) eco(f installationFixture, suffix string) string {
	return fmt.Sprintf("/api/saas/organizations/%d/installations/%d/economy%s", f.OrgID, f.InstallationID, suffix)
}

// secondInstallation adds another installation of the same organization AND Discord guild with its
// own DayZ server (a second server of the same community).
func (w *factionWorld) secondInstallation(f installationFixture) installationFixture {
	w.t.Helper()
	ctx := context.Background()
	var guildID, serverID, instID int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT guild_id FROM discord_guild_connections WHERE id=$1`, f.ConnectionID).Scan(&guildID); err != nil {
		w.t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO game_servers(guild_id, provider, provider_service_id, game, platform, status) VALUES($1,'nitrado',$2,'dayz','PLAYSTATION','ACTIVE') RETURNING id`,
		guildID, fmt.Sprintf("eco2-%d", time.Now().UnixNano())).Scan(&serverID); err != nil {
		w.t.Fatal(err)
	}
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO installations(organization_id, discord_guild_connection_id, game_server_id, status) VALUES($1,$2,$3,'READY') RETURNING id`,
		f.OrgID, f.ConnectionID, serverID).Scan(&instID); err != nil {
		w.t.Fatal(err)
	}
	out := f
	out.InstallationID = instID
	return out
}

func (w *factionWorld) guildOf(f installationFixture) int64 {
	g, _ := w.gameContext(f)
	return g
}

// adjust posts a grant or debit as actor.
func (w *factionWorld) adjust(f installationFixture, op string, account int64, actor string, body any) *apiResult {
	w.t.Helper()
	return w.do(http.MethodPost, w.eco(f, fmt.Sprintf("/accounts/%d/%s", account, op)), actor, body)
}

func (w *factionWorld) grant(f installationFixture, account int64, amount int64) {
	w.t.Helper()
	w.expect(w.adjust(f, "grant", account, w.admin, map[string]any{"amount": amount, "reason": "test funds"}), http.StatusOK, "grant")
}

func (w *factionWorld) balanceOf(f installationFixture, actor string) int64 {
	w.t.Helper()
	acc := w.getJSON(w.eco(f, "/me"), actor)["account"].(map[string]any)
	return int64(acc["balance"].(float64))
}

// ledgerIntegrity asserts, in SQL, that every row of the guild satisfies after = before +/- amount and
// that the account equals its ledger (the reconciliation utility reports nothing).
func (w *factionWorld) ledgerIntegrity(f installationFixture) {
	w.t.Helper()
	ctx := context.Background()
	guild := w.guildOf(f)
	var bad int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM (SELECT amount, balance_after, COALESCE(LAG(balance_after) OVER (PARTITION BY player_id ORDER BY id),0) AS before FROM point_transactions WHERE guild_id=$1) x WHERE balance_after <> before + amount`, guild).Scan(&bad); err != nil {
		w.t.Fatal(err)
	}
	if bad != 0 {
		w.t.Fatalf("%d ledger rows violate balance_after = balance_before + amount", bad)
	}
	scope, err := w.a.EconomyAccounts.Scope(ctx, f.OrgID, f.InstallationID)
	if err != nil {
		w.t.Fatal(err)
	}
	mism, err := w.a.EconomyAccounts.Reconcile(ctx, scope, 10)
	if err != nil || len(mism) != 0 {
		w.t.Fatalf("reconciliation: %+v %v", mism, err)
	}
}

func TestEconomyPlayerBalanceAndTransactions(t *testing.T) {
	w := newFactionWorld(t)
	player, other := w.players[0], w.players[1]
	pid := w.linkPlayer(w.a1, player, "Sgt Balance")
	w.linkPlayer(w.a1, other, "Bystander")

	// Fresh account: zero balance, stable currency metadata, identity from the verified link.
	me := w.getJSON(w.eco(w.a1, "/me"), player)
	if cur := me["currency"].(map[string]any); cur["code"] != "CHAMPION_POINTS" || cur["name"] != "Champion Points" || cur["symbol"] != "pts" {
		t.Fatalf("currency: %v", cur)
	}
	acc := me["account"].(map[string]any)
	if int64(acc["accountId"].(float64)) != pid || acc["gamertag"] != "Sgt Balance" || acc["balance"].(float64) != 0 || acc["updatedAt"] != nil {
		t.Fatalf("fresh account: %v", acc)
	}
	if int64(me["installationId"].(float64)) != w.a1.InstallationID || me["gameServerId"] == nil {
		t.Fatalf("scope fields: %v", me)
	}
	if list := w.getJSON(w.eco(w.a1, "/me/transactions"), player); len(list["items"].([]any)) != 0 || list["nextCursor"] != nil || list["limit"].(float64) != 25 {
		t.Fatalf("empty history: %v", list)
	}

	// 60 transactions: 30 grants of 100 and 30 debits of 10, so the running balance is known.
	for i := 0; i < 30; i++ {
		w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 100, "reason": fmt.Sprintf("secret note %d", i)}), http.StatusOK, "grant")
		w.expect(w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 10, "reason": "fee"}), http.StatusOK, "debit")
	}
	if got := w.balanceOf(w.a1, player); got != 2700 {
		t.Fatalf("balance after 30 x (+100 -10): %d", got)
	}
	w.ledgerIntegrity(w.a1)

	first := w.getJSON(w.eco(w.a1, "/me/transactions"), player)
	items := first["items"].([]any)
	if len(items) != 25 || first["nextCursor"] == nil {
		t.Fatalf("default page is 25 with a cursor: %d", len(items))
	}
	top := items[0].(map[string]any)
	if top["type"] != "ADMIN_DEBIT" || top["direction"] != "DEBIT" || top["amount"].(float64) != 10 || top["balanceAfter"].(float64) != 2700 || top["description"] != "Admin debit" {
		t.Fatalf("newest first: %v", top)
	}
	for _, k := range []string{"id", "type", "direction", "amount", "balanceAfter", "description", "referenceType", "createdAt"} {
		if _, ok := top[k]; !ok {
			t.Errorf("transaction is missing %q", k)
		}
	}
	// Privacy: a player never sees the admin's reason, the admin's id, references or internal ids.
	raw := w.do(http.MethodGet, w.eco(w.a1, "/me/transactions?limit=100"), player, nil).Body
	for _, banned := range []string{"secret note", "fee", "reason", w.admin, "actor", "referenceId", "metadata", "gameServerId", "playerId", "guildId"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Errorf("the player view must not contain %q", banned)
		}
	}

	// Walk the pages with limit 7: 60 rows, no duplicates, strictly newest first, running balance consistent.
	seen := map[float64]bool{}
	cursor, prevID, count := "", 1e18, 0
	for pages := 0; pages < 20; pages++ {
		path := "/me/transactions?limit=7"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		p := w.getJSON(w.eco(w.a1, path), player)
		for _, it := range p["items"].([]any) {
			m := it.(map[string]any)
			id := m["id"].(float64)
			if seen[id] || id >= prevID {
				t.Fatalf("ids must be unique and strictly descending: %v after %v", id, prevID)
			}
			seen[id], prevID = true, id
			count++
		}
		next, _ := p["nextCursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if count != 60 {
		t.Fatalf("walked %d of 60 transactions", count)
	}
	// Type filter, limit clamping and the parameter contract.
	if got := w.getJSON(w.eco(w.a1, "/me/transactions?type=ADMIN_CREDIT&limit=100"), player)["items"].([]any); len(got) != 30 {
		t.Fatalf("filter ADMIN_CREDIT: %d", len(got))
	}
	if got := w.getJSON(w.eco(w.a1, "/me/transactions?type=admin_debit&limit=5"), player)["items"].([]any); len(got) != 5 {
		t.Fatalf("filter is case-insensitive: %d", len(got))
	}
	if got := w.getJSON(w.eco(w.a1, "/me/transactions?type=BOUNTY_CLAIM"), player)["items"].([]any); len(got) != 0 {
		t.Fatalf("filter with no matches: %d", len(got))
	}
	if got := w.getJSON(w.eco(w.a1, "/me/transactions?limit=100000"), player)["limit"].(float64); got != 100 {
		t.Fatalf("limit clamps to 100: %v", got)
	}
	for _, bad := range []string{"limit=0", "limit=-1", "limit=abc", "type=CASINO_BET", "cursor=bogus", "type=A;B"} {
		w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me/transactions?"+bad), player, nil), http.StatusBadRequest, bad)
	}
	// Another player sees only their own (empty) account: there is no way to name someone else.
	if got := w.balanceOf(w.a1, other); got != 0 {
		t.Fatalf("a different player's balance is their own: %d", got)
	}
	w.expect(w.do(http.MethodGet, w.eco(w.a1, fmt.Sprintf("/me/transactions?accountId=%d", pid)), other, nil), http.StatusOK, "an accountId parameter is ignored")
	if got := w.getJSON(w.eco(w.a1, fmt.Sprintf("/me/transactions?accountId=%d", pid)), other)["items"].([]any); len(got) != 0 {
		t.Fatal("a query parameter must never select another player's ledger")
	}
}

func TestEconomyIdentityRequired(t *testing.T) {
	w := newFactionWorld(t)
	unlinked, pending, reader := w.players[0], w.players[1], w.players[2]
	guild := w.guildOf(w.a1)
	var pp int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'PendingName') RETURNING id`, guild, fmt.Sprintf("dz-pend-%d", time.Now().UnixNano())).Scan(&pp); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_links(guild_id, player_id, discord_user_id, status) VALUES($1,$2,$3,'PENDING')`, guild, pp, pending); err != nil {
		t.Fatal(err)
	}
	// A player whose display name equals the unlinked user's Discord name must not be picked up by name.
	w.linkPlayer(w.a1, reader, "Player 0")
	for _, actor := range []string{unlinked, pending} {
		for _, p := range []string{"/me", "/me/transactions"} {
			r := w.expect(w.do(http.MethodGet, w.eco(w.a1, p), actor, nil), http.StatusConflict, "unverified identity "+p)
			if r.errCode(t) != "PLAYER_IDENTITY_REQUIRED" {
				t.Fatalf("%s: %s", p, r.Body)
			}
		}
	}
	// Nothing was created for them.
	var n int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM player_points WHERE guild_id=$1`, guild).Scan(&n); err != nil || n != 0 {
		t.Fatalf("no economy account may be created for an unlinked user: %d %v", n, err)
	}
}

func TestEconomyAuthenticationAndAdminAuthorization(t *testing.T) {
	w := newFactionWorld(t)
	player, leader := w.players[0], w.players[1]
	pid := w.linkPlayer(w.a1, player, "Target")
	w.linkPlayer(w.a1, leader, "Faction Leader")
	w.createFaction(w.a1, leader, "Power Faction", "PWR", "OPEN") // a faction LEADER role grants nothing here

	adminPaths := []string{"/accounts?q=Target", fmt.Sprintf("/accounts/%d", pid), fmt.Sprintf("/accounts/%d/transactions", pid)}
	for _, p := range []string{"/me", "/me/transactions"} {
		w.expect(w.do(http.MethodGet, w.eco(w.a1, p), "", nil), http.StatusUnauthorized, "no acting user "+p)
		w.expect(w.do(http.MethodGet, w.eco(w.a1, p), "never-synced", nil), http.StatusUnauthorized, "unsynced "+p)
	}
	body := map[string]any{"amount": 10, "reason": "sneaky"}
	for _, actor := range []string{player, leader, w.member} { // no role, faction leader, org MEMBER
		for _, p := range adminPaths {
			r := w.expect(w.do(http.MethodGet, w.eco(w.a1, p), actor, nil), http.StatusForbidden, "admin read "+p)
			if r.errCode(t) != "ECONOMY_FORBIDDEN" {
				t.Fatalf("%s: %s", p, r.Body)
			}
		}
		for _, op := range []string{"grant", "debit"} {
			r := w.expect(w.adjust(w.a1, op, pid, actor, body), http.StatusForbidden, op)
			if r.errCode(t) != "ECONOMY_FORBIDDEN" {
				t.Fatalf("%s: %s", op, r.Body)
			}
		}
	}
	w.expect(w.adjust(w.a1, "grant", pid, "", body), http.StatusUnauthorized, "no acting user")
	if got := w.balanceOf(w.a1, player); got != 0 {
		t.Fatalf("no rejected attempt may change the balance: %d", got)
	}
	// Admin and owner are allowed; an admin of ANOTHER organization is not.
	w.expect(w.adjust(w.a1, "grant", pid, w.admin, body), http.StatusOK, "org ADMIN")
	w.expect(w.adjust(w.a1, "grant", pid, w.a1.OwnerDiscordID, body), http.StatusOK, "org OWNER")
	if r := w.adjust(w.a1, "grant", pid, w.b1.OwnerDiscordID, body); r.Status != http.StatusForbidden || r.errCode(t) != "ECONOMY_FORBIDDEN" {
		t.Fatalf("an owner of another organization: %d %s", r.Status, r.Body)
	}
	// Read-only where it must be.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if r := w.do(m, w.eco(w.a1, "/me"), player, nil); r.Status == http.StatusOK {
			t.Errorf("%s /me must not succeed", m)
		}
	}
}

func TestEconomyAdminLookup(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "Ghost Rider")
	w.linkPlayer(w.a1, b, "ghost_walker")
	var un int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `INSERT INTO players(guild_id, dayz_player_id, display_name) VALUES($1,$2,'Ghost Unlinked') RETURNING id`, w.guildOf(w.a1), fmt.Sprintf("dz-un-%d", time.Now().UnixNano())).Scan(&un); err != nil {
		t.Fatal(err)
	}
	w.grant(w.a1, pa, 250)

	res := w.getJSON(w.eco(w.a1, "/accounts?q=ghost"), w.admin)
	items := res["items"].([]any)
	if len(items) != 3 || res["currency"].(map[string]any)["code"] != "CHAMPION_POINTS" {
		t.Fatalf("case-insensitive gamertag search: %v", res)
	}
	byName := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byName[m["gamertag"].(string)] = m
	}
	rider := byName["Ghost Rider"]
	if rider["balance"].(float64) != 250 || rider["linked"] != true || rider["discordUserId"] != a || rider["displayName"] == nil || int64(rider["accountId"].(float64)) != pa || rider["updatedAt"] == nil {
		t.Fatalf("linked account: %v", rider)
	}
	if u := byName["Ghost Unlinked"]; u["linked"] != false || u["discordUserId"] != nil || u["balance"].(float64) != 0 {
		t.Fatalf("unlinked account: %v", u)
	}
	// By the linked Discord identity ("Player 0" is the synced display name of players[0]).
	if got := w.getJSON(w.eco(w.a1, "/accounts?q="+url.QueryEscape("player 0")), w.admin)["items"].([]any); len(got) != 1 || got[0].(map[string]any)["gamertag"] != "Ghost Rider" {
		t.Fatalf("search by linked display identity: %v", got)
	}
	// Exact match ranks first; LIKE wildcards are literal; bad queries are 400.
	if got := w.getJSON(w.eco(w.a1, "/accounts?q="+url.QueryEscape("Ghost Rider")), w.admin)["items"].([]any); len(got) != 1 {
		t.Fatalf("exact: %v", got)
	}
	for _, q := range []string{"%%", "__", "gh%"} {
		if got := w.getJSON(w.eco(w.a1, "/accounts?q="+url.QueryEscape(q)), w.admin)["items"].([]any); len(got) != 0 {
			t.Errorf("q=%q must not act as a wildcard: %v", q, got)
		}
	}
	for _, q := range []string{"", "a", strings.Repeat("x", 51)} {
		w.expect(w.do(http.MethodGet, w.eco(w.a1, "/accounts?q="+url.QueryEscape(q)), w.admin, nil), http.StatusBadRequest, "q="+q)
	}
	// Detail, another guild's account and unknown ids.
	w.expect(w.do(http.MethodGet, w.eco(w.a1, fmt.Sprintf("/accounts/%d", pa)), w.admin, nil), http.StatusOK, "detail")
	pb := w.linkPlayer(w.b1, w.players[2], "Foreign Ghost")
	for _, id := range []int64{pb, 999999999} {
		r := w.expect(w.do(http.MethodGet, w.eco(w.a1, fmt.Sprintf("/accounts/%d", id)), w.admin, nil), http.StatusNotFound, "foreign account")
		if r.errCode(t) != "ECONOMY_ACCOUNT_NOT_FOUND" {
			t.Fatalf("%s", r.Body)
		}
	}
	// The admin history shows the reason and the actor; a system row has no actor.
	hist := w.getJSON(w.eco(w.a1, fmt.Sprintf("/accounts/%d/transactions", pa)), w.admin)["items"].([]any)
	if len(hist) != 1 {
		t.Fatalf("history: %v", hist)
	}
	h := hist[0].(map[string]any)
	if h["reason"] != "test funds" || h["actorDiscordUserId"] != w.admin || h["isSystem"] != false || h["type"] != "ADMIN_CREDIT" || h["gameServerId"] == nil {
		t.Fatalf("admin history row: %v", h)
	}
}

func TestEconomyAdjustmentsValidationAndLedger(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Ledger Guy")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	bad := []struct {
		body any
		code string
		http int
	}{
		{map[string]any{"amount": 0, "reason": "x"}, "INVALID_AMOUNT", 400},
		{map[string]any{"amount": -5, "reason": "x"}, "INVALID_AMOUNT", 400},
		{map[string]any{"amount": 1.5, "reason": "x"}, "INVALID_AMOUNT", 400},
		{`{"amount":"abc","reason":"x"}`, "", 400},
		{`{"amount":1e3,"reason":"x"}`, "INVALID_AMOUNT", 400},
		{map[string]any{"amount": 1_000_000_001, "reason": "x"}, "INVALID_AMOUNT", 400},
		{`{"amount":9223372036854775808,"reason":"x"}`, "INVALID_AMOUNT", 400},
		{map[string]any{"reason": "x"}, "INVALID_AMOUNT", 400},
		{map[string]any{"amount": 5}, "INVALID_REQUEST", 400},
		{map[string]any{"amount": 5, "reason": "   "}, "INVALID_REQUEST", 400},
		{map[string]any{"amount": 5, "reason": "x", "idempotencyKey": "short"}, "INVALID_REQUEST", 400},
		{map[string]any{"amount": 5, "reason": "x", "playerId": 7}, "INVALID_REQUEST", 400}, // unknown key
		{map[string]any{"amount": 5, "reason": "x", "balance": 999999}, "INVALID_REQUEST", 400},
	}
	for i, c := range bad {
		r := w.adjust(w.a1, "grant", pid, w.admin, c.body)
		if r.Status != c.http || (c.code != "" && r.errCode(t) != c.code) {
			t.Errorf("case %d %v: %d %s", i, c.body, r.Status, r.Body)
		}
	}
	if got := w.balanceOf(w.a1, player); got != 0 {
		t.Fatalf("no rejected request may change the balance: %d", got)
	}
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, w.guildOf(w.a1)); n != 0 {
		t.Fatalf("no rejected request may write a ledger row: %d", n)
	}

	// The maximum amount works; a control-character reason is cleaned; a 300-char reason is bounded.
	r := w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 1_000_000_000, "reason": "big\x00 bonus\n" + strings.Repeat("y", 300)}), http.StatusOK, "max grant")
	tx := r.JSON(t)["transaction"].(map[string]any)
	if reason := tx["reason"].(string); strings.ContainsAny(reason, "\x00\n") || len([]rune(reason)) != 200 {
		t.Fatalf("reason handling: %q", reason)
	}
	if tx["direction"] != "CREDIT" || tx["amount"].(float64) != 1e9 || tx["balanceAfter"].(float64) != 1e9 || tx["type"] != "ADMIN_CREDIT" {
		t.Fatalf("grant transaction: %v", tx)
	}
	if acc := r.JSON(t)["account"].(map[string]any); acc["balance"].(float64) != 1e9 || r.JSON(t)["duplicate"] != false {
		t.Fatalf("grant response: %v", r.JSON(t))
	}
	// Debit: insufficient funds is a 409 with nothing written; an exact spend empties the account.
	dr := w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 1_000_000_000, "reason": "reclaim"})
	w.expect(dr, http.StatusOK, "debit all")
	if r := w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 1, "reason": "too much"}); r.Status != http.StatusConflict || r.errCode(t) != "INSUFFICIENT_FUNDS" {
		t.Fatalf("overdraft: %d %s", r.Status, r.Body)
	}
	if got := w.balanceOf(w.a1, player); got != 0 {
		t.Fatalf("balance after the exact debit: %d", got)
	}
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, w.guildOf(w.a1)); n != 2 {
		t.Fatalf("exactly the two successful operations are in the ledger: %d", n)
	}
	// The ledger is append-only: the database refuses to rewrite a financial value.
	if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE point_transactions SET amount = amount + 1 WHERE guild_id=$1`, w.guildOf(w.a1)); err == nil {
		t.Fatal("the ledger must refuse an UPDATE of a financial value")
	}
	// A grant is spendable balance only; it never raises the leaderboard scores.
	var lifetime int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT lifetime_points FROM player_points WHERE guild_id=$1 AND player_id=$2`, w.guildOf(w.a1), pid).Scan(&lifetime); err != nil || lifetime != 0 {
		t.Fatalf("an admin grant must not manufacture leaderboard rank: %d %v", lifetime, err)
	}
	w.ledgerIntegrity(w.a1)

	// Audit: who, where, which account, how much, which transaction - and never the reason text.
	out := logs.String()
	for _, want := range []string{"economy_admin_grant", "economy_admin_debit", "account_id=" + strconv.FormatInt(pid, 10), "transaction_id=", "amount=1000000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log is missing %q", want)
		}
	}
	for _, banned := range []string{"reclaim", "bonus", "test-secret"} {
		if strings.Contains(out, banned) {
			t.Errorf("the audit log must not contain %q", banned)
		}
	}
}

func (w *factionWorld) count(sql string, args ...any) int64 {
	w.t.Helper()
	var n int64
	if err := w.a.DB.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestEconomyIdempotencyThroughTheAPI(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Twice Guy")
	key := "reward-2026-09-19-a"
	first := w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 500, "reason": "event bonus", "idempotencyKey": key}), http.StatusOK, "first").JSON(t)
	second := w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 500, "reason": "event bonus (retry)", "idempotencyKey": key}), http.StatusOK, "replay").JSON(t)
	if first["duplicate"] != false || second["duplicate"] != true {
		t.Fatalf("duplicate flags: %v %v", first["duplicate"], second["duplicate"])
	}
	if first["transaction"].(map[string]any)["id"] != second["transaction"].(map[string]any)["id"] {
		t.Fatal("a replay must return the original transaction")
	}
	if got := w.balanceOf(w.a1, player); got != 500 {
		t.Fatalf("one credit only: %d", got)
	}
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1 AND player_id=$2`, w.guildOf(w.a1), pid); n != 1 {
		t.Fatalf("one ledger row: %d", n)
	}
	// The same key with another amount is refused, not applied.
	r := w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 900, "reason": "x", "idempotencyKey": key})
	if r.Status != http.StatusConflict || r.errCode(t) != "DUPLICATE_TRANSACTION" {
		t.Fatalf("key reuse with a different amount: %d %s", r.Status, r.Body)
	}
	// 20 concurrent identical requests apply exactly once.
	key2 := "burst-key-0000000001"
	var wg sync.WaitGroup
	var mu sync.Mutex
	dups, news := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 7, "reason": "burst", "idempotencyKey": key2})
			mu.Lock()
			defer mu.Unlock()
			if r.Status != http.StatusOK {
				t.Errorf("burst: %d %s", r.Status, r.Body)
				return
			}
			if r.JSON(t)["duplicate"] == true {
				dups++
			} else {
				news++
			}
		}()
	}
	wg.Wait()
	if news != 1 || dups != 19 || w.balanceOf(w.a1, player) != 507 {
		t.Fatalf("burst: %d new, %d duplicates, balance %d", news, dups, w.balanceOf(w.a1, player))
	}
	w.ledgerIntegrity(w.a1)
}

func TestEconomyConcurrentDebitsThroughTheAPI(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Racer")
	w.grant(w.a1, pid, 100)
	// Two simultaneous debits of 80 against a balance of 100: exactly one succeeds.
	for round := 0; round < 5; round++ {
		if bal := w.balanceOf(w.a1, player); bal < 100 {
			w.grant(w.a1, pid, 100-bal) // top back up to 100
		}
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 80, "reason": "race"}).Status
			}(i)
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("round %d: statuses %v", round, codes)
		}
		if got := w.balanceOf(w.a1, player); got != 20 {
			t.Fatalf("round %d: final balance %d, want 20 (never negative)", round, got)
		}
	}
	// A mixed storm of grants and debits reconciles.
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op := "grant"
			if i%2 == 1 {
				op = "debit"
			}
			_ = w.adjust(w.a1, op, pid, w.admin, map[string]any{"amount": 30 + i, "reason": "storm"})
		}(i)
	}
	wg.Wait()
	if w.balanceOf(w.a1, player) < 0 {
		t.Fatal("negative balance")
	}
	w.ledgerIntegrity(w.a1)
}

func TestEconomyTenantAndServerIsolation(t *testing.T) {
	w := newFactionWorld(t)
	a, b := w.players[0], w.players[1]
	pa := w.linkPlayer(w.a1, a, "Alpha Player")
	pb := w.linkPlayer(w.b1, a, "Same User On Org B") // the SAME Champion user, another organization/guild
	w.linkPlayer(w.b1, b, "Beta Player")
	a1b := w.secondInstallation(w.a1) // same organization, same Discord guild, another server

	w.grant(w.a1, pa, 700)
	// The same user has an independent balance on the other organization's installation.
	if got := w.balanceOf(w.b1, a); got != 0 {
		t.Fatalf("balances are never shared across unrelated installations: %d", got)
	}
	w.expect(w.adjust(w.b1, "grant", pb, w.b1.OwnerDiscordID, map[string]any{"amount": 40, "reason": "org b"}), http.StatusOK, "grant on B")
	if w.balanceOf(w.a1, a) != 700 || w.balanceOf(w.b1, a) != 40 {
		t.Fatalf("independent balances: A=%d B=%d", w.balanceOf(w.a1, a), w.balanceOf(w.b1, a))
	}
	// Documented scope: a balance is per Discord guild, so two installations of the SAME guild show the same account.
	if got := w.balanceOf(a1b, a); got != 700 {
		t.Fatalf("same guild, second installation: %d", got)
	}
	w.expect(w.adjust(a1b, "debit", pa, w.admin, map[string]any{"amount": 100, "reason": "from server 2"}), http.StatusOK, "debit via 2nd installation")
	if w.balanceOf(w.a1, a) != 600 {
		t.Fatal("the guild-wide balance moved")
	}
	// The ledger row is attributed to the installation's own server.
	rows := w.getJSON(w.eco(w.a1, fmt.Sprintf("/accounts/%d/transactions", pa)), w.admin)["items"].([]any)
	_, s1 := w.gameContext(w.a1)
	_, s2 := w.gameContext(a1b)
	servers := map[int64]bool{}
	for _, it := range rows {
		servers[int64(it.(map[string]any)["gameServerId"].(float64))] = true
	}
	if !servers[s1] || !servers[s2] {
		t.Fatalf("each write is attributed to its installation's server: %v want %d and %d", servers, s1, s2)
	}
	// Cross-tenant: org A's admin cannot touch org B's installation or accounts, and ids do not cross.
	crossOrg := installationFixture{OrgID: w.b1.OrgID, InstallationID: w.a1.InstallationID}
	crossInst := installationFixture{OrgID: w.a1.OrgID, InstallationID: w.b1.InstallationID}
	for _, scope := range []installationFixture{crossOrg, crossInst} {
		w.expect(w.do(http.MethodGet, w.eco(scope, "/me"), a, nil), http.StatusNotFound, "mismatched installation /me")
		w.expect(w.do(http.MethodGet, w.eco(scope, "/me/transactions"), a, nil), http.StatusNotFound, "mismatched installation history")
	}
	w.expect(w.adjust(crossInst, "grant", pb, w.admin, map[string]any{"amount": 1, "reason": "x"}), http.StatusNotFound, "cross installation grant")
	w.expect(w.adjust(w.b1, "grant", pa, w.b1.OwnerDiscordID, map[string]any{"amount": 1, "reason": "x"}), http.StatusNotFound, "org A's account id on org B's installation")
	w.expect(w.adjust(w.a1, "debit", pb, w.admin, map[string]any{"amount": 1, "reason": "x"}), http.StatusNotFound, "org B's account id on org A's installation")
	w.expect(w.adjust(w.b1, "grant", pb, w.admin, map[string]any{"amount": 1, "reason": "x"}), http.StatusForbidden, "org A's admin on org B")
	if w.balanceOf(w.b1, a) != 40 || w.balanceOf(w.a1, a) != 600 {
		t.Fatal("no cross-tenant call may change a balance")
	}
	w.ledgerIntegrity(w.a1)
	w.ledgerIntegrity(w.b1)
}

func TestEconomyIsIndependentOfFactions(t *testing.T) {
	w := newFactionWorld(t)
	leader, joiner := w.players[0], w.players[1]
	w.linkPlayer(w.a1, leader, "Boss")
	jp := w.linkPlayer(w.a1, joiner, "Wanderer")
	w.grant(w.a1, jp, 321)
	f := w.createFaction(w.a1, leader, "Economy Neutral", "ENU", "OPEN")
	fid := idOf(f)
	w.joined(leader, fid, joiner)
	if got := w.balanceOf(w.a1, joiner); got != 321 {
		t.Fatalf("joining a faction must not touch the balance: %d", got)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, fmt.Sprintf("/%d/leave", fid)), joiner, nil), http.StatusOK, "leave")
	f2 := w.createFaction(w.a1, joiner, "Second Home", "SHM", "OPEN")
	if got := w.balanceOf(w.a1, joiner); got != 321 || idOf(f2) == 0 {
		t.Fatalf("leaving and founding another faction keeps the balance: %d", got)
	}
	w.ledgerIntegrity(w.a1)
}

// recorder is a test economy notifier (the ECONOMY feed's seam).
type ecoRecorder struct {
	mu     sync.Mutex
	events []economy.Event
	boom   bool
}

func (r *ecoRecorder) Notify(e economy.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	if r.boom {
		panic("notifier exploded")
	}
}

func (r *ecoRecorder) list() []economy.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]economy.Event(nil), r.events...)
}

func TestEconomyAdminAdjustmentsFeedTheECONOMYNotifier(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Notified")
	rec := &ecoRecorder{}
	w.a.EconomyService.SetNotifier(rec)

	w.getJSON(w.eco(w.a1, "/me"), player)
	w.getJSON(w.eco(w.a1, "/me/transactions"), player)
	if len(rec.list()) != 0 {
		t.Fatal("reads must never notify")
	}
	w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 900, "reason": "private note", "idempotencyKey": "notify-key-000001"}), http.StatusOK, "grant")
	w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 900, "reason": "private note", "idempotencyKey": "notify-key-000001"}), http.StatusOK, "replay")
	w.expect(w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 400, "reason": "fee"}), http.StatusOK, "debit")
	w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 99999, "reason": "too much"}) // refused
	evs := rec.list()
	if len(evs) != 2 {
		t.Fatalf("one event per committed transaction (no replay, no refusal): %d %+v", len(evs), evs)
	}
	guild, server := w.gameContext(w.a1)
	if e := evs[0]; e.Type != "ADMIN_CREDIT" || !e.Credit || e.Amount != 900 || e.BalanceAfter != 900 || e.PlayerName != "Notified" || e.GuildID != guild || e.ServerID != server {
		t.Fatalf("grant event: %+v", e)
	}
	if e := evs[1]; e.Type != "ADMIN_DEBIT" || e.Credit || e.Amount != 400 || e.BalanceAfter != 500 {
		t.Fatalf("debit event: %+v", e)
	}
	// A notifier that panics can neither fail nor repeat the transaction.
	rec.boom = true
	w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 5, "reason": "after panic"}), http.StatusOK, "grant with a panicking notifier")
	if got := w.balanceOf(w.a1, player); got != 505 {
		t.Fatalf("the transaction stays committed: %d", got)
	}
}

func TestEconomyReconciliationDetectsDriftAndNeverRepairsOnRead(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Drifter")
	w.grant(w.a1, pid, 1000)
	w.expect(w.adjust(w.a1, "debit", pid, w.admin, map[string]any{"amount": 250, "reason": "x"}), http.StatusOK, "debit")
	w.ledgerIntegrity(w.a1)

	ctx := context.Background()
	guild := w.guildOf(w.a1)
	scope, _ := w.a.EconomyAccounts.Scope(ctx, w.a1.OrgID, w.a1.InstallationID)
	// Simulate corruption (outside the API): the balance no longer equals the ledger.
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE player_points SET balance = balance + 5 WHERE guild_id=$1 AND player_id=$2`, guild, pid); err != nil {
		t.Fatal(err)
	}
	mism, err := w.a.EconomyAccounts.Reconcile(ctx, scope, 10)
	if err != nil || len(mism) != 1 || mism[0].Kind != repository.MismatchBalance || mism[0].PlayerID != pid || mism[0].Balance != 755 || mism[0].LedgerSum != 750 {
		t.Fatalf("balance drift: %+v %v", mism, err)
	}
	// Reading the balance neither hides nor repairs the drift.
	if got := w.balanceOf(w.a1, player); got != 755 {
		t.Fatalf("reads report the stored balance: %d", got)
	}
	if again, _ := w.a.EconomyAccounts.Reconcile(ctx, scope, 10); len(again) != 1 {
		t.Fatal("a read must not repair anything")
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `UPDATE player_points SET balance = 750 WHERE guild_id=$1 AND player_id=$2`, guild, pid); err != nil {
		t.Fatal(err)
	}
	// The database itself refuses to remove a ledger row ...
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM point_transactions WHERE guild_id=$1 AND player_id=$2 AND amount=1000`, guild, pid); err == nil {
		t.Fatal("a direct DELETE of a ledger row must be refused")
	}
	// ... so simulate a corrupted restore by disabling the guard triggers in one privileged transaction.
	tx, err := w.a.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE point_transactions DISABLE TRIGGER trg_point_transactions_no_delete`,
		`DELETE FROM point_transactions WHERE guild_id=` + strconv.FormatInt(guild, 10) + ` AND player_id=` + strconv.FormatInt(pid, 10) + ` AND amount=1000`,
		`ALTER TABLE point_transactions ENABLE TRIGGER trg_point_transactions_no_delete`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mism, _ = w.a.EconomyAccounts.Reconcile(ctx, scope, 10)
	kinds := map[string]bool{}
	for _, m := range mism {
		kinds[m.Kind] = true
	}
	if !kinds[repository.MismatchBalance] || !kinds[repository.MismatchChain] {
		t.Fatalf("a deleted ledger row breaks both the sum and the chain: %+v", mism)
	}
}

// TestEconomyHistoryScalesWithLedgerVolume loads a large ledger (PERF_TX, default 100000 rows for one
// player among 200 others) and checks that the balance and the first history page stay fast.
func TestEconomyHistoryScalesWithLedgerVolume(t *testing.T) {
	n := 100000
	if v, err := strconv.Atoi(os.Getenv("PERF_TX")); err == nil && v > 0 {
		n = v
	}
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Whale")
	guild := w.guildOf(w.a1)
	ctx := context.Background()
	// A chain of +1 credits for the whale (balance_after = g) and noise rows for 200 other players.
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO point_transactions(guild_id, player_id, amount, balance_after, reason_type, source_key, created_by)
SELECT $1::bigint, $2::bigint, 1, g, 'SYSTEM_REWARD', 'perf-' || g, 'SYSTEM' FROM generate_series(1, $3::bigint) g`, guild, pid, n); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_points(guild_id, player_id, balance, lifetime_points, season_points) VALUES($1,$2,$3,$3,$3) ON CONFLICT(guild_id, player_id) DO UPDATE SET balance=$3`, guild, pid, n); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO players(guild_id, dayz_player_id, display_name) SELECT $1::bigint, 'perf-' || $2::bigint::text || '-' || g, 'Noise ' || g FROM generate_series(1,200) g`, guild, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO point_transactions(guild_id, player_id, amount, balance_after, reason_type, source_key, created_by)
SELECT $1::bigint, p.id, 1, s, 'SYSTEM_REWARD', 'noise-' || p.id || '-' || s, 'SYSTEM' FROM players p, generate_series(1, $2::bigint) s WHERE p.guild_id=$1 AND p.display_name LIKE 'Noise %'`, guild, n/200); err != nil {
		t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO player_points(guild_id, player_id, balance, lifetime_points, season_points) SELECT $1::bigint, p.id, $2::bigint, $2::bigint, $2::bigint FROM players p WHERE p.guild_id=$1 AND p.display_name LIKE 'Noise %' ON CONFLICT DO NOTHING`, guild, n/200); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"point_transactions", "player_points"} {
		if _, err := w.a.DB.Pool.Exec(ctx, `ANALYZE `+tbl); err != nil {
			t.Fatal(err)
		}
	}
	timeIt := func(name string, fn func()) time.Duration {
		fn()
		s := time.Now()
		fn()
		d := time.Since(s)
		t.Logf("%-46s %v", name, d.Round(time.Microsecond))
		return d
	}
	d1 := timeIt("GET /me (balance)", func() { w.getJSON(w.eco(w.a1, "/me"), player) })
	d2 := timeIt("GET /me/transactions (first page of 25)", func() { w.getJSON(w.eco(w.a1, "/me/transactions"), player) })
	first := w.getJSON(w.eco(w.a1, "/me/transactions?limit=100"), player)
	cur := first["nextCursor"].(string)
	d3 := timeIt("GET /me/transactions (deep page via cursor)", func() {
		w.getJSON(w.eco(w.a1, "/me/transactions?limit=100&cursor="+url.QueryEscape(cur)), player)
	})
	d4 := timeIt("GET /me/transactions?type=ADMIN_DEBIT (rare type)", func() { w.getJSON(w.eco(w.a1, "/me/transactions?type=ADMIN_DEBIT"), player) })
	if got := w.balanceOf(w.a1, player); got != int64(n) {
		t.Fatalf("balance %d", got)
	}
	scope, _ := w.a.EconomyAccounts.Scope(ctx, w.a1.OrgID, w.a1.InstallationID)
	rs := time.Now()
	mism, err := w.a.EconomyAccounts.Reconcile(ctx, scope, 10)
	t.Logf("reconcile of the whole guild ledger (%d rows): %v, %d findings", n*2, time.Since(rs).Round(time.Millisecond), len(mism))
	if err != nil || len(mism) != 0 {
		t.Fatalf("reconcile: %v %v", mism, err)
	}
	for name, d := range map[string]time.Duration{"balance": d1, "first page": d2, "deep page": d3, "filtered page": d4} {
		if d > 500*time.Millisecond {
			t.Errorf("%s took %v: history must not scan the whole ledger", name, d)
		}
	}
}

func TestEconomyRateLimits(t *testing.T) {
	w := newFactionWorld(t)
	player := w.players[0]
	pid := w.linkPlayer(w.a1, player, "Limited")
	w.a.saasEconomyAdjustLimiter = newSaaSRateLimiter(time.Hour, 3)
	w.a.saasEconomyHistoryLimiter = newSaaSRateLimiter(time.Hour, 5)
	for i := 0; i < 3; i++ {
		w.expect(w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 1, "reason": "x"}), http.StatusOK, "within the limit")
	}
	if r := w.adjust(w.a1, "grant", pid, w.admin, map[string]any{"amount": 1, "reason": "x"}); r.Status != http.StatusTooManyRequests || r.errCode(t) != "RATE_LIMITED" {
		t.Fatalf("admin adjustments are rate limited: %d %s", r.Status, r.Body)
	}
	if got := w.balanceOf(w.a1, player); got != 3 {
		t.Fatalf("the limited request must not apply: %d", got)
	}
	for i := 0; i < 5; i++ {
		w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me/transactions"), player, nil), http.StatusOK, "history within the limit")
	}
	w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me/transactions"), player, nil), http.StatusTooManyRequests, "history is rate limited")
	// Ordinary balance reads are never limited.
	for i := 0; i < 60; i++ {
		w.expect(w.do(http.MethodGet, w.eco(w.a1, "/me"), player, nil), http.StatusOK, "balance read")
	}
}

// The ledger delete guard must not break the cascades that remove a whole guild.
func TestEconomyLedgerDeleteGuardAllowsCascades(t *testing.T) {
	w := newFactionWorld(t)
	pid := w.linkPlayer(w.a1, w.players[0], "Doomed")
	w.grant(w.a1, pid, 10)
	ctx := context.Background()
	guild := w.guildOf(w.a1)
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE player_id=$1`, pid); n != 1 {
		t.Fatalf("setup: %d", n)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `DELETE FROM players WHERE id=$1`, pid); err != nil {
		t.Fatalf("deleting a player cascades to the ledger and must still work: %v", err)
	}
	if n := w.count(`SELECT COUNT(*) FROM point_transactions WHERE guild_id=$1`, guild); n != 0 {
		t.Fatalf("cascade removed the rows: %d", n)
	}
}
