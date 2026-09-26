package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

const routeKeyAdminAlerts = "ADMIN_ALERTS"

// AlertSeverity ranks an operational alert.
type AlertSeverity string

const (
	AlertWarning  AlertSeverity = "WARNING"
	AlertCritical AlertSeverity = "CRITICAL"
	AlertResolved AlertSeverity = "RESOLVED"
)

// Alert kinds the publisher emits today. Each one has a real source; kinds
// for systems that do not exist yet (autostart, file sync, payments, route
// permissions) are deliberately absent.
const (
	AlertKindADMStale        = "ADM_STALE"
	AlertKindNitradoFailure  = "NITRADO_API_FAILURE"
	AlertKindZoneIntrusion   = "ZONE_INTRUSION"
	AlertKindUAVIntrusion    = "UAV_INTRUSION"
	AlertKindBaseRadar       = "BASE_RADAR_INTRUSION"
	AlertKindZoneBanViolated = "ZONE_BAN_VIOLATION"
	AlertKindCaseWatchDigest = "CASE_WATCH_DIGEST"
)

// CaseWatchScope is constructed by the authenticated server, not the browser.
type CaseWatchScope struct {
	OrganizationID, InstallationID, GameServerID int64
}

// AdminAlert is one operational condition for one server.
type AdminAlert struct {
	GuildRowID, ServerID int64
	Kind                 string
	Severity             AlertSeverity
	Headline             string // e.g. "ADM STALE"
	Detail               string
	Fields               [][2]string // name, value - already safe text
	At                   time.Time
	// SkipChannel suppresses the send when ADMIN_ALERTS resolves to it (the
	// source already posted there itself).
	SkipChannel string
	// A paid digest is always checked again when the queue drains.
	CaseWatch *CaseWatchScope
}

const (
	adminAlertQueueSize = 64
	// admStaleAfter matches operations.ADMMonitor.ActiveStallAfter: the log has
	// not changed for this long while players are online.
	admStaleAfter = 5 * time.Minute
	// nitradoFailureAlertAfter consecutive failed ADM downloads raise an alert
	// (each single failure is already an ADM monitor diagnostic).
	nitradoFailureAlertAfter = 3
	adminAlertFooter         = "CHAMPION • STAFF INTELLIGENCE"
)

type serverAlertState struct {
	stale            bool
	downloadFailures int
	failing          bool
}

// AdminAlertPublisher is the shared operational alert publisher for the
// ADMIN_ALERTS route. Sources report conditions; the publisher raises an
// alert on the transition into a condition and a resolution on the way out,
// never once per poll. Sends run on one goroutine behind a bounded queue, so
// no source can be stalled by Discord. With no ADMIN_ALERTS route nothing is
// sent (no fallback channel).
type AdminAlertPublisher struct {
	sender      HitSender
	resolver    RouteResolver
	serverNames ServerNameFunc
	caseWatchAllowed func(context.Context, CaseWatchScope) (bool, error)
	queue       chan AdminAlert
	now         func() time.Time

	mu      sync.Mutex
	servers map[int64]*serverAlertState
	dropped int
}

func NewAdminAlertPublisher(sender HitSender, resolver RouteResolver) *AdminAlertPublisher {
	return &AdminAlertPublisher{sender: sender, resolver: resolver, queue: make(chan AdminAlert, adminAlertQueueSize), now: time.Now, servers: map[int64]*serverAlertState{}}
}

// SetCaseWatchAuthorizer is required for any paid message. A nil callback
// suppresses paid output without changing ordinary operational alerts.
func (p *AdminAlertPublisher) SetCaseWatchAuthorizer(f func(context.Context, CaseWatchScope) (bool, error)) {
	if p != nil {p.caseWatchAllowed=f}
}

// QueueCaseWatchDigest accepts a server-authored observation summary only.
// It never blocks the caller when the bounded Discord queue is full.
func (p *AdminAlertPublisher) QueueCaseWatchDigest(a AdminAlert, scope CaseWatchScope) bool {
	if p==nil || a.Kind!=AlertKindCaseWatchDigest || a.GuildRowID<=0 ||
		scope.OrganizationID<=0 || scope.InstallationID<=0 || scope.GameServerID<=0 ||
		a.ServerID!=scope.GameServerID {return false}
	a.CaseWatch=&scope
	if a.At.IsZero(){a.At=p.now()}
	select {case p.queue<-a:return true;default:return false}
}

// SetServerNames adds the server's name to every alert.
func (p *AdminAlertPublisher) SetServerNames(f ServerNameFunc) {
	if p != nil {
		p.serverNames = f
	}
}

func (p *AdminAlertPublisher) state(serverID int64) *serverAlertState {
	st := p.servers[serverID]
	if st == nil {
		st = &serverAlertState{}
		p.servers[serverID] = st
	}
	return st
}

