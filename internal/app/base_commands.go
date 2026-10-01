package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// registerBaseCommands registers /mybase and /registerbase (docs/BASE_REQUESTS.md).
func (a *App) registerBaseCommands(session *discordgo.Session) {
	if a.DB == nil || a.DB.Pool == nil || a.Guilds == nil || a.Discord == nil || session == nil || a.Config == nil || a.Config.DiscordGuildID == "" {
		return
	}
	handler := discord.NewBaseCommandHandler(a.Guilds, a.linkedPlayer, a.publicServerID, a.baseCommandSummary, a.baseCommandRequest, a.securityStoreURL())
	if err := discord.RegisterBaseCommands(session, a.Config.DiscordGuildID); err != nil {
		slog.Warn("component=discord", "msg", "failed to register base commands", "err", err.Error())
	} else {
		slog.Info("component=discord", "msg", "base commands registered")
	}
	a.Discord.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		switch i.ApplicationCommandData().Name {
		case "mybase":
			handler.HandleMyBase(s, i)
		case "registerbase":
			handler.HandleRegisterBase(s, i)
		}
	})
}

func (a *App) baseCommandScope(ctx context.Context, guildRowID, serverID int64) (repository.BaseRequestScope, error) {
	id, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).InstallationForServer(ctx, guildRowID, serverID)
	if err != nil {
		return repository.BaseRequestScope{}, err
	}
	return repository.BaseRequestScope{InstallationID: id, GuildID: guildRowID, ServerID: serverID}, nil
}

// baseCommandSummary builds /mybase: bases, the newest request and paid time.
func (a *App) baseCommandSummary(ctx context.Context, guildRowID, serverID, playerID int64) (discord.BaseCommandSummary, error) {
	var out discord.BaseCommandSummary
	s, err := a.baseCommandScope(ctx, guildRowID, serverID)
	if err != nil {
		return out, err
	}
	repo := repository.NewCaseBaseRequestRepository(a.DB.Pool)
	bases, err := repo.PlayerBases(ctx, s, playerID)
	if err != nil {
		return out, err
	}
	for _, b := range bases {
		out.Bases = append(out.Bases, b.Name)
	}
	reqs, err := repo.Mine(ctx, s, playerID, 5)
	if err != nil {
		return out, err
	}
	for _, q := range reqs {
		if q.Status == repository.BaseRequestPending {
			out.Pending = q.Name
			break
		}
	}
	if out.Pending == "" {
		for _, q := range reqs {
			switch q.Status {
			case repository.BaseRequestApproved:
				out.LastAnswer = q.Name + " was approved."
			case repository.BaseRequestDeclined:
				out.LastAnswer = q.Name + " was declined."
				if q.DeclineReason != "" {
					out.LastAnswer = q.Name + " was declined: " + q.DeclineReason
				}
			default:
				continue
			}
			break
		}
	}
	rented, err := repository.NewBaseRentRepository(a.DB.Pool).PlayerBases(ctx,
		repository.SecurityScope{InstallationID: s.InstallationID, GuildID: s.GuildID, ServerID: s.ServerID}, playerID)
	if err != nil {
		return out, err
	}
	for _, b := range rented {
		out.Rent = append(out.Rent, discord.BaseRentLine{BaseName: b.BaseName, DueAt: b.DueAt, Paused: b.Paused})
	}
	sales := repository.NewSecurityServiceRepository(a.DB.Pool)
	for _, id := range []string{repository.ServiceSentinelPro, repository.ServiceBaseRaidAlarm, repository.ServicePerimeterWatch,
		repository.ServiceBaseBlackBox, repository.ServiceFactionSecurity} {
		until, err := sales.ActiveUntil(ctx, s.InstallationID, playerID, id)
		if err != nil {
			return out, err
		}
		if until != nil {
			out.PaidUntil = append(out.PaidUntil, discord.BasePaidTime{Label: repository.SecurityServiceLabel(id), Until: *until})
		}
	}
	return out, nil
}

// baseCommandRequest sends /registerbase through the same path as the website.
func (a *App) baseCommandRequest(ctx context.Context, guildRowID, serverID, playerID int64, name string, radius float64, note string) (string, error) {
	s, err := a.baseCommandScope(ctx, guildRowID, serverID)
	if err != nil {
		return "", errors.New("this Discord isn't connected to a Champion installation")
	}
	req, code, msg := a.submitBaseRequest(ctx, s, playerID, name, radius, note)
	if code != "" {
		return "", errors.New(msg)
	}
	return "📍 Request sent for **" + escapeBaseName(req.Name) + "**. The server owner will approve or decline it, and you'll get a message either way. Check it any time with `/mybase`.", nil
}

func escapeBaseName(s string) string {
	return strings.NewReplacer("*", "", "_", "", "`", "'", "@", "@​", "~", "", "|", "").Replace(s)
}

// securityStoreURL is the player Security Store page, or "" without an https site address.
func (a *App) securityStoreURL() string {
	if a.Config == nil {
		return ""
	}
	base := strings.TrimRight(a.Config.SiteBaseURL, "/")
	if !strings.HasPrefix(base, "https://") {
		return ""
	}
	return base + "/dashboard/player/security-store"
}
