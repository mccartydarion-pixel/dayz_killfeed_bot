package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// sessionEarningsFunc is the CONNECTIONS card's "Cash earned" look-up for one server: what the
// disconnecting player was credited since they connected. The row is shown only when the
// server's organization has the economy (entitlements.Economy); a legacy server with no
// organization has it whenever the economy exists. Any failure hides the row.
func (a *App) sessionEarningsFunc(row repository.GameServer) discord.SessionEarningsFunc {
	if a.Economy == nil {
		return nil
	}
	return func(ctx context.Context, playerID string, since time.Time) (int64, bool) {
		if a.Servers != nil {
			orgID, err := a.Servers.OrganizationIDFor(ctx, row.ID)
			if err != nil {
				return 0, false
			}
			if orgID != nil {
				plan, err := a.organizationPlan(ctx, *orgID)
				if err != nil || !entitlements.Has(plan, entitlements.Economy) {
					return 0, false
				}
			}
		}
		total, err := a.Economy.EarnedSince(ctx, row.GuildID, playerID, since)
		if err != nil {
			slog.Warn("component=connections", "event", "session_earnings_failed", "server_id", row.ID, "err", err.Error())
			return 0, false
		}
		return total, true
	}
}
