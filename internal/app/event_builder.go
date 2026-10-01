package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/discord"
	competitiveevents "github.com/yourname/dayz-killfeed/internal/events"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Event builder (docs/CLIENT_HUB_GROWTH.md): owners pick a template or build an
// event, schedule it, set prizes, and Champion announces it when it is
// scheduled and when it starts. Scoring, finishing, paying prizes and the
// completion card are the existing event engine's job.

func (a *App) registerEventBuilderRoutes(adminBase string) {
	h := a.HTTPServer.Handle
	h("GET "+adminBase+"/events", a.handleListEvents)
	h("GET "+adminBase+"/events/templates", a.handleEventTemplates)
	h("POST "+adminBase+"/events", a.handleCreateEvent)
	h("POST "+adminBase+"/events/{eventID}/cancel", a.handleCancelEvent)
	h("POST "+adminBase+"/events/{eventID}/end", a.handleEndEvent)
}

func (a *App) handleEventTemplates(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireCapability(w, r, permissions.CapEventsView); !ok {
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": competitiveevents.Templates()})
}

func (a *App) handleListEvents(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapEventsView)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	if a.Events == nil {
		writeSaaSError(w, codeInternalError, "events unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	items, err := a.Events.ListOwnerEvents(ctx, ac.scope.GuildID, 50)
	if err != nil {
		slog.Warn("component=saas_api", "event", "events_list_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load events")
		return
	}
	writeSaaSJSON(w, http.StatusOK, map[string]any{"items": items})
}

type createEventRequest struct {
	TemplateKey   string          `json:"templateKey"`
	Type          string          `json:"type"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Config        json.RawMessage `json:"config"`
	StartsAt      *time.Time      `json:"startsAt"`      // null = start now
	DurationHours float64         `json:"durationHours"` // used when endsAt is absent
	EndsAt        *time.Time      `json:"endsAt"`
	Prizes        *[3]int         `json:"prizes"`
	Announce      *bool           `json:"announce"` // default true
}

func (a *App) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapEventsManage)
	if !ok {
		return
	}
	req, ok := decodeJSONBody[createEventRequest](w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	// A template fills anything the request leaves out.
	if tpl, found := competitiveevents.TemplateByKey(req.TemplateKey); found {
		if req.Type == "" {
			req.Type = tpl.Type
		}
		if req.Name == "" {
			req.Name = tpl.Name
		}
		if req.Description == "" {
			req.Description = tpl.Description
		}
		if len(req.Config) == 0 {
			req.Config = tpl.Config
		}
		if req.DurationHours == 0 && req.EndsAt == nil {
			req.DurationHours = float64(tpl.DurationHours)
		}
		if req.Prizes == nil {
			req.Prizes = &[3]int{tpl.FirstPoints, tpl.SecondPoints, tpl.ThirdPoints}
		}
		req.TemplateKey = tpl.Key
	} else if req.TemplateKey != "" {
		writeSaaSError(w, codeInvalidRequest, "unknown event template")
		return
	}
	start := now
	if req.StartsAt != nil && req.StartsAt.After(now) {
		start = req.StartsAt.UTC()
	}
	var end time.Time
	switch {
	case req.EndsAt != nil:
		end = req.EndsAt.UTC()
	case req.DurationHours > 0 && req.DurationHours <= 14*24:
		end = start.Add(time.Duration(req.DurationHours * float64(time.Hour)))
	default:
		writeSaaSError(w, codeInvalidRequest, "set an end time or a duration between 15 minutes and 14 days")
		return
	}
	prizes := [3]int{}
	if req.Prizes != nil {
		prizes = *req.Prizes
	}
	plan, err := competitiveevents.ValidatePlan(req.Type, req.Name, req.Description, req.Config, req.StartsAt, end, prizes, now)
	if err != nil {
		writeSaaSError(w, codeInvalidRequest, err.Error())
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	if a.Events == nil {
		writeSaaSError(w, codeInternalError, "events unavailable")
		return
	}
	cfg, _ := json.Marshal(plan.Config)
	status := competitiveevents.StatusScheduled
	if plan.Immediate {
		status = competitiveevents.StatusActive
	}
	startsAt, endsAt := plan.StartsAt, plan.EndsAt
	announce := req.Announce == nil || *req.Announce
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	id, err := a.Events.CreateOwnerEvent(ctx, ac.scope.GuildID, repository.CompetitiveEvent{Type: plan.Type, Name: plan.Name, Description: plan.Description,
		Status: status, StartsAt: &startsAt, EndsAt: &endsAt, Config: cfg}, req.TemplateKey, announce, [3]int{plan.FirstPoints, plan.SecondPoints, plan.ThirdPoints}, ac.user.DiscordUserID)
	if err != nil {
		slog.Warn("component=saas_api", "event", "event_create_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not create the event")
		return
	}
	a.recordAudit(ctx, ac, "EVENT_CREATE", "event:"+strconv.FormatInt(id, 10), plan.Name, "success", nil, map[string]any{"type": plan.Type, "status": status, "startsAt": startsAt, "endsAt": endsAt})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"id": id, "status": status, "startsAt": startsAt, "endsAt": endsAt})
}

func (a *App) handleCancelEvent(w http.ResponseWriter, r *http.Request) {
	a.changeEvent(w, r, "cancel")
}
func (a *App) handleEndEvent(w http.ResponseWriter, r *http.Request) { a.changeEvent(w, r, "end") }

func (a *App) changeEvent(w http.ResponseWriter, r *http.Request, action string) {
	ac, ok := a.requireCapability(w, r, permissions.CapEventsManage)
	if !ok {
		return
	}
	eventID, ok := pathInt64(w, r, "eventID")
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	if a.Events == nil {
		writeSaaSError(w, codeInternalError, "events unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	ev, err := a.Events.GetEvent(ctx, ac.scope.GuildID, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeSaaSError(w, codeNotFound, "event not found")
		return
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not load the event")
		return
	}
	now := time.Now().UTC()
	switch action {
	case "cancel":
		if ev.Status != competitiveevents.StatusScheduled && ev.Status != competitiveevents.StatusDraft && ev.Status != competitiveevents.StatusActive {
			writeSaaSError(w, codeInvalidRequest, "only an upcoming or running event can be cancelled")
			return
		}
		err = a.Events.CancelEvent(ctx, ac.scope.GuildID, eventID)
	case "end":
		if ev.Status != competitiveevents.StatusActive {
			writeSaaSError(w, codeInvalidRequest, "only a running event can be ended early")
			return
		}
		err = a.Events.EndEvent(ctx, ac.scope.GuildID, eventID, now)
		if err == nil {
			// EndEvent keeps a future ends_at; ending early must stop scoring now.
			_, err = a.DB.Pool.Exec(ctx, `UPDATE competitive_events SET ends_at=LEAST(COALESCE(ends_at,$3),$3) WHERE guild_id=$1 AND id=$2`, ac.scope.GuildID, eventID, now)
		}
	}
	if err != nil {
		writeSaaSError(w, codeInternalError, "could not change the event")
		return
	}
	a.recordAudit(ctx, ac, "EVENT_"+map[string]string{"cancel": "CANCEL", "end": "END"}[action], "event:"+strconv.FormatInt(eventID, 10), ev.Name, "success", nil, nil)
	writeSaaSJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publishEventAnnouncements posts each owner-built event's "upcoming" and
// "started" card once (scheduler tick). A failed post is retried next tick.
func (a *App) publishEventAnnouncements(ctx context.Context, guildID int64, now time.Time) {
	if a.Events == nil || a.CompletionPublisher == nil {
		return
	}
	pending, err := a.Events.PendingEventAnnouncements(ctx, guildID, 10)
	if err != nil {
		slog.Warn("component=events", "msg", "event announcements load failed", "err", err.Error())
		return
	}
	for _, p := range pending {
		card := discord.EventAnnouncementCard{Kind: p.Kind, Name: p.Event.Name, Description: p.Event.Description, Type: p.Event.Type, Config: p.Event.Config,
			StartsAt: p.Event.StartsAt, EndsAt: p.Event.EndsAt, Prizes: [3]int{p.FirstPoints, p.SecondPoints, p.ThirdPoints}}
		if err := a.CompletionPublisher.Announce(ctx, discord.BuildEventAnnouncementEmbed(card)); err != nil {
			slog.Warn("component=events", "msg", "event announcement failed", "event_id", p.Event.ID, "err", err.Error())
			continue
		}
		if err := a.Events.MarkEventAnnounced(ctx, guildID, p.Event.ID, p.Kind, now); err != nil {
			slog.Warn("component=events", "msg", "event announcement stamp failed", "event_id", p.Event.ID, "err", err.Error())
		}
		if p.Kind == "STARTED" {
			// A started card supersedes a never-posted upcoming card.
			_ = a.Events.MarkEventAnnounced(ctx, guildID, p.Event.ID, "UPCOMING", now)
		}
	}
}
