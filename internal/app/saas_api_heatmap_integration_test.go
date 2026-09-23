//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Phase 5 (docs/HEATMAPS.md) API integration tests: HEATMAP_VIEW capability gating,
// empty-data 200, resolution/range validation, and zone tenant safety. Reuses clientAdminWorld
// exactly like the Phase 4 zone API tests - no second harness.

// hmOwnerID resolves the fixture organization owner's app_users.id (installationFixture only
// carries the Discord snowflake) for direct-repository test setup calls that need a real
// created_by_user_id FK.
func hmOwnerID(t *testing.T, w *clientAdminWorld) int64 {
	t.Helper()
	user, err := w.a.SaaSUsers.GetByDiscordID(context.Background(), w.f.OwnerDiscordID)
	if err != nil || user == nil {
		t.Fatalf("could not resolve owner app_user: %v", err)
	}
	return user.ID
}

func newHeatmapWorld(t *testing.T) *clientAdminWorld {
	t.Helper()
	w := newClientAdminWorld(t)
	w.a.Zones = repository.NewZoneRepository(w.a.DB.Pool)
	w.a.Heatmap = heatmap.NewService(repository.NewHeatmapRepository(w.a.DB.Pool), heatmap.NewCache(time.Minute), heatmap.NewMetrics())
	return w
}

func TestHeatmapRequiresCapability(t *testing.T) {
	w := newHeatmapWorld(t)
	unmapped := syncUser(t, w.a, fmt.Sprintf("hm-unmapped-%d", time.Now().UnixNano()), "Unmapped")
	rr := w.call(w.a.handleHeatmap, http.MethodGet, w.path("/heatmap")+"?type=PVP_KILLS", unmapped.DiscordUserID, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an actor with no mapped level, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = w.call(w.a.handleHeatmap, http.MethodGet, w.path("/heatmap")+"?type=PVP_KILLS", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the organization owner, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHeatmapEmptyDataReturns200(t *testing.T) {
	w := newHeatmapWorld(t)
	rr := w.call(w.a.handleHeatmap, http.MethodGet, w.path("/heatmap")+"?type=PVP_KILLS", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for empty data, got %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[heatmapResponseDTO](t, rr)
	if body.TotalEvents != 0 || len(body.Cells) != 0 {
		t.Fatalf("expected an empty-but-valid response, got %+v", body)
	}
}

func TestHeatmapRejectsInvalidResolution(t *testing.T) {
	w := newHeatmapWorld(t)
	rr := w.call(w.a.handleHeatmap, http.MethodGet, w.path("/heatmap")+"?type=PVP_KILLS&resolution=37", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported resolution, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHeatmapRejectsInvalidType(t *testing.T) {
	w := newHeatmapWorld(t)
	rr := w.call(w.a.handleHeatmap, http.MethodGet, w.path("/heatmap")+"?type=NOT_A_TYPE", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported type, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHeatmapRejectsRangeOverMaximum(t *testing.T) {
	w := newHeatmapWorld(t)
	to := time.Now().UTC()
	from := to.Add(-31 * 24 * time.Hour)
	url := fmt.Sprintf("%s?type=PVP_KILLS&from=%s&to=%s", w.path("/heatmap"), from.Format(time.RFC3339), to.Format(time.RFC3339))
	rr := w.call(w.a.handleHeatmap, http.MethodGet, url, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a range over 30 days, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHeatmapZoneFilterRejectsCrossInstallationZone(t *testing.T) {
	wA := newHeatmapWorld(t)
	wB := newHeatmapWorld(t)
	zone, err := wB.a.Zones.CreateZone(context.Background(), wB.f.InstallationID, wB.guildID, wB.serverID, "B Zone", repository.ZoneTypeRestricted, 0, 0, 10, nil, 300, hmOwnerID(t, wB))
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s?type=ZONE_INTRUSIONS&zoneId=%d", wA.path("/heatmap"), zone.ID)
	rr := wA.call(wA.a.handleHeatmap, http.MethodGet, url, wA.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a zoneId belonging to another installation, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHeatmapZoneFilterAcceptsOwnZone(t *testing.T) {
	w := newHeatmapWorld(t)
	zone, err := w.a.Zones.CreateZone(context.Background(), w.f.InstallationID, w.guildID, w.serverID, "Zone", repository.ZoneTypeRestricted, 0, 0, 10, nil, 300, hmOwnerID(t, w))
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s?type=ZONE_INTRUSIONS&zoneId=%d", w.path("/heatmap"), zone.ID)
	rr := w.call(w.a.handleHeatmap, http.MethodGet, url, w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for the caller's own zone, got %d: %s", rr.Code, rr.Body.String())
	}
}
