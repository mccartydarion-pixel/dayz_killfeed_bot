package app

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Cross-server network (docs/NETWORK.md): a public directory of the servers that opted in and
// leaderboards across them. These routes take the service secret but no acting user - the website
// renders them for anyone. They expose only what a listed server's own public Discord already
// shows (its name, how busy it is, player names and kill figures), and never anything about an
// installation that has not opted in.

const (
	networkTimeout  = 10 * time.Second
	networkCacheTTL = time.Minute
)

func (a *App) registerNetworkRoutes() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/network/servers", a.handleNetworkServers)
	h("GET /api/saas/network/servers/{installationID}", a.handleNetworkServer)
	h("GET /api/saas/network/leaderboard", a.handleNetworkLeaderboard)
	h("GET /api/saas/network/spotlight", a.handleNetworkSpotlight)
}

type networkServerDTO struct {
	InstallationID   int64   `json:"installationId"`
	Name             string  `json:"name"`
	Platform         string  `json:"platform"`
	Description      string  `json:"description"`
	DiscordGuildName string  `json:"discordGuildName"`
	DiscordInviteURL string  `json:"discordInviteUrl"` // "" when the owner shared none
	PlayersOnline    int     `json:"playersOnline"`
	Peak24h          int     `json:"peak24h"`
	ActivePlayers7d  int     `json:"activePlayers7d"`
	Kills7d          int     `json:"kills7d"`
	TotalKills       int     `json:"totalKills"`
	TrackedPlayers   int     `json:"trackedPlayers"`
	LastActivityAt   *string `json:"lastActivityAt"`
}

func toNetworkServerDTO(s repository.NetworkServer) networkServerDTO {
	return networkServerDTO{InstallationID: s.InstallationID, Name: s.Name, Platform: s.Platform, Description: s.Description, DiscordGuildName: s.DiscordName,
		DiscordInviteURL: s.DiscordInvite, PlayersOnline: s.PlayersOnline, Peak24h: s.Peak24h, ActivePlayers7d: s.ActivePlayers7d, Kills7d: s.Kills7d, TotalKills: s.TotalKills,
		TrackedPlayers: s.TrackedPlayers, LastActivityAt: nullableTimeStr(s.LastActivityAt)}
}

type networkRankDTO struct {
	Rank       int     `json:"rank"`
	PlayerName string  `json:"playerName"`
	Platform   string  `json:"platform"`
	Value      float64 `json:"value"`
	Servers    *int    `json:"servers,omitempty"`    // KILLS: listed servers the kills were made on
	ServerName *string `json:"serverName,omitempty"` // LONGEST_KILL / LONGEST_LIFE
	Weapon     *string `json:"weapon,omitempty"`     // LONGEST_KILL
	At         *string `json:"at,omitempty"`
}

func toNetworkRanks(board string, rows []repository.NetworkRank) []networkRankDTO {
	out := make([]networkRankDTO, 0, len(rows))
	for i, n := range rows {
		d := networkRankDTO{Rank: i + 1, PlayerName: n.PlayerName, Platform: n.Platform, Value: n.Value, At: nullableTimeStr(n.At)}
		if board == repository.NetworkBoardKills {
			servers := n.Servers
			d.Servers = &servers
		} else {
			name := n.ServerName
			d.ServerName = &name
		}
		if board == repository.NetworkBoardLongestKill && n.Weapon != "" {
			weapon := n.Weapon
			d.Weapon = &weapon
		}
		out = append(out, d)
	}
	return out
}

// networkPlatform bounds the platform filter to the shape of a platform key.
var networkPlatform = regexp.MustCompile(`^[A-Z0-9_]{1,24}$`)

func parsePlatform(w http.ResponseWriter, r *http.Request) (string, bool) {
	p := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("platform")))
	if p != "" && !networkPlatform.MatchString(p) {
		writeSaaSError(w, codeInvalidRequest, "platform is not valid")
		return "", false
	}
	return p, true
}

// networkCache holds the public network responses for networkCacheTTL: the routes are unauthenticated
// from the visitor's point of view, so the same few queries must not run once per page view.
type networkCache struct {
	mu      sync.Mutex
	entries map[string]networkCacheEntry
}

type networkCacheEntry struct {
	value any
	at    time.Time
}

const networkCacheMax = 256

