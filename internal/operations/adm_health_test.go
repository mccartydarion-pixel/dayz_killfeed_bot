package operations

import (
	"github.com/yourname/dayz-killfeed/internal/health"
	"testing"
	"time"
)

func TestADMQuietServerHealthy(t *testing.T) {
	now := time.Now()
	m := NewADMMonitor()
	state, _ := m.Evaluate(ADMHealthSnapshot{LastPollSuccessAt: now.Add(-time.Second), LastChangeAt: now.Add(-10 * time.Minute), CurrentFile: "x"}, now)
	if state != health.Healthy {
		t.Fatal(state)
	}
}
func TestADMActiveStallDegraded(t *testing.T) {
	now := time.Now()
	m := NewADMMonitor()
	state, _ := m.Evaluate(ADMHealthSnapshot{LastPollSuccessAt: now, LastChangeAt: now.Add(-6 * time.Minute), OnlinePlayers: 2, CurrentFile: "x"}, now)
	if state != health.Degraded {
		t.Fatal(state)
	}
}
