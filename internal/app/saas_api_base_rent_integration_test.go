//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestBaseRentOwnerSettings(t *testing.T) {
	w := newClientAdminWorld(t)
	admin := zoneActor(t, w, "rent-admin")
	w.mapRole(admin, "rent-admin-role", "ADMINISTRATOR")
	path := w.path("/case/base-rent")
	if rr := w.call(w.a.handleGetBaseRent, http.MethodGet, path, admin, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin read: %d", rr.Code)
	}
	type view struct {
		Settings  repository.BaseRentSettings `json:"settings"`
		GraceDays int                         `json:"graceDays"`
	}
	got := decodeBody[view](t, w.call(w.a.handleGetBaseRent, http.MethodGet, path, w.f.OwnerDiscordID, nil, nil))
	if got.Settings.Enabled || got.Settings.Configured || got.GraceDays != 3 {
		t.Fatalf("default: %+v", got)
	}
	for _, bad := range []map[string]any{{"enabled": true, "pricePoints": 0, "periodDays": 7}, {"enabled": true, "pricePoints": 100, "periodDays": 31}, {"enabled": true, "pricePoints": 100, "periodDays": 7, "grace": 9}} {
		if rr := w.call(w.a.handleSetBaseRent, http.MethodPut, path, w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad %v accepted: %d", bad, rr.Code)
		}
	}
	if rr := w.call(w.a.handleSetBaseRent, http.MethodPut, path, admin, map[string]any{"enabled": true, "pricePoints": 100, "periodDays": 7}, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin set: %d", rr.Code)
	}
	on := w.call(w.a.handleSetBaseRent, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"enabled": true, "pricePoints": 250, "periodDays": 14}, nil)
	if s := decodeBody[view](t, on).Settings; on.Code != http.StatusOK || !s.Enabled || s.PricePoints != 250 || s.PeriodDays != 14 {
		t.Fatalf("enable: %d %+v", on.Code, s)
	}
}

