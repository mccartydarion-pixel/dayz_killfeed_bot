package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// --- shared DTOs (section 10) ---------------------------------------------

// NitradoServiceSummary is one discovered, supported DayZ console service.
// Platform is always the stable backend enum ("PLAYSTATION"|"XBOX") - the
// website renders its own friendly label (PlayStation/Xbox), never the
// reverse (see nitrado.ConsolePlatform.DisplayName, used only server-side
// for log/message text).
type NitradoServiceSummary struct {
	ServiceID int64  `json:"serviceId"`
	Name      string `json:"name"`
	Game      string `json:"game"`
	Platform  string `json:"platform"`
	Status    string `json:"status"`
}

// nitradoServiceStatus maps Nitrado's own status string to the small,
// stable vocabulary this API exposes.
func nitradoServiceStatus(status string) string {
	if strings.EqualFold(strings.TrimSpace(status), "active") {
		return "ONLINE"
	}
	return "OFFLINE"
}

// supportedNitradoDayZServices filters and classifies a raw Nitrado service
// list down to just the console platforms Champion supports (section 1):
// DayZ + PLAYSTATION, DayZ + XBOX. PC and unrelated games are dropped
// entirely, never exposed in a setup response (section 5).
func supportedNitradoDayZServices(services []nitrado.Service) []NitradoServiceSummary {
	out := make([]NitradoServiceSummary, 0, len(services))
	for _, svc := range services {
		platform := nitrado.ClassifyDayZPlatform(svc)
		if platform != nitrado.PlatformPlayStation && platform != nitrado.PlatformXbox {
			continue
		}
		serviceID, err := strconv.ParseInt(svc.ID, 10, 64)
		if err != nil {
			continue
		}
		name := svc.Details.Name
		if name == "" {
			name = svc.Details.ServerName
		}
		out = append(out, NitradoServiceSummary{
			ServiceID: serviceID,
			Name:      name,
			Game:      "DayZ",
			Platform:  string(platform),
			Status:    nitradoServiceStatus(svc.Status),
		})
	}
	return out
}

// nitradoClient builds a Nitrado client for token, going through
// saasNitradoClientFactory when a test has set one (so tests can redirect
// Nitrado REST calls to a local server exactly like discordGuildVerifier
// lets Discord-dependent handlers be tested with a fake - see
// saas_api_integration_test.go) and defaulting to the real
// nitrado.DefaultBaseURL otherwise.
func (a *App) nitradoClient(token string) *nitrado.Client {
	if a.saasNitradoClientFactory != nil {
		return a.saasNitradoClientFactory(token)
	}
	return nitrado.NewClient(nitrado.DefaultBaseURL, token, nil)
}

