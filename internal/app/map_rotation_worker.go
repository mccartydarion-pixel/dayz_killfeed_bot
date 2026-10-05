package app

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/livemap"
	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/maprotation/charwipe"
	"github.com/yourname/dayz-killfeed/internal/maprotation/mapswitch"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The map rotation worker (docs/MAP_ROTATION.md). Once a minute it looks at every installation
// whose owner switched the rotation on and takes the next step the planner (maprotation.Plan)
// gives: count a restart, open or close a vote, decide the next map, write the two server files.
//
// This is the only file that imports internal/maprotation/mapswitch and
// internal/maprotation/charwipe (enforced by the isolation test in internal/shop/missionwrite).
// Before every write it checks the three switches again: the map_rotation feature flag, the plan
// and the owner's `enabled` setting. With any of them off the worker does nothing for the
// installation: no Nitrado call, no state change, no Discord post.
//
// The one exception is a server Champion itself stopped to clear the saved characters
// (mapRotationWipeRecover): it is started again whatever was switched off in the meantime.

const (
	mapRotationInterval    = time.Minute
	mapRotationLease       = 5 * time.Minute
	mapRotationPassTimeout = 4 * time.Minute
	mapRotationRestartTTL  = 5 * time.Minute
	// mapRotationResumeFor: a switch interrupted by a crash is finished within this time, at most
	// mapRotationMaxAttempts times. After that nothing more is written and a person must look.
	mapRotationResumeFor   = 30 * time.Minute
	mapRotationMaxAttempts = 3
	mapRotationMaxSteps    = 6
	// mapRotationWipeLease: clearing the saved characters stops and starts the server, which takes
	// minutes. The installation's lease is held this long, longer than the procedure can take
	// (charwipe's Total), so no second bot process touches the server meanwhile.
	mapRotationWipeLease = 10 * time.Minute
	// mapRotationWipeGiveUp: an interrupted clearing whose server cannot even be asked (no Nitrado
	// connection) is retried each minute for this long, then the rotation stops and staff are told.
	mapRotationWipeGiveUp = 30 * time.Minute
	// mapRotationAppliedMessage is the switch's message when it is closed after an interrupted
	// clearing: the files were verified before the clearing began.
	mapRotationAppliedMessage = "The map's files were written and verified."
)

// mapRotationWipeConfig lets tests shorten the waits of clearing the characters and run it in the
// worker's own goroutine. The zero value is production: default waits, in the background.
type mapRotationWipeConfig struct {
	opts   charwipe.Options
	inline bool
}

// mapRotationRestartCache remembers each installation's next scheduled restart for a few minutes,
// so the worker does not ask Nitrado for the task list every tick.
type mapRotationRestartCache struct {
	mu      sync.Mutex
	entries map[int64]mapRotationRestartEntry
}

type mapRotationRestartEntry struct {
	at      *time.Time
	fetched time.Time
}

func mapRotationWorkerName() string { return "map-rotation:" + shopWorkerName() }

// startMapRotationWorker starts the worker. It is cheap when nobody uses the feature: one query
// that returns no rows.
func (a *App) startMapRotationWorker(ctx context.Context) {
	if a.MapRotation == nil {
		return
	}
	go func() {
		t := time.NewTicker(mapRotationInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.mapRotationTick(ctx, time.Now().UTC())
			}
		}
	}()
}

func (a *App) mapRotationTick(ctx context.Context, now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=map_rotation", "msg", "worker panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	// First of all: a server that was stopped to clear characters and may still be down.
	a.mapRotationWipeRecover(ctx, now)
	targets, err := a.MapRotation.ActiveInstallations(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("component=map_rotation", "event", "list_failed", "err", err.Error())
		}
		return
	}
	for _, t := range targets {
		a.mapRotationOne(ctx, t, now)
	}
}

// mapRotationAllowed is the flag and the plan for one installation (the owner's switch is read
// with the settings).
func (a *App) mapRotationAllowed(ctx context.Context, t repository.MapRotationTarget) bool {
	reason, _, err := a.mapRotationAvailable(ctx, t.OrganizationID, t.InstallationID)
	return err == nil && reason == ""
}

