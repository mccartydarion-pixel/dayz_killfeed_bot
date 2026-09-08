package killfeed

import "time"

// LogCheckpoint records the last safely processed log state.
type LogCheckpoint struct {
	ServiceID    string
	FilePath     string
	Offset       int64
	FileSize     int64
	LastModified time.Time
}

// Tracker stores incremental log processing state for one service.
type Tracker struct {
	ServiceID         string
	LastProcessedLine int
	LastByteOffset    int64
	LastProcessedAt   time.Time
	CurrentLogFile    string
	CurrentSize       int64
	CurrentModified   time.Time
	Checkpoints       map[string]LogCheckpoint
	LineBuffer        string
}

// NewTracker creates a new checkpoint tracker.
func NewTracker(serviceID string) *Tracker {
	return &Tracker{
		ServiceID:   serviceID,
		Checkpoints: make(map[string]LogCheckpoint),
	}
}

// TrackProcessedLine marks a line as processed.
func (t *Tracker) TrackProcessedLine(lineNumber int, lastByteOffset int64, logFile string) {
	if t == nil {
		return
	}
	t.LastProcessedLine = lineNumber
	t.LastByteOffset = lastByteOffset
	t.CurrentLogFile = logFile
	t.LastProcessedAt = time.Now()

	if t.CurrentLogFile != "" {
		t.Checkpoints[t.CurrentLogFile] = LogCheckpoint{
			ServiceID:    t.ServiceID,
			FilePath:     t.CurrentLogFile,
			Offset:       lastByteOffset,
			FileSize:     t.CurrentSize,
			LastModified: t.CurrentModified,
		}
	}
}

// UpdateCheckpoint stores the latest known offset and file metadata.
func (t *Tracker) UpdateCheckpoint(serviceID, filePath string, size int64, modified time.Time, offset int64) {
	if t == nil {
		return
	}
	t.ServiceID = serviceID
	t.CurrentLogFile = filePath
	t.CurrentSize = size
	t.CurrentModified = modified
	t.LastByteOffset = offset
	if t.Checkpoints == nil {
		t.Checkpoints = make(map[string]LogCheckpoint)
	}
	t.Checkpoints[filePath] = LogCheckpoint{
		ServiceID:    serviceID,
		FilePath:     filePath,
		Offset:       offset,
		FileSize:     size,
		LastModified: modified,
	}
}

// ShouldReadAgain decides whether a file has changed or needs a new read.
func (t *Tracker) ShouldReadAgain(filePath string, size int64, modified time.Time) bool {
	if t == nil || filePath == "" {
		return false
	}
	checkpoint, ok := t.Checkpoints[filePath]
	if !ok {
		return true
	}
	if size < checkpoint.Offset {
		return true
	}
	if !checkpoint.LastModified.Equal(modified) {
		return true
	}
	return false
}

// ResetForRotation clears state when a rotated or truncated log is detected.
func (t *Tracker) ResetForRotation(filePath string) {
	if t == nil {
		return
	}
	t.CurrentLogFile = filePath
	t.LastByteOffset = 0
	t.LastProcessedLine = 0
	t.LineBuffer = ""
	if t.Checkpoints == nil {
		t.Checkpoints = make(map[string]LogCheckpoint)
	}
	t.Checkpoints[filePath] = LogCheckpoint{ServiceID: t.ServiceID, FilePath: filePath, Offset: 0}
}

// AppendPartialLine appends partial bytes so complete lines can be parsed later.
func (t *Tracker) AppendPartialLine(partial string) {
	if t == nil {
		return
	}
	t.LineBuffer += partial
}

// DrainCompleteLines returns complete lines and retains any trailing partial line.
func (t *Tracker) DrainCompleteLines() []string {
	if t == nil {
		return nil
	}
	if t.LineBuffer == "" {
		return nil
	}

	lines := make([]string, 0)
	for {
		idx := len(t.LineBuffer)
		for i := 0; i < len(t.LineBuffer); i++ {
			if t.LineBuffer[i] == '\n' {
				idx = i
				break
			}
		}
		if idx == len(t.LineBuffer) {
			break
		}
		line := t.LineBuffer[:idx]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
		t.LineBuffer = t.LineBuffer[idx+1:]
	}
	return lines
}
