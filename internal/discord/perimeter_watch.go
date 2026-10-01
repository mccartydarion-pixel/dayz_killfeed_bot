package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Perimeter Watch: when a player position from the server log falls inside the
// area around an owner-registered base (base circle plus the owner's margin),
// and the player isn't the owner, in their faction or on the friend list, the
// base owner gets a Discord DM. Positions only exist when the log reports one,
// so a player passing quickly between reports can be missed. Off until the
// server owner turns it on; one alert per visitor per cooldown.

const (
	perimeterQueueCap   = 500
	perimeterRoute      = "PERIMETER_WATCH_DM"
	perimeterJobTimeout = 15 * time.Second
)

// PerimeterStore is the repository surface Perimeter Watch needs.
type PerimeterStore interface {
	MatchPerimeter(ctx context.Context, guildID, serverID, visitorPlayerID int64, mapX, mapZ float64) ([]repository.PerimeterMatch, error)
	RecordAlert(ctx context.Context, m repository.PerimeterMatch, visitorPlayerID int64, visitorName string) (int64, error)
	MarkDelivery(ctx context.Context, alertID int64, delivery string) error
}

// PerimeterWatchPublisher implements killfeed.LocationObserver for one server.
type PerimeterWatchPublisher struct {
	store      PerimeterStore
	dm         DMSender
	guildRowID int64
	serverID   int64
	serverName ServerNameFunc
	queue      chan killfeed.LocationSample
	dropped    atomic.Int64
}

func NewPerimeterWatchPublisher(store PerimeterStore, dm DMSender, guildRowID, serverID int64) *PerimeterWatchPublisher {
	return &PerimeterWatchPublisher{store: store, dm: dm, guildRowID: guildRowID, serverID: serverID,
		queue: make(chan killfeed.LocationSample, perimeterQueueCap)}
}

// SetServerName labels alerts with the server's name.
func (p *PerimeterWatchPublisher) SetServerName(f ServerNameFunc) {
	if p != nil {
		p.serverName = f
	}
}

// ObserveLocations queues positions for this server. Never blocks.
func (p *PerimeterWatchPublisher) ObserveLocations(samples []killfeed.LocationSample) {
	if p == nil {
		return
	}
	for _, s := range samples {
		if s.PlayerID <= 0 || s.ServerID != p.serverID {
			continue
		}
		select {
		case p.queue <- s:
		default:
			p.dropped.Add(1)
		}
	}
}

// Dropped is how many positions were skipped because the queue was full.
func (p *PerimeterWatchPublisher) Dropped() int64 {
	if p == nil {
		return 0
	}
	return p.dropped.Load()
}

// Run handles queued positions until ctx ends.
func (p *PerimeterWatchPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case s := <-p.queue:
			jobCtx, cancel := context.WithTimeout(ctx, perimeterJobTimeout)
			p.handle(jobCtx, s)
			cancel()
		}
	}
}

func (p *PerimeterWatchPublisher) handle(ctx context.Context, s killfeed.LocationSample) {
	matches, err := p.store.MatchPerimeter(ctx, p.guildRowID, p.serverID, s.PlayerID, s.X, s.Z)
	if err != nil {
		slog.Warn("component=perimeter_watch", "msg", "match failed", "server_id", p.serverID, "err", err.Error())
		return
	}
	name := strings.TrimSpace(s.Gamertag)
	for _, m := range matches {
		alertID, err := p.store.RecordAlert(ctx, m, s.PlayerID, name)
		if err != nil {
			slog.Warn("component=perimeter_watch", "msg", "record failed", "base_id", m.BaseID, "err", err.Error())
			continue
		}
		if alertID == 0 {
			continue
		}
		delivery := p.deliver(m, name, s.ObservedAt)
		if err := p.store.MarkDelivery(ctx, alertID, delivery); err != nil {
			slog.Warn("component=perimeter_watch", "msg", "mark delivery failed", "alert_id", alertID, "err", err.Error())
		}
		slog.Info("component=perimeter_watch", "event", "alert", "server_id", p.serverID, "base_id", m.BaseID, "alert_id", alertID, "delivery", delivery)
	}
}

func (p *PerimeterWatchPublisher) deliver(m repository.PerimeterMatch, visitor string, seenAt time.Time) string {
	if m.OwnerDiscordUserID == "" || p.dm == nil {
		return repository.BaseRaidDeliveryOwnerNotLinked
	}
	server := ""
	if p.serverName != nil {
		server = p.serverName(p.serverID)
	}
	msg := PerimeterWatchMessage(m.BaseName, server, visitor, m.DistanceMeters, seenAt)
	err := deliver(perimeterRoute, "", func() error {
		ch, err := p.dm.UserChannelCreate(m.OwnerDiscordUserID)
		if err != nil {
			return err
		}
		_, err = p.dm.ChannelMessageSendComplex(ch.ID, msg)
		return err
	})
	if err != nil {
		slog.Warn("component=perimeter_watch", "msg", "dm failed", "base_id", m.BaseID, "err", err.Error())
		return repository.BaseRaidDeliveryFailed
	}
	return repository.BaseRaidDeliverySent
}

// PerimeterWatchMessage is the DM the base owner gets. Player text is
// neutralised and mentions are disabled.
func PerimeterWatchMessage(baseName, serverName, visitor string, distance int, seenAt time.Time) *discordgo.MessageSend {
	when := "Just now"
	if !seenAt.IsZero() {
		when = fmt.Sprintf("<t:%d:R>", seenAt.Unix())
	}
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® PERIMETER WATCH"},
		Title:       "👀 Someone is near " + caseFallback(caseSafeText(baseName, 80), "your base"),
		Color:       presentation.WarningAmber,
		Description: "A player who isn't you, your faction or on your friend list was seen near your base.",
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Player", Value: caseFallback(caseSafeText(visitor, 64), "Unknown player"), Inline: true},
			{Name: "🖥️ Server", Value: caseFallback(caseSafeText(serverName, 100), "Your server"), Inline: true},
			{Name: "🕒 Seen", Value: when, Inline: true},
			{Name: "📍 Distance", Value: fmt.Sprintf("About %d m from the centre of your base", distance), Inline: false},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "You'll get at most one message per player every so often"},
	}
	presentation.StampEmbed(embed, time.Now())
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
}
