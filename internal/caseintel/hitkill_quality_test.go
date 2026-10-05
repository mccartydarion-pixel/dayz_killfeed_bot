package caseintel

import (
	"strings"
	"testing"
)

func TestHitKillQualityNeverPromotesObservationToDetector(t *testing.T) {
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	samples := []AdmissibilitySample{
		{EvidenceID: 1, SourceID: "source-a", SourceEndOffset: 120, LineSHA256: hashA, EventType: "PLAYER_HIT", ADMClock: "12:00:00"},
		{EvidenceID: 2, SourceID: "source-a", SourceEndOffset: 240, LineSHA256: hashB, EventType: "PLAYER_KILL", ADMClock: "12:00:00"},
		{EvidenceID: 3, SourceID: "source-b", SourceEndOffset: 120, LineSHA256: hashB, EventType: "PLAYER_RESPAWN", ADMClock: "broken"},
	}
	got, err := AuditHitKillQuality(samples, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.HitObservations != 1 || got.KillObservations != 1 || got.OtherObservations != 1 ||
		got.SourceCount != 2 || !got.WindowTruncated || got.InvalidClockStrings != 1 ||
		got.DetectorStatus != "NOT_VALIDATED" || got.FindingsEnabled || got.Enforcement != "DISABLED" {
		t.Fatalf("unsafe/incorrect quality report: %+v", got)
	}
	if got.Limitations == nil || len(got.Limitations) < 5 {
		t.Fatal("limitations missing")
	}
	// Even repeated hit lines must not become a score, finding or accusation.
	repeated := make([]AdmissibilitySample, 100)
	for i := range repeated {
		repeated[i] = AdmissibilitySample{
			EvidenceID: int64(i + 1), SourceID: "same-source", SourceEndOffset: int64(i + 1),
			LineSHA256: hashA, EventType: "PLAYER_HIT", ADMClock: "13:01:01",
		}
	}
	many, err := AuditHitKillQuality(repeated, 100, false)
	if err != nil || many.HitObservations != 100 || many.DetectorStatus != "NOT_VALIDATED" ||
		many.FindingsEnabled || many.Enforcement != "DISABLED" {
		t.Fatalf("high count incorrectly admitted: %+v %v", many, err)
	}
}
func TestHitKillQualityRejectsUnboundedOrMissingEvidence(t *testing.T) {
	for _, limit := range []int{0, -1, 201} {
		if _, err := AuditHitKillQuality(nil, limit, false); err == nil {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := AuditHitKillQuality([]AdmissibilitySample{{}, {}}, 1, false); err == nil {
		t.Fatal("accepted over-limit sample")
	}
	out, err := AuditHitKillQuality(nil, 10, false)
	if err != nil || out.HitObservations != 0 || out.DetectorStatus != "NOT_VALIDATED" ||
		out.FindingsEnabled || out.Enforcement != "DISABLED" {
		t.Fatalf("empty sample must fail closed: %+v %v", out, err)
	}
}
