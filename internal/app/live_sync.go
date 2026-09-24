package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/livesync"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Champion Live Sync phase 2 (docs/CHAMPION_LIVE_SYNC.md): one livesync.Supervisor per server
// worker watches the RPT, script, crash and restart logs beside the ADM engine, independently of
// it. LIVE_SYNC_WATCHERS=off disables the watchers (the ADM engine is unaffected either way).

const (
	liveSyncNoiseRetention = 3 * 24 * time.Hour
	liveSyncRetention      = 30 * 24 * time.Hour
	liveSyncSweepInterval  = time.Hour
)

func liveSyncWatchersEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("LIVE_SYNC_WATCHERS")), "off")
}

// startLiveSync starts the server's supervisor in its own goroutine and returns a channel closed
// when it stops. A panic inside it is recovered and logged; it never reaches the ADM engine.
func (a *App) startLiveSync(ctx context.Context, row repository.GameServer, client *nitrado.Client) <-chan struct{} {
	done := make(chan struct{})
	if a.LiveSync == nil || client == nil || !liveSyncWatchersEnabled() {
		close(done)
		return done
	}
	var sessions livesync.SessionEnder
	if a.Locations != nil {
		sessions = a.Locations
	}
	sup := livesync.NewSupervisor(livesync.Config{GuildID: row.GuildID, ServerID: row.ID, ServiceID: row.ProviderServiceID}, client, a.LiveSync, sessions)
	a.liveSyncMu.Lock()
	if a.liveSyncSupervisors == nil {
		a.liveSyncSupervisors = map[int64]*livesync.Supervisor{}
	}
	a.liveSyncSupervisors[row.ID] = sup
	a.liveSyncMu.Unlock()
	go func() {
		defer close(done)
		defer func() {
			a.liveSyncMu.Lock()
			if a.liveSyncSupervisors[row.ID] == sup {
				delete(a.liveSyncSupervisors, row.ID)
			}
			a.liveSyncMu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=livesync", "event", "supervisor_panic", "server_id", row.ID, "panic", fmt.Sprint(r))
			}
		}()
		sup.Run(ctx)
	}()
	return done
}

// runLiveSyncRetention prunes old live sync records in bounded batches.
func (a *App) runLiveSyncRetention(ctx context.Context) {
	if a.LiveSync == nil {
		return
	}
	sweep := func() {
		now := time.Now()
		var total int64
		for i := 0; i < 200; i++ {
			n, err := a.LiveSync.PruneRecords(ctx, now.Add(-liveSyncNoiseRetention), now.Add(-liveSyncRetention), 5000)
			if err != nil {
				slog.Warn("component=livesync", "event", "retention_sweep_failed", "err", err.Error())
				return
			}
			total += n
			if n == 0 {
				break
			}
		}
		if total > 0 {
			slog.Info("component=livesync", "event", "retention_deleted", "count", total)
		}
	}
	sweep()
	t := time.NewTicker(liveSyncSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

type liveSyncServerDTO struct {
	ServerID   int64                            `json:"serverId"`
	Watchers   *livesync.Snapshot               `json:"watchers,omitempty"`
	Session    *liveSyncSessionDTO              `json:"admSession,omitempty"`
	Stored     []repository.LiveSyncFamilyStats `json:"storedLast6h"`
	ADMLatency *repository.ADMLatencyStats      `json:"admLatencyLast6h,omitempty"`
}

type liveSyncSessionDTO struct {
	ADMFile       string     `json:"admFile"`
	LocalStart    *time.Time `json:"localStart,omitempty"`
	SelectedAt    time.Time  `json:"selectedAt"`
	EndedAt       *time.Time `json:"endedAt,omitempty"`
	EndedReason   string     `json:"endedReason,omitempty"`
	EndedEvidence string     `json:"endedEvidence,omitempty"`
	Current       bool       `json:"current"`
}

// handleAdminLiveSync is the platform-admin diagnostic view of every running server's live sync:
// independent per-family freshness, the ADM boot session, stored record counts and measured
// latency. It carries no token, signed URL, physical path or raw log line.
func (a *App) handleAdminLiveSync(w http.ResponseWriter, r *http.Request, _ adminIdentity) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	a.liveSyncMu.Lock()
	sups := make([]*livesync.Supervisor, 0, len(a.liveSyncSupervisors))
	for _, s := range a.liveSyncSupervisors {
		sups = append(sups, s)
	}
	a.liveSyncMu.Unlock()
	since := time.Now().Add(-6 * time.Hour)
	out := make([]liveSyncServerDTO, 0, len(sups))
	for _, s := range sups {
		snap := s.Snapshot()
		dto := liveSyncServerDTO{ServerID: snap.ServerID, Watchers: &snap, Stored: []repository.LiveSyncFamilyStats{}}
		guildID := s.GuildID()
		if a.Locations != nil {
			if sess, err := a.Locations.CurrentADMSession(ctx, guildID, snap.ServerID); err == nil && sess != nil {
				dto.Session = &liveSyncSessionDTO{ADMFile: sess.ADMFile, LocalStart: sess.LocalStart, SelectedAt: sess.SelectedAt,
					EndedAt: sess.EndedAt, EndedReason: sess.EndedReason, EndedEvidence: sess.EndedEvidence, Current: sess.EndedAt == nil}
			}
		}
		if a.LiveSync != nil {
			if st, err := a.LiveSync.FamilyStats(ctx, guildID, snap.ServerID, since); err == nil {
				dto.Stored = st
			}
			if lat, err := a.LiveSync.ADMLatency(ctx, guildID, snap.ServerID, since); err == nil {
				dto.ADMLatency = &lat
			}
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	writeSaaSJSON(w, http.StatusOK, map[string]any{"generatedAt": time.Now().UTC().Format(time.RFC3339), "enabled": liveSyncWatchersEnabled(), "servers": out})
}
