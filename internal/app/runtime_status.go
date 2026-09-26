package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RuntimeStatusResponse is the documented shape of GET /api/runtime/status.
// See docs/runtime-status-api.md for the website consumer contract. Every
// field is read from already-running in-memory state (the same source the
// Discord /admin diagnostics command uses) - this handler never talks to
// Nitrado or blocks on a second log reader.
type RuntimeStatusResponse struct {
	OK                     bool                  `json:"ok"`
	GuildID                string                `json:"guildId"`
	ServerID               int64                 `json:"serverId,omitempty"`
	ServerName             string                `json:"serverName,omitempty"`
	PlayersOnline          *int                  `json:"playersOnline"`
	TrackerCount           *int                  `json:"trackerCount"`
	WorkerRunning          bool                  `json:"workerRunning"`
	LogSource              *string               `json:"logSource"`
	SourceStatus           *string               `json:"sourceStatus"`
	PipelineClassification *string               `json:"pipelineClassification"`
	LastLogReadAt          *string               `json:"lastLogReadAt"`
	LastParsedEventAt      *string               `json:"lastParsedEventAt"`
	LastPersistedEventAt   *string               `json:"lastPersistedEventAt"`
	Servers                []RuntimeServerStatus `json:"servers,omitempty"`
	// Health is the evidence-based per-installation state (HEALTHY,
	// DEGRADED, UNAVAILABLE, UNKNOWN) with the reasons behind it.
	Health *RuntimeHealth `json:"health,omitempty"`
}

// RuntimeServerStatus is one entry in the "pick a server" fallback, returned
// when the guild has no single selected public server.
type RuntimeServerStatus struct {
	ServerID   int64  `json:"serverId"`
	ServerName string `json:"serverName"`
	Active     bool   `json:"active"`
}

func writeRuntimeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func nullableTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// validBearerToken reports whether the Authorization header carries the
// expected "Bearer <secret>" value, compared in constant time.
func validBearerToken(header, secret string) bool {
	const prefix = "Bearer "
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimPrefix(header, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// registerRuntimeStatusAPI wires GET /api/runtime/status onto the app's
// existing HTTP server (no second listener/port). Called once from New()
// after every dependency the handler reads is already assigned on a.
func (a *App) registerRuntimeStatusAPI() {
	if a.HTTPServer == nil {
		return
	}
	a.HTTPServer.Handle("/api/runtime/status", a.runtimeStatusHandler)
}

// runtimeStatusHandler serves the website's read-only runtime status API.
// It only ever reads state this process already holds in memory or in
// PostgreSQL - it never polls Nitrado and never accepts writes.
func (a *App) runtimeStatusHandler(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("component=runtime_api", "msg", "panic handling runtime status request", "err", fmt.Sprintf("%v", rec))
			writeRuntimeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal_error"})
		}
	}()

	if r.Method != http.MethodGet {
		writeRuntimeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}

	if a.Config == nil || a.Config.WebsiteAPISecret == "" || !validBearerToken(r.Header.Get("Authorization"), a.Config.WebsiteAPISecret) {
		writeRuntimeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}

	requestedGuildID := strings.TrimSpace(r.URL.Query().Get("discord_guild_id"))
	if requestedGuildID == "" {
		requestedGuildID = a.Config.DiscordGuildID
	}
	// This process (one Railway service) owns exactly one Discord guild;
	// any other requested guild simply is not served here.
	if requestedGuildID == "" || a.Config == nil || requestedGuildID != a.Config.DiscordGuildID {
		writeRuntimeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown_guild"})
		return
	}

	if a.Guilds == nil || a.Servers == nil {
		writeRuntimeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "database_unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	guild, guildRowID, err := a.Guilds.GetGuild(ctx, requestedGuildID)
	if err != nil {
		slog.Warn("component=runtime_api", "msg", "guild lookup failed", "err", err.Error())
		writeRuntimeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal_error"})
		return
	}
	if guild == nil {
		writeRuntimeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown_guild"})
		return
	}

	resp := RuntimeStatusResponse{OK: true, GuildID: requestedGuildID}

	serverID := guild.SelectedPublicServerID
	if raw := strings.TrimSpace(r.URL.Query().Get("server_id")); raw != "" {
		if parsed, parseErr := strconv.ParseInt(raw, 10, 64); parseErr == nil {
			serverID = parsed
		}
	}

	if serverID == 0 {
		// No single server selected for this guild: hand back the small set of
		// active servers instead of guessing which one the caller wants.
		activeServers, listErr := a.Servers.ListActiveByGuild(ctx, guildRowID)
		if listErr != nil {
			slog.Warn("component=runtime_api", "msg", "list active servers failed", "err", listErr.Error())
			writeRuntimeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal_error"})
			return
		}
		for _, row := range activeServers {
			resp.Servers = append(resp.Servers, RuntimeServerStatus{ServerID: row.ID, ServerName: row.DisplayName, Active: row.Active})
		}
		writeRuntimeJSON(w, http.StatusOK, resp)
		return
	}

	resp.ServerID = serverID
	binding := "NOT_FOUND"
	if row, rowErr := a.Servers.GetByID(ctx, serverID); rowErr == nil && row != nil {
		if row.GuildID != guildRowID {
			// Tenant isolation: another guild's server is never described here.
			writeRuntimeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown_server"})
			return
		}
		resp.ServerName = row.DisplayName
		binding = "INACTIVE"
		if row.Active && !strings.EqualFold(row.Status, "DISCONNECTED") {
			binding = "BOUND"
		}
	}
	health := a.runtimeHealth(serverID, binding)
	resp.Health = &health

	if presence, found := a.livePresenceSnapshot(serverID); found {
		online := presence.OnlineCount
		tracked := presence.TrackedEntries
		resp.PlayersOnline = &online
		resp.TrackerCount = &tracked
	}

	if pipeline, found := a.livePipelineSnapshot(serverID); found {
		resp.WorkerRunning = pipeline.WorkerRunning
		resp.LogSource = nullableString(pipeline.SelectedADM)
		resp.LastLogReadAt = nullableTime(pipeline.LastDownloadSuccess)
		resp.LastParsedEventAt = nullableTime(pipeline.LastParsedEventAt)
		if pipeline.LastPersistenceResult == "SUCCESS" {
			resp.LastPersistedEventAt = nullableTime(pipeline.LastPersistenceAt)
		}
		classification := pipeline.Classification()
		resp.PipelineClassification = nullableString(classification)
		if presence, found := a.livePresenceSnapshot(serverID); found && !presence.Presence.Known {
			// Unknown presence is never reported as a count (least of all 0).
			resp.PlayersOnline = nil
		}

		status := "STOPPED"
		switch {
		case pipeline.SelectedADM == "":
			status = "NO_SOURCE_SELECTED"
		case pipeline.WorkerRunning:
			status = "ACTIVE"
		}
		resp.SourceStatus = &status
	}

	writeRuntimeJSON(w, http.StatusOK, resp)
}

// runtimeHealth gathers in-memory evidence for one server and evaluates it.
func (a *App) runtimeHealth(serverID int64, binding string) RuntimeHealth {
	in := healthInputs{Binding: binding, Deliveries: discord.Deliveries.Snapshot()}
	a.presenceMu.Lock()
	engine := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if engine != nil && engine.Diagnostics() != nil {
		in.WorkerFound = true
		in.Pipeline = engine.Diagnostics().Snapshot()
		in.Presence = engine.PresenceSnapshot()
		if pending, ok := engine.PendingEvents(); ok {
			in.Pending = &pending
		}
	}
	if a.onlineCounter != nil {
		in.Counter = a.onlineCounter.Health()
		in.CounterOwned = a.ownsPublicCounter(serverID)
	}
	return evaluateRuntimeHealth(in)
}