// nitradoClientFromEnvelope decrypts envelope's Nitrado credential and
// builds a live client from it - mirrors nitradoClientFromConnection's
// exact decrypt logic (app.go), adapted to repository.CredentialEnvelope
// (the organization-scoped envelope type - see
// saas_credentials_repository.go) instead of the guild-scoped
// repository.NitradoConnection that helper was written for. The decrypted
// token never leaves this function - callers only ever get back a
// *nitrado.Client.
func (a *App) nitradoClientFromEnvelope(envelope repository.CredentialEnvelope) (*nitrado.Client, error) {
	if a.CredentialCipher == nil {
		return nil, errors.New("credential encryption is not configured")
	}
	token, err := a.CredentialCipher.Decrypt(envelope.Ciphertext, envelope.Nonce, envelope.KeyVersion)
	if err != nil {
		return nil, fmt.Errorf("decrypt Nitrado credential: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("stored Nitrado credential is empty")
	}
	return a.nitradoClient(string(token)), nil
}

// --- connect Nitrado (section 4) ------------------------------------------

type nitradoConnectRequest struct {
	Token string `json:"token"`
}

// NitradoConnectResponse never includes the token (section 3/13).
type NitradoConnectResponse struct {
	Connected     bool `json:"connected"`
	ServicesFound int  `json:"servicesFound"`
}

// handleNitradoConnect is POST .../nitrado/connect (section 4). Only
// OWNER/ADMIN may connect a Nitrado account. Validates the token live
// against Nitrado before ever persisting anything - an invalid token is
// never stored.
func (a *App) handleNitradoConnect(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := a.requireOrganizationRole(w, r, organizationID, user.ID); !ok {
		return
	}
	if !enforceRateLimit(w, a.saasNitradoConnectLimiter, rateLimitKey(r)) {
		return
	}
	if a.SaaSCredentials == nil {
		writeSaaSError(w, codeInternalError, "credential service unavailable")
		return
	}
	if a.CredentialCipher == nil {
		writeSaaSError(w, codeInternalError, "credential encryption is not configured")
		return
	}

	var req nitradoConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeSaaSError(w, codeInvalidRequest, "token is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	client := a.nitradoClient(token)
	if err := client.AuthenticationCheck(ctx); err != nil {
		slog.Info("component=saas_api", "event", "saas_nitrado_connect", "organization_id", organizationID, "connected", false)
		writeSaaSError(w, codeNitradoUnavailable, "Nitrado rejected this token")
		return
	}
	services, err := client.GetServices(ctx)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado service discovery failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not discover Nitrado services")
		return
	}
	supported := supportedNitradoDayZServices(services)

	ciphertext, nonce, keyVersion, err := a.CredentialCipher.Encrypt([]byte(token))
	if err != nil {
		slog.Warn("component=saas_api", "msg", "credential encryption failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not store credential")
		return
	}
	// token itself is never referenced again below - only the envelope.
	if err := a.SaaSCredentials.UpsertForOrganizationOnly(ctx, repository.CredentialEnvelope{
		OrganizationID: organizationID,
		Ciphertext:     ciphertext,
		Nonce:          nonce,
		KeyVersion:     keyVersion,
		Status:         "ACTIVE",
	}); err != nil {
		slog.Warn("component=saas_api", "msg", "persist nitrado credential failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not store credential")
		return
	}

	if err := a.advanceSetupProgress(ctx, organizationID, 0, true, func(p *repository.InstallationSetupProgress) {
		p.NitradoCompleted = true
		if p.CurrentStep == "DISCORD" || p.CurrentStep == "NITRADO" || p.CurrentStep == "" {
			p.CurrentStep = "SERVER"
		}
	}); err != nil {
		slog.Warn("component=saas_api", "msg", "advance setup progress after nitrado connect failed", "err", err.Error())
	}

	slog.Info("component=saas_api", "event", "saas_nitrado_connect", "organization_id", organizationID, "connected", true, "services_found", len(supported))
	writeSaaSJSON(w, http.StatusOK, NitradoConnectResponse{Connected: true, ServicesFound: len(supported)})
}

// --- discover services (section 5) -----------------------------------------

// handleNitradoServices is GET .../nitrado/services (section 5). Any member
// may read. Returns only supported DayZ console services (section 1) -
// never an unfiltered raw Nitrado service list.
func (a *App) handleNitradoServices(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := a.requireOrganizationMember(w, r, organizationID, user.ID); !ok {
		return
	}
	if a.SaaSCredentials == nil {
		writeSaaSError(w, codeInternalError, "credential service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	client, errCode, errMsg := a.nitradoClientForOrganization(ctx, organizationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	services, err := client.GetServices(ctx)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado service discovery failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not discover Nitrado services")
		return
	}
	writeSaaSJSON(w, http.StatusOK, supportedNitradoDayZServices(services))
}

// nitradoClientForOrganization loads and decrypts organizationID's Nitrado
// credential, returning a ready-to-use client. Returns a non-empty error
// code/message (already safe to return to the caller) on any failure.
func (a *App) nitradoClientForOrganization(ctx context.Context, organizationID int64) (*nitrado.Client, string, string) {
	envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, organizationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get nitrado credential failed", "err", err.Error())
		return nil, codeInternalError, "could not load Nitrado credential"
	}
	if envelope == nil {
		return nil, codeNotFound, "Nitrado is not connected for this organization"
	}
	client, err := a.nitradoClientFromEnvelope(*envelope)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "build nitrado client failed", "err", err.Error())
		return nil, codeInternalError, "could not use stored Nitrado credential"
	}
	return client, "", ""
}

// --- select DayZ server (section 7) -----------------------------------------

type selectDayZServerRequest struct {
	ServiceID int64 `json:"serviceId"`
}

