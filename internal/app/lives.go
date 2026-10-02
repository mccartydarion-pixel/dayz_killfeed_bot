package app

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Lives (docs/LIVES.md): a life is the stretch between two deaths of one player on one server. This
// file wires the pieces together - closing a life when a death is persisted, the /life command, the
// opt-in recap DM and the player-facing API. The derivation itself lives in
// internal/repository.LifeRepository.

// lifeRecordTimeout bounds the extra work a persisted death does to close its life. It runs inside
// the persistence worker, so it must stay well under the engine's enqueue timeout.
const lifeRecordTimeout = 4 * time.Second

// recordLifeEnd closes the dead player's life from a persisted "died" line. A suicide emote is
// persisted as a death row too, but the "died" line that follows it is the death itself, so only
// PLAYER_DEATH ends a life here. Best-effort - a failure is logged and never reaches the pipeline.
func (p *persistenceStoreAdapter) recordLifeEnd(ctx context.Context, record repository.DeathRecord, ev *killfeed.Event) {
	if p.lives == nil || ev == nil || ev.Type != killfeed.EventPlayerDeath || record.PlayerID == 0 || record.ServerID == 0 {
		return
	}
	in := repository.LifeEndInput{GuildID: record.GuildID, ServerID: record.ServerID, PlayerID: record.PlayerID, SeasonID: record.SeasonID,
		At: lifeEndTime(record.EventTime), Fingerprint: record.Fingerprint}
	name := ""
	if ev.Player != nil {
		name = ev.Player.Name
	}
	p.closeLife(ctx, in, name, "")
}

// recordLifeEndFromKill closes the victim's life from a persisted PvP kill - ADM logs a player kill
// as "killed by Player" and nothing else, so the kill row is the victim's death.
func (p *persistenceStoreAdapter) recordLifeEndFromKill(ctx context.Context, killID int64, record repository.KillRecord, ev *killfeed.Event) {
	if p.lives == nil || record.VictimPlayerID == 0 || record.ServerID == 0 {
		return
	}
	in := repository.LifeEndInput{GuildID: record.GuildID, ServerID: record.ServerID, PlayerID: record.VictimPlayerID, SeasonID: record.SeasonID,
		At: lifeEndTime(record.EventTime), Fingerprint: record.Fingerprint,
		Kill: &repository.LifeEndKill{KillID: killID, KillerPlayerID: record.KillerPlayerID, Weapon: record.WeaponDisplay, Distance: record.Distance}}
	victim, killer := "", ""
	if ev != nil {
		if ev.Victim != nil {
			victim = ev.Victim.Name
		}
		if ev.Killer != nil {
			killer = ev.Killer.Name
		}
	}
	p.closeLife(ctx, in, victim, killer)
}

func lifeEndTime(eventTime *time.Time) time.Time {
	if eventTime != nil {
		return eventTime.UTC()
	}
	return time.Now().UTC()
}

func (p *persistenceStoreAdapter) closeLife(ctx context.Context, in repository.LifeEndInput, playerName, killerName string) {
	lifeCtx, cancel := context.WithTimeout(ctx, lifeRecordTimeout)
	defer cancel()
	life, err := p.lives.RecordDeath(lifeCtx, in)
	if err != nil {
		slog.Warn("component=lives", "event", "life_record_failed", "server_id", in.ServerID, "err", err.Error())
		return
	}
	if life == nil {
		return
	}
	life.PlayerName, life.KillerName = playerName, killerName
	p.lifeRecap.Notify(*life)
}

// backfillDailyPresence recovers presence-only days for one server in the background (kills,
// deaths, retained location events). Idempotent, so it simply runs on every worker start.
func (a *App) backfillDailyPresence(ctx context.Context, row repository.GameServer) {
	if a.ActivityRepository == nil {
		return
	}
	go func() {
		backfillCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		days, err := a.ActivityRepository.BackfillDailyPresence(backfillCtx, row.GuildID, row.ID)
		if err != nil {
			slog.Warn("component=retention", "event", "presence_backfill_failed", "server_id", row.ID, "err", err.Error())
			return
		}
		if days > 0 {
			slog.Info("component=retention", "event", "presence_backfilled", "server_id", row.ID, "days", days)
		}
	}()
}

