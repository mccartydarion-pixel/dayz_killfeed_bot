package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

// The server-down alert (docs/SERVER_DOWN_ALERT.md): one staff alert when a game server's log
// files have stopped growing for long enough that the game is not running, and one when they
// grow again. The decision is ownerops.ServerDown; this file feeds it and posts the result.

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
		st.alerted, st.downSince = false, time.Time{}
		return &discord.AdminAlert{GuildRowID: guildID, ServerID: serverID, Kind: discord.AlertKindServerDown, Severity: discord.AlertResolved,
			Headline: "Game server is back",
			Detail:   "The server's log files are being written again. It was quiet for " + roughDuration(was) + ".", At: in.Now}
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
// Stillness is never counted from before watchingSince, the moment this process began watching:
// the growth times a new process starts with are the stored ones from before it existed, and the
// first minute after a deploy would otherwise look like an outage.
func (a *App) serverDownInput(sup *livesync.Supervisor, serverID int64, after time.Duration, watchingSince, now time.Time) ownerops.DownInput {
	snap := sup.Snapshot()
	in := ownerops.DownInput{Now: now, After: after, ListingFailed: snap.ListingError != ""}
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
	in.LastGrowth = serverDownGrowthFloor(in.LastGrowth, watchingSince)
	return in
}

// serverDownGrowthFloor is the growth time to judge by: the newest growth seen, but never earlier
// than when this process began watching.
func serverDownGrowthFloor(lastGrowth, watchingSince time.Time) time.Time {
	if watchingSince.After(lastGrowth) {
		return watchingSince
	}
	return lastGrowth
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
	watchingSince := time.Now().UTC()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			alert := st.step(a.serverDownInput(sup, row.ID, after, watchingSince, now.UTC()), row.GuildID, row.ID)
			if alert == nil {
				continue
			}
			slog.Warn("component=server_down", "event", "server_down_"+strings.ToLower(string(alert.Severity)), "server_id", row.ID, "guild_id", row.GuildID)
			if a.AdminAlerts != nil {
				a.AdminAlerts.Publish(*alert)
			}
			a.dmOwnerServerDown(ctx, *alert)
		}
	}
}

// dmOwnerServerDown sends the alert to the organization owner by DM as well: an outage at night
// is seen sooner there than in a staff channel. A failure (DMs closed, no owner on record) is
// logged and nothing else: the staff alert has already gone out.
func (a *App) dmOwnerServerDown(ctx context.Context, alert discord.AdminAlert) {
	if a.DB == nil || a.DB.Pool == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ownerID, err := repository.NewCaseBaseRequestRepository(a.DB.Pool).OwnerDiscordForServer(opCtx, alert.GuildRowID, alert.ServerID)
	if err != nil || ownerID == "" {
		if err != nil {
			slog.Warn("component=server_down", "event", "owner_lookup_failed", "server_id", alert.ServerID, "err", err.Error())
		}
		return
	}
	session := a.Discord.Session()
	ch, err := session.UserChannelCreate(ownerID)
	if err == nil {
		_, err = session.ChannelMessageSendComplex(ch.ID, serverDownDM(alert, a.serverName(alert.ServerID)))
	}
	if err != nil {
		slog.Warn("component=server_down", "event", "owner_dm_failed", "server_id", alert.ServerID, "err", err.Error())
	}
}

// serverDownDM is the owner's copy of the alert. It carries no mention and no link.
func serverDownDM(alert discord.AdminAlert, serverName string) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{discord.BuildAdminAlertEmbed(alert, serverName)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}
