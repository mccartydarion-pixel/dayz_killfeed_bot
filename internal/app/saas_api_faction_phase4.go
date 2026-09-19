package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionassets"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/logoimage"
)

// Faction Hub Phase 4 (docs/FACTIONS.md): faction logo storage, leadership transfer and
// self-leave. Same authorization model as the rest of the Hub: the acting user's explicit
// FACTION role, read inside the mutating transaction; an organization OWNER/ADMIN gains
// nothing here.

// Error codes added in Phase 4 (registered in httpStatusForCode).
const (
	codeUnsupportedMediaType = "UNSUPPORTED_MEDIA_TYPE" // 415
	codeLeadershipTransfer   = "LEADERSHIP_TRANSFER_REQUIRED"
)

const (
	// logoUploadBodyLimit bounds the whole multipart body: the file (logoimage.MaxBytes) plus
	// generous room for boundaries and part headers. Anything past it is 413.
	logoUploadBodyLimit = logoimage.MaxBytes + 64<<10
	// logoUploadReadWindow lets a slow client finish a 5 MiB upload; the server-wide ReadTimeout
	// is short and stays that way for every other route.
	logoUploadReadWindow = 60 * time.Second
	logoAssetPath        = "/assets/faction-logos/"
)

func (a *App) registerFactionPhase4Routes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/factions"
	h := a.HTTPServer.Handle
	h("POST "+base+"/{factionID}/logo", a.handleUploadFactionLogo)
	h("DELETE "+base+"/{factionID}/logo", a.handleDeleteFactionLogo)
	h("POST "+base+"/{factionID}/transfer-leadership", a.handleTransferFactionLeadership)
	h("POST "+base+"/{factionID}/leave", a.handleLeaveFaction)
	// Public, unauthenticated: logos are public profile assets addressed by an unguessable id.
	h("GET "+logoAssetPath+"{file}", a.handleFactionLogoAsset)
}

// --- logo DTO ----------------------------------------------------------------------------------

// factionLogoDTO is the public representation of a faction's logo. It carries no storage key,
// bucket or credential - only what a browser needs to render the image.
type factionLogoDTO struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"contentType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// assetBaseURL is the public origin used for absolute logo URLs ("" = root-relative URLs).
func (a *App) assetBaseURL() string {
	if a == nil || a.Config == nil {
		return ""
	}
	return strings.TrimRight(a.Config.PublicBaseURL, "/")
}

func toFactionLogo(asset *factionhub.Asset, base string) *factionLogoDTO {
	if asset == nil {
		return nil
	}
	return &factionLogoDTO{ID: asset.ID, URL: base + logoAssetPath + asset.PublicID + "." + logoimage.ExtFor(asset.ContentType),
		ContentType: asset.ContentType, Width: asset.Width, Height: asset.Height}
}

// --- upload ------------------------------------------------------------------------------------

type uploadLogoResponse struct {
	Logo     *factionLogoDTO `json:"logo"`
	Replaced bool            `json:"replaced"`
}

