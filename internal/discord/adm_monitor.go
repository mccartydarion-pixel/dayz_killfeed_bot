package discord

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// ADMMonitorPublisher edits one private per-server message. Snapshot callbacks
// arrive from the owning worker goroutine, so no engine state is read here.
type ADMMonitorPublisher struct {
	editor         MessageEditor
	store          SetupStore
	guildID        string
	serverID       int64
	messageID      string
	channelID      string
	lastHealth     killfeed.AdmHealth
	lastFile       string
	lastRefresh    time.Time
	saveMessage    func(string)
	downloadFailed bool
	forceRefresh   bool

	// Installation route model: when set, the ADMIN_LOGS route for this
	// server's installation wins over the legacy GuildSetup.ADMMonitorChannelID
	// (cached in channelID). msgChannel is the channel messageID lives in, so a
	// route change moves the persistent message instead of editing a stale id.
	route      *RouteBinding
	msgChannel string
}

// SetRouting attaches the shared route resolver for this publisher's server.
// guildRowID is the internal guilds.id; the publisher already holds serverID,
// so resolution is per (guild, server) - never by guild alone.
func (p *ADMMonitorPublisher) SetRouting(resolver RouteResolver, guildRowID int64) {
	if p == nil || resolver == nil {
		return
	}
	p.route = NewRouteBinding(resolver, guildRowID, p.serverID, routeKeyAdminLogs)
}

// activeChannel picks exactly one destination: the ADMIN_LOGS route when one
// is configured (lookup errors and missing routes count as "none"), otherwise
// the legacy monitor channel. routed reports which. It is re-evaluated on
// every call, so a changed route takes effect on the next snapshot/download
// (in-process writes invalidate the shared resolver cache; out-of-process
// changes land within its TTL).
func (p *ADMMonitorPublisher) activeChannel() (channelID string, routed bool) {
	if id := p.route.ChannelID(); id != "" {
		return id, true
	}
	return p.legacyChannel(), false
}

// legacyChannel is the pre-route lookup, cached once found exactly as before.
func (p *ADMMonitorPublisher) legacyChannel() string {
	if p.channelID == "" && p.store != nil {
		if setup, err := p.store.Get(p.guildID); err == nil && setup != nil {
			p.channelID = setup.ADMMonitorChannelID
		}
	}
	return p.channelID
}

// deleteMessage removes a superseded monitor message, best-effort.
func (p *ADMMonitorPublisher) deleteMessage(channelID, messageID string) {
	if channelID == "" || messageID == "" {
		return
	}
	if d, ok := p.editor.(messageDeleter); ok {
		_ = d.ChannelMessageDelete(channelID, messageID)
	}
}

// HandleDownload posts a standalone admin-log message only for a state change
// worth interrupting the channel for (a healthy<->failing transition, a
// rotation, or a checkpoint failure) - never for a routine successful
// download, which happens roughly once per poll cycle during active
// gameplay and would otherwise flood the admin-logs channel with a new
// message every ~10s (the persistent Update() panel already reflects
// current file/offset/health for anyone who wants that detail on demand).
func (p *ADMMonitorPublisher) HandleDownload(report killfeed.DownloadReport) {
	if p == nil || p.editor == nil || p.store == nil {
		return
	}
	channelID, _ := p.activeChannel()
	if channelID == "" {
		return
	}
	routine := report.Result == "success" || report.Result == "success_no_new_events"
	if report.Result == "failure" {
		p.downloadFailed = true
	} else if routine && p.downloadFailed {
		report.Result = "recovered"
		p.downloadFailed = false
	}
	notable := report.Result == "recovered" || report.Result == "failure" || report.Result == "checkpoint_failed" || report.Rotation || report.Truncated
	if !notable {
		return
	}
	// Only a notable event forces the separate Update() status panel to
	// refresh ahead of its own interval throttle - a routine download must
	// never bypass that throttle (see above).
	p.forceRefresh = true
	_, _ = p.editor.ChannelMessageSendEmbed(channelID, BuildADMDownloadEmbed(report))
}

