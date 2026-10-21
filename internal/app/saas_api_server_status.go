package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Server status for server owners (docs/SERVER_STATUS.md): "is my killfeed working", answered
// with plain facts for one installation. Read-only. Everything comes from memory or from rows
// Champion already keeps; this route never calls Nitrado or Discord, and never returns a token, a
// credential, a log path or anything belonging to another installation.
//
// The JSON shape below is a contract the website is built against. Add fields; do not rename,
// retype or remove them.

type serverStatusFeedDTO struct {
	State  string `json:"state"` // RUNNING | QUIET | DEGRADED | DOWN
	Reason string `json:"reason"`
}

type serverStatusPlayersDTO struct {
	Online *int   `json:"online"` // null while unknown; never a guess
	Source string `json:"source"` // NITRADO_QUERY | NITRADO_SERVER_STOPPED | ADM_PLAYER_LIST | ADM_BOOT_RESET | UNKNOWN
}

type serverStatusNitradoDTO struct {
	Reachable     *bool   `json:"reachable"` // null when Champion has not tried yet
	LastSuccessAt *string `json:"lastSuccessAt"`
	LastFailureAt *string `json:"lastFailureAt"`
	ErrorClass    *string `json:"errorClass"` // set only while not reachable
}

type serverStatusDiscordProblemDTO struct {
	Feed      string  `json:"feed"`    // the Champion feed, e.g. KILLFEED
	Problem   string  `json:"problem"` // CHANNEL_OR_PERMISSION | FAILING
	ChannelID *string `json:"channelId"`
	Since     *string `json:"since"`
}

type serverStatusDiscordDTO struct {
	Reachable  bool                            `json:"reachable"`
	BotInGuild bool                            `json:"botInGuild"`
	Problems   []serverStatusDiscordProblemDTO `json:"problems"`
}

type serverStatusPositionDTO struct {
	State                string  `json:"state"` // OK | NOT_ARRIVING | UNKNOWN
	LastPositionAt       *string `json:"lastPositionAt"`
	PlayersOnlineMinutes int     `json:"playersOnlineMinutes"`
	Advice               *string `json:"advice"`
}

type serverStatusMapRotationDTO struct {
	CurrentMap    *string `json:"currentMap"`
	NextMap       *string `json:"nextMap"`
	SwitchAt      *string `json:"switchAt"`
	VoteOpen      bool    `json:"voteOpen"`
	Stopped       bool    `json:"stopped"`
	StoppedReason *string `json:"stoppedReason"`
}

type serverStatusResponse struct {
	InstallationID   int64                       `json:"installationId"`
	ServerID         *int64                      `json:"serverId"`
	ServerName       *string                     `json:"serverName"`
	CheckedAt        string                      `json:"checkedAt"`
	Feed             serverStatusFeedDTO         `json:"feed"`
	LastLogCheckAt   *string                     `json:"lastLogCheckAt"`
	LastLogLineAt    *string                     `json:"lastLogLineAt"`
	LastKillAt       *string                     `json:"lastKillAt"`
	LastKillPostedAt *string                     `json:"lastKillPostedAt"`
	Players          serverStatusPlayersDTO      `json:"players"`
	Nitrado          serverStatusNitradoDTO      `json:"nitrado"`
	Discord          serverStatusDiscordDTO      `json:"discord"`
	PositionLogging  serverStatusPositionDTO     `json:"positionLogging"`
	MapRotation      *serverStatusMapRotationDTO `json:"mapRotation"`
}

// serverStatusInputs is everything the response is built from, gathered by the handler so the
// building itself is a pure function.
type serverStatusInputs struct {
	Now          time.Time
	Fact         repository.FleetFact
	Served       bool // this process serves the installation's Discord server
	RuntimeReady bool
	Runtime      serverRuntime
	Watch        feedWatchView
	// DiscordConnected is this process's gateway connection.
	DiscordConnected bool
	Deliveries       []discord.RouteDelivery
	MapRotation      *serverStatusMapRotationDTO
}

const killfeedDeliveryRoute = "KILLFEED"

