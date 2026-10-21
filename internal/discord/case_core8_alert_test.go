package discord

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/caseintel"
)

func core8Finding(tier caseintel.EvidenceTier) caseintel.Finding {
	at := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	return caseintel.Finding{DetectorID: "CASE-TELEPORT-001", DetectorVersion: "0.1.0",
		Scope: caseintel.Core8Scope{GuildID: 1, InstallationID: 2, ServerID: 3}, PlayerID: 42,
		PlayerName: "@everyone `x`", EvidenceIDs: []int64{10, 11}, EventAt: at, ObservedAt: at.Add(5 * time.Second),
		Behavior: "Moved across the map faster than anyone can run", Explanation: "Moved 2000 m in 10 seconds, about 200 m/s.",
		EvidenceCompleteness: "COMPLETE", ExclusionsChecked: []string{"RESPAWN", "SERVER_RESTART", "VEHICLE", "UNKNOWN_CODE"},
		Tier: tier, IncidentKey: strings.Repeat("ab", 32)}
}

func caseAlertText(t *testing.T, tier caseintel.EvidenceTier, url string) string {
	t.Helper()
	e := BuildCASECore8StaffEmbed(core8Finding(tier), "Teleport Alerts", "Chernarus #1", url)
	if e == nil {
		t.Fatal("embed missing")
	}
	raw, _ := json.Marshal(e)
	text := string(raw) + e.Title + e.Description + e.Footer.Text + " url=" + e.URL
	for _, f := range e.Fields {
		text += "\n" + f.Name + ": " + f.Value
	}
	return text
}

func TestCASECore8StaffEmbedIsPlainEnglishAndNeverAVerdict(t *testing.T) {
	text := caseAlertText(t, caseintel.TierSuspicious, "https://example.com/dashboard/anti-cheat?tab=players&playerId=42")
	for _, want := range []string{"CHAMPIONS® C.A.S.E.", "⚡ Teleporting — needs a staff look", "Moved across the map faster than anyone can run",
		"👤 Player", "🖥️ Server", "Chernarus #1", "🕒 When", "<t:", "📋 Evidence", "2 records · complete",
		"Already ruled out", "Respawning, a server restart, driving or flying", "not proof of cheating",
		"Staff only · No automatic bans · Ref abababab", "url=https://example.com/"} {
		if !strings.Contains(text, want) {
			t.Fatalf("embed omitted %q: %s", want, text)
		}
	}
	for _, bad := range []string{"@everyone", "CASE-TELEPORT-001", "UNKNOWN_CODE", "CONFIRMED CHEATER", "BANNED", "`x`", "retained evidence"} {
		if strings.Contains(text, bad) {
			t.Fatalf("embed contains %q", bad)
		}
	}
	if !strings.Contains(caseAlertText(t, caseintel.TierObserved, ""), "Teleporting — noticed") {
		t.Fatal("observed tier mislabelled")
	}
	if strings.Contains(caseAlertText(t, caseintel.TierObserved, "javascript:alert(1)"), "url=javascript") {
		t.Fatal("non-https link accepted")
	}
	unkeyed := core8Finding(caseintel.TierSuspicious)
	unkeyed.IncidentKey = ""
	if BuildCASECore8StaffEmbed(unkeyed, "Teleport Alerts", "", "") != nil {
		t.Fatal("unvalidated finding rendered")
	}
}
