package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/dayzmap"
	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/stadium"
	"github.com/yourname/dayz-killfeed/internal/stadium/stadiumwrite"
)

// The Stadium (docs/STADIUM.md): the owner's tournament arena, previewed on the website and
// written to the DayZ server as an object-spawner file. Organization OWNER/ADMIN only, scoped to
// the installation; a "view as customer" session reads but never builds (requireSaaSServiceAuth
// refuses every non-GET it sends).
//
// This is the only file that imports internal/stadium/stadiumwrite (enforced by the isolation
// test in internal/shop/missionwrite). The owner pressing Build (or Remove) is the approval of
// that one write procedure; nothing here runs on a schedule or at start-up, and nothing restarts
// the server.

const (
	stadiumPositionWindow = 24 * time.Hour
	stadiumBuildTimeout   = 90 * time.Second
	stadiumMaxBody        = 64 << 10
	stadiumStatusNone     = "NONE"
)

func (a *App) registerStadiumRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/stadium"
	h := a.HTTPServer.Handle
	h("GET "+base, a.handleStadiumGet)
	h("PUT "+base, a.handleStadiumPut)
	h("POST "+base+"/position", a.handleStadiumPosition)
	h("POST "+base+"/build", a.handleStadiumBuild)
	h("POST "+base+"/remove", a.handleStadiumRemove)
	h("GET "+base+"/file", a.handleStadiumFile)
}

// stadiumRemote is the Nitrado surface a build uses: the three reads of maprotation.Reader and
// the three write primitives, which only internal/stadium/stadiumwrite ever calls.
// *nitrado.Client satisfies it.
type stadiumRemote interface {
	maprotation.Reader
	RequestUploadToken(ctx context.Context, serviceID, dir, name string) (nitrado.UploadTarget, error)
	PostUpload(ctx context.Context, t nitrado.UploadTarget, data []byte) error
	Mkdir(ctx context.Context, serviceID, parent, name string) error
}

// stadiumRemote opens the installation's Nitrado access with the organization's stored
// credential. The token never leaves the client. stadiumRemoteFor replaces it in tests.
func (a *App) stadiumRemote(ctx context.Context, t repository.StadiumTarget) (stadiumRemote, error) {
	if a.stadiumRemoteFor != nil {
		return a.stadiumRemoteFor(ctx, t)
	}
	if t.NitradoServiceID == "" || a.SaaSCredentials == nil {
		return nil, errors.New("no Nitrado service is connected")
	}
	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, t.OrganizationID)
	if err != nil || envelope == nil {
		return nil, errors.New("no Nitrado credential is connected")
	}
	return a.nitradoClientFromEnvelope(*envelope)
}

// --- DTOs (docs/STADIUM.md "API") ----------------------------------------------------------------

type stadiumBuildDTO struct {
	BuiltAt     *string               `json:"builtAt"`
	FileSHA256  *string               `json:"fileSha256"`
	ObjectCount int                   `json:"objectCount"`
	Referenced  bool                  `json:"referenced"`
	LastOutcome *stadiumwrite.Outcome `json:"lastOutcome"`
}

type stadiumDTO struct {
	Status    string           `json:"status"`
	Params    *stadium.Params  `json:"params"`
	Preview   *stadium.Preview `json:"preview"`
	Build     *stadiumBuildDTO `json:"build"`
	Dirty     bool             `json:"dirty"`
	UpdatedAt *string          `json:"updatedAt"`
	RemovedAt *string          `json:"removedAt"`
}

type stadiumPositionDTO struct {
	X          float64 `json:"x"`
	Z          float64 `json:"z"`
	AltitudeY  float64 `json:"altitudeY"`
	ObservedAt string  `json:"observedAt"`
	PlayerName string  `json:"playerName"`
	Source     string  `json:"source"`
}

