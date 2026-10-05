package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Server name sync (docs/SERVER_NAME_SYNC.md).
//
// Champion copies a server's name from Nitrado when the server is connected. This worker keeps it
// current: about every twenty minutes it reads each active Nitrado server's name (one GET
// /services/:id/gameservers per server, through the shared client and its rate-limit handling) and
//
//   - always records the name as the last one seen from Nitrado;
//   - makes it the display name, unless the owner typed a name in Champion (a custom name wins).
//
// A failed read, a response for another service or an empty name changes nothing: a name is never
// blanked. Servers whose installations are all suspended are skipped. After a failure with one
// credential the rest of that credential's servers wait for the next pass, so a revoked token or a
// rate limit costs one request per pass, not one per server. Tokens are never logged.

const (
	serverNameSyncInterval = 20 * time.Minute
	// serverNameSyncJitter is added to every wait, so the passes of several processes (and the
	// requests of every Champion install) do not line up.
	serverNameSyncJitter = 5 * time.Minute
	// serverNameSyncStartDelay keeps the first pass out of the start-up burst.
	serverNameSyncStartDelay  = 2 * time.Minute
	serverNameSyncReadTimeout = 15 * time.Second
	serverNameSyncPassTimeout = 10 * time.Minute
	// serverNameSyncPause spaces the reads of one pass.
	serverNameSyncPause = 500 * time.Millisecond
)

// serverNameReader is the Nitrado read the sync needs (*nitrado.Client).
type serverNameReader interface {
	GameserverName(ctx context.Context, serviceID string) (nitrado.GameserverName, error)
}

// serverNameStore is the storage the sync needs (*repository.ServerNameRepository).
type serverNameStore interface {
	ListSyncTargets(ctx context.Context) ([]repository.ServerNameTarget, error)
	RecordProviderName(ctx context.Context, serverID int64, name string) (bool, error)
}

// serverNameClientFunc returns the Nitrado reader for a target and a key that identifies the
// credential behind it (never the token itself).
type serverNameClientFunc func(ctx context.Context, t repository.ServerNameTarget) (serverNameReader, string, error)

// serverNameSyncResult counts one pass, for the log line and the tests.
type serverNameSyncResult struct {
	Servers, Renamed, Unchanged, NoName, NoCredential, Failed, Skipped int
}

// syncServerNames runs one pass. onRenamed is called for every server whose display name changed.
func syncServerNames(ctx context.Context, store serverNameStore, clientFor serverNameClientFunc, onRenamed func(serverID int64), pause time.Duration) (serverNameSyncResult, error) {
	var res serverNameSyncResult
	targets, err := store.ListSyncTargets(ctx)
	if err != nil {
		return res, err
	}
	failedCredential := map[string]bool{}
	for i, t := range targets {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Servers++
		reader, credentialKey, err := clientFor(ctx, t)
		if err != nil || reader == nil {
			res.NoCredential++
			continue
		}
		if failedCredential[credentialKey] {
			res.Skipped++
			continue
		}
		if i > 0 && pause > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(pause):
			}
		}
		readCtx, cancel := context.WithTimeout(ctx, serverNameSyncReadTimeout)
		got, err := reader.GameserverName(readCtx, t.ProviderServiceID)
		cancel()
		if err != nil {
			res.Failed++
			failedCredential[credentialKey] = true
			slog.Debug("component=server_name_sync", "event", "read_failed", "server_id", t.ServerID, "kind", nitradoErrorKind(err))
			continue
		}
		if !got.BelongsTo(t.ProviderServiceID) {
			res.Failed++ // not provably this server's name
			continue
		}
		// The same precedence the connect flows use, minus the service list (not read here).
		name := nitrado.ServerName(got, nitrado.Service{})
		if name == "" {
			res.NoName++
			continue
		}
		changed, err := store.RecordProviderName(ctx, t.ServerID, name)
		if err != nil {
			res.Failed++
			if ctx.Err() == nil {
				slog.Warn("component=server_name_sync", "event", "store_failed", "server_id", t.ServerID, "err", err.Error())
			}
			continue
		}
		if !changed {
			res.Unchanged++
			continue
		}
		res.Renamed++
		slog.Info("component=server_name_sync", "event", "server_renamed", "server_id", t.ServerID, "name", name)
		if onRenamed != nil {
			onRenamed(t.ServerID)
		}
	}
	return res, nil
}

