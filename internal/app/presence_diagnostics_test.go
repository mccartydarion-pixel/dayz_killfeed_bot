package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
)

func TestClassifyPresence(t *testing.T) {
	cases := []struct {
		name             string
		snapshot         killfeed.PresenceSnapshot
		voice            int
		resolved, worker bool
		want             string
	}{
		{"disconnect persistence", killfeed.PresenceSnapshot{OnlineCount: 1, LastEventType: "PLAYER_DISCONNECT", LastPersistenceResult: "FAILURE"}, 1, true, true, "DISCONNECT_NOT_PERSISTED"},
		{"voice stale", killfeed.PresenceSnapshot{OnlineCount: 0}, 1, true, true, "VOICE_COUNTER_NOT_REFRESHED"},
		{"tracker removal", killfeed.PresenceSnapshot{OnlineCount: 1, LastEventType: "PLAYER_DISCONNECT", LastPersistenceResult: "SUCCESS"}, 1, true, true, "TRACKER_REMOVE_FAILED"},
		{"wrong worker", killfeed.PresenceSnapshot{}, 0, false, false, "WRONG_SERVER_WORKER_SELECTED"},
		{"healthy", killfeed.PresenceSnapshot{OnlineCount: 0}, 0, true, true, "HEALTHY"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyPresence(test.snapshot, test.voice, test.resolved, test.worker); got != test.want {
				t.Fatalf("got %s want %s", got, test.want)
			}
		})
	}
}
