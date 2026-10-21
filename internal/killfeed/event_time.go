package killfeed

import (
	"log/slog"
	"time"
)

// The real time of an ADM line. A line carries only the server's local clock (HH:MM:SS), and
// Nitrado exposes the log in steps of minutes, so the time the bot read a line says little about
// when the event happened. The engine derives the event's UTC time instead, from three things it
// already tracks, and that one derivation is what both Event.Timestamp (persisted as
// kills.event_time / deaths.event_time and shown on the cards) and Event.LoggedAt (the latency
// measurement's "game log" stage, feed_latency.go) are made from:
//
//   - the date: the ADM file's boot date from its name (DayZServer_PS4_x64_2026-10-14_20-57-59.ADM)
//     or, for a file without one, the "AdminLog started on" header - admClock (playerlist.go),
//     one per file, which also counts midnight rollovers: a clock that goes back by more than an
//     hour within one file is the next day;
//   - the clock: the line's own TimeOfDay, in the server's local zone;
//   - the offset: the server's UTC offset that Live Sync learned from restart.log, read through
//     Engine.SetServerUTCOffset.
//
// Any of them unknown: no time, never a guess. Event.Timestamp stays zero, the row stores NULL and
// the card falls back to the read time (Event.DisplayTime).

const (
	// eventTimeMaxAge rejects a derived time further in the past than this: a wrong date or a
	// clock stuck on an old boot, not a late log.
	eventTimeMaxAge = 24 * time.Hour
	// eventTimeMaxFuture rejects a derived time further in the future than this: a wrong offset.
	// ADM clocks have one-second resolution and the game server's clock is not the bot's, so a
	// few seconds ahead is normal.
	eventTimeMaxFuture = 10 * time.Minute
	// eventTimeWarnEvery is how often an engine warns about implausible times, at most.
	eventTimeWarnEvery = 5 * time.Minute
)

// lineTimeUTC converts a line's server-local time (admClock.at) to UTC with the server's UTC
// offset in minutes. Zero when either is unknown: nothing is assumed.
func lineTimeUTC(local *time.Time, offsetMinutes int, offsetKnown bool) time.Time {
	if local == nil || !offsetKnown {
		return time.Time{}
	}
	return local.Add(-time.Duration(offsetMinutes) * time.Minute).UTC()
}

// plausibleEventTime reports whether a derived event time can be stored: at most eventTimeMaxAge
// before now and eventTimeMaxFuture after it. A zero time is never plausible.
func plausibleEventTime(at, now time.Time) bool {
	if at.IsZero() {
		return false
	}
	lag := now.Sub(at)
	return lag <= eventTimeMaxAge && lag >= -eventTimeMaxFuture
}

// stampEventTime sets ev.Timestamp from the line's converted time (ev.LoggedAt) when it is known
// and plausible at now. An implausible time leaves Timestamp zero (stored as NULL) and is logged
// at most every eventTimeWarnEvery, with the server id only.
func (e *Engine) stampEventTime(ev *Event, now time.Time) {
	if ev == nil || ev.LoggedAt.IsZero() {
		return
	}
	if !plausibleEventTime(ev.LoggedAt, now) {
		if e.lastEventTimeWarn.IsZero() || now.Sub(e.lastEventTimeWarn) >= eventTimeWarnEvery {
			e.lastEventTimeWarn = now
			slog.Warn("component=killfeed", "event", "event_time_implausible", "server_id", e.serverID,
				"lag_ms", now.Sub(ev.LoggedAt).Milliseconds())
		}
		return
	}
	ev.Timestamp = ev.LoggedAt.UTC()
	ev.TimestampDerived = true
}
