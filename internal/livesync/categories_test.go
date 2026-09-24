package livesync

import (
	"os"
	"strings"
	"testing"
)

// Phase 2 categories, from real Champions lines (sanitized excerpts in testdata): the bulk of what
// phase 1 left UNKNOWN in a live RPT, and two restart.log line kinds.
func TestPhase2CategoriesOnRealLines(t *testing.T) {
	rpt, err := os.ReadFile("testdata/rpt_excerpt_2026-09-24_05-51-41.txt")
	if err != nil {
		t.Fatal(err)
	}
	stamp := parseStamp("2026-09-24_05-51-41")
	res, consumed := ParseRPT(rpt, 0, stamp)
	if consumed != int64(len(rpt)) {
		t.Fatalf("consumed %d of %d", consumed, len(rpt))
	}
	want := map[string]int{CategoryEngineStartup: 4, CategoryModelWarning: 5, CategorySpawnerConfig: 1}
	for c, n := range want {
		if res.Stats[c] != n {
			t.Fatalf("%s: got %d want %d (stats %v)", c, res.Stats[c], n, res.Stats)
		}
	}
	if res.Stats[CategoryUnknown] != 0 || len(res.Records) != 10 {
		t.Fatalf("the bare time-of-day line is not an observation; nothing else unknown: %v (%d records)", res.Stats, len(res.Records))
	}
	for _, r := range res.Records {
		if r.Category == CategoryModelWarning && strings.Contains(r.Evidence, ".p3d") && r.Payload["model"] == "" {
			t.Fatalf("model path kept: %+v", r)
		}
		if r.Category == CategorySpawnerConfig && r.Payload["spawner"] != "Infected" {
			t.Fatalf("spawner: %+v", r)
		}
		if r.SourceLocalTime == nil {
			t.Fatalf("time of day on the file's date: %+v", r)
		}
	}

	rl, err := os.ReadFile("testdata/restart_excerpt.log")
	if err != nil {
		t.Fatal(err)
	}
	res, _ = ParseRestartLog(rl, 0)
	if res.Stats[CategoryStopRequested] != 1 || res.Stats[CategoryAutomatedRestart] != 1 || res.Stats[CategoryUnknown] != 1 {
		t.Fatalf("restart.log categories: %v", res.Stats)
	}
	if res.Records[0].Payload["requestedVia"] != "Webinterface" || res.Records[0].SourceUTC == nil {
		t.Fatalf("stop request: %+v", res.Records[0])
	}
}
