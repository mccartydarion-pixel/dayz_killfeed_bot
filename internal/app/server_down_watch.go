package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/livesync"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// The server-down alert and the restart lines of the status board (docs/SERVER_DOWN_ALERT.md).
// One alert goes out when a game server's log files have stopped growing for long enough that the
// game is not running; when they grow again that same message is edited to say so. The status
// board shows the game server's state, its last restart and about when the next one is due. The
// decisions are in ownerops (ServerDown, GameState); this file feeds them and posts the result.

const serverDownCheckEvery = time.Minute

// serverDownAfter is the wait before the alert: SERVER_DOWN_ALERT_MINUTES, default 20, never
// under 10. 0 or "off" switches the alert off.
func serverDownAfter() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv("SERVER_DOWN_ALERT_MINUTES"))
	if raw == "" {
		return ownerops.DefaultServerDownAfter, true
	}
	if strings.EqualFold(raw, "off") {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return ownerops.DefaultServerDownAfter, true
	}
	if n <= 0 {
		return 0, false
	}
	after := time.Duration(n) * time.Minute
	if after < ownerops.MinServerDownAfter {
		after = ownerops.MinServerDownAfter
	}
	return after, true
}

// serverDownState is one server's alert state, in memory only: after a bot restart a server that
// is still down is reported once more.
type serverDownState struct {
	alerted   bool
	downSince time.Time
}

// step folds one observation into the state and returns the alert to post, if any.
func (st *serverDownState) step(in ownerops.DownInput, guildID, serverID int64) *discord.AdminAlert {
	silentFor, down, known := ownerops.ServerDown(in)
	if !known {
		return nil
	}
	switch {
	case down && !st.alerted:
		st.alerted, st.downSince = true, in.LastGrowth
		return &discord.AdminAlert{GuildRowID: guildID, ServerID: serverID, Kind: discord.AlertKindServerDown, Severity: discord.AlertCritical,
			Headline: "Game server looks down",
			Detail:   "The server's log files have not changed for " + roughDuration(silentFor) + ". A running server writes to them every minute or two, so the game is most likely not running. Check the server in Nitrado and start it if it is stopped.",
			Fields:   [][2]string{{"Last log activity", presentation.Timestamp(in.LastGrowth, 'R')}},
			At:       in.Now}
	case !down && st.alerted && in.LastGrowth.After(st.downSince):
		was := in.LastGrowth.Sub(st.downSince)
		from := st.downSince
		st.alerted, st.downSince = false, time.Time{}
		return &discord.AdminAlert{GuildRowID: guildID, ServerID: serverID, Kind: discord.AlertKindServerDown, Severity: discord.AlertResolved,
			Headline: "Game server is back",
			Detail:   "The server's log files are being written again. It was not running for " + roughDuration(was) + ".",
			Fields:   [][2]string{{"Stopped", presentation.Timestamp(from, 't')}, {"Back", presentation.Timestamp(in.LastGrowth, 't')}},
			At:       in.Now}
	}
	return nil
}

// roughDuration reads a duration the way staff would say it: "25 minutes", "3 hours 10 minutes".
func roughDuration(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	if mins < 1 {
		mins = 1
	}
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return presentation.Plural(int64(m), "minute", "minutes")
	case m == 0:
		return presentation.Plural(int64(h), "hour", "hours")
	}
	return fmt.Sprintf("%s %s", presentation.Plural(int64(h), "hour", "hours"), presentation.Plural(int64(m), "minute", "minutes"))
}

// serverDownInput reads what the live sync supervisor and the ADM reader know about one server.
// starts are the server-local start times of the runs seen so far.
func (a *App) serverDownInput(sup *livesync.Supervisor, serverID int64, after time.Duration, starts []time.Time, now time.Time) ownerops.DownInput {
	snap := sup.Snapshot()
	in := ownerops.DownInput{Now: now, After: after, ListingFailed: snap.ListingError != "", RunLength: ownerops.TypicalRunLength(starts)}
	if snap.LastListingAt != nil {
		in.LastListing = *snap.LastListingAt
	}
	for _, src := range snap.Sources {
		if src.LastGrowthAt != nil && src.LastGrowthAt.After(in.LastGrowth) {
			in.LastGrowth = *src.LastGrowthAt
		}
	}
	// The ADM is read by its own worker; a line from it is activity too.
	if v := a.feedWatch.view(serverID, now); v.Watching && v.Sample.LastLogLineAt.After(in.LastGrowth) {
		in.LastGrowth = v.Sample.LastLogLineAt
	}
	// File names carry the server's own clock; the learned offset turns the newest into UTC.
	if boot := newestStart(snap); !boot.IsZero() && snap.UTCOffsetMinutes != nil {
		in.BootAt = boot.Add(-time.Duration(*snap.UTCOffsetMinutes) * time.Minute)
	}
	return in
}

