package livesync_test

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/livesync"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

// Requirement 9: a stalled RPT watcher cannot block ADM ingestion. The real killfeed engine and the
// live sync supervisor share one Nitrado remote (as in production); every RPT download hangs.

type sharedRemote struct {
	mu       sync.Mutex
	files    map[string][]byte
	rptReads atomic.Int64
}

func (s *sharedRemote) ListLogs(_ context.Context, _ string) ([]nitrado.LogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []nitrado.LogFile
	for p, c := range s.files {
		if strings.HasSuffix(p, ".ADM") {
			out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: path.Dir(p), Size: int64(len(c)), Modified: time.Now(), Type: "ADM"})
		}
	}
	return out, nil
}

func (s *sharedRemote) ListDir(_ context.Context, _ string, dir string) ([]nitrado.LogFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []nitrado.LogFile
	for p, c := range s.files {
		if path.Dir(p) == dir {
			out = append(out, nitrado.LogFile{Name: path.Base(p), Path: p, Directory: dir, Size: int64(len(c)), Modified: time.Now()})
		}
	}
	return out, nil
}

func (s *sharedRemote) ReadLog(ctx context.Context, _ string, p string) ([]byte, error) {
	if strings.HasSuffix(p, ".RPT") {
		s.rptReads.Add(1)
		<-ctx.Done() // the RPT download never completes
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.files[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return append([]byte(nil), c...), nil
}

func (s *sharedRemote) appendTo(p, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = append(append([]byte(nil), s.files[p]...), line...)
}

type nopStore struct{}

func (nopStore) LoadSources(context.Context, int64, int64) ([]livesync.SourceState, error) {
	return nil, nil
}
func (nopStore) CommitSource(_ context.Context, _, _ int64, _ livesync.SourceState, recs []livesync.StoredRecord) (int, error) {
	return len(recs), nil
}
func (nopStore) SetServerUTCOffset(context.Context, int64, int64, int, string) error { return nil }

func TestStalledRPTWatcherCannotBlockADM(t *testing.T) {
	t.Setenv("NITRADO_POLL_INTERVAL", "1s")
	const dir = "/games/svc_2/noftp/dayzps/config"
	adm := dir + "/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"
	remote := &sharedRemote{files: map[string][]byte{
		adm: []byte("AdminLog started on 2026-09-24 at 04:15:07\n"),
		dir + "/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT": []byte("Current time:  2026/09/24 04:15:07\n"),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sup := livesync.NewSupervisor(livesync.Config{
		GuildID: 7, ServerID: 1, ServiceID: "svc", ListEvery: 20 * time.Millisecond, Tick: 10 * time.Millisecond,
		OpTimeout: 30 * time.Second, // a long stall: the RPT family is stuck inside ReadLog
		Policies:  []livesync.FamilyPolicy{{Family: livesync.FamilyRPT, ProbeEvery: 20 * time.Millisecond}},
	}, remote, nopStore{}, nil)
	supDone := make(chan struct{})
	go func() { defer close(supDone); sup.Run(ctx) }()

	var admOffset atomic.Int64
	engine := killfeed.NewEngine(remote, "svc", killfeed.NewADMParser())
	engine.OnDownload(func(r killfeed.DownloadReport) {
		if r.Result == "success" && r.NewOffset > admOffset.Load() {
			admOffset.Store(r.NewOffset)
		}
	})
	engDone := make(chan struct{})
	go func() { defer close(engDone); _ = engine.Start(ctx) }()

	deadline := time.Now().Add(20 * time.Second)
	for remote.rptReads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if remote.rptReads.Load() == 0 {
		t.Fatal("the RPT watcher never started its (stalled) read")
	}
	// While the RPT read hangs, ADM lines keep arriving and must be ingested.
	line := "04:20:00 | Player \"Ceiyxe\" (id=AAAA= pos=<4621.1, 8397.2, 319.6>) is connected\n"
	remote.appendTo(adm, line)
	want := int64(len("AdminLog started on 2026-09-24 at 04:15:07\n") + len(line))
	for admOffset.Load() < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := admOffset.Load(); got < want {
		t.Fatalf("ADM ingestion stalled behind the RPT watcher: offset %d, want %d", got, want)
	}
	cancel()
	<-engDone
	<-supDone
}
