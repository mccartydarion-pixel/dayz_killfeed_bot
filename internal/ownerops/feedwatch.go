package ownerops

import (
	"fmt"
	"time"
)

// Silent feeds, the owner-facing server status and position logging (docs/SERVER_STATUS.md,
// docs/OWNER_OPS.md "Silent feeds"). Pure decisions over plain values, like the rest of the package.

// Thresholds of the silent-feed incident.
const (
	// SilentAfterWithPlayerList: this server writes a player list every five minutes while
	// somebody is online, so this long without one line, with players online throughout, cannot
	// be a quiet server.
	SilentAfterWithPlayerList = 20 * time.Minute
	// SilentAfter is the same rule for a server that has not been seen writing player lists: a
	// lone idle player really can produce no line for a long time, so it waits much longer.
	SilentAfter = 60 * time.Minute
	// UnreadableAfter is how long every log read may fail (Nitrado errors that are not a refused
	// token, which has its own incident) before it counts.
	UnreadableAfter = 15 * time.Minute
)

// SilentFeed reports whether a running worker's feed has gone silent in a way that is worth an
// incident, with the reason. It never fires for a server Nitrado reports as stopped, for an empty
// server, or on durations that have not been observed that long - the caller measures them from
// when this process began watching, so a restart starts every clock again.
func SilentFeed(s ServerState) (detail string, silent bool) {
	if !s.WorkerRunning || s.ServerStopped {
		return "", false
	}
	if s.SourceState == sourceTransport && s.SourceErrorClass != "authentication" && s.SourceErrorClass != "permission" &&
		s.SourceBadFor >= UnreadableAfter {
		class := s.SourceErrorClass
		if class == "" {
			class = "unknown error"
		}
		return fmt.Sprintf("every read of the server log has failed for %d minutes (Nitrado: %s)", minutes(s.SourceBadFor), class), true
	}
	if !s.PlayersKnown || s.PlayersOnline <= 0 {
		return "", false
	}
	threshold, note := SilentAfter, "; if this server has the player list log switched off it may only be idle"
	if s.PlayerListSeen {
		threshold, note = SilentAfterWithPlayerList, ", although it writes a player list every five minutes"
	}
	if s.PlayersOnlineFor >= threshold && s.LogSilentFor >= threshold {
		return fmt.Sprintf("%d player(s) have been online for %d minutes and the server log has produced nothing for %d minutes%s",
			s.PlayersOnline, minutes(s.PlayersOnlineFor), minutes(s.LogSilentFor), note), true
	}
	return "", false
}

func minutes(d time.Duration) int { return int(d / time.Minute) }

// --- owner-facing server status ---------------------------------------------------------------------

// Server status states a customer sees (GET .../admin/server-status). Four on purpose.
const (
	StatusRunning  = "RUNNING"  // the log is read and events are flowing
	StatusQuiet    = "QUIET"    // the log is read; nothing is happening on the server
	StatusDegraded = "DEGRADED" // working in part, or about to recover by itself
	StatusDown     = "DOWN"     // the customer is not getting a feed
)

// Killfeed delivery states as internal/discord reports them.
const (
	deliveryConfigFault = "CONFIG_FAULT"
	deliveryFailing     = "FAILING"
)

// StatusInput is what the owner-facing status is derived from.
type StatusInput struct {
	Server ServerState
	// ServerSelected: the installation has a DayZ server at all.
	ServerSelected bool
	// KillfeedDelivery is the Discord delivery state of the killfeed channel ("" when unknown).
	KillfeedDelivery string
}