func (a *App) mapRotationOne(parent context.Context, t repository.MapRotationTarget, now time.Time) {
	if a.mapRotationWipeBusy(t.InstallationID) || !a.mapRotationAllowed(parent, t) {
		return
	}
	ctx, cancel := context.WithTimeout(parent, mapRotationPassTimeout)
	defer cancel()
	owner := mapRotationWorkerName()
	claimed, err := a.MapRotation.Claim(ctx, t.InstallationID, owner, now, now.Add(mapRotationLease))
	if err != nil || !claimed {
		return
	}
	defer func() {
		if a.mapRotationWipeBusy(t.InstallationID) {
			return // the characters are being cleared in the background, which keeps the lease
		}
		a.mapRotationRelease(parent, t.InstallationID, owner)
	}()

	for i := 0; i < mapRotationMaxSteps; i++ {
		snap, err := a.MapRotation.Load(ctx, t.InstallationID)
		if err != nil {
			slog.Warn("component=map_rotation", "event", "load_failed", "installation_id", t.InstallationID, "err", err.Error())
			return
		}
		if !snap.Settings.Enabled || snap.Settings.HaltedReason != "" {
			break
		}
		more, err := a.mapRotationStep(ctx, t, snap, now)
		if err != nil {
			slog.Warn("component=map_rotation", "event", "step_failed", "installation_id", t.InstallationID, "err", err.Error())
			break
		}
		if !more {
			break
		}
	}
	a.mapRotationNotices(ctx, t, now)
}

// mapRotationStep takes one step. It reports whether another step may follow in this tick.
func (a *App) mapRotationStep(ctx context.Context, t repository.MapRotationTarget, snap repository.MapRotationSnapshot, now time.Time) (bool, error) {
	// A switch interrupted by a crash comes first: finish it, or give up without writing.
	pending, err := a.MapRotation.PendingSwitch(ctx, t.InstallationID)
	if err != nil {
		return false, err
	}
	if pending != nil {
		if charwipe.InProgress(pending.WipeState) {
			// The server may be stopped. mapRotationWipeRecover deals with that and nothing else
			// happens for the installation until it has.
			return false, nil
		}
		return false, a.mapRotationResume(ctx, t, snap, *pending, now)
	}

	s := snap.Settings
	maps := rotationMaps(snap.Maps)
	bootFile, bootAt, err := a.MapRotation.BootSession(ctx, t.ServerID)
	if err != nil {
		return false, err
	}
	obs := maprotation.Observation{BootFile: bootFile, BootAt: bootAt}
	if bootFile != "" && bootFile == s.LastBootFile && s.Phase != maprotation.PhaseDone && maprotation.FinalPeriod(s.EveryRestarts, s.RestartsSinceSwitch) {
		next, known := a.mapRotationNextRestart(ctx, t, now)
		if !known && s.Phase == maprotation.PhaseIdle {
			// Nitrado did not answer. Whether a vote opens now or later depends on the schedule, so
			// nothing is decided on a guess; the next tick asks again.
			return false, nil
		}
		obs.NextRestart = next
		if known {
			if err := a.MapRotation.SetNextRestart(ctx, t.InstallationID, next); err != nil {
				return false, err
			}
		}
	}
	st := maprotation.State{
		CurrentMapID: int64Value(s.CurrentMapID), LastBootFile: s.LastBootFile, RestartsSinceSwitch: s.RestartsSinceSwitch, Phase: s.Phase,
		NextMapID: int64Value(s.NextMapID), StaffNextMapID: int64Value(s.StaffNextMapID),
	}
	if s.PhaseStartedAt != nil {
		st.PhaseStartedAt = *s.PhaseStartedAt
	}
	if s.NextDecidedBy != nil {
		st.NextDecidedBy = *s.NextDecidedBy
	}
	if s.WipeRestartAt != nil {
		st.WipeRestartAt = *s.WipeRestartAt
	}
	if bootFile != "" && s.LastBootFile != "" && bootFile != s.LastBootFile {
		// A new boot: does it load a switch that was applied?
		if st.AwaitingActivation, err = a.MapRotation.SwitchAwaitingRestart(ctx, t.InstallationID); err != nil {
			return false, err
		}
	}
	if snap.Vote != nil && snap.Vote.Status == "OPEN" {
		st.VoteOpen, st.VoteClosesAt = true, snap.Vote.ClosesAt
	}
	cfg := maprotation.Settings{EveryRestarts: s.EveryRestarts, Order: s.Order, VoteEnabled: s.VoteEnabled, VoteMinutes: s.VoteMinutes}
	step := maprotation.Plan(cfg, st, maps, obs, now, rand.Intn)

	switch step.Kind {
	case maprotation.StepBaseline:
		// The first period is counted from now, not from the boot: the rotation was only just
		// switched on, so a vote (or the wait before a switch) gets its full length.
		return true, a.MapRotation.Baseline(ctx, t.InstallationID, bootFile, now)
	case maprotation.StepRestart:
		// A further boot shortly after Champion's own restart (clearing the characters) is the same
		// restart: remembered, not counted.
		activated, err := a.MapRotation.RecordRestart(ctx, t.InstallationID, bootFile, bootAt, now, step.Restart != maprotation.RestartSame)
		if err != nil {
			return false, err
		}
		a.mapRotationForgetRestart(t.InstallationID)
		slog.Info("component=map_rotation", "event", "restart_seen", "installation_id", t.InstallationID, "map_activated", activated != nil, "effect", string(step.Restart))
		return true, nil
	case maprotation.StepOpenVote:
		options := make([]repository.MapRotationVoteOption, 0, len(step.Options))
		for _, o := range step.Options {
			opt := repository.MapRotationVoteOption{MapID: o.ID, Name: o.Name}
			for _, m := range snap.Maps {
				if m.ID == o.ID {
					opt.ImageURL = m.ImageURL
				}
			}
			options = append(options, opt)
		}
		id, err := a.MapRotation.OpenVote(ctx, t.InstallationID, now, step.ClosesAt, bootFile, options)
		if err != nil {
			return false, err
		}
		slog.Info("component=map_rotation", "event", "vote_opened", "installation_id", t.InstallationID, "vote_id", id, "options", len(options))
		return false, nil
	case maprotation.StepCloseVote:
		vote := snap.Vote
		ids := make([]int64, 0, len(vote.Options))
		counts := map[int64]int{}
		for _, o := range vote.Options {
			ids = append(ids, o.MapID)
			counts[o.MapID] = o.Votes
		}
		m, by, ok := maprotation.ResolveVote(s.Order, maps, st.CurrentMapID, st.StaffNextMapID, ids, counts, rand.Intn)
		if !ok {
			return false, fmt.Errorf("no map to decide on")
		}
		if err := a.MapRotation.Decide(ctx, t.InstallationID, &vote.ID, m.ID, m.Name, by, now); err != nil {
			return false, err
		}
		slog.Info("component=map_rotation", "event", "vote_closed", "installation_id", t.InstallationID, "vote_id", vote.ID, "decided_by", by, "votes", vote.TotalVotes)
		return true, nil
	case maprotation.StepDecide:
		if err := a.MapRotation.Decide(ctx, t.InstallationID, nil, step.Map.ID, step.Map.Name, step.DecidedBy, now); err != nil {
			return false, err
		}
		return true, nil
	case maprotation.StepApply:
		next, ok := maprotation.Find(maps, st.NextMapID)
		if !ok || !next.Enabled || st.NextDecidedBy == "" {
			return false, nil
		}
		return false, a.mapRotationSwitch(ctx, t, snap, next.ID, next.Name, next.MapFile, next.SpawnFile, st.NextDecidedBy, bootFile, now)
	}
	return false, nil
}

