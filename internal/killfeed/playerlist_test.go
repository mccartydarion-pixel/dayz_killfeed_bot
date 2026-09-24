package killfeed

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ceiyxeADM is the real Champions ADM session (id redacted) the owner captured on 2026-09-24:
// one connect, then five-minute player-list snapshots, the last four at the same spot.
const ceiyxeADM = `AdminLog started on 2026-09-24 at 04:15:07
04:16:29 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4344.2, 8533.8, 317.4>) is connected
04:20:42 | ##### PlayerList log: 1 players
04:20:42 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4285.9, 8666.3, 318.9>)
04:20:42 | #####
04:35:42 | ##### PlayerList log: 1 players
04:35:42 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4621.1, 8397.2, 319.6>)
04:35:42 | #####
04:40:42 | ##### PlayerList log: 1 players
04:40:42 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4621.1, 8397.2, 319.6>)
04:40:42 | #####
04:45:42 | ##### PlayerList log: 1 players
04:45:42 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4621.1, 8397.2, 319.6>)
04:45:42 | #####
04:50:42 | ##### PlayerList log: 1 players
04:50:42 | Player "Ceiyxe" (id=AAAA_redacted_test_id= pos=<4621.1, 8397.2, 319.6>)
04:50:42 | #####
`

const ceiyxePath = "/games/ni1_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM"

// feed runs ADM text through the engine exactly like a poll: complete lines with absolute end offsets.
func feed(t *testing.T, e *Engine, path, text string, base int64) int64 {
	t.Helper()
	off := base
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		off += int64(len(line))
		if _, err := e.processLineAt(strings.TrimRight(line, "\n"), path, off); err != nil {
			t.Fatal(err)
		}
	}
	return off
}

func TestPlayerListLinesParse(t *testing.T) {
	p := NewADMParser()
	ev, _ := p.ParseLine(`04:35:42 | ##### PlayerList log: 1 players`)
	if ev == nil || ev.Type != EventPlayerListHeader || *ev.PlayerListCount != 1 || ev.TimeOfDay != "04:35:42" {
		t.Fatalf("header: %+v", ev)
	}
	ev, _ = p.ParseLine(`04:35:42 | Player "Ceiyxe" (id=AAAA= pos=<4621.1, 8397.2, 319.6>)`)
	if ev == nil || ev.Type != EventPlayerListEntry || ev.Player.Name != "Ceiyxe" || ev.Player.ID != "AAAA=" {
		t.Fatalf("entry: %+v", ev)
	}
	pos := *ev.Player.Position
	if pos.MapX() != 4621.1 || pos.Altitude() != 319.6 || pos.MapZ() != 8397.2 {
		t.Fatalf("ADM <x, z, altitude> -> x=4621.1 y=319.6 z=8397.2, got %+v", pos)
	}
	ev, _ = p.ParseLine(`04:35:42 | #####`)
	if ev == nil || ev.Type != EventPlayerListFooter {
		t.Fatalf("footer: %+v", ev)
	}
	ev, _ = p.ParseLine(`AdminLog started on 2026-09-24 at 04:15:07`)
	if ev == nil || ev.Type != EventAdminLogStarted || ev.AdminLogStart != "2026-09-24 04:15:07" {
		t.Fatalf("adm header: %+v", ev)
	}
	ev, _ = p.ParseLine(`05:00:00 | ##### PlayerList log: 0 players`)
	if ev == nil || *ev.PlayerListCount != 0 {
		t.Fatalf("empty list header: %+v", ev)
	}
	// Lines that only look similar keep their own meaning.
	for line, want := range map[string]EventType{
		`04:16:29 | Player "Ceiyxe" (id=AAAA= pos=<1, 2, 3>) is connected`: EventPlayerConnect,
		`04:16:29 | Player "Ceiyxe" (id=AAAA= pos=<1, 2, 3>) placed Fence`: EventBuildAction,
	} {
		if ev, _ := p.ParseLine(line); ev == nil || ev.Type != want {
			t.Errorf("%s -> %+v", line, ev)
		}
	}
	for _, line := range []string{
		`04:35:42 | Player "Ceiyxe" (DEAD) (id=AAAA= pos=<1, 2, 3>)`, // not a player-list entry
		`04:35:42 | Player "Ceiyxe" (pos=<1, 2, 3>)`,                 // no id: never an anonymous observation
		`##### PlayerList log: 1 players`,                            // no clock
		`04:35:42 | ##### PlayerList log: many players`,
	} {
		if ev, _ := p.ParseLine(line); ev != nil && (ev.Type == EventPlayerListEntry || ev.Type == EventPlayerListHeader) {
			t.Errorf("%s must not parse as a player list line: %+v", line, ev)
		}
	}
}

