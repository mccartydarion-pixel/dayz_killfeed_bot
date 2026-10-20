package ownerops

import (
	"strings"
	"testing"
	"time"
)

func TestSilentFeed(t *testing.T) {
	min := time.Minute
	for name, c := range map[string]struct {
		mutate func(*ServerState)
		want   bool
		detail string
	}{
		"healthy server":                 {func(s *ServerState) {}, false, ""},
		"empty server, silent for hours": {func(s *ServerState) { s.PlayersKnown, s.PlayersOnline, s.LogSilentFor = true, 0, 5*time.Hour }, false, ""},
		"player count unknown": {func(s *ServerState) {
			s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = 4, 3*time.Hour, 3*time.Hour
		}, false, ""},
		"players online, lines still arriving": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = true, 6, 3*time.Hour, 2*min
		}, false, ""},
		"players only just joined a silent server": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = true, 6, 10*min, 3*time.Hour
		}, false, ""},
		"no player list seen: 59 minutes is not enough": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = true, 1, 59*min, 59*min
		}, false, ""},
		"no player list seen: silent after an hour": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = true, 1, 61*min, 60*min
		}, true, "player list log switched off"},
		"player lists were arriving: 19 minutes is not enough": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayerListSeen, s.PlayersOnlineFor, s.LogSilentFor = true, 3, true, 19*min, 19*min
		}, false, ""},
		"player lists were arriving: silent after 20 minutes": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayerListSeen, s.PlayersOnlineFor, s.LogSilentFor = true, 3, true, 45*min, 21*min
		}, true, "3 player(s) have been online for 45 minutes and the server log has produced nothing for 21 minutes"},
		"just after a restart every clock is short": {func(s *ServerState) {
			s.PlayersKnown, s.PlayersOnline, s.PlayerListSeen, s.PlayersOnlineFor, s.LogSilentFor = true, 9, false, 4*min, 4*min
		}, false, ""},
		"Nitrado says the server is stopped": {func(s *ServerState) {
			s.ServerStopped, s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = true, true, 2, 2*time.Hour, 2*time.Hour
			s.SourceState, s.SourceErrorClass, s.SourceBadFor = "TRANSPORT_ERROR", "temporary", time.Hour
		}, false, ""},
		"no worker": {func(s *ServerState) {
			s.WorkerRunning, s.PlayersKnown, s.PlayersOnline, s.PlayersOnlineFor, s.LogSilentFor = false, true, 2, 2*time.Hour, 2*time.Hour
		}, false, ""},
		"reads failing for 14 minutes": {func(s *ServerState) {
			s.SourceState, s.SourceErrorClass, s.SourceBadFor = "TRANSPORT_ERROR", "temporary", 14*min
		}, false, ""},
		"reads failing for 15 minutes, empty server": {func(s *ServerState) {
			s.SourceState, s.SourceErrorClass, s.SourceBadFor = "TRANSPORT_ERROR", "temporary", 15*min
			s.PlayersKnown = true
		}, true, "every read of the server log has failed for 15 minutes (Nitrado: temporary)"},
		"a refused token is the Nitrado access incident, not this one": {func(s *ServerState) {
			s.SourceState, s.SourceErrorClass, s.SourceBadFor = "TRANSPORT_ERROR", "authentication", time.Hour
		}, false, ""},
	} {
		s := live()
		c.mutate(&s)
		detail, got := SilentFeed(s)
		if got != c.want || !strings.Contains(detail, c.detail) || (!got && detail != "") {
			t.Errorf("%s: SilentFeed = (%q, %v), want %v containing %q", name, detail, got, c.want, c.detail)
		}
	}
}

