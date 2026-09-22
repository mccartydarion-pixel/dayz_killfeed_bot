package killfeed

import "time"

// DownloadReport is a sanitized result of one actual ADM download attempt.
// File is always a basename; raw content and provider paths never cross this boundary.
type DownloadReport struct {
	ServerID          int64
	File              string
	PreviousFile      string
	RemoteSize        int64
	DownloadedBytes   int64
	PreviousOffset    int64
	NewOffset         int64
	NewBytes          int64
	EventsParsed      int
	Duration          time.Duration
	Result            string
	ErrorClass        string
	Rotation          bool
	Truncated         bool
	CheckpointCurrent bool
	At                time.Time
	// Mode is "full" for the existing ReadLog path, or a nitrado.Capability string (e.g.
	// "SEEK_SUPPORTED") when a partial read served this download (Champion Performance Phase 1.5,
	// task section 29). Always "full" unless NITRADO_DELTA_READ_MODE is set to something other
	// than "off".
	Mode string
}
