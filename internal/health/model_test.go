package health

import "testing"

func TestRegistrySeverity(t *testing.T) {
	r := NewRegistry()
	r.Set(Component{Name: "leaderboard", State: Degraded})
	if r.Snapshot().Overall != Degraded {
		t.Fatal("noncritical degradation should surface as degraded")
	}
	r.Set(Component{Name: "database", State: Unhealthy, Critical: true})
	if r.Snapshot().Overall != Unhealthy {
		t.Fatal("critical failure should be unhealthy")
	}
}
