package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
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
	h("GET /api/saas/player/home", a.handlePlayerHome)
	h("GET /api/saas/player/servers/{installationID}/stats", a.handlePlayerServerStats)
	h("GET /api/saas/player/servers/{installationID}/ranked", a.handlePlayerServerRanked)
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
	InstallationID int64   `json:"installationId"`
	Kills          int     `json:"kills"`
	Deaths         int     `json:"deaths"` // every death
	KD             float64 `json:"kd"`     // overall: kills / deaths
	// pvpDeaths + pveDeaths = deaths; pvpKd is kills / pvpDeaths (internal/deathstats).
	PvPDeaths         int     `json:"pvpDeaths"`
	PvEDeaths         int     `json:"pveDeaths"`
	PvPKD             float64 `json:"pvpKd"`
	Headshots         int     `json:"headshots"`
	Longshots         int     `json:"longshots"`
	LongestKillMeters float64 `json:"longestKillMeters"`
	PlaytimeSeconds   int64   `json:"playtimeSeconds"`
	BountiesClaimed   int     `json:"bountiesClaimed"`
	BountyValue       int64   `json:"bountyValue"`
	Faction           *string `json:"faction"` // Faction Hub faction name on THIS installation, or null
	LastSeenAt        *string `json:"lastSeenAt"`
	// TournamentTitle is the title the player holds from winning a tournament on this
	// installation (docs/TOURNAMENTS.md), or null.
	TournamentTitle *string `json:"tournamentTitle"`
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

// playerHomeServerDTO is the server of GET /api/saas/player/home. Unlike playerServerSummaryDTO it
// does carry organizationId: the website's Player Hub needs it to build the organization-scoped
// PLAYER routes (wallet, shop, factions, leaderboards), which take an organization id in the path
// but need no organization role. The id grants nothing by itself - every route authorizes again.
type playerHomeServerDTO struct {
	InstallationID   int64  `json:"installationId"`
	OrganizationID   int64  `json:"organizationId"`
	OrganizationName string `json:"organizationName"`
	ServerName       string `json:"serverName"`
	Platform         string `json:"platform"`
	ServerStatus     string `json:"serverStatus"`
	DiscordGuildID   string `json:"discordGuildId"`
	DiscordGuildName string `json:"discordGuildName"`
	LinkStatus       string `json:"linkStatus"`
	Observed         bool   `json:"observed"`
}

// How playerHomeResponseDTO.Server was chosen.
const (
	playerHomeSelectedPreferred = "PREFERRED" // the caller's ?installationId=, one of the user's servers
	playerHomeSelectedDefault   = "DEFAULT"   // the first server of the documented order
)

// playerHomeResponseDTO is GET /api/saas/player/home. Server and Selected are null (never
// omitted) when the acting user has no verified link in any guild that has an installation with a
// game server; Servers is then [] (never null).
type playerHomeResponseDTO struct {
	Server   *playerHomeServerDTO  `json:"server"`
	Servers  []playerHomeServerDTO `json:"servers"`
	Selected *string               `json:"selected"`
}

func playerHomeServer(home repository.PlayerHome) playerHomeServerDTO {
	return playerHomeServerDTO{
		InstallationID: home.InstallationID, OrganizationID: home.OrganizationID, OrganizationName: home.OrganizationName,
		ServerName: home.ServerName, Platform: home.Platform, ServerStatus: home.ServerStatus,
		DiscordGuildID: home.DiscordGuildID, DiscordGuildName: home.DiscordGuildName,
		LinkStatus: home.LinkStatus, Observed: home.Observed,
	}
}

// playerHomeResponse builds the response from the user's servers in default order. preferred is
// the caller's wish (0 = none): it is honoured only when it is one of homes, so the answer can
// never be a server the user is not linked on, whatever the browser sent.
func playerHomeResponse(homes []repository.PlayerHome, preferred int64) playerHomeResponseDTO {
	resp := playerHomeResponseDTO{Servers: make([]playerHomeServerDTO, 0, len(homes))}
	chosen := -1
	for i, home := range homes {
		resp.Servers = append(resp.Servers, playerHomeServer(home))
		if chosen < 0 && preferred > 0 && home.InstallationID == preferred {
			chosen = i
		}
	}
	if len(homes) == 0 {
		return resp
	}
	selected := playerHomeSelectedPreferred
	if chosen < 0 {
		chosen, selected = 0, playerHomeSelectedDefault
	}
	server := resp.Servers[chosen]
	resp.Server, resp.Selected = &server, &selected
	return resp
}

// playerHomePreference reads the optional ?installationId=. Anything that is not a positive
// integer is no preference (0) - the route never fails on it.
func playerHomePreference(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.URL.Query().Get("installationId"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// handlePlayerHome is GET /api/saas/player/home: the servers the website's Player Hub can show for
// the acting user and the one it shows now (docs/PLAYER_API.md "Player home"). It needs a VERIFIED
// link but, unlike handlePlayerServers, no observed activity, so a player who has just linked gets
// their hub. No server is a normal answer ({"server":null,"servers":[],"selected":null}).
func (a *App) handlePlayerHome(w http.ResponseWriter, r *http.Request) {
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
	homes, err := a.SaaSPlayer.HomesForDiscordUser(ctx, user.DiscordUserID)
	if err != nil {
		playerFailed(w, "resolve player home", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, playerHomeResponse(homes, playerHomePreference(r)))
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

	combat, err := a.SaaSPlayer.CombatStats(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
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

	dto := toPlayerStatsDTO(installationID, combat)
	dto.PlaytimeSeconds, dto.BountiesClaimed, dto.BountyValue, dto.Faction, dto.LastSeenAt = playtimeSeconds, claimed, bountyValue, faction, lastSeen
	if a.Tournaments != nil {
		if title, err := a.Tournaments.Title(ctx, installationID, scope.PlayerID); err == nil && title != "" {
			dto.TournamentTitle = &title
		}
	}
	writeSaaSJSON(w, http.StatusOK, dto)
}

// toPlayerStatsDTO fills the combat figures: deaths and kd keep their meaning (every death, the
// overall K/D); pvpDeaths, pveDeaths and pvpKd are the split.
func toPlayerStatsDTO(installationID int64, c repository.PlayerCombatStats) playerStatsDTO {
	return playerStatsDTO{
		InstallationID: installationID, Kills: c.Kills, Deaths: c.Deaths, KD: killsToKD(c.Kills, c.Deaths),
		PvPDeaths: c.PvPDeaths, PvEDeaths: c.PvEDeaths(), PvPKD: killsToKD(c.Kills, c.PvPDeaths),
		Headshots: c.Headshots, Longshots: c.Longshots, LongestKillMeters: c.LongestMeters,
	}
}

func playerFailed(w http.ResponseWriter, what string, err error) {
	slog.Warn("component=saas_api", "event", "player_read_failed", "what", what, "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not load "+what)
}
