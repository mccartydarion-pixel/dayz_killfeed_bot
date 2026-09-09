package discord

import (
	"strings"
	"testing"
)

func TestRegisterAnalyticsCommandsNilSessionReturnsError(t *testing.T) {
	if err := RegisterAnalyticsCommands(nil, "guild"); err == nil || !strings.Contains(err.Error(), "application ID") {
		t.Fatalf("expected dependency error, got %v", err)
	}
}
