//go:build integration

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestServerRankedSeasonOwnerStartAndReset(t *testing.T) {
	w := newClientAdminWorld(t)
	w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
	thresholds := ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}
	start := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: thresholds}
	path := w.path("/ranked/server-season")
	if rr := w.call(w.a.handleGetServerRankedSeason, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"season":null`) {
		t.Fatalf("expected empty owner season state: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, "other-user", start, nil); rr.Code == http.StatusOK {
		t.Fatal("non-owner started ranked season")
	}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK {
		t.Fatalf("owner start: %d %s", rr.Code, rr.Body.String())
	}
	// The request above had no sameVictimCooldownMinutes (an older website): the season keeps five minutes.
	if rr := w.call(w.a.handleGetServerRankedSeason, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"rpPerKill":100`) || !strings.Contains(rr.Body.String(), `"sameVictimCooldownMinutes":5`) {
		t.Fatalf("owner read active rules: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate start: %d %s", rr.Code, rr.Body.String())
	}
	resetPath := w.path("/ranked/server-season/reset")
	if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, resetPath, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusConflict {
		t.Fatalf("reset without confirmation: %d %s", rr.Code, rr.Body.String())
	}
	start.Confirm = "RESET SERVER RANKED"
	// A wait that is not a whole number from 0 to 120 is refused and leaves the season as it was.
	for _, bad := range []string{"-1", "121", "5.5", `"5"`} {
		start.SameVictimCooldownMinutes = json.RawMessage(bad)
		if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, resetPath, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "whole number of minutes from 0 to 120") {
			t.Fatalf("wait %s: expected a 400 naming the range, got %d %s", bad, rr.Code, rr.Body.String())
		}
	}
	// A reset can also choose a new wait (it can be changed without one too: see the test below).
	start.SameVictimCooldownMinutes = json.RawMessage("30")
	if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, resetPath, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"sameVictimCooldownMinutes":30`) {
		t.Fatalf("owner reset: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.call(w.a.handleGetServerRankedSeason, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"sameVictimCooldownMinutes":30`) {
		t.Fatalf("owner read reset rules: %d %s", rr.Code, rr.Body.String())
	}
	var archivedWait, activeWait int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT MAX(same_victim_cooldown_minutes) FILTER (WHERE status='ARCHIVED'),MAX(same_victim_cooldown_minutes) FILTER (WHERE status='ACTIVE') FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&archivedWait, &activeWait); err != nil || archivedWait != 5 || activeWait != 30 {
		t.Fatalf("archived season keeps its own wait: archived=%d active=%d %v", archivedWait, activeWait, err)
	}
	var archived, active int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE status='ARCHIVED'),count(*) FILTER (WHERE status='ACTIVE') FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&archived, &active); err != nil || archived != 1 || active != 1 {
		t.Fatalf("expected one preserved archive and one active season: %d %d %v", archived, active, err)
	}
}