// The silent feed is raised as FEED_STALLED, once, and never where nothing is expected.
func TestDetectSilentFeed(t *testing.T) {
	silent := func() ServerState {
		s := live()
		s.PlayersKnown, s.PlayersOnline, s.PlayerListSeen, s.PlayersOnlineFor, s.LogSilentFor = true, 4, true, time.Hour, time.Hour
		return s
	}
	if got := Detect(silent()); len(got) != 1 || !strings.Contains(got[KindFeedStalled], "4 player(s)") {
		t.Fatalf("silent feed = %v", got)
	}
	for name, mutate := range map[string]func(*ServerState){
		"suspended":       func(s *ServerState) { s.InstallationStatus = "SUSPENDED" },
		"not set up":      func(s *ServerState) { s.InstallationStatus = "CONFIGURING" },
		"server inactive": func(s *ServerState) { s.ServerActive = false },
		"just restarted":  func(s *ServerState) { s.RuntimeReady = false },
		"empty":           func(s *ServerState) { s.PlayersOnline = 0 },
		"recovered":       func(s *ServerState) { s.LogSilentFor = 30 * time.Second },
	} {
		s := silent()
		mutate(&s)
		if got := Detect(s); len(got) != 0 {
			t.Errorf("%s: implied %v", name, got)
		}
	}
	// A stalled worker keeps its own reason; a dead worker is only "worker down".
	stalled := silent()
	stalled.SourceState = "WORKER_STALLED"
	if got := Detect(stalled); len(got) != 1 || got[KindFeedStalled] != "the log worker has not completed a poll cycle recently" {
		t.Fatalf("stalled and silent = %v", got)
	}
	down := silent()
	down.WorkerRunning = false
	if got := Detect(down); len(got) != 1 || got[KindWorkerDown] == "" {
		t.Fatalf("down and silent = %v", got)
	}
	// Reads failing with a refused token: the access incident only.
	refused := live()
	refused.SourceState, refused.SourceErrorClass, refused.SourceBadFor = "TRANSPORT_ERROR", "permission", time.Hour
	if got := Detect(refused); len(got) != 1 || got[KindNitradoAccess] == "" {
		t.Fatalf("refused token = %v", got)
	}
}

func TestServerStatus(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(*StatusInput)
		state  string
		reason string
	}{
		"healthy":         {func(in *StatusInput) {}, StatusRunning, "events are flowing"},
		"quiet":           {func(in *StatusInput) { in.Server.SourceState = "QUIET" }, StatusQuiet, "nothing has happened"},
		"suspended":       {func(in *StatusInput) { in.Server.InstallationStatus = "SUSPENDED" }, StatusDown, "suspended"},
		"no server":       {func(in *StatusInput) { in.ServerSelected = false }, StatusDown, "No DayZ server"},
		"setup open":      {func(in *StatusInput) { in.Server.InstallationStatus = "CONFIGURING" }, StatusDown, "Setup is not finished"},
		"server inactive": {func(in *StatusInput) { in.Server.ServerActive = false }, StatusDown, "disconnected"},
		"bot removed":     {func(in *StatusInput) { in.Server.BotInstalled = false }, StatusDown, "not in your Discord server"},
		"no worker":       {func(in *StatusInput) { in.Server.WorkerRunning = false }, StatusDown, "Nothing is reading"},
		"no worker just after a restart": {func(in *StatusInput) {
			in.Server.WorkerRunning, in.Server.RuntimeReady = false, false
		}, StatusDegraded, "just restarted"},
		"stalled": {func(in *StatusInput) { in.Server.SourceState = "WORKER_STALLED" }, StatusDown, "stopped making progress"},
		"first poll not done after a restart": {func(in *StatusInput) {
			in.Server.SourceState, in.Server.RuntimeReady = "WORKER_STALLED", false
		}, StatusDegraded, "just restarted"},
		"token refused": {func(in *StatusInput) {
			in.Server.SourceState, in.Server.SourceErrorClass = "TRANSPORT_ERROR", "authentication"
		}, StatusDown, "reconnect Nitrado"},
		"nitrado failing": {func(in *StatusInput) {
			in.Server.SourceState, in.Server.SourceErrorClass = "TRANSPORT_ERROR", "temporary"
		}, StatusDegraded, "Nitrado is not answering"},
		"killfeed channel gone": {func(in *StatusInput) { in.KillfeedDelivery = "CONFIG_FAULT" }, StatusDegraded, "Discord refuses the killfeed channel"},
		"killfeed post failing": {func(in *StatusInput) { in.KillfeedDelivery = "FAILING" }, StatusDegraded, "could not be posted"},
		"lagging":               {func(in *StatusInput) { in.Server.SourceState = "SOURCE_LAGGING" }, StatusDegraded, "not advancing"},
		"silent with players": {func(in *StatusInput) {
			in.Server.SourceState = "QUIET"
			in.Server.PlayersKnown, in.Server.PlayersOnline, in.Server.PlayersOnlineFor, in.Server.LogSilentFor = true, 2, 2*time.Hour, 2*time.Hour
		}, StatusDegraded, "produced nothing"},
		"idle delivery is not a problem": {func(in *StatusInput) { in.KillfeedDelivery = "IDLE" }, StatusRunning, ""},
		// What stops the feed altogether is said first.
		"suspended beats everything": {func(in *StatusInput) {
			in.Server.InstallationStatus, in.Server.WorkerRunning, in.KillfeedDelivery = "SUSPENDED", false, "CONFIG_FAULT"
		}, StatusDown, "suspended"},
	} {
		in := StatusInput{Server: live(), ServerSelected: true, KillfeedDelivery: "OK"}
		c.mutate(&in)
		state, reason := ServerStatus(in)
		if state != c.state || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: ServerStatus = (%s, %q), want %s containing %q", name, state, reason, c.state, c.reason)
		}
		if reason == "" || strings.Contains(reason, "\n") || !strings.HasSuffix(reason, ".") {
			t.Errorf("%s: the reason must be one plain sentence, got %q", name, reason)
		}
	}
}

