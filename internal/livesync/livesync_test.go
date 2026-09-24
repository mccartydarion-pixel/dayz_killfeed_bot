package livesync

import (
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func local(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		panic(err)
	}
	return t
}

func byCategory(rs []Record, cat string) []Record {
	var out []Record
	for _, r := range rs {
		if r.Category == cat {
			out = append(out, r)
		}
	}
	return out
}

func TestClassifySource(t *testing.T) {
	for path, want := range map[string]struct{ family, canonical, stamp string }{
		"/games/ni1_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM":   {FamilyADM, "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM", "2026-09-24 05:23:05"},
		"/games/ni1_2/ftproot/dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM": {FamilyADM, "dayzps/config/DayZServer_PS4_x64_2026-09-24_05-23-05.ADM", "2026-09-24 05:23:05"},
		"/games/ni1_2/noftp/dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT":   {FamilyRPT, "dayzps/config/DayZServer_PS4_x64_2026-09-24_04-15-07.RPT", "2026-09-24 04:15:07"},
		"/games/ni1_2/noftp/dayzps/config/script_2026-09-24_05-23-09.log":               {FamilyScript, "dayzps/config/script_2026-09-24_05-23-09.log", "2026-09-24 05:23:09"},
		"/games/ni1_2/noftp/dayzps/config/crash_2026-09-24_05-23-09.log":                {FamilyCrash, "dayzps/config/crash_2026-09-24_05-23-09.log", "2026-09-24 05:23:09"},
		"/games/ni1_2/ftproot/restart.log":                                              {FamilyRestart, "restart.log", ""},
		"/games/ni1_2/noftp/dayzps/ban.txt":                                             {FamilyBanList, "dayzps/ban.txt", ""},
		"/games/ni1_2/noftp/dayzps/whitelist.txt":                                       {FamilyWhitelist, "dayzps/whitelist.txt", ""},
		"/games/ni1_2/noftp/dayzps/config/something.log":                                {FamilyUnknown, "dayzps/config/something.log", ""},
	} {
		got := ClassifySource(path)
		if got.Family != want.family || got.CanonicalID != want.canonical {
			t.Errorf("%s: %+v", path, got)
		}
		if want.stamp == "" && got.FileLocalStart != nil || want.stamp != "" && (got.FileLocalStart == nil || !got.FileLocalStart.Equal(local(want.stamp))) {
			t.Errorf("%s: stamp %v", path, got.FileLocalStart)
		}
	}
}

func TestParseRPTFixture(t *testing.T) {
	res, consumed := ParseRPT(fixture(t, "DayZServer_PS4_x64_2026-09-24_04-15-07.RPT"), 0, nil)
	if consumed != int64(len(fixture(t, "DayZServer_PS4_x64_2026-09-24_04-15-07.RPT"))) {
		t.Fatalf("every complete line consumed: %d", consumed)
	}
	if res.BootLocal == nil || !res.BootLocal.Equal(local("2026-09-24 04:15:07")) || res.Version != "1.29.163709" {
		t.Fatalf("boot %v version %q", res.BootLocal, res.Version)
	}
	spawner := byCategory(res.Records, CategoryObjectSpawnerError)
	if len(spawner) != 1 || spawner[0].Payload["missingFile"] != "custom/The_Losst_City.json" || spawner[0].Payload["function"] != "SpawnObjects" ||
		!strings.Contains(spawner[0].Payload["stack"], "objectspawner.c:28 SpawnObjects") || strings.Count(spawner[0].Payload["stack"], "\n") != 1 {
		t.Fatalf("object spawner error: %+v", spawner)
	}
	if n := len(byCategory(res.Records, CategoryShutdownCountdown)); n != 3 {
		t.Fatalf("countdown lines: %d", n)
	}
	done := byCategory(res.Records, CategoryShutdownComplete)
	if len(done) != 1 || done[0].SourceLocalTime == nil || !done[0].SourceLocalTime.Equal(local("2026-09-24 05:21:24").Add(103*time.Millisecond)) {
		t.Fatalf("termination: %+v", done)
	}
	left := byCategory(res.Records, CategoryNetworkPlayerLeft)
	if len(left) != 1 || left[0].Payload["gamertag"] != "Ceiyxe" || strings.Contains(left[0].Evidence, "000000001") {
		t.Fatalf("player removed (id must be redacted): %+v", left)
	}
	if res.Stats[CategoryMissingModel] != 2 || res.Stats[CategoryLocalization] != 3 || res.Stats[CategoryCentralEconomy] != 1 || res.Stats[CategoryConfigWarning] != 1 {
		t.Fatalf("stats: %v", res.Stats)
	}
	unknown := byCategory(res.Records, CategoryUnknown)
	found := false
	for _, u := range unknown {
		found = found || strings.Contains(u.Evidence, "Something new and unrecognized")
		if u.Status != StatusUnknown {
			t.Fatalf("an unknown line is never PARSED: %+v", u)
		}
	}
	if !found {
		t.Fatal("unknown lines must remain discoverable")
	}
	for _, r := range res.Records {
		if strings.Contains(r.Evidence, "203.0.113.10") || strings.Contains(r.Evidence, "ni00000000") {
			t.Fatalf("evidence must be redacted: %q", r.Evidence)
		}
	}
}

