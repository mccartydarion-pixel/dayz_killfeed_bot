package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/routing"
	"github.com/yourname/dayz-killfeed/internal/stadium"
	"github.com/yourname/dayz-killfeed/internal/tournament"
	"github.com/yourname/dayz-killfeed/internal/tournamentcard"
)

// Tournament mode (docs/TOURNAMENTS.md): the engine (internal/tournament) wired to the kill
// pipeline, the economy, Discord and the scheduler. The HTTP routes are in
// saas_api_tournaments.go.

// tournamentTickEvery is how often the leader runs the tournament clock.
const tournamentTickEvery = 5 * time.Second

// economyPayer pays tournament prizes as earned SYSTEM_REWARD credits; the reference makes a
// repeat a no-op.
type economyPayer struct{ svc *economy.Service }

func (p economyPayer) Pay(ctx context.Context, guildID, serverID, playerID, amount int64, reference, description string) (int64, error) {
	if p.svc == nil {
		return 0, errors.New("economy unavailable")
	}
	res, err := p.svc.Credit(ctx, economy.Request{GuildID: guildID, ServerID: serverID, PlayerID: playerID, Amount: amount, Type: economy.TypeSystemReward, ReferenceID: reference, Description: description})
	if err != nil {
		return 0, err
	}
	return res.TransactionID, nil
}

// newTournamentService builds the engine over the repositories.
func (a *App) newTournamentService() *tournament.Service {
	if a.Tournaments == nil {
		return nil
	}
	repo := a.Tournaments
	ranker := func(ctx context.Context, t *tournament.Tournament) []tournament.Rank {
		var players []int64
		for _, e := range t.Entries {
			for _, p := range e.Players {
				players = append(players, p.PlayerID)
			}
		}
		rp, err := repo.RankedRP(ctx, t.GuildID, t.ServerID, players)
		if err != nil {
			slog.Warn("component=tournament", "event", "ranked_rp_failed", "err", err.Error())
			return nil
		}
		var out []tournament.Rank
		for _, e := range t.Entries {
			r := tournament.Rank{EntryID: e.ID}
			for _, p := range e.Players {
				r.RP += rp[p.PlayerID]
			}
			out = append(out, r)
		}
		return out
	}
	presence := func(ctx context.Context, t *tournament.Tournament, players []int64) map[int64]bool {
		present, err := repo.Present(ctx, t.GuildID, t.ServerID, players)
		if err != nil {
			slog.Warn("component=tournament", "event", "presence_failed", "err", err.Error())
			return nil
		}
		return present
	}
	var payer tournament.Payer
	if a.EconomyService != nil {
		payer = economyPayer{a.EconomyService}
	}
	return tournament.NewService(repo, payer, ranker, presence)
}

// tournamentTiers is each entry player's Ranked tier on the server, for the images.
func (a *App) tournamentTiers(ctx context.Context, t *tournament.Tournament) map[int64]ranked.Tier {
	out := map[int64]ranked.Tier{}
	if a.Ranked == nil {
		return out
	}
	for _, e := range t.Entries {
		for _, p := range e.Players {
			prog, err := a.Ranked.ServerPlayerProgress(ctx, t.GuildID, t.ServerID, p.PlayerID)
			if err == nil {
				out[p.PlayerID] = prog.Tier
			}
		}
	}
	return out
}

// tournamentPlayerInfo is the tier and faction of each entry player (the public shape).
func (a *App) tournamentPlayerInfo(ctx context.Context, t *tournament.Tournament) map[int64]tournament.PlayerInfo {
	out := map[int64]tournament.PlayerInfo{}
	tiers := a.tournamentTiers(ctx, t)
	var players []int64
	for _, e := range t.Entries {
		for _, p := range e.Players {
			players = append(players, p.PlayerID)
		}
	}
	factions := map[int64]tournament.FactionDTO{}
	if a.Tournaments != nil {
		if f, err := a.Tournaments.PlayerFactions(ctx, t.InstallationID, t.GuildID, players); err == nil {
			factions = f
		}
	}
	for _, p := range players {
		info := tournament.PlayerInfo{RankTier: string(tiers[p])}
		if f, ok := factions[p]; ok {
			ff := f
			info.Faction = &ff
		}
		out[p] = info
	}
	return out
}

// tournamentServerName is the display name of the tournament's server.
func (a *App) tournamentServerName(ctx context.Context, t *tournament.Tournament) string {
	if fn := a.serverNameFunc(); fn != nil {
		return fn(t.ServerID)
	}
	return ""
}

// tournamentAdminChannel is where admin pings go: the installation's ADMIN_ALERTS route, else "".
func (a *App) tournamentAdminChannel(ctx context.Context, t *tournament.Tournament) string {
	if a.ChannelRoutes == nil {
		return ""
	}
	ch, found, err := a.ChannelRoutes.Resolve(ctx, t.GuildID, t.ServerID, routing.RouteAdminAlerts)
	if err != nil || !found {
		return ""
	}
	return ch
}

