package discord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestStartNilReceiverAndSessionReturnError(t *testing.T) {
	var nilClient *Client
	if err := nilClient.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "discord session not initialized") {
		t.Fatalf("unexpected nil receiver error: %v", err)
	}
	if err := (&Client{}).Start(context.Background()); err == nil || !strings.Contains(err.Error(), "discord session not initialized") {
		t.Fatalf("unexpected nil session error: %v", err)
	}
}
func TestNewInitializesSession(t *testing.T) {
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || client.session == nil {
		t.Fatal("constructor must initialize Discord session")
	}
	if _, err := New(" "); err == nil {
		t.Fatal("empty token should be rejected")
	}
}

func TestNewRequestsOnlyConfiguredMembersIntent(t *testing.T) {
	without, err := New("test-token", false)
	if err != nil {
		t.Fatal(err)
	}
	if without.GuildMembersIntentRequested() {
		t.Fatal("members intent should not be requested when disabled")
	}
	with, err := New("test-token", true)
	if err != nil {
		t.Fatal(err)
	}
	if !with.GuildMembersIntentRequested() {
		t.Fatal("members intent should be requested when enabled")
	}
}

func TestWaitForSessionUserReturnsTrueImmediatelyWhenAlreadyReady(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	session.State.User = &discordgo.User{ID: "1"}
	if !waitForSessionUser(context.Background(), session, time.Second) {
		t.Fatal("expected true when State.User is already populated")
	}
}

func TestWaitForSessionUserWaitsForReadyThenSucceeds(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	go func() {
		time.Sleep(20 * time.Millisecond)
		session.State.User = &discordgo.User{ID: "1"}
	}()
	if !waitForSessionUser(context.Background(), session, time.Second) {
		t.Fatal("expected true once READY populates State.User")
	}
}

func TestWaitForSessionUserTimesOutWhenReadyNeverArrives(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	if waitForSessionUser(context.Background(), session, 30*time.Millisecond) {
		t.Fatal("expected false when READY never arrives before the deadline")
	}
}

func TestWaitForSessionUserStopsOnContextCancel(t *testing.T) {
	session := &discordgo.Session{State: discordgo.NewState()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForSessionUser(ctx, session, time.Second) {
		t.Fatal("expected false when context is already cancelled")
	}
}