// DayZServerSelection is POST .../dayz-server's response.
type DayZServerSelection struct {
	ID          int64  `json:"id"`
	ServiceID   int64  `json:"serviceId"`
	DisplayName string `json:"displayName"`
	Game        string `json:"game"`
	Platform    string `json:"platform"`
	Status      string `json:"status"`
}

// SelectDayZServerResponse is POST .../dayz-server's actual response
// envelope. installationId is the RESOLVED installation - it can differ
// from the installationID in the request path when this exact Discord
// guild + DayZ server pair was already owned by a different, existing
// installation (reusedInstallation=true): the existing installation is
// authoritative, never duplicated or overwritten. The website should
// switch to installationId for any further setup calls.
type SelectDayZServerResponse struct {
	Server             DayZServerSelection `json:"server"`
	InstallationID     int64               `json:"installationId"`
	ReusedInstallation bool                `json:"reusedInstallation"`
}

// handleSelectDayZServer is POST .../installations/{installationID}/dayz-server
// (section 7). Only OWNER/ADMIN may select a server. Re-verifies the
// service against the live Nitrado account (never trusts a client-supplied
// platform/name) before persisting.
func (a *App) handleSelectDayZServer(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := a.requireOrganizationRole(w, r, organizationID, user.ID); !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil || a.SaaSServers == nil || a.SaaSCredentials == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	var req selectDayZServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ServiceID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "serviceId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// installation belongs to organization + resolves to a guild - reuses
	// the same chain handleVerifyInstallation already relies on.
	loaded, _, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	client, errCode, errMsg := a.nitradoClientForOrganization(ctx, organizationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}

	// Live re-verification: the service must currently belong to this
	// connected Nitrado account (section 7 - "verify service belongs to
	// connected customer account"), never trusted from an earlier listing.
	services, err := client.GetServices(ctx)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado service discovery failed", "err", err.Error())
		writeSaaSError(w, codeNitradoUnavailable, "could not verify Nitrado services")
		return
	}

	var matched *nitrado.Service
	for i := range services {
		id, parseErr := strconv.ParseInt(services[i].ID, 10, 64)
		if parseErr == nil && id == req.ServiceID {
			matched = &services[i]
			break
		}
	}
	if matched == nil {
		writeSaaSError(w, codeNotFound, "that Nitrado service was not found on the connected account")
		return
	}
	platform := nitrado.ClassifyDayZPlatform(*matched)
	if platform != nitrado.PlatformPlayStation && platform != nitrado.PlatformXbox {
		writeSaaSError(w, codeInvalidRequest, "that Nitrado service is not a supported DayZ console server")
		return
	}

	name := matched.Details.Name
	if name == "" {
		name = matched.Details.ServerName
	}
	server, err := a.SaaSServers.UpsertForInstallation(ctx, organizationID, loaded.mustGuildRowID(), repository.GameServer{
		Provider:          "NITRADO",
		ProviderServiceID: matched.ID,
		Game:              "DayZ",
		Platform:          string(platform),
		DisplayName:       name,
		Status:            nitradoServiceStatus(matched.Status),
		Active:            true,
	})
	if err != nil {
		slog.Warn("component=saas_api", "msg", "upsert dayz server failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not persist DayZ server")
		return
	}
	// UpsertForInstallation never overwrites a DIFFERENT organization's
	// existing claim (see its own doc comment) - a mismatch here means this
	// service's game_servers row already belongs to someone else. Never
	// silently proceed with it (section 6/10).
	if server.OrganizationID != nil && *server.OrganizationID != organizationID {
		writeSaaSError(w, codeConflict, "this DayZ server is already connected to a different organization")
		return
	}

	resolvedInstallationID, reused, err := a.resolveDayZServerInstallation(ctx, organizationID, installationID, loaded.DiscordGuildConnectionID, server.ID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "resolve dayz server installation failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not associate DayZ server with installation")
		return
	}

	slog.Info("component=saas_api", "event", "saas_dayz_installation_resolution",
		"requested_installation_id", installationID, "resolved_installation_id", resolvedInstallationID,
		"game_server_id", server.ID, "reused_existing", reused)

	// Setup-completion task, section 13: selecting a genuinely DIFFERENT
	// DayZ server on an already-READY installation is a critical config
	// change - permission verification was never run against this server's
	// context, so it can't stay READY on the strength of stale results.
	// Only applies to the direct (non-reused) path: a reused installation's
	// game_server_id never actually changes by definition of the reuse
	// match, so there's nothing to invalidate there.
	// The installation -> server mapping just changed, which changes which
	// installation's routes a server's publishers resolve to.
	a.ChannelRoutes.InvalidateAll()
	a.RouteSyncer.Trigger() // re-sync routed panels/leaderboard now
	a.BountyBoard.Trigger()  // re-reconcile the bounty board now

	criticalServerChange := resolvedInstallationID == installationID && loaded.Status == repository.InstallationReady &&
		loaded.GameServerID != nil && *loaded.GameServerID != server.ID

	if err := a.advanceSetupProgress(ctx, organizationID, resolvedInstallationID, false, func(p *repository.InstallationSetupProgress) {
		p.ServerSelected = true
		p.CurrentStep = "CHANNELS"
		if criticalServerChange {
			p.ValidationCompleted = false
		}
	}); err != nil {
		slog.Warn("component=saas_api", "msg", "advance setup progress after server selection failed", "err", err.Error())
	}
	if criticalServerChange {
		if err := a.SaaSInstallations.UpdateStatus(ctx, organizationID, resolvedInstallationID, repository.InstallationConfiguring); err != nil {
			slog.Warn("component=saas_api", "msg", "update installation status failed", "err", err.Error())
		}
	}

	if reused && resolvedInstallationID != installationID {
		// Best-effort cleanup of the redundant, still-empty installation
		// the customer's request originally targeted (section 7) - never
		// fails the response, and never touches an installation that has
		// any real progress/settings (DeleteIfEmpty's own query is the
		// safety check, not a judgment call made here).
		if deleted, delErr := a.SaaSInstallations.DeleteIfEmpty(ctx, organizationID, installationID); delErr != nil {
			slog.Warn("component=saas_api", "msg", "delete redundant empty installation failed", "err", delErr.Error())
		} else if deleted {
			slog.Info("component=saas_api", "event", "saas_dayz_installation_resolution", "action", "deleted_redundant_installation", "installation_id", installationID)
		}
	}

	writeSaaSJSON(w, http.StatusOK, SelectDayZServerResponse{
		Server: DayZServerSelection{
			ID:          server.ID,
			ServiceID:   req.ServiceID,
			DisplayName: server.DisplayName,
			Game:        server.Game,
			Platform:    server.Platform,
			Status:      server.Status,
		},
		InstallationID:     resolvedInstallationID,
		ReusedInstallation: reused,
	})
}

