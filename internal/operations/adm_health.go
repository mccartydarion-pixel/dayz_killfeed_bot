package operations

import (
	"github.com/yourname/dayz-killfeed/internal/health"
	"strings"
	"time"
)

type ADMHealthSnapshot struct {
	LastPollAt, LastPollSuccessAt, LastChangeAt, LastLineAt time.Time
	OnlinePlayers                                           int
	CurrentFile                                             string
	CurrentOffset                                           int64
}
type ADMMonitor struct {
	PollFailureAfter, ActiveStallAfter time.Duration
	rotations                          []time.Time
}

func NewADMMonitor() *ADMMonitor {
	return &ADMMonitor{PollFailureAfter: 2 * time.Minute, ActiveStallAfter: 5 * time.Minute}
}
func (m *ADMMonitor) Rotate(at time.Time) {
	m.rotations = append(m.rotations, at)
	cut := at.Add(-20 * time.Minute)
	i := 0
	for i < len(m.rotations) && m.rotations[i].Before(cut) {
		i++
	}
	m.rotations = m.rotations[i:]
}
func (m *ADMMonitor) RestartLoop() bool { return len(m.rotations) >= 3 }
func (m *ADMMonitor) Evaluate(s ADMHealthSnapshot, now time.Time) (health.State, string) {
	if s.LastPollSuccessAt.IsZero() || now.Sub(s.LastPollSuccessAt) > m.PollFailureAfter {
		return health.Unhealthy, "no successful ADM poll"
	}
	if s.OnlinePlayers > 0 && !s.LastChangeAt.IsZero() && now.Sub(s.LastChangeAt) > m.ActiveStallAfter {
		return health.Degraded, "ADM updates delayed while players are online"
	}
	if strings.TrimSpace(s.CurrentFile) == "" {
		return health.Unknown, "ADM file not selected"
	}
	if m.RestartLoop() {
		return health.Degraded, "possible ADM restart loop"
	}
	return health.Healthy, "ADM polling healthy"
}
