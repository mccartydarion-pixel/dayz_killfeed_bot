package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Anti-cheat setup checklist (docs/CASE_SETUP.md): one owner-facing view of
// what C.A.S.E. needs on this server, each step with a plain state and what to
// do. It reads state only; the one thing it can change is the owner's own
// evidence switch, and only when the platform allows self-serve.

const (
	setupOK      = "OK"      // done
	setupAction  = "ACTION"  // the owner can fix it
	setupWait    = "WAIT"    // happens by itself (players, a restart)
	setupLocked  = "LOCKED"  // decided by Champions
	setupTesting = "TESTING" // detectors still being tested
)

type caseSetupStep struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"` // a hint for the website: RUN_SETUP, TOGGLE_EVIDENCE, TOGGLE_ALERTS, REGISTER_BASES
}

type caseSetupEvidence struct {
	Running        bool `json:"running"`
	Configured     bool `json:"configured"`
	OwnerChoice    bool `json:"ownerChoice"`
	SelfServe      bool `json:"selfServe"`
	SetByChampions bool `json:"setByChampions"`
	PendingRestart bool `json:"pendingRestart"`
}

type caseSetupDetector struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Released bool   `json:"released"`
}

type caseSetupResponse struct {
	Steps     []caseSetupStep     `json:"steps"`
	Evidence  caseSetupEvidence   `json:"evidence"`
	Detectors []caseSetupDetector `json:"detectors"`
	BaseBoost caseSetupBaseBoost  `json:"baseBoost"`
}

// caseSetupBaseBoost is what the Base Boosting test run needs on this server.
type caseSetupBaseBoost struct {
	Steps []caseSetupStep `json:"steps"`
}

const (
	baseBoostDetectorID = "CASE-BASE-001"
	baseBoostLookback   = 7 * 24 * time.Hour
)

