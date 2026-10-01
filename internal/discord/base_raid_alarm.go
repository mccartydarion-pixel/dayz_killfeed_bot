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

// Base Raid Alarm: when a player dismantles part of a base inside an
// owner-registered base circle, and they are not the owner, in the owner's
// faction or on the base's friend list, the base owner gets a Discord DM.
//
// It only acts on the ADM "dismantled" build line, which the server writes
// when adminLogBuildActions is on. Walking up to a base and explosive damage
// are not in that line, so they never raise an alarm. The switch is off until
// the server owner turns it on, and each base alarms at most once per cooldown.

const (
	baseRaidQueueCap   = 100
	baseRaidRoute      = "BASE_RAID_ALARM_DM"
	baseRaidJobTimeout = 15 * time.Second
)

// BaseRaidStore is the repository surface the alarm needs.
type BaseRaidStore interface {
	MatchRaid(ctx context.Context, guildID, serverID int64, raiderAdmID string, mapX, mapZ float64) ([]repository.BaseRaidMatch, error)
	RecordAlert(ctx context.Context, m repository.BaseRaidMatch, ev repository.BaseRaidEvent) (int64, error)
	MarkDelivery(ctx context.Context, alertID int64, delivery string) error
}

// DMSender opens a DM channel and sends to it.
type DMSender interface {
	UserChannelCreate(recipientID string, options ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

type baseRaidJob struct {
	raiderID   string
	event      repository.BaseRaidEvent
	mapX, mapZ float64
}

// BaseRaidAlarmPublisher implements killfeed.BuildPublisher for one server.
// PublishBuild never blocks: jobs go on a bounded queue drained by Run.
type BaseRaidAlarmPublisher struct {
	store      BaseRaidStore
	dm         DMSender
	guildRowID int64
	serverID   int64
	serverName ServerNameFunc
	queue      chan baseRaidJob
	dropped    atomic.Int64
	faction    *factionSharer
}

// SetFactionSecurity also sends each alarm to the base owner's faction when
// Faction Security allows it.
func (p *BaseRaidAlarmPublisher) SetFactionSecurity(store FactionShareStore) {
	if p != nil && store != nil {
		p.faction = &factionSharer{store: store, dm: p.dm}
	}
}

func NewBaseRaidAlarmPublisher(store BaseRaidStore, dm DMSender, guildRowID, serverID int64) *BaseRaidAlarmPublisher {
	return &BaseRaidAlarmPublisher{store: store, dm: dm, guildRowID: guildRowID, serverID: serverID,
		queue: make(chan baseRaidJob, baseRaidQueueCap)}
}

// SetServerName labels alarms with the server's name.
func (p *BaseRaidAlarmPublisher) SetServerName(f ServerNameFunc) {
	if p != nil {
		p.serverName = f
	}
}

// PublishBuild queues dismantle lines that carry a player id and position.
func (p *BaseRaidAlarmPublisher) PublishBuild(ev *killfeed.Event) {
	if p == nil || ev == nil || ev.Build == nil || ev.Build.Action != "Dismantled" || ev.Build.Object == "" ||
		ev.Player == nil || ev.Player.ID == "" || ev.Player.Position == nil {
		return
	}
	name := strings.TrimSpace(ev.Player.Name)
	if name == "" {
		name = "Unknown player"
	}
	job := baseRaidJob{raiderID: ev.Player.ID, mapX: ev.Player.Position.MapX(), mapZ: ev.Player.Position.MapZ(),
		event: repository.BaseRaidEvent{RaiderName: name, Part: ev.Build.Object, Target: ev.Build.Target,
			Tool: ev.Build.Tool, TimeOfDay: ev.TimeOfDay}}
	select {
	case p.queue <- job:
	default:
		p.dropped.Add(1)
	}
}

// Dropped is how many dismantle lines were skipped because the queue was full.
func (p *BaseRaidAlarmPublisher) Dropped() int64 {
	if p == nil {
		return 0
	}
	return p.dropped.Load()
}

// Run handles queued jobs until ctx ends.
func (p *BaseRaidAlarmPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-p.queue:
			jobCtx, cancel := context.WithTimeout(ctx, baseRaidJobTimeout)
			p.handle(jobCtx, job)
			cancel()
		}
	}
}

