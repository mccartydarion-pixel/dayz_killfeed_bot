// Package app: Owner Hub user directory (docs/ADMIN_API.md "Users").
package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/adminrepo"
)

// handleAdminListUsers is GET /api/admin/users: every website account, newest first, with
// organizations, last login and ban state. Filters: search (username / global name / Discord
// id), banned=true|false.
func (a *App) handleAdminListUsers(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	p, ok := parseAdminListParams(w, r)
	if !ok {
		return
	}
	f := adminrepo.UserFilter{Limit: p.Limit, Cursor: p.Cursor, Search: p.Search}
	switch strings.ToLower(strings.TrimSpace(p.Query.Get("banned"))) {
	case "true":
		v := true
		f.Banned = &v
	case "false":
		v := false
		f.Banned = &v
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rows, next, err := a.adminSaaS.ListUsers(ctx, f)
	if err != nil {
		a.adminReadFailed(w, "users", err)
		return
	}
	total, banned, active, err := a.adminSaaS.CountUsers(ctx)
	if err != nil {
		a.adminReadFailed(w, "user counts", err)
		return
	}
	page := adminList(rows, next, p.Limit)
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"items": page.Items, "nextCursor": page.NextCursor, "limit": page.Limit,
		"counts": map[string]int64{"total": total, "banned": banned, "activeLast30d": active}})
}

// handleAdminGetUser is GET /api/admin/users/{userID}.
func (a *App) handleAdminGetUser(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	id, ok := pathInt64(w, r, "userID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	u, err := a.adminSaaS.GetUser(ctx, id)
	if err != nil {
		a.adminReadFailed(w, "user", err)
		return
	}
	if u == nil {
		writeSaaSError(w, codeNotFound, "user not found")
		return
	}
	a.writeAdminJSON(w, http.StatusOK, u)
}

func (a *App) registerUsersAPI() {
	if a.HTTPServer == nil {
		return
	}
	a.adminHandle("GET /api/admin/users", a.handleAdminListUsers)
	a.adminHandle("GET /api/admin/users/{userID}", a.handleAdminGetUser)
}