// mapRotationNextRestart is the server's next scheduled restart as Nitrado lists it (the same
// reading the live map's clock uses). at is nil when Nitrado lists no restart task; known is false
// when Nitrado could not be asked at all (which is never cached).
func (a *App) mapRotationNextRestart(ctx context.Context, t repository.MapRotationTarget, now time.Time) (at *time.Time, known bool) {
	c := &a.mapRotationRestarts
	c.mu.Lock()
	if e, ok := c.entries[t.InstallationID]; ok && now.Sub(e.fetched) < mapRotationRestartTTL && now.Sub(e.fetched) >= 0 && (e.at == nil || e.at.After(now)) {
		c.mu.Unlock()
		return e.at, true
	}
	c.mu.Unlock()
	remote, err := a.mapRotationRemote(ctx, t)
	if err != nil {
		return nil, false
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	tasks, err := remote.ListScheduledTasks(rctx, t.NitradoServiceID)
	cancel()
	if err != nil {
		return nil, false
	}
	entry := mapRotationRestartEntry{fetched: now}
	if next, ok := livemap.NextRestart(liveMapTasks(tasks), now); ok {
		entry.at = &next
	}
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[int64]mapRotationRestartEntry{}
	}
	c.entries[t.InstallationID] = entry
	c.mu.Unlock()
	return entry.at, true
}

