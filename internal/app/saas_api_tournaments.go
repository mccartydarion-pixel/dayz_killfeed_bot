package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

// Tournament mode over HTTP (docs/TOURNAMENTS.md): the public live shape the website's
// tournament page reads (bearer only, cached 3 s), the owner's routes (organization OWNER/ADMIN,
// a view-as session reads only) and the player's routes (a verified link).

const (
	tournamentPublicCacheTTL = 3 * time.Second
	tournamentMaxBody        = 64 << 10
	tournamentListLimit      = 50
	tournamentPlayerPast     = 10
)

func (a *App) registerTournamentRoutes() {
	if a.HTTPServer == nil {
		return
	}
	h := a.HTTPServer.Handle
	h("GET /api/saas/network/servers/{installationID}/tournament", a.handlePublicTournament)
	const owner = "/api/saas/organizations/{organizationID}/installations/{installationID}/tournaments"
	h("GET "+owner, a.handleOwnerTournamentList)
	h("POST "+owner, a.handleOwnerTournamentCreate)
	h("GET "+owner+"/{tournamentID}", a.handleOwnerTournamentGet)
	h("PATCH "+owner+"/{tournamentID}", a.handleOwnerTournamentPatch)
	for _, action := range []string{"open", "start", "pause", "resume", "cancel", "call"} {
		h("POST "+owner+"/{tournamentID}/"+action, a.ownerTournamentAction(action))
	}
	h("POST "+owner+"/{tournamentID}/matches/{matchID}/result", a.handleOwnerTournamentResult)
	h("POST "+owner+"/{tournamentID}/matches/{matchID}/replay", a.handleOwnerTournamentReplay)
	h("POST "+owner+"/{tournamentID}/entries/{entryID}/dq", a.handleOwnerTournamentDQ)
	const player = "/api/saas/player/servers/{installationID}/tournaments"
	h("GET "+player, a.handlePlayerTournaments)
	h("POST "+player+"/{tournamentID}/join", a.handlePlayerTournamentJoin)
	h("POST "+player+"/{tournamentID}/leave", a.handlePlayerTournamentLeave)
	h("POST "+player+"/{tournamentID}/checkin", a.handlePlayerTournamentCheckin)
}

// --- DTOs -------------------------------------------------------------------------------------------

// tournamentFlagDTO is a round that needs a ruling.
type tournamentFlagDTO struct {
	MatchID    int64   `json:"matchId"`
	N          int     `json:"n"`
	Flag       string  `json:"flag"`
	KillerName string  `json:"killerName"`
	VictimName string  `json:"victimName"`
	Weapon     string  `json:"weapon"`
	At         string  `json:"at"`
	Winner     *int64  `json:"winner"`
	DecidedBy  *string `json:"decidedBy"`
}

// ownerTournamentDTO is the public tournament plus the owner's extras.
type ownerTournamentDTO struct {
	*tournament.TournamentDTO
	Flags            []tournamentFlagDTO `json:"flags"`
	DiscordChannelID string              `json:"discordChannelId"`
	CreatedAt        string              `json:"createdAt"`
	UpdatedAt        string              `json:"updatedAt"`
}

func (a *App) ownerTournament(ctx context.Context, t *tournament.Tournament) ownerTournamentDTO {
	out := ownerTournamentDTO{TournamentDTO: tournament.ToDTO(t, a.tournamentPlayerInfo(ctx, t)), Flags: []tournamentFlagDTO{}, DiscordChannelID: t.DiscordChannelID,
		CreatedAt: rfc3339(t.CreatedAt), UpdatedAt: rfc3339(t.UpdatedAt)}
	for _, m := range t.Matches {
		if m.Finished() {
			continue
		}
		for _, r := range m.Rounds {
			if r.Flag == nil || *r.Flag == tournament.FlagManual {
				continue
			}
			out.Flags = append(out.Flags, tournamentFlagDTO{MatchID: m.ID, N: r.N, Flag: *r.Flag, KillerName: r.KillerName, VictimName: r.VictimName, Weapon: r.Weapon, At: rfc3339(r.At), Winner: r.WinnerEntry, DecidedBy: r.DecidedBy})
		}
	}
	return out
}

