package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/heatmap"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// Champion Phase 5 (docs/HEATMAPS.md): PvP kill/death, player-activity, and zone-intrusion
// heatmap datasets aggregated from Phase 3/4's already-persisted data. This handler is
// deliberately thin (task section 31: "auth, capability check, parse params, call service, write
// response") - every validation rule, the cache, and the SQL aggregation itself live in
// internal/heatmap and internal/repository.

func (a *App) registerHeatmapRoutes(base string) {
	a.HTTPServer.Handle("GET "+base+"/heatmap", a.handleHeatmap)
}

// defaultHeatmapResolution applies when the caller omits resolution - a predictable, documented
// default rather than an error, since a first-time caller exploring the endpoint shouldn't need
// every parameter. heatmapCacheTTL is the service-level cache lifetime (task section 21: "30-60
// seconds, do not use hours-long cache") - wired into the Service once in App.New.
const (
	defaultHeatmapResolution = heatmap.DefaultResolution
	heatmapCacheTTL          = 45 * time.Second
)

type heatmapCellDTO struct {
	CellX     int64   `json:"cellX"`
	CellZ     int64   `json:"cellZ"`
	CenterX   float64 `json:"centerX"`
	CenterZ   float64 `json:"centerZ"`
	Count     int64   `json:"count"`
	Intensity float64 `json:"intensity"`
}

type heatmapResponseDTO struct {
	Type        string           `json:"type"`
	From        string           `json:"from"`
	To          string           `json:"to"`
	Resolution  int              `json:"resolution"`
	TotalEvents int64            `json:"totalEvents"`
	Cells       []heatmapCellDTO `json:"cells"`
}

func toHeatmapResponseDTO(r *heatmap.Result) heatmapResponseDTO {
	out := heatmapResponseDTO{
		Type: string(r.Type), From: r.From.UTC().Format(time.RFC3339), To: r.To.UTC().Format(time.RFC3339),
		Resolution: r.Resolution, TotalEvents: r.TotalEvents, Cells: make([]heatmapCellDTO, 0, len(r.Cells)),
	}
	for _, c := range r.Cells {
		out.Cells = append(out.Cells, heatmapCellDTO{
			CellX: c.CellX, CellZ: c.CellZ, CenterX: c.CenterX, CenterZ: c.CenterZ, Count: c.Count, Intensity: c.Intensity,
		})
	}
	return out
}

// handleHeatmap is GET .../admin/heatmap (HEATMAP_VIEW). Response is always an aggregate grid -
// never a player ID, gamertag, Discord ID, or faction member identity (task section 15).
func (a *App) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapHeatmapView)
	if !ok {
		return
	}
	if a.Heatmap == nil {
		writeSaaSError(w, codeInternalError, "heatmap service unavailable")
		return
	}
	q := r.URL.Query()

	heatmapType := heatmap.Type(strings.ToUpper(strings.TrimSpace(q.Get("type"))))

	resolution := defaultHeatmapResolution
	if raw := strings.TrimSpace(q.Get("resolution")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "resolution must be an integer")
			return
		}
		resolution = v
	}

	to := time.Now().UTC()
	from := to.Add(-heatmap.DefaultWindow)
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "to must be an RFC3339 timestamp")
			return
		}
		to = t.UTC()
		from = to.Add(-heatmap.DefaultWindow)
	}
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "from must be an RFC3339 timestamp")
			return
		}
		from = t.UTC()
	}

	var zoneID *int64
	if raw := strings.TrimSpace(q.Get("zoneId")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeSaaSError(w, codeInvalidRequest, "zoneId must be an integer")
			return
		}
		// Zone tenant safety (task section 27): a zoneId from another installation must never be
		// usable here, exactly like every zone-scoped Client Admin route already enforces.
		if a.Zones == nil {
			writeSaaSError(w, codeInternalError, "zones unavailable")
			return
		}
		zoneCtx, zoneCancel := context.WithTimeout(r.Context(), adminTimeout)
		_, err = a.Zones.GetZone(zoneCtx, ac.scope.InstallationID, v)
		zoneCancel()
		if err != nil {
			writeSaaSError(w, codeNotFound, "zone not found")
			return
		}
		zoneID = &v
	}

	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}

	req := heatmap.Request{
		GuildID: ac.scope.GuildID, InstallationID: ac.scope.InstallationID,
		Type: heatmapType, From: from, To: to, Resolution: resolution, ZoneID: zoneID,
	}
	if heatmapType != heatmap.TypeZoneIntrusions {
		if ac.scope.ServerID == nil {
			writeSaaSError(w, codeInvalidRequest, "no DayZ server selected for this installation")
			return
		}
		req.ServerID = *ac.scope.ServerID
	}

	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	result, err := a.Heatmap.Query(ctx, req)
	if err != nil {
		var verr *heatmap.ValidationError
		if errors.As(err, &verr) {
			writeSaaSError(w, codeInvalidRequest, verr.Message)
			return
		}
		slog.Warn("component=saas_api", "msg", "heatmap query failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not compute heatmap")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toHeatmapResponseDTO(result))
}
