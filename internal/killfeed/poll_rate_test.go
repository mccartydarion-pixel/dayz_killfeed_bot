package killfeed

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

type budgetSource struct {
	LogSource
	b nitrado.Budget
}

func (s budgetSource) RateBudget() nitrado.Budget { return s.b }

func TestPollingIntervalAdaptsToActivityAndBudget(t *testing.T) {
	now := time.Now()
	e := &Engine{pollInterval: 10 * time.Second, fastInterval: 3 * time.Second}
	healthy := nitrado.Budget{Known: true, Limit: 1000, Remaining: 900, Reset: now.Add(time.Hour)}
	low := nitrado.Budget{Known: true, Limit: 1000, Remaining: 100, Reset: now.Add(time.Hour)}

	e.client = budgetSource{b: healthy}
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("idle server stays at base: %v", got)
	}
	e.lastLogChange = now.Add(-time.Minute)
	if got := e.pollingInterval(now); got != 3*time.Second {
		t.Fatalf("busy server with headroom polls fast: %v", got)
	}
	e.client = budgetSource{b: nitrado.Budget{}}
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("unknown budget never polls fast: %v", got)
	}
	e.client = budgetSource{b: nitrado.Budget{Known: true, Limit: 1000, Remaining: 400, Reset: now.Add(time.Hour)}}
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("under half left stays at base: %v", got)
	}
	e.client = budgetSource{b: low}
	if got := e.pollingInterval(now); got != 30*time.Second {
		t.Fatalf("low budget slows down: %v", got)
	}
	e.fastInterval = 0
	e.client = budgetSource{b: healthy}
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("fast disabled: %v", got)
	}
}