// toStadiumDTO builds the response for a row (nil: nothing saved yet). The preview is rebuilt
// from the stored params; dirty says the saved params no longer match the file that was built.
func toStadiumDTO(row *repository.Stadium) stadiumDTO {
	if row == nil {
		return stadiumDTO{Status: stadiumStatusNone}
	}
	out := stadiumDTO{Status: row.Status}
	if t := rfc3339(row.UpdatedAt); t != "" {
		out.UpdatedAt = &t
	}
	if row.RemovedAt != nil {
		t := rfc3339(*row.RemovedAt)
		out.RemovedAt = &t
	}
	var fileSHA string
	if p, err := stadium.Parse(row.Params); err == nil {
		out.Params = &p
		if l, err := stadium.Build(p); err == nil {
			out.Preview = &l.Preview
			fileSHA = stadium.SHA256(stadium.Render(l.Objects))
		}
	}
	if row.Status != repository.StadiumDraft || row.LastOutcome != nil {
		b := &stadiumBuildDTO{ObjectCount: row.ObjectCount, FileSHA256: row.FileSHA256}
		if row.BuiltAt != nil {
			t := rfc3339(*row.BuiltAt)
			b.BuiltAt = &t
		}
		if len(row.LastOutcome) > 0 {
			var o stadiumwrite.Outcome
			if json.Unmarshal(row.LastOutcome, &o) == nil {
				b.LastOutcome = &o
				b.Referenced = o.Referenced
			}
		}
		out.Build = b
	}
	if row.Status == repository.StadiumBuilt && row.FileSHA256 != nil && fileSHA != "" && *row.FileSHA256 != fileSHA {
		out.Dirty = true
	}
	return out
}

// --- the chain ----------------------------------------------------------------------------------

type stadiumRequest struct {
	user  *repository.AppUser
	scope repository.AdminScope
}