// newestStart is the newest server-local start time in the names of the engine reports being read.
func newestStart(snap livesync.Snapshot) time.Time {
	var newest time.Time
	for _, src := range snap.Sources {
		if src.Family == livesync.FamilyRPT && src.FileLocalStart != nil && src.FileLocalStart.After(newest) {
			newest = *src.FileLocalStart
		}
	}
	return newest
}

// knownStarts adds start to starts unless it is already there, keeping the newest 24.
func knownStarts(starts []time.Time, start time.Time) []time.Time {
	if start.IsZero() {
		return starts
	}
	for _, s := range starts {
		if s.Equal(start) {
			return starts
		}
	}
	starts = append(starts, start)
	if len(starts) > 24 {
		starts = starts[len(starts)-24:]
	}
	return starts
}

// storedStarts are the start times of the runs live sync has on record, oldest first.
func (a *App) storedStarts(ctx context.Context, row repository.GameServer) []time.Time {
	if a.LiveSync == nil {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sources, err := a.LiveSync.LoadSources(opCtx, row.GuildID, row.ID)
	if err != nil {
		slog.Warn("component=server_down", "event", "starts_load_failed", "server_id", row.ID, "err", err.Error())
		return nil
	}
	var starts []time.Time
	for _, src := range sources {
		if src.Family == livesync.FamilyRPT && src.FileLocalStart != nil {
			starts = append(starts, *src.FileLocalStart)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	var out []time.Time
	for _, s := range starts {
		out = knownStarts(out, s)
	}
	return out
}

// gameServerStatus is what the status board shows for one server. While an alert is open the
// state stays "down" until the logs grow again, even through a check that cannot see the server.
func gameServerStatus(in ownerops.DownInput, st serverDownState) (discord.GameServerStatus, bool) {
	state := ownerops.GameState(in)
	if st.alerted {
		state = ownerops.GameDown
	}
	if state == ownerops.GameUnknown {
		return discord.GameServerStatus{}, false
	}
	g := discord.GameServerStatus{State: state, StartedAt: in.BootAt, NextRestart: in.NextRestart(), DownSince: st.downSince}
	// Whole minutes: the board is edited only when what it shows changes.
	g.NextRestart = g.NextRestart.Truncate(time.Minute)
	return g, true
}

// runServerDownWatch checks one server once a minute until ctx ends.
func (a *App) runServerDownWatch(ctx context.Context, row repository.GameServer, sup *livesync.Supervisor) {
	after, on := serverDownAfter()
	if !on || sup == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=server_down", "event", "watch_panic", "server_id", row.ID, "panic", fmt.Sprint(r))
		}
	}()
	ticker := time.NewTicker(serverDownCheckEvery)
	defer ticker.Stop()
	var st serverDownState
	starts := a.storedStarts(ctx, row)
	notifier := &serverDownNotifier{transport: a.serverDownTransport(), targets: func() []string { return a.serverDownTargets(ctx, row) }, serverName: func() string { return a.serverName(row.ID) }}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			starts = knownStarts(starts, newestStart(sup.Snapshot()))
			in := a.serverDownInput(sup, row.ID, after, starts, now.UTC())
			alert := st.step(in, row.GuildID, row.ID)
			if g, known := gameServerStatus(in, st); known && a.ServerStatusBoard != nil {
				a.ServerStatusBoard.ObserveGame(row.ID, g)
			}
			if alert == nil {
				continue
			}
			slog.Warn("component=server_down", "event", "server_down_"+strings.ToLower(string(alert.Severity)), "server_id", row.ID, "guild_id", row.GuildID,
				"restart_due", in.RestartDue())
			notifier.notify(*alert)
		}
	}
}