// nitradoErrorKind is a failure's class for the log (never the message, which may carry a URL).
func nitradoErrorKind(err error) string {
	var re *nitrado.RequestError
	if errors.As(err, &re) {
		return string(re.Kind)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// serverNameSyncClient builds the reader for one server: the organization's credential when the
// server belongs to one, else the guild's own connection (servers connected with /server).
// Clients are reused within a pass through cache.
func (a *App) serverNameSyncClient(cache map[string]serverNameReader) serverNameClientFunc {
	return func(ctx context.Context, t repository.ServerNameTarget) (serverNameReader, string, error) {
		if t.OrganizationID != nil && a.SaaSCredentials != nil {
			key := fmt.Sprintf("org:%d", *t.OrganizationID)
			if c, ok := cache[key]; ok {
				return c, key, nil
			}
			if envelope, err := a.SaaSCredentials.GetForOrganizationOnly(ctx, *t.OrganizationID); err == nil && envelope != nil {
				if client, err := a.nitradoClientFromEnvelope(*envelope); err == nil {
					cache[key] = client
					return client, key, nil
				}
			}
		}
		if a.Servers == nil {
			return nil, "", errors.New("no Nitrado credential")
		}
		key := fmt.Sprintf("guild:%d", t.GuildID)
		if c, ok := cache[key]; ok {
			return c, key, nil
		}
		connection, err := a.Servers.GetConnection(ctx, t.GuildID)
		if err != nil || connection == nil {
			return nil, "", errors.New("no Nitrado credential")
		}
		client, err := nitradoClientFromConnection(a.CredentialCipher, *connection)
		if err != nil {
			return nil, "", err
		}
		cache[key] = client
		return client, key, nil
	}
}

// serverNameSyncPass runs one pass and logs its outcome.
func (a *App) serverNameSyncPass(parent context.Context, store serverNameStore) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=server_name_sync", "msg", "worker panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	ctx, cancel := context.WithTimeout(parent, serverNameSyncPassTimeout)
	defer cancel()
	res, err := syncServerNames(ctx, store, a.serverNameSyncClient(map[string]serverNameReader{}), a.forgetServerName, serverNameSyncPause)
	if err != nil {
		if parent.Err() == nil {
			slog.Warn("component=server_name_sync", "event", "pass_failed", "err", err.Error())
		}
		return
	}
	level := slog.LevelDebug
	if res.Renamed > 0 || res.Failed > 0 {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "component=server_name_sync", "event", "pass_done", "servers", res.Servers, "renamed", res.Renamed, "unchanged", res.Unchanged,
		"no_name", res.NoName, "no_credential", res.NoCredential, "failed", res.Failed, "skipped", res.Skipped)
}

// serverNameSyncWait is the time until the next pass: the interval plus up to the jitter.
func serverNameSyncWait(base time.Duration) time.Duration {
	return base + time.Duration(rand.Int63n(int64(serverNameSyncJitter)))
}

// startServerNameSync starts the worker. It needs the database; without one it does nothing.
func (a *App) startServerNameSync(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	store := repository.NewServerNameRepository(a.DB.Pool)
	// One process only (singleton_leader.go): a second one would repeat every Nitrado read.
	go a.singleton(ctx, "server_name_sync", func(ctx context.Context) {
		wait := serverNameSyncWait(serverNameSyncStartDelay)
		for {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			a.serverNameSyncPass(ctx, store)
			wait = serverNameSyncWait(serverNameSyncInterval)
		}
	})
}
