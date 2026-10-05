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
	"github.com/yourname/dayz-killfeed/internal/maprotation/mapswitch"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The map rotation worker (docs/MAP_ROTATION.md). Once a minute it looks at every installation
// whose owner switched the rotation on and takes the next step the planner (maprotation.Plan)
// gives: count a restart, open or close a vote, decide the next map, write the two server files.
//
// This is the only file that imports internal/maprotation/mapswitch (enforced by the isolation
// test in internal/shop/missionwrite). Before every write it checks the three switches again: the
// map_rotation feature flag, the plan and the owner's `enabled` setting. With any of them off the
// worker does nothing for the installation: no Nitrado call, no state change, no Discord post.

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
)

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
	if !a.mapRotationAllowed(parent, t) {
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
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer rcancel()
		_ = a.MapRotation.Release(rctx, t.InstallationID, owner)
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
		activated, err := a.MapRotation.RecordRestart(ctx, t.InstallationID, bootFile, bootAt, now)
		if err != nil {
			return false, err
		}
		a.mapRotationForgetRestart(t.InstallationID)
		slog.Info("component=map_rotation", "event", "restart_seen", "installation_id", t.InstallationID, "map_activated", activated != nil)
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
	// The outcome is recorded even when the pass's own deadline has passed.
	finish := func(res mapswitch.Result) error {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		halt := ""
		if res.NeedsAttention {
			halt = res.Message
		}
		slog.Info("component=map_rotation", "event", "switch_finished", "installation_id", t.InstallationID, "switch_id", sw.ID, "status", res.Status,
			"uploads", res.Writes, "needs_attention", res.NeedsAttention)
		return a.MapRotation.FinishSwitch(fctx, t.InstallationID, sw.ID, res.Status, res.Message, halt, time.Now().UTC())
	}
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
	return finish(res)
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
	if a.mapRotationAlert != nil {
		a.mapRotationAlert(alert)
		return
	}
	if a.AdminAlerts != nil {
		a.AdminAlerts.Publish(alert)
	}
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