// publicServerID is the game server the guild's public Discord surfaces report on: the selected
// public server, else the guild's only connected server.
func (a *App) publicServerID(ctx context.Context, guildRowID int64) (int64, bool) {
	if a.Guilds != nil {
		if guild, err := a.Guilds.GetGuildByID(ctx, guildRowID); err == nil && guild != nil && guild.SelectedPublicServerID != 0 {
			return guild.SelectedPublicServerID, true
		}
	}
	if a.Servers != nil {
		if id, err := a.Servers.ConnectedServerID(ctx, guildRowID); err == nil && id != 0 {
			return id, true
		}
	}
	return 0, false
}

// linkedPlayer resolves a Discord user's VERIFIED player and display name in one guild.
func (a *App) linkedPlayer(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool) {
	if a.Links == nil || discordUserID == "" {
		return 0, "", false
	}
	playerID, err := a.Links.GetVerifiedPlayerByDiscord(ctx, guildRowID, discordUserID)
	if err != nil || playerID == 0 {
		return 0, "", false
	}
	name := ""
	if a.Players != nil {
		if names, err := a.Players.DisplayNamesByID(ctx, guildRowID, []int64{playerID}); err == nil {
			name = names[playerID]
		}
	}
	return playerID, name, true
}

// registerLifeCommands registers /life and starts the recap notifier. Called once from Run, before
// the server workers (and their persistence adapter) are created.
func (a *App) registerLifeCommands(ctx context.Context, session *discordgo.Session, commands discord.CommandRegistrar) {
	if a.Lives == nil || a.Guilds == nil || a.Discord == nil || session == nil || a.Config.DiscordGuildID == "" {
		return
	}
	a.LifeRecap = discord.NewLifeRecapNotifier(session, a.Lives, a.serverNameFunc())
	go a.LifeRecap.Run(ctx)
	handler := discord.NewLifeCommandHandler(a.Lives, a.Guilds, a.linkedPlayer, a.publicServerID)
	if err := discord.RegisterLifeCommands(commands, a.Config.DiscordGuildID); err != nil {
		slog.Warn("component=discord", "msg", "failed to register life command", "err", err.Error())
	} else {
		slog.Info("component=discord", "msg", "life command queued")
	}
	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type == discordgo.InteractionApplicationCommand && i.ApplicationCommandData().Name == "life" {
			handler.Handle(s, i)
		}
	})
}

// --- player-facing API ---------------------------------------------------------------------------

func (a *App) registerLifeRoutes() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/player/servers/{installationID}/lives", a.handlePlayerLives)
	h("GET /api/saas/player/servers/{installationID}/lives/leaderboard", a.handleLifeLeaderboard)
}

type lifeDTO struct {
	ID                   int64    `json:"id"`
	PlayerName           string   `json:"playerName"`
	StartedAt            string   `json:"startedAt"`
	EndedAt              string   `json:"endedAt"`
	PlaytimeSeconds      *int64   `json:"playtimeSeconds"` // null: the life began before lives were recorded
	Kills                int      `json:"kills"`
	Headshots            int      `json:"headshots"`
	LongestKillMeters    *float64 `json:"longestKillMeters"`
	TrackedDistanceMeter *float64 `json:"trackedDistanceMeters"` // a lower bound; null with fewer than two samples
	Cause                string   `json:"cause"`
	KillerName           *string  `json:"killerName"`
	Weapon               *string  `json:"weapon"`
	DistanceMeters       *float64 `json:"distanceMeters"`
}

func toLifeDTO(l repository.Life) lifeDTO {
	d := lifeDTO{
		ID: l.ID, PlayerName: l.PlayerName, StartedAt: l.StartedAt.UTC().Format(time.RFC3339), EndedAt: l.EndedAt.UTC().Format(time.RFC3339),
		PlaytimeSeconds: l.PlaytimeSeconds, Kills: l.Kills, Headshots: l.Headshots, LongestKillMeters: l.LongestKillM,
		TrackedDistanceMeter: l.TrackedDistance, Cause: l.Cause, DistanceMeters: l.DistanceM,
	}
	if l.Cause == repository.LifeCausePVP && l.KillerName != "" {
		d.KillerName = &l.KillerName
	}
	if l.Weapon != "" {
		d.Weapon = &l.Weapon
	}
	return d
}