func TestParseRPTPartialLineAndDelta(t *testing.T) {
	full := fixture(t, "DayZServer_PS4_x64_2026-09-24_04-15-07.RPT")
	cut := len(full) - 10 // in the middle of the last line
	res, consumed := ParseRPT(full[:cut], 0, nil)
	if consumed >= int64(cut) || full[consumed-1] != '\n' {
		t.Fatalf("a partial trailing line must not be consumed: %d/%d", consumed, cut)
	}
	// Resume from the checkpoint with the boot time seeded: the rest parses with correct times and
	// the same offsets as a single full read.
	rest, _ := ParseRPT(full[consumed:], consumed, res.BootLocal)
	whole, _ := ParseRPT(full, 0, nil)
	if len(res.Records)+len(rest.Records) != len(whole.Records) {
		t.Fatalf("split reads must yield the same records: %d+%d vs %d", len(res.Records), len(rest.Records), len(whole.Records))
	}
	last := rest.Records[len(rest.Records)-1]
	if last.Offset != int64(len(full)) || last.Offset != whole.Records[len(whole.Records)-1].Offset {
		t.Fatalf("offsets must be absolute: %d", last.Offset)
	}
}

func TestParseScriptAndCrashLogs(t *testing.T) {
	res, _ := ParseScriptLog(fixture(t, "script_2026-09-24_05-23-09.log"), 0, 2026)
	if res.BootLocal == nil || !res.BootLocal.Equal(local("2026-09-24 05:23:09")) {
		t.Fatalf("header: %v", res.BootLocal)
	}
	if n := len(byCategory(res.Records, CategoryScriptModule)); n != 5 {
		t.Fatalf("modules: %d", n)
	}
	exc := byCategory(res.Records, CategoryObjectSpawnerError)
	if len(exc) != 1 || exc[0].Payload["missingFile"] != "custom/The_Losst_City.json" || strings.Count(exc[0].Payload["stack"], "\n") != 3 {
		t.Fatalf("exception: %+v", exc)
	}
	if exc[0].Payload["block"] != "" {
		t.Fatal("the VM exception header was seen")
	}
	shutdown, _ := ParseScriptLog(fixture(t, "script_2026-09-24_01-47-18.log"), 0, 2026)
	if n := len(byCategory(shutdown.Records, CategoryEngineDestroy)); n != 1 {
		t.Fatalf("~DayZGame(): %d", n)
	}
	crash, _ := ParseScriptLog(fixture(t, "crash_2026-09-24_05-23-09.log"), 0, 2026)
	ce := byCategory(crash.Records, CategoryObjectSpawnerError)
	if len(ce) != 1 || ce[0].SourceLocalTime == nil || !ce[0].SourceLocalTime.Equal(local("2026-09-24 05:23:39")) {
		t.Fatalf("crash exception: %+v", ce)
	}
	for _, r := range crash.Records {
		if strings.Contains(r.Evidence, "203.0.113.10") || strings.Contains(r.Evidence, "CLI params") {
			t.Fatalf("CLI params (server address) must never be kept: %q", r.Evidence)
		}
	}
	noYear, _ := ParseScriptLog(fixture(t, "script_2026-09-24_05-23-09.log"), 0, 0)
	if noYear.BootLocal != nil || noYear.Records[0].Status != StatusPartial {
		t.Fatal("without a year the header time is not invented")
	}
}

func TestParseRestartLog(t *testing.T) {
	res, _ := ParseRestartLog(fixture(t, "restart.log"), 0)
	req := byCategory(res.Records, CategoryRestartRequested)
	if len(req) != 4 {
		t.Fatalf("restart requests: %d", len(req))
	}
	web := req[2] // "2026-09-24 08:13:29 Server restart requested (Webinterface)"
	if web.Payload["requestedVia"] != "Webinterface" || web.Payload["clock"] != "utc_inferred" || web.SourceLocalTime != nil || !web.SourceUTC.Equal(local("2026-09-24 08:13:29")) {
		t.Fatalf("iso line: %+v", web)
	}
	win := req[3] // "Thu, 24 Sep 2026 04:13:32 -0400 Server restart requested (WINDOWS)"
	if win.Payload["requestedVia"] != "WINDOWS" || !win.SourceLocalTime.Equal(local("2026-09-24 04:13:32")) || !win.SourceUTC.Equal(local("2026-09-24 08:13:32")) {
		t.Fatalf("offset line: %+v", win)
	}
	if len(byCategory(res.Records, CategoryHostReboot)) != 1 || len(byCategory(res.Records, CategoryClientAdminRequest)) != 1 || res.Stats[CategoryPreStartCheck] != 5 {
		t.Fatalf("stats %v", res.Stats)
	}
	if u := byCategory(res.Records, CategoryUnknown); len(u) != 1 || u[0].Status != StatusUnknown {
		t.Fatalf("unknown: %+v", u)
	}
	for _, r := range res.Records {
		if strings.Contains(r.Evidence, "ni00000000") {
			t.Fatalf("service account must be redacted: %q", r.Evidence)
		}
	}
}

