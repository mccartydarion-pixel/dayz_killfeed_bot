package discord

import (
	"context"
	"strings"
	"testing"
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