func TestBaseRentPlayerPaysAndReminders(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	renterDiscord := w.players[0]
	renter := w.linkPlayer(w.a1, renterDiscord, "Renter")
	guild, server := w.gameContext(w.a1)
	s := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	reqs := repository.NewCaseBaseRequestRepository(pool)
	q, err := reqs.Create(ctx, repository.BaseRequestScope(s), repository.BaseRequestInput{PlayerID: renter, Name: "Hut", CenterX: 1, CenterZ: 1, Radius: 30, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := reqs.Approve(ctx, repository.BaseRequestScope(s), q.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *d.Request.BaseID
	path := fmt.Sprintf("/api/saas/organizations/%d/installations/%d/security-marketplace/base-rent", w.a1.OrgID, w.a1.InstallationID)
	if v := w.expect(w.do(http.MethodGet, path, renterDiscord, nil), http.StatusOK, "rent off").JSON(t); v["enabled"] != false || len(v["bases"].([]any)) != 0 {
		t.Fatalf("rent off: %+v", v)
	}
	pay := func(key string) map[string]any { return map[string]any{"baseId": baseID, "idempotencyKey": key} }
	w.expect(w.do(http.MethodPost, path, renterDiscord, pay("rent-pay-01")), http.StatusConflict, "rent off")
	if _, err := repository.NewBaseRentRepository(pool).SetSettings(ctx, s, true, 300, 7, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.NewBaseRentRepository(pool).SetSettings(context.Background(), s, false, 300, 7, nil)
	})
	v := w.expect(w.do(http.MethodGet, path, renterDiscord, nil), http.StatusOK, "rent on").JSON(t)
	if v["enabled"] != true || v["pricePoints"].(float64) != 300 || len(v["bases"].([]any)) != 1 {
		t.Fatalf("rent on: %+v", v)
	}
	w.expect(w.do(http.MethodPost, path, renterDiscord, pay("rent-pay-02")), http.StatusConflict, "no points")
	if _, err := repository.NewEconomyRepository(pool).Credit(ctx, repository.LedgerParams{GuildID: guild, PlayerID: renter,
		Type: repository.TxAdminCredit, Amount: 500, ReferenceID: "seed-rent", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	w.expect(w.do(http.MethodPost, path, renterDiscord, map[string]any{"baseId": baseID, "idempotencyKey": "rent-pay-03", "price": 1}), http.StatusBadRequest, "unknown field")
	paid := w.expect(w.do(http.MethodPost, path, renterDiscord, pay("rent-pay-04")), http.StatusCreated, "pay").JSON(t)
	if paid["remainingBalance"].(float64) != 200 {
		t.Fatalf("pay: %+v", paid)
	}
	w.expect(w.do(http.MethodPost, path, renterDiscord, pay("rent-pay-04")), http.StatusOK, "replay")
	if h := w.expect(w.do(http.MethodGet, path, renterDiscord, nil), http.StatusOK, "history").JSON(t)["history"].([]any); len(h) != 1 || h[0].(map[string]any)["playerName"] != "Renter" {
		t.Fatalf("history: %+v", h)
	}
	w.expect(w.do(http.MethodPost, path, w.players[1], pay("rent-pay-05")), http.StatusConflict, "unlinked player")

	// Reminders: the base becomes paused; one DM, never repeated.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_payments SET starts_at=NOW()-INTERVAL '11 days',ends_at=NOW()-INTERVAL '4 days' WHERE base_id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	// Rent and the base are older than the payment too.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_settings SET enabled_since=NOW()-INTERVAL '12 days' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_registered_bases SET created_at=NOW()-INTERVAL '12 days' WHERE id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	var sent []string
	send := func(userID string, _ *discordgo.MessageSend) error { sent = append(sent, userID); return nil }
	w.a.sendBaseRentReminders(ctx, send)
	w.a.sendBaseRentReminders(ctx, send)
	mine := 0
	for _, id := range sent {
		if id == renterDiscord {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("reminders (once for this renter): %v", sent)
	}
}

func TestBaseRentGiftOwnerOnly(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	admin := zoneActor(t, w, "rentgift-admin")
	w.mapRole(admin, "rentgift-admin-role", "ADMINISTRATOR")
	renter := w.seedPlayer("Gifted")
	s := repository.SecurityScope{InstallationID: w.f.InstallationID, GuildID: w.guildID, ServerID: w.serverID}
	reqs := repository.NewCaseBaseRequestRepository(pool)
	q, err := reqs.Create(ctx, repository.BaseRequestScope(s), repository.BaseRequestInput{PlayerID: renter, Name: "Gift Hut", CenterX: 1, CenterZ: 1, Radius: 30, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := reqs.Approve(ctx, repository.BaseRequestScope(s), q.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *d.Request.BaseID
	path := w.path("/case/base-rent/gift")
	body := map[string]any{"baseId": baseID, "days": 10, "note": "Sorry for the downtime", "idempotencyKey": "rent-gift-0001"}
	if rr := w.call(w.a.handleGiftBaseRent, http.MethodPost, path, w.f.OwnerDiscordID, body, nil); rr.Code != http.StatusConflict {
		t.Fatalf("rent off: base doesn't pay rent: %d", rr.Code)
	}
	if _, err := repository.NewBaseRentRepository(pool).SetSettings(ctx, s, true, 300, 7, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = repository.NewBaseRentRepository(pool).SetSettings(context.Background(), s, false, 300, 7, nil)
	})
	if rr := w.call(w.a.handleGiftBaseRent, http.MethodPost, path, admin, body, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin gift: %d", rr.Code)
	}
	for _, bad := range []map[string]any{
		{"baseId": baseID, "days": 0, "idempotencyKey": "rent-gift-0002"},
		{"baseId": baseID, "days": 91, "idempotencyKey": "rent-gift-0002"},
		{"baseId": baseID, "days": 5, "idempotencyKey": "x"},
		{"baseId": baseID, "days": 5, "idempotencyKey": "rent-gift-0002", "price": 3},
	} {
		if rr := w.call(w.a.handleGiftBaseRent, http.MethodPost, path, w.f.OwnerDiscordID, bad, nil); rr.Code != http.StatusBadRequest {
			t.Fatalf("bad gift %v: %d", bad, rr.Code)
		}
	}
	type res struct {
		Gift      repository.BaseRentPayment `json:"gift"`
		Duplicate bool                       `json:"duplicate"`
	}
	rr := w.call(w.a.handleGiftBaseRent, http.MethodPost, path, w.f.OwnerDiscordID, body, nil)
	got := decodeBody[res](t, rr)
	if rr.Code != http.StatusCreated || !got.Gift.Gift || got.Gift.PricePoints != 0 || got.Gift.PeriodDays != 10 || got.Gift.PlayerID != renter {
		t.Fatalf("gift: %d %+v", rr.Code, got)
	}
	if again := w.call(w.a.handleGiftBaseRent, http.MethodPost, path, w.f.OwnerDiscordID, body, nil); again.Code != http.StatusOK || !decodeBody[res](t, again).Duplicate {
		t.Fatalf("replay: %d", again.Code)
	}
	bases, err := repository.NewBaseRentRepository(pool).PlayerBases(ctx, s, renter)
	if err != nil || len(bases) != 1 || bases[0].Overdue || bases[0].DueAt.Before(time.Now().Add(9*24*time.Hour)) {
		t.Fatalf("gifted rent must count: %+v %v", bases, err)
	}
	view := decodeBody[struct {
		Payments []repository.BaseRentPayment `json:"payments"`
	}](t, w.call(w.a.handleGetBaseRent, http.MethodGet, w.path("/case/base-rent"), w.f.OwnerDiscordID, nil, nil))
	if len(view.Payments) != 1 || !view.Payments[0].Gift || view.Payments[0].Note != "Sorry for the downtime" {
		t.Fatalf("owner list: %+v", view.Payments)
	}
	sum, err := repository.NewBaseRentRepository(pool).Summary(ctx, s, time.Now().AddDate(0, 0, -7))
	if err != nil || sum.Payments != 0 || sum.Points != 0 || sum.GiftedDays != 10 || sum.PaidUp != 1 {
		t.Fatalf("gifts aren't income: %+v %v", sum, err)
	}
	// A player can't use a gift key to replay the owner's gift as their own payment.
	if _, err := repository.NewBaseRentRepository(pool).Pay(ctx, s, renter, baseID, "gift-rent-gift-0001"); err == nil {
		t.Fatal("gift- key accepted for a payment")
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='BASE_RENT_GIFTED'`, w.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audits (once): %d %v", audits, err)
	}
}

type fakeRentAlerts struct{ alerts []discord.AdminAlert }

func (f *fakeRentAlerts) Publish(a discord.AdminAlert) { f.alerts = append(f.alerts, a) }

func TestBaseRentPauseDigest(t *testing.T) {
	w := newFactionWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	renter := w.linkPlayer(w.a1, w.players[0], "Late Payer")
	guild, server := w.gameContext(w.a1)
	s := repository.SecurityScope{InstallationID: w.a1.InstallationID, GuildID: guild, ServerID: server}
	reqs := repository.NewCaseBaseRequestRepository(pool)
	q, err := reqs.Create(ctx, repository.BaseRequestScope(s), repository.BaseRequestInput{PlayerID: renter, Name: "Late Hut", CenterX: 1, CenterZ: 1, Radius: 30, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := reqs.Approve(ctx, repository.BaseRequestScope(s), q.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *d.Request.BaseID
	rent := repository.NewBaseRentRepository(pool)
	if _, err := rent.SetSettings(ctx, s, true, 300, 7, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rent.SetSettings(context.Background(), s, false, 300, 7, nil) })
	mine := func(f *fakeRentAlerts) []discord.AdminAlert {
		var out []discord.AdminAlert
		for _, a := range f.alerts {
			if a.ServerID == server && a.GuildRowID == guild {
				out = append(out, a)
			}
		}
		return out
	}
	none := &fakeRentAlerts{}
	w.a.sendRentPauseDigests(ctx, none)
	if len(mine(none)) != 0 {
		t.Fatalf("nothing paused yet: %+v", none.alerts)
	}
	// Paused 12 hours ago (rent due 3.5 days ago).
	if _, err := pool.Exec(ctx, `UPDATE base_rent_settings SET enabled_since=NOW()-INTERVAL '84 hours' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE case_registered_bases SET created_at=NOW()-INTERVAL '5 days' WHERE id=$1`, baseID); err != nil {
		t.Fatal(err)
	}
	first := &fakeRentAlerts{}
	w.a.sendRentPauseDigests(ctx, first)
	got := mine(first)
	if len(got) != 1 || got[0].Kind != discord.AlertKindRentPaused || got[0].Severity != discord.AlertInfo ||
		!strings.Contains(got[0].Detail, "Late Hut") || !strings.Contains(got[0].Detail, "Late Payer") || !strings.Contains(got[0].Detail, "1 base was") {
		t.Fatalf("digest: %+v", got)
	}
	again := &fakeRentAlerts{}
	w.a.sendRentPauseDigests(ctx, again)
	if len(mine(again)) != 0 {
		t.Fatal("at most once a day")
	}
	// A day later with nothing newly paused: no notice (the clock moves 25 hours back for both).
	if _, err := pool.Exec(ctx, `UPDATE base_rent_digests SET last_sent_at=NOW()-INTERVAL '25 hours' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE base_rent_settings SET enabled_since=NOW()-INTERVAL '109 hours' WHERE installation_id=$1`, s.InstallationID); err != nil {
		t.Fatal(err)
	}
	later := &fakeRentAlerts{}
	w.a.sendRentPauseDigests(ctx, later)
	if len(mine(later)) != 0 {
		t.Fatalf("already reported base listed again: %+v", mine(later))
	}
}

func TestBaseRentFreeOwnerOnly(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	pool := w.a.DB.Pool
	admin := zoneActor(t, w, "rentfree-admin")
	w.mapRole(admin, "rentfree-admin-role", "ADMINISTRATOR")
	player := w.seedPlayer("Staffer")
	s := repository.SecurityScope{InstallationID: w.f.InstallationID, GuildID: w.guildID, ServerID: w.serverID}
	reqs := repository.NewCaseBaseRequestRepository(pool)
	q, err := reqs.Create(ctx, repository.BaseRequestScope(s), repository.BaseRequestInput{PlayerID: player, Name: "Staff Hut", CenterX: 1, CenterZ: 1, Radius: 30, PositionSeenAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := reqs.Approve(ctx, repository.BaseRequestScope(s), q.ID, "chernarusplus", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseID := *d.Request.BaseID
	path := w.path("/case/base-rent/rent-free")
	body := map[string]any{"baseId": baseID, "free": true, "note": "Staff base"}
	if rr := w.call(w.a.handleSetBaseRentFree, http.MethodPut, path, admin, body, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("admin: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetBaseRentFree, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"baseId": baseID + 99999, "free": true}, nil); rr.Code != http.StatusConflict {
		t.Fatalf("unknown base: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetBaseRentFree, http.MethodPut, path, w.f.OwnerDiscordID, map[string]any{"baseId": baseID, "free": true, "x": 1}, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rr.Code)
	}
	if rr := w.call(w.a.handleSetBaseRentFree, http.MethodPut, path, w.f.OwnerDiscordID, body, nil); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	view := decodeBody[struct {
		RentFree []repository.RentFreeBase `json:"rentFree"`
	}](t, w.call(w.a.handleGetBaseRent, http.MethodGet, w.path("/case/base-rent"), w.f.OwnerDiscordID, nil, nil))
	if len(view.RentFree) != 1 || view.RentFree[0].Note != "Staff base" {
		t.Fatalf("owner view: %+v", view.RentFree)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit_log WHERE installation_id=$1 AND action='BASE_RENT_FREE_SAVED'`, w.f.InstallationID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audits: %d %v", audits, err)
	}
}
