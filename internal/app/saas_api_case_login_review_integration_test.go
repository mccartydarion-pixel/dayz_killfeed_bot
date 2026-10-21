//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestCaseLoginReviewChecksRecentPlayers(t *testing.T) {
	w := newClientAdminWorld(t)
	ctx := context.Background()
	repo := repository.NewCaseEvidenceRepository(w.a.DB.Pool)
	source := fmt.Sprintf("dayzps/config/DayZServer_PS4_x64_%s_22-00-00.ADM", time.Now().UTC().Format("2006-01-02"))
	kinds := []string{"PLAYER_CONNECT", "PLAYER_DISCONNECT", "PLAYER_CONNECT", "PLAYER_DISCONNECT", "PLAYER_CONNECT"}
	for i, kind := range kinds {
		x := caseSessionFixture(w.guildID, w.serverID, int64(i+1)*100, source, kind)
		x.ADMClock = fmt.Sprintf("22:%02d:00", 10+i)
		if err := repo.RecordCaseEvidence(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	other := caseSessionFixture(w.guildID, w.serverID, 900, source, "PLAYER_CONNECT")
	other.Subject = repository.CaseEvidencePerson{DayZID: "case-review-second", Name: "Second"}
	other.LineSHA256 = fmt.Sprintf("%064x", 987654)
	if err := repo.RecordCaseEvidence(ctx, other); err != nil {
		t.Fatal(err)
	}
	path := w.path("/case/login-review")
	if rr := w.call(w.a.handleCaseLoginReview, http.MethodGet, path+"?days=30", w.f.OwnerDiscordID, nil, nil); rr.Code != http.StatusBadRequest {
		t.Fatalf("days 30 accepted: %d", rr.Code)
	}
	moderator := zoneActor(t, w, "review-mod")
	w.mapRole(moderator, "review-mod-role", "MODERATOR")
	if rr := w.call(w.a.handleCaseLoginReview, http.MethodGet, path, moderator, nil, nil); rr.Code != http.StatusForbidden {
		t.Fatalf("moderator without location view: %d", rr.Code)
	}
	rr := w.call(w.a.handleCaseLoginReview, http.MethodGet, path+"?days=7", w.f.OwnerDiscordID, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rr.Code, rr.Body.String())
	}
	got := decodeBody[loginReviewResponse](t, rr)
	// Both players are checked; the test world has no live ADM worker, so no
	// conclusions are drawn and nothing is flagged.
	if got.Days != 7 || got.PlayersChecked != 2 || len(got.Findings) != 0 || got.Truncated || got.Findings == nil || got.PausedReasons == nil {
		t.Fatalf("review: %+v", got)
	}
	total := 0
	for _, n := range got.Statuses {
		total += n
	}
	if total != 2 {
		t.Fatalf("statuses: %+v", got.Statuses)
	}
}
