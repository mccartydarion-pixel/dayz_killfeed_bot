package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Faction Hub Phase 1 (docs/FACTIONS.md): the installation-scoped faction directory,
// profiles, membership and recruitment API. Backend only.
//
// Authorization is layered and separate:
//   - service auth + acting user (every route), exactly like the other /api/saas routes;
//   - reads and player actions (create a faction, apply, withdraw) are open to ANY
//     synced Champion user, not only organization members: players are not customers.
//     The installation must belong to the organization in the path, or the answer is 404;
//   - mutations of a faction are decided ONLY by the acting user's explicit FACTION role
//     (LEADER / OFFICER), read inside the mutating transaction. An organization
//     OWNER/ADMIN is not a faction leader, and the platform admin allowlist is not
//     consulted at all;
//   - the one organization-role privilege is a read-only moderation view of a faction's
//     applications (HubAccess.OrgViewer).

const (
	maxFactionBody         = 16 << 10
	defaultDirectoryLimit  = 25
	maxDirectoryLimit      = 100
	defaultApplicationsLim = 25
	defaultMembersLimit    = 100
	maxMembersLimit        = 200
	factionCursorPrefix    = "fh1:"
	factionTimeout         = 8 * time.Second
)

func (a *App) registerFactionHubRoutes() {
	const base = "/api/saas/organizations/{organizationID}/installations/{installationID}/factions"
	h := a.HTTPServer.Handle
	h("GET "+base, a.handleFactionDirectory)
	h("POST "+base, a.handleCreateFaction)
	h("GET "+base+"/me", a.handleMyFaction)
	h("GET "+base+"/{factionID}", a.handleGetFaction)
	h("PUT "+base+"/{factionID}", a.handleUpdateFaction)
	h("POST "+base+"/{factionID}/applications", a.handleApplyToFaction)
	h("GET "+base+"/{factionID}/applications", a.handleListFactionApplications)
	h("POST "+base+"/{factionID}/applications/{applicationID}/accept", a.handleAcceptFactionApplication)
	h("POST "+base+"/{factionID}/applications/{applicationID}/deny", a.handleDenyFactionApplication)
	h("POST "+base+"/{factionID}/applications/{applicationID}/withdraw", a.handleWithdrawFactionApplication)
	h("GET "+base+"/{factionID}/members", a.handleListFactionMembers)
	h("POST "+base+"/{factionID}/members/{memberID}/promote", a.handlePromoteFactionMember)
	h("POST "+base+"/{factionID}/members/{memberID}/demote", a.handleDemoteFactionMember)
	h("DELETE "+base+"/{factionID}/members/{memberID}", a.handleRemoveFactionMember)
}

// --- DTOs -------------------------------------------------------------------------------------

type factionSummaryDTO struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"`
	Tag                string  `json:"tag"`
	Slug               string  `json:"slug"`
	DescriptionPreview string  `json:"descriptionPreview"`
	RecruitmentStatus  string  `json:"recruitmentStatus"`
	MemberCount        int     `json:"memberCount"`
	LogoKey            *string `json:"logoKey"`
	FlagKey            *string `json:"flagKey"`
	ArmbandKey         *string `json:"armbandKey"`
	PrimaryColor       *string `json:"primaryColor"`
	SecondaryColor     *string `json:"secondaryColor"`
}

func toFactionSummary(f repository.HubFaction) factionSummaryDTO {
	return factionSummaryDTO{ID: f.ID, Name: f.Name, Tag: f.Tag, Slug: f.Slug, DescriptionPreview: factionhub.Preview(f.Description),
		RecruitmentStatus: f.RecruitmentStatus, MemberCount: f.MemberCount, LogoKey: f.LogoKey, FlagKey: f.FlagKey, ArmbandKey: f.ArmbandKey,
		PrimaryColor: f.PrimaryColor, SecondaryColor: f.SecondaryColor}
}

