package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/leader"
)

// The deploy self-check (docs/DEPLOY.md): after start-up the process checks itself and logs ONE
// line saying what is running - the commit, the migrations, leadership, the Discord gateway, the
// server workers and how long start-up took. If it is still not healthy five minutes after the
// process started, the platform admins are told through the Owner Hub's alert path (the same DMs
// incidents use, behind the same "alerts" switch). The same facts are in GET /api/runtime/status
// under "deploy".

// processStartedAt is when this process began (package initialisation, before main).
var processStartedAt = time.Now()

const (
	// deployCheckEvery is how often the check looks again while it waits.
	deployCheckEvery = 5 * time.Second
	// deployCheckDeadline is how long after process start everything must be healthy. A deploy
	// overlap (the old process still holds the leader lock) and the first Nitrado read of every
	// worker fit in it many times over.
	deployCheckDeadline = 5 * time.Minute
	// deployRecoveryWatch is how long an unhealthy start is watched for a recovery afterwards.
	deployRecoveryWatch = time.Hour
	deployRecoveryEvery = 30 * time.Second
)

// Deploy self-check verdicts.
const (
	DeployPending   = "PENDING"
	DeployHealthy   = "HEALTHY"
	DeployUnhealthy = "UNHEALTHY"
)

// deployFacts is what the check looks at, gathered from memory.
type deployFacts struct {
	Commit            string
	DatabaseExpected  bool // DATABASE_URL is configured
	DatabaseConnected bool
	MigrationsApplied int // -1 when not counted
	Leadership        leader.Status
	DiscordGateway    bool
	WorkersExpected   int // active game servers when the process started them
	WorkersStarted    int
	WorkersReading    int // started workers whose last log read worked
	ReadyAfter        time.Duration
}

// deployLeadership names the leadership state in one word.
func deployLeadership(st leader.Status) string {
	switch {
	case !st.Enabled:
		return "NO_LOCK" // no election: this process runs every singleton worker
	case st.Leader:
		return "LEADER"
	case st.LastError != "":
		return "ERROR"
	}
	return "STANDBY"
}

