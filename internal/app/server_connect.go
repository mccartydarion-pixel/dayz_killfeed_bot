package app

import (
	"context"
	"fmt"
	"log/slog"
)

// markFirstConnect flags a server so its next worker start seeds the ADM
// checkpoint at the log tail instead of byte 0 (see ConnectServer).
func (a *App) markFirstConnect(serverID int64) {
	a.firstConnectMu.Lock()
	if a.firstConnectServers == nil {
		a.firstConnectServers = make(map[int64]bool)
	}
	a.firstConnectServers[serverID] = true
	a.firstConnectMu.Unlock()
}

// consumeFirstConnect reports and clears the first-connect flag for a server,
// so only the triggering start (not later restarts) skips log history.
func (a *App) consumeFirstConnect(serverID int64) bool {
	a.firstConnectMu.Lock()
	defer a.firstConnectMu.Unlock()
	if a.firstConnectServers[serverID] {
		delete(a.firstConnectServers, serverID)
		return true
	}
	return false
}

// ConnectServer implements discord.ServerRuntime: it marks the server as a
// first connect (tail-start) and starts its worker via WorkerManager. Safe to
// call for a server that was never active at startup (e.g. /server select on
// a fresh guild) because WorkerManager's factory resolves unknown IDs from
// the database.
func (a *App) ConnectServer(ctx context.Context, serverID int64) error {
	if a.WorkerManager == nil {
		return fmt.Errorf("killfeed runtime is not initialized (database required)")
	}
	a.markFirstConnect(serverID)
	a.selectPublicServer(ctx, serverID)
	if a.WorkerManager.Running(serverID) {
		return nil
	}
	return a.WorkerManager.Start(ctx, serverID)
}

// DisconnectServer implements discord.ServerRuntime: stops the worker, if any,
// and returns only once its goroutine has exited (bounded by the manager's stop
// timeout), so a DisconnectServer+RepairServer sequence really restarts it.
func (a *App) DisconnectServer(serverID int64) {
	if a.WorkerManager != nil {
		a.WorkerManager.Stop(serverID)
	}
}

// RepairServer implements discord.ServerRuntime: restarts the worker without
// the first-connect tail-start (a repair must not skip missed activity).
func (a *App) RepairServer(ctx context.Context, serverID int64) error {
	if a.WorkerManager == nil {
		return fmt.Errorf("killfeed runtime is not initialized (database required)")
	}
	if a.WorkerManager.Running(serverID) {
		return nil
	}
	return a.WorkerManager.Start(ctx, serverID)
}

// selectPublicServer makes serverID the public counter server of ITS OWN guild. For a server
// of the primary guild that is the process-wide public counter (publicCounterServerID) and the
// primary guild's persisted selection; for a server of any other guild only that guild's
// persisted selection changes, which its own guild counter reads (online_counter.go). A
// customer's /server select must never move the primary guild's voice counter onto their
// server. When the server's guild cannot be resolved the primary-guild behaviour is kept.
func (a *App) selectPublicServer(ctx context.Context, serverID int64) {
	serverGuildRowID, primaryGuildRowID := int64(0), int64(0)
	if a.Servers != nil {
		if row, err := a.Servers.GetByID(ctx, serverID); err == nil && row != nil {
			serverGuildRowID = row.GuildID
		}
	}
	if a.Guilds != nil && a.Config != nil {
		if _, id, err := a.Guilds.GetGuild(ctx, a.Config.DiscordGuildID); err == nil {
			primaryGuildRowID = id
		}
	}
	if serverGuildRowID != 0 && primaryGuildRowID != 0 && serverGuildRowID != primaryGuildRowID {
		if err := a.Guilds.SetSelectedPublicServerByID(ctx, serverGuildRowID, serverID); err != nil {
			slog.Warn("component=servers", "event", "public_counter_selection_persist_failed", "guild_row_id", serverGuildRowID, "err", err.Error())
		}
		a.pokeOnlineCounter()
		return
	}
	a.counterOwnerMu.Lock()
	a.publicCounterServerID = serverID
	a.counterOwnerMu.Unlock()
	if primaryGuildRowID > 0 {
		if err := a.Guilds.SetSelectedPublicServer(ctx, a.Config.DiscordGuildID, serverID); err != nil {
			slog.Warn("component=servers", "event", "public_counter_selection_persist_failed", "err", err.Error())
		}
	}
	a.pokeOnlineCounter()
}
