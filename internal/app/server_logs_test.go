package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/permissions"
)

type fakeLogSource struct {
	files     []nitrado.LogFile
	content   map[string][]byte // by path
	seekFails bool
	seeks     int
	fullReads int
	paths     []string
}

func (f *fakeLogSource) ConfigDir(context.Context, string) (string, error) {
	return "/games/ni1_1/noftp/dayzps/config", nil
}

func (f *fakeLogSource) ListDir(context.Context, string, string) ([]nitrado.LogFile, error) {
	return f.files, nil
}

func (f *fakeLogSource) ReadLogRange(_ context.Context, _ string, path string, offset, length int64) (*nitrado.PartialReadResult, bool) {
	f.seeks++
	f.paths = append(f.paths, path)
	data, ok := f.content[path]
	if f.seekFails || !ok || offset+length > int64(len(data)) {
		return nil, false
	}
	return &nitrado.PartialReadResult{Data: data[offset : offset+length], StartOffset: offset, EndOffset: offset + length}, true
}

func (f *fakeLogSource) ReadRemoteFile(_ context.Context, _ string, path string) ([]byte, error) {
	f.fullReads++
	f.paths = append(f.paths, path)
	data, ok := f.content[path]
	if !ok {
		return nil, errors.New("not found")
	}
	return data, nil
}

func logFile(name string, size int64) nitrado.LogFile {
	return nitrado.LogFile{Name: name, Path: "/games/ni1_1/noftp/dayzps/config/" + name, Size: size}
}

func TestServerLogsCapabilityIsOwnerOnly(t *testing.T) {
	if permissions.Allows(permissions.LevelAdministrator, permissions.CapServerLogsView) {
		t.Fatal("an administrator must not read server logs")
	}
	if !permissions.Allows(permissions.LevelOwner, permissions.CapServerLogsView) {
		t.Fatal("the owner must be able to read server logs")
	}
}

func TestServerLogKindAcceptsOnlyServerLogNames(t *testing.T) {
	for name, want := range map[string]string{
		"DayZServer_PS4_x64_2026-10-09_22-33-25.ADM":        "adm",
		"DayZServer_PS4_x64_2026-10-09_22-33-25.RPT":        "rpt",
		"script_2026-10-09_22-33-29.log":                    "script",
		"crash_2026-10-09_22-33-29.log":                     "crash",
		"serverDZ.cfg":                                      "",
		"ban.txt":                                           "",
		"../crash_2026-10-09_22-33-29.log":                  "",
		"/etc/passwd":                                       "",
		"https://example.com/crash_2026-10-09_22-33-29.log": "",
		"": "",
	} {
		if got := serverLogKind(name); got != want {
			t.Errorf("serverLogKind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestListServerLogsKeepsLogsNewestStartFirst(t *testing.T) {
	src := &fakeLogSource{files: []nitrado.LogFile{
		logFile("DayZServer_PS4_x64_2026-10-09_22-33-25.ADM", 10),
		logFile("serverDZ.cfg", 10),
		logFile("crash_2026-10-09_22-33-29.log", 10),
		logFile("DayZServer_PS4_x64_2026-10-10_09-17-46.ADM", 10),
		logFile("whitelist.txt", 10),
	}}
	files, err := listServerLogs(context.Background(), src, "1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := "DayZServer_PS4_x64_2026-10-10_09-17-46.ADM,crash_2026-10-09_22-33-29.log,DayZServer_PS4_x64_2026-10-09_22-33-25.ADM"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("files = %s\nwant    %s", got, want)
	}
}

func TestReadServerLogWindowReadsTheTailInWholeLines(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n" // 100 bytes
	body := []byte(strings.Repeat(line, 20000))
	f := logFile("script_2026-10-09_22-33-29.log", int64(len(body)))
	src := &fakeLogSource{content: map[string][]byte{f.Path: body}}

	got, err := readServerLogWindow(context.Background(), src, "1", f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.End != f.Size || !got.HasEarlier {
		t.Fatalf("end=%d hasEarlier=%v, want the end of the file with more above", got.End, got.HasEarlier)
	}
	if got.Start%100 != 0 || got.Start < f.Size-serverLogWindowBytes {
		t.Fatalf("start=%d is not on a line boundary inside the window", got.Start)
	}
	if int64(len(got.Text)) != got.End-got.Start || !strings.HasPrefix(got.Text, "xxx") {
		t.Fatalf("text is %d bytes for range %d..%d", len(got.Text), got.Start, got.End)
	}
	if src.seeks != 4 || src.fullReads != 0 {
		t.Fatalf("seeks=%d fullReads=%d, want 4 chunk reads and no download", src.seeks, src.fullReads)
	}

	// Reading above the first window continues exactly where it began.
	earlier, err := readServerLogWindow(context.Background(), src, "1", f, got.Start)
	if err != nil {
		t.Fatal(err)
	}
	if earlier.End != got.Start || earlier.Start >= earlier.End {
		t.Fatalf("earlier window %d..%d does not end at %d", earlier.Start, earlier.End, got.Start)
	}
}

func TestReadServerLogWindowFallsBackToTheWholeFile(t *testing.T) {
	body := []byte("first\nsecond\nthird\n")
	f := logFile("crash_2026-10-09_22-33-29.log", int64(len(body)))
	src := &fakeLogSource{content: map[string][]byte{f.Path: body}, seekFails: true}
	got, err := readServerLogWindow(context.Background(), src, "1", f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != string(body) || got.HasEarlier || got.Start != 0 {
		t.Fatalf("got %+v", got)
	}
	for _, p := range src.paths {
		if p != f.Path {
			t.Fatalf("read path %q, want only the listed path", p)
		}
	}
}

func TestReadServerLogWindowRefusesAHugeFileWithoutSeek(t *testing.T) {
	f := logFile("DayZServer_PS4_x64_2026-10-09_22-33-25.RPT", serverLogFullReadMax+1)
	src := &fakeLogSource{seekFails: true}
	if _, err := readServerLogWindow(context.Background(), src, "1", f, 0); !errors.Is(err, errServerLogTooLarge) {
		t.Fatalf("err = %v, want errServerLogTooLarge", err)
	}
	if src.fullReads != 0 {
		t.Fatal("a file over the limit must not be downloaded")
	}
}

func TestRedactServerLog(t *testing.T) {
	in := strings.Join([]string{
		"== /games/ni13295416_2/noftp/dayzps/DayZServer -ip=203.0.113.9 -port=2302",
		"=====================================================================",
		"Version 1.28.160.593",
		"Player \"Bob\" (id=ABC123 pos=<100.0, 200.0, 3.0>) connected from 203.0.113.9:2304",
		"passwordAdmin = \"hunter2\";",
		"Token: abc.def",
	}, "\r\n")
	got := redactServerLog(in)
	for _, leaked := range []string{"203.0.113.9", "ni13295416_2", "hunter2", "abc.def", "-port"} {
		if strings.Contains(got, leaked) {
			t.Errorf("redacted text still holds %q:\n%s", leaked, got)
		}
	}
	for _, kept := range []string{"Version 1.28.160.593", "id=ABC123", "pos=<100.0, 200.0, 3.0>", "====================", "passwordAdmin = [hidden]"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted text lost %q:\n%s", kept, got)
		}
	}
}