type currentLifeDTO struct {
	PlayerName      string `json:"playerName"`
	StartedAt       string `json:"startedAt"`
	PlaytimeSeconds *int64 `json:"playtimeSeconds"`
	Kills           int    `json:"kills"`
	Online          bool   `json:"online"`
}

func toCurrentLifeDTO(c repository.CurrentLife) currentLifeDTO {
	return currentLifeDTO{PlayerName: c.PlayerName, StartedAt: c.StartedAt.UTC().Format(time.RFC3339), PlaytimeSeconds: c.PlaytimeSeconds, Kills: c.Kills, Online: c.Online}
}

type lifeSummaryDTO struct {
	Lives                  int      `json:"lives"`
	LongestPlaytimeSeconds *int64   `json:"longestPlaytimeSeconds"`
	AveragePlaytimeSeconds *float64 `json:"averagePlaytimeSeconds"`
	MostKills              int      `json:"mostKills"`
	TrackedDistanceMeters  float64  `json:"trackedDistanceMeters"`
	DeathsByPVP            int      `json:"deathsByPvp"`
	DeathsBySuicide        int      `json:"deathsBySuicide"`
	DeathsByOther          int      `json:"deathsByOther"`
}

type playerLivesResponseDTO struct {
	InstallationID int64           `json:"installationId"`
	Current        *currentLifeDTO `json:"current"`
	Summary        lifeSummaryDTO  `json:"summary"`
	Recent         []lifeDTO       `json:"recent"`
}