// deployProblems lists what is not healthy. Before the deadline the caller treats the list as
// "still starting"; at the deadline it is the failure.
func deployProblems(f deployFacts) []string {
	var out []string
	if f.DatabaseExpected && !f.DatabaseConnected {
		out = append(out, "the database is not connected")
	}
	if f.DatabaseConnected && f.MigrationsApplied <= 0 {
		out = append(out, "the applied migrations could not be counted")
	}
	switch deployLeadership(f.Leadership) {
	case "ERROR":
		out = append(out, "the leader lock cannot be taken ("+f.Leadership.LastError+"), so no board, reminder or scheduler runs")
	case "STANDBY":
		out = append(out, "another bot process still holds the leader lock, so two processes are running")
	}
	if !f.DiscordGateway {
		out = append(out, "the Discord gateway is not connected")
	}
	if f.WorkersStarted < f.WorkersExpected {
		out = append(out, fmt.Sprintf("%d of %d server workers are not running", f.WorkersExpected-f.WorkersStarted, f.WorkersExpected))
	}
	if f.WorkersReading < f.WorkersStarted {
		out = append(out, fmt.Sprintf("%d of %d server workers have not read their server log", f.WorkersStarted-f.WorkersReading, f.WorkersStarted))
	}
	return out
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// deploySummary is the one line.
func deploySummary(result string, f deployFacts, problems []string) string {
	migrations := "migrations not counted"
	if f.MigrationsApplied >= 0 {
		migrations = fmt.Sprintf("%d migrations applied", f.MigrationsApplied)
	}
	gateway := "Discord gateway connected"
	if !f.DiscordGateway {
		gateway = "Discord gateway NOT connected"
	}
	line := fmt.Sprintf("deploy self-check %s: commit %s, %s, leadership %s, %s, %d/%d server workers started, %d reading logs, ready %.1fs after start",
		result, shortCommit(f.Commit), migrations, deployLeadership(f.Leadership), gateway, f.WorkersStarted, f.WorkersExpected, f.WorkersReading, f.ReadyAfter.Seconds())
	if len(problems) > 0 {
		line += "; problems: " + strings.Join(problems, "; ")
	}
	return line
}

// deploySelfCheck is the recorded outcome.
type deploySelfCheck struct {
	mu                sync.Mutex
	migrationsApplied int
	migrationsCounted bool
	expectedWorkers   int
	readyAt           time.Time
	state             string
	checkedAt         time.Time
	alerted           bool
}

func (d *deploySelfCheck) setMigrations(n int) {
	d.mu.Lock()
	d.migrationsApplied, d.migrationsCounted = n, true
	d.mu.Unlock()
}

func (d *deploySelfCheck) setExpectedWorkers(n int) {
	d.mu.Lock()
	d.expectedWorkers = n
	d.mu.Unlock()
}

func (d *deploySelfCheck) record(state string, at time.Time) {
	d.mu.Lock()
	d.state, d.checkedAt = state, at
	d.mu.Unlock()
}

// deployFactsNow gathers the facts. No database, no Nitrado, no Discord call.
func (a *App) deployFactsNow(now time.Time) deployFacts {
	a.deploy.mu.Lock()
	f := deployFacts{Commit: buildCommit(), MigrationsApplied: -1, WorkersExpected: a.deploy.expectedWorkers}
	if a.deploy.migrationsCounted {
		f.MigrationsApplied = a.deploy.migrationsApplied
	}
	if !a.deploy.readyAt.IsZero() {
		f.ReadyAfter = a.deploy.readyAt.Sub(processStartedAt)
	}
	a.deploy.mu.Unlock()
	f.DatabaseExpected = a.Config != nil && a.Config.DatabaseURL != ""
	f.DatabaseConnected = a.DB != nil
	f.Leadership = a.Leader.Status()
	f.DiscordGateway = a.discordGatewayConnected()
	for _, id := range a.watchedServerIDs() {
		s := a.sampleServer(id, now)
		if !s.WorkerRunning {
			continue
		}
		f.WorkersStarted++
		if !s.LastLogCheckAt.IsZero() && s.SourceState != killfeed.ADMWorkerStalled && s.SourceState != killfeed.ADMTransportError {
			f.WorkersReading++
		}
	}
	return f
}

// markReady records that start-up finished and starts the self-check. Called once, when the
// process is about to serve.
func (a *App) markReady(ctx context.Context, expectedWorkers int) {
	a.deploy.mu.Lock()
	first := a.deploy.readyAt.IsZero()
	if first {
		a.deploy.readyAt = time.Now()
		a.deploy.expectedWorkers = expectedWorkers
		a.deploy.state = DeployPending
	}
	a.deploy.mu.Unlock()
	if first {
		go a.runDeploySelfCheck(ctx, deployCheckEvery, deployCheckDeadline, deployRecoveryEvery, deployRecoveryWatch)
	}
}

// runDeploySelfCheck waits for the process to be healthy, logs the one line, and alerts when the
// deadline passes first.
func (a *App) runDeploySelfCheck(ctx context.Context, every, deadline, recoveryEvery, recoveryWatch time.Duration) {
	limit := processStartedAt.Add(deadline)
	a.deploy.mu.Lock()
	if a.deploy.readyAt.Add(deadline / 2).After(limit) {
		// A start-up that itself took most of the window still gets time to settle.
		limit = a.deploy.readyAt.Add(deadline / 2)
	}
	a.deploy.mu.Unlock()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		now := time.Now()
		facts := a.deployFactsNow(now)
		problems := deployProblems(facts)
		if len(problems) == 0 {
			a.deploy.record(DeployHealthy, now)
			slog.Info("component=startup", "event", "deploy_self_check", "result", DeployHealthy, "msg", deploySummary(DeployHealthy, facts, nil),
				"commit", facts.Commit, "migrations_applied", facts.MigrationsApplied, "leadership", deployLeadership(facts.Leadership),
				"discord_gateway", facts.DiscordGateway, "workers_expected", facts.WorkersExpected, "workers_started", facts.WorkersStarted,
				"workers_reading", facts.WorkersReading, "ready_ms", facts.ReadyAfter.Milliseconds(), "healthy_after_s", int(now.Sub(processStartedAt).Seconds()))
			return
		}
		if !now.Before(limit) {
			a.deployUnhealthy(ctx, now, facts, problems)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
	// Unhealthy at the deadline: keep looking, so a late recovery is logged and announced once.
	recovery := time.NewTicker(recoveryEvery)
	defer recovery.Stop()
	until := time.Now().Add(recoveryWatch)
	for time.Now().Before(until) {
		select {
		case <-ctx.Done():
			return
		case <-recovery.C:
		}
		now := time.Now()
		facts := a.deployFactsNow(now)
		if len(deployProblems(facts)) > 0 {
			continue
		}
		a.deploy.record(DeployHealthy, now)
		line := deploySummary("RECOVERED", facts, nil)
		slog.Info("component=startup", "event", "deploy_self_check", "result", "RECOVERED", "msg", line,
			"healthy_after_s", int(now.Sub(processStartedAt).Seconds()))
		a.deploy.mu.Lock()
		alerted := a.deploy.alerted
		a.deploy.mu.Unlock()
		if alerted {
			a.ownerOpsNotifyAdmins("**Deploy recovered**\n" + line)
		}
		return
	}
}

// deployUnhealthy logs the failed check and tells the platform admins, if the owner has alerts
// switched on in the Owner Hub.
func (a *App) deployUnhealthy(ctx context.Context, now time.Time, facts deployFacts, problems []string) {
	a.deploy.record(DeployUnhealthy, now)
	line := deploySummary(DeployUnhealthy, facts, problems)
	slog.Error("component=startup", "event", "deploy_self_check", "result", DeployUnhealthy, "msg", line,
		"commit", facts.Commit, "migrations_applied", facts.MigrationsApplied, "leadership", deployLeadership(facts.Leadership),
		"discord_gateway", facts.DiscordGateway, "workers_expected", facts.WorkersExpected, "workers_started", facts.WorkersStarted,
		"workers_reading", facts.WorkersReading, "ready_ms", facts.ReadyAfter.Milliseconds(), "problems", strings.Join(problems, "; "))
	if a.PlatformOps == nil {
		return
	}
	settingsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	settings, _, err := a.PlatformOps.Settings(settingsCtx)
	cancel()
	if err != nil || !settings.AlertsEnabled {
		return
	}
	text := fmt.Sprintf("**Deploy self-check failed**\nCommit %s is not healthy %d minutes after it started:\n- %s\n%s",
		shortCommit(facts.Commit), int(now.Sub(processStartedAt).Minutes()), strings.Join(problems, "\n- "), line)
	if a.ownerOpsNotifyAdmins(text) > 0 {
		a.deploy.mu.Lock()
		a.deploy.alerted = true
		a.deploy.mu.Unlock()
	}
}

// RuntimeDeploy is the "deploy" block of GET /api/runtime/status: the self-check's verdict and
// the facts as they are now.
type RuntimeDeploy struct {
	// State is the self-check's verdict: PENDING until it has decided, then HEALTHY or UNHEALTHY
	// (UNHEALTHY becomes HEALTHY if the process recovers within the hour).
	State            string  `json:"state"`
	CheckedAt        *string `json:"checkedAt"`
	ProcessStartedAt string  `json:"processStartedAt"`
	ReadyAt          *string `json:"readyAt"`
	// ReadyMs is the time from process start to serving HTTP.
	ReadyMs           *int64 `json:"readyMs"`
	MigrationsApplied *int   `json:"migrationsApplied"`
	// Leadership is LEADER, STANDBY, ERROR or NO_LOCK (no election; acts as leader).
	Leadership      string `json:"leadership"`
	DiscordGateway  bool   `json:"discordGateway"`
	WorkersExpected int    `json:"workersExpected"`
	WorkersStarted  int    `json:"workersStarted"`
	WorkersReading  int    `json:"workersReading"`
	// Problems is what is wrong right now (empty when nothing is).
	Problems []string `json:"problems"`
}

func (a *App) runtimeDeploy(now time.Time) *RuntimeDeploy {
	facts := a.deployFactsNow(now)
	out := &RuntimeDeploy{State: DeployPending, ProcessStartedAt: processStartedAt.UTC().Format(time.RFC3339), Leadership: deployLeadership(facts.Leadership),
		DiscordGateway: facts.DiscordGateway, WorkersExpected: facts.WorkersExpected, WorkersStarted: facts.WorkersStarted, WorkersReading: facts.WorkersReading,
		Problems: deployProblems(facts)}
	if out.Problems == nil {
		out.Problems = []string{}
	}
	if facts.MigrationsApplied >= 0 {
		n := facts.MigrationsApplied
		out.MigrationsApplied = &n
	}
	a.deploy.mu.Lock()
	defer a.deploy.mu.Unlock()
	if a.deploy.state != "" {
		out.State = a.deploy.state
	}
	out.CheckedAt, out.ReadyAt = nullableTime(a.deploy.checkedAt), nullableTime(a.deploy.readyAt)
	if !a.deploy.readyAt.IsZero() {
		ms := a.deploy.readyAt.Sub(processStartedAt).Milliseconds()
		out.ReadyMs = &ms
	}
	return out
}
