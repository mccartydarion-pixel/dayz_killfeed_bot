package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// discordGuildVerifier is the minimal Discord capability the SaaS Discord
// handlers depend on - satisfied by *discord.Client in production
// (registerSaaSAPI wires it up). Depending on this narrow interface rather
// than the concrete client lets tests exercise the full
// auth/membership/role gate chain with a fake, since the real Verify makes a
// live Discord API call that cannot run in a test environment.
type discordGuildVerifier interface {
	Verify(guildID, channelID string) discord.Verification
	// HasGuildCached is a zero-network-call membership check (local gateway
	// state cache only - see discord.Client.HasGuildCached). Used for
	// per-candidate eligibility listing, where calling Verify once per
	// guild would mean one live Discord REST round-trip per candidate -
	// fine for a single selected guild (handleConnectDiscordGuild,
	// handleVerifyInstallation), but not for a loop over every guild the
	// user's Discord OAuth session returned.
	HasGuildCached(guildID string) bool
}

// --- eligible guilds (section 9) ----------------------------------------

// discordGuildCandidate is one guild the website's own Discord OAuth
// exchange (Auth.js, "identify guilds" scope) already told it the acting
// user belongs to, including Discord's own per-guild permission bitfield
// for that user. This is the contract this task calls for: the Go backend
// cannot determine a user's permissions in a guild the bot isn't in, so the
// website supplies OAuth-verified candidates and the Go API independently
// re-derives eligibility from the permission bitfield (never trusting a
// pre-computed "eligible" boolean from the browser/website) and cross-checks
// bot presence itself.
type discordGuildCandidate struct {
	DiscordGuildID string `json:"discordGuildId"`
	GuildName      string `json:"guildName"`
	GuildIcon      string `json:"guildIcon"`
	// Permissions is the raw Discord permission bitfield for this user in
	// this guild, as a decimal string - exactly the shape Discord's own API
	// returns it in (large enough to exceed safe JS integer range).
	Permissions string `json:"permissions"`
}

type eligibleGuildsRequest struct {
	Guilds []discordGuildCandidate `json:"guilds"`
}

// DiscordGuildSummary is one candidate guild's eligibility result.
type DiscordGuildSummary struct {
	DiscordGuildID string `json:"discordGuildId"`
	GuildName      string `json:"guildName,omitempty"`
	GuildIcon      string `json:"guildIcon,omitempty"`
	Eligible       bool   `json:"eligible"`
	BotInstalled   bool   `json:"botInstalled"`
}

// requiredGuildPermissions is ADMINISTRATOR or MANAGE_GUILD (section 9).
const requiredGuildPermissions = discordgo.PermissionAdministrator | discordgo.PermissionManageGuild

// handleEligibleGuilds is POST .../discord/guilds/eligible (section 9): the
// website submits OAuth-verified candidates, the Go API re-derives
// eligibility from each candidate's permission bitfield itself and
// cross-checks bot presence against the bot's local gateway state cache -
// never trusting bot guild membership alone as a stand-in for user
// permissions (a guild the bot happens to already be in says nothing about
// whether THIS user can administer it).
//
// This must stay a zero-network-call loop: a website OAuth session can list
// dozens of guilds for a single user, and calling the live, REST-backed
// Verify once per candidate turned this into N sequential Discord API round
// trips - the exact cause of the 12s website timeout this was fixed for.
// HasGuildCached is a pure in-memory lookup instead, so this handler's cost
// no longer scales with how many servers the caller happens to be in. The
// final, single selected guild is still authoritatively verified live by
// handleConnectDiscordGuild and handleVerifyInstallation - this endpoint
// only ever produces a candidate list, never persists anything.
func (a *App) handleEligibleGuilds(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
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

	var req eligibleGuildsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}

	out := make([]DiscordGuildSummary, 0, len(req.Guilds))
	for _, cand := range req.Guilds {
		guildID := strings.TrimSpace(cand.DiscordGuildID)
		if guildID == "" {
			continue
		}
		perms, err := strconv.ParseInt(strings.TrimSpace(cand.Permissions), 10, 64)
		if err != nil {
			// An unparsable permission field cannot be trusted as eligible.
			perms = 0
		}
		eligible := perms&requiredGuildPermissions != 0
		botInstalled := false
		if a.saasDiscordVerifier != nil {
			botInstalled = a.saasDiscordVerifier.HasGuildCached(guildID)
		}
		out = append(out, DiscordGuildSummary{
			DiscordGuildID: guildID,
			GuildName:      strings.TrimSpace(cand.GuildName),
			GuildIcon:      strings.TrimSpace(cand.GuildIcon),
			Eligible:       eligible,
			BotInstalled:   botInstalled,
		})
	}

	// Safe completion diagnostic: candidate count and duration only - never
	// guild IDs, permission bitfields, tokens, or any request body/header
	// content (section 8).
	slog.Info("component=saas_api", "event", "saas_eligible_guilds_complete",
		"candidate_count", len(out), "duration_ms", time.Since(start).Milliseconds())
	writeSaaSJSON(w, http.StatusOK, out)
}