func (a *App) mapRotationForgetRestart(installationID int64) {
	a.mapRotationRestarts.mu.Lock()
	delete(a.mapRotationRestarts.entries, installationID)
	a.mapRotationRestarts.mu.Unlock()
}

// mapRotationResume deals with a switch a crash left PENDING. Within mapRotationResumeFor it is
// carried on: files that already have the wanted content are not written again and the backup
// saved by the first attempt is kept. Later than that nothing is written: the switch is closed as
// failed and, if a write may have happened, the rotation stops for a person to look.
func (a *App) mapRotationResume(ctx context.Context, t repository.MapRotationTarget, snap repository.MapRotationSnapshot, sw repository.MapRotationSwitch, now time.Time) error {
	if charwipe.Closed(sw.WipeState) {
		// The files were verified and the characters dealt with; only closing the switch was left.
		// Nothing is written and nothing is cleared a second time.
		return a.mapRotationFinish(ctx, t, sw.ID, mapswitch.Result{Status: mapswitch.StatusApplied, Message: withWipeNote(mapRotationAppliedMessage, sw.WipeNote)})
	}
	if now.Sub(sw.CreatedAt) <= mapRotationResumeFor && sw.Attempts < mapRotationMaxAttempts {
		return a.mapRotationSwitch(ctx, t, snap, int64Value(sw.MapID), sw.MapName, sw.MapFile, sw.SpawnFile, sw.DecidedBy, snap.Settings.LastBootFile, now)
	}
	msg, halt := "The switch was interrupted before anything was written to the server. Nothing was changed.", ""
	if sw.BackupSaved {
		msg = "The switch was interrupted while the server's files were being written and could not be finished. Check cfggameplay.json and cfgplayerspawnpoints.xml before the next restart; Champion keeps a backup of both."
		halt = msg
	}
	return a.MapRotation.FinishSwitch(ctx, t.InstallationID, sw.ID, repository.MapSwitchFailed, msg, halt, now)
}

// mapRotationSwitch writes the files for one map and records what happened. The PENDING row is
// written first, so a crash at any point leaves a record that the next tick picks up. That row
// also holds the map's spawn contents as stored at that moment; a map without any fails here,
// before anything is sent to the server, and staff are told like for any failed switch.
func (a *App) mapRotationSwitch(ctx context.Context, t repository.MapRotationTarget, snap repository.MapRotationSnapshot, mapID int64, name, mapFile, spawnFile, decidedBy, bootFile string, now time.Time) error {
	// The three switches, once more, immediately before anything can be written.
	if !snap.Settings.Enabled || !a.mapRotationAllowed(ctx, t) {
		return nil
	}
	sw, _, err := a.MapRotation.BeginSwitch(ctx, t.InstallationID, mapID, name, mapFile, spawnFile, decidedBy, bootFile, now)
	if err != nil {
		return err
	}
	finish := func(res mapswitch.Result) error { return a.mapRotationFinish(ctx, t, sw.ID, res) }
	remote, err := a.mapRotationRemote(ctx, t)
	if err != nil {
		return finish(mapswitch.Result{Status: mapswitch.StatusFailed, Message: "The server's Nitrado connection could not be opened. Nothing was changed."})
	}
	// Every map file of this installation is Champion's to remove from objectSpawnersArr: the
	// configured maps and the file of the current map (which may have been removed from the list).
	owned := make([]string, 0, len(snap.Maps)+1)
	for _, m := range snap.Maps {
		owned = append(owned, m.MapFile)
	}
	if snap.Settings.CurrentMapFile != nil {
		owned = append(owned, *snap.Settings.CurrentMapFile)
	}
	// The spawn contents are the copy the switch took when it began (BeginSwitch), so a resumed
	// switch writes what its first attempt wrote, whatever was uploaded since.
	req := mapswitch.Request{ServiceID: t.NitradoServiceID, MapName: name, MapFile: mapFile, SpawnFile: spawnFile, Spawns: sw.SpawnXML, Owned: owned}
	if sw.BackupSaved {
		if len(sw.SpawnXML) == 0 {
			// Only a switch that began before spawn files were stored in Champion can be here: an
			// earlier attempt may have written a file and there is nothing to finish it with.
			return finish(mapswitch.Result{Status: mapswitch.StatusFailed, NeedsAttention: true,
				Message: "The switch was interrupted while the server's files were being written and could not be finished. Check cfggameplay.json and cfgplayerspawnpoints.xml before the next restart; Champion keeps a backup of both."})
		}
		req.Prior = &mapswitch.Backup{Gameplay: sw.PrevGameplay, Spawns: sw.PrevSpawns}
	}
	res := mapswitch.Apply(ctx, remote, req, func(ctx context.Context, b mapswitch.Backup) error {
		return a.MapRotation.SaveSwitchBackup(ctx, sw.ID, b.Gameplay, b.Spawns, time.Now().UTC())
	})
	// Fresh characters: only after both files are verified in place, only when the owner switched
	// it on, and only once per switch (a switch that already began clearing never comes here: an
	// interrupted one goes to mapRotationWipeRecover, a finished one is closed in mapRotationResume).
	if res.Status == mapswitch.StatusApplied && snap.Settings.WipeCharacters && (sw.WipeState == "" || sw.WipeState == charwipe.StateNone) {
		a.mapRotationWipe(ctx, t, *sw, remote, res)
		return nil
	}
	return finish(res)
}

