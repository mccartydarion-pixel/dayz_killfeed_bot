package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/playercard"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Card (docs/CHAMPION_CARD.md): a shareable PNG of one player's stats on one server, and
// the same card animated as a GIF. Three ways in: the player API (the player's own card, JSON, PNG
// or GIF), a public share link the player creates and can revoke (the only unauthenticated route,
// PNG only: link previews need a still), and /card in Discord (the GIF).

const (
	cardSharePath = "/cards/"
	// cardCacheTTL bounds how often one share link is re-rendered; it is also the public
	// Cache-Control lifetime, so link previews refresh on the same schedule.
	cardCacheTTL     = 5 * time.Minute
	cardCacheEntries = 512
)

func (a *App) registerCardRoutes() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/player/servers/{installationID}/card", a.handlePlayerCard)
	h("GET /api/saas/player/servers/{installationID}/card.png", a.handlePlayerCardImage)
	h("GET /api/saas/player/servers/{installationID}/card.gif", a.handlePlayerCardGIF)
	h("POST /api/saas/player/servers/{installationID}/card/share", a.handleShareCard)
	h("DELETE /api/saas/player/servers/{installationID}/card/share", a.handleUnshareCard)
	h("GET /api/saas/cards/{token}", a.handleSharedCard)
	// Public, unauthenticated: a shared card is addressed by an unguessable, revocable token.
	h("GET "+cardSharePath+"{file}", a.handleSharedCardImage)
}

// buildCard assembles a card for one player on one server: the same card whichever way in (the
// player API, the public share routes, /card). installationID is optional (0): it only adds the
// Faction Hub faction. Returns (nil, nil) when the player does not exist.
func (a *App) buildCard(ctx context.Context, installationID, guildID, serverID, playerID int64) (*playercard.Card, error) {
	if a.Cards == nil {
		return nil, fmt.Errorf("cards unavailable")
	}
	stats, err := a.Cards.Stats(ctx, guildID, serverID, playerID)
	if err != nil || stats == nil {
		return nil, err
	}
	card := &playercard.Card{
		PlayerName: stats.PlayerName, ServerName: stats.ServerName, Kills: stats.Kills, Deaths: stats.Deaths, PvPDeaths: &stats.PvPDeaths, Headshots: stats.Headshots,
		LongestKillMeters: stats.LongestKillMeters, PlaytimeSeconds: stats.PlaytimeSeconds, LongestLifeSeconds: stats.LongestLifeSeconds,
		Rank: stats.Rank, RankedPlayers: stats.RankedPlayers, SiteHost: a.siteHost(), GeneratedAt: time.Now().UTC(),
	}
	if installationID > 0 {
		if name, tag, ok, err := a.Cards.Faction(ctx, installationID, guildID, playerID); err == nil && ok {
			card.FactionName, card.FactionTag = name, tag
		}
	}
	if a.Seasons != nil {
		if season, err := a.Seasons.GetActiveSeason(ctx, guildID); err == nil && season != nil {
			card.SeasonName = season.Name
		}
	}
	card.Ranked = a.cardRanked(ctx, guildID, serverID, playerID)
	return card, nil
}

// cardRanked is the player's Ranked Points standing for the card, nil when the server has no
// active season. A failure to read it is logged and leaves the card without a rank: the card
// itself never fails over its rank row.
func (a *App) cardRanked(ctx context.Context, guildID, serverID, playerID int64) *playercard.Ranked {
	if a.Ranked == nil {
		return nil
	}
	p, err := a.Ranked.ServerPlayerProgress(ctx, guildID, serverID, playerID)
	if errors.Is(err, repository.ErrRankedIneligible) {
		return nil
	}
	if err != nil {
		slog.Warn("component=saas_api", "event", "card_ranked_failed", "err", err.Error())
		return nil
	}
	return &playercard.Ranked{Tier: p.Tier, RP: p.RP, Position: p.ServerPosition, NextTier: p.NextTier, Remaining: p.Remaining, TierStartRP: p.TierStartRP, NextTierRP: p.NextTierRP}
}

// siteHost is the website's host name for the card's footer ("championshp.vip"), or "" when no
// site address is configured.
func (a *App) siteHost() string {
	if a == nil || a.Config == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(a.Config.SiteBaseURL))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

