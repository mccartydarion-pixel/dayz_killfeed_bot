//go:build integration

package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The website builds the "Add Champion" link from the running bot, never from its own settings.
func TestBotInviteNamesTheRunningBot(t *testing.T) {
	a, _ := saasIntegrationApp(t)
	rr := httptest.NewRecorder()
	a.handleBotInvite(rr, saasRequest(http.MethodGet, "/api/saas/discord/bot-invite", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("bot invite: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[BotInviteSummary](t, rr)
	if got.ApplicationID != "fake-bot-id" || got.Permissions != "8" {
		t.Fatalf("bot invite = %+v", got)
	}
	unauth := httptest.NewRecorder()
	req := saasRequest(http.MethodGet, "/api/saas/discord/bot-invite", nil)
	req.Header.Del("Authorization")
	a.handleBotInvite(unauth, req)
	if unauth.Code == http.StatusOK {
		t.Fatal("bot invite answered without service auth")
	}
}
