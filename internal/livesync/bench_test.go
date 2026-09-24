package livesync

import (
	"bytes"
	"os"
	"testing"
)

// BenchmarkParseRPTRealSize parses ~420 KB of real RPT lines - the size of a Champions boot's RPT
// on 2026-09-24 (414-562 KB) - which is what one full-download watcher read has to parse at most.
func BenchmarkParseRPTRealSize(b *testing.B) {
	a, err := os.ReadFile("testdata/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
	if err != nil {
		b.Fatal(err)
	}
	ex, err := os.ReadFile("testdata/rpt_excerpt_2026-09-24_05-51-41.txt")
	if err != nil {
		b.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write(a)
	for buf.Len() < 420*1024 {
		buf.Write(ex)
	}
	content := buf.Bytes()
	stamp := parseStamp("2026-09-24_04-15-07")
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseRPT(content, 0, stamp)
	}
}