// handleUploadFactionLogo is POST .../factions/{factionID}/logo (faction LEADER only).
// multipart/form-data with exactly one part named "file". The LEADER check runs BEFORE the body
// is read, so an unauthorized caller never makes the server buffer an upload.
func (a *App) handleUploadFactionLogo(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	if a.FactionAssets == nil {
		writeSaaSError(w, codeInternalError, "logo storage unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*logoUploadReadWindow)
	defer cancel()
	if err := a.FactionAssets.RequireLeader(ctx, fr.orgID, fr.instID, factionID, fr.user.ID); err != nil {
		factionFailed(w, "upload logo", err)
		return
	}
	key := rateLimitKey(r)
	if !enforceRateLimit(w, a.saasFactionLogoLimiter, key) || !enforceRateLimit(w, a.saasFactionLogoDayLimiter, key) {
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeSaaSError(w, codeUnsupportedMediaType, "send the logo as multipart/form-data with a single field named file")
		return
	}
	// Give this one route time to receive up to 5 MiB; every other route keeps the short server timeout.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(logoUploadReadWindow))

	file, err := readLogoPart(w, r, params["boundary"])
	if err != nil {
		var tooBig *http.MaxBytesError
		var bad *logoFormError
		switch {
		case errors.As(err, &tooBig), errors.Is(err, logoimage.ErrTooLarge):
			writeSaaSError(w, codePayloadTooLarge, "the logo must be at most 5 MiB")
		case errors.As(err, &bad):
			writeSaaSError(w, codeInvalidRequest, bad.msg)
		default:
			writeSaaSError(w, codeInvalidRequest, "could not read the upload")
		}
		return
	}

	res, err := a.FactionAssets.UploadLogo(ctx, factionassets.UploadInput{OrganizationID: fr.orgID, InstallationID: fr.instID, FactionID: factionID,
		ActorUserID: fr.user.ID, Data: file.data, DeclaredType: file.declaredType, Filename: file.filename})
	if err != nil {
		logoUploadFailed(w, err)
		return
	}
	event := "faction_logo_uploaded"
	status := http.StatusCreated
	if res.Replaced {
		event, status = "faction_logo_replaced", http.StatusOK
	}
	// Safe identifiers only: never the image bytes or the client-supplied file name.
	factionAudit(event, fr, "faction_id", factionID, "asset_id", res.Asset.ID, "content_type", res.Asset.ContentType, "size_bytes", res.Asset.SizeBytes)
	writeSaaSJSON(w, status, uploadLogoResponse{Logo: toFactionLogo(&res.Asset, a.assetBaseURL()), Replaced: res.Replaced})
}

type logoFormError struct{ msg string }

func (e *logoFormError) Error() string { return e.msg }

type logoFile struct {
	data                   []byte
	declaredType, filename string
}

// readLogoPart streams the multipart body and returns the one "file" part. Any other field
// (externalUrl, a base64 blob, raw SVG in a text field, ...) is refused, as is a second file.
func readLogoPart(w http.ResponseWriter, r *http.Request, boundary string) (logoFile, error) {
	body := http.MaxBytesReader(w, r.Body, logoUploadBodyLimit)
	mr := multipart.NewReader(body, boundary)
	var out logoFile
	seen := false
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return logoFile{}, err
		}
		if part.FormName() != "file" {
			_ = part.Close()
			return logoFile{}, &logoFormError{"the only accepted form field is file"}
		}
		if seen {
			_ = part.Close()
			return logoFile{}, &logoFormError{"send exactly one file"}
		}
		seen = true
		data, err := io.ReadAll(io.LimitReader(part, logoimage.MaxBytes+1))
		_ = part.Close()
		if err != nil {
			return logoFile{}, err
		}
		if len(data) > logoimage.MaxBytes {
			return logoFile{}, logoimage.ErrTooLarge
		}
		out = logoFile{data: data, declaredType: part.Header.Get("Content-Type"), filename: part.FileName()}
	}
	if !seen {
		return logoFile{}, &logoFormError{"the file field is required"}
	}
	return out, nil
}

func logoUploadFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, logoimage.ErrEmpty):
		writeSaaSError(w, codeInvalidRequest, "the file is empty")
	case errors.Is(err, logoimage.ErrTooLarge):
		writeSaaSError(w, codePayloadTooLarge, "the logo must be at most 5 MiB")
	case errors.Is(err, logoimage.ErrUnsupported):
		writeSaaSError(w, codeUnsupportedMediaType, "only PNG, JPEG and WebP images are accepted")
	case errors.Is(err, factionassets.ErrTypeMismatch):
		writeSaaSError(w, codeUnsupportedMediaType, "the declared content type does not match the image; only PNG, JPEG and WebP are accepted")
	case errors.Is(err, logoimage.ErrCorrupt):
		writeSaaSError(w, codeInvalidRequest, "the image is corrupt or not a valid PNG, JPEG or WebP")
	case errors.Is(err, logoimage.ErrDimensions):
		writeSaaSError(w, codeInvalidRequest, fmt.Sprintf("the image must be between %dx%d and %dx%d pixels", logoimage.MinDimension, logoimage.MinDimension, logoimage.MaxDimension, logoimage.MaxDimension))
	default:
		factionFailed(w, "upload logo", err)
	}
}

// --- delete ------------------------------------------------------------------------------------

type deleteLogoResponse struct {
	Logo    *factionLogoDTO `json:"logo"` // always null: the faction is back on the Champion default logo
	Deleted bool            `json:"deleted"`
}