func (a *App) handleGetCaseSetup(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseDetectorSettingsActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	serverID := *ac.scope.ServerID
	out := caseSetupResponse{Steps: make([]caseSetupStep, 0, 6)}

	// 1. Private Discord channel for staff alerts.
	discordStep := caseSetupStep{Key: "discord", Label: "Private staff channel in Discord"}
	switch {
	case a.SaaSChannelRoutes == nil || a.saasDiscordVerifier == nil:
		discordStep.State, discordStep.Detail = setupWait, "Discord can't be checked right now. Try again in a minute."
	case ac.scope.DiscordGuildID == "":
		discordStep.State, discordStep.Detail, discordStep.Action = setupAction, "This server isn't connected to a Discord server yet.", "RUN_SETUP"
	default:
		dest, derr := privateCaseAlertsChannel(ctx, a.SaaSChannelRoutes, a.saasDiscordVerifier, ac.scope.OrganizationID, ac.scope.InstallationID, ac.scope.DiscordGuildID)
		if derr != nil {
			discordStep.State, discordStep.Detail, discordStep.Action = setupAction, derr.message+".", "RUN_SETUP"
			if derr.code == codeDiscordUnavailable {
				discordStep.State, discordStep.Action = setupWait, ""
			}
		} else {
			discordStep.State, discordStep.Detail = setupOK, "#"+dest.ChannelName+" is private to staff. Give your staff roles access to the C.A.S.E. category."
		}
	}
	out.Steps = append(out.Steps, discordStep)

	// 2. Game log feed.
	feed := caseSetupStep{Key: "feed", Label: "Game log feed"}
	a.presenceMu.Lock()
	engine := a.presenceEngines[serverID]
	a.presenceMu.Unlock()
	if engine == nil {
		feed.State, feed.Detail = setupWait, "Champion isn't reading this server's logs right now. Check your Nitrado connection in Settings; it starts again on its own."
	} else {
		feed.State, feed.Detail = caseFeedStep(killfeed.ClassifyADMSourceHealth(engine.SourceHealth(), time.Now()))
	}
	out.Steps = append(out.Steps, feed)

	// 3. Server clock.
	clock := caseSetupStep{Key: "clock", Label: "Server clock"}
	var offset int
	err := a.DB.Pool.QueryRow(ctx, `SELECT utc_offset_minutes FROM live_sync_server_clock WHERE guild_id=$1 AND server_id=$2`,
		ac.scope.GuildID, serverID).Scan(&offset)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		clock.State, clock.Detail = setupWait, "Champion learns your server's clock from its restart log. This happens by itself after the next server restart."
	case err != nil:
		slog.Warn("component=case", "event", "setup_clock_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the anti-cheat setup")
		return
	default:
		clock.State, clock.Detail = setupOK, "Learned from your server's restart log, so login and session times can be trusted."
	}
	out.Steps = append(out.Steps, clock)

	// 4. Evidence collection.
	choice, err := repository.NewCaseEvidenceOptinRepository(a.DB.Pool).Get(ctx, ac.scope.InstallationID)
	if err != nil {
		slog.Warn("component=case", "event", "setup_optin_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the anti-cheat setup")
		return
	}
	ev := caseSetupEvidence{Running: caseCollectorAttached(serverID), Configured: caseEvidenceEnabledForServer(serverID),
		OwnerChoice: choice.Enabled, SelfServe: caseEvidenceSelfServe()}
	_, ev.SetByChampions = caseFlags.Overrides(ac.scope.InstallationID)[featureflags.CaseEvidence]
	ev.PendingRestart = ev.Configured != ev.Running
	out.Evidence = ev
	out.Steps = append(out.Steps, caseEvidenceStep(ev))

	// 5. Staff alerts switch.
	alerts := caseSetupStep{Key: "alerts", Label: "Staff alerts", Action: "TOGGLE_ALERTS"}
	settings, err := repository.NewCaseStaffAlertRepository(a.DB.Pool).GetSettings(ctx, ac.scope.InstallationID, ac.scope.GuildID, serverID)
	if err != nil {
		slog.Warn("component=case", "event", "setup_alerts_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the anti-cheat setup")
		return
	}
	if settings.Enabled {
		alerts.State, alerts.Detail = setupOK, "On. When a detector is ready, its alerts go to your private staff channel."
	} else {
		alerts.State, alerts.Detail = setupAction, "Off. Turn it on so you're told when a detector is ready and finds something."
	}
	out.Steps = append(out.Steps, alerts)

	// 6. Detectors.
	released := 0
	for _, d := range caseintel.ClientCatalog() {
		rel := caseintel.ModuleReleased(d.ID)
		if rel {
			released++
		}
		out.Detectors = append(out.Detectors, caseSetupDetector{ID: d.ID, Name: d.Name, Released: rel})
	}
	det := caseSetupStep{Key: "detectors", Label: "Detectors"}
	if released == 0 {
		det.State, det.Detail = setupTesting, "Champions is still testing the detectors on real servers. They'll switch on here when they're ready; there's nothing for you to do."
	} else {
		det.State, det.Detail = setupOK, "Some detectors are ready. They use your evidence and alert settings above."
	}
	out.Steps = append(out.Steps, det)

	// Base Boosting (docs/CASE_BASE_SHADOW_REVIEW.md "Setup checklist").
	now := time.Now().UTC()
	ready, err := repository.NewCaseBaseRegistrationRepository(a.DB.Pool).BaseBoostReadiness(ctx, ac.scope.InstallationID, ac.scope.GuildID, serverID, now.Add(-baseBoostLookback))
	if err != nil {
		slog.Warn("component=case", "event", "setup_base_boost_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the anti-cheat setup")
		return
	}
	review, _, err := repository.NewCaseShadowVerdictRepository(a.DB.Pool).Summary(ctx,
		repository.CaseVerdictScope{InstallationID: ac.scope.InstallationID, GuildID: ac.scope.GuildID, ServerID: serverID}, baseBoostDetectorID, 1)
	if err != nil {
		slog.Warn("component=case", "event", "setup_base_review_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the anti-cheat setup")
		return
	}
	buildOn := caseBuildEvidenceEnabledForServer(serverID)
	out.BaseBoost.Steps = caseBaseBoostSteps(buildOn, buildOn && caseCollectorAttached(serverID), ready, review,
		caseintel.ModuleReleased(baseBoostDetectorID), now)
	writeSaaSJSON(w, http.StatusOK, out)
}

