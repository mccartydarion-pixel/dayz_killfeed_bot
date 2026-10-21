//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Retention dashboard over a real PostgreSQL (docs/RETENTION.md).

// activeOn records a day of presence for a player, as the presence rollup would.
func (w *clientAdminWorld) activeOn(player int64, day time.Time, seconds int64, sessions int) {
	w.t.Helper()
	if _, err := w.a.DB.Pool.Exec(context.Background(), `INSERT INTO player_daily_activity(guild_id, server_id, player_id, day, observed_seconds, sessions, first_seen_at, last_seen_at)
VALUES($1,$2,$3,$4::date,$5,$6,$4,$4)`, w.guildID, w.serverID, player, day, seconds, sessions); err != nil {
		w.t.Fatal(err)
	}
}

func (w *clientAdminWorld) purchase(player int64, status, product string, unit int64, qty int, at time.Time) {
	w.t.Helper()
	ctx := context.Background()
	var id int64
	if err := w.a.DB.Pool.QueryRow(ctx, `INSERT INTO shop_purchases(organization_id, installation_id, player_id, status, total_points, delivery_type, idempotency_key, created_at)
VALUES($1,$2,$3,$4,$5,'MANUAL',$6,$7) RETURNING id`, w.f.OrgID, w.f.InstallationID, player, status, unit*int64(qty), fmt.Sprintf("ret-key-%d", standoutSeq.Add(1)), at).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO shop_purchase_items(purchase_id, product_name, unit_price_points, quantity, line_total_points) VALUES($1,$2,$3,$4,$5)`,
		id, product, unit, qty, unit*int64(qty)); err != nil {
		w.t.Fatal(err)
	}
}

func TestRetentionDashboard(t *testing.T) {
	w := newStandoutWorld(t)
	ctx := context.Background()
	owner := w.f.OwnerDiscordID
	today := time.Now().UTC().Truncate(24 * time.Hour)
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7)) // Monday of the current UTC week

	loyal, oneOff, fresh := w.player("Loyal"), w.player("OneOff"), w.player("Fresh")
	// Loyal: first seen three weeks ago, back in each of the next two weeks and again today.
	w.activeOn(loyal, monday.AddDate(0, 0, -21), 3600, 2)
	w.activeOn(loyal, monday.AddDate(0, 0, -14), 1800, 1)
	w.activeOn(loyal, monday.AddDate(0, 0, -6), 600, 1)
	if !today.Equal(monday.AddDate(0, 0, -6)) {
		w.activeOn(loyal, today, 900, 1)
	}
	// OneOff: joined the same week as Loyal, came back the next week, then stopped.
	w.activeOn(oneOff, monday.AddDate(0, 0, -20), 7200, 3)
	w.activeOn(oneOff, monday.AddDate(0, 0, -13), 100, 1)
	// Fresh: first seen today.
	w.activeOn(fresh, today, 300, 1)

	// Concurrency: the same weekday and hour two weeks running, peaks of 3 and 5.
	slot := today.AddDate(0, 0, -1).Add(20 * time.Hour)
	for i, peak := range []int{5, 3} {
		if _, err := w.a.DB.Pool.Exec(ctx, `INSERT INTO server_hourly_activity(guild_id, server_id, hour, peak_players, player_seconds, samples) VALUES($1,$2,$3,$4,100,10)`,
			w.guildID, w.serverID, slot.AddDate(0, 0, -7*i), peak); err != nil {
			t.Fatal(err)
		}
	}
	w.purchase(loyal, "FULFILLED", "M4 Loadout", 500, 2, today.Add(-48*time.Hour))
	w.purchase(fresh, "PAID", "M4 Loadout", 500, 1, today.Add(-24*time.Hour))
	w.purchase(oneOff, "REFUNDED", "M4 Loadout", 500, 9, today.Add(-24*time.Hour))
	w.purchase(loyal, "FULFILLED", "Base Kit", 2000, 1, today.Add(-24*time.Hour))
	w.purchase(loyal, "FULFILLED", "Ancient Purchase", 9999, 1, today.AddDate(0, 0, -200))

	rr := w.call(w.a.handleRetention, http.MethodGet, w.path("/retention")+"?days=30", owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("retention: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[retentionResponseDTO](t, rr)

	s := got.Summary
	if s.ActiveToday != 2 || s.Active7d != 2 || s.Active30d != 3 || s.New7d != 1 || s.New30d != 3 || s.TrackedPlayers != 3 {
		t.Fatalf("summary = %+v", s)
	}
	if s.Stickiness == nil || *s.Stickiness < 0.66 || *s.Stickiness > 0.67 {
		t.Fatalf("stickiness = %v, want 2/3", s.Stickiness)
	}
	if s.CollectingSince == nil || *s.CollectingSince != monday.AddDate(0, 0, -21).Format("2006-01-02") || s.AverageSessionSeconds == nil {
		t.Fatalf("summary = %+v", s)
	}

	if len(got.Daily) != 30 || got.Daily[29].Day != today.Format("2006-01-02") {
		t.Fatalf("daily spans %d days ending %s", len(got.Daily), got.Daily[len(got.Daily)-1].Day)
	}
	last := got.Daily[29]
	if last.Active != 2 || last.New != 1 || last.Returning != 1 {
		t.Fatalf("today = %+v", last)
	}
	byDay := map[string]retentionDayDTO{}
	for _, d := range got.Daily {
		byDay[d.Day] = d
	}
	firstDay := byDay[monday.AddDate(0, 0, -21).Format("2006-01-02")]
	if firstDay.Active != 1 || firstDay.New != 1 || firstDay.ObservedSeconds != 3600 || firstDay.Sessions != 2 {
		t.Fatalf("first day = %+v", firstDay)
	}
	if quiet := byDay[monday.AddDate(0, 0, -19).Format("2006-01-02")]; quiet.Day == "" || quiet.Active != 0 {
		t.Fatalf("a day nobody played is missing or non-zero: %+v", quiet)
	}

	// Cohorts: the week three weeks back holds Loyal and OneOff; both returned in week 1, only Loyal
	// in week 2, and week 3 (the current week) is still in progress, so it is null - not zero.
	var cohort *retentionCohortDTO
	for i := range got.Cohorts {
		if got.Cohorts[i].WeekStart == monday.AddDate(0, 0, -21).Format("2006-01-02") {
			cohort = &got.Cohorts[i]
		}
	}
	if len(got.Cohorts) != 8 || cohort == nil {
		t.Fatalf("cohorts = %+v", got.Cohorts)
	}
	if cohort.Size != 2 || cohort.Retained[0] == nil || *cohort.Retained[0] != 2 || cohort.Retained[1] == nil || *cohort.Retained[1] != 1 || cohort.Retained[2] != nil || cohort.Retained[3] != nil {
		t.Fatalf("cohort = size %d retained %v %v %v %v", cohort.Size, cohort.Retained[0], cohort.Retained[1], cohort.Retained[2], cohort.Retained[3])
	}
	current := got.Cohorts[7]
	if current.WeekStart != monday.Format("2006-01-02") || current.Retained[0] != nil {
		t.Fatalf("current cohort = %+v", current)
	}

	if len(got.PeakHours) != 1 || got.PeakHours[0].Weekday != int(slot.Weekday()) || got.PeakHours[0].Hour != 20 ||
		got.PeakHours[0].AveragePeak != 4 || got.PeakHours[0].MaxPeak != 5 || got.PeakHours[0].Samples != 2 {
		t.Fatalf("peak hours = %+v", got.PeakHours)
	}

	// Shop: refunded and out-of-window purchases are excluded; best earner first.
	if len(got.Shop) != 2 || got.Shop[0].ProductName != "Base Kit" || got.Shop[0].Points != 2000 ||
		got.Shop[1].ProductName != "M4 Loadout" || got.Shop[1].Points != 1500 || got.Shop[1].Units != 3 || got.Shop[1].Purchases != 2 || got.Shop[1].Buyers != 2 {
		t.Fatalf("shop = %+v", got.Shop)
	}

	// The time zone shifts the hour; an unknown zone and out-of-range windows are caller errors.
	rr = w.call(w.a.handleRetention, http.MethodGet, w.path("/retention")+"?tz=Asia/Tokyo", owner, nil, nil)
	tokyo := decodeBody[retentionResponseDTO](t, rr)
	if rr.Code != http.StatusOK || len(tokyo.PeakHours) != 1 || tokyo.PeakHours[0].Hour != 5 || tokyo.PeakHours[0].Weekday != int(slot.AddDate(0, 0, 1).Weekday()) {
		t.Fatalf("tokyo peak hours: %d %+v", rr.Code, tokyo.PeakHours)
	}
	for _, q := range []string{"?tz=Not/AZone", "?tz=UTC%27--", "?days=3", "?days=abc"} {
		if rr := w.call(w.a.handleRetention, http.MethodGet, w.path("/retention")+q, owner, nil, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", q, rr.Code, rr.Body.String())
		}
	}

	// Lapsed: OneOff stopped 13+ days ago; Loyal and Fresh were on today.
	rr = w.call(w.a.handleRetentionLapsed, http.MethodGet, w.path("/retention/lapsed")+"?minDays=7&maxDays=60", owner, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("lapsed: %d %s", rr.Code, rr.Body.String())
	}
	lapsed := decodeBody[struct {
		Items []lapsedPlayerDTO `json:"items"`
	}](t, rr)
	if len(lapsed.Items) != 1 || lapsed.Items[0].PlayerName != "OneOff" || lapsed.Items[0].ActiveDays != 2 || lapsed.Items[0].ObservedSeconds != 7300 ||
		lapsed.Items[0].Linked || lapsed.Items[0].DaysSinceSeen != int(today.Sub(monday.AddDate(0, 0, -13)).Hours()/24) {
		t.Fatalf("lapsed = %+v", lapsed.Items)
	}
	if rr := w.call(w.a.handleRetentionLapsed, http.MethodGet, w.path("/retention/lapsed")+"?minDays=30&maxDays=7", owner, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("inverted window accepted: %d", rr.Code)
	}

	// Permissions: a Moderator may not read the dashboard; an Administrator may.
	mod := syncUser(t, w.a, fmt.Sprintf("ret-mod-%d", standoutSeq.Add(1)), "Mod")
	admin := syncUser(t, w.a, fmt.Sprintf("ret-admin-%d", standoutSeq.Add(1)), "Admin")
	w.mapRole(mod.DiscordUserID, "ret-role-mod", "MODERATOR")
	w.mapRole(admin.DiscordUserID, "ret-role-admin", "ADMINISTRATOR")
	for _, h := range []http.HandlerFunc{w.a.handleRetention, w.a.handleRetentionLapsed} {
		if rr := w.call(h, http.MethodGet, w.path("/retention"), mod.DiscordUserID, nil, nil); rr.Code != http.StatusForbidden {
			t.Fatalf("moderator read retention: %d", rr.Code)
		}
		if rr := w.call(h, http.MethodGet, w.path("/retention"), admin.DiscordUserID, nil, nil); rr.Code != http.StatusOK {
			t.Fatalf("administrator could not read retention: %d %s", rr.Code, rr.Body.String())
		}
	}
}

func TestRetentionIsScopedToTheInstallationServer(t *testing.T) {
	a, b := newStandoutWorld(t), newStandoutWorld(t)
	today := time.Now().UTC().Truncate(24 * time.Hour)
	b.activeOn(b.player("Elsewhere"), today, 100, 1)
	rr := a.call(a.a.handleRetention, http.MethodGet, a.path("/retention"), a.f.OwnerDiscordID, nil, nil)
	got := decodeBody[retentionResponseDTO](t, rr)
	if rr.Code != http.StatusOK || got.Summary.TrackedPlayers != 0 || got.Summary.ActiveToday != 0 || got.Summary.Stickiness != nil || got.Summary.CollectingSince != nil {
		t.Fatalf("another tenant's activity leaked: %d %+v", rr.Code, got.Summary)
	}
}