// mapRotationFinish records a switch's outcome, even when the pass's own deadline has passed.
func (a *App) mapRotationFinish(ctx context.Context, t repository.MapRotationTarget, switchID int64, res mapswitch.Result) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	halt := ""
	if res.NeedsAttention {
		halt = res.Message
	}
	slog.Info("component=map_rotation", "event", "switch_finished", "installation_id", t.InstallationID, "switch_id", switchID, "status", res.Status,
		"uploads", res.Writes, "needs_attention", res.NeedsAttention)
	return a.MapRotation.FinishSwitch(fctx, t.InstallationID, switchID, res.Status, res.Message, halt, time.Now().UTC())
}

func (a *App) mapRotationRelease(ctx context.Context, installationID int64, owner string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = a.MapRotation.Release(rctx, installationID, owner)
}

// --- fresh characters on a map switch ---------------------------------------------------------------
//
// With the owner's wipe_characters option on, a switch whose files are verified in place is
// followed by charwipe: stop the server, delete storage_1/players.db, start the server. It takes
// minutes, so it runs in the background while the worker goes on to the next installation; the
// installation itself is skipped (mapRotationWipeBusy) and its lease held until it is over.

func (a *App) mapRotationWipeBusy(installationID int64) bool {
	_, busy := a.mapRotationWiping.Load(installationID)
	return busy
}

func withWipeNote(message, note string) string {
	return strings.TrimSpace(strings.TrimSpace(message) + " " + strings.TrimSpace(note))
}

// mapRotationWipeSave stores a state of the clearing on the switch before the step it names.
func (a *App) mapRotationWipeSave(t repository.MapRotationTarget, switchID int64) charwipe.SaveFunc {
	return func(ctx context.Context, state string) error {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		return a.MapRotation.SetWipeState(sctx, t.InstallationID, switchID, state, time.Now().UTC())
	}
}

// mapRotationWipeRun runs work for the installation with its lease held for the length of a
// clearing, in the background (or, in tests, at once). It reports false when the lease could not
// be taken, in which case work is not run.
func (a *App) mapRotationWipeRun(ctx context.Context, t repository.MapRotationTarget, work func(bg context.Context)) bool {
	owner := mapRotationWorkerName()
	bg := context.WithoutCancel(ctx)
	lctx, cancel := context.WithTimeout(bg, 15*time.Second)
	now := time.Now().UTC()
	claimed, err := a.MapRotation.Claim(lctx, t.InstallationID, owner, now, now.Add(mapRotationWipeLease))
	cancel()
	if err != nil || !claimed {
		return false
	}
	a.mapRotationWiping.Store(t.InstallationID, struct{}{})
	run := func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("component=map_rotation", "msg", "character clearing panic recovered", "installation_id", t.InstallationID, "panic", fmt.Sprint(r))
			}
			a.mapRotationRelease(bg, t.InstallationID, owner)
			a.mapRotationWiping.Delete(t.InstallationID)
		}()
		work(bg)
	}
	if a.mapRotationWipeCfg.inline {
		run()
	} else {
		go run()
	}
	return true
}