// ServerStatus turns the facts into one of four states and one plain sentence. The order is the
// order a customer would want to hear it: what stops the feed altogether first.
func ServerStatus(in StatusInput) (state, reason string) {
	s := in.Server
	switch {
	case s.InstallationStatus == "SUSPENDED":
		return StatusDown, "This installation is suspended, so its feed is switched off."
	case !in.ServerSelected:
		return StatusDown, "No DayZ server is connected to this installation yet."
	case !s.operational():
		return StatusDown, "Setup is not finished yet, so the feed has not started."
	case !s.ServerActive:
		return StatusDown, "The DayZ server is disconnected from Champion."
	case !s.BotInstalled:
		return StatusDown, "The Champion bot is not in your Discord server, so nothing can be posted."
	case !s.WorkerRunning && !s.RuntimeReady:
		return StatusDegraded, "Champion has just restarted and is reconnecting to your server; this takes a minute or two."
	case !s.WorkerRunning:
		return StatusDown, "Nothing is reading your server's log right now; Champion will try to restart it."
	case s.SourceState == sourceTransport && (s.SourceErrorClass == "authentication" || s.SourceErrorClass == "permission"):
		return StatusDown, "Nitrado is refusing the saved access token; reconnect Nitrado to bring the feed back."
	case s.SourceState == sourceStalled && s.RuntimeReady:
		return StatusDown, "The log reader has stopped making progress; Champion will try to restart it."
	case s.SourceState == sourceStalled:
		return StatusDegraded, "Champion has just restarted and is reconnecting to your server; this takes a minute or two."
	case s.SourceState == sourceTransport:
		return StatusDegraded, "Nitrado is not answering Champion's requests for the server log; the feed continues as soon as it does."
	case in.KillfeedDelivery == deliveryConfigFault:
		return StatusDegraded, "The server log is being read, but Discord refuses the killfeed channel: it was deleted or the bot lost permission to post there."
	}
	if _, silent := SilentFeed(s); silent {
		return StatusDegraded, "Players are online but the server log has produced nothing for a long time."
	}
	switch {
	case s.SourceState == sourceLagging:
		return StatusDegraded, "Players are online but the server log is not advancing, or a newer log file has not been picked up yet."
	case in.KillfeedDelivery == deliveryFailing:
		return StatusDegraded, "The server log is being read, but the last killfeed message could not be posted to Discord; it is being retried."
	case s.SourceState == sourceQuiet:
		return StatusQuiet, "The server log is being read; nothing has happened on the server in the last few minutes."
	}
	return StatusRunning, "The server log is being read and events are flowing."
}

// --- position logging -------------------------------------------------------------------------------

// Position logging states.
const (
	PositionOK          = "OK"           // player positions are arriving
	PositionNotArriving = "NOT_ARRIVING" // players are online and no position has arrived
	PositionUnknown     = "UNKNOWN"      // nobody has been online long enough to tell
)

const (
	// PositionFreshFor: DayZ writes the player list every five minutes; two missed lists and a
	// margin is still "arriving".
	PositionFreshFor = 12 * time.Minute
	// PositionMissingAfter is how long players must have been online without one position line.
	PositionMissingAfter = 15 * time.Minute
)

// PositionAdvice is what to tell an owner whose server logs no positions.
const PositionAdvice = "Players are online but your server is not logging their positions, so the live map stays empty. " +
	"In your Nitrado web interface open Settings > General, switch on \"Log player list\" (adminLogPlayerList = 1 in the server configuration), " +
	"save and restart the server. Positions then arrive every five minutes."

// PositionInput is what the position-logging check reads.
type PositionInput struct {
	WorkerRunning    bool
	PlayersKnown     bool
	PlayersOnline    int
	PlayersOnlineFor time.Duration
	// PlayerListSeen and SinceLastPlayerList describe the last player list this process read.
	PlayerListSeen      bool
	SinceLastPlayerList time.Duration
}

// PositionLogging says whether player positions are arriving, and the advice when they are not.
func PositionLogging(in PositionInput) (state, advice string) {
	if !in.WorkerRunning {
		return PositionUnknown, ""
	}
	if in.PlayerListSeen && in.SinceLastPlayerList <= PositionFreshFor {
		return PositionOK, ""
	}
	if in.PlayersKnown && in.PlayersOnline > 0 && in.PlayersOnlineFor >= PositionMissingAfter &&
		(!in.PlayerListSeen || in.SinceLastPlayerList >= PositionMissingAfter) {
		return PositionNotArriving, PositionAdvice
	}
	return PositionUnknown, ""
}
