package discord

import (
	"context"
	"errors"
	"fmt"

	"github.com/yourname/dayz-killfeed/internal/linking"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// ServerSeasonRankReader projects the selected public server's active season
// into the guild V3 board. A guild with several servers must have an explicit
// public selection; each server still has its own dedicated ranks panel.
type ServerSeasonRankReader struct {
	Servers interface {
		ConnectedServerID(context.Context, int64) (int64, error)
	}
	Ranked interface {
		ServerStandings(context.Context, int64, int) ([]repository.ServerStanding, error)
	}
}

func (r ServerSeasonRankReader) TopCurrentRanks(ctx context.Context, guildID int64, limit int) ([]RankEntry, error) {
	if r.Servers == nil || r.Ranked == nil {
		return nil, fmt.Errorf("server rank reader is not configured")
	}
	serverID, err := r.Servers.ConnectedServerID(ctx, guildID)
	if errors.Is(err, linking.ErrNoConnectedServer) || errors.Is(err, linking.ErrMultipleConnectedServers) {
		return nil, repository.ErrRankedIneligible
	}
	if err != nil {
		return nil, err
	}
	rows, err := r.Ranked.ServerStandings(ctx, serverID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RankEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, RankEntry{
			DisplayName: row.Name,
			Rank: fmt.Sprintf("%s • %s RP", row.Tier, presentation.FormatThousands(row.RP)),
		})
	}
	return out, nil
}