type factionMemberDTO struct {
	ID          int64   `json:"id"`
	UserID      int64   `json:"userId"`
	DiscordID   string  `json:"discordUserId"`
	Username    string  `json:"username"`
	DisplayName string  `json:"displayName"`
	Avatar      string  `json:"avatar,omitempty"`
	Gamertag    *string `json:"gamertag"`
	Role        string  `json:"role"`
	JoinedAt    string  `json:"joinedAt"`
}

func displayName(u repository.HubUser) string {
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

func toFactionMember(m repository.HubMember) factionMemberDTO {
	return factionMemberDTO{ID: m.ID, UserID: m.User.ID, DiscordID: m.User.DiscordUserID, Username: m.User.Username, DisplayName: displayName(m.User),
		Avatar: m.User.Avatar, Gamertag: m.Gamertag, Role: m.RoleKey, JoinedAt: m.JoinedAt.UTC().Format(time.RFC3339)}
}

type factionViewerDTO struct {
	Role                  *string `json:"role"` // the viewer's faction role, null when not a member
	CanEdit               bool    `json:"canEdit"`
	CanManageApplications bool    `json:"canManageApplications"`
	// ModerationView is true when the viewer is an organization OWNER/ADMIN: they may
	// view (never change) the faction's management state.
	ModerationView bool `json:"moderationView"`
}

type factionProfileDTO struct {
	factionSummaryDTO
	InstallationID int64               `json:"installationId"`
	GameServerID   int64               `json:"gameServerId"`
	Description    string              `json:"description"`
	Requirements   factionhub.Settings `json:"requirements"`
	Leader         *factionMemberDTO   `json:"leader"`
	Officers       []factionMemberDTO  `json:"officers"`
	Members        []factionMemberDTO  `json:"members"`
	CreatedAt      string              `json:"createdAt"`
	UpdatedAt      string              `json:"updatedAt"`
	Viewer         factionViewerDTO    `json:"viewer"`
}

type factionApplicationDTO struct {
	ID               int64             `json:"id"`
	FactionID        int64             `json:"factionId"`
	UserID           int64             `json:"userId"`
	Applicant        factionMemberUser `json:"applicant"`
	Status           string            `json:"status"`
	Message          string            `json:"message"`
	ReviewedByUserID *int64            `json:"reviewedByUserId"`
	ReviewedAt       *string           `json:"reviewedAt"`
	CreatedAt        string            `json:"createdAt"`
	UpdatedAt        string            `json:"updatedAt"`
	Faction          *factionRefDTO    `json:"faction,omitempty"`
}

type factionMemberUser struct {
	DiscordID   string `json:"discordUserId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Avatar      string `json:"avatar,omitempty"`
}

type factionRefDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Tag  string `json:"tag"`
	Slug string `json:"slug"`
}

func toFactionApplication(a repository.HubApplication, withFaction bool) factionApplicationDTO {
	out := factionApplicationDTO{ID: a.ID, FactionID: a.FactionID, UserID: a.User.ID,
		Applicant: factionMemberUser{DiscordID: a.User.DiscordUserID, Username: a.User.Username, DisplayName: displayName(a.User), Avatar: a.User.Avatar},
		Status:    a.Status, Message: a.Message, ReviewedByUserID: a.ReviewedByUserID, ReviewedAt: nullableTimeStr(a.ReviewedAt),
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: a.UpdatedAt.UTC().Format(time.RFC3339)}
	if withFaction {
		out.Faction = &factionRefDTO{ID: a.FactionID, Name: a.FactionName, Tag: a.FactionTag, Slug: a.FactionSlug}
	}
	return out
}

type factionPage[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
	Limit      int     `json:"limit"`
}

func encodeFactionCursor(id int64) *string {
	if id <= 0 {
		return nil
	}
	s := base64.RawURLEncoding.EncodeToString([]byte(factionCursorPrefix + strconv.FormatInt(id, 10)))
	return &s
}

func decodeFactionCursor(s string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), factionCursorPrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(string(raw), factionCursorPrefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// --- shared request plumbing ------------------------------------------------------------------

type factionRequest struct {
	orgID, instID int64
	user          *repository.AppUser
}

// factionContext runs the checks every Faction Hub route shares: service auth, acting user,
// and the path ids. Organization membership is deliberately NOT required (players are not
// organization members); the repository scopes every query by organization + installation,
// so a foreign installation is a 404.
func (a *App) factionContext(w http.ResponseWriter, r *http.Request) (fr factionRequest, ok bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	orgID, good := pathInt64(w, r, "organizationID")
	if !good {
		return
	}
	instID, good := pathInt64(w, r, "installationID")
	if !good {
		return
	}
	if a.FactionHub == nil {
		writeSaaSError(w, codeInternalError, "faction service unavailable")
		return
	}
	return factionRequest{orgID: orgID, instID: instID, user: user}, true
}

// orgViewer reports whether the user is an OWNER/ADMIN of the organization (read-only
// moderation visibility).
func (a *App) orgViewer(ctx context.Context, orgID, userID int64) bool {
	if a.SaaSOrganizations == nil {
		return false
	}
	role, member, err := a.SaaSOrganizations.VerifyMembership(ctx, orgID, userID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "faction moderation view membership check failed", "err", err.Error())
		return false
	}
	return member && (role == repository.RoleOwner || role == repository.RoleAdmin)
}

// decodeFactionBody decodes exactly one JSON object with no unknown keys.
func decodeFactionBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, maxFactionBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields() // typed structure only: an unexpected key (e.g. an image URL) is rejected, not stored
	tooBig := func(err error) bool {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeSaaSError(w, codePayloadTooLarge, "request body is too large")
			return true
		}
		return false
	}
	if err := dec.Decode(dst); err != nil {
		if !tooBig(err) {
			writeSaaSError(w, codeInvalidRequest, "invalid request body")
		}
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if !tooBig(err) {
			writeSaaSError(w, codeInvalidRequest, "invalid request body")
		}
		return false
	}
	return true
}

func factionLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		writeSaaSError(w, codeInvalidRequest, "limit must be a positive integer")
		return 0, false
	}
	if n > max {
		n = max
	}
	return n, true
}

func factionCursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if raw == "" {
		return 0, true
	}
	id, ok := decodeFactionCursor(raw)
	if !ok {
		writeSaaSError(w, codeInvalidRequest, "invalid cursor")
		return 0, false
	}
	return id, true
}

// factionFailed maps a repository/service error to a fixed, safe response. Raw errors are
// logged, never returned.
func factionFailed(w http.ResponseWriter, what string, err error) {
	var invalid *factionhub.ValidationError
	switch {
	case errors.As(err, &invalid):
		writeSaaSError(w, codeInvalidRequest, "invalid faction input: "+strings.Join(invalid.Issues, "; "))
	case errors.Is(err, factionhub.ErrNotFound):
		writeSaaSError(w, codeNotFound, "not found")
	case errors.Is(err, factionhub.ErrForbidden):
		writeSaaSError(w, codeForbidden, "your faction role does not allow this")
	case errors.Is(err, factionhub.ErrLeaderProtected):
		writeSaaSError(w, codeForbidden, factionhub.ErrLeaderProtected.Error())
	case errors.Is(err, factionhub.ErrNoServer), errors.Is(err, factionhub.ErrInstallationInert),
		errors.Is(err, factionhub.ErrNameTaken), errors.Is(err, factionhub.ErrTagTaken),
		errors.Is(err, factionhub.ErrAlreadyInFaction), errors.Is(err, factionhub.ErrRecruitmentClosed),
		errors.Is(err, factionhub.ErrAlreadyApplied), errors.Is(err, factionhub.ErrNotPending),
		errors.Is(err, factionhub.ErrInvalidTransition):
		// The typed errors carry fixed, safe messages.
		writeSaaSError(w, codeConflict, err.Error())
	default:
		slog.Warn("component=saas_api", "msg", "faction "+what+" failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not "+what)
	}
}

// factionAudit records a safe audit event: identifiers only - never names, messages or
// any text a user wrote.
func factionAudit(event string, fr factionRequest, attrs ...any) {
	base := []any{"event", event, "organization_id", fr.orgID, "installation_id", fr.instID, "acting_user_id", fr.user.ID}
	slog.Info("component=saas_api", append(base, attrs...)...)
}

// buildFactionProfile assembles the public profile: the faction, its members (most
// senior first, capped) and the viewer's own standing. It never includes application data.
func (a *App) buildFactionProfile(ctx context.Context, fr factionRequest, f repository.HubFaction, members []repository.HubMember) factionProfileDTO {
	out := factionProfileDTO{factionSummaryDTO: toFactionSummary(f), InstallationID: f.InstallationID, GameServerID: f.GameServerID,
		Description: f.Description, Requirements: f.Settings, Officers: []factionMemberDTO{}, Members: []factionMemberDTO{},
		CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: f.UpdatedAt.UTC().Format(time.RFC3339)}
	var viewerRole string
	for _, m := range members {
		dto := toFactionMember(m)
		out.Members = append(out.Members, dto)
		switch m.RoleKey {
		case factionhub.RoleLeader:
			d := dto
			out.Leader = &d
		case factionhub.RoleOfficer:
			out.Officers = append(out.Officers, dto)
		}
		if m.User.ID == fr.user.ID {
			viewerRole = m.RoleKey
		}
	}
	if viewerRole == "" && len(members) < f.MemberCount { // viewer beyond the capped list
		viewerRole, _ = a.FactionHub.MemberRole(ctx, f.OrganizationID, f.InstallationID, f.ID, fr.user.ID)
	}
	if viewerRole != "" {
		out.Viewer.Role = &viewerRole
	}
	out.Viewer.CanEdit = factionhub.CanEditFaction(viewerRole)
	out.Viewer.CanManageApplications = factionhub.CanManageApplications(viewerRole)
	out.Viewer.ModerationView = a.orgViewer(ctx, fr.orgID, fr.user.ID)
	return out
}

func (a *App) loadFactionProfile(ctx context.Context, fr factionRequest, factionID int64) (factionProfileDTO, error) {
	f, err := a.FactionHub.Get(ctx, fr.orgID, fr.instID, factionID)
	if err != nil {
		return factionProfileDTO{}, err
	}
	members, _, err := a.FactionHub.Members(ctx, fr.orgID, fr.instID, factionID, defaultMembersLimit)
	if err != nil {
		return factionProfileDTO{}, err
	}
	return a.buildFactionProfile(ctx, fr, *f, members), nil
}

// --- directory / profile / me -----------------------------------------------------------------

// handleFactionDirectory is GET .../factions: the installation's factions, newest first, with
// member counts from one aggregate query. Filters: recruiting=true (OPEN only), q (name or
// tag, case-insensitive substring), limit (default 25, max 100), cursor.
func (a *App) handleFactionDirectory(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, defaultDirectoryLimit, maxDirectoryLimit)
	if !ok {
		return
	}
	after, ok := factionCursor(w, r)
	if !ok {
		return
	}
	search, good := factionhub.NormalizeSearch(r.URL.Query().Get("q"))
	if !good {
		writeSaaSError(w, codeInvalidRequest, "q is too long")
		return
	}
	rec := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("recruiting")))
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	items, more, err := a.FactionHub.Directory(ctx, fr.orgID, fr.instID, repository.HubDirectoryQuery{
		RecruitingOnly: rec == "true" || rec == "1", Search: search, Limit: limit, AfterID: after})
	if err != nil {
		factionFailed(w, "load factions", err)
		return
	}
	page := factionPage[factionSummaryDTO]{Items: make([]factionSummaryDTO, 0, len(items)), Limit: limit}
	for _, f := range items {
		page.Items = append(page.Items, toFactionSummary(f))
	}
	if more {
		page.NextCursor = encodeFactionCursor(items[len(items)-1].ID)
	}
	writeSaaSJSON(w, http.StatusOK, page)
}

// handleGetFaction is GET .../factions/{factionID}: the public profile.
func (a *App) handleGetFaction(w http.ResponseWriter, r *http.Request) {
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
	profile, err := a.loadFactionProfile(ctx, fr, factionID)
	if err != nil {
		factionFailed(w, "load faction", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, profile)
}

type myFactionResponse struct {
	Faction             *factionSummaryDTO      `json:"faction"`
	Membership          *factionMemberDTO       `json:"membership"`
	Role                *string                 `json:"role"`
	PendingApplications []factionApplicationDTO `json:"pendingApplications"`
}

// handleMyFaction is GET .../factions/me: the caller's faction (if any), their role and
// their pending applications - one call for the website to route on.
func (a *App) handleMyFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	my, err := a.FactionHub.MyFaction(ctx, fr.orgID, fr.instID, fr.user.ID)
	if err != nil {
		factionFailed(w, "load your faction", err)
		return
	}
	resp := myFactionResponse{PendingApplications: make([]factionApplicationDTO, 0, len(my.Pending))}
	if my.Faction != nil {
		s := toFactionSummary(*my.Faction)
		m := toFactionMember(*my.Member)
		role := my.Member.RoleKey
		resp.Faction, resp.Membership, resp.Role = &s, &m, &role
	}
	for _, p := range my.Pending {
		resp.PendingApplications = append(resp.PendingApplications, toFactionApplication(p, true))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// --- create / update --------------------------------------------------------------------------

type createFactionRequest struct {
	Name              string `json:"name"`
	Tag               string `json:"tag"`
	Description       string `json:"description"`
	RecruitmentStatus string `json:"recruitmentStatus"`
}

// handleCreateFaction is POST .../factions: the caller founds a faction and becomes its LEADER
// (faction + membership in one transaction). recruitmentStatus defaults to CLOSED.
func (a *App) handleCreateFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	key := rateLimitKey(r)
	if !enforceRateLimit(w, a.saasFactionCreateLimiter, key) || !enforceRateLimit(w, a.saasFactionCreateDayLimiter, key) {
		return
	}
	var req createFactionRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	in, err := normalizeFactionInput(req)
	if err != nil {
		factionFailed(w, "create faction", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	created, err := a.FactionHub.CreateFaction(ctx, fr.orgID, fr.instID, fr.user.ID, in)
	if err != nil {
		factionFailed(w, "create faction", err)
		return
	}
	factionAudit("faction_created", fr, "faction_id", created.ID)
	profile, err := a.loadFactionProfile(ctx, fr, created.ID)
	if err != nil {
		factionFailed(w, "load faction", err)
		return
	}
	writeSaaSJSON(w, http.StatusCreated, profile)
}

func normalizeFactionInput(req createFactionRequest) (repository.HubFactionInput, error) {
	name, err := factionhub.ValidateName(req.Name)
	if err != nil {
		return repository.HubFactionInput{}, err
	}
	tag, err := factionhub.ValidateTag(req.Tag)
	if err != nil {
		return repository.HubFactionInput{}, err
	}
	desc, err := factionhub.ValidateDescription(req.Description)
	if err != nil {
		return repository.HubFactionInput{}, err
	}
	status := strings.ToUpper(strings.TrimSpace(req.RecruitmentStatus))
	if status == "" {
		status = factionhub.RecruitmentClosed
	}
	if !factionhub.ValidRecruitmentStatus(status) {
		return repository.HubFactionInput{}, &factionhub.ValidationError{Issues: []string{"recruitmentStatus must be OPEN, INVITE_ONLY or CLOSED"}}
	}
	return repository.HubFactionInput{Name: name, Tag: tag, Description: desc, RecruitmentStatus: status}, nil
}

// updateFactionRequest: every field is optional (absent = unchanged). primaryColor /
// secondaryColor take "" to clear. requirements, when present, replaces the requirements as
// a whole. The organization, installation, server, slug and visual keys are not editable, and
// unknown keys (an image URL, an installation id) are rejected.
type updateFactionRequest struct {
	Name              *string              `json:"name"`
	Tag               *string              `json:"tag"`
	Description       *string              `json:"description"`
	RecruitmentStatus *string              `json:"recruitmentStatus"`
	PrimaryColor      *string              `json:"primaryColor"`
	SecondaryColor    *string              `json:"secondaryColor"`
	Requirements      *factionhub.Settings `json:"requirements"`
}

// handleUpdateFaction is PUT .../factions/{factionID}: LEADER only (decided by the faction
// role inside the transaction).
func (a *App) handleUpdateFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	var req updateFactionRequest
	if !decodeFactionBody(w, r, &req) {
		return
	}
	upd, err := normalizeFactionUpdate(req)
	if err != nil {
		factionFailed(w, "update faction", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	if _, err := a.FactionHub.UpdateFaction(ctx, fr.orgID, fr.instID, factionID, fr.user.ID, upd); err != nil {
		factionFailed(w, "update faction", err)
		return
	}
	factionAudit("faction_updated", fr, "faction_id", factionID)
	profile, err := a.loadFactionProfile(ctx, fr, factionID)
	if err != nil {
		factionFailed(w, "load faction", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, profile)
}

func normalizeFactionUpdate(req updateFactionRequest) (repository.HubFactionUpdate, error) {
	var out repository.HubFactionUpdate
	if req.Name != nil {
		v, err := factionhub.ValidateName(*req.Name)
		if err != nil {
			return out, err
		}
		out.Name = &v
	}
	if req.Tag != nil {
		v, err := factionhub.ValidateTag(*req.Tag)
		if err != nil {
			return out, err
		}
		out.Tag = &v
	}
	if req.Description != nil {
		v, err := factionhub.ValidateDescription(*req.Description)
		if err != nil {
			return out, err
		}
		out.Description = &v
	}
	if req.RecruitmentStatus != nil {
		v := strings.ToUpper(strings.TrimSpace(*req.RecruitmentStatus))
		if !factionhub.ValidRecruitmentStatus(v) {
			return out, &factionhub.ValidationError{Issues: []string{"recruitmentStatus must be OPEN, INVITE_ONLY or CLOSED"}}
		}
		out.RecruitmentStatus = &v
	}
	if req.PrimaryColor != nil {
		v, err := factionhub.ValidateColor("primaryColor", *req.PrimaryColor)
		if err != nil {
			return out, err
		}
		out.PrimaryColor = &v
	}
	if req.SecondaryColor != nil {
		v, err := factionhub.ValidateColor("secondaryColor", *req.SecondaryColor)
		if err != nil {
			return out, err
		}
		out.SecondaryColor = &v
	}
	if req.Requirements != nil {
		v, err := factionhub.ValidateSettings(*req.Requirements)
		if err != nil {
			return out, err
		}
		out.Settings = &v
	}
	return out, nil
}

// --- applications -----------------------------------------------------------------------------

// applyFactionRequest carries only a message. leader-defined questions (answers_json) are
// reserved in the schema but disabled in the API: an "answers" key is rejected as unknown.
type applyFactionRequest struct {
	Message string `json:"message"`
}

// handleApplyToFaction is POST .../factions/{factionID}/applications.
func (a *App) handleApplyToFaction(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	key := rateLimitKey(r)
	if !enforceRateLimit(w, a.saasFactionApplyLimiter, key) || !enforceRateLimit(w, a.saasFactionApplyDayLimiter, key) {
		return
	}
	var req applyFactionRequest
	if r.ContentLength != 0 { // an empty body is a valid, message-less application
		if !decodeFactionBody(w, r, &req) {
			return
		}
	}
	message, err := factionhub.ValidateMessage(req.Message)
	if err != nil {
		factionFailed(w, "apply", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	app, err := a.FactionHub.Apply(ctx, fr.orgID, fr.instID, factionID, fr.user.ID, message)
	if err != nil {
		factionFailed(w, "apply", err)
		return
	}
	factionAudit("faction_application_created", fr, "faction_id", factionID, "application_id", app.ID)
	writeSaaSJSON(w, http.StatusCreated, toFactionApplication(*app, false))
}

// handleListFactionApplications is GET .../factions/{factionID}/applications: LEADER/OFFICER
// of the faction, or an organization OWNER/ADMIN in a read-only moderation view. Optional
// status filter; keyset paging like the other lists.
func (a *App) handleListFactionApplications(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, defaultApplicationsLim, maxDirectoryLimit)
	if !ok {
		return
	}
	before, ok := factionCursor(w, r)
	if !ok {
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && !factionhub.ValidApplicationStatus(status) {
		writeSaaSError(w, codeInvalidRequest, "unknown status")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	access := repository.HubAccess{UserID: fr.user.ID, OrgViewer: a.orgViewer(ctx, fr.orgID, fr.user.ID)}
	items, more, err := a.FactionHub.ListApplications(ctx, fr.orgID, fr.instID, factionID, access, status, limit, before)
	if err != nil {
		factionFailed(w, "load applications", err)
		return
	}
	page := factionPage[factionApplicationDTO]{Items: make([]factionApplicationDTO, 0, len(items)), Limit: limit}
	for _, it := range items {
		page.Items = append(page.Items, toFactionApplication(it, false))
	}
	if more {
		page.NextCursor = encodeFactionCursor(items[len(items)-1].ID)
	}
	writeSaaSJSON(w, http.StatusOK, page)
}

// applicationRoute parses the shared {factionID}/{applicationID} path values.
func applicationRoute(w http.ResponseWriter, r *http.Request) (factionID, applicationID int64, ok bool) {
	if factionID, ok = pathInt64(w, r, "factionID"); !ok {
		return
	}
	applicationID, ok = pathInt64(w, r, "applicationID")
	return
}

type acceptApplicationResponse struct {
	Application factionApplicationDTO `json:"application"`
	Member      factionMemberDTO      `json:"member"`
}

// handleAcceptFactionApplication is POST .../applications/{applicationID}/accept (LEADER/OFFICER).
func (a *App) handleAcceptFactionApplication(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, applicationID, ok := applicationRoute(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	app, member, err := a.FactionHub.AcceptApplication(ctx, fr.orgID, fr.instID, factionID, applicationID, fr.user.ID)
	if err != nil {
		factionFailed(w, "accept application", err)
		return
	}
	factionAudit("faction_application_accepted", fr, "faction_id", factionID, "application_id", applicationID, "member_id", member.ID, "applicant_user_id", app.User.ID)
	writeSaaSJSON(w, http.StatusOK, acceptApplicationResponse{Application: toFactionApplication(*app, false), Member: toFactionMember(*member)})
}

// handleDenyFactionApplication is POST .../applications/{applicationID}/deny (LEADER/OFFICER).
func (a *App) handleDenyFactionApplication(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, applicationID, ok := applicationRoute(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	app, err := a.FactionHub.DenyApplication(ctx, fr.orgID, fr.instID, factionID, applicationID, fr.user.ID)
	if err != nil {
		factionFailed(w, "deny application", err)
		return
	}
	factionAudit("faction_application_denied", fr, "faction_id", factionID, "application_id", applicationID, "applicant_user_id", app.User.ID)
	writeSaaSJSON(w, http.StatusOK, toFactionApplication(*app, false))
}

// handleWithdrawFactionApplication is POST .../applications/{applicationID}/withdraw: the
// applicant withdraws their own pending application.
func (a *App) handleWithdrawFactionApplication(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, applicationID, ok := applicationRoute(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	app, err := a.FactionHub.WithdrawApplication(ctx, fr.orgID, fr.instID, factionID, applicationID, fr.user.ID)
	if err != nil {
		factionFailed(w, "withdraw application", err)
		return
	}
	factionAudit("faction_application_withdrawn", fr, "faction_id", factionID, "application_id", applicationID)
	writeSaaSJSON(w, http.StatusOK, toFactionApplication(*app, false))
}

// --- members ----------------------------------------------------------------------------------

type factionMembersResponse struct {
	Items []factionMemberDTO `json:"items"`
	Total int                `json:"total"`
	Limit int                `json:"limit"`
}

// handleListFactionMembers is GET .../factions/{factionID}/members: the public member list,
// most senior first (limit default 100, max 200; total is the real member count).
func (a *App) handleListFactionMembers(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, ok := pathInt64(w, r, "factionID")
	if !ok {
		return
	}
	limit, ok := factionLimit(w, r, defaultMembersLimit, maxMembersLimit)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	members, total, err := a.FactionHub.Members(ctx, fr.orgID, fr.instID, factionID, limit)
	if err != nil {
		factionFailed(w, "load members", err)
		return
	}
	resp := factionMembersResponse{Items: make([]factionMemberDTO, 0, len(members)), Total: total, Limit: limit}
	for _, m := range members {
		resp.Items = append(resp.Items, toFactionMember(m))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

func memberRoute(w http.ResponseWriter, r *http.Request) (factionID, memberID int64, ok bool) {
	if factionID, ok = pathInt64(w, r, "factionID"); !ok {
		return
	}
	memberID, ok = pathInt64(w, r, "memberID")
	return
}

// handlePromoteFactionMember is POST .../members/{memberID}/promote: MEMBER -> OFFICER (LEADER only).
func (a *App) handlePromoteFactionMember(w http.ResponseWriter, r *http.Request) {
	a.changeFactionRole(w, r, "faction_member_promoted", "promote member", a.FactionHub.PromoteMember)
}

// handleDemoteFactionMember is POST .../members/{memberID}/demote: OFFICER -> MEMBER (LEADER only).
func (a *App) handleDemoteFactionMember(w http.ResponseWriter, r *http.Request) {
	a.changeFactionRole(w, r, "faction_member_demoted", "demote member", a.FactionHub.DemoteMember)
}

type roleChangeFn func(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64) (*repository.HubMember, error)

func (a *App) changeFactionRole(w http.ResponseWriter, r *http.Request, event, what string, change roleChangeFn) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, memberID, ok := memberRoute(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	m, err := change(ctx, fr.orgID, fr.instID, factionID, memberID, fr.user.ID)
	if err != nil {
		factionFailed(w, what, err)
		return
	}
	factionAudit(event, fr, "faction_id", factionID, "member_id", memberID, "target_user_id", m.User.ID, "new_role", m.RoleKey)
	writeSaaSJSON(w, http.StatusOK, toFactionMember(*m))
}

type removedMemberResponse struct {
	Removed bool             `json:"removed"`
	Member  factionMemberDTO `json:"member"`
}

// handleRemoveFactionMember is DELETE .../members/{memberID}: the LEADER removes MEMBER or
// OFFICER, an OFFICER removes MEMBER; the LEADER is never removable in Phase 1.
func (a *App) handleRemoveFactionMember(w http.ResponseWriter, r *http.Request) {
	fr, ok := a.factionContext(w, r)
	if !ok {
		return
	}
	factionID, memberID, ok := memberRoute(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), factionTimeout)
	defer cancel()
	m, err := a.FactionHub.RemoveMember(ctx, fr.orgID, fr.instID, factionID, memberID, fr.user.ID)
	if err != nil {
		factionFailed(w, "remove member", err)
		return
	}
	factionAudit("faction_member_removed", fr, "faction_id", factionID, "member_id", memberID, "target_user_id", m.User.ID, "removed_role", m.RoleKey)
	writeSaaSJSON(w, http.StatusOK, removedMemberResponse{Removed: true, Member: toFactionMember(*m)})
}
