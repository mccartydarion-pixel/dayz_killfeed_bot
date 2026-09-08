package discord

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// maxPlayersListed caps how many names are rendered before collapsing to a
// "+ N more players" footer, keeping the embed within Discord limits.
const maxPlayersListed = 40

// MessageEditor is the subset of Discord message ops the persistent panels need.
type MessageEditor interface {
	ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error)
	ChannelMessageEditEmbed(channelID, messageID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error)
}

// OnlinePlayersPanel maintains ONE persistent online-players message, editing it
// in place on debounced state changes. It never deletes+resends or spams.
type OnlinePlayersPanel struct {
	editor    MessageEditor
	channelID string
	messageID string

	mu          sync.Mutex
	dirty       bool
	lastContent string
	timer       *time.Timer
	debounce    time.Duration
}

// NewOnlinePlayersPanel creates a panel bound to a channel (and existing message ID if known).
func NewOnlinePlayersPanel(editor MessageEditor, channelID, messageID string) *OnlinePlayersPanel {
	return &OnlinePlayersPanel{editor: editor, channelID: channelID, messageID: messageID, debounce: 2 * time.Second}
}

// MessageID returns the current persisted message ID.
func (p *OnlinePlayersPanel) MessageID() string { return p.messageID }

// SetMessageID records the persisted message ID after creation.
func (p *OnlinePlayersPanel) SetMessageID(id string) { p.messageID = id }

// MarkDirty schedules a debounced render of the latest player state. Multiple
// rapid joins/leaves collapse into a single edit.
func (p *OnlinePlayersPanel) MarkDirty(players []string, online bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = true
	if p.timer != nil {
		p.timer.Stop()
	}
	snapshot := append([]string(nil), players...)
	p.timer = time.AfterFunc(p.debounce, func() {
		p.render(snapshot, online)
	})
}

// render builds the embed and edits the existing message (or sends once if absent).
func (p *OnlinePlayersPanel) render(players []string, online bool) {
	p.mu.Lock()
	p.dirty = false
	p.mu.Unlock()

	content := strings.Join(players, "\n") + fmt.Sprintf("|%d|%t", len(players), online)
	if content == p.lastContent && p.messageID != "" {
		return // no visible change; avoid a needless edit
	}

	embed := OnlinePlayersEmbed(players, online)

	if p.messageID == "" {
		msg, err := p.editor.ChannelMessageSendEmbed(p.channelID, embed)
		if err != nil {
			slog.Warn("component=discord", "msg", "online-players initial send failed", "err", err.Error())
			return
		}
		p.messageID = msg.ID
		p.lastContent = content
		return
	}

	if _, err := p.editor.ChannelMessageEditEmbed(p.channelID, p.messageID, embed); err != nil {
		slog.Warn("component=discord", "msg", "online-players edit failed", "err", err.Error())
		return
	}
	p.lastContent = content
	slog.Debug("component=discord", "msg", "online-players updated", "players", len(players))
}

// OnlinePlayersEmbed renders the online-players embed, safely capping large lists.
func OnlinePlayersEmbed(players []string, online bool) *discordgo.MessageEmbed {
	status := "🔴 Server Offline"
	if online {
		status = "🟢 Server Online"
	}

	shown := players
	extra := 0
	if len(players) > maxPlayersListed {
		shown = players[:maxPlayersListed]
		extra = len(players) - maxPlayersListed
	}

	var list strings.Builder
	if len(shown) == 0 {
		list.WriteString("_No players online_")
	}
	for i, name := range shown {
		fmt.Fprintf(&list, "%d. %s\n", i+1, name)
	}
	if extra > 0 {
		fmt.Fprintf(&list, "\n+ %d more players", extra)
	}

	return &discordgo.MessageEmbed{
		Title:       "🏆 CHAMPION — ONLINE PLAYERS",
		Description: fmt.Sprintf("%s\n\n**Players Online**\n%d\n\n━━━━━━━━━━━━━━━━\n%s\n━━━━━━━━━━━━━━━━\n\n*Last Updated: <t:%d:R>*\n\nCHAMPION KILLFEED", status, len(players), list.String(), time.Now().Unix()),
		Color:       0x2ECC71,
	}
}

// ServerStatusEmbed builds the persistent server-status embed using only values
// we actually know. No ping/FPS/queue/map/restart data is fabricated.
func ServerStatusPanel(nitradoConnected, admConnected bool, playersOnline int, killfeedActive bool) *discordgo.MessageEmbed {
	nitrado := "🔴 Disconnected"
	if nitradoConnected {
		nitrado = "🟢 Connected"
	}
	adm := "🔴 Disconnected"
	if admConnected {
		adm = "🟢 Connected"
	}
	kf := "🔴 Inactive"
	if killfeedActive {
		kf = "🟢 Active"
	}

	return &discordgo.MessageEmbed{
		Title: "🏆 CHAMPION SERVER STATUS",
		Color: 0x3498DB,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Nitrado", Value: nitrado, Inline: true},
			{Name: "ADM Log", Value: adm, Inline: true},
			{Name: "Killfeed", Value: kf, Inline: true},
			{Name: "Players Online", Value: fmt.Sprintf("%d", playersOnline), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("CHAMPION KILLFEED • Last Update <t:%d:R>", time.Now().Unix())},
	}
}
