package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Player API (docs/PLAYER_API.md, Champion Access Model Phase 2 Part A/B). A Player is a
// tracked DayZ identity, never an organization member - these routes take no organizationID and
// never check organization membership, unlike every other /api/saas route. Authorization instead
// comes entirely from internal/repository.PlayerServerRepository: a VERIFIED player_links row for
// an installation's guild PLUS observed activity on that installation's own server. An
// installation id in the URL is never itself proof of anything - it is only a lookup key the
// backend independently verifies before returning anything.
//
// Reuses codePlayerIdentityRequired exactly as internal/economy's "Me" endpoint already defines
// it (saas_api_economy.go) - the same stable error for "no verified DayZ link" in both places.

const playerTimeout = 10 * time.Second

func (a *App) registerPlayerRoutes() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/player/servers", a.handlePlayerServers)
	h("GET /api/saas/player/servers/{installationID}/stats", a.handlePlayerServerStats)
}

// --- DTOs (docs/PLAYER_API.md "Exact DTOs") -----------------------------------------------------

// playerServerSummaryDTO deliberately omits organizationId: a Player is never an organization
// member and has no legitimate use for an organization id (least exposure - task Part A.2's "?
// only if safe/needed" resolved to "not needed").
type playerServerSummaryDTO struct {
	InstallationID   int64   `json:"installationId"`
	ServerName       string  `json:"serverName"`
	Platform         string  `json:"platform"`
	ServerStatus     string  `json:"serverStatus"`
	DiscordGuildName string  `json:"discordGuildName"`
	LastSeenAt       *string `json:"lastSeenAt"`
	LinkedPlayerID   int64   `json:"linkedPlayerId"`
}

// playerServersResponseDTO is GET /api/saas/player/servers. DefaultInstallationID is nil only
// when Items is empty (an unverified or never-observed user) - see handlePlayerServers.
type playerServersResponseDTO struct {
	Items                 []playerServerSummaryDTO `json:"items"`
	DefaultInstallationID *int64                   `json:"defaultInstallationId"`
}

func playerServerSummary(p repository.PlayerInstallation) playerServerSummaryDTO {
	return playerServerSummaryDTO{
		InstallationID: p.InstallationID, ServerName: p.ServerName, Platform: p.Platform, ServerStatus: p.ServerStatus,
		DiscordGuildName: p.DiscordGuildName, LastSeenAt: nullableTimeStr(p.LastSeenAt), LinkedPlayerID: p.PlayerID,
	}
}

// playerStatsDTO is GET .../player/servers/{installationId}/stats. bestStreak/currentStreak and
// the classic per-player faction are intentionally NOT included: both are only tracked guild-wide
// in the current data model (player_combat_stats / FactionRepository.GetActiveFactionForPlayer
// have no server dimension), and blending another server's activity into a response scoped to THIS
// installation would violate the installation isolation this endpoint exists to guarantee (docs/
// PLAYER_API.md "Fields not included"). Faction IS included, but sourced from the genuinely
// installation-scoped Faction Hub membership instead (hub_faction_members.installation_id) - null
// when the player is not in a Faction Hub faction on this installation.
type playerStatsDTO struct {
	InstallationID    int64   `json:"installationId"`
	Kills             int     `json:"kills"`
	Deaths            int     `json:"deaths"`
	KD                float64 `json:"kd"`
	Headshots         int     `json:"headshots"`
	Longshots         int     `json:"longshots"`
	LongestKillMeters float64 `json:"longestKillMeters"`
	PlaytimeSeconds   int64   `json:"playtimeSeconds"`
	BountiesClaimed   int     `json:"bountiesClaimed"`
	BountyValue       int64   `json:"bountyValue"`
	Faction           *string `json:"faction"` // Faction Hub faction name on THIS installation, or null
	LastSeenAt        *string `json:"lastSeenAt"`
}

func killsToKD(kills, deaths int) float64 {
	if deaths == 0 {
		return float64(kills)
	}
	return float64(kills) / float64(deaths)
}

// --- handlers ------------------------------------------------------------------------------------