// handleDeleteFactionLogo is DELETE .../factions/{factionID}/logo (faction LEADER only).
// Idempotent: 200 whether or not a logo existed (deleted says which).
func (a *App) handleDeleteFactionLogo(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	if a.FactionAssets == nil {
		writeSaaSError(w, codeInternalError, "logo storage unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	old, err := a.FactionAssets.DeleteLogo(ctx, fr.orgID, fr.instID, factionID, fr.user.ID)
	if err != nil {
		factionFailed(w, "delete logo", err)
		return
	}
	if old != nil {
		factionAudit("faction_logo_deleted", fr, "faction_id", factionID, "asset_id", old.ID)
	}
	writeSaaSJSON(w, http.StatusOK, deleteLogoResponse{Deleted: old != nil})
}

// --- public serving ----------------------------------------------------------------------------

// handleFactionLogoAsset is GET /assets/faction-logos/{uuid}.{png|jpg|webp}: the public logo.
// No service auth (browsers load it directly); the unguessable id is the only address, and a
// replaced or deleted logo no longer resolves. Responses are immutable, non-sniffable and
// sandboxed so a stored image can never be interpreted as a document.
func (a *App) handleFactionLogoAsset(w http.ResponseWriter, r *http.Request) {
	id, ext, ok := splitAssetFile(r.PathValue("file"))
	if !ok || a.FactionAssets == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	served, err := a.FactionAssets.Serve(ctx, id)
	if errors.Is(err, factionhub.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Warn("component=saas_api", "msg", "serve faction logo failed", "err", err.Error())
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	if logoimage.ExtFor(served.ContentType) != ext { // the URL names the stored format
		http.NotFound(w, r)
		return
	}
	etag := `"` + served.Asset.PublicID + `"`
	h := w.Header()
	h.Set("Content-Type", served.ContentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", fmt.Sprint(len(served.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(served.Data)
	}
}

// splitAssetFile splits "<uuid>.<ext>" and validates both halves.
func splitAssetFile(file string) (id, ext string, ok bool) {
	i := strings.LastIndexByte(file, '.')
	if i != 36 {
		return "", "", false
	}
	id, ext = file[:i], file[i+1:]
	if ext != "png" && ext != "jpg" && ext != "webp" {
		return "", "", false
	}
	for j := 0; j < len(id); j++ {
		c := id[j]
		switch {
		case j == 8 || j == 13 || j == 18 || j == 23:
			if c != '-' {
				return "", "", false
			}
		case !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f'):
			return "", "", false
		}
	}
	return id, ext, true
}

// --- leadership transfer -----------------------------------------------------------------------

type transferLeadershipRequest struct {
	MemberID int64 `json:"memberId"`
}

type transferLeadershipResponse struct {
	Leader         factionMemberDTO `json:"leader"`
	PreviousLeader factionMemberDTO `json:"previousLeader"`
}

// handleTransferFactionLeadership is POST .../factions/{factionID}/transfer-leadership (faction
// LEADER only). The target must be another member of THIS faction; the previous leader becomes
// an OFFICER. Atomic: the faction never has zero or two leaders.
func (a *App) handleTransferFactionLeadership(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	var req transferLeadershipRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	if req.MemberID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "memberId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	next, prev, err := a.FactionHub.TransferLeadership(ctx, fr.orgID, fr.instID, factionID, fr.user.ID, req.MemberID)
	if err != nil {
		factionFailed(w, "transfer leadership", err)
		return
	}
	factionAudit("faction_leadership_transferred", fr, "faction_id", factionID, "previous_leader_user_id", prev.User.ID, "new_leader_user_id", next.User.ID)
	a.factionStatsChanged(fr, factionID)
	writeSaaSJSON(w, http.StatusOK, transferLeadershipResponse{Leader: toFactionMember(next), PreviousLeader: toFactionMember(prev)})
}

// --- self-leave --------------------------------------------------------------------------------

type leaveFactionResponse struct {
	Left   bool             `json:"left"`
	Member factionMemberDTO `json:"member"` // the membership that ended
}

// handleLeaveFaction is POST .../factions/{factionID}/leave: the caller leaves their own faction.
// MEMBER and OFFICER may; the LEADER gets 409 LEADERSHIP_TRANSFER_REQUIRED and must transfer
// leadership first. A caller who is not a member (including a repeated leave) gets 403.
func (a *App) handleLeaveFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	m, err := a.FactionHub.LeaveFaction(ctx, fr.orgID, fr.instID, factionID, fr.user.ID)
	if err != nil {
		factionFailed(w, "leave faction", err)
		return
	}
	factionAudit("faction_member_left", fr, "faction_id", factionID, "member_id", m.ID, "left_role", m.RoleKey)
	a.factionStatsChanged(fr, factionID)
	writeSaaSJSON(w, http.StatusOK, leaveFactionResponse{Left: true, Member: toFactionMember(*m)})
}
