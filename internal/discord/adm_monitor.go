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
	editor      MessageEditor
	store       SetupStore
	guildID     string
	serverID    int64
	messageID   string
	channelID   string
	lastHealth  killfeed.AdmHealth
	lastFile    string
	lastRefresh time.Time
	saveMessage func(string)
}

func NewADMMonitorPublisher(editor MessageEditor, store SetupStore, guildID string, serverID int64, messageID string, saveMessage func(string)) *ADMMonitorPublisher {
	return &ADMMonitorPublisher{editor: editor, store: store, guildID: guildID, serverID: serverID, messageID: messageID, saveMessage: saveMessage}
}

func (p *ADMMonitorPublisher) Update(snapshot killfeed.AdmSnapshot) {
	if p == nil || p.editor == nil {
		return
	}
	if p.channelID == "" && p.store != nil {
		if setup, err := p.store.Get(p.guildID); err == nil && setup != nil {
			p.channelID = setup.ADMMonitorChannelID
		}
	}
	if p.channelID == "" {
		return
	}
	now := time.Now()
	health := snapshot.Health(now, 5*time.Minute)
	important := health != p.lastHealth || snapshot.CurrentFile != p.lastFile || (!snapshot.LastRotationAt.IsZero() && snapshot.LastRotationAt.After(p.lastRefresh))
	if !important && !p.lastRefresh.IsZero() && now.Sub(p.lastRefresh) < killfeed.ADMMonitorRefreshInterval {
		return
	}
	embed := BuildADMMonitorEmbed(snapshot, now)
	var msg *discordgo.Message
	var err error
	if p.messageID == "" {
		msg, err = p.editor.ChannelMessageSendEmbed(p.channelID, embed)
	} else {
		msg, err = p.editor.ChannelMessageEditEmbed(p.channelID, p.messageID, embed)
	}
	if err != nil {
		return
	}
	if p.messageID == "" && msg != nil {
		p.messageID = msg.ID
		if p.saveMessage != nil {
			p.saveMessage(msg.ID)
		}
	}
	p.lastHealth = health
	p.lastFile = snapshot.CurrentFile
	p.lastRefresh = now
}

func BuildADMMonitorEmbed(snapshot killfeed.AdmSnapshot, now time.Time) *discordgo.MessageEmbed {
	health := snapshot.Health(now, 5*time.Minute)
	color := presentation.SuccessGreen
	if health == killfeed.AdmStale || health == killfeed.AdmSwitching {
		color = presentation.WarningAmber
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
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED • ADM MONITOR • Auto-refreshes every 5 minutes"}
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
