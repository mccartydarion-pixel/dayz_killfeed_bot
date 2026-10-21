package app

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Login-check review list (docs/CASE_LOGIN_SHADOW_REVIEW.md): the same
// read-only Suspicious Logins test run the Players tab does for one player,
// over everyone who connected recently, so staff can mark every flag in one
// place. It sends nothing, cannot notify and never acts on a player.

const (
	loginReviewMaxPlayers  = 60
	loginReviewMaxFindings = 200
)

type loginReviewFinding struct {
	IncidentKey string    `json:"incidentKey"`
	PlayerID    int64     `json:"playerId"`
	PlayerName  string    `json:"playerName,omitempty"`
	EventAt     time.Time `json:"eventAt"`
	Behavior    string    `json:"behavior"`
	Explanation string    `json:"explanation"`
	EvidenceIDs []int64   `json:"evidenceIds"`
	Tier        string    `json:"tier"`
}

type loginReviewResponse struct {
	Days           int                  `json:"days"`
	PlayersChecked int                  `json:"playersChecked"`
	Statuses       map[string]int       `json:"statuses"`
	Paused         bool                 `json:"paused"`
	PausedReasons  []string             `json:"pausedReasons"`
	Findings       []loginReviewFinding `json:"findings"`
	Truncated      bool                 `json:"truncated"`
}

// handleCaseLoginReview is GET .../admin/case/login-review?days=1..14.
func (a *App) handleCaseLoginReview(w http.ResponseWriter, r *http.Request) {
	ac, ok := a.requireCapability(w, r, permissions.CapPlayerLocationView)
	if !ok {
		return
	}
	if ac.scope.ServerID == nil {
		writeSaaSError(w, codeInvalidRequest, "no DayZ server selected")
		return
	}
	if a.DB == nil || a.DB.Pool == nil {
		writeSaaSError(w, codeInternalError, "C.A.S.E. review unavailable")
		return
	}
	if !enforceRateLimit(w, a.saasAdminReadLimiter, rateLimitKey(r)) {
		return
	}
	days := 7
	if raw := r.URL.Query().Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 14 {
			writeSaaSError(w, codeInvalidRequest, "days must be 1 to 14")
			return
		}
		days = n
	}
	serverID := *ac.scope.ServerID
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	now := time.Now().UTC()
	players, err := repository.NewCaseStaffAlertRepository(a.DB.Pool).RecentSessionPlayers(ctx, ac.scope.GuildID, serverID, now.AddDate(0, 0, -days), loginReviewMaxPlayers+1)
	if err != nil {
		slog.Warn("component=case", "event", "login_review_players_failed", "err", err.Error())
		writeSaaSError(w, codeInternalError, "could not load the login review")
		return
	}
	out := loginReviewResponse{Days: days, Statuses: map[string]int{}, PausedReasons: []string{}, Findings: []loginReviewFinding{}}
	if len(players) > loginReviewMaxPlayers {
		players, out.Truncated = players[:loginReviewMaxPlayers], true
	}
	repo := repository.NewCaseEvidenceRepository(a.DB.Pool)
	offset, err := repo.CaseServerUTCOffset(ctx, ac.scope.GuildID, serverID)
	if err != nil {
		offset = nil // fail closed: no trusted time, no conclusions
	}
	telemetry := a.caseADMTelemetry(serverID, now)
	retained := caseEvidenceEnabledForServer(serverID)
	reasons := map[string]bool{}
	for _, playerID := range players {
		events, next, err := repo.ListCaseSessionEvidence(ctx, ac.scope.GuildID, serverID, playerID, nil, 200)
		if err != nil {
			slog.Warn("component=case", "event", "login_review_player_failed", "player_id", playerID, "err", err.Error())
			writeSaaSError(w, codeInternalError, "could not load the login review")
			return
		}
		name := ""
		for _, e := range events {
			if e.SubjectID != nil && *e.SubjectID == playerID && e.SubjectName != "" {
				name = e.SubjectName
				break
			}
		}
		report := caseintel.ShadowSuspiciousLogins(caseintel.ShadowLoginInput{
			Scope:    caseintel.Core8Scope{GuildID: ac.scope.GuildID, InstallationID: ac.scope.InstallationID, ServerID: serverID},
			PlayerID: playerID, PlayerName: name, Events: events, UTCOffsetMinutes: offset,
			Telemetry: telemetry, SessionsRetained: retained, WindowTruncated: next != nil,
		})
		out.PlayersChecked++
		out.Statuses[report.Result.Status]++
		if report.Result.Status == "SUSPENDED" {
			out.Paused = true
			for _, reason := range report.Result.Reasons {
				reasons[reason] = true
			}
		}
		for _, f := range report.Result.Findings {
			if f.IncidentKey == "" {
				continue
			}
			out.Findings = append(out.Findings, loginReviewFinding{IncidentKey: f.IncidentKey, PlayerID: playerID, PlayerName: name,
				EventAt: f.EventAt, Behavior: f.Behavior, Explanation: f.Explanation, EvidenceIDs: f.EvidenceIDs, Tier: string(f.Tier)})
		}
	}
	for reason := range reasons {
		out.PausedReasons = append(out.PausedReasons, reason)
	}
	sort.Strings(out.PausedReasons)
	sort.SliceStable(out.Findings, func(i, j int) bool { return out.Findings[i].EventAt.After(out.Findings[j].EventAt) })
	if len(out.Findings) > loginReviewMaxFindings {
		out.Findings, out.Truncated = out.Findings[:loginReviewMaxFindings], true
	}
	a.recordAudit(ctx, ac, "CASE_LOGIN_REVIEW_VIEWED", "", "", "success", nil,
		map[string]any{"players": out.PlayersChecked, "findings": len(out.Findings), "days": days})
	writeSaaSJSON(w, http.StatusOK, out)
}
