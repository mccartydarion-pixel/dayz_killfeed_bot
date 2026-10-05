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
	// The wait changes only through a reset, like RP per kill.
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