func (a *App) cachedNetwork(key string, load func() (any, error)) (any, error) {
	a.networkCacheOnce.Do(func() { a.networkResponses = &networkCache{entries: map[string]networkCacheEntry{}} })
	c := a.networkResponses
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.entries[key]; ok && now.Sub(e.at) < networkCacheTTL {
		c.mu.Unlock()
		return e.value, nil
	}
	c.mu.Unlock()
	v, err := load()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.entries) >= networkCacheMax {
		for k, e := range c.entries {
			if now.Sub(e.at) >= networkCacheTTL {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= networkCacheMax {
			c.entries = map[string]networkCacheEntry{}
		}
	}
	c.entries[key] = networkCacheEntry{value: v, at: now}
	c.mu.Unlock()
	return v, nil
}

// invalidateNetworkCache drops every cached network response (a listing just changed).
func (a *App) invalidateNetworkCache() {
	if a.networkResponses == nil {
		return
	}
	a.networkResponses.mu.Lock()
	a.networkResponses.entries = map[string]networkCacheEntry{}
	a.networkResponses.mu.Unlock()
}

func (a *App) networkReady(w http.ResponseWriter, r *http.Request) bool {
	if !a.requireSaaSServiceAuth(w, r) {
		return false
	}
	if a.Network == nil {
		writeSaaSError(w, codeInternalError, "network unavailable")
		return false
	}
	return true
}

func networkFailed(w http.ResponseWriter, what string, err error) {
	slog.Warn("component=saas_api", "event", "network_failed", "what", what, "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not load "+what)
}

// handleNetworkServers is GET /api/saas/network/servers?platform=: the directory, busiest first.
func (a *App) handleNetworkServers(w http.ResponseWriter, r *http.Request) {
	if !a.networkReady(w, r) {
		return
	}
	platform, ok := parsePlatform(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	v, err := a.cachedNetwork("servers|"+platform, func() (any, error) {
		rows, err := a.Network.Servers(ctx, time.Now(), platform, 200)
		if err != nil {
			return nil, err
		}
		items := make([]networkServerDTO, 0, len(rows))
		for _, s := range rows {
			items = append(items, toNetworkServerDTO(s))
		}
		return items, nil
	})
	if err != nil {
		networkFailed(w, "servers", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": v})
}

type networkServerDetailDTO struct {
	networkServerDTO
	TopKillers   []networkRankDTO `json:"topKillers"`
	LongestKills []networkRankDTO `json:"longestKills"`
	LongestLives []networkRankDTO `json:"longestLives"`
}

// handleNetworkServer is GET /api/saas/network/servers/{installationID}: one listed server with
// its own top boards. An installation that is not listed is a plain 404.
func (a *App) handleNetworkServer(w http.ResponseWriter, r *http.Request) {
	if !a.networkReady(w, r) {
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	v, err := a.cachedNetwork("server|"+r.PathValue("installationID"), func() (any, error) {
		s, err := a.Network.Server(ctx, time.Now(), installationID)
		if err != nil || s == nil {
			return (*networkServerDetailDTO)(nil), err
		}
		detail := &networkServerDetailDTO{networkServerDTO: toNetworkServerDTO(*s)}
		for board, dst := range map[string]*[]networkRankDTO{repository.NetworkBoardKills: &detail.TopKillers,
			repository.NetworkBoardLongestKill: &detail.LongestKills, repository.NetworkBoardLongestLife: &detail.LongestLives} {
			rows, err := a.Network.Leaderboard(ctx, board, "", installationID, nil, 10)
			if err != nil {
				return nil, err
			}
			*dst = toNetworkRanks(board, rows)
		}
		return detail, nil
	})
	if err != nil {
		networkFailed(w, "server", err)
		return
	}
	detail, _ := v.(*networkServerDetailDTO)
	if detail == nil {
		writeSaaSError(w, codeNotFound, "server not found")
		return
	}
	writeSaaSJSON(w, http.StatusOK, detail)
}

// handleNetworkLeaderboard is GET /api/saas/network/leaderboard?board=&platform=&days=&limit=.
func (a *App) handleNetworkLeaderboard(w http.ResponseWriter, r *http.Request) {
	if !a.networkReady(w, r) {
		return
	}
	board := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("board")))
	if board == "" {
		board = repository.NetworkBoardKills
	}
	if board != repository.NetworkBoardKills && board != repository.NetworkBoardLongestKill && board != repository.NetworkBoardLongestLife {
		writeSaaSError(w, codeInvalidRequest, "board must be KILLS, LONGEST_KILL or LONGEST_LIFE")
		return
	}
	platform, ok := parsePlatform(w, r)
	if !ok {
		return
	}
	days, ok := queryInt(w, r, "days", 0, 0, 365)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", 25, 1, 100)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
	defer cancel()
	key := strings.Join([]string{"board", board, platform, r.URL.Query().Get("days"), r.URL.Query().Get("limit")}, "|")
	v, err := a.cachedNetwork(key, func() (any, error) {
		var since *time.Time
		if days > 0 {
			t := time.Now().UTC().AddDate(0, 0, -days)
			since = &t
		}
		rows, err := a.Network.Leaderboard(ctx, board, platform, 0, since, limit)
		if err != nil {
			return nil, err
		}
		return toNetworkRanks(board, rows), nil
	})
	if err != nil {
		networkFailed(w, "leaderboard", err)
		return
	}
	var window *int
	if days > 0 {
		window = &days
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"board": board, "platform": platform, "windowDays": window, "items": v})
}