// --- guild connection (section 10) --------------------------------------

type connectGuildRequest struct {
	DiscordGuildID string `json:"discordGuildId"`
	GuildName      string `json:"guildName"`
	GuildIcon      string `json:"guildIcon"`
	// Permissions re-proves eligibility at the moment of connecting, exactly
	// like handleEligibleGuilds - a guild that was eligible when listed
	// could have had its permissions changed since, so this is re-verified
	// here rather than trusted from an earlier response.
	Permissions string `json:"permissions"`
}

// handleConnectDiscordGuild is POST .../discord/connection (section 10):
// persists a verified Discord guild selection. Requires OWNER/ADMIN,
// re-verifies eligibility from the submitted permission bitfield, and
// rejects (CONFLICT) a guild already claimed by a DIFFERENT organization
// rather than silently reassigning it.
func (a *App) handleConnectDiscordGuild(w http.ResponseWriter, r *http.Request) {
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
	if a.Guilds == nil || a.SaaSGuildConnections == nil {
		writeSaaSError(w, codeInternalError, "guild connection service unavailable")
		return
	}

	var req connectGuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	discordGuildID := strings.TrimSpace(req.DiscordGuildID)
	if discordGuildID == "" {
		writeSaaSError(w, codeInvalidRequest, "discordGuildId is required")
		return
	}
	perms, permErr := strconv.ParseInt(strings.TrimSpace(req.Permissions), 10, 64)
	if permErr != nil || perms&requiredGuildPermissions == 0 {
		writeSaaSError(w, codeForbidden, "this guild selection was not verified as eligible")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	botInstalled := false
	if a.saasDiscordVerifier != nil {
		botInstalled = a.saasDiscordVerifier.Verify(discordGuildID, "").GuildFound
	} else {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	guildRecord, guildRowID, err := a.Guilds.GetGuild(ctx, discordGuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "guild lookup failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve guild")
		return
	}
	if guildRecord == nil {
		// First time Champion has ever seen this guild - create the minimal
		// operational row so the connection has something to reference.
		// This does not touch any channel/Nitrado configuration - just the
		// bare discord_guild_id row UpsertGuild already supports creating
		// incrementally.
		guildRowID, err = a.Guilds.UpsertGuild(ctx, repository.GuildRecord{DiscordGuildID: discordGuildID})
		if err != nil {
			slog.Warn("component=saas_api", "msg", "create guild row failed", "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not create guild record")
			return
		}
	}

	if existing, err := a.SaaSGuildConnections.GetByGuildID(ctx, guildRowID); err != nil {
		slog.Warn("component=saas_api", "msg", "guild connection conflict check failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not verify guild connection")
		return
	} else if existing != nil && existing.OrganizationID != organizationID {
		writeSaaSError(w, codeConflict, "this Discord server is already connected to a different organization")
		return
	}

	conn, err := a.SaaSGuildConnections.Upsert(ctx, repository.DiscordGuildConnection{
		OrganizationID:      organizationID,
		GuildID:             guildRowID,
		GuildName:           strings.TrimSpace(req.GuildName),
		GuildIcon:           strings.TrimSpace(req.GuildIcon),
		BotInstalled:        botInstalled,
		PermissionsVerified: true,
	})
	if err != nil {
		slog.Warn("component=saas_api", "msg", "upsert guild connection failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not persist guild connection")
		return
	}
	writeSaaSJSON(w, http.StatusOK, toDiscordGuildConnectionSummary(*conn))
}

// --- bot installation verification (section 11) --------------------------

// DiscordVerificationResult is POST .../discord/verify-installation's
// response.
type DiscordVerificationResult struct {
	Installed      bool   `json:"installed"`
	GuildReachable bool   `json:"guildReachable"`
	VerifiedAt     string `json:"verifiedAt"`
}

// handleVerifyInstallation is POST
// .../installations/{installationID}/discord/verify-installation (section
// 11): verifies bot presence using the live Discord session, never trusting
// a redirect/query-string success. On success, transitions a NOT_STARTED
// installation to DISCORD_CONNECTED (section 13) - never further, and never
// regresses an installation already past that point.
func (a *App) handleVerifyInstallation(w http.ResponseWriter, r *http.Request) {
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
	if !enforceRateLimit(w, a.saasDiscordVerifyLimiter, rateLimitKey(r)) {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "verification service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	inst, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		slog.Info("component=saas_api", "event", "saas_install_verify", "installation_id", installationID, "guild_resolved", false, "installed", false)
		writeSaaSError(w, errCode, errMsg)
		return
	}

	verification := a.saasDiscordVerifier.Verify(discordGuildID, "")
	now := time.Now()
	result := DiscordVerificationResult{
		Installed:      verification.GuildFound,
		GuildReachable: verification.GuildFound,
		VerifiedAt:     now.UTC().Format(time.RFC3339),
	}
	slog.Info("component=saas_api", "event", "saas_install_verify", "installation_id", installationID, "guild_resolved", true, "installed", result.Installed)

	// Keep the connection's bot_installed flag in sync with this live check
	// (a narrow, targeted update - never Upsert, which would also overwrite
	// guild_name/guild_icon/permissions_verified back to empty/false since
	// those aren't set on this refresh path).
	if err := a.SaaSGuildConnections.UpdateBotInstalled(ctx, organizationID, inst.mustGuildRowID(), verification.GuildFound); err != nil {
		slog.Warn("component=saas_api", "msg", "update bot_installed flag failed", "err", err.Error())
	}

	if verification.GuildFound && inst.Status == repository.InstallationNotStarted {
		if err := a.SaaSInstallations.UpdateStatus(ctx, organizationID, installationID, repository.InstallationDiscordConnected); err != nil {
			slog.Warn("component=saas_api", "msg", "update installation status failed", "err", err.Error())
		}
	}

	writeSaaSJSON(w, http.StatusOK, result)
}

// --- permission verification (section 12) ---------------------------------

type verifyPermissionsRequest struct {
	ChannelID string `json:"channelId"`
}

// CapabilityCheck is one bot capability's verification result.
type CapabilityCheck struct {
	Capability string `json:"capability"`
	Result     string `json:"result"` // PASS | WARNING | FAIL
}

// PermissionVerificationResult is POST
// .../installations/{installationID}/discord/verify-permissions's response.
type PermissionVerificationResult struct {
	Capabilities []CapabilityCheck `json:"capabilities"`
	OverallPass  bool              `json:"overallPass"`
}

const (
	resultPass    = "PASS"
	resultWarning = "WARNING"
	resultFail    = "FAIL"
)

// blockingCapabilities cannot function at all if missing - a killfeed
// cannot post without them. embedCapabilities degrade the presentation but
// don't block posting entirely, so a missing one is WARNING, not FAIL. This
// split is a Champion product decision (not something Discord itself
// defines), documented in docs/SAAS_HTTP_API.md.
var blockingCapabilities = map[string]bool{"View Channel": true, "Send Messages": true}

// requiredCapabilities is the fixed, documented order every
// PermissionVerificationResult reports in (section 12).
var requiredCapabilities = []string{"View Channel", "Send Messages", "Embed Links", "Read Message History"}

// mapPermissionVerification is the pure PASS/WARNING/FAIL mapping (kept
// separate from the HTTP handler so it's directly unit-testable without a
// live Discord session or database - see saas_api_test.go). An unreachable
// guild/channel fails every capability outright: "do not trust
// redirect/query-string success" (section 11) extends to never guessing a
// capability passed just because we couldn't check it.
func mapPermissionVerification(v discord.Verification) PermissionVerificationResult {
	if !v.GuildFound || !v.ChannelFound {
		capabilities := make([]CapabilityCheck, 0, len(requiredCapabilities))
		for _, name := range requiredCapabilities {
			capabilities = append(capabilities, CapabilityCheck{Capability: name, Result: resultFail})
		}
		return PermissionVerificationResult{Capabilities: capabilities, OverallPass: false}
	}

	missing := make(map[string]bool, len(v.Missing))
	for _, m := range v.Missing {
		missing[m] = true
	}

	overallPass := true
	capabilities := make([]CapabilityCheck, 0, len(requiredCapabilities))
	for _, name := range requiredCapabilities {
		result := resultPass
		if missing[name] {
			if blockingCapabilities[name] {
				result = resultFail
				overallPass = false
			} else {
				result = resultWarning
			}
		}
		capabilities = append(capabilities, CapabilityCheck{Capability: name, Result: result})
	}
	return PermissionVerificationResult{Capabilities: capabilities, OverallPass: overallPass}
}

// handleVerifyPermissions is POST
// .../installations/{installationID}/discord/verify-permissions (section
// 12): checks the bot's actual channel permissions (never raw Discord
// permission bitfields in the response - only the mapped PASS/WARNING/FAIL
// result per capability).
func (a *App) handleVerifyPermissions(w http.ResponseWriter, r *http.Request) {
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
	if !enforceRateLimit(w, a.saasDiscordVerifyLimiter, rateLimitKey(r)) {
		return
	}
	if a.SaaSInstallations == nil || a.SaaSGuildConnections == nil || a.Guilds == nil {
		writeSaaSError(w, codeInternalError, "verification service unavailable")
		return
	}
	if a.saasDiscordVerifier == nil {
		writeSaaSError(w, codeDiscordUnavailable, "Discord bot session is unavailable")
		return
	}

	var req verifyPermissionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return
	}
	channelID := strings.TrimSpace(req.ChannelID)
	if channelID == "" {
		writeSaaSError(w, codeInvalidRequest, "channelId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	inst, discordGuildID, errCode, errMsg := a.loadInstallationGuildSnowflake(ctx, organizationID, installationID)
	if errCode != "" {
		writeSaaSError(w, errCode, errMsg)
		return
	}
	if inst.Status == repository.InstallationNotStarted {
		writeSaaSError(w, codeInstallationNotVerified, "connect Discord before verifying permissions")
		return
	}

	verification := a.saasDiscordVerifier.Verify(discordGuildID, channelID)
	writeSaaSJSON(w, http.StatusOK, mapPermissionVerification(verification))
}

// loadInstallationGuildSnowflake resolves installationID (org-scoped) to its
// live Discord guild snowflake, walking installation -> guild connection ->
// guilds row. Returns a non-empty error code/message (already safe to
// return to the caller) on any failure.
func (a *App) loadInstallationGuildSnowflake(ctx context.Context, organizationID, installationID int64) (*loadedInstallation, string, string, string) {
	inst, err := a.SaaSInstallations.GetScoped(ctx, organizationID, installationID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get installation failed", "err", err.Error())
		return nil, "", codeInternalError, "could not load installation"
	}
	if inst == nil {
		return nil, "", codeNotFound, "installation not found"
	}
	conn, err := a.SaaSGuildConnections.GetScoped(ctx, organizationID, inst.DiscordGuildConnectionID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get guild connection failed", "err", err.Error())
		return nil, "", codeInternalError, "could not load guild connection"
	}
	if conn == nil {
		return nil, "", codeNotFound, "guild connection not found"
	}
	guildRecord, err := a.Guilds.GetGuildByID(ctx, conn.GuildID)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "get guild failed", "err", err.Error())
		return nil, "", codeInternalError, "could not load guild"
	}
	if guildRecord == nil {
		return nil, "", codeNotFound, "guild not found"
	}
	return &loadedInstallation{Installation: *inst, guildRowID: conn.GuildID}, guildRecord.DiscordGuildID, "", ""
}

// loadedInstallation bundles the installation with the internal guilds.id it
// resolved through, so callers that need to re-touch the guild connection
// (e.g. refreshing bot_installed) don't have to look it up a second time.
type loadedInstallation struct {
	repository.Installation
	guildRowID int64
}

func (l *loadedInstallation) mustGuildRowID() int64 { return l.guildRowID }
