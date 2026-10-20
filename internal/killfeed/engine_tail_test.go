package killfeed

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// tailLogSource adds verified-tail partial reads (nitrado.TailReader) to fakeLogSource.
type tailLogSource struct {
	fakeLogSource
	tailCalls int
	pastEnd   int // reads that asked for bytes past the end of the file
	corrupt   bool
}

// ReadLogRange behaves like Nitrado's seek: asking for bytes past the end of the file fails.
func (f *tailLogSource) ReadLogRange(_ context.Context, _ string, _ string, offset, length int64) (*nitrado.PartialReadResult, bool) {
	f.tailCalls++
	if offset < 0 || offset+length > int64(len(f.content)) {
		f.pastEnd++
		return nil, false
	}
	data := append([]byte(nil), f.content[offset:offset+length]...)
	if len(data) > 100 {
		data = data[:100]
	}
	if f.corrupt && len(data) > 1 {
		data[len(data)-1] = '#'
	}
	return &nitrado.PartialReadResult{Data: data, RequestedOffset: offset, StartOffset: offset, EndOffset: offset + int64(len(data)), Method: "FAKE_SEEK"}, true
}

func newTailEngine(serviceID string, corrupt bool) (*Engine, *tailLogSource) {
	content := "AdminLog started\n"
	fake := &tailLogSource{fakeLogSource: fakeLogSource{
		logs:    []nitrado.LogFile{{Name: "DayZServer_x64.ADM", Path: "/logs/DayZServer_x64.ADM", Size: int64(len(content)), Modified: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), Type: "ADM"}},
		content: []byte(content),
	}, corrupt: corrupt}
	e := NewEngine(fake, serviceID, oracleParser{})
	return e, fake
}

func (f *tailLogSource) grow(line string) {
	f.content = append(f.content, line...)
	f.logs[0].Size = int64(len(f.content))
	f.logs[0].Modified = f.logs[0].Modified.Add(time.Second)
}

func TestADMVerifiedTailReadsAfterTrust(t *testing.T) {
	e, fake := newTailEngine("svc-adm-tail", false)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // discovery + first (full) read
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lines := int64(1)
	for i := 1; i <= 30; i++ {
		fake.grow(fmt.Sprintf("12:00:%02d | Player \"p%d\" hit by something long enough to span chunks\n", i%60, i))
		lines++
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if e.linesDiscovered != lines {
			t.Fatalf("after append %d: every line exactly once, got %d want %d", i, e.linesDiscovered, lines)
		}
	}
	if !nitrado.TailTrust()["svc-adm-tail"].Trusted {
		t.Fatal("the service should be trusted after matching verifications")
	}
	fullBefore := fake.reads
	for i := 31; i <= 40; i++ {
		fake.grow(fmt.Sprintf("12:01:%02d | Player \"p%d\" hit\n", i%60, i))
		lines++
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if e.linesDiscovered != lines {
		t.Fatalf("tail reads lost or repeated lines: %d want %d", e.linesDiscovered, lines)
	}
	if fake.reads-fullBefore > 1 {
		t.Fatalf("a trusted service must stop downloading the whole ADM (one periodic recheck allowed), got %d", fake.reads-fullBefore)
	}
	if fake.pastEnd != 0 {
		t.Fatalf("%d partial reads asked for bytes past the end of the file", fake.pastEnd)
	}
}

func TestADMBrokenPartialReadsAreDisabled(t *testing.T) {
	e, fake := newTailEngine("svc-adm-bad", true)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	lines := int64(1)
	for i := 1; i <= 5; i++ {
		fake.grow(fmt.Sprintf("12:00:%02d | Player \"p%d\" hit\n", i, i))
		lines++
		if err := e.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st := nitrado.TailTrust()["svc-adm-bad"]; !st.Disabled || st.Trusted {
		t.Fatalf("mismatching partial reads must disable tail reads: %+v", st)
	}
	if e.linesDiscovered != lines {
		t.Fatalf("full reads must still deliver every line: %d want %d", e.linesDiscovered, lines)
	}
}