// ObserveSnapshot evaluates one server's ADM snapshot for a stale log.
func (p *AdminAlertPublisher) ObserveSnapshot(guildRowID, serverID int64, snap killfeed.AdmSnapshot) {
	if p == nil {
		return
	}
	now := p.now()
	stale := snap.OnlineCount > 0 && !snap.LastLogChange.IsZero() && now.Sub(snap.LastLogChange) > admStaleAfter
	p.mu.Lock()
	st := p.state(serverID)
	changed := stale != st.stale
	st.stale = stale
	p.mu.Unlock()
	if !changed {
		return
	}
	if stale {
		p.Publish(AdminAlert{GuildRowID: guildRowID, ServerID: serverID, Kind: AlertKindADMStale, Severity: AlertWarning, Headline: "ADM STALE",
			Detail: "The ADM log has stopped updating while players are online.",
			Fields: [][2]string{{"Last Log Change", fmt.Sprintf("<t:%d:R>", snap.LastLogChange.Unix())}, {"Players Online", fmt.Sprintf("%d", snap.OnlineCount)}},
			At:     now})
		return
	}
	p.Publish(AdminAlert{GuildRowID: guildRowID, ServerID: serverID, Kind: AlertKindADMStale, Severity: AlertResolved, Headline: "ADM STALE",
		Detail: "ADM log updates resumed.", At: now})
}

// ObserveDownload counts consecutive failed ADM downloads for one server.
func (p *AdminAlertPublisher) ObserveDownload(guildRowID int64, report killfeed.DownloadReport) {
	if p == nil {
		return
	}
	var alert *AdminAlert
	p.mu.Lock()
	st := p.state(report.ServerID)
	switch report.Result {
	case "failure":
		st.downloadFailures++
		if st.downloadFailures >= nitradoFailureAlertAfter && !st.failing {
			st.failing = true
			fields := [][2]string{{"Consecutive Failures", fmt.Sprintf("%d", st.downloadFailures)}}
			if report.ErrorClass != "" {
				fields = append(fields, [2]string{"Error", safeMonitorText(report.ErrorClass)})
			}
			alert = &AdminAlert{Kind: AlertKindNitradoFailure, Severity: AlertCritical, Headline: "NITRADO API FAILURE",
				Detail: "ADM log downloads from Nitrado keep failing. Kills and events are delayed until they recover.", Fields: fields}
		}
	case "success", "success_no_new_events":
		st.downloadFailures = 0
		if st.failing {
			st.failing = false
			alert = &AdminAlert{Kind: AlertKindNitradoFailure, Severity: AlertResolved, Headline: "NITRADO API FAILURE",
				Detail: "ADM log downloads recovered."}
		}
	}
	p.mu.Unlock()
	if alert != nil {
		alert.GuildRowID, alert.ServerID, alert.At = guildRowID, report.ServerID, p.now()
		p.Publish(*alert)
	}
}

// Publish enqueues an alert without blocking; a full queue drops it.
func (p *AdminAlertPublisher) Publish(a AdminAlert) {
	if p == nil {
		return
	}
	if a.At.IsZero() {
		a.At = p.now()
	}
	select {
	case p.queue <- a:
	default:
		p.mu.Lock()
		p.dropped++
		n := p.dropped
		p.mu.Unlock()
		if n == 1 || n%50 == 0 { // bounded logging under a flood
			slog.Warn("component=admin_alerts", "event", "admin_alert_dropped", "kind", a.Kind, "server_id", a.ServerID, "dropped_total", n)
		}
	}
}

// Run sends queued alerts until ctx ends.
func (p *AdminAlertPublisher) Run(ctx context.Context) {
	if p == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-p.queue:
			p.send(ctx, a)
		}
	}
}

func (p *AdminAlertPublisher) send(ctx context.Context, a AdminAlert) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("component=admin_alerts", "msg", "send panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	if p.sender == nil || p.resolver == nil {
		return
	}
	if a.Kind==AlertKindCaseWatchDigest {
		if a.CaseWatch==nil || a.CaseWatch.GameServerID!=a.ServerID ||
			a.CaseWatch.OrganizationID<=0 || a.CaseWatch.InstallationID<=0 ||
			p.caseWatchAllowed==nil {return}
	} else if a.CaseWatch!=nil {return}
	channel, found, err := p.resolver.Resolve(ctx, a.GuildRowID, a.ServerID, routeKeyAdminAlerts)
	if err != nil {
		slog.Warn("component=admin_alerts", "event", "channel_route_fallback", "route_key", routeKeyAdminAlerts, "server_id", a.ServerID, "reason", "lookup_error", "err", err.Error())
		return
	}
	if !found || channel == "" || channel == a.SkipChannel {
		return
	}
	// Route lookup can block; authorize immediately before the actual send,
	// after routing, rather than trusting the earlier enqueue decision.
	if a.CaseWatch!=nil {
		allowed,checkErr:=p.caseWatchAllowed(ctx,*a.CaseWatch)
		if checkErr!=nil || !allowed {
			if checkErr!=nil {slog.Warn("component=case","event","watch_digest_access_failed","server_id",a.ServerID,"err",checkErr.Error())}
			return
		}
	}
	name := ""
	if p.serverNames != nil {
		name = p.serverNames(a.ServerID)
	}
	embed:=BuildAdminAlertEmbed(a,name)
	if a.Kind==AlertKindCaseWatchDigest {embed=BuildCaseWatchDigestEmbed(a,name)}
	if _, err := p.sender.ChannelMessageSendComplex(channel, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}); err != nil {
		slog.Warn("component=admin_alerts", "event", "admin_alert_send_failed", "kind", a.Kind, "server_id", a.ServerID, "err", err.Error())
	}
}