// mapRotationWipe clears the saved characters after an applied switch and then closes the switch
// with the outcome in its message. The switch stays APPLIED whatever the clearing did.
func (a *App) mapRotationWipe(ctx context.Context, t repository.MapRotationTarget, sw repository.MapRotationSwitch, remote charwipe.Remote, res mapswitch.Result) {
	started := a.mapRotationWipeRun(ctx, t, func(bg context.Context) {
		// Until the stop is requested a minute is plenty; from then on charwipe works on a context
		// of its own.
		rctx, cancel := context.WithTimeout(bg, time.Minute)
		defer cancel()
		out := charwipe.Run(rctx, remote, t.NitradoServiceID, a.mapRotationWipeSave(t, sw.ID), a.mapRotationWipeCfg.opts)
		a.mapRotationWipeClose(bg, t, sw, out, res)
	})
	if !started {
		// The lease could not be extended, so the server is not touched.
		a.mapRotationWipeClose(context.WithoutCancel(ctx), t, sw, charwipe.Outcome{Reason: charwipe.ReasonNotRecorded}, res)
	}
}

// mapRotationWipeClose records the outcome of a clearing, tells staff when the characters were not
// cleared or the server is down, and closes the switch. When the server could not be seen
// starting again, the rotation stops in the same transaction that records the outcome.
func (a *App) mapRotationWipeClose(bg context.Context, t repository.MapRotationTarget, sw repository.MapRotationSwitch, out charwipe.Outcome, res mapswitch.Result) {
	note, halt := out.Note(), ""
	if out.ServerDown {
		halt = charwipe.ServerDownMessage
	}
	slog.Info("component=map_rotation", "event", "characters_cleared", "installation_id", t.InstallationID, "switch_id", sw.ID, "cleared", out.Cleared,
		"stop_requested", out.StopRequested, "server_down", out.ServerDown, "restarts_sent", out.Restarts)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		cctx, cancel := context.WithTimeout(bg, 15*time.Second)
		err = a.MapRotation.CloseWipe(cctx, t.InstallationID, sw.ID, out.State(), note, out.Cleared, halt, time.Now().UTC())
		cancel()
		if err == nil {
			break
		}
	}
	if !out.Cleared || out.ServerDown {
		a.mapRotationWipeAlert(t, sw.MapName, note, out.ServerDown)
	}
	if err != nil {
		// The outcome is not stored. The switch stays as it is: if the server was stopped,
		// mapRotationWipeRecover looks at it again on the next tick.
		slog.Warn("component=map_rotation", "event", "wipe_close_failed", "installation_id", t.InstallationID, "switch_id", sw.ID, "err", err.Error())
		if out.StopRequested {
			return
		}
	}
	res.Message = withWipeNote(res.Message, note)
	if err := a.mapRotationFinish(bg, t, sw.ID, res); err != nil {
		slog.Warn("component=map_rotation", "event", "switch_finish_failed", "installation_id", t.InstallationID, "switch_id", sw.ID, "err", err.Error())
	}
}

// mapRotationWipeRecover finishes clearings a crash interrupted. For each it never stops the
// server and never deletes: it makes sure the server is running and closes the switch. It does
// not ask whether the rotation is still switched on, stopped or suspended: a server Champion
// stopped is started again regardless.
func (a *App) mapRotationWipeRecover(ctx context.Context, now time.Time) {
	targets, err := a.MapRotation.WipesInProgress(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("component=map_rotation", "event", "wipe_list_failed", "err", err.Error())
		}
		return
	}
	for _, t := range targets {
		if a.mapRotationWipeBusy(t.InstallationID) {
			continue // this process is clearing them right now
		}
		t := t
		a.mapRotationWipeRun(ctx, t, func(bg context.Context) { a.mapRotationWipeRecoverOne(bg, t, now) })
	}
}

