package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Base Black Box: a per-base history of players seen near a registered base or
// taking parts off it, for the base owner to look back on. It records only what
// the server log already reports (positions and dismantle lines), never messages
// anyone and never acts on the player. Off until the server owner turns it on.

const (
	blackBoxQueueCap   = 500
	blackBoxJobTimeout = 10 * time.Second
)

type blackBoxStore interface {
	MatchVisit(ctx context.Context, guildID, serverID, playerID int64, x, z float64) ([]repository.BlackBoxMatch, error)
	MatchDismantle(ctx context.Context, guildID, serverID int64, admPlayerID string, x, z float64) ([]repository.BlackBoxMatch, error)
	Record(ctx context.Context, m repository.BlackBoxMatch, kind, playerName, detail string) error
}

type blackBoxJob struct {
	kind     string
	playerID int64
	admID    string
	name     string
	detail   string
	x, z     float64
}

// baseBlackBoxRecorder implements killfeed.LocationObserver and
// killfeed.BuildPublisher for one server. Neither call blocks.
type baseBlackBoxRecorder struct {
	store      blackBoxStore
	guildRowID int64
	serverID   int64
	queue      chan blackBoxJob
	dropped    atomic.Int64
}

func newBaseBlackBoxRecorder(store blackBoxStore, guildRowID, serverID int64) *baseBlackBoxRecorder {
	return &baseBlackBoxRecorder{store: store, guildRowID: guildRowID, serverID: serverID, queue: make(chan blackBoxJob, blackBoxQueueCap)}
}

func (b *baseBlackBoxRecorder) enqueue(j blackBoxJob) {
	select {
	case b.queue <- j:
	default:
		b.dropped.Add(1)
	}
}

func (b *baseBlackBoxRecorder) ObserveLocations(samples []killfeed.LocationSample) {
	if b == nil {
		return
	}
	for _, s := range samples {
		if s.PlayerID <= 0 || s.ServerID != b.serverID {
			continue
		}
		b.enqueue(blackBoxJob{kind: repository.BlackBoxVisit, playerID: s.PlayerID, name: strings.TrimSpace(s.Gamertag), x: s.X, z: s.Z})
	}
}

func (b *baseBlackBoxRecorder) PublishBuild(ev *killfeed.Event) {
	if b == nil || ev == nil || ev.Build == nil || ev.Build.Action != "Dismantled" || ev.Build.Object == "" ||
		ev.Player == nil || ev.Player.ID == "" || ev.Player.Position == nil {
		return
	}
	b.enqueue(blackBoxJob{kind: repository.BlackBoxDismantle, admID: ev.Player.ID, name: strings.TrimSpace(ev.Player.Name),
		detail: ev.Build.Object, x: ev.Player.Position.MapX(), z: ev.Player.Position.MapZ()})
}

func (b *baseBlackBoxRecorder) Run(ctx context.Context) {
	if b == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-b.queue:
			jobCtx, cancel := context.WithTimeout(ctx, blackBoxJobTimeout)
			b.handle(jobCtx, j)
			cancel()
		}
	}
}

func (b *baseBlackBoxRecorder) handle(ctx context.Context, j blackBoxJob) {
	var matches []repository.BlackBoxMatch
	var err error
	if j.kind == repository.BlackBoxDismantle {
		matches, err = b.store.MatchDismantle(ctx, b.guildRowID, b.serverID, j.admID, j.x, j.z)
	} else {
		matches, err = b.store.MatchVisit(ctx, b.guildRowID, b.serverID, j.playerID, j.x, j.z)
	}
	if err != nil {
		slog.Warn("component=base_black_box", "msg", "match failed", "server_id", b.serverID, "err", err.Error())
		return
	}
	for _, m := range matches {
		if err := b.store.Record(ctx, m, j.kind, j.name, j.detail); err != nil {
			slog.Warn("component=base_black_box", "msg", "record failed", "base_id", m.BaseID, "err", err.Error())
		}
	}
}

// locationObserverFanout hands each batch of positions to several observers.
type locationObserverFanout []killfeed.LocationObserver

func (f locationObserverFanout) ObserveLocations(samples []killfeed.LocationSample) {
	for _, o := range f {
		if o != nil {
			o.ObserveLocations(samples)
		}
	}
}

// startBaseBlackBoxPruner removes history older than each server's retention, hourly.
func (a *App) startBaseBlackBoxPruner(ctx context.Context) {
	if a.DB == nil || a.DB.Pool == nil {
		return
	}
	repo := repository.NewBaseBlackBoxRepository(a.DB.Pool)
	prune := func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=base_black_box", "msg", "prune panic recovered", "panic", fmt.Sprint(r))
			}
		}()
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if n, err := repo.Prune(pctx); err != nil {
			slog.Warn("component=base_black_box", "msg", "prune failed", "err", err.Error())
		} else if n > 0 {
			slog.Info("component=base_black_box", "event", "pruned", "rows", n)
		}
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				prune()
			}
		}
	}()
}