// tournamentDefaultArenas is the arena a new tournament gets: the built stadium's centre with a
// radius that covers it (nothing when no stadium is built).
func (a *App) tournamentDefaultArenas(ctx context.Context, installationID int64) []tournament.Arena {
	if a.Stadium == nil || a.Tournaments == nil {
		return nil
	}
	target, err := a.Tournaments.Target(ctx, installationID)
	if err != nil || target == nil {
		return nil
	}
	row, err := a.Stadium.Get(ctx, target.OrganizationID, installationID)
	if err != nil || row == nil || row.Status != repository.StadiumBuilt {
		return nil
	}
	p, err := stadium.Parse(row.Params)
	if err != nil {
		return nil
	}
	layout, err := stadium.Build(p)
	if err != nil {
		return nil
	}
	radius := 0.0
	for _, c := range layout.Preview.Footprint {
		radius = math.Max(radius, math.Hypot(c[0]-p.CenterX, c[1]-p.CenterZ))
	}
	radius = math.Ceil(radius) + 10
	if radius < 50 {
		radius = 50
	}
	return []tournament.Arena{{No: 1, Name: "Stadium", X: math.Round(p.CenterX), Z: math.Round(p.CenterZ), Radius: radius}}
}

// renderTournamentBracket draws the bracket for /tournament bracket.
func (a *App) renderTournamentBracket(ctx context.Context, t *tournament.Tournament) ([]byte, error) {
	return tournamentcard.Render(tournamentcard.FromTournament(t, a.tournamentTiers(ctx, t), a.tournamentServerName(ctx, t), a.siteHost()))
}

// registerTournamentCommand wires /tournament, its buttons and the Discord announcer. Called
// from Run before the server workers start.
func (a *App) registerTournamentCommand(session *discordgo.Session, commands discord.CommandRegistrar) {
	if a.TournamentService == nil || a.Tournaments == nil || a.Guilds == nil || a.Discord == nil || session == nil || a.Config.DiscordGuildID == "" {
		return
	}
	repo := a.Tournaments
	announcer := discord.NewTournamentAnnouncer(discord.SessionTournamentSender{S: session},
		func(ctx context.Context, id int64, channelID, signupMessageID, bracketMessageID string) error {
			return a.TournamentService.SetMessages(ctx, id, channelID, signupMessageID, bracketMessageID)
		}, a.tournamentTiers, a.tournamentAdminChannel, a.tournamentServerName, a.siteHost())
	a.TournamentService.SetNotifier(announcer)
	target := func(ctx context.Context, serverID int64) (int64, string) {
		t, err := repo.TargetForServer(ctx, serverID)
		if err != nil || t == nil {
			return 0, ""
		}
		return t.InstallationID, t.ServerName
	}
	handler := discord.NewTournamentCommandHandler(a.TournamentService, a.Guilds, a.publicServerID, target, repo.CurrentForServer, a.linkedPlayer, a.tournamentDefaultArenas, a.renderTournamentBracket)
	handler.SetDraftLookup(repo.LatestDraftForServer)
	if err := discord.RegisterTournamentCommands(commands, a.Config.DiscordGuildID); err != nil {
		slog.Warn("component=discord", "msg", "failed to register tournament commands", "err", err.Error())
	} else {
		slog.Info("component=discord", "msg", "tournament commands queued")
	}
	routes := a.Discord.Interactions()
	// The bracket is posted for the channel; everything else answers privately.
	routes.Command("tournament", discord.AckPrivate, handler.Handle, discord.SubAck{Path: "bracket", Ack: discord.AckPublic})
	routes.ComponentPrefix(discord.TournamentComponentPrefix, discord.AckPrivate, handler.HandleComponent)
}

// startTournamentScheduler runs the tournament clock on the leader: sign-up and check-in
// opening, the start, the ready and match timers.
func (a *App) startTournamentScheduler(ctx context.Context) {
	if a.TournamentService == nil {
		return
	}
	go a.singleton(ctx, "tournament_scheduler", func(ctx context.Context) {
		t := time.NewTicker(tournamentTickEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tickCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				a.TournamentService.Tick(tickCtx)
				cancel()
			}
		}
	})
}

// --- the kill pipeline --------------------------------------------------------------------------------

// tournamentKill hands a persisted kill to the live tournament of its server.
func (p *persistenceStoreAdapter) tournamentKill(ctx context.Context, killID int64, record repository.KillRecord, ev *killfeed.Event) {
	if p.tournaments == nil || record.ServerID == 0 || record.KillerPlayerID == 0 || record.VictimPlayerID == 0 {
		return
	}
	k := tournament.Kill{KillID: killID, KillerPlayerID: record.KillerPlayerID, VictimPlayerID: record.VictimPlayerID, Weapon: record.WeaponDisplay, Distance: record.Distance, At: time.Now().UTC()}
	if record.EventTime != nil {
		k.At = *record.EventTime
	}
	if ev != nil {
		if ev.Killer != nil {
			k.KillerName = ev.Killer.Name
			if ev.Killer.Position != nil {
				x, z := ev.Killer.Position.MapX(), ev.Killer.Position.MapZ()
				k.KillerX, k.KillerZ = &x, &z
			}
		}
		if ev.Victim != nil {
			k.VictimName = ev.Victim.Name
		}
	}
	p.tournaments.OnKill(ctx, record.ServerID, k)
}

// tournamentDeath hands a persisted non-player death to the live tournament of its server.
func (p *persistenceStoreAdapter) tournamentDeath(ctx context.Context, record repository.DeathRecord, ev *killfeed.Event) {
	if p.tournaments == nil || record.ServerID == 0 || record.PlayerID == 0 {
		return
	}
	d := tournament.Death{PlayerID: record.PlayerID, At: time.Now().UTC()}
	if record.EventTime != nil {
		d.At = *record.EventTime
	}
	if ev != nil {
		if ev.Player != nil {
			d.Name = ev.Player.Name
		}
		if ev.Cause != "" {
			d.Cause = fmt.Sprint(ev.Cause)
		}
	}
	p.tournaments.OnDeath(ctx, record.ServerID, d)
}