// TestServerRankedSeasonStartsOnRealServerStatuses reproduces the production
// 400 on POST .../admin/ranked/server-season: game_servers.status is a display
// label that production writes as CONNECTED (/server select, reconnect) or
// ONLINE/OFFLINE (SaaS Nitrado setup) and never 'ACTIVE', yet the start query
// required status='ACTIVE', so every real owner request was rejected as
// "server unavailable". Eligibility is the active flag, the owning guild and a
// console platform - exactly what the ADM workers use.
func TestServerRankedSeasonStartsOnRealServerStatuses(t *testing.T) {
	for _, status := range []string{"CONNECTED", "ONLINE", "OFFLINE"} {
		t.Run(status, func(t *testing.T) {
			w := newClientAdminWorld(t)
			w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
			if _, err := w.a.DB.Pool.Exec(context.Background(), `UPDATE game_servers SET status=$2, active=TRUE WHERE id=$1`, w.serverID, status); err != nil {
				t.Fatal(err)
			}
			// The owner's production rules, unchanged.
			req := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{3000, 6000, 9000, 12000, 15000, 18000, 25000}}
			rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, w.path("/ranked/server-season"), w.f.OwnerDiscordID, req, nil)
			if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"rpPerKill":100`) || !strings.Contains(rr.Body.String(), `"thresholds":[3000,6000,9000,12000,15000,18000,25000]`) {
				t.Fatalf("status %s: owner start must succeed, got %d %s", status, rr.Code, rr.Body.String())
			}
		})
	}
}

// Ineligible servers still fail closed, with an explanation the owner can act on.
func TestServerRankedSeasonRejectsIneligibleServerWithReason(t *testing.T) {
	cases := map[string]string{
		"inactive": `UPDATE game_servers SET active=FALSE, status='DISCONNECTED' WHERE id=$1`,
		"pc":       `UPDATE game_servers SET platform='PC' WHERE id=$1`,
	}
	for name, update := range cases {
		t.Run(name, func(t *testing.T) {
			w := newClientAdminWorld(t)
			w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
			if _, err := w.a.DB.Pool.Exec(context.Background(), update, w.serverID); err != nil {
				t.Fatal(err)
			}
			req := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{3000, 6000, 9000, 12000, 15000, 18000, 25000}}
			rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, w.path("/ranked/server-season"), w.f.OwnerDiscordID, req, nil)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "RANKED_SERVER_INELIGIBLE") {
				t.Fatalf("%s: expected a 400 RANKED_SERVER_INELIGIBLE, got %d %s", name, rr.Code, rr.Body.String())
			}
			var n int
			_ = w.a.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&n)
			if n != 0 {
				t.Fatalf("%s: no season may be created", name)
			}
		})
	}
}

// The wait, and only the wait, changes on the active season without a reset.
func TestServerRankedSeasonWaitChangesMidSeason(t *testing.T) {
	w := newClientAdminWorld(t)
	w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
	ctx := context.Background()
	path := w.path("/ranked/server-season")
	change := func(actor, body string) (int, string) {
		t.Helper()
		rr := w.call(w.a.handleChangeServerRankedSeasonWait, http.MethodPatch, path, actor, json.RawMessage(body), nil)
		return rr.Code, rr.Body.String()
	}
	// No active season: 409 in the house style.
	if code, body := change(w.f.OwnerDiscordID, `{"sameVictimCooldownMinutes":2}`); code != http.StatusConflict || !strings.Contains(body, "RANKED_NO_ACTIVE_SEASON") {
		t.Fatalf("no season: %d %s", code, body)
	}
	start := serverRankedSeasonRequest{RPPerKill: 100, Thresholds: ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}, SameVictimCooldownMinutes: json.RawMessage("30")}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rr.Code, rr.Body.String())
	}
	if code, body := change("other-user", `{"sameVictimCooldownMinutes":2}`); code == http.StatusOK {
		t.Fatalf("non-owner changed the wait: %s", body)
	}
	for _, bad := range []string{`{}`, `{"sameVictimCooldownMinutes":null}`, `{"sameVictimCooldownMinutes":-1}`, `{"sameVictimCooldownMinutes":121}`, `{"sameVictimCooldownMinutes":5.5}`, `{"sameVictimCooldownMinutes":"5"}`} {
		if code, body := change(w.f.OwnerDiscordID, bad); code != http.StatusBadRequest || !strings.Contains(body, "whole number of minutes from 0 to 120") {
			t.Fatalf("%s: expected a 400 naming the range, got %d %s", bad, code, body)
		}
	}
	// The frozen rules are refused, not silently ignored.
	for _, frozen := range []string{`{"sameVictimCooldownMinutes":2,"rpPerKill":500}`, `{"sameVictimCooldownMinutes":2,"thresholds":[1,2,3,4,5,6,7]}`} {
		if code, body := change(w.f.OwnerDiscordID, frozen); code != http.StatusBadRequest || !strings.Contains(body, "frozen") {
			t.Fatalf("%s: expected a 400 about frozen rules, got %d %s", frozen, code, body)
		}
	}
	var seasonID int64
	state := func() (wait int, rp int64, history int) {
		t.Helper()
		if err := w.a.DB.Pool.QueryRow(ctx, `SELECT s.id,s.same_victim_cooldown_minutes,s.rp_per_kill,(SELECT count(*) FROM ranked_season_cooldown_changes c WHERE c.season_id=s.id)
FROM ranked_seasons s WHERE s.server_id=$1 AND s.status='ACTIVE'`, w.serverID).Scan(&seasonID, &wait, &rp, &history); err != nil {
			t.Fatal(err)
		}
		return
	}
	if wait, rp, history := state(); wait != 30 || rp != 100 || history != 1 {
		t.Fatalf("refused requests must change nothing: wait=%d rp=%d history=%d", wait, rp, history)
	}
	firstSeason := seasonID
	code, body := change(w.f.OwnerDiscordID, `{"sameVictimCooldownMinutes":2}`)
	if code != http.StatusOK || !strings.Contains(body, `"sameVictimCooldownMinutes":2`) || !strings.Contains(body, `"rpPerKill":100`) || !strings.Contains(body, `"thresholds":[100,300,600,1000,1500,2100,2800]`) || !strings.Contains(body, `"status":"ACTIVE"`) {
		t.Fatalf("owner change: %d %s", code, body)
	}
	if wait, rp, history := state(); wait != 2 || rp != 100 || history != 2 || seasonID != firstSeason {
		t.Fatalf("after change: wait=%d rp=%d history=%d season=%d (was %d)", wait, rp, history, seasonID, firstSeason)
	}
	var by string
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT changed_by FROM ranked_season_cooldown_changes WHERE season_id=$1 ORDER BY effective_from DESC,id DESC LIMIT 1`, seasonID).Scan(&by); err != nil || by != w.f.OwnerDiscordID {
		t.Fatalf("change must record who made it: %q %v", by, err)
	}
	// Saving the same value again is a success and adds no history.
	if code, body := change(w.f.OwnerDiscordID, `{"sameVictimCooldownMinutes":2}`); code != http.StatusOK {
		t.Fatalf("same value: %d %s", code, body)
	}
	if _, _, history := state(); history != 2 {
		t.Fatalf("an unchanged save added history: %d", history)
	}
	// One audit entry for the one real change (who and when, with the value before and after).
	entries, err := w.a.AdminAudit.List(ctx, w.f.OrgID, &w.f.InstallationID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	audited := 0
	for _, e := range entries {
		if e.Action == "SERVER_RANKED_WAIT_CHANGE" {
			audited++
			if e.ActorDiscordID != w.f.OwnerDiscordID || !strings.Contains(strings.ReplaceAll(string(e.BeforeState), " ", ""), `"sameVictimCooldownMinutes":30`) || !strings.Contains(strings.ReplaceAll(string(e.AfterState), " ", ""), `"sameVictimCooldownMinutes":2`) {
				t.Fatalf("audit entry: %+v", e)
			}
		}
	}
	if audited != 1 {
		t.Fatalf("expected one wait-change audit entry, got %d", audited)
	}
	if rr := w.call(w.a.handleGetServerRankedSeason, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"sameVictimCooldownMinutes":2`) {
		t.Fatalf("read after change: %d %s", rr.Code, rr.Body.String())
	}
}
