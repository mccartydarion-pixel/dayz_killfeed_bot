package killfeed

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
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

func TestNextDelayCountsFromPollStart(t *testing.T) {
	e := &Engine{pollInterval: 2 * time.Second, state: StatePolling}
	if got := e.nextDelay(700 * time.Millisecond); got != 1300*time.Millisecond {
		t.Fatalf("poll time is taken off the wait: %v", got)
	}
	if got := e.nextDelay(5 * time.Second); got != minPollGap {
		t.Fatalf("a slow poll still leaves the minimum gap: %v", got)
	}
	e.state = StateDiscovery
	if got, want := e.nextDelay(time.Second), discoveryBackoff(0); got != want {
		t.Fatalf("discovery backoff is a full wait: %v want %v", got, want)
	}
}

// Players online keep the fast rate through the gap between two 5-minute player-list writes, but
// not forever: a log that has not changed for playersBusyWindow is quiet whatever the tracker says.
func TestPollingIntervalStaysFastWhilePlayersAreOnline(t *testing.T) {
	now := time.Now()
	e := &Engine{pollInterval: 10 * time.Second, fastInterval: 3 * time.Second, players: NewPlayerTracker()}
	e.client = budgetSource{b: nitrado.Budget{Known: true, Limit: 1000, Remaining: 900, Reset: now.Add(time.Hour)}}

	e.lastLogChange = now.Add(-busyWindow - 20*time.Second)
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("no players, log quiet past busyWindow: %v", got)
	}
	e.players.PlayerConnected(&PlayerRef{Name: "A", ID: "a1"})
	if got := e.pollingInterval(now); got != 3*time.Second {
		t.Fatalf("players online, last change just past busyWindow: %v", got)
	}
	e.lastLogChange = now.Add(-playersBusyWindow - time.Second)
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("players 'online' but the log has not changed for playersBusyWindow: %v", got)
	}
	e.lastLogChange = time.Time{}
	if got := e.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("no log change seen yet: %v", got)
	}
	// The budget still decides: players online never override a low or unknown budget.
	e.lastLogChange = now.Add(-time.Minute)
	e.client = budgetSource{b: nitrado.Budget{Known: true, Limit: 1000, Remaining: 100, Reset: now.Add(time.Hour)}}
	if got := e.pollingInterval(now); got != 30*time.Second {
		t.Fatalf("low budget with players online: %v", got)
	}
}

// A 429, a 5xx or a network failure ends the fast rate at once: the next poll waits the base
// interval, then doubles per further failure up to the cap, and is a full wait however long the
// failed cycle took. A missing file (rotation) is not a reason to wait. One success ends it.
func TestPollingBacksOffImmediatelyOnNitradoFailures(t *testing.T) {
	now := time.Now()
	e := &Engine{pollInterval: 10 * time.Second, fastInterval: 3 * time.Second, state: StatePolling, lastLogChange: now.Add(-time.Minute)}
	e.client = budgetSource{b: nitrado.Budget{Known: true, Limit: 1000, Remaining: 900, Reset: now.Add(time.Hour)}}
	if got := e.pollingInterval(now); got != 3*time.Second {
		t.Fatalf("precondition, busy server polls fast: %v", got)
	}
	rateLimited := &nitrado.RequestError{Op: "stat file", Kind: nitrado.KindTemporary, StatusCode: 429}
	for i, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second} {
		e.noteTransportError(rateLimited)
		if got := e.pollingInterval(now); got != want {
			t.Fatalf("after %d consecutive failures: %v, want %v", i+1, got, want)
		}
	}
	if got := e.nextDelay(45 * time.Second); got != 60*time.Second {
		t.Fatalf("the backoff must be a full wait after a slow failed cycle: %v", got)
	}
	e.state = StateDiscovery // the selected log was given up on: discovery waits at least as long
	if got := e.nextInterval(); got != 60*time.Second {
		t.Fatalf("discovery during a failure streak: %v", got)
	}
	e.state = StatePolling
	e.noteTransportSuccess()
	if got := e.pollingInterval(now); got != 3*time.Second {
		t.Fatalf("one success restores the normal rate: %v", got)
	}

	e.noteTransportError(&nitrado.RequestError{Op: "stat file", Kind: nitrado.KindNotFound, StatusCode: 404})
	if got := e.pollingInterval(now); got != 3*time.Second {
		t.Fatalf("a missing file is a rotation, not an overloaded API: %v", got)
	}
	e.state = StateDiscovery
	if got, want := e.nextInterval(), discoveryBackoff(0); got != want {
		t.Fatalf("rediscovery after a rotation keeps its own short backoff: %v want %v", got, want)
	}

	// Without rate-limit telemetry (a client that reports no budget) the backoff still applies, and
	// a failed signed download (5xx from the file host, a network error) counts like an API failure.
	plain := &Engine{pollInterval: 10 * time.Second, state: StatePolling, client: &fakeLogSource{}}
	plain.noteTransportError(&nitrado.RequestError{Op: "read remote log", Kind: nitrado.KindUnknown, StatusCode: 503})
	plain.noteTransportError(fmt.Errorf("read remote log: %w", &url.Error{Op: "Get", URL: "https://files.invalid/x", Err: errors.New("connection reset")}))
	if got := plain.pollingInterval(now); got != 20*time.Second {
		t.Fatalf("no budget source, two failures: %v", got)
	}
	plain.noteTransportSuccess()
	plain.noteTransportError(errors.New("no log files discovered"))
	if got := plain.pollingInterval(now); got != 10*time.Second {
		t.Fatalf("an empty listing is not an overloaded API: %v", got)
	}
}

type countingSource struct{ lists atomic.Int64 }

func (c *countingSource) ListLogs(context.Context, string) ([]nitrado.LogFile, error) {
	c.lists.Add(1)
	return nil, nil
}
func (c *countingSource) ReadLog(context.Context, string, string) ([]byte, error) { return nil, nil }

// A worker's first cycle starts about a second after it starts, not a whole poll interval later:
// after a restart the bot is looking at the log again at once.
func TestStartRunsFirstCycleWithoutWaitingAWholeInterval(t *testing.T) {
	src := &countingSource{}
	e := NewEngine(src, "svc-first-poll", testParser{})
	e.pollInterval = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Start(ctx) }()
	deadline := time.Now().Add(firstPollDelay + 2*time.Second)
	for src.lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if src.lists.Load() == 0 {
		t.Fatalf("no poll within %v of the start", firstPollDelay+2*time.Second)
	}
}
