package app

import "github.com/yourname/dayz-killfeed/internal/killfeed"

func classifyPresence(snapshot killfeed.PresenceSnapshot, voiceCount int, selectedResolved, workerFound bool) string {
	if !selectedResolved || !workerFound {
		return "WRONG_SERVER_WORKER_SELECTED"
	}
	if snapshot.LastEventType == "PLAYER_DISCONNECT" && snapshot.LastPersistenceResult == "FAILURE" {
		return "DISCONNECT_NOT_PERSISTED"
	}
	if snapshot.OnlineCount == 0 && voiceCount != 0 {
		return "VOICE_COUNTER_NOT_REFRESHED"
	}
	if snapshot.LastEventType == "PLAYER_DISCONNECT" && snapshot.LastPersistenceResult == "SUCCESS" && snapshot.OnlineCount > 0 {
		return "TRACKER_REMOVE_FAILED"
	}
	if snapshot.OnlineCount == voiceCount {
		return "HEALTHY"
	}
	return "UNKNOWN"
}
