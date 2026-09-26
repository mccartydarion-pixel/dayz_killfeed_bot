package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/server"
)

func intp(n int) *int { return &n }

func TestResolveOnlineReading(t *testing.T) {
	now := time.Now()
	started := func(n int) nitrado.GameserverLive {
		return nitrado.GameserverLive{Status: "started", Slots: 18, PlayerCurrent: intp(n), PlayerMax: 18}
	}
	cases := []struct {
		name   string
		in     onlineReadingInput
		want   discord.CounterReading
		source string
	}{
		{"zero players", onlineReadingInput{Live: started(0), LiveRead: true, Now: now},
			discord.CounterReading{Count: 0, Slots: 18, Known: true}, counterSourceNitrado},
		{"one player", onlineReadingInput{Live: started(1), LiveRead: true, Now: now},
			discord.CounterReading{Count: 1, Slots: 18, Known: true}, counterSourceNitrado},
		{"two players", onlineReadingInput{Live: started(2), LiveRead: true, Now: now},
			discord.CounterReading{Count: 2, Slots: 18, Known: true}, counterSourceNitrado},
		// The bot's tracker holding phantoms (5) never overrides Nitrado (2).
		{"nitrado beats stale tracker", onlineReadingInput{Live: started(2), LiveRead: true, Tracker: 5, ADMProven: now, Now: now},
			discord.CounterReading{Count: 2, Slots: 18, Known: true}, counterSourceNitrado},
		{"server stopped is a known zero", onlineReadingInput{Live: nitrado.GameserverLive{Status: "stopped", Slots: 18}, LiveRead: true, Tracker: 3, Now: now},
			discord.CounterReading{Count: 0, Slots: 18, Known: true}, counterSourceNitradoStopped},
		{"restarting without query is unknown", onlineReadingInput{Live: nitrado.GameserverLive{Status: "restarting", Slots: 18}, LiveRead: true, Tracker: 3, Now: now},
			discord.CounterReading{Slots: 18}, counterSourceUnknown},
		{"nitrado down, proven ADM list", onlineReadingInput{LiveRead: true, LiveErr: errors.New("503"), Tracker: 2, ADMProven: now.Add(-4 * time.Minute), KnownSlots: 18, Now: now},
			discord.CounterReading{Count: 2, Slots: 18, Known: true}, counterSourceADMPlayerList},
		// Connect/disconnect lines alone never become a live count.
		{"nitrado down, unproven tracker is unknown", onlineReadingInput{LiveRead: true, LiveErr: errors.New("503"), Tracker: 2, KnownSlots: 18, Now: now},
			discord.CounterReading{Slots: 18}, counterSourceUnknown},
		{"nitrado down, stale ADM list is unknown", onlineReadingInput{LiveRead: true, LiveErr: errors.New("503"), Tracker: 2, ADMProven: now.Add(-30 * time.Minute), KnownSlots: 18, Now: now},
			discord.CounterReading{Slots: 18}, counterSourceUnknown},
		// ADM unavailable (no list, tracker empty) does not produce a fake 0
		// when Nitrado is fine - Nitrado is used.
		{"ADM unavailable, nitrado fine", onlineReadingInput{Live: started(2), LiveRead: true, Tracker: 0, Now: now},
			discord.CounterReading{Count: 2, Slots: 18, Known: true}, counterSourceNitrado},
		{"everything unavailable", onlineReadingInput{LiveRead: true, LiveErr: errors.New("timeout"), Now: now},
			discord.CounterReading{}, counterSourceUnknown},
	}
	for _, tc := range cases {
		got, source := resolveOnlineReading(tc.in)
		if got != tc.want || source != tc.source {
			t.Errorf("%s: got %+v/%s want %+v/%s", tc.name, got, source, tc.want, tc.source)
		}
	}
}

type fakeLive struct {
	mu   sync.Mutex
	live nitrado.GameserverLive
	err  error
	gets int
}

func (f *fakeLive) set(live nitrado.GameserverLive, err error) {
	f.mu.Lock()
	f.live, f.err = live, err
	f.mu.Unlock()
}

func (f *fakeLive) GameserverLive(context.Context, string) (nitrado.GameserverLive, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.live, f.err
}

