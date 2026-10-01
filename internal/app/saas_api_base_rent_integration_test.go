//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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
	w.expect(w.do(http.MethodPost, path, w.players[1], pay("rent-pay-05")), http.StatusConflict, "unlinked player")

	// Reminders: the base becomes paused; one DM, never repeated.
	if _, err := pool.Exec(ctx, `UPDATE base_rent_payments SET starts_at=NOW()-INTERVAL '11 days',ends_at=NOW()-INTERVAL '4 days' WHERE base_id=$1`, baseID); err != nil {
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
