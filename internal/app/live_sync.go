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

	"github.com/yourname/dayz-killfeed/internal/health"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
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
	// Phase 2.1: boot authority, ADM source health, and a check that no stored record keeps RPT
	// command-line evidence.
	BootAuthority            *bootAuthorityDTO `json:"bootAuthority,omitempty"`
	ADMSource                string            `json:"admSource,omitempty"`
	CommandLineHeaderRecords *int64            `json:"commandLineHeaderRecords,omitempty"`
}

type bootAuthorityDTO struct {
	AcceptedBoot         string `json:"acceptedBoot,omitempty"`
	AcceptedFile         string `json:"acceptedFile,omitempty"`
	AcceptedAt           string `json:"acceptedAt,omitempty"`
	LastNewBootSeenAt    string `json:"lastNewBootSeenAt,omitempty"`
	LastNewBootFile      string `json:"lastNewBootFile,omitempty"`
	RejectedOlder        int64  `json:"rejectedOlder"`
	UnverifiedCandidates int64  `json:"unverifiedCandidates"`
	LastRejectedFile     string `json:"lastRejectedFile,omitempty"`
}

func fmtTime(t time.Time, layout string) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(layout)
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
			if n, err := a.LiveSync.CommandLineHeaderRecords(ctx, guildID, snap.ServerID); err == nil {
				dto.CommandLineHeaderRecords = &n
			}
		}
		a.presenceMu.Lock()
		eng := a.presenceEngines[snap.ServerID]
		a.presenceMu.Unlock()
		if eng != nil {
			b := eng.BootAuthority()
			dto.BootAuthority = &bootAuthorityDTO{AcceptedBoot: fmtTime(b.AcceptedBoot, "2006-01-02T15:04:05"), AcceptedFile: b.AcceptedFile,
				AcceptedAt: fmtTime(b.AcceptedAt, time.RFC3339), LastNewBootSeenAt: fmtTime(b.LastNewBootSeenAt, time.RFC3339),
				LastNewBootFile: b.LastNewBootFile, RejectedOlder: b.RejectedOlder, UnverifiedCandidates: b.UnverifiedCandidates, LastRejectedFile: b.LastRejectedFile}
			dto.ADMSource = admSourceComponent(snap.ServerID, eng.SourceHealth(), time.Now()).Message
		}
		out = append(out, dto)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	writeSaaSJSON(w, http.StatusOK, map[string]any{"generatedAt": time.Now().UTC().Format(time.RFC3339), "enabled": liveSyncWatchersEnabled(), "servers": out})
}

// admSourceComponents classifies every running ADM engine's source health (Live Sync phase 2.1):
// HEALTHY and QUIET are healthy (a quiet server is not a failure), SOURCE_LAGGING and
// TRANSPORT_ERROR are degraded, WORKER_STALLED is unhealthy. The state name leads the message.
func (a *App) admSourceComponents(now time.Time) []health.Component {
	a.presenceMu.Lock()
	engines := make(map[int64]*killfeed.Engine, len(a.presenceEngines))
	for id, e := range a.presenceEngines {
		engines[id] = e
	}
	a.presenceMu.Unlock()
	out := make([]health.Component, 0, len(engines))
	for id, e := range engines {
		if e == nil {
			continue
		}
		out = append(out, admSourceComponent(id, e.SourceHealth(), now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func admSourceComponent(serverID int64, h killfeed.ADMSourceHealth, now time.Time) health.Component {
	state, reason := killfeed.ClassifyADMSourceHealth(h, now)
	st := health.Healthy
	switch state {
	case killfeed.ADMSourceLagging, killfeed.ADMTransportError:
		st = health.Degraded
	case killfeed.ADMWorkerStalled:
		st = health.Unhealthy
	}
	return health.Component{Name: fmt.Sprintf("adm_source_%d", serverID), State: st, Message: state + ": " + reason}
}
