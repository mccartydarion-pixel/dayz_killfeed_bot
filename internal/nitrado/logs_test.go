package nitrado

import "testing"

func TestParseLogCandidatesFindsLogFiles(t *testing.T) {
	payload := []byte(`{
		"files": [
			{"name": "DayZServer_x64.ADM", "path": "/logs/DayZServer_x64.ADM", "size": 1024, "modified": "2026-08-01T12:00:00Z"},
			{"name": "crash.log", "path": "/logs/crash.log", "size": 512, "modified": "2026-08-01T12:05:00Z"}
		]
	}`)

	entries, err := parseLogCandidates(payload)
	if err != nil {
		t.Fatalf("parseLogCandidates returned unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 log candidates, got %d", len(entries))
	}
	if entries[0].Type == "" {
		t.Fatal("expected a log type to be inferred")
	}
}
