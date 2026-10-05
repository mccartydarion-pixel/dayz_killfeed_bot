// Package app: platform roles and the platform staff list (docs/ADMIN_API.md "Roles").
//
// Platform staff are Discord accounts the platform owner lets into the Owner Hub to look, not
// to touch: every /api/admin read works for them and every write answers 403 (adminRoute). The
// owner manages the list here; each change is audited with the owner's reason. Platform owners
// are never on the list - they come only from CHAMPION_ADMIN_DISCORD_IDS.
package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// staffNoteMax is the longest note the owner may keep next to a staff member.
const staffNoteMax = 120

// isDiscordSnowflake reports whether s looks like a Discord user id: digits only, 15 to 20 long.
func isDiscordSnowflake(s string) bool {
	if len(s) < 15 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type staffMemberDTO struct {
	DiscordID string `json:"discordId"`
	Note      string `json:"note"`
	AddedBy   string `json:"addedBy"`
	AddedAt   string `json:"addedAt"`
}

func staffDTO(m repository.PlatformStaffMember) staffMemberDTO {
	return staffMemberDTO{DiscordID: m.DiscordID, Note: m.Note, AddedBy: m.AddedBy, AddedAt: m.AddedAt.UTC().Format(time.RFC3339)}
}

// handleAdminMe is GET /api/admin/me: who the caller is to the platform. Anyone who is neither
// owner nor staff never gets here (requirePlatformAdmin answers 403).
func (a *App) handleAdminMe(w http.ResponseWriter, _ *http.Request, admin adminIdentity) {
	a.writeAdminJSON(w, http.StatusOK, map[string]string{"role": admin.Role, "discordId": admin.DiscordID})
}

// writeStaffList answers with the whole staff list, the shape every staff route returns.
func (a *App) writeStaffList(ctx context.Context, w http.ResponseWriter, store platformStaffStore) {
	rows, err := store.ListPlatformStaff(ctx)
	if err != nil {
		a.adminReadFailed(w, "platform staff", err)
		return
	}
	staff := make([]staffMemberDTO, 0, len(rows))
	for _, m := range rows {
		staff = append(staff, staffDTO(m))
	}
	a.writeAdminJSON(w, http.StatusOK, map[string]any{"staff": staff})
}

func (a *App) staffStoreOrFail(w http.ResponseWriter) (platformStaffStore, bool) {
	store := a.staffStore()
	if store == nil {
		writeSaaSError(w, codeInternalError, "owner controls unavailable")
		return nil, false
	}
	return store, true
}

// handleAdminListStaff is GET /api/admin/staff. Owner and staff may read it.
func (a *App) handleAdminListStaff(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	store, ok := a.staffStoreOrFail(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	a.writeStaffList(ctx, w, store)
}

type staffAddRequest struct {
	DiscordID string `json:"discordId"`
	Note      string `json:"note"`
}

// handleAdminAddStaff is POST /api/admin/staff, body {discordId, note?, reason}: put a Discord
// account on the staff list, or change the note of one already on it. Owner only (adminRoute).
func (a *App) handleAdminAddStaff(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	reason, body, ok := readOwnerBody[staffAddRequest](a, w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(body.DiscordID)
	note := strings.TrimSpace(body.Note)
	if !isDiscordSnowflake(id) {
		writeSaaSError(w, codeInvalidRequest, "discordId must be a Discord user id (15 to 20 digits)")
		return
	}
	if utf8.RuneCountInString(note) > staffNoteMax {
		writeSaaSError(w, codeInvalidRequest, "note is too long (max 120 characters)")
		return
	}
	if a.Config != nil && a.Config.IsPlatformAdmin(id) {
		writeSaaSError(w, codeConflict, "this account is a platform owner and already has full access; it cannot be added as staff")
		return
	}
	store, ok := a.staffStoreOrFail(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	before, after, err := store.UpsertPlatformStaff(ctx, id, note, admin.DiscordID)
	if err != nil {
		ownerFailed(w, "add platform staff", err)
		return
	}
	action, beforeState := "staff.added", any(nil)
	if before != nil {
		action, beforeState = "staff.note_updated", staffDTO(*before)
	}
	a.ownerAudit(ctx, admin, action, "platform_staff", 0, nil, reason, "OK", beforeState, staffDTO(after))
	a.writeStaffList(ctx, w, store)
}

// handleAdminRemoveStaff is DELETE /api/admin/staff/{discordID}, body {reason}: take a Discord
// account off the staff list. It loses access on its next request. Owner only (adminRoute).
func (a *App) handleAdminRemoveStaff(w http.ResponseWriter, r *http.Request, admin adminIdentity) {
	req, ok := a.readOwnerRequest(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("discordID"))
	if !isDiscordSnowflake(id) {
		writeSaaSError(w, codeNotFound, "platform staff member not found")
		return
	}
	store, ok := a.staffStoreOrFail(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ownerActionTimeout)
	defer cancel()
	removed, err := store.RemovePlatformStaff(ctx, id)
	if errors.Is(err, repository.ErrPlatformStaffNotFound) {
		writeSaaSError(w, codeNotFound, "platform staff member not found")
		return
	}
	if err != nil {
		ownerFailed(w, "remove platform staff", err)
		return
	}
	a.ownerAudit(ctx, admin, "staff.removed", "platform_staff", 0, nil, req.Reason, "OK", staffDTO(removed), nil)
	a.writeStaffList(ctx, w, store)
}

// registerStaffAPI wires the role and staff routes.
func (a *App) registerStaffAPI() {
	a.adminHandle("GET /api/admin/me", a.handleAdminMe)
	a.adminHandle("GET /api/admin/staff", a.handleAdminListStaff)
	a.adminHandle("POST /api/admin/staff", a.handleAdminAddStaff)
	a.adminHandle("DELETE /api/admin/staff/{discordID}", a.handleAdminRemoveStaff)
}
