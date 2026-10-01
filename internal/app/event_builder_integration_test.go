//go:build integration

package app

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestEventBuilder(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	owner := w.f.OwnerDiscordID

	if rr := w.call(w.a.handleEventTemplates, http.MethodGet, w.path("/events/templates"), owner, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("templates: %d", rr.Code)
	}
	// A template started now, with the template's prizes.
	rr := w.call(w.a.handleCreateEvent, http.MethodPost, w.path("/events"), owner, map[string]any{"templateKey": "KILL_FRENZY"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create from template: %d %s", rr.Code, rr.Body.String())
	}
	now := decodeBody[struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}](t, rr)
	if now.Status != "ACTIVE" {
		t.Fatalf("no start time means start now: %+v", now)
	}
	// A custom scheduled event three days out.
	start := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Minute)
	rr = w.call(w.a.handleCreateEvent, http.MethodPost, w.path("/events"), owner, map[string]any{
		"type": "LONGEST_KILL", "name": "Sunday Snipe", "config": map[string]any{"minimum_distance": 300},
		"startsAt": start, "durationHours": 3, "prizes": []int{5000, 0, 0}}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create scheduled: %d %s", rr.Code, rr.Body.String())
	}
	sched := decodeBody[struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}](t, rr)
	if sched.Status != "SCHEDULED" {
		t.Fatalf("future start schedules: %+v", sched)
	}
	for name, body := range map[string]map[string]any{
		"unknown template": {"templateKey": "NOPE"},
		"bounty type":      {"type": "BOUNTY", "name": "x", "durationHours": 1},
		"no duration":      {"type": "MOST_KILLS", "name": "x"},
		"bad config":       {"type": "MOST_KILLS", "name": "x", "durationHours": 1, "config": map[string]any{"oops": 1}},
	} {
		if rr := w.call(w.a.handleCreateEvent, http.MethodPost, w.path("/events"), owner, body, nil); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	if rr := w.call(w.a.handleCreateEvent, http.MethodPost, w.path("/events"), "stranger", map[string]any{"templateKey": "KILL_FRENZY"}, nil); rr.Code == http.StatusOK {
		t.Fatal("non-staff must not create events")
	}

	list := w.call(w.a.handleListEvents, http.MethodGet, w.path("/events"), owner, nil, nil)
	items := decodeBody[struct {
		Items []repository.OwnerEvent `json:"items"`
	}](t, list).Items
	if len(items) < 2 || items[0].Status != "ACTIVE" || items[0].Prizes != [3]int{1000, 500, 250} || !items[0].Announce || items[0].TemplateKey != "KILL_FRENZY" {
		t.Fatalf("list: %+v", items)
	}

	// Announcements: one STARTED (the running event) and one UPCOMING (the scheduled one), each posted once.
	pending, err := w.a.Events.PendingEventAnnouncements(ctx, w.guildID, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending announcements: %+v %v", pending, err)
	}
	kinds := map[string]bool{}
	for _, p := range pending {
		kinds[p.Kind] = true
		if err := w.a.Events.MarkEventAnnounced(ctx, w.guildID, p.Event.ID, p.Kind, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if !kinds["STARTED"] || !kinds["UPCOMING"] {
		t.Fatalf("kinds: %v", kinds)
	}
	if again, _ := w.a.Events.PendingEventAnnouncements(ctx, w.guildID, 10); len(again) != 0 {
		t.Fatal("each card posts once")
	}

	// End the running one early; cancel the scheduled one.
	if rr := w.call(w.a.handleEndEvent, http.MethodPost, w.path("/events/x/end"), owner, nil, map[string]string{"eventID": strconv.FormatInt(now.ID, 10)}); rr.Code != http.StatusOK {
		t.Fatalf("end: %d %s", rr.Code, rr.Body.String())
	}
	if ev, _ := w.a.Events.GetEvent(ctx, w.guildID, now.ID); ev.Status != "ENDED" || ev.EndsAt.After(time.Now()) {
		t.Fatalf("ended early: %+v", ev)
	}
	if rr := w.call(w.a.handleEndEvent, http.MethodPost, w.path("/events/x/end"), owner, nil, map[string]string{"eventID": strconv.FormatInt(sched.ID, 10)}); rr.Code != http.StatusBadRequest {
		t.Fatal("a scheduled event cannot be ended")
	}
	if rr := w.call(w.a.handleCancelEvent, http.MethodPost, w.path("/events/x/cancel"), owner, nil, map[string]string{"eventID": strconv.FormatInt(sched.ID, 10)}); rr.Code != http.StatusOK {
		t.Fatalf("cancel: %d", rr.Code)
	}
	if rr := w.call(w.a.handleCancelEvent, http.MethodPost, w.path("/events/x/cancel"), owner, nil, map[string]string{"eventID": "999999999"}); rr.Code != http.StatusNotFound {
		t.Fatalf("other guild / missing event: %d", rr.Code)
	}
}
