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

func TestPlayerTimeline(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	w.a.PlayerTimeline = repository.NewPlayerTimelineRepository(pool)
	now := time.Now().UTC().Truncate(time.Second)
	me, foe := w.player("Me"), w.player("Foe")

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at) VALUES($1,$2,$3,'Me',1,2,'CONNECT',$4),($1,$2,$3,'Me',1,2,'DISCONNECT',$5)`,
		w.guildID, w.serverID, me, now.Add(-5*time.Hour), now.Add(-time.Hour))
	w.plainKill(me, foe, now.Add(-4*time.Hour), 212.34)
	w.plainKill(foe, me, now.Add(-3*time.Hour), 15)
	exec(`INSERT INTO player_warnings(guild_id,player_id,reason,issued_at) VALUES($1,$2,'Combat logging',$3)`, w.guildID, me, now.Add(-2*time.Hour))
	exec(`UPDATE players SET display_name='MeRenamed' WHERE id=$1`, me)
	exec(`UPDATE players SET display_name='MeRenamed' WHERE id=$1`, me) // no change: no row

	rr := w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline"), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(me, 10)})
	if rr.Code != http.StatusOK {
		t.Fatalf("timeline: %d %s", rr.Code, rr.Body.String())
	}
	body := decodeBody[struct {
		Name  string                     `json:"name"`
		Items []repository.TimelineEntry `json:"items"`
	}](t, rr)
	kinds := []string{}
	for _, e := range body.Items {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"NAME_CHANGE", "DISCONNECT", "WARNING", "DEATH", "KILL", "CONNECT"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	kill := body.Items[4]
	if kill.Other != "Foe" || kill.Note != "212.3" || body.Items[0].Other != "Me" || body.Items[0].Value != "MeRenamed" || body.Name != "MeRenamed" {
		t.Fatalf("details: %+v", body.Items)
	}

	// Filter + paging.
	rr = w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline?kinds=kill,death&limit=1"), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(me, 10)})
	page := decodeBody[struct {
		Items      []repository.TimelineEntry `json:"items"`
		NextBefore *time.Time                 `json:"nextBefore"`
	}](t, rr)
	if len(page.Items) != 1 || page.Items[0].Kind != "DEATH" || page.NextBefore == nil {
		t.Fatalf("page 1: %+v", page)
	}
	rr = w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline?kinds=KILL,DEATH&limit=1&before="+page.NextBefore.Format(time.RFC3339Nano)), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(me, 10)})
	page = decodeBody[struct {
		Items      []repository.TimelineEntry `json:"items"`
		NextBefore *time.Time                 `json:"nextBefore"`
	}](t, rr)
	if len(page.Items) != 1 || page.Items[0].Kind != "KILL" {
		t.Fatalf("page 2: %+v", page)
	}
	if rr := w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline?kinds=NOPE"), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(me, 10)}); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", rr.Code)
	}

	// Another guild's player is not found; non-staff are refused.
	other := newStandoutWorld(t)
	stranger := other.player("Elsewhere")
	if rr := w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline"), w.f.OwnerDiscordID, nil, map[string]string{"playerID": strconv.FormatInt(stranger, 10)}); rr.Code != http.StatusNotFound {
		t.Fatalf("cross-guild player: %d", rr.Code)
	}
	if rr := w.call(w.a.handlePlayerTimeline, http.MethodGet, w.path("/players/x/timeline"), "stranger", nil, map[string]string{"playerID": strconv.FormatInt(me, 10)}); rr.Code == http.StatusOK {
		t.Fatal("non-staff read a timeline")
	}
}
