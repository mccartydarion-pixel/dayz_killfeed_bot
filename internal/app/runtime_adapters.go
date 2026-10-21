package app

import (
	"context"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// linkedPlayerLookup adapts the account-linking service to discord.PlayerLinks:
// the economy commands resolve "my balance" through a VERIFIED link only.
type linkedPlayerLookup struct {
	svc *linking.LinkVerificationService
}

func (l linkedPlayerLookup) LinkedPlayerID(ctx context.Context, guildRowID int64, discordUserID string) (int64, bool) {
	if l.svc == nil {
		return 0, false
	}
	rec, err := l.svc.Status(ctx, guildRowID, discordUserID)
	if err != nil {
		return 0, false
	}
	return discord.VerifiedPlayerID(rec)
}

// playerNames returns the sorted display names of currently online players.
func playerNames(tracker *killfeed.PlayerTracker) []string {
	if tracker == nil {
		return nil
	}
	online := tracker.GetOnlinePlayers()
	names := make([]string, 0, len(online))
	for _, p := range online {
		names = append(names, p.Name)
	}
	return names
}

// rankedSeasonStatus joins the guild's active servers with their Ranked
// seasons for /season status.
type rankedSeasonStatus struct {
	servers *repository.ServerRepository
	ranked  *repository.RankedRepository
}

func (r rankedSeasonStatus) ListActiveByGuild(ctx context.Context, guildID int64) ([]repository.GameServer, error) {
	return r.servers.ListActiveByGuild(ctx, guildID)
}

func (r rankedSeasonStatus) ActiveServerSeason(ctx context.Context, guildID, serverID int64) (*repository.ServerRankedSeason, error) {
	return r.ranked.ActiveServerSeason(ctx, guildID, serverID)
}
