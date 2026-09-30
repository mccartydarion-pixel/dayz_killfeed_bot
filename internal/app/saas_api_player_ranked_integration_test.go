//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestPlayerRankedProgressIsVerifiedAndServerScoped(t *testing.T) {
	w := newFactionWorld(t)
	w.a.Ranked = repository.NewRankedRepository(w.a.DB.Pool)
	actor := w.players[0]
	player := w.linkPlayer(w.a1, actor, "RankedSurvivor")
	other := w.secondInstallation(w.a1)
	w.insertKill(w.a1, player, time.Now().UTC().Add(-time.Minute), false) // observed association, before season
	path := fmt.Sprintf("/api/saas/player/servers/%d/ranked", w.a1.InstallationID)
	before := w.getJSON(path, actor)
	if before["status"] != "NOT_STARTED" || before["progress"] != nil {
		t.Fatalf("unexpected pre-season progress: %v", before)
	}
	ctx := context.Background()
	guild, server := w.gameContext(w.a1)
	_, err := w.a.Ranked.StartServerSeason(ctx, guild, server, 100, ranked.Thresholds{100, 300, 600, 1000, 1500, 2100, 2800}, false, time.Now().UTC())
	if err != nil { t.Fatal(err) }
	zero := w.getJSON(path, actor)
	zp := zero["progress"].(map[string]any)
	if zero["status"] != "ACTIVE" || zp["tier"] != "UNRANKED" || zp["rp"].(float64) != 0 || zp["remainingRp"].(float64) != 100 || zp["serverPosition"] != nil {
		t.Fatalf("incorrect zero-RP progress: %v", zero)
	}
	w.insertKill(w.a1, player, time.Now().UTC(), false)
	var killID int64
	if err := w.a.DB.Pool.QueryRow(ctx, `SELECT id FROM kills WHERE server_id=$1 AND killer_player_id=$2 ORDER BY id DESC LIMIT 1`, server, player).Scan(&killID); err != nil { t.Fatal(err) }
	if _, err := w.a.Ranked.AwardActiveServerKill(ctx, server, killID); err != nil { t.Fatal(err) }
	after := w.getJSON(path, actor)
	p := after["progress"].(map[string]any)
	if p["tier"] != "ROOKIE" || p["rp"].(float64) != 100 || p["remainingRp"].(float64) != 200 || p["tierStartRp"].(float64) != 100 || p["nextTierRp"].(float64) != 300 || p["serverPosition"].(float64) != 1 {
		t.Fatalf("incorrect awarded progress: %v", after)
	}
	otherPath := fmt.Sprintf("/api/saas/player/servers/%d/ranked", other.InstallationID)
	if resp := w.do(http.MethodGet, otherPath, actor, nil); resp.Status != http.StatusNotFound {
		t.Fatalf("unobserved server leaked progress: %d %s", resp.Status, resp.Body)
	}
	if resp := w.do(http.MethodGet, path, w.players[1], nil); resp.Status == http.StatusOK {
		t.Fatalf("another user read progress: %d %s", resp.Status, resp.Body)
	}
}