// BuildAdminAlertEmbed renders one alert: amber for warnings, red for
// critical conditions, green when resolved - always distinguishable from the
// steel ADM diagnostics and gold build activity sharing admin-logs.
func BuildAdminAlertEmbed(a AdminAlert, serverName string) *discordgo.MessageEmbed {
	title, color := "🚨 ADMIN ALERT", presentation.WarningAmber
	switch a.Severity {
	case AlertCritical:
		color = presentation.ErrorRed
	case AlertResolved:
		title, color = "✅ ALERT RESOLVED", presentation.SuccessGreen
	}
	embed := presentation.NewChampionEmbed(title, color)
	desc := "**" + a.Headline + "**"
	if strings.TrimSpace(a.Detail) != "" {
		desc += "\n" + a.Detail
	}
	embed.Description = desc
	if a.Severity != AlertResolved {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Severity", Value: string(a.Severity), Inline: true})
	}
	if strings.TrimSpace(serverName) != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Server", Value: presentation.SafeName(serverName, 60), Inline: true})
	}
	for _, f := range a.Fields {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: f[0], Value: f[1], Inline: true})
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: adminAlertFooter}
	presentation.StampEmbed(embed, a.At)
	return embed
}

// IntrusionAdminAlert maps a zone engine event to an admin alert. Exits and
// suppressed events are not alerts.
func IntrusionAdminAlert(ev killfeed.IntrusionEvent) (AdminAlert, bool) {
	if ev.Suppressed {
		return AdminAlert{}, false
	}
	var kind, headline string
	severity := AlertWarning
	switch ev.Kind {
	case killfeed.AlertZoneIntrusion:
		kind, headline = AlertKindZoneIntrusion, "ZONE INTRUSION"
	case killfeed.AlertUAVIntrusion:
		kind, headline = AlertKindUAVIntrusion, "UAV INTRUSION"
	case killfeed.AlertBaseRadarIntrusion:
		kind, headline = AlertKindBaseRadar, "BASE RADAR INTRUSION"
	case killfeed.AlertZoneBanViolation:
		kind, headline, severity = AlertKindZoneBanViolated, "ZONE BAN VIOLATION", AlertCritical
	default:
		return AdminAlert{}, false
	}
	skip := ""
	if ev.Zone.AlertChannelID != nil {
		skip = *ev.Zone.AlertChannelID
	}
	who, zone := "A player", "a protected zone"
	if strings.TrimSpace(ev.Gamertag) != "" {
		who = "**" + presentation.SafeName(ev.Gamertag, presentation.MaxRankNameRunes) + "**"
	}
	if strings.TrimSpace(ev.Zone.Name) != "" {
		zone = "zone **" + presentation.SafeName(ev.Zone.Name, 60) + "**"
	}
	var fields [][2]string
	if strings.TrimSpace(ev.Zone.ZoneType) != "" {
		fields = append(fields, [2]string{"Zone Type", presentation.SafeName(ev.Zone.ZoneType, 40)})
	}
	return AdminAlert{
		GuildRowID: ev.Zone.GuildID, ServerID: ev.Zone.ServerID, Kind: kind, Severity: severity, Headline: headline,
		Detail: who + " entered " + zone + ".", Fields: fields, At: ev.At, SkipChannel: skip,
	}, true
}

// BuildCaseWatchDigestEmbed is observational, never an accusation, score or
// statement that no cheating occurred in a period without recorded evidence.
func BuildCaseWatchDigestEmbed(a AdminAlert, serverName string) *discordgo.MessageEmbed {
	embed:=presentation.NewChampionEmbed("C.A.S.E. WATCH • OBSERVATION DIGEST",presentation.InfoSteel)
	embed.Description="Persisted ADM source observations from the selected server. Counts describe collected evidence only; they are not cheat alerts or gameplay verdicts."
	if strings.TrimSpace(serverName)!="" {
		embed.Fields=append(embed.Fields,&discordgo.MessageEmbedField{Name:"Server",Value:presentation.SafeName(serverName,60),Inline:true})
	}
	for _,field:=range a.Fields {
		if strings.TrimSpace(field[0])=="" || strings.TrimSpace(field[1])=="" {continue}
		embed.Fields=append(embed.Fields,&discordgo.MessageEmbedField{Name:presentation.SafeName(field[0],70),Value:presentation.SafeName(field[1],150),Inline:true})
	}
	embed.Footer=&discordgo.MessageEmbedFooter{Text:"C.A.S.E. • SOURCE OBSERVATION ONLY • NO ENFORCEMENT"}
	presentation.StampEmbed(embed,a.At)
	return embed
}