type cardDTO struct {
	PlayerName  string  `json:"playerName"`
	ServerName  string  `json:"serverName"`
	FactionName *string `json:"factionName"`
	FactionTag  *string `json:"factionTag"`
	SeasonName  *string `json:"seasonName"`
	Kills       int     `json:"kills"`
	Deaths      int     `json:"deaths"` // every death
	KD          float64 `json:"kd"`     // overall: kills / deaths
	// pvpDeaths + pveDeaths = deaths; pvpKd is kills / pvpDeaths (internal/deathstats).
	PvPDeaths          *int          `json:"pvpDeaths,omitempty"`
	PvEDeaths          *int          `json:"pveDeaths,omitempty"`
	PvPKD              *float64      `json:"pvpKd,omitempty"`
	Headshots          int           `json:"headshots"`
	LongestKillMeters  float64       `json:"longestKillMeters"`
	PlaytimeSeconds    int64         `json:"playtimeSeconds"`
	LongestLifeSeconds *int64        `json:"longestLifeSeconds"`
	Rank               *int          `json:"rank"`
	RankedPlayers      int           `json:"rankedPlayers"`
	Ranked             cardRankedDTO `json:"ranked"`
	Tiles              []cardTileDTO `json:"tiles"` // exactly what the image shows, in order
	Image              cardImageDTO  `json:"image"`
	Share              *cardShareDTO `json:"share"`
}

// cardRankedDTO is the player's Ranked Points standing on the card: {"status": "NOT_STARTED"} when
// the server has no active season, else the progress fields beside "status": "ACTIVE".
type cardRankedDTO struct {
	Status string `json:"status"`
	*cardRankedProgressDTO
}

type cardRankedProgressDTO struct {
	Tier     ranked.Tier `json:"tier"`
	RP       int64       `json:"rp"`
	Position *int64      `json:"position"` // null until the player has RP
	// The climb to the next tier; absent at Master (remainingRp is never 0 below Master).
	NextTier    ranked.Tier `json:"nextTier,omitempty"`
	RemainingRP int64       `json:"remainingRp,omitempty"`
	TierStartRP int64       `json:"tierStartRp"`
	NextTierRP  *int64      `json:"nextTierRp,omitempty"`
}

func toCardRankedDTO(r *playercard.Ranked) cardRankedDTO {
	if r == nil {
		return cardRankedDTO{Status: "NOT_STARTED"}
	}
	p := &cardRankedProgressDTO{Tier: r.Tier, RP: r.RP, Position: r.Position, TierStartRP: r.TierStartRP}
	if !r.AtTop() {
		p.NextTier, p.RemainingRP, p.NextTierRP = r.NextTier, r.Remaining, r.NextTierRP
	}
	return cardRankedDTO{Status: "ACTIVE", cardRankedProgressDTO: p}
}

type cardTileDTO struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Note is the tile's small caption on the image (the PvP/PvE split, what the server rank
	// counts); absent when it has none.
	Note string `json:"note,omitempty"`
}

