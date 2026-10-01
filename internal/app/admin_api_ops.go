// Package app: Owner Hub operations console (docs/ADMIN_API.md "Operations").
//
// The Discord /admin command's diagnostics and maintenance actions, on the web for the platform
// owner: GET /api/admin/ops/status returns what /admin status|diagnostics show, and the two
// maintenance actions (leaderboard refresh, ADM source scan) are audited owner writes.
package app

import (
	"context"
	"net/http"
	"time"
)

const admSourceScanTimeout = 90 * time.Second

// handleAdminOpsStatus is GET /api/admin/ops/status: runtime snapshot, component health,
// workers and the link / presence / pipeline diagnostics the Discord /admin command shows.
// Everything comes from admin.Service, which already sanitizes for guild admins; the admin
// secret guard still applies on the way out.
func (a *App) handleAdminOpsStatus(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	if a.AdminService == nil {
		writeSaaSError(w, codeInternalError, "operations service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out := a.AdminService.Status(ctx)
	out["generatedAt"] = time.Now().UTC().Format(time.RFC3339)
	out["workerManager"] = a.workerManagerSnapshot()
	a.writeAdminJSON(w, http.StatusOK, out)
}

// workerManagerSnapshot lists the ADM workers the WorkerManager currently runs, by game server.
func (a *App) workerManagerSnapshot() map[string]any {
	out := map[string]any{"initialized": a.WorkerManager != nil}
	if a.WorkerManager == nil || a.Servers == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := a.Servers.ListActive(ctx)
	if err != nil {
		out["error"] = "could not list active servers"
		return out
	}
	servers := make([]map[string]any, 0, len(rows))
	running := 0
	for _, s := range rows {
		isRunning := a.WorkerManager.Running(s.ID)
		if isRunning {
			running++
		}
		servers = append(servers, map[string]any{"serverId": s.ID, "guildId": s.GuildID, "displayName": s.DisplayName, "platform": s.Platform, "status": s.Status, "running": isRunning})
	}
	out["servers"] = servers
	out["running"] = running
	out["active"] = len(rows)
	return out
}

// handleAdminOpsLeaderboardRefresh is POST /api/admin/ops/leaderboard-refresh: the same manual
// refresh the Discord /admin leaderboard-refresh subcommand triggers.
func (a *App) handleAdminOpsLeaderboardRefresh(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	if a.AdminService == nil {
		writeSaaSError(w, codeInternalError, "operations service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	if err := a.AdminService.RefreshLeaderboard(ctx); err != nil {
		a.ownerAudit(ctx, admin, "ops.leaderboard_refreshed", "platform", 0, nil, req.Reason, "FAILED", nil, map[string]string{"error": err.Error()})
		ownerFailed(w, "refresh leaderboard", err)
		return
	}
	a.ownerAudit(ctx, admin, "ops.leaderboard_refreshed", "platform", 0, nil, req.Reason, "OK", nil, map[string]any{"durationMs": time.Since(started).Milliseconds()})
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"refreshed": true, "durationMs": time.Since(started).Milliseconds()})
}

// handleAdminOpsADMSourceScan is POST /api/admin/ops/adm-source-scan: the slow (~30s) live scan
// of Nitrado ADM candidates for the actively written source. Audited; the result is returned
// as-is so the owner sees what the Discord command would print.
func (a *App) handleAdminOpsADMSourceScan(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	if a.AdminService == nil {
		writeSaaSError(w, codeInternalError, "operations service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), admSourceScanTimeout)
	defer cancel()
	started := time.Now()
	result, err := a.AdminService.RunADMSourceScan(ctx)
	if err != nil {
		a.ownerAudit(ctx, admin, "ops.adm_source_scanned", "platform", 0, nil, req.Reason, "FAILED", nil, map[string]string{"error": err.Error()})
		ownerFailed(w, "run ADM source scan", err)
		return
	}
	a.ownerAudit(ctx, admin, "ops.adm_source_scanned", "platform", 0, nil, req.Reason, "OK", nil, map[string]any{"durationMs": time.Since(started).Milliseconds()})
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"result": result, "durationMs": time.Since(started).Milliseconds()})
}

func (a *App) registerOpsAPI() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/admin/ops/status", a.adminRoute(a.handleAdminOpsStatus))
	h("POST /api/admin/ops/leaderboard-refresh", a.adminRoute(a.handleAdminOpsLeaderboardRefresh))
	h("POST /api/admin/ops/adm-source-scan", a.adminRoute(a.handleAdminOpsADMSourceScan))
}
