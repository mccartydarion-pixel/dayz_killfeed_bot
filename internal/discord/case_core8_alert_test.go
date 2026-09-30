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
		Behavior: "Position change faster than on-foot movement", Explanation: "2000 m in 10s",
		EvidenceCompleteness: "COMPLETE", ExclusionsChecked: []string{"RESPAWN", "VEHICLE"},
		Tier: tier, IncidentKey: strings.Repeat("ab", 32)}
}

func TestCASECore8StaffEmbedShowsRequiredFieldsWithoutVerdict(t *testing.T) {
	embed := BuildCASECore8StaffEmbed(core8Finding(caseintel.TierSuspicious), "Teleport Alerts", "Chernarus #1")
	if embed == nil {
		t.Fatal("embed missing")
	}
	raw, _ := json.Marshal(embed)
	text := string(raw) + embed.Title + embed.Footer.Text
	for _, f := range embed.Fields {
		text += "\n" + f.Name + ": " + f.Value
	}
	for _, want := range []string{"CHAMPIONS® C.A.S.E.", "Teleport Alerts", "Detection type", "Player", "Affected server",
		"Chernarus #1", "Observed behavior", "Supporting evidence", "2 retained evidence record(s)", "Event time", "<t:",
		"Investigation status", "Not a confirmed violation", "STAFF ONLY", "NO AUTOMATIC ENFORCEMENT"} {
		if !strings.Contains(text, want) {
			t.Fatalf("embed omitted %q: %s", want, text)
		}
	}
	for _, bad := range []string{"@everyone", "CONFIRMED CHEATER", "BANNED", "`x`"} {
		if strings.Contains(text, bad) {
			t.Fatalf("embed contains %q", bad)
		}
	}
	if !strings.Contains(BuildCASECore8StaffEmbed(core8Finding(caseintel.TierObserved), "Teleport Alerts", "").Fields[7].Value, "Observed activity") {
		t.Fatal("observed tier mislabelled")
	}
	unkeyed := core8Finding(caseintel.TierSuspicious)
	unkeyed.IncidentKey = ""
	if BuildCASECore8StaffEmbed(unkeyed, "Teleport Alerts", "") != nil {
		t.Fatal("unvalidated finding rendered")
	}
}
