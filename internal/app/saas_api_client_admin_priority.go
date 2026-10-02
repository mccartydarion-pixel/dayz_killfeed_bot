package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

// --- priority list (Nitrado's "Prioritized players": who skips the queue when the server is full) ---
//
// Nitrado holds the list as one setting and is the only source of truth: Champion keeps no copy,
// so a name added in Nitrado's own panel shows here and the other way round. Every change reads
// the current list, changes one entry and writes the list back (internal/nitrado/priority.go).
// Nitrado applies the setting when the server next restarts.

type priorityListDTO struct {
	// Supported is false when the service has no priority setting at all; the website then explains
	// instead of offering controls.
	Supported bool     `json:"supported"`
	Items     []string `json:"items"`
	Limit     int      `json:"limit"`
}

type priorityRequest struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// priorityIndex is the position of name in list, ignoring case (gamertags are not case-sensitive), or -1.
func priorityIndex(list []string, name string) int {
	for i, entry := range list {
		if strings.EqualFold(entry, name) {
			return i
		}
	}
	return -1
}

// readPriorityList reads the list, answering the request itself on failure.
func (a *App) readPriorityList(ctx context.Context, w http.ResponseWriter, client *nitrado.Client, serviceID string) ([]string, bool) {
	list, err := client.PriorityList(ctx, serviceID)
	if errors.Is(err, nitrado.ErrPriorityUnsupported) {
		writeSaaSError(w, codeInvalidRequest, "this server has no priority list in Nitrado")
		return nil, false
	}
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado priority list read failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not read the priority list from Nitrado")
		return nil, false
	}
	return list, true
}

func (a *App) handleListPriority(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPriorityManage)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	list, err := client.PriorityList(ctx, serviceID)
	if errors.Is(err, nitrado.ErrPriorityUnsupported) {
		writeSaaSJSON(w, http.StatusOK, priorityListDTO{Supported: false, Items: []string{}, Limit: nitrado.MaxPriorityEntries})
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado priority list read failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not read the priority list from Nitrado")
		return
	}
	writeSaaSJSON(w, http.StatusOK, priorityListDTO{Supported: true, Items: list, Limit: nitrado.MaxPriorityEntries})
}

func (a *App) handleAddPriority(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPriorityManage)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[priorityRequest](w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !nitrado.ValidPriorityName(name) {
		writeSaaSError(w, codeInvalidRequest, "enter one player name or ID, up to 64 characters")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	list, ok := a.readPriorityList(ctx, w, client, serviceID)
	if !ok {
		return
	}
	if priorityIndex(list, name) >= 0 {
		writeSaaSJSON(w, http.StatusOK, priorityListDTO{Supported: true, Items: list, Limit: nitrado.MaxPriorityEntries})
		return
	}
	if len(list) >= nitrado.MaxPriorityEntries {
		writeSaaSError(w, codeInvalidRequest, "the priority list is full")
		return
	}
	updated := append(append(make([]string, 0, len(list)+1), list...), name)
	if err := client.SetPriorityList(ctx, serviceID, updated); err != nil {
		a.recordAudit(ctx, ac, "PRIORITY_ADD", name, req.Reason, "failure", nil, nil)
		slog.Warn("component=saas_api", "msg", "nitrado priority list write failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "Nitrado rejected this request")
		return
	}
	a.recordAudit(ctx, ac, "PRIORITY_ADD", name, req.Reason, "success", map[string]int{"count": len(list)}, map[string]int{"count": len(updated)})
	writeSaaSJSON(w, http.StatusCreated, priorityListDTO{Supported: true, Items: updated, Limit: nitrado.MaxPriorityEntries})
}

func (a *App) handleRemovePriority(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPriorityManage)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeSaaSError(w, codeInvalidRequest, "name is required")
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client, serviceID, ok := a.nitradoClientForOrg(ctx, w, ac)
	if !ok {
		return
	}
	list, ok := a.readPriorityList(ctx, w, client, serviceID)
	if !ok {
		return
	}
	at := priorityIndex(list, name)
	if at < 0 {
		// Already gone (removed in Nitrado's panel, or by another staff member a moment ago).
		writeSaaSJSON(w, http.StatusOK, priorityListDTO{Supported: true, Items: list, Limit: nitrado.MaxPriorityEntries})
		return
	}
	updated := append(append(make([]string, 0, len(list)), list[:at]...), list[at+1:]...)
	if err := client.SetPriorityList(ctx, serviceID, updated); err != nil {
		a.recordAudit(ctx, ac, "PRIORITY_REMOVE", name, "", "failure", nil, nil)
		slog.Warn("component=saas_api", "msg", "nitrado priority list write failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "Nitrado rejected this request")
		return
	}
	a.recordAudit(ctx, ac, "PRIORITY_REMOVE", name, "", "success", map[string]int{"count": len(list)}, map[string]int{"count": len(updated)})
	writeSaaSJSON(w, http.StatusOK, priorityListDTO{Supported: true, Items: updated, Limit: nitrado.MaxPriorityEntries})
}