func (a *App) mapRotationWipeRecoverOne(bg context.Context, t repository.MapRotationTarget, now time.Time) {
	lctx, cancel := context.WithTimeout(bg, 15*time.Second)
	sw, err := a.MapRotation.PendingSwitch(lctx, t.InstallationID)
	cancel()
	if err != nil || sw == nil || !charwipe.InProgress(sw.WipeState) {
		return
	}
	cleared := sw.CharactersCleared != nil && *sw.CharactersCleared
	slog.Warn("component=map_rotation", "event", "wipe_resumed", "installation_id", t.InstallationID, "switch_id", sw.ID, "state", sw.WipeState)
	var out charwipe.Outcome
	remote, err := a.mapRotationRemote(bg, t)
	switch {
	case err == nil:
		out = charwipe.Resume(bg, remote, t.NitradoServiceID, sw.WipeState, cleared, a.mapRotationWipeSave(t, sw.ID), a.mapRotationWipeCfg.opts)
	case sw.WipeStateAt != nil && now.Sub(*sw.WipeStateAt) < mapRotationWipeGiveUp:
		return // Nitrado cannot be asked right now: tried again on the next tick
	default:
		// Nobody could be asked for too long. Whether the server runs is unknown; a person must look.
		out = charwipe.Outcome{Cleared: cleared, StopRequested: true, ServerDown: true}
		if !cleared {
			out.Reason = charwipe.ReasonInterrupted
		}
	}
	a.mapRotationWipeClose(bg, t, *sw, out, mapswitch.Result{Status: mapswitch.StatusApplied, Message: mapRotationAppliedMessage})
}

// mapRotationWipeAlert tells staff (ADMIN_ALERTS) that the saved characters were not cleared, or,
// critically, that the server was stopped and could not be started again.
func (a *App) mapRotationWipeAlert(t repository.MapRotationTarget, mapName, note string, serverDown bool) {
	alert := discord.AdminAlert{GuildRowID: t.GuildID, ServerID: t.ServerID, Kind: discord.AlertKindMapRotation, Severity: discord.AlertWarning,
		Headline: "Saved characters not cleared", Detail: presentation.Truncate(note, 900),
		Fields: [][2]string{{"Map", presentation.SafeName(mapName, 60)}}}
	if serverDown {
		alert.Severity = discord.AlertCritical
		alert.Headline = "Server stopped and not started again"
		alert.Fields = append(alert.Fields, [2]string{"Rotation", "Stopped until the settings are saved again"})
	}
	a.mapRotationPublish(alert)
}

func (a *App) mapRotationPublish(alert discord.AdminAlert) {
	if a.mapRotationAlert != nil {
		a.mapRotationAlert(alert)
		return
	}
	if a.AdminAlerts != nil {
		a.AdminAlerts.Publish(alert)
	}
}

// --- Discord -------------------------------------------------------------------------------------------

// mapRotationNotices sends what is due: the vote, its result, the map change and, to staff, a
// failed switch. A post that fails is tried again on the next tick while it is still current.
func (a *App) mapRotationNotices(ctx context.Context, t repository.MapRotationTarget, now time.Time) {
	due, err := a.MapRotation.DueNotices(ctx, t.InstallationID, now)
	if err != nil || len(due) == 0 {
		return
	}
	snap, err := a.MapRotation.Load(ctx, t.InstallationID)
	if err != nil {
		return
	}
	channel := ""
	if snap.Settings.AnnounceChannelID != nil {
		channel = *snap.Settings.AnnounceChannelID
	}
	link := fmt.Sprintf("%s/vote/%d", a.siteURL(), t.InstallationID)
	for _, n := range due {
		if n.Kind == repository.MapNoticeSwitchFailed {
			a.mapRotationStaffAlert(t, *n.Switch, snap.Settings.HaltedReason != "")
			_ = a.MapRotation.MarkNotice(ctx, n, now)
			continue
		}
		if channel != "" {
			var msg *discordgo.MessageSend
			switch n.Kind {
			case repository.MapNoticeVoteOpen:
				msg = buildMapVoteOpenMessage(t.ServerName, *n.Vote, link, snap.Settings.PingEveryone)
			case repository.MapNoticeVoteResult:
				msg = buildMapVoteResultMessage(t.ServerName, *n.Vote)
			case repository.MapNoticeMapChanged:
				msg = buildMapChangedMessage(t.ServerName, n.Switch.MapName, now)
			}
			if msg == nil {
				continue
			}
			if err := a.mapRotationSend(t, channel, msg); err != nil {
				slog.Warn("component=map_rotation", "event", "post_failed", "installation_id", t.InstallationID, "kind", n.Kind, "err", err.Error())
				continue // tried again next tick
			}
		}
		_ = a.MapRotation.MarkNotice(ctx, n, now)
	}
}

func (a *App) mapRotationSend(t repository.MapRotationTarget, channelID string, msg *discordgo.MessageSend) error {
	if a.mapRotationPost != nil {
		return a.mapRotationPost(t, channelID, msg)
	}
	if a.Discord == nil || a.Discord.Session() == nil {
		return fmt.Errorf("discord session unavailable")
	}
	_, err := a.feedSender(t.ServerID).ChannelMessageSendComplex(channelID, msg)
	return err
}

