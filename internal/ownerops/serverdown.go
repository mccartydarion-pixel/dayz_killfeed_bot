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
	return silentFor, silentFor >= after, true
}
