package discord

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
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

	// gen counts MarkDirty calls. A timer callback that has already fired can
	// not be stopped, so it may start after a newer MarkDirty; comparing its
	// generation lets the older snapshot be dropped instead of overwriting the
	// newer one. renderMu serialises renders: messageID/lastContent are read and
	// written across Discord I/O, and two overlapping renders would both see an
	// empty messageID and post two persistent messages.
	gen      uint64
	renderMu sync.Mutex
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
	p.gen++
	gen := p.gen
	if p.timer != nil {
		p.timer.Stop()
	}
	snapshot := append([]string(nil), players...)
	p.timer = time.AfterFunc(p.debounce, func() {
		p.mu.Lock()
		stale := gen != p.gen
		p.mu.Unlock()
		if stale {
			return // a newer MarkDirty owns the next render
		}
		p.render(snapshot, online)
	})
}

// render builds the embed and edits the existing message (or sends once if absent).
func (p *OnlinePlayersPanel) render(players []string, online bool) {
	p.renderMu.Lock()
	defer p.renderMu.Unlock()

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
	status := "OFFLINE"
	if online {
		status = "ONLINE"
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

	embed := presentation.NewChampionEmbed("LIVE PLAYERS", presentation.SuccessGreen)
	embed.Description = fmt.Sprintf("**STATUS**\n%s\n\n**ONLINE PLAYERS**\n%d\n\n%s", status, len(players), list.String())
	embed.Footer = presentation.UpdatedFooter(time.Now())
	presentation.StampEmbed(embed, time.Now())
	return embed
}

// ServerStatusEmbed builds the persistent server-status embed using only values
// we actually know. No ping/FPS/queue/map/restart data is fabricated.
func ServerStatusPanel(nitradoConnected, admConnected bool, playersOnline int, killfeedActive bool) *discordgo.MessageEmbed {
	nitrado := "DISCONNECTED"
	if nitradoConnected {
		nitrado = "CONNECTED"
	}
	adm := "DISCONNECTED"
	if admConnected {
		adm = "CONNECTED"
	}
	kf := "DEGRADED"
	if killfeedActive {
		kf = "HEALTHY"
	}

	embed := presentation.NewChampionEmbed("SERVER STATUS", presentation.InfoSteel)
	embed.Fields = []*discordgo.MessageEmbedField{
		presentation.StatusField("NITRADO", nitrado, true),
		presentation.StatusField("ADM LOG", adm, true),
		presentation.StatusField("KILLFEED", kf, true),
		presentation.StatusField("ONLINE PLAYERS", fmt.Sprintf("%d", playersOnline), true),
	}
	embed.Footer = presentation.UpdatedFooter(time.Now())
	presentation.StampEmbed(embed, time.Now())
	return embed
}
