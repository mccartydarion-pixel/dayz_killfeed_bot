package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// OrganizationSummary is the customer-safe view of an organizations row,
// with the acting user's own role folded in (never another member's).
type OrganizationSummary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Role string `json:"role"`
}

func toOrganizationSummary(o repository.Organization, role string) OrganizationSummary {
	return OrganizationSummary{ID: o.ID, Name: o.Name, Slug: o.Slug, Role: role}
}

// handleListOrganizations is GET /api/saas/organizations (section 5):
// every organization the acting user belongs to, each with their role in it.
func (a *App) handleListOrganizations(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	if a.SaaSOrganizations == nil {
		writeSaaSError(w, codeInternalError, "organization directory unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	orgs, err := a.SaaSOrganizations.ListForUser(ctx, user.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list organizations failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not list organizations")
		return
	}

	out := make([]OrganizationSummary, 0, len(orgs))
	for _, o := range orgs {
		role, member, roleErr := a.SaaSOrganizations.VerifyMembership(ctx, o.ID, user.ID)
		if roleErr != nil || !member {
			// ListForUser already joined on organization_members, so this
			// should never miss - if it somehow does, skip rather than
			// report a role we can't actually confirm.
			continue
		}
		out = append(out, toOrganizationSummary(o, role))
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

type createOrganizationRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// handleCreateOrganization is POST /api/saas/organizations (section 5):
// creates the organization and the acting user's OWNER membership
// atomically (OrganizationRepository.Create already does this in one
// transaction - see saas_organizations_repository.go).
func (a *App) handleCreateOrganization(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	if !enforceRateLimit(w, a.saasOrgCreateLimiter, rateLimitKey(r)) {
		return
	}
	if a.SaaSOrganizations == nil || a.SaaSSubscriptions == nil {
		writeSaaSError(w, codeInternalError, "organization directory unavailable")
		return
	}

	var req createOrganizationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Slug = strings.TrimSpace(strings.ToLower(req.Slug))
	if req.Name == "" || req.Slug == "" {
		writeSaaSError(w, codeInvalidRequest, "name and slug are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	org, err := a.SaaSOrganizations.Create(ctx, req.Name, req.Slug, user.ID)
	if err != nil {
		if err == repository.ErrDuplicate {
			writeSaaSError(w, codeConflict, "that organization slug is already taken")
			return
		}
		slog.Warn("component=saas_api", "msg", "create organization failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not create organization")
		return
	}
	// Every organization starts on a 14-day trial - GetForOrganization never
	// has to special-case "no subscription yet" for a freshly created org.
	if _, err := a.SaaSSubscriptions.EnsureTrial(ctx, org.ID, time.Now().Add(14*24*time.Hour)); err != nil {
		slog.Warn("component=saas_api", "msg", "ensure trial subscription failed", "err", err.Error())
		// Non-fatal to the caller: the organization itself was created
		// successfully. A missing subscription row is self-healing (the
		// dashboard/GetForOrganization call can retry EnsureTrial).
	}

	writeSaaSJSON(w, http.StatusCreated, toOrganizationSummary(*org, repository.RoleOwner))
}

// handleGetOrganization is GET /api/saas/organizations/{organizationID}
// (section 5): requires membership, never exposes an organization the
// acting user doesn't belong to.
func (a *App) handleGetOrganization(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	role, ok := a.requireOrganizationMember(w, r, organizationID, user.ID)
	if !ok {
		return
	}

	org, err := a.getOrganizationRow(r.Context(), organizationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get organization failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load organization")
		return
	}
	if org == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toOrganizationSummary(*org, role))
}

// getOrganizationRow fetches one organization by ID via ListForUser's
// underlying query pattern is membership-shaped, so for a single-row lookup
// (already membership-checked by the caller) we query organizations
// directly. Kept here rather than in the repository package since it's an
// unscoped-by-design internal helper only ever called after
// requireOrganizationMember has already authorized the caller.
func (a *App) getOrganizationRow(ctx context.Context, organizationID int64) (*repository.Organization, error) {
	if a.SaaSOrganizations == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return a.SaaSOrganizations.GetByID(ctx, organizationID)
}

// --- dashboard (section 6) ----------------------------------------------

type SubscriptionSummary struct {
	Plan          string  `json:"plan"`
	Status        string  `json:"status"`
	TrialEndsAt   *string `json:"trialEndsAt,omitempty"`
	CurrentPeriod *string `json:"currentPeriodEnd,omitempty"`
}

type SetupProgressSummary struct {
	CurrentStep         string  `json:"currentStep"`
	DiscordCompleted    bool    `json:"discordCompleted"`
	NitradoCompleted    bool    `json:"nitradoCompleted"`
	ServerSelected      bool    `json:"serverSelected"`
	ChannelsCompleted   bool    `json:"channelsCompleted"`
	ValidationCompleted bool    `json:"validationCompleted"`
	CompletedAt         *string `json:"completedAt,omitempty"`
}

func toSetupProgressSummary(p repository.InstallationSetupProgress) SetupProgressSummary {
	return SetupProgressSummary{
		CurrentStep:         p.CurrentStep,
		DiscordCompleted:    p.DiscordCompleted,
		NitradoCompleted:    p.NitradoCompleted,
		ServerSelected:      p.ServerSelected,
		ChannelsCompleted:   p.ChannelsCompleted,
		ValidationCompleted: p.ValidationCompleted,
		CompletedAt:         nullableTimeStr(p.CompletedAt),
	}
}

type DiscordGuildConnectionSummary struct {
	ID                  int64  `json:"id"`
	GuildName           string `json:"guildName,omitempty"`
	GuildIcon           string `json:"guildIcon,omitempty"`
	BotInstalled        bool   `json:"botInstalled"`
	PermissionsVerified bool   `json:"permissionsVerified"`
	ConnectedAt         string `json:"connectedAt"`
}

func toDiscordGuildConnectionSummary(c repository.DiscordGuildConnection) DiscordGuildConnectionSummary {
	return DiscordGuildConnectionSummary{
		ID:                  c.ID,
		GuildName:           c.GuildName,
		GuildIcon:           c.GuildIcon,
		BotInstalled:        c.BotInstalled,
		PermissionsVerified: c.PermissionsVerified,
		ConnectedAt:         c.ConnectedAt.UTC().Format(time.RFC3339),
	}
}

type DayZServerSummary struct {
	ID          int64  `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	Game        string `json:"game"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`
}

func toDayZServerSummary(s repository.GameServer) DayZServerSummary {
	return DayZServerSummary{ID: s.ID, DisplayName: s.DisplayName, Game: s.Game, Platform: s.Platform, Status: s.Status}
}

// InstallationSummary is the customer-safe view of one installation and its
// child state, folded into a single response so the dashboard needs one
// round trip per installation instead of four.
type InstallationSummary struct {
	ID                int64                          `json:"id"`
	Status            string                         `json:"status"`
	Plan              string                         `json:"plan,omitempty"`
	Health            string                         `json:"health"`
	SetupProgress     *SetupProgressSummary          `json:"setupProgress,omitempty"`
	DiscordConnection *DiscordGuildConnectionSummary `json:"discordConnection,omitempty"`
	DayZServer        *DayZServerSummary             `json:"dayzServer,omitempty"`
	CreatedAt         string                         `json:"createdAt"`
	SetupCompletedAt  *string                        `json:"setupCompletedAt,omitempty"`
	LastHealthCheckAt *string                        `json:"lastHealthCheckAt,omitempty"`
}

// installationHealth derives a high-level, customer-facing health state
// from status alone - never exposes internal DB structure, just the same
// status vocabulary the website already needs to understand.
func installationHealth(status string) string {
	switch status {
	case repository.InstallationReady:
		return "HEALTHY"
	case repository.InstallationDegraded:
		return "DEGRADED"
	case repository.InstallationDisconnected, repository.InstallationSuspended:
		return "OFFLINE"
	default:
		return "SETTING_UP"
	}
}

// buildInstallationSummary assembles one InstallationSummary, reading its
// child rows (setup progress always exists; Discord connection and DayZ
// server may not yet). organizationID re-scopes every child lookup, even
// though inst was already fetched org-scoped - defense in depth, and cheap.
func (a *App) buildInstallationSummary(ctx context.Context, organizationID int64, inst repository.Installation) InstallationSummary {
	out := InstallationSummary{
		ID:                inst.ID,
		Status:            inst.Status,
		Plan:              inst.Plan,
		Health:            installationHealth(inst.Status),
		CreatedAt:         inst.CreatedAt.UTC().Format(time.RFC3339),
		SetupCompletedAt:  nullableTimeStr(inst.SetupCompletedAt),
		LastHealthCheckAt: nullableTimeStr(inst.LastHealthCheckAt),
	}
	if a.SaaSInstallations != nil {
		if progress, err := a.SaaSInstallations.GetSetupProgress(ctx, organizationID, inst.ID); err == nil && progress != nil {
			summary := toSetupProgressSummary(*progress)
			out.SetupProgress = &summary
		}
	}
	if a.SaaSGuildConnections != nil {
		if conn, err := a.SaaSGuildConnections.GetScoped(ctx, organizationID, inst.DiscordGuildConnectionID); err == nil && conn != nil {
			summary := toDiscordGuildConnectionSummary(*conn)
			out.DiscordConnection = &summary
		}
	}
	if inst.GameServerID != nil && a.SaaSServers != nil {
		if srv, err := a.SaaSServers.GetScoped(ctx, organizationID, *inst.GameServerID); err == nil && srv != nil {
			summary := toDayZServerSummary(*srv)
			out.DayZServer = &summary
		}
	}
	return out
}

// DashboardSummary is GET .../dashboard's response (section 6): everything
// the customer dashboard's landing page needs in one call. No secrets, no
// raw DB structure - every field is one of the DTOs above.
type DashboardSummary struct {
	Organization  OrganizationSummary   `json:"organization"`
	Subscription  *SubscriptionSummary  `json:"subscription,omitempty"`
	Installations []InstallationSummary `json:"installations"`
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	organizationID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return
	}
	role, ok := a.requireOrganizationMember(w, r, organizationID, user.ID)
	if !ok {
		return
	}
	if a.SaaSOrganizations == nil || a.SaaSInstallations == nil {
		writeSaaSError(w, codeInternalError, "dashboard data unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	org, err := a.getOrganizationRow(ctx, organizationID)
	if err != nil || org == nil {
		writeSaaSError(w, codeNotFound, "organization not found")
		return
	}

	resp := DashboardSummary{Organization: toOrganizationSummary(*org, role)}

	if a.SaaSSubscriptions != nil {
		if sub, subErr := a.SaaSSubscriptions.GetForOrganization(ctx, organizationID); subErr == nil && sub != nil {
			resp.Subscription = &SubscriptionSummary{
				Plan:          sub.Plan,
				Status:        sub.Status,
				TrialEndsAt:   nullableTimeStr(sub.TrialEndsAt),
				CurrentPeriod: nullableTimeStr(sub.CurrentPeriodEnd),
			}
		}
	}

	installs, err := a.SaaSInstallations.ListByOrganization(ctx, organizationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "list installations for dashboard failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load installations")
		return
	}
	resp.Installations = make([]InstallationSummary, 0, len(installs))
	for _, inst := range installs {
		resp.Installations = append(resp.Installations, a.buildInstallationSummary(ctx, organizationID, inst))
	}

	writeSaaSJSON(w, http.StatusOK, resp)
}