type fakeVoiceChannel struct {
	mu      sync.Mutex
	name    string
	renames []string
	err     error
	edited  chan string
}

func (f *fakeVoiceChannel) ChannelEdit(id string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return nil, err
	}
	f.name = data.Name
	f.renames = append(f.renames, data.Name)
	f.mu.Unlock()
	if f.edited != nil {
		f.edited <- data.Name
	}
	return &discordgo.Channel{ID: id, Name: data.Name}, nil
}

func (f *fakeVoiceChannel) Channel(id string) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &discordgo.Channel{ID: id, Name: f.name}, nil
}

func (f *fakeVoiceChannel) current() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.name, len(f.renames)
}

type counterHarness struct {
	app     *App
	live    *fakeLive
	channel *fakeVoiceChannel
	counter *discord.VoiceChannelCounter
	engine  *killfeed.Engine
}

func newCounterHarness(initialName string) *counterHarness {
	a := &App{State: server.NewState()}
	a.publicCounterServerID = 42
	live := &fakeLive{}
	ch := &fakeVoiceChannel{name: initialName, edited: make(chan string, 16)}
	counter := discord.NewVoiceChannelCounter(ch, "vc-online")
	counter.SetDebounce(0)
	counter.SetMinRenameInterval(0)
	engine := killfeed.NewEngine(nil, "19806451", killfeed.NewADMParser())
	a.registerCounterSource(42, counterSource{serviceID: "19806451", live: live, engine: engine})
	return &counterHarness{app: a, live: live, channel: ch, counter: counter, engine: engine}
}

func (h *counterHarness) eval(t *testing.T) {
	t.Helper()
	h.app.evaluateOnlineCounter(context.Background(), h.counter)
}

// waitName waits for the channel to show want (Publish renames asynchronously).
func (h *counterHarness) waitName(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if name, _ := h.channel.current(); name == want {
			return
		}
		select {
		case <-h.channel.edited:
		case <-deadline:
			name, _ := h.channel.current()
			t.Fatalf("channel name %q, want %q", name, want)
		}
	}
}

func started(n int) nitrado.GameserverLive {
	return nitrado.GameserverLive{Status: "started", Slots: 18, PlayerCurrent: intp(n), PlayerMax: 18}
}

func TestOnlineCounterPublishesNitradoCount(t *testing.T) {
	// The reported incident: two players connected, the channel showing a
	// stale count from the bot's own tracker.
	h := newCounterHarness("🟢・Online Players: 5")
	h.engine.PlayerTracker().PlayerConnected(&killfeed.PlayerRef{ID: "phantom-1", Name: "Ghost"})
	h.live.set(started(2), nil)
	h.eval(t)
	h.waitName(t, "🟢・Online: 2/18")
	if got := h.app.State.Snapshot()["online_players"]; got != 2 {
		t.Fatalf("runtime state online_players = %v, want 2", got)
	}
	st := h.app.OnlineCounterStatus()
	if st.Source != counterSourceNitrado || st.TrackerCount != 1 || st.NitradoCount == nil || *st.NitradoCount != 2 {
		t.Fatalf("unexpected status %+v", st)
	}
}

func TestOnlineCounterZeroOneTwoAndDisconnect(t *testing.T) {
	h := newCounterHarness("")
	for _, n := range []int{0, 1, 2, 1} {
		h.live.set(started(n), nil)
		h.eval(t)
		h.waitName(t, discord.OnlineCounterName(n, 18))
	}
	_, renames := h.channel.current()
	if renames != 4 {
		t.Fatalf("expected 4 renames, got %d", renames)
	}
	// No change -> no rename.
	h.eval(t)
	time.Sleep(20 * time.Millisecond)
	if _, again := h.channel.current(); again != renames {
		t.Fatalf("unchanged count renamed the channel (%d -> %d)", renames, again)
	}
}

