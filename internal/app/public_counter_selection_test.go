package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestSelectPublicCounterServerRestoresPersistedSelection(t *testing.T) {
	active := []repository.GameServer{{ID: 10}, {ID: 20}}
	if got, ok := selectPublicCounterServer(20, active); !ok || got != 20 {
		t.Fatalf("expected persisted server 20, got %d ok=%v", got, ok)
	}
}

func TestSelectPublicCounterServerRejectsMissingPersistedSelection(t *testing.T) {
	active := []repository.GameServer{{ID: 10}, {ID: 20}}
	if got, ok := selectPublicCounterServer(30, active); ok || got != 0 {
		t.Fatalf("expected unresolved selection, got %d ok=%v", got, ok)
	}
}

func TestSelectPublicCounterServerInitialFallbackOnlyWithoutSelection(t *testing.T) {
	active := []repository.GameServer{{ID: 10}, {ID: 20}}
	if got, ok := selectPublicCounterServer(0, active); !ok || got != 10 {
		t.Fatalf("expected initial fallback 10, got %d ok=%v", got, ok)
	}
}
