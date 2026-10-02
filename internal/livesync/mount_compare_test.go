package livesync

import (
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
)

func mounts(noftp, ftproot int64) []nitrado.LogFile {
	var out []nitrado.LogFile
	if noftp >= 0 {
		out = append(out, nitrado.LogFile{Path: "/games/x/noftp/dayzps/config/A.ADM", Size: noftp})
	}
	if ftproot >= 0 {
		out = append(out, nitrado.LogFile{Path: "/games/x/ftproot/dayzps/config/A.ADM", Size: ftproot})
	}
	return out
}

func TestMountCompareTimesTheFollower(t *testing.T) {
	m := newMountCompare()
	t0 := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	if got := m.observe(t0, mounts(100, 100)); len(got) != 0 {
		t.Fatalf("the first sight of a file measures nothing: %+v", got)
	}
	if got := m.observe(t0.Add(20*time.Second), mounts(100, 100)); len(got) != 0 {
		t.Fatalf("no growth, no measurement: %+v", got)
	}
	// noftp shows 60 new bytes; ftproot shows them 40 seconds later.
	if got := m.observe(t0.Add(40*time.Second), mounts(160, 100)); len(got) != 0 {
		t.Fatalf("a lead is reported when the follower catches up, not before: %+v", got)
	}
	if got := m.observe(t0.Add(60*time.Second), mounts(160, 100)); len(got) != 0 {
		t.Fatalf("still behind: %+v", got)
	}
	got := m.observe(t0.Add(80*time.Second), mounts(160, 160))
	if len(got) != 1 || got[0].Leader != mountNoftp || got[0].Size != 160 || got[0].Lag != 40*time.Second || got[0].ID != "dayzps/config/A.ADM" {
		t.Fatalf("unexpected measurement: %+v", got)
	}
	// The other way round.
	m.observe(t0.Add(100*time.Second), mounts(160, 200))
	got = m.observe(t0.Add(120*time.Second), mounts(230, 200))
	if len(got) != 1 || got[0].Leader != mountFtproot || got[0].Lag != 20*time.Second {
		t.Fatalf("ftproot led by one pass: %+v", got)
	}
}

func TestMountCompareTieAndMissingMount(t *testing.T) {
	m := newMountCompare()
	t0 := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	m.observe(t0, mounts(100, 100))
	got := m.observe(t0.Add(20*time.Second), mounts(150, 150))
	if len(got) != 1 || got[0].Leader != "tie" || got[0].Lag != 0 {
		t.Fatalf("both grew in the same pass: %+v", got)
	}
	// A file listed in only one mount is not compared, and one that disappears is forgotten.
	if got := m.observe(t0.Add(40*time.Second), mounts(170, -1)); len(got) != 0 {
		t.Fatalf("one mount only: %+v", got)
	}
	if got := m.observe(t0.Add(60*time.Second), nil); len(got) != 0 || len(m.size) != 0 || len(m.pending) != 0 {
		t.Fatalf("a file gone from both listings is forgotten: %+v %d %d", got, len(m.size), len(m.pending))
	}
	if mountOf("/games/x/other/A.ADM") != "" {
		t.Fatal("a path outside both mounts has no mount")
	}
}

func TestSummarizeMounts(t *testing.T) {
	mod := time.Date(2026, 10, 2, 19, 30, 0, 0, time.UTC)
	files := []nitrado.LogFile{
		// The live boot's logs are listed by noftp only.
		{Path: "/games/x/noftp/dayzps/config/DayZServer_PS4_x64_2026-10-02_15-32-05.ADM", Size: 900, Modified: mod},
		{Path: "/games/x/noftp/dayzps/config/DayZServer_PS4_x64_2026-10-02_15-32-05.RPT", Size: 4000, Modified: mod},
		// An older boot is in both.
		{Path: "/games/x/noftp/dayzps/config/DayZServer_PS4_x64_2026-10-02_14-23-52.RPT", Size: 500},
		{Path: "/games/x/ftproot/dayzps/config/DayZServer_PS4_x64_2026-10-02_14-23-52.RPT", Size: 500},
		// And one file only ftproot has.
		{Path: "/games/x/ftproot/dayzps/config/server.log", Size: 7},
		{Path: "/games/x/elsewhere/ignored.ADM", Size: 1},
	}
	got := summarizeMounts(files)
	if got.Both != 1 || got.NoftpOnly != 2 || got.FtprootOnly != 1 {
		t.Fatalf("counts: %+v", got)
	}
	if len(got.Newest) != 2 {
		t.Fatalf("newest: %+v", got.Newest)
	}
	adm, rpt := got.Newest[0], got.Newest[1]
	if adm.Family != FamilyADM || adm.Noftp != 900 || adm.Ftproot != -1 || !adm.NoftpModified.Equal(mod) {
		t.Fatalf("the live ADM is in noftp only: %+v", adm)
	}
	if rpt.Family != FamilyRPT || rpt.ID != "dayzps/config/DayZServer_PS4_x64_2026-10-02_15-32-05.RPT" || rpt.Noftp != 4000 || rpt.Ftproot != -1 {
		t.Fatalf("the newest RPT is the live boot's: %+v", rpt)
	}
	if s := summarizeMounts(nil); s.Both != 0 || len(s.Newest) != 0 {
		t.Fatalf("empty: %+v", s)
	}
}
