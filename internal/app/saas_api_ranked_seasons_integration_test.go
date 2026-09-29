//go:build integration

package app

import (
	"context"
	"net/http"
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
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, "other-user", start, nil); rr.Code == http.StatusOK {
		t.Fatal("non-owner started ranked season")
	}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK {
		t.Fatalf("owner start: %d %s", rr.Code, rr.Body.String())
	}
	if rr := w.call(w.a.handleStartServerRankedSeason, http.MethodPost, path, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate start: %d %s", rr.Code, rr.Body.String())
	}
	resetPath := w.path("/ranked/server-season/reset")
	if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, resetPath, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusConflict {
		t.Fatalf("reset without confirmation: %d %s", rr.Code, rr.Body.String())
	}
	start.Confirm = "RESET SERVER RANKED"
	if rr := w.call(w.a.handleResetServerRankedSeason, http.MethodPost, resetPath, w.f.OwnerDiscordID, start, nil); rr.Code != http.StatusOK {
		t.Fatalf("owner reset: %d %s", rr.Code, rr.Body.String())
	}
	var archived, active int
	if err := w.a.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE status='ARCHIVED'),count(*) FILTER (WHERE status='ACTIVE') FROM ranked_seasons WHERE server_id=$1`, w.serverID).Scan(&archived, &active); err != nil || archived != 1 || active != 1 {
		t.Fatalf("expected one preserved archive and one active season: %d %d %v", archived, active, err)
	}
}