// resolvePlayerScope runs the player API's authorization chain (docs/PLAYER_API.md): service auth,
// acting user, a VERIFIED link for the installation's guild, and observed activity on the
// installation's own server. It writes the error response itself.
func (a *App) resolvePlayerScope(w http.ResponseWriter, r *http.Request) (repository.PlayerInstallationScope, context.Context, context.CancelFunc, bool) {
	if !a.requireSaaSServiceAuth(w, r) {
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	if a.SaaSPlayer == nil {
		writeSaaSError(w, codeInternalError, "player service unavailable")
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	scope, found, linked, err := a.SaaSPlayer.ResolvePlayerInstallation(ctx, installationID, user.DiscordUserID)
	if err != nil {
		cancel()
		playerFailed(w, "resolve player installation", err)
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	if !found {
		cancel()
		writeSaaSError(w, codeNotFound, "installation not found")
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	if !linked {
		cancel()
		writeSaaSError(w, codePlayerIdentityRequired, "a verified DayZ link is required")
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	observed, err := a.SaaSPlayer.HasObservedActivity(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		cancel()
		playerFailed(w, "check player installation association", err)
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	if !observed {
		cancel()
		writeSaaSError(w, codeNotFound, "installation not found")
		return repository.PlayerInstallationScope{}, nil, nil, false
	}
	return scope, ctx, cancel, true
}

// handlePlayerLives is GET .../player/servers/{installationID}/lives: the acting player's own life
// in progress, lifetime summary and most recent lives on that installation's server.
func (a *App) handlePlayerLives(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.Lives == nil {
		writeSaaSError(w, codeInternalError, "lives unavailable")
		return
	}
	limit := 10
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 50 {
			writeSaaSError(w, codeInvalidRequest, "limit must be between 1 and 50")
			return
		}
		limit = v
	}
	cur, err := a.Lives.CurrentLife(ctx, scope.GuildID, scope.ServerID, scope.PlayerID, time.Now())
	if err != nil {
		playerFailed(w, "current life", err)
		return
	}
	sum, err := a.Lives.Summary(ctx, scope.GuildID, scope.ServerID, scope.PlayerID)
	if err != nil {
		playerFailed(w, "life summary", err)
		return
	}
	recent, err := a.Lives.PlayerLives(ctx, scope.GuildID, scope.ServerID, scope.PlayerID, limit)
	if err != nil {
		playerFailed(w, "recent lives", err)
		return
	}
	resp := playerLivesResponseDTO{
		InstallationID: scope.InstallationID,
		Summary: lifeSummaryDTO{Lives: sum.Lives, LongestPlaytimeSeconds: sum.LongestPlaytime, AveragePlaytimeSeconds: sum.AveragePlaytime, MostKills: sum.MostKills,
			TrackedDistanceMeters: sum.TotalTrackedMeters, DeathsByPVP: sum.DeathsByPVP, DeathsBySuicide: sum.DeathsBySuicide, DeathsByOther: sum.DeathsByOther},
		Recent: make([]lifeDTO, 0, len(recent)),
	}
	if cur != nil {
		dto := toCurrentLifeDTO(*cur)
		resp.Current = &dto
	}
	for _, l := range recent {
		resp.Recent = append(resp.Recent, toLifeDTO(l))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}

type lifeLeaderboardResponseDTO struct {
	InstallationID int64            `json:"installationId"`
	Board          string           `json:"board"`
	Lives          []lifeDTO        `json:"lives"` // ended lives (boards LONGEST, KILLS, DISTANCE)
	Alive          []currentLifeDTO `json:"alive"` // lives in progress (board ALIVE)
	WindowDays     *int             `json:"windowDays"`
}

// lifeAliveWindow matches the Discord board: only players seen in the last two weeks rank as alive.
const lifeAliveWindow = 14 * 24 * time.Hour

// handleLifeLeaderboard is GET .../player/servers/{installationID}/lives/leaderboard?board=&days=.
// Open to any verified player of the server; it exposes names and life figures only, the same data
// the /life top Discord boards already show.
func (a *App) handleLifeLeaderboard(w http.ResponseWriter, r *http.Request) {
	scope, ctx, cancel, ok := a.resolvePlayerScope(w, r)
	if !ok {
		return
	}
	defer cancel()
	if a.Lives == nil {
		writeSaaSError(w, codeInternalError, "lives unavailable")
		return
	}
	q := r.URL.Query()
	board := strings.ToUpper(strings.TrimSpace(q.Get("board")))
	if board == "" {
		board = "ALIVE"
	}
	resp := lifeLeaderboardResponseDTO{InstallationID: scope.InstallationID, Board: board, Lives: []lifeDTO{}, Alive: []currentLifeDTO{}}
	now := time.Now().UTC()
	if board == "ALIVE" {
		alive, err := a.Lives.LongestAlive(ctx, scope.GuildID, scope.ServerID, now, now.Add(-lifeAliveWindow), 25)
		if err != nil {
			playerFailed(w, "life leaderboard", err)
			return
		}
		for _, c := range alive {
			resp.Alive = append(resp.Alive, toCurrentLifeDTO(c))
		}
		writeSaaSJSON(w, http.StatusOK, resp)
		return
	}
	metric, known := map[string]string{"LONGEST": repository.LifeMetricPlaytime, "KILLS": repository.LifeMetricKills, "DISTANCE": repository.LifeMetricDistance}[board]
	if !known {
		writeSaaSError(w, codeInvalidRequest, "board must be ALIVE, LONGEST, KILLS or DISTANCE")
		return
	}
	var since *time.Time
	if raw := strings.TrimSpace(q.Get("days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > 365 {
			writeSaaSError(w, codeInvalidRequest, "days must be between 1 and 365")
			return
		}
		t := now.Add(-time.Duration(days) * 24 * time.Hour)
		since, resp.WindowDays = &t, &days
	}
	lives, err := a.Lives.TopLives(ctx, scope.GuildID, scope.ServerID, metric, since, 25)
	if err != nil {
		playerFailed(w, "life leaderboard", err)
		return
	}
	for _, l := range lives {
		resp.Lives = append(resp.Lives, toLifeDTO(l))
	}
	writeSaaSJSON(w, http.StatusOK, resp)
}
