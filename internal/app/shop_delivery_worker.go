package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/repository"
	"github.com/yourname/dayz-killfeed/internal/shop/deliveryworker"
)

// The Shop automatic delivery worker's place in the bot (docs/SHOP_DELIVERY_WORKER_DESIGN.md).
//
// Nothing here runs unless CHAMPION_SHOP_AUTO_DELIVERY is exactly "report" or "enabled" and lists an
// installation. Even then an installation is worked only while its owner's switch is on and it is
// not paused, and an order is delivered only for a product the owner marked automatic.
//
//   - "report": every pass is read-only. It inspects the server and logs what it would stage and
//     which boot it sees; it writes nothing to the server and nothing to the Shop tables (it does
//     hold the installation's lease).
//   - "enabled": the worker delivers.

const (
	shopWorkerInterval       = time.Minute
	shopWorkerReportInterval = 5 * time.Minute
	shopWorkerLease          = 4 * time.Minute
	shopWorkerPassTimeout    = 3 * time.Minute
)

// shopWorkerName identifies this bot process in the ledger and the journal.
func shopWorkerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "bot"
	}
	if len(host) > 60 {
		host = host[:60]
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// shopWorkerStore is what the runner needs beyond the worker's own interfaces.
type shopWorkerStore interface {
	ClaimInstallations(ctx context.Context, now time.Time, owner string, leaseFor time.Duration, allowed []int64) ([]repository.ShopAutoInstallation, error)
	ReleaseLease(ctx context.Context, inst int64, owner string) error
}

// shopWorkerRunner claims installations and runs one pass on each.
type shopWorkerRunner struct {
	worker  *deliveryworker.Worker
	store   shopWorkerStore
	allowed []int64
	owner   string
	now     func() time.Time
	// server builds the installation's Nitrado access (its stored credential).
	server func(ctx context.Context, inst repository.ShopAutoInstallation) (deliveryworker.Server, error)
}

// tick runs one round over every installation this process may work.
func (r *shopWorkerRunner) tick(ctx context.Context) {
	claimed, err := r.store.ClaimInstallations(ctx, r.now(), r.owner, shopWorkerLease, r.allowed)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("component=shop_delivery_worker", "event", "claim_failed", "err", err.Error())
		}
		return
	}
	for _, inst := range claimed {
		r.one(ctx, inst)
	}
}

func (r *shopWorkerRunner) one(parent context.Context, inst repository.ShopAutoInstallation) {
	ctx, cancel := context.WithTimeout(parent, shopWorkerPassTimeout)
	defer cancel()
	defer func() {
		// The lease is released with its own short deadline so a timed-out pass still frees it.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer rcancel()
		if err := r.store.ReleaseLease(rctx, inst.InstallationID, r.owner); err != nil {
			slog.Warn("component=shop_delivery_worker", "event", "release_failed", "installation_id", inst.InstallationID, "err", err.Error())
		}
	}()
	srv, err := r.server(ctx, inst)
	if err != nil {
		slog.Warn("component=shop_delivery_worker", "event", "server_unavailable", "installation_id", inst.InstallationID, "err", err.Error())
		return
	}
	rep, err := r.worker.Pass(ctx, inst, srv)
	if err != nil {
		slog.Warn("component=shop_delivery_worker", "event", "pass_failed", "installation_id", inst.InstallationID, "err", err.Error())
	}
	if rep.Inspected || rep.Paused != "" || len(rep.Staged)+len(rep.Unstaged)+len(rep.Failed)+len(rep.Fulfilled)+len(rep.Delivered)+len(rep.Abandoned)+len(rep.WouldStage) > 0 {
		slog.Info("component=shop_delivery_worker", "event", "pass", "installation_id", inst.InstallationID, "boot", rep.CurrentBoot, "paused", rep.Paused != "",
			"skipped", rep.Skipped, "staged", len(rep.Staged), "unstaged", len(rep.Unstaged), "delivered", len(rep.Delivered), "fulfilled", len(rep.Fulfilled),
			"failed", len(rep.Failed), "abandoned", len(rep.Abandoned), "would_stage", len(rep.WouldStage), "write", rep.WriteOutcome)
	}
}

func (r *shopWorkerRunner) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// startShopDeliveryWorker starts the worker when its lock is open. It returns whether it started.
func (a *App) startShopDeliveryWorker(ctx context.Context) bool {
	lock := a.Config.ShopAutoDelivery
	mode := lock.Mode
	if mode == config.ShopAutoDeliveryOff {
		mode = "off"
	}
	slog.Info("component=shop_delivery_worker", "mode", mode, "installations", len(lock.InstallationIDs), "max_staged", lock.MaxStaged)
	if lock.Mode == config.ShopAutoDeliveryOff || a.DB == nil || a.shopConfirmationRepo == nil || a.Servers == nil {
		return false
	}
	pool := a.DB.Pool
	store := repository.NewShopAutoDeliveryRepository(pool)
	worker := deliveryworker.New(deliveryworker.Config{Name: shopWorkerName(), ReportOnly: lock.Mode != config.ShopAutoDeliveryEnabled, MaxStaged: lock.MaxStaged},
		repository.NewShopAttemptRepository(pool), store, repository.NewShopRepository(pool), a.shopConfirmationRepo)
	worker.OnTicket = a.shopTicketOpened
	runner := &shopWorkerRunner{worker: worker, store: store, allowed: lock.InstallationIDs, owner: "worker:" + shopWorkerName(), now: time.Now,
		server: func(ctx context.Context, inst repository.ShopAutoInstallation) (deliveryworker.Server, error) {
			row, err := a.Servers.GetByID(ctx, inst.GameServerID)
			if err != nil {
				return nil, fmt.Errorf("load game server: %w", err)
			}
			if row == nil || row.ProviderServiceID != inst.NitradoServiceID {
				return nil, fmt.Errorf("the game server's Nitrado service does not match the installation binding")
			}
			client, err := a.nitradoClientForServer(ctx, *row)
			if err != nil {
				return nil, err
			}
			return deliveryworker.NewNitradoServer(client, inst), nil
		}}
	every := shopWorkerInterval
	if lock.Mode == config.ShopAutoDeliveryReport {
		every = shopWorkerReportInterval
	}
	go runner.run(ctx, every)
	return true
}