func TestCeiyxePlayerListObservationsArePreserved(t *testing.T) {
	store := newFakeLocationStore()
	e := NewEngine(&fakeLogSource{}, "svc-1", NewADMParser())
	e.guildID, e.serverID = 7, 9
	q := NewLocationQueue(store, 7, 9)
	e.SetLocationQueue(q)
	pub := &recordingPublisher{}
	e.SetKillPublisher(pub)
	conn := &recordingConnectionPublisher{}
	e.SetConnectionPublisher(conn)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)

	feed(t, e, ceiyxePath, ceiyxeADM, 0)
	q.Close()
	cancel()

	var lists []string
	for _, r := range store.allRecords() {
		if r.EventType != LocationEventPlayerList {
			continue
		}
		if r.SourceFile != "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM" || r.SourceOffset <= 0 || r.SnapshotRef == "" || r.SourceLocalTime == nil {
			t.Fatalf("sourced observation: %+v", r)
		}
		lists = append(lists, r.SourceLocalTime.Format("15:04:05"))
		if r.SourceLocalTime.Format("15:04:05") == "04:50:42" && (r.X != 4621.1 || *r.Y != 319.6 || r.Z != 8397.2) {
			t.Fatalf("canonical x=4621.1 y=319.6 z=8397.2, got %+v y=%v", r, *r.Y)
		}
	}
	if strings.Join(lists, ",") != "04:20:42,04:35:42,04:40:42,04:45:42,04:50:42" {
		t.Fatalf("every five-minute observation is preserved, stationary ones included: %v", lists)
	}
	// The connect row is sourced too (same file, earlier offset).
	connects := 0
	for _, r := range store.allRecords() {
		if r.EventType == LocationEventConnect && r.SourceFile != "" && r.SourceOffset > 0 {
			connects++
		}
	}
	if connects != 1 {
		t.Fatalf("connect rows: %d", connects)
	}
	if len(pub.kills) != 0 {
		t.Fatal("player-list entries never reach the killfeed")
	}
	if len(conn.notices) != 1 || conn.notices[0].Kind != ConnectionConnected {
		t.Fatalf("only the real connect produces a notice: %+v", conn.notices)
	}
	st := e.PlayerListStats()
	if st.Snapshots != 5 || st.CompleteSnapshots != 5 || st.Entries != 5 || st.LastSnapshotPlayers != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestPlayerListReplayDeduplicatesBySource(t *testing.T) {
	store := newFakeLocationStore()
	e := NewEngine(&fakeLogSource{}, "svc-1", NewADMParser())
	e.guildID, e.serverID = 7, 9
	q := NewLocationQueue(store, 7, 9)
	e.SetLocationQueue(q)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	feed(t, e, ceiyxePath, ceiyxeADM, 0)
	// A replay of the same bytes produces the same (file, offset) identities - the database's
	// source unique index turns them into no-ops. Here we assert identities repeat exactly.
	feed(t, e, ceiyxePath, ceiyxeADM, 0)
	q.Close()
	cancel()
	seen := map[int64]int{}
	for _, r := range store.allRecords() {
		if r.EventType == LocationEventPlayerList {
			seen[r.SourceOffset]++
		}
	}
	if len(seen) != 5 {
		t.Fatalf("five distinct source identities: %v", seen)
	}
	for off, n := range seen {
		if n != 2 {
			t.Fatalf("offset %d replayed %d times with a different identity", off, n)
		}
	}
}