func (p *BaseRaidAlarmPublisher) handle(ctx context.Context, job baseRaidJob) {
	matches, err := p.store.MatchRaid(ctx, p.guildRowID, p.serverID, job.raiderID, job.mapX, job.mapZ)
	if err != nil {
		slog.Warn("component=base_raid_alarm", "msg", "match failed", "server_id", p.serverID, "err", err.Error())
		return
	}
	for _, m := range matches {
		alertID, err := p.store.RecordAlert(ctx, m, job.event)
		if err != nil {
			slog.Warn("component=base_raid_alarm", "msg", "record failed", "server_id", p.serverID, "base_id", m.BaseID, "err", err.Error())
			continue
		}
		if alertID == 0 {
			continue // this base already alarmed within its cooldown
		}
		delivery := p.deliver(m, job.event)
		if err := p.store.MarkDelivery(ctx, alertID, delivery); err != nil {
			slog.Warn("component=base_raid_alarm", "msg", "mark delivery failed", "alert_id", alertID, "err", err.Error())
		}
		if p.faction != nil {
			msg := FactionRaidMessage(BaseRaidAlarmMessage(m.BaseName, p.server(), job.event, time.Now()), m.BaseName)
			p.faction.share(ctx, factionShare{route: baseRaidRoute, source: repository.FactionShareRaidAlarm, installationID: m.InstallationID,
				guildID: m.GuildID, serverID: m.ServerID, base: m.BaseID, ownerPlayerID: m.OwnerPlayerID}, msg)
		}
		slog.Info("component=base_raid_alarm", "event", "alarm", "server_id", p.serverID, "base_id", m.BaseID,
			"alert_id", alertID, "delivery", delivery)
	}
}

func (p *BaseRaidAlarmPublisher) server() string {
	if p.serverName != nil {
		return p.serverName(p.serverID)
	}
	return ""
}

func (p *BaseRaidAlarmPublisher) deliver(m repository.BaseRaidMatch, ev repository.BaseRaidEvent) string {
	if m.OwnerDiscordUserID == "" || p.dm == nil {
		return repository.BaseRaidDeliveryOwnerNotLinked
	}
	msg := BaseRaidAlarmMessage(m.BaseName, p.server(), ev, time.Now())
	err := deliver(baseRaidRoute, "", func() error {
		ch, err := p.dm.UserChannelCreate(m.OwnerDiscordUserID)
		if err != nil {
			return err
		}
		_, err = p.dm.ChannelMessageSendComplex(ch.ID, msg)
		return err
	})
	if err != nil {
		slog.Warn("component=base_raid_alarm", "msg", "dm failed", "server_id", p.serverID, "base_id", m.BaseID, "err", err.Error())
		return repository.BaseRaidDeliveryFailed
	}
	return repository.BaseRaidDeliverySent
}

// BaseRaidAlarmMessage is the DM the base owner gets. Player-written text is
// neutralised and mentions are disabled.
func BaseRaidAlarmMessage(baseName, serverName string, ev repository.BaseRaidEvent, at time.Time) *discordgo.MessageSend {
	what := "Took apart **" + caseSafeText(ev.Part, 64) + "**"
	if t := caseSafeText(ev.Target, 64); t != "" {
		what += " from " + t
	}
	if tool := caseSafeText(ev.Tool, 64); tool != "" {
		what += " with a " + tool
	}
	when := fmt.Sprintf("<t:%d:R>", at.Unix())
	if tod := caseSafeText(ev.TimeOfDay, 16); tod != "" {
		when = tod + " server time (" + when + ")"
	}
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® BASE RAID ALARM"},
		Title:       "🚨 Someone is breaking into " + caseFallback(caseSafeText(baseName, 80), "your base"),
		Color:       presentation.ErrorRed,
		Description: "A player who isn't you, your faction or on your friend list is taking your base apart.",
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Player", Value: caseFallback(caseSafeText(ev.RaiderName, 64), "Unknown player"), Inline: true},
			{Name: "🖥️ Server", Value: caseFallback(caseSafeText(serverName, 100), "Your server"), Inline: true},
			{Name: "🕒 When", Value: when, Inline: true},
			{Name: "🔨 What happened", Value: what, Inline: false},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "You'll get at most one alarm per base every few minutes"},
	}
	presentation.StampEmbed(embed, at)
	return &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{
			Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{},
		},
	}
}
