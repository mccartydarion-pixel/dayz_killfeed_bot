package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/leader"
)

func TestDeployProblems(t *testing.T) {
	healthy := func() deployFacts {
		return deployFacts{Commit: "0d66f2dfcdf8541ef439b80bf5b67ab8a38d58e0", DatabaseExpected: true, DatabaseConnected: true, MigrationsApplied: 125,
			Leadership: leader.Status{Enabled: true, Leader: true}, DiscordGateway: true, WorkersExpected: 2, WorkersStarted: 2, WorkersReading: 2,
			ReadyAfter: 1700 * time.Millisecond}
	}
	for name, c := range map[string]struct {
		mutate func(*deployFacts)
		want   []string // a fragment of each expected problem, in order
	}{
		"healthy":                    {func(f *deployFacts) {}, nil},
		"no election acts as leader": {func(f *deployFacts) { f.Leadership = leader.Status{Leader: true} }, nil},
		"no database configured": {func(f *deployFacts) {
			f.DatabaseExpected, f.DatabaseConnected, f.MigrationsApplied = false, false, -1
		}, nil},
		"no servers yet":         {func(f *deployFacts) { f.WorkersExpected, f.WorkersStarted, f.WorkersReading = 0, 0, 0 }, nil},
		"a server added later":   {func(f *deployFacts) { f.WorkersStarted, f.WorkersReading = 3, 3 }, nil},
		"database down":          {func(f *deployFacts) { f.DatabaseConnected = false }, []string{"database is not connected"}},
		"migrations not counted": {func(f *deployFacts) { f.MigrationsApplied = -1 }, []string{"migrations could not be counted"}},
		"standby":                {func(f *deployFacts) { f.Leadership = leader.Status{Enabled: true} }, []string{"two processes are running"}},
		"lock unreachable": {func(f *deployFacts) {
			f.Leadership = leader.Status{Enabled: true, LastError: "connection refused"}
		}, []string{"leader lock cannot be taken (connection refused)"}},
		"gateway down":         {func(f *deployFacts) { f.DiscordGateway = false }, []string{"Discord gateway is not connected"}},
		"a worker missing":     {func(f *deployFacts) { f.WorkersStarted, f.WorkersReading = 1, 1 }, []string{"1 of 2 server workers are not running"}},
		"a worker not reading": {func(f *deployFacts) { f.WorkersReading = 1 }, []string{"1 of 2 server workers have not read their server log"}},
		"several at once": {func(f *deployFacts) {
			f.DiscordGateway, f.WorkersStarted, f.WorkersReading = false, 1, 0
		}, []string{"Discord gateway", "1 of 2 server workers are not running", "1 of 1 server workers have not read"}},
	} {
		f := healthy()
		c.mutate(&f)
		got := deployProblems(f)
		if len(got) != len(c.want) {
			t.Errorf("%s: problems = %q, want %d", name, got, len(c.want))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], c.want[i]) {
				t.Errorf("%s: problem %d = %q, want it to contain %q", name, i, got[i], c.want[i])
			}
		}
	}

	line := deploySummary(DeployHealthy, healthy(), nil)
	want := "deploy self-check HEALTHY: commit 0d66f2dfcdf8, 125 migrations applied, leadership LEADER, Discord gateway connected, 2/2 server workers started, 2 reading logs, ready 1.7s after start"
	if line != want {
		t.Fatalf("summary line:\n got %q\nwant %q", line, want)
	}
	bad := healthy()
	bad.DiscordGateway = false
	if got := deploySummary(DeployUnhealthy, bad, deployProblems(bad)); !strings.Contains(got, "UNHEALTHY") || !strings.Contains(got, "NOT connected") || !strings.HasSuffix(got, "problems: the Discord gateway is not connected") {
		t.Fatalf("unhealthy line = %q", got)
	}
	if strings.Contains(line, "\n") {
		t.Fatal("the self-check is one line")
	}
}

// The check waits while the process is still starting, records HEALTHY once everything is in
// place, and records UNHEALTHY (then a recovery) when the deadline passes first.
func TestDeploySelfCheckRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	a := &App{}
	a.discordReady = func() bool {
		select {
		case <-ready:
			return true
		default:
			return false
		}
	}
	a.deploy.readyAt, a.deploy.state = time.Now(), DeployPending
	if got := a.runtimeDeploy(time.Now()); got.State != DeployPending || len(got.Problems) != 1 || got.DiscordGateway || got.Leadership != "NO_LOCK" || got.ReadyMs == nil {
		t.Fatalf("pending view = %+v", got)
	}
	done := make(chan struct{})
	go func() {
		a.runDeploySelfCheck(ctx, 5*time.Millisecond, time.Hour, 5*time.Millisecond, time.Second)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	if got := a.runtimeDeploy(time.Now()); got.State != DeployPending {
		t.Fatalf("before the gateway is up the check must still be pending, got %s", got.State)
	}
	close(ready)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the check did not finish once healthy")
	}
	if got := a.runtimeDeploy(time.Now()); got.State != DeployHealthy || got.CheckedAt == nil || len(got.Problems) != 0 {
		t.Fatalf("healthy view = %+v", got)
	}

	// Never healthy before the deadline: UNHEALTHY, no admin message without the Owner Hub
	// repository, and HEALTHY again when it recovers afterwards.
	recovered := make(chan struct{})
	var dms int
	b := &App{}
	b.ownerOpsDM = func(string, string) error { dms++; return nil }
	b.discordReady = func() bool {
		select {
		case <-recovered:
			return true
		default:
			return false
		}
	}
	b.deploy.readyAt, b.deploy.state = time.Now(), DeployPending
	done = make(chan struct{})
	go func() {
		b.runDeploySelfCheck(ctx, 5*time.Millisecond, 40*time.Millisecond, 5*time.Millisecond, 5*time.Second)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for b.runtimeDeploy(time.Now()).State != DeployUnhealthy {
		if time.Now().After(deadline) {
			t.Fatal("the check never became UNHEALTHY")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(recovered)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the check did not notice the recovery")
	}
	if got := b.runtimeDeploy(time.Now()); got.State != DeployHealthy {
		t.Fatalf("after recovery = %+v", got)
	}
	if dms != 0 {
		t.Fatalf("nobody was told it failed, so nobody is told it recovered; sent %d", dms)
	}
}

type stubFeed struct{ wait time.Duration }

func (s stubFeed) WaitDone() { time.Sleep(s.wait) }

// A shutdown waits for the feeds' last flush, but not forever.
func TestWaitFeedsFlushedIsBounded(t *testing.T) {
	if !waitFeedsFlushed([]stubFeed{{0}, {10 * time.Millisecond}}, 5*time.Second) {
		t.Fatal("quick feeds must count as flushed")
	}
	started := time.Now()
	if waitFeedsFlushed([]stubFeed{{time.Minute}}, time.Millisecond) {
		t.Fatal("a feed that never finishes must not count as flushed")
	}
	if took := time.Since(started); took < 2*time.Second || took > 10*time.Second {
		t.Fatalf("the wait must be bounded (floor 2s), took %s", took)
	}
	if shutdownBudget >= 30*time.Second || shutdownWorkerStop >= shutdownBudget {
		t.Fatal("the shutdown budget must stay inside Railway's 30 second drain")
	}
}