// mapRotationStaffAlert reports a failed switch through the staff alert route (ADMIN_ALERTS).
func (a *App) mapRotationStaffAlert(t repository.MapRotationTarget, sw repository.MapRotationSwitch, halted bool) {
	alert := discord.AdminAlert{GuildRowID: t.GuildID, ServerID: t.ServerID, Kind: discord.AlertKindMapRotation, Severity: discord.AlertWarning,
		Headline: "Map not changed", Detail: presentation.Truncate(sw.Message, 900),
		Fields: [][2]string{{"Map", presentation.SafeName(sw.MapName, 60)}}}
	if halted {
		alert.Severity = discord.AlertCritical
		alert.Fields = append(alert.Fields, [2]string{"Rotation", "Stopped until the settings are saved again"})
	}
	a.mapRotationPublish(alert)
}

func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}
}

// buildMapVoteOpenMessage is the "pick the next map" post. Only with ping does the message start
// with @everyone and allow that one mention; nothing else in it can ever ping.
func buildMapVoteOpenMessage(serverName string, v repository.MapRotationVote, link string, ping bool) *discordgo.MessageSend {
	embed := presentation.NewChampionEmbed("🗺️ Pick the next map", presentation.Crimson)
	names := make([]string, 0, len(v.Options))
	for _, o := range v.Options {
		names = append(names, o.Name) // BulletBlock makes each line safe
	}
	where := "the server"
	if strings.TrimSpace(serverName) != "" {
		where = "**" + presentation.SafeName(serverName, 60) + "**"
	}
	embed.Description = "Vote for the map " + where + " loads after the next restart.\n\n**[Vote here](" + link + ")**"
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Maps", Value: presentation.BulletBlock(names, len(names))},
		{Name: "Voting closes", Value: presentation.Timestamp(v.ClosesAt, 't') + " (" + presentation.Timestamp(v.ClosesAt, 'R') + ")", Inline: true},
		{Name: "Who can vote", Value: "Players with a linked gamertag", Inline: true},
	}
	presentation.StampEmbed(embed, v.OpensAt)
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{presentation.FitEmbed(embed)}, AllowedMentions: noMentions()}
	if ping {
		msg.Content = "@everyone Pick the next map: " + link
		msg.AllowedMentions.Parse = []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeEveryone}
	}
	return msg
}

func buildMapVoteResultMessage(serverName string, v repository.MapRotationVote) *discordgo.MessageSend {
	embed := presentation.NewChampionEmbed("🗺️ Next map: "+presentation.SafeName(v.WinnerName, 60), presentation.SuccessGreen)
	how := "The vote decided."
	switch {
	case v.DecidedBy != nil && *v.DecidedBy == maprotation.DecidedStaff:
		how = "Staff picked the next map."
	case v.TotalVotes == 0 || (v.DecidedBy != nil && *v.DecidedBy == maprotation.DecidedRotation):
		how = "Nobody voted, so the rotation picked it."
	}
	embed.Description = how + " It loads after the next restart."
	lines := make([]string, 0, len(v.Options))
	for _, o := range v.Options {
		lines = append(lines, fmt.Sprintf("%s: %s", o.Name, presentation.Plural(int64(o.Votes), "vote", "votes"))) // BulletBlock makes each line safe
	}
	embed.Fields = []*discordgo.MessageEmbedField{{Name: "Votes", Value: presentation.BulletBlock(lines, len(lines))}}
	embed.Footer = presentation.Footer(serverName, "Map vote") // the server is named in the footer
	if v.ClosedAt != nil {
		presentation.StampEmbed(embed, *v.ClosedAt)
	}
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{presentation.FitEmbed(embed)}, AllowedMentions: noMentions()}
}

func buildMapChangedMessage(serverName, mapName string, at time.Time) *discordgo.MessageSend {
	embed := presentation.NewChampionEmbed("🗺️ Map changed to "+presentation.SafeName(mapName, 60), presentation.InfoSteel)
	if strings.TrimSpace(serverName) != "" {
		embed.Description = "**" + presentation.SafeName(serverName, 60) + "** restarted on the new map."
	} else {
		embed.Description = "The server restarted on the new map."
	}
	presentation.StampEmbed(embed, at)
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{presentation.FitEmbed(embed)}, AllowedMentions: noMentions()}
}
