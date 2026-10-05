package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// roleReconcileInterval/roleReconcileBatch bound the Verified-role
// reconciler: at most roleReconcileBatch Discord role calls per interval.
const (
	roleReconcileInterval = 2 * time.Minute
	roleReconcileBatch    = 10
)

// runRoleReconciler re-delivers Verified roles that failed (or never
// completed) - including across process restarts, since the pending state is
// persisted (migration 0056). It runs once shortly after startup, then on
// roleReconcileInterval. It never blocks ADM ingestion (own goroutine).
func (a *App) runRoleReconciler(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=link", "event", "role_reconciler_panic", "err", fmt.Sprint(r))
		}
	}()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if a.Guilds != nil && a.LinkService != nil && a.Config != nil {
			runCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if _, guildRowID, err := a.Guilds.GetGuild(runCtx, a.Config.DiscordGuildID); err == nil && guildRowID > 0 {
				if _, err := a.LinkService.ReconcileRoles(runCtx, guildRowID, roleReconcileBatch); err != nil {
					slog.Warn("component=link", "event", "role_reconcile_failed", "err", err.Error())
				}
			}
			cancel()
		}
		timer.Reset(roleReconcileInterval)
	}
}