func TestPresenceUsesOnlyCompleteSnapshots(t *testing.T) {
	e := NewEngine(&fakeLogSource{}, "svc-1", NewADMParser())
	changes := 0
	e.OnPlayersChanged(func(int) { changes++ })
	path := ceiyxePath
	// Champion started mid-session: no connect line seen for either player.
	off := feed(t, e, path, `04:20:42 | ##### PlayerList log: 2 players
04:20:42 | Player "A" (id=a1 pos=<1, 2, 3>)
04:20:42 | Player "B" (id=b1 pos=<4, 5, 6>)
04:20:42 | #####
`, 0)
	if e.players.OnlineCount() != 2 || changes != 1 {
		t.Fatalf("a complete snapshot proves who is online: %d (changes %d)", e.players.OnlineCount(), changes)
	}
	// Incomplete (declares 2, lists 1): nobody is removed.
	off = feed(t, e, path, `04:25:42 | ##### PlayerList log: 2 players
04:25:42 | Player "A" (id=a1 pos=<1, 2, 3>)
04:25:42 | #####
`, off)
	if e.players.OnlineCount() != 2 {
		t.Fatal("an incomplete snapshot must not remove anyone")
	}
	// Missing footer: the next header abandons the open snapshot as incomplete.
	off = feed(t, e, path, `04:30:42 | ##### PlayerList log: 1 players
04:30:42 | Player "A" (id=a1 pos=<1, 2, 3>)
`, off)
	if e.players.OnlineCount() != 2 {
		t.Fatal("a snapshot without its footer changes nothing yet")
	}
	off = feed(t, e, path, `04:35:42 | ##### PlayerList log: 1 players
04:35:42 | Player "A" (id=a1 pos=<1, 2, 3>)
04:35:42 | #####
`, off)
	if e.players.OnlineCount() != 1 {
		t.Fatalf("a complete snapshot without B proves B is gone: %d", e.players.OnlineCount())
	}
	st := e.PlayerListStats()
	if st.CompleteSnapshots != 2 || st.IncompleteSnapshots != 2 || st.PresenceAdded != 2 || st.PresenceRemoved != 1 {
		t.Fatalf("stats: %+v", st)
	}
	// A bare "#####" with no open snapshot is not a snapshot.
	feed(t, e, path, "04:40:42 | #####\n", off)
	if e.PlayerListStats().Snapshots != 4 {
		t.Fatal("a stray footer is not a snapshot")
	}
}

type recordingSessionStore struct {
	files  []string
	starts []*time.Time
}

func (r *recordingSessionStore) SetCurrentADMSession(_ context.Context, _, _ int64, file string, start *time.Time) error {
	r.files = append(r.files, file)
	r.starts = append(r.starts, start)
	return nil
}

func TestADMSessionRecordedOncePerLogicalFile(t *testing.T) {
	e := NewEngine(&fakeLogSource{}, "svc-1", NewADMParser())
	e.guildID, e.serverID = 7, 9
	store := &recordingSessionStore{}
	e.SetADMSessionStore(store)
	e.noteADMSession("/games/ni1_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM")
	e.noteADMSession("/games/ni1_2/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.ADM") // alias: same session
	e.noteADMSession("/games/ni1_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM")   // new boot
	if len(store.files) != 2 || store.files[1] != "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM" {
		t.Fatalf("%v", store.files)
	}
	if store.starts[1] == nil || store.starts[1].Format("2006-01-02 15:04:05") != "2026-09-24 05:23:05" {
		t.Fatalf("session start: %v", store.starts[1])
	}
}

func TestADMClockRollsOverMidnight(t *testing.T) {
	c, ok := newADMClock("DayZServer_PS4_x64_2026-09-24_23-30-00.ADM")
	if !ok {
		t.Fatal("clock")
	}
	if got := c.at("23:59:59").Format("2006-01-02 15:04:05"); got != "2026-09-24 23:59:59" {
		t.Fatal(got)
	}
	if got := c.at("00:00:05").Format("2006-01-02 15:04:05"); got != "2026-09-25 00:00:05" {
		t.Fatal(got)
	}
	if got := c.at("00:00:03").Format("2006-01-02 15:04:05"); got != "2026-09-25 00:00:03" {
		t.Fatalf("a slightly reordered line is not another day: %s", got)
	}
	if _, ok := newADMClock("no-stamp.ADM"); ok {
		t.Fatal("no stamp, no clock")
	}
	if c.at("25:00:00") != nil {
		t.Fatal("invalid time")
	}
}
