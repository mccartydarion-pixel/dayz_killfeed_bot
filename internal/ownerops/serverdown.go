package ownerops

import "time"

// The server-down rule (docs/SERVER_DOWN_ALERT.md). A running DayZ server writes to its engine
// report every minute or two even with nobody on it, so log files that have stopped growing mean
// the game process is not running. Nitrado's own status is not used: it kept answering "started"
// through a nine-hour outage on 2026-10-10.

const (
	// DefaultServerDownAfter is how long the logs must be still before the alert. It has to
	// outlast a restart (a few minutes) and Nitrado's five-minute steps in showing new bytes.
	DefaultServerDownAfter = 20 * time.Minute
	// MinServerDownAfter is the shortest wait that does not alert on an ordinary restart.
	MinServerDownAfter = 10 * time.Minute
	// FailedRestartAfter is the shorter wait when the logs stopped at a restart that was due.
	// Nitrado shows the files of a new start about 10 minutes after the shutdown was seen
	// (production, 2026-10-09: 8.6 to 10.2 minutes), so anything shorter alerts on healthy restarts.
	FailedRestartAfter = 15 * time.Minute
	// restartDueSlack: the logs of a run stop a couple of minutes before the next start.
	restartDueSlack = 5 * time.Minute
	// restartingAfter: logs still for this long at a due restart read as "restarting".
	restartingAfter = 3 * time.Minute
	// serverDownListingMaxAge: an older listing means Champion cannot see the server's files, so
	// it does not know whether they are growing.
	serverDownListingMaxAge = 5 * time.Minute
	// serverDownMaxSilence: logs still for longer than this belong to a server that was switched
	// off or given up, not to an outage somebody needs waking for.
	serverDownMaxSilence = 24 * time.Hour
)

// DownInput is what is known about one server's log files at one moment.
type DownInput struct {
	Now time.Time
	// LastGrowth is the last time any of the server's log files was seen to grow; zero when none
	// has been seen to.
	LastGrowth time.Time
	// LastListing is the last time Nitrado listed the server's files; ListingFailed is true when
	// the newest attempt failed.
	LastListing   time.Time
	ListingFailed bool
	// After is how long the logs must be still (0 = DefaultServerDownAfter).
	After time.Duration
	// BootAt is when the current run started (UTC), zero when unknown. RunLength is the usual
	// time from one start to the next, 0 when unknown.
	BootAt    time.Time
	RunLength time.Duration
}

// RestartDue reports whether the logs stopped at (or after) the moment this run was due to shut
// down for its scheduled restart.
func (in DownInput) RestartDue() bool {
	return !in.BootAt.IsZero() && in.RunLength > 0 && !in.LastGrowth.IsZero() && !in.LastGrowth.Before(in.BootAt.Add(in.RunLength-restartDueSlack))
}

// NextRestart is about when the next start is expected, zero when unknown.
func (in DownInput) NextRestart() time.Time {
	if in.BootAt.IsZero() || in.RunLength <= 0 {
		return time.Time{}
	}
	return in.BootAt.Add(in.RunLength)
}

// Game server states for the status board.
const (
	GameUnknown    = "UNKNOWN"
	GameOnline     = "ONLINE"
	GameRestarting = "RESTARTING"
	GameDown       = "DOWN"
)

// GameState is how the game server reads right now.
func GameState(in DownInput) string {
	silentFor, down, known := ServerDown(in)
	switch {
	case !known:
		return GameUnknown
	case down:
		return GameDown
	case silentFor >= restartingAfter && in.RestartDue():
		return GameRestarting
	}
	return GameOnline
}

// TypicalRunLength is the usual time from one server start to the next: the median gap between
// the given start times. 0 when fewer than three starts are known or the gaps are not plausible.
func TypicalRunLength(starts []time.Time) time.Duration {
	if len(starts) < 3 {
		return 0
	}
	sorted := append([]time.Time(nil), starts...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Before(sorted[j-1]); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	if len(sorted) > 13 {
		sorted = sorted[len(sorted)-13:]
	}
	gaps := make([]time.Duration, 0, len(sorted)-1)
	for i := 1; i < len(sorted); i++ {
		if g := sorted[i].Sub(sorted[i-1]); g >= 10*time.Minute && g <= 24*time.Hour {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) < 2 {
		return 0
	}
	for i := 1; i < len(gaps); i++ {
		for j := i; j > 0 && gaps[j] < gaps[j-1]; j-- {
			gaps[j], gaps[j-1] = gaps[j-1], gaps[j]
		}
	}
	return gaps[len(gaps)/2]
}

// ServerDown reports whether the server looks down and for how long its logs have been still.
// known is false when Champion cannot tell: it has seen no growth yet, or it cannot list the
// server's files right now. An unknown answer must leave an alert as it is.
func ServerDown(in DownInput) (silentFor time.Duration, down, known bool) {
	after := in.After
	if after <= 0 {
		after = DefaultServerDownAfter
	}
	if in.LastGrowth.IsZero() || in.LastListing.IsZero() || in.ListingFailed || in.Now.Sub(in.LastListing) > serverDownListingMaxAge {
		return 0, false, false
	}
	silentFor = in.Now.Sub(in.LastGrowth)
	if silentFor > serverDownMaxSilence {
		return silentFor, false, true
	}
	if in.RestartDue() && after > FailedRestartAfter {
		after = FailedRestartAfter
	}
	return silentFor, silentFor >= after, true
}
