package app

import "github.com/yourname/dayz-killfeed/internal/killfeed"

func classifyPresence(snapshot killfeed.PresenceSnapshot, voiceCount int, selectedResolved, workerFound bool) string {
	return classifyPresenceActual(snapshot, voiceCount, true, selectedResolved, workerFound)
}

func classifyPresenceActual(snapshot killfeed.PresenceSnapshot, actualCount int, actualKnown, selectedResolved, workerFound bool) string {
	if !selectedResolved || !workerFound {
		return "WRONG_SERVER_WORKER_SELECTED"
	}
	if snapshot.LastEventType == "PLAYER_DISCONNECT" && snapshot.LastPersistenceResult == "FAILURE" {
		return "DISCONNECT_NOT_PERSISTED"
	}
	if actualKnown && snapshot.OnlineCount != actualCount {
		return "VOICE_COUNTER_NOT_REFRESHED"
	}
	if snapshot.LastEventType == "PLAYER_DISCONNECT" && snapshot.LastPersistenceResult == "SUCCESS" && snapshot.OnlineCount > 0 {
		return "TRACKER_REMOVE_FAILED"
	}
	if actualKnown && snapshot.OnlineCount == actualCount && snapshot.LastVoicePublishResult == "SUCCESS" {
		return "HEALTHY"
	}
	return "UNKNOWN"
}