// resolveDayZServerInstallation implements the reuse flow (sections 1-4):
// if gameServerID is already associated with a DIFFERENT installation under
// the same guild connection, that installation is authoritative - return it
// instead of colliding with UNIQUE(discord_guild_connection_id,
// game_server_id). Otherwise, associate gameServerID with
// requestedInstallationID as normal. A genuine race (SetGameServer still
// hits ErrGameServerAlreadyAssigned despite the pre-check) is handled by
// re-resolving rather than surfacing a 500.
func (a *App) resolveDayZServerInstallation(ctx context.Context, organizationID, requestedInstallationID, discordGuildConnectionID, gameServerID int64) (resolvedInstallationID int64, reused bool, err error) {
	existing, err := a.SaaSInstallations.GetByGuildConnectionAndGameServer(ctx, organizationID, discordGuildConnectionID, gameServerID)
	if err != nil {
		return 0, false, err
	}
	if existing != nil && existing.ID != requestedInstallationID {
		return existing.ID, true, nil
	}

	if err := a.SaaSInstallations.SetGameServer(ctx, organizationID, requestedInstallationID, gameServerID); err != nil {
		if errors.Is(err, repository.ErrGameServerAlreadyAssigned) {
			// Lost a race: some other request claimed this pair between our
			// check above and this write. Re-resolve to whoever won it.
			again, findErr := a.SaaSInstallations.GetByGuildConnectionAndGameServer(ctx, organizationID, discordGuildConnectionID, gameServerID)
			if findErr != nil {
				return 0, false, findErr
			}
			if again != nil {
				return again.ID, again.ID != requestedInstallationID, nil
			}
		}
		return 0, false, err
	}
	return requestedInstallationID, false, nil
}

// --- validate selected DayZ server (section 12) -----------------------------