// tournamentBody is the create and edit body.
type tournamentBody struct {
	Name             string             `json:"name"`
	TeamSize         int                `json:"teamSize"`
	BracketSize      int                `json:"bracketSize"`
	BestOf           int                `json:"bestOf"`
	Seeding          string             `json:"seeding"`
	StartsAt         string             `json:"startsAt"`
	SignupOpensAt    *string            `json:"signupOpensAt"`
	CheckinMinutes   *int               `json:"checkinMinutes"`
	Rules            *tournament.Rules  `json:"rules"`
	Prizes           []tournament.Prize `json:"prizes"`
	DiscordChannelID *string            `json:"discordChannelId"`

	arenasGiven bool
	raw         map[string]json.RawMessage
}

func (a *App) readTournamentBody(w http.ResponseWriter, r *http.Request, body any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, tournamentMaxBody+1))
	if err != nil || len(raw) > tournamentMaxBody {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return false
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, body); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid request body")
		return false
	}
	if b, ok := body.(*tournamentBody); ok {
		_ = json.Unmarshal(raw, &b.raw)
		if rules, ok := b.raw["rules"]; ok {
			var probe map[string]json.RawMessage
			if json.Unmarshal(rules, &probe) == nil {
				_, b.arenasGiven = probe["arenas"]
			}
		}
	}
	return true
}

// params turns the body into engine parameters; base is the tournament being edited (nil on
// create). A field left out of an edit keeps its value.
func (a *App) tournamentParams(ctx context.Context, b *tournamentBody, base *tournament.Tournament, installationID int64) (tournament.Params, error) {
	p := tournament.Params{Name: b.Name, TeamSize: b.TeamSize, BracketSize: b.BracketSize, BestOf: b.BestOf, Seeding: b.Seeding, Prizes: b.Prizes, CheckinMinutes: tournament.DefaultCheckin}
	if base != nil {
		p = tournament.Params{Name: base.Name, TeamSize: base.TeamSize, BracketSize: base.BracketSize, BestOf: base.BestOf, Seeding: base.Seeding, StartsAt: base.StartsAt,
			SignupOpensAt: base.SignupOpensAt, CheckinMinutes: base.CheckinMinutes, Rules: base.Rules, Prizes: base.Prizes}
		if b.Name != "" {
			p.Name = b.Name
		}
		if b.TeamSize != 0 {
			p.TeamSize = b.TeamSize
		}
		if b.BracketSize != 0 {
			p.BracketSize = b.BracketSize
		}
		if b.BestOf != 0 {
			p.BestOf = b.BestOf
		}
		if b.Seeding != "" {
			p.Seeding = b.Seeding
		}
		if _, ok := b.raw["prizes"]; ok {
			p.Prizes = b.Prizes
		}
	}
	if b.StartsAt != "" {
		t, err := time.Parse(time.RFC3339, b.StartsAt)
		if err != nil {
			return p, fmt.Errorf("%w: startsAt must be an RFC 3339 time", tournament.ErrInvalid)
		}
		p.StartsAt = t.UTC()
	}
	if b.SignupOpensAt != nil {
		if *b.SignupOpensAt == "" {
			p.SignupOpensAt = nil
		} else {
			t, err := time.Parse(time.RFC3339, *b.SignupOpensAt)
			if err != nil {
				return p, fmt.Errorf("%w: signupOpensAt must be an RFC 3339 time", tournament.ErrInvalid)
			}
			u := t.UTC()
			p.SignupOpensAt = &u
		}
	}
	if b.CheckinMinutes != nil {
		p.CheckinMinutes = *b.CheckinMinutes
	}
	if b.Rules != nil {
		rules := *b.Rules
		if base != nil {
			if rules.MatchTimerMinutes == 0 {
				rules.MatchTimerMinutes = base.Rules.MatchTimerMinutes
			}
			if rules.ReadyMinutes == 0 {
				rules.ReadyMinutes = base.Rules.ReadyMinutes
			}
			if !b.arenasGiven {
				rules.Arenas = base.Rules.Arenas
			}
		}
		p.Rules = rules
	}
	if base == nil && !b.arenasGiven {
		p.Rules.Arenas = a.tournamentDefaultArenas(ctx, installationID)
	}
	return p, nil
}