func buildServerStatus(in serverStatusInputs) serverStatusResponse {
	f := in.Fact
	out := serverStatusResponse{InstallationID: f.InstallationID, CheckedAt: in.Now.UTC().Format(time.RFC3339),
		Players:     serverStatusPlayersDTO{Source: counterSourceUnknown},
		Discord:     serverStatusDiscordDTO{Reachable: in.DiscordConnected, BotInGuild: f.BotInstalled, Problems: []serverStatusDiscordProblemDTO{}},
		MapRotation: in.MapRotation, LastKillAt: nullableTimeStr(f.LastKillAt)}
	if f.ServerID > 0 {
		id := f.ServerID
		out.ServerID = &id
		out.ServerName = nullableString(f.ServerName)
	}
	if !in.Served {
		// Another Champion process owns this Discord server: this one has no worker for it and
		// must not describe its own.
		in.Runtime, in.Watch, in.Deliveries = serverRuntime{}, feedWatchView{}, nil
	}

	state := serverState(f, in.Runtime, in.RuntimeReady)
	sample := in.Watch.Sample
	if in.Watch.Watching {
		state.PlayersKnown, state.PlayersOnline, state.ServerStopped = sample.PlayersKnown, sample.PlayersOnline, sample.ServerStopped
		state.PlayersOnlineFor, state.LogSilentFor, state.SourceBadFor, state.PlayerListSeen = in.Watch.PlayersOnlineFor, in.Watch.LogSilentFor, in.Watch.SourceBadFor, in.Watch.PlayerListSeen
		out.LastLogCheckAt, out.LastLogLineAt = nullableTime(sample.LastLogCheckAt), nullableTime(sample.LastLogLineAt)
		if sample.PlayersKnown {
			n := sample.PlayersOnline
			out.Players.Online = &n
		}
		if sample.PlayersSource != "" {
			out.Players.Source = sample.PlayersSource
		}
	}

	killfeedDelivery := ""
	for _, d := range in.Deliveries {
		st := d.State()
		if d.Route == killfeedDeliveryRoute {
			killfeedDelivery = st
			out.LastKillPostedAt = nullableTime(d.LastSuccessAt)
		}
		if st != discord.DeliveryConfigFault && st != "FAILING" {
			continue
		}
		p := serverStatusDiscordProblemDTO{Feed: d.Route, Problem: "FAILING", ChannelID: nullableString(d.ChannelID), Since: nullableTime(d.LastFailureAt)}
		if st == discord.DeliveryConfigFault {
			p.Problem = "CHANNEL_OR_PERMISSION"
		}
		out.Discord.Problems = append(out.Discord.Problems, p)
	}

	out.Feed.State, out.Feed.Reason = ownerops.ServerStatus(ownerops.StatusInput{Server: state, ServerSelected: f.ServerID > 0, KillfeedDelivery: killfeedDelivery})

	// Nitrado: what the worker sees now wins over the last recorded check of the connection.
	out.Nitrado.LastSuccessAt, out.Nitrado.LastFailureAt = nullableTimeStr(f.NitradoLastOKAt), nullableTimeStr(f.NitradoLastFailAt)
	switch {
	case in.Runtime.WorkerRunning && in.Runtime.SourceState == "TRANSPORT_ERROR":
		no := false
		out.Nitrado.Reachable = &no
		out.Nitrado.ErrorClass = nullableString(in.Runtime.ErrorClass)
		if out.Nitrado.ErrorClass == nil {
			out.Nitrado.ErrorClass = nullableString("unknown")
		}
	case in.Runtime.WorkerRunning && in.Runtime.LastCycleAt != nil && in.Runtime.SourceState != "WORKER_STALLED":
		yes := true
		out.Nitrado.Reachable = &yes
		out.Nitrado.LastSuccessAt = nullableTimeStr(in.Runtime.LastCycleAt)
	case f.NitradoStatus != "" && (f.NitradoLastOKAt != nil || f.NitradoLastFailAt != nil):
		ok := f.NitradoLastOKAt != nil && (f.NitradoLastFailAt == nil || f.NitradoLastOKAt.After(*f.NitradoLastFailAt))
		out.Nitrado.Reachable = &ok
		if !ok {
			out.Nitrado.ErrorClass = nullableString(f.NitradoErrorClass)
		}
	}

	pos := ownerops.PositionInput{WorkerRunning: in.Runtime.WorkerRunning && in.Watch.Watching, PlayersKnown: sample.PlayersKnown, PlayersOnline: sample.PlayersOnline,
		PlayersOnlineFor: in.Watch.PlayersOnlineFor, PlayerListSeen: in.Watch.PlayerListSeen}
	if in.Watch.PlayerListSeen {
		pos.SinceLastPlayerList = in.Now.Sub(sample.LastPlayerList)
		out.PositionLogging.LastPositionAt = nullableTime(sample.LastPlayerList)
	}
	advice := ""
	out.PositionLogging.State, advice = ownerops.PositionLogging(pos)
	out.PositionLogging.Advice = nullableString(advice)
	out.PositionLogging.PlayersOnlineMinutes = int(in.Watch.PlayersOnlineFor / time.Minute)
	return out
}

