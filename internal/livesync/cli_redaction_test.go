package livesync

import (
	"os"
	"strings"
	"testing"
)

// Live Sync phase 2.1: no part of an RPT's executable path / command-line header is stored - not
// the port, config file, profiles path, service directory or any argument.
func TestRPTCommandLineHeaderIsNeverStored(t *testing.T) {
	content, err := os.ReadFile("testdata/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "-port=") {
		t.Fatal("fixture must contain a real command line")
	}
	res, _ := ParseRPT(content, 0, parseStamp("2026-09-24_04-15-07"))
	commandLines := 0
	for _, r := range res.Records {
		stored := r.Evidence
		for _, v := range r.Payload {
			stored += " " + v
		}
		for _, bad := range []string{"-port=", "-ip=", "-config=", "-profiles=", "serverDZ", ".exe", "SERVICES", "_local", "15200"} {
			if strings.Contains(stored, bad) {
				t.Fatalf("record at %d stores %q: %q", r.Offset, bad, stored)
			}
		}
		if r.Payload["redacted"] == "command_line" {
			commandLines++
			if r.Evidence != "" || r.Category != CategoryLogHeader {
				t.Fatalf("command-line header keeps no evidence: %+v", r)
			}
		}
	}
	if commandLines != 2 { // "== <exe path>" and "== <exe> <arguments>"
		t.Fatalf("both command-line header lines are recorded without content, got %d", commandLines)
	}
	// Separator lines and the build timestamp are still kept.
	if res.Stats[CategoryLogHeader] < 4 {
		t.Fatalf("other header lines kept: %v", res.Stats)
	}
}