// DayZServerValidation is POST .../dayz-server/validate's response.
type DayZServerValidation struct {
	Reachable bool   `json:"reachable"`
	Supported bool   `json:"supported"`
	Platform  string `json:"platform,omitempty"`
	Message   string `json:"message"`
}

// handleValidateDayZServer is POST .../dayz-server/validate (section 12): a
// safe, read-only live check that the installation's selected DayZ server
// still exists and is still a supported console platform on the connected
// Nitrado account. Any member may run it.
func (a *App) handleValidateDayZServer(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := a.requireOrganizationMember(w, r, organizationID, user.ID); !ok {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSServers == nil || a.SaaSCredentials == nil {
		writeSaaSError(w, codeInternalError, "installation service unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	inst, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get installation failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load installation")
		return
	}
	if inst == nil {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	if inst.GameServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server has been selected for this installation yet")
		return
	}
	server, err := a.SaaSServers.GetScoped(ctx, organizationID, *inst.GameServerID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get dayz server failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load DayZ server")
		return
	}
	if server == nil {
		writeSaaSError(w, codeNotFound, "DayZ server not found")
		return
	}

	client, errCode, errMsg := a.nitradoClientForOrganization(ctx, organizationID)
	if errCode != "" {
		writeSaaSJSON(w, http.StatusOK, DayZServerValidation{Reachable: false, Supported: false, Message: errMsg})
		return
	}

	services, err := client.GetServices(ctx)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "nitrado service discovery failed", "err", err.Error())
		writeSaaSJSON(w, http.StatusOK, DayZServerValidation{Reachable: false, Supported: false, Message: "could not reach Nitrado to verify this server"})
		return
	}

	var matched *nitrado.Service
	for i := range services {
		if services[i].ID == server.ProviderServiceID {
			matched = &services[i]
			break
		}
	}
	if matched == nil {
		writeSaaSJSON(w, http.StatusOK, DayZServerValidation{Reachable: false, Supported: false, Message: "this DayZ server was not found on the connected Nitrado account"})
		return
	}
	platform := nitrado.ClassifyDayZPlatform(*matched)
	supported := platform == nitrado.PlatformPlayStation || platform == nitrado.PlatformXbox
	message := fmt.Sprintf("DayZ %s server connected.", platform.DisplayName())
	if !supported {
		message = "This Nitrado service is no longer a supported DayZ console server."
	}
	writeSaaSJSON(w, http.StatusOK, DayZServerValidation{
		Reachable: true,
		Supported: supported,
		Platform:  string(platform),
		Message:   message,
	})
}

// --- setup progress helper --------------------------------------------------

// advanceSetupProgress loads installationID's current setup progress,
// applies mutate, validates the merged result against
// validateSetupProgressOrder (the same server-side ordering guard
// handleUpdateSetupProgress enforces - section 11 "do not skip backend
// ordering rules"), and persists only if valid. When
// orgScopedToAllInstallations is true, installationID is ignored and every
// installation belonging to organizationID is advanced instead - the
// Nitrado connection is organization-scoped (section 4's route has no
// installation in its path), so a successful connect advances setup
// progress for every installation still mid-setup under that organization.
func (a *App) advanceSetupProgress(ctx context.Context, organizationID, installationID int64, orgScopedToAllInstallations bool, mutate func(*repository.InstallationSetupProgress)) error {
	if a.SaaSInstallations == nil {
		return nil
	}
	targets := []int64{installationID}
	if orgScopedToAllInstallations {
		installs, err := a.SaaSInstallations.ListByOrganization(ctx, organizationID)
		if err != nil {
			return fmt.Errorf("list installations: %w", err)
		}
		targets = targets[:0]
		for _, inst := range installs {
			targets = append(targets, inst.ID)
		}
	}

	var firstErr error
	for _, id := range targets {
		current, err := a.SaaSInstallations.GetSetupProgress(ctx, organizationID, id)
		if err != nil || current == nil {
			if err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		merged := *current
		mutate(&merged)
		if !validateSetupProgressOrder(merged) {
			// A mutation that would violate step order is silently skipped
			// for this installation rather than failing the whole
			// operation - the credential/server persistence above already
			// succeeded and must not be rolled back over a progress-display
			// nicety.
			continue
		}
		if err := a.SaaSInstallations.UpdateSetupProgress(ctx, organizationID, id, merged); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