// caseBaseBoostSteps turns Base Boosting readiness into owner steps.
func caseBaseBoostSteps(buildConfigured, buildRunning bool, ready repository.BaseBoostReadiness, review repository.CaseVerdictSummary, released bool, now time.Time) []caseSetupStep {
	steps := make([]caseSetupStep, 0, 4)

	build := caseSetupStep{Key: "buildEvidence", Label: "Build evidence"}
	switch {
	case buildRunning:
		build.State, build.Detail = setupOK, "On. Champion keeps build and placement lines from your game log for the Base Boosting test."
	case buildConfigured:
		build.State, build.Detail = setupWait, "On. It starts after Champion's next restart (usually within a day)."
	default:
		build.State, build.Detail = setupLocked, "Off. Champions turns this on for servers taking part in the Base Boosting test. Contact support if you'd like to take part."
	}
	steps = append(steps, build)

	lines := caseSetupStep{Key: "buildLines", Label: "Build lines from your game log"}
	switch {
	case !buildRunning:
		lines.State, lines.Detail = setupWait, "Checked once build evidence is on."
	case ready.BuildLines > 0:
		lines.State, lines.Detail = setupOK, fmt.Sprintf("%d build or placement %s in the last 7 days.", ready.BuildLines, plural(ready.BuildLines, "line", "lines"))
	default:
		lines.State = setupAction
		lines.Detail = "None in the last 7 days. In your Nitrado server settings, turn on admin logging of build actions and placement (adminLogBuildActions and adminLogPlacement), then restart the server. If they're already on, nobody has built anything yet."
		if ready.LastBuildAt != nil {
			days := int(now.Sub(*ready.LastBuildAt).Hours() / 24)
			lines.Detail = fmt.Sprintf("None in the last 7 days; the last one was %d days ago. Check that build and placement logging (adminLogBuildActions, adminLogPlacement) are still on in your Nitrado server settings.", days)
		}
	}
	steps = append(steps, lines)

	bases := caseSetupStep{Key: "bases", Label: "Registered bases", Action: "REGISTER_BASES"}
	if ready.Bases > 0 {
		bases.State, bases.Detail = setupOK, fmt.Sprintf("%d %s registered. Only building inside a registered base, after it was registered, is checked.", ready.Bases, plural(ready.Bases, "base", "bases"))
	} else {
		bases.State, bases.Detail = setupAction, "None yet. Register your players' bases on the Bases tab; Base Boosting only checks building inside a registered base."
	}
	steps = append(steps, bases)

	rev := caseSetupStep{Key: "baseReview", Label: "Base Boosting review"}
	switch {
	case released:
		rev.State, rev.Detail = setupOK, "Base Boosting is on. Its alerts go to your private staff channel."
	case review.Reviewed == 0:
		rev.State, rev.Detail = setupTesting, "In testing. Flags appear on the Bases tab; mark each one False alarm or Looks suspicious. About 30 over 14 days with few false alarms is enough to switch it on."
	default:
		rev.State = setupTesting
		rev.Detail = fmt.Sprintf("In testing. %d reviewed, %d false %s, %d %s of reviewing. About 30 over 14 days with few false alarms is enough to switch it on.",
			review.Reviewed, review.FalseAlarms, plural(review.FalseAlarms, "alarm", "alarms"), review.DaysReviewing, plural(review.DaysReviewing, "day", "days"))
	}
	return append(steps, rev)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// caseFeedStep turns the ADM source state into owner words.
func caseFeedStep(state, _ string) (string, string) {
	switch state {
	case killfeed.ADMHealthy:
		return setupOK, "Champion is reading your server's game log."
	case killfeed.ADMQuiet:
		return setupWait, "Connected, but nothing new is being written. This is normal when nobody is playing; it fills in as players join."
	case killfeed.ADMTransportError:
		return setupAction, "Champion can't download your logs from Nitrado. Check your Nitrado connection in Settings."
	case killfeed.ADMSourceLagging:
		return setupWait, "Your server started a new log that Champion hasn't caught up with yet. It usually catches up within a few minutes."
	case killfeed.ADMWorkerStalled:
		return setupWait, "Champion hasn't checked your logs recently. It restarts on its own; if this lasts more than an hour, contact support."
	}
	return setupWait, "The game log can't be checked right now."
}

func caseEvidenceStep(ev caseSetupEvidence) caseSetupStep {
	step := caseSetupStep{Key: "evidence", Label: "Evidence collection"}
	switch {
	case ev.SetByChampions && ev.Configured:
		step.State, step.Detail = setupOK, "On, set by Champions for your server."
	case ev.SetByChampions:
		step.State, step.Detail = setupLocked, "Off, set by Champions for your server. Contact support to change it."
	case ev.Configured && ev.Running:
		step.State, step.Detail = setupOK, "On. Champion records logins, sessions and other evidence for staff review. Players see nothing."
	case ev.Configured:
		step.State, step.Detail = setupWait, "On. It starts after Champion's next restart (usually within a day)."
	case ev.Running:
		step.State, step.Detail = setupWait, "Turned off. It stops after Champion's next restart. Evidence already kept stays."
	case ev.SelfServe:
		step.State, step.Detail, step.Action = setupAction, "Off. Turn it on to start recording evidence for staff review. It writes more data, so it's off until you choose.", "TOGGLE_EVIDENCE"
	default:
		step.State, step.Detail = setupLocked, "Off. Champions turns this on for servers during the anti-cheat test period. Contact support if you'd like to take part."
	}
	if ev.SelfServe && !ev.SetByChampions && (ev.Configured || ev.OwnerChoice) {
		step.Action = "TOGGLE_EVIDENCE"
	}
	return step
}

type caseEvidenceChoiceRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetCaseEvidence is PUT .../admin/case/setup/evidence {enabled}: the
// owner's own switch. Refused unless the platform allows self-serve.
func (a *App) handleSetCaseEvidence(w http.ResponseWriter, r *http.Request) {
	ac, _, ok := a.caseDetectorSettingsActor(w, r)
	if !ok {
		return
	}
	if !enforceRateLimit(w, a.saasAdminActionLimiter, rateLimitKey(r)) {
		return
	}
	var req caseEvidenceChoiceRequest
	if err := readCaseBaseJSON(w, r, &req); err != nil {
		writeSaaSError(w, codeInvalidRequest, "invalid choice")
		return
	}
	if !caseEvidenceSelfServe() {
		writeSaaSError(w, codeConflict, "evidence collection is turned on by Champions during the test period; contact support")
		return
	}
	if _, set := caseFlags.Overrides(ac.scope.InstallationID)[featureflags.CaseEvidence]; set {
		writeSaaSError(w, codeConflict, "Champions has set evidence collection for this server; contact support to change it")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var actor *int64
	if ac.user != nil {
		actor = &ac.user.ID
	}
	choice, err := repository.NewCaseEvidenceOptinRepository(a.DB.Pool).Set(ctx, ac.scope.InstallationID, *ac.scope.ServerID, req.Enabled, actor)
	if err != nil {
		slog.Warn("component=case", "event", "evidence_optin_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not save your choice")
		return
	}
	a.recordAudit(ctx, ac, "CASE_EVIDENCE_OPTIN_SAVED", "case-evidence", "", "success", nil, map[string]any{"enabled": choice.Enabled})
	writeSaaSJSON(w, http.StatusOK, map[string]any{"choice": choice, "appliesAfterRestart": true})
}