// alertTransport is the little of Discord the notifier needs.
type alertTransport interface {
	Send(channelID string, msg *discordgo.MessageSend) (messageID string, err error)
	Edit(channelID, messageID string, embed *discordgo.MessageEmbed) error
}

type sentAlert struct{ channelID, messageID string }

// serverDownNotifier posts the down alert once and, when the server is back, edits those same
// messages instead of posting again: one message per outage in each place. The message ids are
// kept in memory, so after a bot restart the "back" notice is a new message.
type serverDownNotifier struct {
	transport  alertTransport
	targets    func() []string // channel ids: the staff alerts channel and the owner's DM
	serverName func() string
	sent       []sentAlert
}

func (n *serverDownNotifier) notify(alert discord.AdminAlert) {
	if n == nil || n.transport == nil {
		return
	}
	msg := serverDownDM(alert, n.serverName())
	if alert.Severity == discord.AlertResolved && len(n.sent) > 0 {
		for _, s := range n.sent {
			if err := n.transport.Edit(s.channelID, s.messageID, msg.Embeds[0]); err != nil {
				slog.Warn("component=server_down", "event", "alert_edit_failed", "server_id", alert.ServerID, "err", err.Error())
				if _, err := n.transport.Send(s.channelID, msg); err != nil {
					slog.Warn("component=server_down", "event", "alert_send_failed", "server_id", alert.ServerID, "err", err.Error())
				}
			}
		}
		n.sent = nil
		return
	}
	n.sent = nil
	for _, channelID := range n.targets() {
		id, err := n.transport.Send(channelID, msg)
		if err != nil {
			slog.Warn("component=server_down", "event", "alert_send_failed", "server_id", alert.ServerID, "err", err.Error())
			continue
		}
		if alert.Severity != discord.AlertResolved {
			n.sent = append(n.sent, sentAlert{channelID: channelID, messageID: id})
		}
	}
}

// sessionAlertTransport sends and edits through the bot's Discord session.
type sessionAlertTransport struct{ session *discordgo.Session }

func (t sessionAlertTransport) Send(channelID string, msg *discordgo.MessageSend) (string, error) {
	m, err := t.session.ChannelMessageSendComplex(channelID, msg)
	if err != nil {
		return "", err
	}
	return m.ID, nil
}

func (t sessionAlertTransport) Edit(channelID, messageID string, embed *discordgo.MessageEmbed) error {
	embeds := []*discordgo.MessageEmbed{embed}
	_, err := t.session.ChannelMessageEditComplex(&discordgo.MessageEdit{Channel: channelID, ID: messageID, Embeds: &embeds})
	return err
}

func (a *App) serverDownTransport() alertTransport {
	if a.Discord == nil || a.Discord.Session() == nil {
		return nil
	}
	return sessionAlertTransport{session: a.Discord.Session()}
}

// serverDownTargets are the channels the alert goes to: the server's staff alerts channel and the
// organization owner's DM. Either can be missing (no route, DMs closed, no owner on record).
func (a *App) serverDownTargets(ctx context.Context, row repository.GameServer) []string {
	var out []string
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if a.ChannelRoutes != nil {
		ch, found, err := a.ChannelRoutes.Resolve(opCtx, row.GuildID, row.ID, "ADMIN_ALERTS")
		if err != nil {
			slog.Warn("component=server_down", "event", "route_lookup_failed", "server_id", row.ID, "err", err.Error())
		} else if found && ch != "" {
			out = append(out, ch)
		}
	}
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return out
	}
	ownerID, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).OwnerDiscordForServer(opCtx, row.GuildID, row.ID)
	if err != nil {
		slog.Warn("component=server_down", "event", "owner_lookup_failed", "server_id", row.ID, "err", err.Error())
		return out
	}
	if ownerID == "" {
		return out
	}
	dm, err := a.Discord.Session().UserChannelCreate(ownerID)
	if err != nil {
		slog.Warn("component=server_down", "event", "owner_dm_failed", "server_id", row.ID, "err", err.Error())
		return out
	}
	return append(out, dm.ID)
}

// serverDownDM is the alert as a message. It carries no mention and no link.
func serverDownDM(alert discord.AdminAlert, serverName string) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{discord.BuildAdminAlertEmbed(alert, serverName)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}