func TestOnlineCounterRestartHoldsThenShowsUnknown(t *testing.T) {
	h := newCounterHarness("")
	h.live.set(started(2), nil)
	h.eval(t)
	h.waitName(t, "🟢・Online: 2/18")

	// Server restarting: Nitrado has no query result. The last value is held
	// through a short outage rather than dropping to a manufactured 0.
	h.live.set(nitrado.GameserverLive{Status: "restarting", Slots: 18}, nil)
	h.eval(t)
	time.Sleep(20 * time.Millisecond)
	if name, _ := h.channel.current(); name != "🟢・Online: 2/18" {
		t.Fatalf("expected last known value held during restart, got %q", name)
	}
	if st := h.app.OnlineCounterStatus(); !st.Held || st.Reading.Known {
		t.Fatalf("expected held unknown status, got %+v", st)
	}

	// Still unknown after the grace period: show "?", never 0.
	l := h.app.counterLoop()
	l.mu.Lock()
	l.lastKnown[42] = time.Now().Add(-onlineCounterUnknownGrace - time.Minute)
	l.startedAt = l.lastKnown[42]
	l.mu.Unlock()
	h.eval(t)
	h.waitName(t, "⚪・Online: ?/18")

	// Server back up with nobody on yet.
	h.live.set(started(0), nil)
	h.eval(t)
	h.waitName(t, "🟢・Online: 0/18")
}

func TestOnlineCounterNitradoUnavailableNeverManufacturesZero(t *testing.T) {
	h := newCounterHarness("")
	h.live.set(started(2), nil)
	h.eval(t)
	h.waitName(t, "🟢・Online: 2/18")

	h.live.set(nitrado.GameserverLive{}, errors.New("nitrado: 503"))
	h.eval(t)
	time.Sleep(20 * time.Millisecond)
	if name, _ := h.channel.current(); name != "🟢・Online: 2/18" {
		t.Fatalf("a Nitrado outage must not change the count, got %q", name)
	}
	st := h.app.OnlineCounterStatus()
	if st.NitradoError == "" || st.Reading.Known || !st.Held {
		t.Fatalf("expected unknown-held status with Nitrado error, got %+v", st)
	}
	if got := h.app.State.Snapshot()["online_players"]; got != 2 {
		t.Fatalf("runtime state must keep the last known count, got %v", got)
	}
}

func TestOnlineCounterDiscordFailureIsRetried(t *testing.T) {
	h := newCounterHarness("🟢・Online: 0/18")
	h.channel.mu.Lock()
	h.channel.err = errors.New("discord unavailable")
	h.channel.mu.Unlock()
	h.live.set(started(2), nil)
	h.eval(t)
	if h.counter.UpdateErrors() == 0 {
		t.Fatal("expected the failed rename to be counted")
	}
	if got := h.app.State.Snapshot()["online_counter_update_errors"]; got != 1 {
		t.Fatalf("expected runtime state to report the failure, got %v", got)
	}
	// Discord recovers: the next evaluation publishes the pending count.
	h.channel.mu.Lock()
	h.channel.err = nil
	h.channel.mu.Unlock()
	h.eval(t)
	h.waitName(t, "🟢・Online: 2/18")
}

func TestOnlineCounterNoSelectedServerLeavesChannelAlone(t *testing.T) {
	h := newCounterHarness("🟢・Online: 3/18")
	h.app.publicCounterServerID = 0
	h.live.set(started(1), nil)
	h.eval(t)
	if name, renames := h.channel.current(); name != "🟢・Online: 3/18" || renames != 0 {
		t.Fatalf("expected untouched channel, got %q after %d renames", name, renames)
	}
	if h.live.gets != 0 {
		t.Fatal("no Nitrado read expected without a selected server")
	}
}

// Bot startup: the loop evaluates before the worker registers. The channel
// keeps what it shows; it is never flashed to "?" or 0.
func TestOnlineCounterStartupBeforeWorkerKeepsChannel(t *testing.T) {
	h := newCounterHarness("🟢・Online: 2/18")
	h.app.unregisterCounterSource(42)
	h.eval(t)
	st := h.app.OnlineCounterStatus()
	if st.Reading.Known || st.Source != counterSourceUnknown || !st.Held {
		t.Fatalf("expected held unknown without a running worker, got %+v", st)
	}
	if name, renames := h.channel.current(); name != "🟢・Online: 2/18" || renames != 0 {
		t.Fatalf("expected untouched channel at startup, got %q after %d renames", name, renames)
	}
}
