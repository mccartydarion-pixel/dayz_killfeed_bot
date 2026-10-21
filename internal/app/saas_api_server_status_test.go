package app

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func statusKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The JSON shape is the contract in docs/SERVER_STATUS.md: the website is built against these
// exact field names. A change here is a change to that document.
func TestServerStatusContractShape(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	out := buildServerStatus(serverStatusInputs{Now: now, Fact: repository.FleetFact{InstallationID: 4}, MapRotation: &serverStatusMapRotationDTO{}})
	out.Discord.Problems = append(out.Discord.Problems, serverStatusDiscordProblemDTO{})
	for name, c := range map[string]struct {
		v    any
		want string
	}{
		"top level": {out, "checkedAt discord feed installationId lastKillAt lastKillPostedAt lastLogCheckAt lastLogLineAt mapRotation nitrado players positionLogging serverId serverName"},
		"feed":      {out.Feed, "reason state"},
		"players":   {out.Players, "online source"},
		"nitrado":   {out.Nitrado, "errorClass lastFailureAt lastSuccessAt reachable"},
		"discord":   {out.Discord, "botInGuild problems reachable"},
		"problem":   {out.Discord.Problems[0], "channelId feed problem since"},
		"position":  {out.PositionLogging, "advice lastPositionAt playersOnlineMinutes state"},
		"rotation":  {out.MapRotation, "currentMap nextMap stopped stoppedReason switchAt voteOpen"},
	} {
		if got := strings.Join(statusKeys(t, c.v), " "); got != c.want {
			t.Errorf("%s keys:\n got %s\nwant %s", name, got, c.want)
		}
	}
	// Unknown is null, never a zero that reads as a fact; lists are never null.
	raw, _ := json.Marshal(buildServerStatus(serverStatusInputs{Now: now, Fact: repository.FleetFact{InstallationID: 4}}))
	for _, want := range []string{`"serverId":null`, `"serverName":null`, `"lastLogLineAt":null`, `"lastKillAt":null`, `"players":{"online":null,"source":"UNKNOWN"}`,
		`"reachable":null`, `"problems":[]`, `"mapRotation":null`, `"checkedAt":"2026-10-05T12:00:00Z"`, `"state":"DOWN"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty installation: %s missing from %s", want, raw)
		}
	}
}

func TestBuildServerStatus(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	ptr := func(t time.Time) *time.Time { return &t }
	fact := repository.FleetFact{InstallationID: 4, OrganizationID: 2, Status: repository.InstallationReady, BotInstalled: true, ServerID: 9, ServerName: "Chernarus PvP",
		ServerActive: true, LastKillAt: ptr(ago(3 * time.Hour)), NitradoStatus: "CONNECTED", NitradoLastOKAt: ptr(ago(time.Hour))}
	base := func() serverStatusInputs {
		return serverStatusInputs{Now: now, Fact: fact, Served: true, RuntimeReady: true, DiscordConnected: true,
			Runtime: serverRuntime{WorkerRunning: true, SourceState: killfeed.ADMHealthy, LastCycleAt: ptr(ago(20 * time.Second))},
			Watch: feedWatchView{Watching: true, PlayersOnlineFor: 40 * time.Minute, LogSilentFor: time.Minute, PlayerListSeen: true,
				Sample: feedSample{WorkerRunning: true, PlayersKnown: true, PlayersOnline: 7, PlayersSource: counterSourceNitrado, SourceState: killfeed.ADMHealthy,
					LastLogLineAt: ago(time.Minute), LastLogCheckAt: ago(20 * time.Second), LastPlayerList: ago(3 * time.Minute)}},
			Deliveries: []discord.RouteDelivery{{Route: "KILLFEED", ChannelID: "111", Delivered: 5, LastSuccessAt: ago(2 * time.Hour)}}}
	}

	// Healthy and busy.
	out := buildServerStatus(base())
	if out.Feed.State != ownerops.StatusRunning || *out.ServerID != 9 || *out.ServerName != "Chernarus PvP" || *out.Players.Online != 7 || out.Players.Source != "NITRADO_QUERY" {
		t.Fatalf("healthy = %+v", out)
	}
	if *out.LastLogLineAt != "2026-10-05T11:59:00Z" || *out.LastLogCheckAt != "2026-10-05T11:59:40Z" || *out.LastKillAt != "2026-10-05T09:00:00Z" || *out.LastKillPostedAt != "2026-10-05T10:00:00Z" {
		t.Fatalf("times = %+v", out)
	}
	if out.Nitrado.Reachable == nil || !*out.Nitrado.Reachable || out.Nitrado.ErrorClass != nil || *out.Nitrado.LastSuccessAt != "2026-10-05T11:59:40Z" {
		t.Fatalf("nitrado = %+v", out.Nitrado)
	}
	if !out.Discord.Reachable || !out.Discord.BotInGuild || len(out.Discord.Problems) != 0 {
		t.Fatalf("discord = %+v", out.Discord)
	}
	if out.PositionLogging.State != ownerops.PositionOK || out.PositionLogging.Advice != nil || *out.PositionLogging.LastPositionAt != "2026-10-05T11:57:00Z" || out.PositionLogging.PlayersOnlineMinutes != 40 {
		t.Fatalf("position logging = %+v", out.PositionLogging)
	}

	// The customer problem: players online for 40 minutes and not one position line.
	in := base()
	in.Watch.PlayerListSeen, in.Watch.Sample.LastPlayerList = false, time.Time{}
	out = buildServerStatus(in)
	if out.PositionLogging.State != ownerops.PositionNotArriving || out.PositionLogging.Advice == nil || !strings.Contains(*out.PositionLogging.Advice, "Log player list") || out.PositionLogging.LastPositionAt != nil {
		t.Fatalf("position logging off = %+v", out.PositionLogging)
	}
	if out.Feed.State != ownerops.StatusRunning {
		t.Fatalf("missing positions do not make the feed unhealthy: %+v", out.Feed)
	}

	// Nitrado failing right now wins over an older recorded success.
	in = base()
	in.Runtime.SourceState, in.Runtime.ErrorClass, in.Watch.Sample.SourceState = killfeed.ADMTransportError, "temporary", killfeed.ADMTransportError
	out = buildServerStatus(in)
	if out.Feed.State != ownerops.StatusDegraded || out.Nitrado.Reachable == nil || *out.Nitrado.Reachable || *out.Nitrado.ErrorClass != "temporary" {
		t.Fatalf("nitrado failing = %+v / %+v", out.Feed, out.Nitrado)
	}

	// A deleted killfeed channel is a named Discord problem and degrades the feed.
	in = base()
	in.Deliveries = []discord.RouteDelivery{
		{Route: "KILLFEED", ChannelID: "111", Delivered: 5, Failed: 2, ConsecutiveFailures: 2, LastErrorClass: discord.DeliveryConfigFault, LastFailureAt: ago(time.Minute),
			LastError: "HTTP 404 Not Found, {\"message\": \"Unknown Channel\"} Authorization: Bot SECRET-TOKEN"},
		{Route: "DEATH_FEED", ChannelID: "222", Delivered: 1, ConsecutiveFailures: 1, LastErrorClass: discord.DeliveryTransient, LastFailureAt: ago(time.Minute)},
		{Route: "HIT_FEED", Delivered: 9},
	}
	out = buildServerStatus(in)
	if out.Feed.State != ownerops.StatusDegraded || !strings.Contains(out.Feed.Reason, "killfeed channel") || len(out.Discord.Problems) != 2 {
		t.Fatalf("channel gone = %+v / %+v", out.Feed, out.Discord)
	}
	if p := out.Discord.Problems[0]; p.Feed != "KILLFEED" || p.Problem != "CHANNEL_OR_PERMISSION" || *p.ChannelID != "111" || *p.Since != "2026-10-05T11:59:00Z" {
		t.Fatalf("problem = %+v", p)
	}
	if out.Discord.Problems[1].Problem != "FAILING" {
		t.Fatalf("second problem = %+v", out.Discord.Problems[1])
	}
	// The raw Discord error (which can quote a request) never leaves the process.
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "SECRET-TOKEN") || strings.Contains(string(raw), "Unknown Channel") {
		t.Fatalf("the response leaks the raw delivery error: %s", raw)
	}

	// Suspended: DOWN whatever the worker says.
	in = base()
	in.Fact.Status = repository.InstallationSuspended
	if out = buildServerStatus(in); out.Feed.State != ownerops.StatusDown || !strings.Contains(out.Feed.Reason, "suspended") {
		t.Fatalf("suspended = %+v", out.Feed)
	}

	// An installation whose Discord server another process serves: this process's worker, watch
	// and delivery state are not described, only what is stored about the installation itself.
	in = base()
	in.Served = false
	out = buildServerStatus(in)
	if out.Feed.State != ownerops.StatusDown || out.Players.Online != nil || out.LastLogLineAt != nil || out.LastKillPostedAt != nil || len(out.Discord.Problems) != 0 ||
		out.PositionLogging.State != ownerops.PositionUnknown || *out.LastKillAt != "2026-10-05T09:00:00Z" {
		t.Fatalf("not served here = %+v", out)
	}
	if out.Nitrado.Reachable == nil || !*out.Nitrado.Reachable || *out.Nitrado.LastSuccessAt != "2026-10-05T11:00:00Z" {
		t.Fatalf("stored nitrado state = %+v", out.Nitrado)
	}

	// No worker, and the last recorded Nitrado check failed after the last success.
	in = base()
	in.Runtime, in.Watch = serverRuntime{}, feedWatchView{}
	in.Fact.NitradoLastFailAt, in.Fact.NitradoErrorClass = ptr(ago(time.Minute)), "authentication"
	out = buildServerStatus(in)
	if out.Feed.State != ownerops.StatusDown || out.Nitrado.Reachable == nil || *out.Nitrado.Reachable || *out.Nitrado.ErrorClass != "authentication" {
		t.Fatalf("no worker = %+v / %+v", out.Feed, out.Nitrado)
	}
}