func TestPositionLogging(t *testing.T) {
	min := time.Minute
	for name, c := range map[string]struct {
		in    PositionInput
		state string
	}{
		"no worker":                         {PositionInput{PlayersKnown: true, PlayersOnline: 5, PlayersOnlineFor: time.Hour}, PositionUnknown},
		"nobody online, nothing seen":       {PositionInput{WorkerRunning: true, PlayersKnown: true}, PositionUnknown},
		"count unknown":                     {PositionInput{WorkerRunning: true, PlayersOnline: 5, PlayersOnlineFor: time.Hour}, PositionUnknown},
		"players just joined":               {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 5, PlayersOnlineFor: 14 * min}, PositionUnknown},
		"players online 15 min, none seen":  {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 5, PlayersOnlineFor: 15 * min}, PositionNotArriving},
		"a fresh list":                      {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 5, PlayersOnlineFor: time.Hour, PlayerListSeen: true, SinceLastPlayerList: 6 * min}, PositionOK},
		"a fresh list on an empty server":   {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayerListSeen: true, SinceLastPlayerList: 4 * min}, PositionOK},
		"lists stopped while players stay":  {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 2, PlayersOnlineFor: time.Hour, PlayerListSeen: true, SinceLastPlayerList: 40 * min}, PositionNotArriving},
		"an old list and nobody online now": {PositionInput{WorkerRunning: true, PlayersKnown: true, PlayerListSeen: true, SinceLastPlayerList: 3 * time.Hour}, PositionUnknown},
	} {
		state, advice := PositionLogging(c.in)
		if state != c.state {
			t.Errorf("%s: state = %s, want %s", name, state, c.state)
		}
		if (state == PositionNotArriving) != (advice != "") {
			t.Errorf("%s: advice %q does not match state %s", name, advice, state)
		}
		if advice != "" && !strings.Contains(advice, "adminLogPlayerList") {
			t.Errorf("%s: the advice must name the setting: %q", name, advice)
		}
	}
}