// handlePlayerServers is GET /api/saas/player/servers: every installation the acting user is a
// legitimate, proven player on, most-recently-active first. An unverified/never-observed user
// gets an empty list (200), not an error - "you have no servers yet" is a normal, expected state
// for a Player who has not linked or has not played, not a failure.
func (a *App) handlePlayerServers(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	if a.SaaSPlayer == nil {
		writeSaaSError(w, codeInternalError, "player service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	rows, err := a.SaaSPlayer.ListForDiscordUser(ctx, user.DiscordUserID)
	if err != nil {
		playerFailed(w, "list player servers", err)
		return
	}
	resp := playerServersResponseDTO{Items: make([]playerServerSummaryDTO, 0, len(rows))}
	for _, row := range rows {
		resp.Items = append(resp.Items, playerServerSummary(row))
	}
	// rows is already ordered most-recently-active first (LastSeenAt DESC NULLS LAST, id ASC) by
	// PlayerServerRepository.ListForDiscordUser, so the first row IS the default per docs/PLAYER_API.md
	// "Default server rule" - no separate re-sort needed here.
	if len(rows) > 0 {
		id := rows[0].InstallationID
		resp.DefaultInstallationID = &id
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

// handlePlayerServerStats is GET .../player/servers/{installationID}/stats: the acting user's own
// stats on installationID, resolved server-side through their VERIFIED link - the browser can never
// submit a playerId or gamertag as authority (task Part B.6).
func (a *App) handlePlayerServerStats(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	if a.SaaSPlayer == nil {
		writeSaaSError(w, codeInternalError, "player service unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()

	scope, found, linked, err := a.SaaSPlayer.ResolvePlayerInstallation(ctx, installationID, user.DiscordUserID)
	if err != nil {
		playerFailed(w, "resolve player installation", err)
		return
	}
	if !found {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	if !linked {
		writeSaaSError(w, codePlayerIdentityRequired, "a verified DayZ link is required")
		return
	}
	// Identity is proven (a VERIFIED link exists for this installation's guild), but that alone is
	// not proof for THIS specific server - one guild can back several installations. Never expose
	// stats for a server with no observed association (section 10: non-enumerating 404, same as an
	// unknown installation, rather than a distinct "not associated" code that would confirm the
	// installation exists to a caller who has never played there).
	observed, err := a.SaaSPlayer.HasObservedActivity(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "check player installation association", err)
		return
	}
	if !observed {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}

	kills, deaths, headshots, longshots, longest, err := a.SaaSPlayer.KillStats(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "load player kill stats", err)
		return
	}
	claimed, bountyValue, err := a.SaaSPlayer.BountyStats(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "load player bounty stats", err)
		return
	}
	var lastSeen *string
	if a.ActivityRepository != nil {
		if activity, err := a.ActivityRepository.Get(ctx, scope.GuildID, scope.ServerID, scope.PlayerID); err == nil && activity != nil {
			lastSeen = nullableTimeStr(&activity.LastSeenAt)
		}
	}
	var playtimeSeconds int64
	if a.ActivityRepository != nil {
		if d, err := a.ActivityRepository.GetObservedPlaytime(ctx, scope.GuildID, scope.ServerID, scope.PlayerID, time.Now()); err == nil {
			playtimeSeconds = int64(d.Seconds())
		}
	}
	var faction *string
	if name, _, ok, err := a.SaaSPlayer.CurrentFaction(ctx, installationID, user.ID); err == nil && ok {
		faction = &name
	}

	writeSaaSJSON(w, http.StatusOK, playerStatsDTO{
		InstallationID: installationID, Kills: kills, Deaths: deaths, KD: killsToKD(kills, deaths),
		Headshots: headshots, Longshots: longshots, LongestKillMeters: longest, PlaytimeSeconds: playtimeSeconds,
		BountiesClaimed: claimed, BountyValue: bountyValue, Faction: faction, LastSeenAt: lastSeen,
	})
}

func playerFailed(w http.ResponseWriter, what string, err error) {
	slog.Warn("component=saas_api", "event", "player_read_failed", "what", what, "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not load "+what)
}
