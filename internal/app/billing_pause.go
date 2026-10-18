package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/dayz-killfeed/internal/billing"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Billing pause: a customer's killfeed runs only while their organization pays for it. An
// expired trial, an ended or suspended subscription, a lapsed owner grant, or a payment that
// has been failing for longer than pastDueGrace pauses the organization's server workers; paying
// again resumes them on the next reconcile. Servers no organization has claimed (legacy
// /server connect) and a platform owner's own organization are never paused.

// pastDueGrace is how long a failed payment keeps the service running.
const pastDueGrace = 3 * 24 * time.Hour

// workerReconcileInterval is how often reconcileServerWorkers runs after start-up.
const workerReconcileInterval = 5 * time.Minute

// errBillingPaused is returned by a server worker that refuses to start for billing.
var errBillingPaused = errors.New("service paused: billing required")

// servicePaused reports whether organizationID's subscription no longer pays for live service.
func servicePaused(organizationID int64, sub *repository.Subscription, now time.Time) bool {
	st := billing.StateFor(organizationID, sub, now)
	if st.PlatformOwnerAccess {
		return false
	}
	if st.BillingRequired {
		return true
	}
	if sub != nil && sub.Status == repository.SubscriptionPastDue {
		// The grace runs from the first failed payment (past_due_since, kept by
		// ApplyProviderState while the status stays PAST_DUE). UpdatedAt and
		// CurrentPeriodStart both move on Stripe's dunning retries and would restart it.
		since := sub.UpdatedAt
		if sub.PastDueSince != nil {
			since = *sub.PastDueSince
		} else if sub.CurrentPeriodStart != nil {
			since = *sub.CurrentPeriodStart
		}
		return now.Sub(since) > pastDueGrace
	}
	return false
}

// serverBillingPaused reports whether game server serverID belongs to an organization whose
// service is paused for billing.
func (a *App) serverBillingPaused(ctx context.Context, serverID int64) (bool, error) {
	if a.Servers == nil || a.SaaSSubscriptions == nil {
		return false, nil
	}
	orgID, err := a.Servers.OrganizationIDFor(ctx, serverID)
	if err != nil || orgID == nil {
		return false, err
	}
	sub, err := a.SaaSSubscriptions.GetForOrganization(ctx, *orgID)
	if err != nil {
		return false, err
	}
	return servicePaused(*orgID, sub, time.Now()), nil
}

// errNoNitrado is returned by a server worker that has no saved Nitrado connection.
var errNoNitrado = errors.New("no Nitrado connection for this server")

// serverHasNitrado reports whether row has a saved Nitrado connection: its guild's, or that of
// the organization that owns it (website onboarding stores it there). Without one no worker can
// read logs, so none is started or restarted; owner ops raises NITRADO_ACCESS for the owner
// instead, and the reconciler starts the worker once they connect Nitrado.
func (a *App) serverHasNitrado(ctx context.Context, row repository.GameServer) (bool, error) {
	if a.Servers == nil {
		return true, nil
	}
	if _, err := a.Servers.GetConnection(ctx, row.GuildID); err == nil {
		return true, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	envelope, err := a.organizationCredentialForServer(ctx, row.ID)
	if err != nil {
		return false, err
	}
	return envelope != nil, nil
}

// reconcileServerWorkers makes the running workers match the active servers: it starts every
// active server that is not running (any guild's, not only DISCORD_GUILD_ID's) and stops every
// worker whose organization's service is paused for billing. It returns how many it started.
func (a *App) reconcileServerWorkers(ctx context.Context) int {
	if a.WorkerManager == nil || a.Servers == nil {
		return 0
	}
	rows, err := a.Servers.ListActive(ctx)
	if err != nil {
		slog.Warn("component=servers", "event", "worker_reconcile_failed", "err", err.Error())
		return 0
	}
	started := 0
	for _, row := range rows {
		paused, err := a.serverBillingPaused(ctx, row.ID)
		if err != nil {
			// Unknown billing state: leave the worker as it is.
			slog.Warn("component=servers", "event", "billing_check_failed", "server_id", row.ID, "err", err.Error())
			continue
		}
		running := a.WorkerManager.Running(row.ID)
		switch {
		case paused && running:
			a.WorkerManager.Stop(row.ID)
			slog.Warn("component=servers", "event", "server_paused_billing", "server_id", row.ID, "guild_id", row.GuildID)
		case !paused && !running:
			if ok, err := a.serverHasNitrado(ctx, row); err != nil || !ok {
				continue
			}
			if err := a.WorkerManager.Start(ctx, row.ID); err != nil {
				slog.Warn("component=servers", "event", "worker_reconcile_start_failed", "server_id", row.ID, "err", err.Error())
				continue
			}
			started++
			slog.Info("component=servers", "msg", "server worker started", "server_id", row.ID, "guild_id", row.GuildID, "display_name", row.DisplayName)
		}
	}
	return started
}

// runWorkerReconciler runs reconcileServerWorkers every workerReconcileInterval until ctx ends.
func (a *App) runWorkerReconciler(ctx context.Context) {
	ticker := time.NewTicker(workerReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.reconcileServerWorkers(ctx)
		}
	}
}

// startCustomerServer starts a server's worker after the customer selects it on the website,
// tail-starting like a first /server connect so old log history is not replayed.
func (a *App) startCustomerServer(ctx context.Context, serverID int64) {
	if a.WorkerManager == nil || a.WorkerManager.Running(serverID) {
		return
	}
	if paused, err := a.serverBillingPaused(ctx, serverID); err != nil || paused {
		slog.Warn("component=servers", "event", "customer_worker_start_refused", "server_id", serverID, "paused", paused, "err", fmt.Sprint(err))
		return
	}
	a.markFirstConnect(serverID)
	if err := a.WorkerManager.Start(ctx, serverID); err != nil {
		slog.Warn("component=servers", "event", "customer_worker_start_failed", "server_id", serverID, "err", err.Error())
	}
}