// tournamentHTTPError maps engine errors to the error contract.
func tournamentHTTPError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, tournament.ErrNotFound), errors.Is(err, tournament.ErrMatchNotFound), errors.Is(err, tournament.ErrEntryNotFound):
		writeSaaSError(w, codeNotFound, "tournament not found")
	case errors.Is(err, tournament.ErrInvalid):
		writeSaaSError(w, codeValidationError, strings.TrimPrefix(err.Error(), tournament.ErrInvalid.Error()+": "))
	case errors.Is(err, tournament.ErrWrongStatus), errors.Is(err, tournament.ErrFull), errors.Is(err, tournament.ErrAlreadyEntered), errors.Is(err, tournament.ErrNotEntered),
		errors.Is(err, tournament.ErrPartner), errors.Is(err, tournament.ErrNoMatch):
		writeSaaSError(w, codeConflict, strings.TrimPrefix(err.Error(), tournament.ErrWrongStatus.Error()+": "))
	default:
		slog.Warn("component=tournament", "event", "api_failed", "action", action, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not "+action)
	}
}

// --- the public live shape ---------------------------------------------------------------------------

type tournamentPublicEntry struct {
	value *tournament.LiveDTO
	at    time.Time
}

var tournamentPublicCache struct {
	mu      sync.Mutex
	entries map[int64]tournamentPublicEntry
}

// buildLiveTournament assembles the public shape; nil when the installation has no server.
func (a *App) buildLiveTournament(ctx context.Context, installationID int64) (*tournament.LiveDTO, error) {
	target, err := a.Tournaments.Target(ctx, installationID)
	if err != nil || target == nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := &tournament.LiveDTO{InstallationID: installationID, Server: tournament.ServerDTO{Name: target.ServerName, Platform: target.Platform, Map: target.MapKey, DiscordInvite: target.DiscordInvite},
		Fighters: map[string]tournament.FighterDTO{}, GeneratedAt: rfc3339(now)}
	if n, err := a.Tournaments.OnlineCount(ctx, target.GuildID, target.ServerID); err == nil {
		out.Server.OnlinePlayers = n
	}
	id, err := a.Tournaments.Relevant(ctx, installationID, now)
	if err != nil || id == 0 {
		return out, err
	}
	t, err := a.Tournaments.Get(ctx, id)
	if err != nil {
		if errors.Is(err, tournament.ErrNotFound) {
			return out, nil
		}
		return nil, err
	}
	out.Tournament = tournament.ToDTO(t, a.tournamentPlayerInfo(ctx, t))
	var players []int64
	for _, e := range t.Entries {
		for _, p := range e.Players {
			players = append(players, p.PlayerID)
		}
	}
	stats, err := a.Tournaments.FighterStats(ctx, t.GuildID, t.ServerID, players)
	if err != nil {
		return nil, err
	}
	out.Fighters = tournament.Fighters(t, stats)
	return out, nil
}