// stadiumContext is the standard chain: service auth (which refuses a view-as write), acting
// user, path ids, OWNER/ADMIN of the organization, the installation's scope, and the admin
// rate limit (reads and writes have their own budgets).
func (a *App) stadiumContext(w http.ResponseWriter, r *http.Request, write bool) (stadiumRequest, bool) {
	if a.Stadium == nil || a.ClientAdmin == nil {
		writeSaaSError(w, codeInternalError, "stadium unavailable")
		return stadiumRequest{}, false
	}
	if !a.requireSaaSServiceAuth(w, r) {
		return stadiumRequest{}, false
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return stadiumRequest{}, false
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return stadiumRequest{}, false
	}
	instID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return stadiumRequest{}, false
	}
	if _, ok := a.requireOrganizationRole(w, r, orgID, user.ID); !ok {
		return stadiumRequest{}, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return stadiumRequest{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope, err := a.ClientAdmin.Scope(ctx, orgID, instID)
	if errors.Is(err, repository.ErrInstallationScopeNotFound) {
		writeSaaSError(w, codeNotFound, "installation not found")
		return stadiumRequest{}, false
	}
	if err != nil {
		slog.Warn("component=stadium", "event", "scope_failed", "installation_id", instID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve installation")
		return stadiumRequest{}, false
	}
	return stadiumRequest{user: user, scope: scope}, true
}

func (a *App) stadiumRow(w http.ResponseWriter, ctx context.Context, sr stadiumRequest) (*repository.Stadium, bool) {
	row, err := a.Stadium.Get(ctx, sr.scope.OrganizationID, sr.scope.InstallationID)
	if err != nil {
		slog.Warn("component=stadium", "event", "load_failed", "installation_id", sr.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the stadium")
		return nil, false
	}
	return row, true
}

func (a *App) writeStadium(w http.ResponseWriter, row *repository.Stadium) {
	writeSaaSJSON(w, http.StatusOK, map[string]any{"stadium": toStadiumDTO(row)})
}

// --- handlers -----------------------------------------------------------------------------------

func (a *App) handleStadiumGet(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.stadiumContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	row, ok := a.stadiumRow(w, ctx, sr)
	if !ok {
		return
	}
	a.writeStadium(w, row)
}

// handleStadiumPut saves the configuration as a DRAFT (a BUILT stadium stays BUILT, with the new
// params flagged dirty until the owner builds again) and answers with a fresh preview.
func (a *App) handleStadiumPut(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.stadiumContext(w, r, true)
	if !ok {
		return
	}
	body, err := stadiumReadBody(r, stadiumMaxBody)
	if err != nil || len(body) > stadiumMaxBody {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	p, err := stadium.Parse(body)
	if err != nil {
		writeSaaSError(w, codeValidationError, strings.TrimPrefix(err.Error(), stadium.ErrInvalidParams.Error()+": "))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	if p.MapKey == "" {
		// The map configured for the installation (the Shop's delivery map, which the live map
		// uses too); unset or unsupported leaves the key empty and the preview says it assumed.
		if key, err := a.Stadium.InstallationMapKey(ctx, sr.scope.InstallationID); err == nil {
			if m, ok := dayzmap.Lookup(key); ok {
				p.MapKey = m.Key
			}
		}
	}
	layout, err := stadium.Build(p)
	if err != nil {
		writeSaaSError(w, codeValidationError, strings.TrimPrefix(err.Error(), stadium.ErrInvalidParams.Error()+": "))
		return
	}
	before, ok := a.stadiumRow(w, ctx, sr)
	if !ok {
		return
	}
	raw, err := json.Marshal(p)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not save the stadium")
		return
	}
	row := repository.Stadium{InstallationID: sr.scope.InstallationID, OrganizationID: sr.scope.OrganizationID, Params: raw, Status: repository.StadiumDraft}
	if before != nil {
		row.FileSHA256, row.ObjectCount, row.BuiltAt, row.RemovedAt, row.LastOutcome = before.FileSHA256, before.ObjectCount, before.BuiltAt, before.RemovedAt, before.LastOutcome
		if before.Status == repository.StadiumBuilt {
			row.Status = repository.StadiumBuilt
		}
	}
	if err := a.Stadium.Upsert(ctx, row); err != nil {
		slog.Warn("component=stadium", "event", "save_failed", "installation_id", sr.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save the stadium")
		return
	}
	after, ok := a.stadiumRow(w, ctx, sr)
	if !ok {
		return
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "STADIUM_SAVE", fmt.Sprintf("installation:%d", sr.scope.InstallationID), "", "success",
		stadiumAuditState(before), map[string]any{"params": p, "objectCount": layout.Preview.ObjectCount})
	a.writeStadium(w, after)
}

func stadiumAuditState(row *repository.Stadium) any {
	if row == nil {
		return nil
	}
	return map[string]any{"status": row.Status, "params": json.RawMessage(row.Params), "fileSha256": row.FileSHA256}
}

type stadiumPositionBody struct {
	PlayerName string `json:"playerName"`
}

// handleStadiumPosition answers the latest ADM-recorded position (with its real altitude) of the
// acting owner's linked player on this server, or of the named player, within the last 24 hours.
func (a *App) handleStadiumPosition(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.stadiumContext(w, r, true)
	if !ok {
		return
	}
	var body stadiumPositionBody
	if raw, err := stadiumReadBody(r, 4<<10); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	} else if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeSaaSError(w, codeInvalidRequest, "invalid request body")
			return
		}
	}
	if sr.scope.ServerID == nil || *sr.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	since := time.Now().UTC().Add(-stadiumPositionWindow)
	var pos *repository.StadiumPosition
	var err error
	if name := strings.TrimSpace(body.PlayerName); name != "" {
		if len(name) > 64 {
			writeSaaSError(w, codeValidationError, "playerName is too long")
			return
		}
		pos, err = a.Stadium.LatestPositionByName(ctx, sr.scope.GuildID, *sr.scope.ServerID, name, since)
	} else {
		var playerID int64
		if playerID, err = a.Stadium.LinkedPlayer(ctx, sr.scope.GuildID, sr.user.DiscordUserID); err == nil {
			if playerID == 0 {
				writeSaaSError(w, codeNotFound, "no recent position: link your DayZ player to your Discord account first, or name a player")
				return
			}
			pos, err = a.Stadium.LatestPosition(ctx, sr.scope.GuildID, *sr.scope.ServerID, playerID, since)
		}
	}
	if err != nil {
		slog.Warn("component=stadium", "event", "position_failed", "installation_id", sr.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not read positions")
		return
	}
	if pos == nil {
		writeSaaSError(w, codeNotFound, "no recent position: the player must have been seen on this server in the last 24 hours")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"position": stadiumPositionDTO{X: pos.X, Z: pos.Z, AltitudeY: pos.AltitudeY, ObservedAt: rfc3339(pos.ObservedAt), PlayerName: pos.PlayerName, Source: "ADM"}})
}

func (a *App) handleStadiumBuild(w http.ResponseWriter, r *http.Request) { a.stadiumApply(w, r, true) }
func (a *App) handleStadiumRemove(w http.ResponseWriter, r *http.Request) {
	a.stadiumApply(w, r, false)
}

// stadiumApply is the owner-approved write: Build writes the layout and references it, Remove
// writes the empty file. The outcome is answered and recorded whatever it is; only a request
// that cannot start at all (no configuration, no server, another build running, no Nitrado
// access) is an error.
func (a *App) stadiumApply(w http.ResponseWriter, r *http.Request, build bool) {
	sr, ok := a.stadiumContext(w, r, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), stadiumBuildTimeout)
	defer cancel()
	row, ok := a.stadiumRow(w, ctx, sr)
	if !ok {
		return
	}
	if row == nil {
		writeSaaSError(w, codeConflict, "save the stadium configuration first")
		return
	}
	params, err := stadium.Parse(row.Params)
	if err != nil {
		writeSaaSError(w, codeConflict, "the saved stadium configuration is no longer valid: save it again")
		return
	}
	req := stadiumwrite.Request{Payload: stadium.EmptyFile()}
	if build {
		layout, err := stadium.Build(params)
		if err != nil {
			writeSaaSError(w, codeValidationError, strings.TrimPrefix(err.Error(), stadium.ErrInvalidParams.Error()+": "))
			return
		}
		req = stadiumwrite.Request{Payload: stadium.Render(layout.Objects), Reference: true, ObjectCount: len(layout.Objects)}
	}
	target, err := a.Stadium.Target(ctx, sr.scope.OrganizationID, sr.scope.InstallationID)
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not resolve the server")
		return
	}
	if target == nil || target.NitradoServiceID == "" {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return
	}
	req.ServiceID = target.NitradoServiceID
	if _, running := a.stadiumBusy.LoadOrStore(sr.scope.InstallationID, true); running {
		writeSaaSError(w, codeConflict, "a stadium build is already running for this server")
		return
	}
	defer a.stadiumBusy.Delete(sr.scope.InstallationID)
	remote, err := a.stadiumRemote(ctx, *target)
	if err != nil {
		writeSaaSError(w, codeNitradoUnavailable, "could not open the server's Nitrado connection")
		return
	}
	action := "STADIUM_REMOVE"
	if build {
		action = "STADIUM_BUILD"
	}
	outcome := stadiumwrite.Apply(ctx, remote, req)
	slog.Info("component=stadium", "event", strings.ToLower(action), "installation_id", sr.scope.InstallationID, "status", outcome.Status,
		"file_verified", outcome.FileVerified, "referenced", outcome.Referenced, "writes", outcome.Writes)

	// Record it, even when the request's own deadline has passed.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer rcancel()
	next := *row
	next.Status = row.Status
	if raw, err := json.Marshal(outcome); err == nil {
		next.LastOutcome = raw
	}
	now := time.Now().UTC()
	switch outcome.Status {
	case stadiumwrite.StatusBuilt:
		sha := outcome.SHA256
		next.Status, next.FileSHA256, next.ObjectCount, next.BuiltAt, next.RemovedAt = repository.StadiumBuilt, &sha, outcome.ObjectCount, &now, nil
	case stadiumwrite.StatusRemoved:
		sha := outcome.SHA256
		next.Status, next.FileSHA256, next.ObjectCount, next.RemovedAt = repository.StadiumRemoved, &sha, 0, &now
	default:
		if outcome.FileVerified && build {
			// The file is on the server but not referenced: remember its digest so a rebuild of
			// the same params is recognised, but the status stays as it was.
			sha := outcome.SHA256
			next.FileSHA256, next.ObjectCount = &sha, outcome.ObjectCount
		}
	}
	if err := a.Stadium.Upsert(rctx, next); err != nil {
		slog.Warn("component=stadium", "event", "record_failed", "installation_id", sr.scope.InstallationID, "err", err.Error())
	}
	a.recordAudit(rctx, adminActor{user: sr.user, scope: sr.scope}, action, fmt.Sprintf("installation:%d", sr.scope.InstallationID), "", strings.ToLower(outcome.Status),
		stadiumAuditState(row), map[string]any{"status": outcome.Status, "sha256": outcome.SHA256, "referenced": outcome.Referenced, "message": outcome.Message})
	after, err := a.Stadium.Get(rctx, sr.scope.OrganizationID, sr.scope.InstallationID)
	if err != nil {
		after = &next
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"outcome": outcome, "stadium": toStadiumDTO(after)})
}

// handleStadiumFile downloads the current layout as the spawner file, for a manual upload.
func (a *App) handleStadiumFile(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.stadiumContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	row, ok := a.stadiumRow(w, ctx, sr)
	if !ok {
		return
	}
	if row == nil {
		writeSaaSError(w, codeNotFound, "no stadium configuration has been saved")
		return
	}
	params, err := stadium.Parse(row.Params)
	if err != nil {
		writeSaaSError(w, codeConflict, "the saved stadium configuration is no longer valid: save it again")
		return
	}
	layout, err := stadium.Build(params)
	if err != nil {
		writeSaaSError(w, codeValidationError, strings.TrimPrefix(err.Error(), stadium.ErrInvalidParams.Error()+": "))
		return
	}
	body := stadium.Render(layout.Objects)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+stadium.FileName+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// stadiumReadBody reads at most max bytes of the request body.
func stadiumReadBody(r *http.Request, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, max+1))
}