// serverStatusFactsTTL bounds how often the fleet query runs for this route: a dashboard polling
// several installations shares one read.
const serverStatusFactsTTL = 20 * time.Second

// serverStatusFact returns the stored facts of one installation. It reuses the Owner Hub's fleet
// read (one bounded statement), cached briefly, and hands back only the requested installation.
func (a *App) serverStatusFact(ctx context.Context, installationID int64, now time.Time) (repository.FleetFact, bool, error) {
	a.serverStatusMu.Lock()
	defer a.serverStatusMu.Unlock()
	if a.serverStatusFacts == nil || now.Sub(a.serverStatusFactsAt) > serverStatusFactsTTL {
		facts, err := a.PlatformOps.FleetFacts(ctx, now, true)
		if err != nil {
			return repository.FleetFact{}, false, err
		}
		byID := make(map[int64]repository.FleetFact, len(facts))
		for _, f := range facts {
			byID[f.InstallationID] = f
		}
		a.serverStatusFacts, a.serverStatusFactsAt = byID, now
	}
	f, ok := a.serverStatusFacts[installationID]
	return f, ok, nil
}

// serverStatusMapRotation is the map rotation block: null unless the feature is available to the
// installation and the owner switched it on.
func (a *App) serverStatusMapRotation(ctx context.Context, scope repository.AdminScope, now time.Time) *serverStatusMapRotationDTO {
	if a.MapRotation == nil {
		return nil
	}
	if reason, _, err := a.mapRotationAvailable(ctx, scope.OrganizationID, scope.InstallationID); err != nil || reason != "" {
		return nil
	}
	snap, err := a.MapRotation.Load(ctx, scope.InstallationID)
	if err != nil || !snap.Settings.Enabled {
		return nil
	}
	dto := toMapRotationAdminDTO(snap, "", now)
	out := &serverStatusMapRotationDTO{CurrentMap: dto.Current.Name, NextMap: dto.Next.Name, SwitchAt: dto.Next.SwitchAt,
		VoteOpen: dto.Vote != nil && dto.Vote.Status == "OPEN", Stopped: snap.Settings.HaltedReason != ""}
	out.StoppedReason = nullableString(snap.Settings.HaltedReason)
	return out
}

// discordGatewayConnected reports whether this process's Discord gateway session is ready.
func (a *App) discordGatewayConnected() bool {
	if a.discordReady != nil {
		return a.discordReady()
	}
	if a.Discord == nil {
		return false
	}
	session := a.Discord.Session()
	if session == nil {
		return false
	}
	session.RLock()
	defer session.RUnlock()
	return session.DataReady
}

// handleServerStatus is GET .../admin/server-status (SERVER_STATUS_VIEW).
func (a *App) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapServerStatusView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.PlatformOps == nil {
		writeSaaSError(w, codeInternalError, "server status is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	now := time.Now().UTC()
	fact, found, err := a.serverStatusFact(ctx, ac.scope.InstallationID, now)
	if err != nil {
		slog.Warn("component=saas_api", "msg", "server status facts failed", "installation_id", ac.scope.InstallationID, "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load server status")
		return
	}
	if !found || fact.OrganizationID != ac.scope.OrganizationID {
		writeSaaSError(w, codeNotFound, "installation not found")
		return
	}
	in := serverStatusInputs{Now: now, Fact: fact, RuntimeReady: a.ownerOpsRuntimeReady(), DiscordConnected: a.discordGatewayConnected(),
		Served:      a.Config != nil && a.Config.DiscordGuildID != "" && ac.scope.DiscordGuildID == a.Config.DiscordGuildID,
		MapRotation: a.serverStatusMapRotation(ctx, ac.scope, now)}
	if in.Served {
		// A fresh sample, so the answer does not lag the 30-second watch.
		if fact.ServerID > 0 {
			a.feedWatch.observe(fact.ServerID, a.sampleServer(fact.ServerID, now), now)
		}
		in.Runtime = a.serverRuntime(fact.ServerID, now)
		in.Watch = a.feedWatch.view(fact.ServerID, now)
		in.Deliveries = discord.Deliveries.Snapshot()
	}
	writeSaaSJSON(w, http.StatusOK, buildServerStatus(in))
}