// handlePublicTournament is GET /api/saas/network/servers/{installationID}/tournament (bearer
// only; the website serves it at /api/live/{installationID}/tournament). Cached 3 s.
func (a *App) handlePublicTournament(w http.ResponseWriter, r *http.Request) {
	if !a.requireSaaSServiceAuth(w, r) {
		return
	}
	if a.Tournaments == nil {
		writeSaaSError(w, codeInternalError, "tournaments unavailable")
		return
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return
	}
	tournamentPublicCache.mu.Lock()
	if tournamentPublicCache.entries == nil {
		tournamentPublicCache.entries = map[int64]tournamentPublicEntry{}
	}
	entry, hit := tournamentPublicCache.entries[installationID]
	tournamentPublicCache.mu.Unlock()
	if !hit || time.Since(entry.at) >= tournamentPublicCacheTTL {
		ctx, cancel := context.WithTimeout(r.Context(), networkTimeout)
		defer cancel()
		v, err := a.buildLiveTournament(ctx, installationID)
		if err != nil {
			slog.Warn("component=tournament", "event", "public_failed", "installation_id", installationID, "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not load the tournament")
			return
		}
		entry = tournamentPublicEntry{value: v, at: time.Now()}
		tournamentPublicCache.mu.Lock()
		if len(tournamentPublicCache.entries) >= networkCacheMax {
			tournamentPublicCache.entries = map[int64]tournamentPublicEntry{}
		}
		tournamentPublicCache.entries[installationID] = entry
		tournamentPublicCache.mu.Unlock()
	}
	if entry.value == nil {
		writeSaaSError(w, codeNotFound, "server not found")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3, s-maxage=3")
	writeSaaSJSON(w, http.StatusOK, entry.value)
}

// invalidateTournamentPublic drops the cached public shape of an installation.
func invalidateTournamentPublic(installationID int64) {
	tournamentPublicCache.mu.Lock()
	delete(tournamentPublicCache.entries, installationID)
	tournamentPublicCache.mu.Unlock()
}

// --- owner routes ------------------------------------------------------------------------------------

type tournamentOwnerRequest struct {
	user  *repository.AppUser
	scope repository.AdminScope
}

// tournamentOwnerContext is the stadium's chain: service auth (view-as reads only), acting user,
// OWNER/ADMIN of the organization, the installation's scope, the admin rate limits.
func (a *App) tournamentOwnerContext(w http.ResponseWriter, r *http.Request, write bool) (tournamentOwnerRequest, bool) {
	if a.Tournaments == nil || a.TournamentService == nil || a.ClientAdmin == nil {
		writeSaaSError(w, codeInternalError, "tournaments unavailable")
		return tournamentOwnerRequest{}, false
	}
	if !a.requireSaaSServiceAuth(w, r) {
		return tournamentOwnerRequest{}, false
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return tournamentOwnerRequest{}, false
	}
	orgID, ok := pathInt64(w, r, "organizationID")
	if !ok {
		return tournamentOwnerRequest{}, false
	}
	instID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return tournamentOwnerRequest{}, false
	}
	if _, ok := a.requireOrganizationRole(w, r, orgID, user.ID); !ok {
		return tournamentOwnerRequest{}, false
	}
	limiter := a.saasAdminReadLimiter
	if write {
		limiter = a.saasAdminActionLimiter
	}
	if !enforceRateLimit(w, limiter, rateLimitKey(r)) {
		return tournamentOwnerRequest{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	scope, err := a.ClientAdmin.Scope(ctx, orgID, instID)
	if errors.Is(err, repository.ErrInstallationScopeNotFound) {
		writeSaaSError(w, codeNotFound, "installation not found")
		return tournamentOwnerRequest{}, false
	}
	if err != nil {
		slog.Warn("component=tournament", "event", "scope_failed", "installation_id", instID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not resolve installation")
		return tournamentOwnerRequest{}, false
	}
	return tournamentOwnerRequest{user: user, scope: scope}, true
}

// ownerTournamentByPath loads the path's tournament and checks it belongs to the installation.
func (a *App) ownerTournamentByPath(w http.ResponseWriter, r *http.Request, ctx context.Context, sr tournamentOwnerRequest) (*tournament.Tournament, bool) {
	id, ok := pathInt64(w, r, "tournamentID")
	if !ok {
		return nil, false
	}
	t, err := a.Tournaments.Get(ctx, id)
	if err != nil || t.InstallationID != sr.scope.InstallationID {
		if err != nil && !errors.Is(err, tournament.ErrNotFound) {
			tournamentHTTPError(w, "load the tournament", err)
			return nil, false
		}
		writeSaaSError(w, codeNotFound, "tournament not found")
		return nil, false
	}
	return t, true
}

func (a *App) writeOwnerTournament(w http.ResponseWriter, ctx context.Context, status int, t *tournament.Tournament) {
	invalidateTournamentPublic(t.InstallationID)
	writeSaaSJSON(w, status, map[string]any{"tournament": a.ownerTournament(ctx, t)})
}

func (a *App) handleOwnerTournamentList(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	rows, err := a.Tournaments.ListForInstallation(ctx, sr.scope.InstallationID, tournamentListLimit)
	if err != nil {
		tournamentHTTPError(w, "list tournaments", err)
		return
	}
	items := make([]ownerTournamentDTO, 0, len(rows))
	for _, row := range rows {
		t, err := a.Tournaments.Get(ctx, row.ID)
		if err != nil {
			tournamentHTTPError(w, "list tournaments", err)
			return
		}
		items = append(items, a.ownerTournament(ctx, t))
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleOwnerTournamentCreate(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, true)
	if !ok {
		return
	}
	if sr.scope.ServerID == nil || *sr.scope.ServerID <= 0 {
		writeSaaSError(w, codeInvalidRequest, "select a DayZ server first")
		return
	}
	var body tournamentBody
	if !a.readTournamentBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	p, err := a.tournamentParams(ctx, &body, nil, sr.scope.InstallationID)
	if err != nil {
		tournamentHTTPError(w, "create the tournament", err)
		return
	}
	t := &tournament.Tournament{InstallationID: sr.scope.InstallationID, GuildID: sr.scope.GuildID, ServerID: *sr.scope.ServerID, CreatedByDiscordID: sr.user.DiscordUserID}
	if body.DiscordChannelID != nil {
		t.DiscordChannelID = strings.TrimSpace(*body.DiscordChannelID)
	} else if ch, err := a.Tournaments.EventsChannel(ctx, sr.scope.InstallationID); err == nil {
		t.DiscordChannelID = ch
	}
	created, err := a.TournamentService.Create(ctx, t, p)
	if err != nil {
		tournamentHTTPError(w, "create the tournament", err)
		return
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_CREATE", fmt.Sprintf("tournament:%d", created.ID), "", "success", nil, map[string]any{"name": created.Name, "startsAt": created.StartsAt})
	a.writeOwnerTournament(w, ctx, http.StatusCreated, created)
}

func (a *App) handleOwnerTournamentGet(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
	if !ok {
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"tournament": a.ownerTournament(ctx, t)})
}

func (a *App) handleOwnerTournamentPatch(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, true)
	if !ok {
		return
	}
	var body tournamentBody
	if !a.readTournamentBody(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
	if !ok {
		return
	}
	p, err := a.tournamentParams(ctx, &body, t, sr.scope.InstallationID)
	if err != nil {
		tournamentHTTPError(w, "update the tournament", err)
		return
	}
	updated, err := a.TournamentService.Update(ctx, t.ID, p)
	if err != nil {
		tournamentHTTPError(w, "update the tournament", err)
		return
	}
	if body.DiscordChannelID != nil {
		if err := a.TournamentService.SetMessages(ctx, t.ID, strings.TrimSpace(*body.DiscordChannelID), "", ""); err == nil {
			updated, _ = a.Tournaments.Get(ctx, t.ID)
		}
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_UPDATE", fmt.Sprintf("tournament:%d", t.ID), "", "success", nil, map[string]any{"name": updated.Name})
	a.writeOwnerTournament(w, ctx, http.StatusOK, updated)
}

// ownerTournamentAction is open, start, pause, resume, cancel and call.
func (a *App) ownerTournamentAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sr, ok := a.tournamentOwnerContext(w, r, true)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
		defer cancel()
		t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
		if !ok {
			return
		}
		var out *tournament.Tournament
		var err error
		switch action {
		case "open":
			out, err = a.TournamentService.Open(ctx, t.ID)
		case "start":
			out, err = a.TournamentService.Start(ctx, t.ID)
		case "pause":
			out, err = a.TournamentService.Pause(ctx, t.ID)
		case "resume":
			out, err = a.TournamentService.Resume(ctx, t.ID)
		case "cancel":
			out, err = a.TournamentService.Cancel(ctx, t.ID, "An admin cancelled the tournament.")
		case "call":
			out, err = a.TournamentService.Call(ctx, t.ID)
		}
		if err != nil {
			tournamentHTTPError(w, action+" the tournament", err)
			return
		}
		a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_"+strings.ToUpper(action), fmt.Sprintf("tournament:%d", t.ID), "", "success", nil, map[string]any{"status": out.Status})
		a.writeOwnerTournament(w, ctx, http.StatusOK, out)
	}
}

type tournamentResultBody struct {
	WinnerEntryID int64  `json:"winnerEntryId"`
	Note          string `json:"note"`
}

func (a *App) handleOwnerTournamentResult(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, true)
	if !ok {
		return
	}
	var body tournamentResultBody
	if !a.readTournamentBody(w, r, &body) {
		return
	}
	matchID, ok := pathInt64(w, r, "matchID")
	if !ok {
		return
	}
	if body.WinnerEntryID <= 0 {
		writeSaaSError(w, codeValidationError, "winnerEntryId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
	if !ok {
		return
	}
	out, err := a.TournamentService.Result(ctx, t.ID, matchID, body.WinnerEntryID, sr.user.DiscordUserID, strings.TrimSpace(body.Note))
	if err != nil {
		tournamentHTTPError(w, "record the result", err)
		return
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_RESULT", fmt.Sprintf("tournament:%d:match:%d", t.ID, matchID), body.Note, "success", nil, map[string]any{"winnerEntryId": body.WinnerEntryID})
	a.writeOwnerTournament(w, ctx, http.StatusOK, out)
}

func (a *App) handleOwnerTournamentReplay(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, true)
	if !ok {
		return
	}
	matchID, ok := pathInt64(w, r, "matchID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
	if !ok {
		return
	}
	out, err := a.TournamentService.Replay(ctx, t.ID, matchID, sr.user.DiscordUserID)
	if err != nil {
		tournamentHTTPError(w, "replay the match", err)
		return
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_REPLAY", fmt.Sprintf("tournament:%d:match:%d", t.ID, matchID), "", "success", nil, nil)
	a.writeOwnerTournament(w, ctx, http.StatusOK, out)
}

type tournamentDQBody struct {
	Reason string `json:"reason"`
}

func (a *App) handleOwnerTournamentDQ(w http.ResponseWriter, r *http.Request) {
	sr, ok := a.tournamentOwnerContext(w, r, true)
	if !ok {
		return
	}
	var body tournamentDQBody
	if !a.readTournamentBody(w, r, &body) {
		return
	}
	entryID, ok := pathInt64(w, r, "entryID")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	t, ok := a.ownerTournamentByPath(w, r, ctx, sr)
	if !ok {
		return
	}
	out, err := a.TournamentService.DQ(ctx, t.ID, entryID, sr.user.DiscordUserID, strings.TrimSpace(body.Reason))
	if err != nil {
		tournamentHTTPError(w, "disqualify the entry", err)
		return
	}
	a.recordAudit(ctx, adminActor{user: sr.user, scope: sr.scope}, "TOURNAMENT_DQ", fmt.Sprintf("tournament:%d:entry:%d", t.ID, entryID), body.Reason, "success", nil, nil)
	a.writeOwnerTournament(w, ctx, http.StatusOK, out)
}

// --- player routes -----------------------------------------------------------------------------------

type playerTournamentMatchDTO struct {
	MatchID   int64   `json:"matchId"`
	RoundName string  `json:"roundName"`
	Status    string  `json:"status"`
	Opponent  *string `json:"opponent"`
	Arena     *int    `json:"arena"`
	ScoreMe   int     `json:"scoreMe"`
	ScoreThem int     `json:"scoreThem"`
}

type playerTournamentMeDTO struct {
	EntryID   int64                     `json:"entryId"`
	Status    string                    `json:"status"`
	CheckedIn bool                      `json:"checkedIn"`
	Seed      *int                      `json:"seed"`
	NextMatch *playerTournamentMatchDTO `json:"nextMatch"`
	Place     int                       `json:"place"`
}

type playerTournamentPastDTO struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	StartsAt   string  `json:"startsAt"`
	FinishedAt *string `json:"finishedAt"`
	Champion   *string `json:"champion"`
	MyPlace    int     `json:"myPlace"`
}

type playerTournamentsDTO struct {
	InstallationID int64                     `json:"installationId"`
	Linked         bool                      `json:"linked"`
	Current        *tournament.TournamentDTO `json:"current"`
	Me             *playerTournamentMeDTO    `json:"me"`
	Past           []playerTournamentPastDTO `json:"past"`
}

type playerTournamentRequest struct {
	scope  repository.PlayerInstallationScope
	linked bool
	user   *repository.AppUser
}

// playerTournamentContext: service auth, acting user, the installation (404 when unknown or
// without a server). linked says whether the user holds a VERIFIED link for its guild.
func (a *App) playerTournamentContext(w http.ResponseWriter, r *http.Request) (playerTournamentRequest, bool) {
	if a.Tournaments == nil || a.TournamentService == nil || a.SaaSPlayer == nil {
		writeSaaSError(w, codeInternalError, "tournaments unavailable")
		return playerTournamentRequest{}, false
	}
	if !a.requireSaaSServiceAuth(w, r) {
		return playerTournamentRequest{}, false
	}
	user := a.resolveActingUser(w, r)
	if user == nil {
		return playerTournamentRequest{}, false
	}
	installationID, ok := pathInt64(w, r, "installationID")
	if !ok {
		return playerTournamentRequest{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	scope, found, linked, err := a.SaaSPlayer.ResolvePlayerInstallation(ctx, installationID, user.DiscordUserID)
	if err != nil {
		playerFailed(w, "resolve player installation", err)
		return playerTournamentRequest{}, false
	}
	if !found {
		writeSaaSError(w, codeNotFound, "installation not found")
		return playerTournamentRequest{}, false
	}
	return playerTournamentRequest{scope: scope, linked: linked, user: user}, true
}

func (a *App) playerMe(t *tournament.Tournament, discordUserID string) *playerTournamentMeDTO {
	e := t.EntryOfDiscordUser(discordUserID)
	if e == nil {
		return nil
	}
	me := &playerTournamentMeDTO{EntryID: e.ID, Status: e.Status, CheckedIn: e.CheckedIn(), Seed: e.Seed, Place: t.Place(e.ID)}
	for _, m := range t.Matches {
		if !m.Has(e.ID) || m.Finished() {
			continue
		}
		opp := m.EntryB
		scoreMe, scoreThem := m.ScoreA, m.ScoreB
		if opp != nil && *opp == e.ID {
			opp = m.EntryA
			scoreMe, scoreThem = m.ScoreB, m.ScoreA
		}
		next := &playerTournamentMatchDTO{MatchID: m.ID, RoundName: m.RoundName, Status: m.Status, Arena: m.ArenaNo, ScoreMe: scoreMe, ScoreThem: scoreThem}
		if opp != nil {
			if oe := t.Entry(*opp); oe != nil {
				names := make([]string, 0, len(oe.Players))
				for _, p := range oe.Players {
					names = append(names, p.Name)
				}
				s := strings.Join(names, " & ")
				next.Opponent = &s
			}
		}
		me.NextMatch = next
		break
	}
	return me
}

// handlePlayerTournaments is GET /api/saas/player/servers/{installationID}/tournaments.
func (a *App) handlePlayerTournaments(w http.ResponseWriter, r *http.Request) {
	pr, ok := a.playerTournamentContext(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	out := playerTournamentsDTO{InstallationID: pr.scope.InstallationID, Linked: pr.linked, Past: []playerTournamentPastDTO{}}
	if id, err := a.Tournaments.Current(ctx, pr.scope.InstallationID); err == nil && id > 0 {
		if t, err := a.Tournaments.Get(ctx, id); err == nil {
			out.Current = tournament.ToDTO(t, a.tournamentPlayerInfo(ctx, t))
			out.Me = a.playerMe(t, pr.user.DiscordUserID)
		}
	}
	rows, err := a.Tournaments.ListForInstallation(ctx, pr.scope.InstallationID, tournamentListLimit)
	if err != nil {
		playerFailed(w, "list tournaments", err)
		return
	}
	for _, row := range rows {
		if !row.Over() || len(out.Past) >= tournamentPlayerPast {
			continue
		}
		t, err := a.Tournaments.Get(ctx, row.ID)
		if err != nil {
			continue
		}
		past := playerTournamentPastDTO{ID: t.ID, Name: t.Name, Status: t.Status, StartsAt: rfc3339(t.StartsAt), FinishedAt: nullableTimeStr(t.FinishedAt)}
		if c := t.Champion(); c != nil {
			names := make([]string, 0, len(c.Players))
			for _, p := range c.Players {
				names = append(names, p.Name)
			}
			s := strings.Join(names, " & ")
			past.Champion = &s
		}
		if e := t.EntryOfDiscordUser(pr.user.DiscordUserID); e != nil {
			past.MyPlace = t.Place(e.ID)
		}
		out.Past = append(out.Past, past)
	}
	writeSaaSJSON(w, http.StatusOK, out)
}

// playerTournamentWrite is the shared start of join, leave and check in: a VERIFIED link and the
// path's tournament on this installation.
func (a *App) playerTournamentWrite(w http.ResponseWriter, r *http.Request) (playerTournamentRequest, *tournament.Tournament, bool) {
	pr, ok := a.playerTournamentContext(w, r)
	if !ok {
		return pr, nil, false
	}
	if !pr.linked {
		writeSaaSError(w, codePlayerIdentityRequired, "a verified DayZ link is required")
		return pr, nil, false
	}
	id, ok := pathInt64(w, r, "tournamentID")
	if !ok {
		return pr, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	t, err := a.Tournaments.Get(ctx, id)
	if err != nil || t.InstallationID != pr.scope.InstallationID {
		writeSaaSError(w, codeNotFound, "tournament not found")
		return pr, nil, false
	}
	return pr, t, true
}

func (a *App) writePlayerTournament(w http.ResponseWriter, ctx context.Context, pr playerTournamentRequest, t *tournament.Tournament) {
	invalidateTournamentPublic(t.InstallationID)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"tournament": tournament.ToDTO(t, a.tournamentPlayerInfo(ctx, t)), "me": a.playerMe(t, pr.user.DiscordUserID)})
}

type playerJoinBody struct {
	PartnerDiscordID string `json:"partnerDiscordId"`
}

func (a *App) handlePlayerTournamentJoin(w http.ResponseWriter, r *http.Request) {
	var body playerJoinBody
	if !a.readTournamentBody(w, r, &body) {
		return
	}
	pr, t, ok := a.playerTournamentWrite(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	_, name, err := a.Tournaments.LinkedPlayer(ctx, pr.scope.GuildID, pr.user.DiscordUserID)
	if err != nil {
		playerFailed(w, "resolve player", err)
		return
	}
	players := []tournament.EntryPlayer{{PlayerID: pr.scope.PlayerID, DiscordUserID: pr.user.DiscordUserID, Name: name}}
	if partner := strings.TrimSpace(body.PartnerDiscordID); partner != "" {
		if partner == pr.user.DiscordUserID {
			writeSaaSError(w, codeValidationError, "a partner must be another player")
			return
		}
		pid, pname, err := a.Tournaments.LinkedPlayer(ctx, pr.scope.GuildID, partner)
		if err != nil {
			playerFailed(w, "resolve partner", err)
			return
		}
		if pid == 0 {
			writeSaaSError(w, codeValidationError, "your partner has no verified DayZ link on this server")
			return
		}
		players = append(players, tournament.EntryPlayer{PlayerID: pid, DiscordUserID: partner, Name: pname})
	}
	out, _, err := a.TournamentService.Join(ctx, t.ID, players)
	if err != nil {
		tournamentHTTPError(w, "join the tournament", err)
		return
	}
	a.writePlayerTournament(w, ctx, pr, out)
}

func (a *App) handlePlayerTournamentLeave(w http.ResponseWriter, r *http.Request) {
	pr, t, ok := a.playerTournamentWrite(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	out, err := a.TournamentService.Leave(ctx, t.ID, pr.user.DiscordUserID)
	if err != nil {
		tournamentHTTPError(w, "leave the tournament", err)
		return
	}
	a.writePlayerTournament(w, ctx, pr, out)
}

func (a *App) handlePlayerTournamentCheckin(w http.ResponseWriter, r *http.Request) {
	pr, t, ok := a.playerTournamentWrite(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), playerTimeout)
	defer cancel()
	out, err := a.TournamentService.Checkin(ctx, t.ID, pr.user.DiscordUserID)
	if err != nil {
		tournamentHTTPError(w, "check in", err)
		return
	}
	a.writePlayerTournament(w, ctx, pr, out)
}