type cardImageDTO struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type cardShareDTO struct {
	Token     string `json:"token"`
	ImageURL  string `json:"imageUrl"`
	CreatedAt string `json:"createdAt"`
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (a *App) toCardDTO(c playercard.Card) cardDTO {
	d := cardDTO{
		PlayerName: c.PlayerName, ServerName: c.ServerName, FactionName: optionalString(c.FactionName), FactionTag: optionalString(c.FactionTag),
		SeasonName: optionalString(c.SeasonName), Kills: c.Kills, Deaths: c.Deaths, KD: c.KD(), Headshots: c.Headshots,
		LongestKillMeters: c.LongestKillMeters, PlaytimeSeconds: c.PlaytimeSeconds, LongestLifeSeconds: c.LongestLifeSeconds,
		Rank: c.Rank, RankedPlayers: c.RankedPlayers, Ranked: toCardRankedDTO(c.Ranked), Image: cardImageDTO{Width: playercard.Width, Height: playercard.Height},
	}
	if pve, ok := c.PvEDeaths(); ok {
		pvpKD, _ := c.PvPKD()
		d.PvPDeaths, d.PvEDeaths, d.PvPKD = c.PvPDeaths, &pve, &pvpKD
	}
	notes := c.TileNotes()
	for i, t := range c.Tiles() {
		d.Tiles = append(d.Tiles, cardTileDTO{Label: t[0], Value: t[1], Note: notes[i]})
	}
	return d
}

func (a *App) cardShareDTO(token string, createdAt time.Time) *cardShareDTO {
	return &cardShareDTO{Token: token, ImageURL: a.assetBaseURL() + cardSharePath + token + ".png", CreatedAt: createdAt.UTC().Format(time.RFC3339)}
}

// handlePlayerCard is GET .../player/servers/{installationID}/card: the acting player's own card
// as data, plus their active share link if they have one.
func (a *App) handlePlayerCard(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	card, err := a.buildCard(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil || card == nil {
		cardFailed(w, "build card", err)
		return
	}
	dto := a.toCardDTO(*card)
	if share, err := a.Cards.ActiveShare(ctx, scope.InstallationID, scope.PlayerID); err == nil && share != nil {
		dto.Share = a.cardShareDTO(share.Token, share.CreatedAt)
	}
	writeSaaSJSON(w, http.StatusOK, dto)
}

// handlePlayerCardImage is GET .../card.png: the acting player's own card as a PNG.
func (a *App) handlePlayerCardImage(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	card, err := a.buildCard(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil || card == nil {
		cardFailed(w, "build card", err)
		return
	}
	png, err := playercard.Render(*card)
	if err != nil {
		cardFailed(w, "render card", err)
		return
	}
	writeCardImage(w, r, "image/png", png, "private, no-store")
}

// handlePlayerCardGIF is GET .../card.gif: the acting player's own card, animated.
func (a *App) handlePlayerCardGIF(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	card, err := a.buildCard(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil || card == nil {
		cardFailed(w, "build card", err)
		return
	}
	gif, err := playercard.RenderAnimation(*card)
	if err != nil {
		cardFailed(w, "animate card", err)
		return
	}
	writeCardImage(w, r, "image/gif", gif, "private, no-store")
}

// handleShareCard is POST .../card/share: create (or return) the acting player's public link.
func (a *App) handleShareCard(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.Cards == nil {
		writeSaaSError(w, codeInternalError, "cards unavailable")
		return
	}
	share, err := a.Cards.EnsureShare(ctx, scope.InstallationID, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		cardFailed(w, "share card", err)
		return
	}
	writeSaaSJSON(w, http.StatusOK, a.cardShareDTO(share.Token, share.CreatedAt))
}

// handleUnshareCard is DELETE .../card/share: revoke the acting player's public link.
func (a *App) handleUnshareCard(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.Cards == nil {
		writeSaaSError(w, codeInternalError, "cards unavailable")
		return
	}
	share, _ := a.Cards.ActiveShare(ctx, scope.InstallationID, scope.PlayerID)
	revoked, err := a.Cards.RevokeShare(ctx, scope.InstallationID, scope.PlayerID)
	if err != nil {
		cardFailed(w, "unshare card", err)
		return
	}
	if share != nil {
		a.cardCache().drop(share.Token)
	}
	writeSaaSJSON(w, http.StatusOK, map[string]bool{"revoked": revoked})
}

// handleSharedCard is GET /api/saas/cards/{token}: the data behind a share link, for the
// website's share page. Service-authenticated but with no acting user - the page is public.
func (a *App) handleSharedCard(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if a.Cards == nil {
		writeSaaSError(w, codeInternalError, "cards unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	share, err := a.Cards.ResolveShare(ctx, r.PathValue("token"))
	if err != nil {
		cardFailed(w, "resolve share", err)
		return
	}
	if share == nil {
		writeSaaSError(w, codeNotFound, "card not found")
		return
	}
	card, err := a.buildCard(ctx, share.InstallationID, share.GuildID, share.ServerID, share.PlayerID)
	if err != nil || card == nil {
		cardFailed(w, "build card", err)
		return
	}
	dto := a.toCardDTO(*card)
	dto.Share = a.cardShareDTO(share.Token, share.CreatedAt)
	writeSaaSJSON(w, http.StatusOK, dto)
}

// handleSharedCardImage is GET /cards/{token}.png: the public card image. A revoked or unknown
// token is a plain 404; rendering is cached per token for cardCacheTTL.
func (a *App) handleSharedCardImage(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutSuffix(r.PathValue("file"), ".png")
	if !ok || token == "" || len(token) > 64 || a.Cards == nil {
		http.NotFound(w, r)
		return
	}
	cache := a.cardCache()
	if png, hit := cache.get(token); hit {
		writeCardImage(w, r, "image/png", png, cardPublicCacheControl)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	share, err := a.Cards.ResolveShare(ctx, token)
	if err != nil {
		slog.Warn("component=saas_api", "event", "card_share_lookup_failed", "err", err.Error())
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	if share == nil {
		http.NotFound(w, r)
		return
	}
	card, err := a.buildCard(ctx, share.InstallationID, share.GuildID, share.ServerID, share.PlayerID)
	if err != nil || card == nil {
		if err != nil {
			slog.Warn("component=saas_api", "event", "card_build_failed", "err", err.Error())
		}
		http.NotFound(w, r)
		return
	}
	png, err := playercard.Render(*card)
	if err != nil {
		slog.Warn("component=saas_api", "event", "card_render_failed", "err", err.Error())
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	cache.put(token, png)
	writeCardImage(w, r, "image/png", png, cardPublicCacheControl)
}

var cardPublicCacheControl = fmt.Sprintf("public, max-age=%d", int(cardCacheTTL.Seconds()))

func writeCardImage(w http.ResponseWriter, r *http.Request, contentType string, data []byte, cacheControl string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Cache-Control", cacheControl)
	h.Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func cardFailed(w http.ResponseWriter, what string, err error) {
	if err == nil {
		writeSaaSError(w, codeNotFound, "player not found")
		return
	}
	slog.Warn("component=saas_api", "event", "card_failed", "what", what, "err", err.Error())
	writeSaaSError(w, codeInternalError, "could not "+what)
}

// renderedCardCache holds recently rendered public cards. Bounded: at capacity the oldest entry is
// evicted, so an attacker cycling tokens cannot grow it (unknown tokens are never cached at all).
type renderedCardCache struct {
	mu      sync.Mutex
	entries map[string]renderedCard
	now     func() time.Time
}

type renderedCard struct {
	png []byte
	at  time.Time
}

func (a *App) cardCache() *renderedCardCache {
	a.cardCacheOnce.Do(func() {
		a.renderedCards = &renderedCardCache{entries: map[string]renderedCard{}, now: time.Now}
	})
	return a.renderedCards
}

func (c *renderedCardCache) get(token string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[token]
	if !ok || c.now().Sub(e.at) > cardCacheTTL {
		delete(c.entries, token)
		return nil, false
	}
	return e.png, true
}

func (c *renderedCardCache) put(token string, png []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= cardCacheEntries {
		oldest, oldestAt := "", time.Time{}
		for k, e := range c.entries {
			if oldest == "" || e.at.Before(oldestAt) {
				oldest, oldestAt = k, e.at
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[token] = renderedCard{png: png, at: c.now()}
}

func (c *renderedCardCache) drop(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, token)
}

// registerCardCommand registers /card: post your own Champion Card in the channel.
func (a *App) registerCardCommand(session *discordgo.Session, commands discord.CommandRegistrar) {
	if a.Cards == nil || a.Guilds == nil || a.Discord == nil || session == nil || a.Config.DiscordGuildID == "" {
		return
	}
	card := func(ctx context.Context, guildRowID, serverID, playerID int64) (*playercard.Card, error) {
		installationID, _ := a.Cards.InstallationForServer(ctx, serverID)
		return a.buildCard(ctx, installationID, guildRowID, serverID, playerID)
	}
	handler := discord.NewCardCommandHandler(a.Guilds, a.linkedPlayer, a.publicServerID, card)
	if err := discord.RegisterCardCommand(commands, a.Config.DiscordGuildID); err != nil {
		slog.Warn("component=discord", "msg", "failed to register card command", "err", err.Error())
	} else {
		slog.Info("component=discord", "msg", "card command queued")
	}
	// The card is posted for the channel to see, so a slow /card defers publicly.
	a.Discord.Interactions().Command("card", discord.AckPublic, handler.Handle)
}