func BuildADMDownloadEmbed(report killfeed.DownloadReport) *discordgo.MessageEmbed {
	color := presentation.SuccessGreen
	title := "📥 ADM DOWNLOADED"
	status := "SUCCESS"
	if report.Result == "failure" {
		color = presentation.ErrorRed
		title = "🚨 ADM DOWNLOAD FAILED"
		status = "DOWNLOAD FAILED\nRetry scheduled"
	} else if report.Result == "recovered" {
		color = presentation.SuccessGreen
		title = "✅ ADM DOWNLOAD RECOVERED"
		status = "HEALTHY\nProcessing resumed"
	} else if report.Result == "success_no_new_events" {
		status = "WAITING FOR COMPLETE ADM LINE"
	} else if report.Result == "checkpoint_failed" {
		color = presentation.WarningAmber
		status = "CHECKPOINT FAILED\nRetry scheduled"
	}
	if report.Rotation {
		title = "🔄 ADM ROTATION"
	}
	embed := &discordgo.MessageEmbed{Author: presentation.ChampionAuthor(), Title: title, Color: color, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION • ADM MONITOR"}}
	add := func(name, value string) {
		embed.Fields = append(embed.Fields, presentation.StatusField(name, value, true))
	}
	if report.PreviousFile != "" && report.Rotation {
		add("PREVIOUS", safeMonitorText(report.PreviousFile))
	}
	add("FILE", safeMonitorText(report.File))
	if report.RemoteSize > 0 {
		add("REMOTE SIZE", formatBytes(report.RemoteSize))
	}
	if report.DownloadedBytes > 0 {
		add("DOWNLOADED", formatBytes(report.DownloadedBytes))
	}
	if report.NewBytes >= 0 {
		add("NEW DATA", formatBytes(report.NewBytes))
	}
	add("EVENTS PARSED", fmt.Sprintf("%d", report.EventsParsed))
	if report.PreviousOffset != report.NewOffset {
		add("PROCESSED OFFSET", fmt.Sprintf("%s → %s", formatBytes(report.PreviousOffset), formatBytes(report.NewOffset)))
	}
	if report.ErrorClass != "" {
		add("ERROR", report.ErrorClass)
	}
	add("RESULT", status)
	if !report.At.IsZero() {
		add("COMPLETED", fmt.Sprintf("<t:%d:R>", report.At.Unix()))
	}
	return embed
}

func NewADMMonitorPublisher(editor MessageEditor, store SetupStore, guildID string, serverID int64, messageID string, saveMessage func(string)) *ADMMonitorPublisher {
	return &ADMMonitorPublisher{editor: editor, store: store, guildID: guildID, serverID: serverID, messageID: messageID, saveMessage: saveMessage}
}

func (p *ADMMonitorPublisher) Update(snapshot killfeed.AdmSnapshot) {
	if p == nil || p.editor == nil {
		return
	}
	channelID, routed := p.activeChannel()
	if channelID == "" {
		return
	}
	// The destination changed while a monitor message exists elsewhere (route
	// added, changed or removed): retire the old message and post a fresh one
	// right away, rather than editing an id that does not exist in the new
	// channel or waiting out the refresh interval.
	if p.messageID != "" && p.msgChannel != "" && p.msgChannel != channelID {
		p.deleteMessage(p.msgChannel, p.messageID)
		p.messageID = ""
		p.forceRefresh = true
	}
	now := time.Now()
	health := snapshot.Health(now, 5*time.Minute)
	important := health != p.lastHealth || snapshot.CurrentFile != p.lastFile || (!snapshot.LastRotationAt.IsZero() && snapshot.LastRotationAt.After(p.lastRefresh))
	if !important && !p.forceRefresh && !p.lastRefresh.IsZero() && now.Sub(p.lastRefresh) < killfeed.ADMMonitorRefreshInterval {
		return
	}
	embed := BuildADMMonitorEmbed(snapshot, now)
	var msg *discordgo.Message
	var err error
	posted := false
	if p.messageID == "" {
		msg, err = p.editor.ChannelMessageSendEmbed(channelID, embed)
		posted = true
	} else {
		msg, err = p.editor.ChannelMessageEditEmbed(channelID, p.messageID, embed)
		if err != nil && routed && isUnknownMessage(err) {
			// The stored id is not in the routed channel: the route was set
			// while the process was down (the id belongs to the legacy channel)
			// or the message was deleted. Retire a stray legacy copy, then
			// post once in the routed channel. Only on route-derived
			// destinations, so legacy behaviour is unchanged.
			if legacy := p.legacyChannel(); legacy != "" && legacy != channelID {
				p.deleteMessage(legacy, p.messageID)
			}
			msg, err = p.editor.ChannelMessageSendEmbed(channelID, embed)
			posted = true
		}
	}
	if err != nil {
		return
	}
	if posted && msg != nil {
		p.messageID = msg.ID
		if p.saveMessage != nil {
			p.saveMessage(msg.ID)
		}
	}
	p.msgChannel = channelID
	p.lastHealth = health
	p.lastFile = snapshot.CurrentFile
	p.lastRefresh = now
	p.forceRefresh = false
}

func BuildADMMonitorEmbed(snapshot killfeed.AdmSnapshot, now time.Time) *discordgo.MessageEmbed {
	health := snapshot.Health(now, 5*time.Minute)
	color := presentation.SuccessGreen
	if health == killfeed.AdmStale || health == killfeed.AdmSwitching {
		color = presentation.WarningAmber
	}
	if health == killfeed.AdmSelectionStale {
		color = presentation.ErrorRed
	}
	if health == killfeed.AdmError {
		color = presentation.ErrorRed
	}
	embed := presentation.NewChampionEmbed("ADM MONITOR", color)
	embed.Description = fmt.Sprintf("**STATUS**\n%s", strings.ToUpper(string(health)))
	add := func(name, value string) {
		if value != "" {
			embed.Fields = append(embed.Fields, presentation.StatusField(name, value, true))
		}
	}
	add("CURRENT ADM", safeMonitorText(snapshot.CurrentFile))
	add("NEWEST DISCOVERED ADM", safeMonitorText(snapshot.NewestDiscoveredFile))
	if !snapshot.NewestDiscoveredModified.IsZero() {
		add("NEWEST MODIFIED", fmt.Sprintf("<t:%d:R>", snapshot.NewestDiscoveredModified.Unix()))
	}
	add("CANDIDATE COUNT", fmt.Sprintf("%d", snapshot.CandidateCount))
	add("SELECTION REASON", snapshot.SelectionReason)
	if snapshot.NewestDiscoveredFile != "" && snapshot.CurrentFile != snapshot.NewestDiscoveredFile {
		add("SELECTION MATCH", "NO")
	}
	if !snapshot.Modified.IsZero() {
		add("REMOTE MODIFIED", fmt.Sprintf("<t:%d:R>", snapshot.Modified.Unix()))
	}
	if snapshot.FileSize > 0 {
		add("REMOTE SIZE", formatBytes(snapshot.FileSize))
	}
	if snapshot.ProcessedOffset > 0 {
		add("PROCESSED", formatBytes(snapshot.ProcessedOffset))
	}
	if snapshot.FileSize >= snapshot.ProcessedOffset && snapshot.FileSize > 0 {
		add("UNREAD", formatBytes(snapshot.FileSize-snapshot.ProcessedOffset))
	}
	if !snapshot.LastPoll.IsZero() {
		add("LAST METADATA CHECK", fmt.Sprintf("<t:%d:R>", snapshot.LastPoll.Unix()))
	}
	if !snapshot.LastLogChange.IsZero() {
		add("LAST REMOTE CHANGE", fmt.Sprintf("<t:%d:R>", snapshot.LastLogChange.Unix()))
	}
	if !snapshot.LastDownload.IsZero() {
		add("LAST DOWNLOAD", fmt.Sprintf("<t:%d:R>", snapshot.LastDownload.Unix()))
	}
	if snapshot.PendingPartialLine != "" {
		add("PENDING PARTIAL LINE", "YES")
	}
	add("CHECKPOINT", "CURRENT")
	add("ONLINE PLAYERS", fmt.Sprintf("%d", snapshot.OnlineCount))
	if !snapshot.LastRotationAt.IsZero() {
		add("LAST ROTATION", fmt.Sprintf("<t:%d:R>", snapshot.LastRotationAt.Unix()))
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "CHAMPION • ADM MONITOR • AUTO-REFRESH EVERY 5 MIN"}
	return embed
}

func safeMonitorText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\\", "/")
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		value = value[idx+1:]
	}
	return safePanelText(value)
}

func formatBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	return fmt.Sprintf("%.1f KB", float64(value)/1024)
}