func TestBootCorrelationCountsOneRestartOnce(t *testing.T) {
	ev := []BootEvidence{
		{Kind: EvidenceRestartReq, SourceID: "restart.log", Local: local("2026-09-24 04:13:32"), Detail: "WINDOWS"},
		{Kind: EvidencePreStartCheck, SourceID: "restart.log", Local: local("2026-09-24 04:14:31")},
		{Kind: EvidenceRPTHeader, SourceID: "rpt", Local: local("2026-09-24 04:15:07")},
		{Kind: EvidenceADMHeader, SourceID: "adm", Local: local("2026-09-24 04:15:07")},
		{Kind: EvidenceFilename, SourceID: "adm", Local: local("2026-09-24 04:15:07")},
		{Kind: EvidenceScriptHeader, SourceID: "script", Local: local("2026-09-24 04:15:10")},
		{Kind: EvidenceCrashHeader, SourceID: "crash", Local: local("2026-09-24 04:15:41")},
		// The next natural boot (no restart request).
		{Kind: EvidencePreStartCheck, SourceID: "restart.log", Local: local("2026-09-24 05:22:27")},
		{Kind: EvidenceFilename, SourceID: "adm2", Local: local("2026-09-24 05:23:05")},
		{Kind: EvidenceScriptHeader, SourceID: "script2", Local: local("2026-09-24 05:23:09")},
		// An isolated pre-start check with no boot evidence joins nothing.
		{Kind: EvidencePreStartCheck, SourceID: "restart.log", Local: local("2026-09-24 02:54:21")},
	}
	boots := CorrelateBoots(ev)
	if len(boots) != 2 {
		t.Fatalf("two boots, each counted once: %+v", boots)
	}
	b := boots[0]
	if b.BootID != "boot:2026-09-24T04:15:07" || b.AnchorKind != EvidenceRPTHeader || b.RestartCause != "WINDOWS" || !b.Confirmed || len(b.Evidence) != 7 {
		t.Fatalf("first boot: %+v", b)
	}
	if boots[1].BootID != "boot:2026-09-24T05:23:09" || boots[1].RestartCause != "" || !boots[1].Confirmed || len(boots[1].Evidence) != 3 {
		t.Fatalf("second boot: %+v", boots[1])
	}
	// A filename alone is weak evidence: not confirmed.
	only := CorrelateBoots([]BootEvidence{{Kind: EvidenceFilename, SourceID: "x", Local: local("2026-09-24 06:00:00")}})
	if len(only) != 1 || only[0].Confirmed {
		t.Fatalf("%+v", only)
	}
}

func TestEnvelopeIdentityIsDeterministicAndScoped(t *testing.T) {
	s := Scope{OrganizationID: 1, InstallationID: 11, ServerID: 1}
	r := Record{Offset: 42, Category: CategoryObjectSpawnerError, Status: StatusParsed}
	a := r.Envelope(s, FamilyRPT, "dayzps/config/x.RPT", "boot:1", time.Now(), time.Now())
	b := r.Envelope(s, FamilyRPT, "dayzps/config/x.RPT", "boot:1", time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if a.EventID != b.EventID || a.Parser != ParserVersion {
		t.Fatal("a replayed record keeps its id")
	}
	other := r.Envelope(Scope{OrganizationID: 2, InstallationID: 12, ServerID: 2}, FamilyRPT, "dayzps/config/x.RPT", "boot:1", time.Now(), time.Now())
	if other.EventID == a.EventID {
		t.Fatal("tenants never share an event id")
	}
	if NewEventID(s, "dayzps/config/x.RPT", 43, CategoryObjectSpawnerError) == a.EventID {
		t.Fatal("a different record has a different id")
	}
}

func TestRedact(t *testing.T) {
	in := `Player "A" (id=AbC123= pos=<1,2,3>) dpid=99 id 121259640 at 203.0.113.10 ni13295416_2`
	out := Redact(in)
	for _, leak := range []string{"AbC123", "dpid=99", "121259640", "203.0.113.10", "ni13295416_2"} {
		if strings.Contains(out, leak) {
			t.Fatalf("%q leaked in %q", leak, out)
		}
	}
	if len([]rune(Redact(strings.Repeat("x", 500)))) != 240 {
		t.Fatal("bounded")
	}
}
