package maprotation

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFileNameValidation(t *testing.T) {
	for _, ok := range []string{"arena1.json", "Arena-1_v2.json", "a.b.json", "X.JSON", strings.Repeat("a", 75) + ".json"} {
		if err := ValidateMapFile(ok); err != nil {
			t.Errorf("map file %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".json", "arena1", "arena1.xml", "custom/arena1.json", "../arena1.json", "..json", "a..b.json", `a\b.json`, "arena 1.json",
		"arena1.json ", "arena%2f.json", "arena1.json\x00", ".hidden.json", strings.Repeat("a", 76) + ".json", "ärena.json", "champion_shop_delivery.json", "Champion_Shop_Delivery.JSON", "champion_stadium.json", "CHAMPION_STADIUM.json"} {
		if err := ValidateMapFile(bad); err == nil {
			t.Errorf("map file %q accepted", bad)
		}
	}
	if err := ValidateSpawnFile("arena1_spawns.xml"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"arena1_spawns.json", "spawns", "a/b.xml", "../cfgplayerspawnpoints.xml", ".xml", "x.xml/"} {
		if err := ValidateSpawnFile(bad); err == nil {
			t.Errorf("spawn file %q accepted", bad)
		}
	}
	if !errors.Is(ValidateMapFile("a.xml"), ErrMapExt) || !errors.Is(ValidateSpawnFile("a.json"), ErrSpawnExt) || !errors.Is(ValidateMapFile("a/b.json"), ErrFileName) {
		t.Fatal("the reason for a refusal must be the specific one")
	}
}

const gameplayFixture = "{\n\t\"version\": 123,\n\t\"GeneralData\":\n\t{\n\t\t\"disableBaseDamage\": false,\n\t\t\"unknownFutureKey\": [1, 2.50, {\"a\": null}]\n\t},\n" +
	"\t\"WorldsData\":\n\t{\n\t\t\"lightingConfig\": 1,\n\t\t\"objectSpawnersArr\": [\n\t\t\t\"custom/shop.json\",\n\t\t\t\"custom/arena1.json\",\n\t\t\t\"custom/loadouts.json\"\n\t\t],\n" +
	"\t\t\"environmentMinTemps\": [-3, -2, 0, 4, 9, 14, 18, 17, 12, 7, 4, 0],\n\t\t\"wetnessWeightModifiers\": [1.0, 1.0, 1.33, 1.66, 2.0]\n\t},\n\t\"PlayerData\": {\"x\": 1.50}\n}\n"

func spawners(t *testing.T, b []byte) []string {
	t.Helper()
	var w struct {
		WorldsData struct {
			ObjectSpawnersArr []string `json:"objectSpawnersArr"`
		}
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatalf("result does not parse: %v", err)
	}
	return w.WorldsData.ObjectSpawnersArr
}

func TestEditSpawnersReplacesOnlyOwnMapEntries(t *testing.T) {
	owned := []string{"arena1.json", "arena2.json", "arena3.json"}
	ed, err := EditSpawners([]byte(gameplayFixture), owned, "arena2.json")
	if err != nil || !ed.Changed {
		t.Fatalf("edit: %v changed=%v", err, ed.Changed)
	}
	if got := strings.Join(spawners(t, ed.Out), ","); got != "custom/shop.json,custom/arena2.json,custom/loadouts.json" {
		t.Fatalf("entries: %s", got)
	}
	// Every byte outside the array is kept: the result is the input with one line changed.
	want := strings.Replace(gameplayFixture, "custom/arena1.json", "custom/arena2.json", 1)
	if string(ed.Out) != want {
		t.Fatalf("formatting or another key changed:\n%s", ed.Out)
	}
	if strings.Join(ed.Before, ",") != "custom/shop.json,custom/arena1.json,custom/loadouts.json" || len(ed.After) != 3 {
		t.Fatalf("before/after: %v %v", ed.Before, ed.After)
	}
	// Applying it again changes nothing (idempotent).
	again, err := EditSpawners(ed.Out, owned, "arena2.json")
	if err != nil || again.Changed || !bytes.Equal(again.Out, ed.Out) {
		t.Fatalf("second edit: %v changed=%v", err, again.Changed)
	}
}

func TestEditSpawnersCollapsesSeveralOwnEntriesAndMatchesLoosely(t *testing.T) {
	in := `{"WorldsData": {"objectSpawnersArr": ["./custom/Arena1.JSON", "custom/keep.json", "custom\\arena3.json", "other/arena1.json"], "k": true}}`
	ed, err := EditSpawners([]byte(in), []string{"arena1.json", "arena3.json"}, "arena2.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spawners(t, ed.Out), ","); got != "custom/arena2.json,custom/keep.json,other/arena1.json" {
		t.Fatalf("entries: %s", got)
	}
	if want := `{"WorldsData": {"objectSpawnersArr": ["custom/arena2.json", "custom/keep.json", "other/arena1.json"], "k": true}}`; string(ed.Out) != want {
		t.Fatalf("single-line style not kept: %s", ed.Out)
	}
}

// The Stadium adds Champion's own reserved file (which no owner can configure as a map) once and
// keeps everything else; the same call on the result changes nothing.
func TestEditChampionSpawnerAddsTheReservedFileOnce(t *testing.T) {
	if !IsReservedName("champion_stadium.json") || !IsReservedName("Champion_Shop_Delivery.JSON") || IsReservedName("arena1.json") {
		t.Fatal("reserved names")
	}
	if _, err := EditChampionSpawner([]byte(gameplayFixture), "arena1.json"); !errors.Is(err, ErrFileName) {
		t.Fatalf("an owner's map name must be refused here: %v", err)
	}
	ed, err := EditChampionSpawner([]byte(gameplayFixture), "champion_stadium.json")
	if err != nil || !ed.Changed {
		t.Fatalf("edit: %v changed=%v", err, ed.Changed)
	}
	if got := strings.Join(spawners(t, ed.Out), ","); got != "custom/shop.json,custom/arena1.json,custom/loadouts.json,custom/champion_stadium.json" {
		t.Fatalf("entries: %s", got)
	}
	again, err := EditChampionSpawner(ed.Out, "champion_stadium.json")
	if err != nil || again.Changed || !bytes.Equal(again.Out, ed.Out) {
		t.Fatalf("second edit: %v changed=%v", err, again.Changed)
	}
	// A rotation switch never removes it, whatever the owner's maps are called.
	sw, err := EditSpawners(ed.Out, []string{"arena1.json", "champion_stadium.json"}, "arena1.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spawners(t, sw.Out), ","); got != "custom/shop.json,custom/arena1.json,custom/loadouts.json,custom/champion_stadium.json" {
		t.Fatalf("after a switch: %s", got)
	}
}

func TestEditSpawnersAppendsWhenNoOwnEntry(t *testing.T) {
	ed, err := EditSpawners([]byte(gameplayFixture), []string{"arena2.json"}, "arena2.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spawners(t, ed.Out), ","); got != "custom/shop.json,custom/arena1.json,custom/loadouts.json,custom/arena2.json" {
		t.Fatalf("entries: %s", got)
	}
	if !strings.Contains(string(ed.Out), "\t\t\t\"custom/loadouts.json\",\n\t\t\t\"custom/arena2.json\"\n\t\t],") {
		t.Fatalf("indentation not kept:\n%s", ed.Out)
	}
}

func TestEditSpawnersHandlesMissingAndEmptyArray(t *testing.T) {
	for name, in := range map[string]string{
		"missing key":         "{\r\n  \"version\": 1,\r\n  \"WorldsData\": {\r\n    \"lightingConfig\": 0,\r\n    \"temps\": [1, 2]\r\n  }\r\n}",
		"empty WorldsData":    `{"WorldsData": {}, "version": 2}`,
		"empty array":         "{\"WorldsData\": {\"objectSpawnersArr\": [], \"a\": 1}}",
		"empty spanned array": "{\n\t\"WorldsData\": {\n\t\t\"objectSpawnersArr\": [\n\t\t],\n\t\t\"a\": 1\n\t}\n}",
		"with a BOM":          "\xEF\xBB\xBF{\"WorldsData\": {\"a\": 1}}",
	} {
		ed, err := EditSpawners([]byte(in), nil, "arena1.json")
		if err != nil || !ed.Changed {
			t.Fatalf("%s: %v changed=%v", name, err, ed.Changed)
		}
		out := bytes.TrimPrefix(ed.Out, utf8BOM)
		if got := spawners(t, out); len(got) != 1 || got[0] != "custom/arena1.json" {
			t.Fatalf("%s: entries %v", name, got)
		}
		// Every other key and value survives.
		var before, after map[string]any
		_ = json.Unmarshal(bytes.TrimPrefix([]byte(in), utf8BOM), &before)
		_ = json.Unmarshal(out, &after)
		delete(after["WorldsData"].(map[string]any), "objectSpawnersArr")
		delete(before["WorldsData"].(map[string]any), "objectSpawnersArr")
		a, _ := json.Marshal(after)
		b, _ := json.Marshal(before)
		if !bytes.Equal(a, b) {
			t.Fatalf("%s: other keys changed: %s vs %s", name, a, b)
		}
		if strings.HasPrefix(in, "\xEF\xBB\xBF") != bytes.HasPrefix(ed.Out, utf8BOM) {
			t.Fatalf("%s: the BOM was not kept", name)
		}
	}
	crlf, _ := EditSpawners([]byte("{\r\n  \"WorldsData\": {\r\n    \"lightingConfig\": 0\r\n  }\r\n}"), nil, "arena1.json")
	if want := "{\r\n  \"WorldsData\": {\r\n    \"objectSpawnersArr\": [\"custom/arena1.json\"],\r\n    \"lightingConfig\": 0\r\n  }\r\n}"; string(crlf.Out) != want {
		t.Fatalf("inserted key does not follow the file's layout: %q", crlf.Out)
	}
}

func TestEditSpawnersRefusesWhatItCannotEditSafely(t *testing.T) {
	for name, in := range map[string]string{
		"malformed":            `{"WorldsData": {"objectSpawnersArr": ["custom/a.json",]}}`,
		"truncated":            `{"WorldsData": {"objectSpawnersArr": ["custom/a.json"`,
		"empty":                ``,
		"comment":              "{\n// note\n\"WorldsData\": {}}",
		"not an object":        `["WorldsData"]`,
		"no WorldsData":        `{"version": 1}`,
		"WorldsData not obj":   `{"WorldsData": []}`,
		"array is a string":    `{"WorldsData": {"objectSpawnersArr": "custom/a.json"}}`,
		"array holds a number": `{"WorldsData": {"objectSpawnersArr": ["custom/a.json", 3]}}`,
		"array holds null":     `{"WorldsData": {"objectSpawnersArr": [null]}}`,
		"array holds an array": `{"WorldsData": {"objectSpawnersArr": [["custom/a.json"]]}}`,
	} {
		ed, err := EditSpawners([]byte(in), []string{"arena1.json"}, "arena1.json")
		if err == nil || ed.Changed || ed.Out != nil {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := EditSpawners([]byte(`{"WorldsData": {}}`), nil, "../evil.json"); err == nil {
		t.Fatal("an unsafe map file name must be refused")
	}
	if _, err := EditSpawners([]byte(`not json`), nil, "a.json"); !errors.Is(err, ErrGameplayInvalid) {
		t.Fatalf("malformed JSON: %v", err)
	}
	if _, err := EditSpawners([]byte(`{"a":1}`), nil, "a.json"); !errors.Is(err, ErrGameplayStructure) {
		t.Fatalf("missing WorldsData: %v", err)
	}
}

// A key named objectSpawnersArr outside WorldsData, or inside a string, is never touched.
func TestEditSpawnersOnlyTouchesWorldsData(t *testing.T) {
	in := `{"note": "\"objectSpawnersArr\": [\"custom/arena1.json\"]", "Other": {"objectSpawnersArr": ["custom/arena1.json"]}, "WorldsData": {"nested": {"objectSpawnersArr": ["custom/arena1.json"]}, "objectSpawnersArr": ["custom/arena1.json"]}}`
	ed, err := EditSpawners([]byte(in), []string{"arena1.json"}, "arena2.json")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(in, `"objectSpawnersArr": ["custom/arena1.json"]}}`, `"objectSpawnersArr": ["custom/arena2.json"]}}`, 1)
	if string(ed.Out) != want {
		t.Fatalf("wrong key edited: %s", ed.Out)
	}
}

func TestValidateSpawnXML(t *testing.T) {
	good := "<?xml version=\"1.0\" encoding=\"UTF-8\" standalone=\"yes\"?>\n<playerspawnpoints>\n\t<fresh>\n\t\t<spawn_params><min_dist_infected>30</min_dist_infected></spawn_params>\n" +
		"\t\t<generator_posbubbles>\n\t\t\t<pos x=\"7500.5\" z=\"7500\" />\n\t\t</generator_posbubbles>\n\t</fresh>\n</playerspawnpoints>\n"
	if err := ValidateSpawnXML([]byte(good)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSpawnXML([]byte(strings.Replace(good, "UTF-8", "windows-1252", 1))); err != nil {
		t.Fatalf("a declared single-byte encoding: %v", err)
	}
	for name, c := range map[string]struct {
		in   string
		want error
	}{
		"empty":        {"", ErrSpawnXML},
		"not xml":      {"{}", ErrSpawnXML},
		"unclosed":     {"<playerspawnpoints><fresh><pos x=\"1\" z=\"2\"/></fresh>", ErrSpawnXML},
		"wrong root":   {"<types><pos x=\"1\" z=\"2\"/></types>", ErrSpawnRoot},
		"no positions": {"<playerspawnpoints><fresh></fresh></playerspawnpoints>", ErrSpawnNoPos},
		"pos no z":     {"<playerspawnpoints><fresh><pos x=\"1\"/></fresh></playerspawnpoints>", ErrSpawnNoPos},
	} {
		if err := ValidateSpawnXML([]byte(c.in)); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
	if ValidateMapJSON([]byte(`{"Objects": []}`)) != nil || ValidateMapJSON([]byte(`[]`)) == nil || ValidateMapJSON([]byte(`{`)) == nil {
		t.Fatal("map file JSON check")
	}
}

func fiveMaps() []Map {
	return []Map{
		{ID: 11, Name: "A", MapFile: "a.json", SpawnFile: "a.xml", Enabled: true, Position: 0},
		{ID: 12, Name: "B", MapFile: "b.json", SpawnFile: "b.xml", Enabled: true, Position: 1},
		{ID: 13, Name: "C", MapFile: "c.json", SpawnFile: "c.xml", Enabled: false, Position: 2},
		{ID: 14, Name: "D", MapFile: "d.json", SpawnFile: "d.xml", Enabled: true, Position: 3},
	}
}

func TestSequenceOrderWrapsAround(t *testing.T) {
	maps := fiveMaps()
	for current, want := range map[int64]int64{0: 11, 11: 12, 12: 14, 14: 11, 13: 14, 999: 11} {
		got, ok := NextInSequence(maps, current)
		if !ok || got.ID != want {
			t.Errorf("after %d: got %d, want %d", current, got.ID, want)
		}
	}
	if _, ok := NextInSequence(nil, 0); ok {
		t.Fatal("no maps, no next map")
	}
	// The order the rotation reaches the maps in, starting after the current one.
	ids := func(ms []Map) string {
		var s []string
		for _, m := range ms {
			s = append(s, m.Name)
		}
		return strings.Join(s, "")
	}
	if got := ids(RotationOrder(maps, 12)); got != "DAB" {
		t.Fatalf("rotation order after B: %s", got)
	}
	if got := ids(RotationOrder(maps, 0)); got != "ABD" {
		t.Fatalf("rotation order with no current map: %s", got)
	}
	if got := ids(VoteOptions(maps, 12)); got != "DA" {
		t.Fatalf("vote options leave out the current map when two others exist: %s", got)
	}
	if got := ids(VoteOptions(maps[:2], 11)); got != "BA" {
		t.Fatalf("with two maps both are options: %s", got)
	}
}

func TestRandomOrderNeverRepeatsTheCurrentMap(t *testing.T) {
	maps := fiveMaps()
	seen := map[int64]bool{}
	for n := 0; n < 200; n++ {
		got, ok := NextRandom(maps, 12, func(k int) int { return n % k })
		if !ok || got.ID == 12 || !got.Enabled {
			t.Fatalf("random pick %d: %+v", n, got)
		}
		seen[got.ID] = true
	}
	if !seen[11] || !seen[14] || len(seen) != 2 {
		t.Fatalf("every other enabled map must be reachable: %v", seen)
	}
	// With a single other map it is that map; with only the current map it stays.
	if got, _ := NextRandom(maps[:2], 11, func(int) int { return 0 }); got.ID != 12 {
		t.Fatalf("two maps: %d", got.ID)
	}
	if got, ok := NextRandom(maps[:1], 11, nil); !ok || got.ID != 11 {
		t.Fatalf("one map: %+v %v", got, ok)
	}
	if got, _ := NextRandom(maps, 12, func(int) int { return 99 }); got.ID != 11 {
		t.Fatal("an out-of-range pick falls back to the first candidate")
	}
}

func TestTallyAndTieBreak(t *testing.T) {
	options := []int64{14, 11, 12} // rotation order
	counts, winner, total := Tally(options, []int64{11, 12, 12, 11, 14, 999})
	if total != 5 || winner != 11 || counts[11] != 2 || counts[12] != 2 || counts[14] != 1 {
		t.Fatalf("tie goes to the earlier map in rotation order: winner %d total %d %v", winner, total, counts)
	}
	if _, w, _ := Tally(options, []int64{12, 12, 11}); w != 12 {
		t.Fatalf("most votes wins: %d", w)
	}
	if _, w, _ := Tally(options, []int64{14, 11, 12}); w != 14 {
		t.Fatalf("three-way tie: %d", w)
	}
	if c, w, n := Tally(options, nil); w != 0 || n != 0 || len(c) != 3 {
		t.Fatalf("no votes, no winner: %d %d", w, n)
	}

	maps := fiveMaps()
	// No votes: the rotation decides.
	if m, by, _ := ResolveVote(OrderSequence, maps, 12, 0, []int64{14, 11}, map[int64]int{}, nil); m.ID != 14 || by != DecidedRotation {
		t.Fatalf("no votes: %d %s", m.ID, by)
	}
	if m, by, _ := ResolveVote(OrderSequence, maps, 12, 0, []int64{14, 11}, map[int64]int{14: 1, 11: 1}, nil); m.ID != 14 || by != DecidedVote {
		t.Fatalf("tie: %d %s", m.ID, by)
	}
	if m, by, _ := ResolveVote(OrderSequence, maps, 12, 0, []int64{14, 11}, map[int64]int{11: 3, 14: 1}, nil); m.ID != 11 || by != DecidedVote {
		t.Fatalf("winner: %d %s", m.ID, by)
	}
	// A staff choice overrides the vote; a map switched off during the vote cannot win.
	if m, by, _ := ResolveVote(OrderSequence, maps, 12, 11, []int64{14, 11}, map[int64]int{14: 9}, nil); m.ID != 11 || by != DecidedStaff {
		t.Fatalf("staff: %d %s", m.ID, by)
	}
	if m, by, _ := ResolveVote(OrderSequence, maps, 12, 0, []int64{13, 11}, map[int64]int{13: 9, 11: 1}, nil); m.ID != 11 || by != DecidedVote {
		t.Fatalf("disabled option: %d %s", m.ID, by)
	}
}

func TestRestartCounting(t *testing.T) {
	for _, c := range []struct{ every, since, until int }{{1, 0, 1}, {1, 5, 1}, {2, 0, 2}, {2, 1, 1}, {2, 2, 1}, {3, 0, 3}, {3, 1, 2}, {3, 2, 1}, {3, 7, 1}, {0, 0, 1}} {
		if got := RestartsUntilSwitch(c.every, c.since); got != c.until {
			t.Errorf("every %d, %d since the switch: %d restarts left, want %d", c.every, c.since, got, c.until)
		}
		if FinalPeriod(c.every, c.since) != (c.until == 1) {
			t.Errorf("final period for every %d since %d", c.every, c.since)
		}
	}
}

// The planner over three restarts with everyRestarts = 2: boots are counted, nothing happens in
// the first period, and the second period decides and applies.
func TestPlanCountsRestartsAndWaitsForTheFinalPeriod(t *testing.T) {
	maps := fiveMaps()
	cfg := Settings{EveryRestarts: 2, Order: OrderSequence, VoteMinutes: 30}
	t0 := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	st := State{Phase: PhaseIdle}
	if s := Plan(cfg, st, maps, Observation{}, t0, nil); s.Kind != StepNone {
		t.Fatalf("no boot recorded: %s", s.Kind)
	}
	if s := Plan(cfg, st, maps, Observation{BootFile: "b1", BootAt: t0}, t0, nil); s.Kind != StepBaseline {
		t.Fatalf("first boot: %s", s.Kind)
	}
	st.LastBootFile, st.PhaseStartedAt = "b1", t0
	// Period 1 of 2: nothing, however late it gets.
	if s := Plan(cfg, st, maps, Observation{BootFile: "b1", BootAt: t0}, t0.Add(5*time.Hour), nil); s.Kind != StepNone {
		t.Fatalf("not the final period: %s", s.Kind)
	}
	if s := Plan(cfg, st, maps, Observation{BootFile: "b2", BootAt: t0.Add(6 * time.Hour)}, t0.Add(6*time.Hour), nil); s.Kind != StepRestart {
		t.Fatalf("new boot: %s", s.Kind)
	}
	st.LastBootFile, st.RestartsSinceSwitch, st.PhaseStartedAt = "b2", 1, t0.Add(6*time.Hour)
	obs := Observation{BootFile: "b2", BootAt: st.PhaseStartedAt}
	// Final period, no schedule known, no vote: decide voteMinutes after the period began.
	if s := Plan(cfg, st, maps, obs, st.PhaseStartedAt.Add(29*time.Minute), nil); s.Kind != StepNone {
		t.Fatalf("before the apply point: %s", s.Kind)
	}
	s := Plan(cfg, st, maps, obs, st.PhaseStartedAt.Add(30*time.Minute), nil)
	if s.Kind != StepDecide || s.Map.ID != 11 || s.DecidedBy != DecidedRotation {
		t.Fatalf("decide: %+v", s)
	}
	st.Phase, st.NextMapID, st.NextDecidedBy = PhaseDecided, 11, DecidedRotation
	if s := Plan(cfg, st, maps, obs, st.PhaseStartedAt.Add(31*time.Minute), nil); s.Kind != StepApply {
		t.Fatalf("apply: %s", s.Kind)
	}
	st.Phase = PhaseDone
	if s := Plan(cfg, st, maps, obs, st.PhaseStartedAt.Add(3*time.Hour), nil); s.Kind != StepNone {
		t.Fatalf("after the switch nothing happens until the restart: %s", s.Kind)
	}
	// Fewer than two enabled maps: never anything.
	if s := Plan(cfg, State{LastBootFile: "b2", RestartsSinceSwitch: 5, Phase: PhaseIdle}, maps[:1], obs, t0.Add(99*time.Hour), nil); s.Kind != StepNone {
		t.Fatalf("one map: %s", s.Kind)
	}
}

func TestPlanVoteWindows(t *testing.T) {
	maps := fiveMaps()
	cfg := Settings{EveryRestarts: 1, Order: OrderSequence, VoteEnabled: true, VoteMinutes: 30}
	boot := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	st := State{LastBootFile: "b1", Phase: PhaseIdle, PhaseStartedAt: boot, CurrentMapID: 12}

	// Schedule known: the vote opens 35 minutes before the restart and closes 5 minutes before.
	restart := boot.Add(4 * time.Hour)
	obs := Observation{BootFile: "b1", BootAt: boot, NextRestart: &restart}
	if s := Plan(cfg, st, maps, obs, restart.Add(-36*time.Minute), nil); s.Kind != StepNone {
		t.Fatalf("before the window: %s", s.Kind)
	}
	s := Plan(cfg, st, maps, obs, restart.Add(-35*time.Minute), nil)
	if s.Kind != StepOpenVote || !s.ClosesAt.Equal(restart.Add(-5*time.Minute)) || len(s.Options) != 2 || s.Options[0].ID != 14 {
		t.Fatalf("open: %+v", s)
	}
	voting := st
	voting.Phase, voting.VoteOpen, voting.VoteClosesAt = PhaseVoting, true, s.ClosesAt
	if s := Plan(cfg, voting, maps, obs, restart.Add(-6*time.Minute), nil); s.Kind != StepNone {
		t.Fatalf("vote still open: %s", s.Kind)
	}
	if s := Plan(cfg, voting, maps, obs, restart.Add(-5*time.Minute), nil); s.Kind != StepCloseVote {
		t.Fatalf("close: %s", s.Kind)
	}
	// The bot only looks again 3 minutes before the restart: too late for a vote, the rotation decides.
	if s := Plan(cfg, st, maps, obs, restart.Add(-3*time.Minute), nil); s.Kind != StepDecide || s.DecidedBy != DecidedRotation || s.Map.ID != 14 {
		t.Fatalf("late: %+v", s)
	}
	// Decided, but the restart is less than two minutes away: nothing is written now.
	decided := st
	decided.Phase, decided.NextMapID, decided.NextDecidedBy = PhaseDecided, 14, DecidedVote
	if s := Plan(cfg, decided, maps, obs, restart.Add(-90*time.Second), nil); s.Kind != StepNone {
		t.Fatalf("too close to the restart: %s", s.Kind)
	}
	if s := Plan(cfg, decided, maps, obs, restart.Add(-4*time.Minute), nil); s.Kind != StepApply {
		t.Fatalf("apply: %s", s.Kind)
	}

	// Schedule unknown: the vote opens as soon as the final period begins, for 30 minutes from then.
	unknown := Observation{BootFile: "b1", BootAt: boot}
	now := boot.Add(2 * time.Minute)
	if s := Plan(cfg, st, maps, unknown, now, nil); s.Kind != StepOpenVote || !s.ClosesAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("open without a schedule: %+v", s)
	}

	// A staff choice skips the vote and is applied at the same point a vote would close.
	staff := st
	staff.StaffNextMapID = 11
	if s := Plan(cfg, staff, maps, obs, restart.Add(-20*time.Minute), nil); s.Kind != StepNone {
		t.Fatalf("staff choice waits for the apply point: %s", s.Kind)
	}
	if s := Plan(cfg, staff, maps, obs, restart.Add(-5*time.Minute), nil); s.Kind != StepDecide || s.Map.ID != 11 || s.DecidedBy != DecidedStaff {
		t.Fatalf("staff: %+v", s)
	}
	// ...and one made after the vote decided still wins, until the files are written.
	late := decided
	late.StaffNextMapID = 11
	if s := Plan(cfg, late, maps, obs, restart.Add(-4*time.Minute), nil); s.Kind != StepDecide || s.Map.ID != 11 || s.DecidedBy != DecidedStaff {
		t.Fatalf("late staff choice: %+v", s)
	}
	// A decision carried over from a failed switch is not voted on again.
	carried := st
	carried.NextMapID, carried.NextDecidedBy = 14, DecidedVote
	if s := Plan(cfg, carried, maps, unknown, boot.Add(time.Minute), nil); s.Kind != StepNone {
		t.Fatalf("carried decision waits: %s", s.Kind)
	}
	if s := Plan(cfg, carried, maps, unknown, boot.Add(30*time.Minute), nil); s.Kind != StepDecide || s.Map.ID != 14 || s.DecidedBy != DecidedVote {
		t.Fatalf("carried decision: %+v", s)
	}
	// Random order uses the pick.
	random := Settings{EveryRestarts: 1, Order: OrderRandom, VoteMinutes: 5}
	if s := Plan(random, st, maps, unknown, boot.Add(5*time.Minute), func(n int) int { return n - 1 }); s.Kind != StepDecide || s.Map.ID != 14 {
		t.Fatalf("random: %+v", s)
	}
}

// The restart count with "fresh characters on every map switch": Champion starts the server itself
// a few minutes before the scheduled restart, and both boots are one restart.
func TestRestartCountingAroundAWipeRestart(t *testing.T) {
	wipe := time.Date(2026, 10, 4, 11, 56, 0, 0, time.UTC)
	type boot struct {
		after time.Duration // after the wipe restart (or after `wipe` when there was none)
		want  RestartEffect
	}
	cases := []struct {
		name      string
		wipeOn    bool
		applied   bool // a switch was applied before the first boot
		boots     []boot
		wantCount int // restarts counted since the switch became active
		wantMaps  int // how often a switch was activated
	}{
		{"wipe off: the boot after a switch activates it, the next is counted", false, true,
			[]boot{{2 * time.Minute, RestartActivate}, {6 * time.Minute, RestartCount}}, 1, 1},
		{"wipe off, no switch: every boot is counted", false, false,
			[]boot{{2 * time.Minute, RestartCount}, {6 * time.Minute, RestartCount}, {25 * time.Minute, RestartCount}}, 3, 0},
		{"wipe on: boots at +2 and +6 minutes are one restart", true, true,
			[]boot{{2 * time.Minute, RestartActivate}, {6 * time.Minute, RestartSame}}, 0, 1},
		{"wipe on: only the scheduled boot is seen", true, true,
			[]boot{{6 * time.Minute, RestartActivate}}, 0, 1},
		{"wipe on: a boot at +25 minutes counts normally", true, true,
			[]boot{{2 * time.Minute, RestartActivate}, {25 * time.Minute, RestartCount}}, 1, 1},
		{"wipe on: +2, +6, then +25 and later count", true, true,
			[]boot{{2 * time.Minute, RestartActivate}, {6 * time.Minute, RestartSame}, {25 * time.Minute, RestartCount}, {4 * time.Hour, RestartCount}}, 2, 1},
		{"wipe on: the edge of the window", true, true,
			[]boot{{time.Minute, RestartActivate}, {20 * time.Minute, RestartSame}, {20*time.Minute + time.Second, RestartCount}}, 1, 1},
		{"wipe on but the switch failed: boots in the window are still one restart", true, false,
			[]boot{{2 * time.Minute, RestartSame}, {6 * time.Minute, RestartSame}, {30 * time.Minute, RestartCount}}, 1, 0},
	}
	cfg := Settings{EveryRestarts: 3, Order: OrderSequence}
	maps := []Map{{ID: 1, Name: "A", MapFile: "a.json", SpawnFile: "a.xml", Enabled: true, Position: 0}, {ID: 2, Name: "B", MapFile: "b.json", SpawnFile: "b.xml", Enabled: true, Position: 1}}
	for _, c := range cases {
		st := State{LastBootFile: "boot0", Phase: PhaseDone, AwaitingActivation: c.applied}
		if c.wipeOn {
			st.WipeRestartAt = wipe
		}
		count, activated := 0, 0
		for i, b := range c.boots {
			at := wipe.Add(b.after)
			file := "boot" + string(rune('1'+i))
			step := Plan(cfg, st, maps, Observation{BootFile: file, BootAt: at}, at.Add(30*time.Second), nil)
			if step.Kind != StepRestart || step.Restart != b.want {
				t.Fatalf("%s: boot %d at +%s: got %s/%s, want %s", c.name, i+1, b.after, step.Kind, step.Restart, b.want)
			}
			switch step.Restart {
			case RestartActivate:
				activated++
				count = 0
				st.AwaitingActivation = false
			case RestartCount:
				count++
			}
			// What the worker stores for every kind of restart: the boot, and the period begins.
			st.LastBootFile, st.Phase, st.PhaseStartedAt, st.RestartsSinceSwitch = file, PhaseIdle, at, count
		}
		if count != c.wantCount || activated != c.wantMaps {
			t.Fatalf("%s: counted %d restarts and %d activations, want %d and %d", c.name, count, activated, c.wantCount, c.wantMaps)
		}
	}
}

// Between Champion's own restart and the scheduled one nothing is decided or written, so those few
// minutes never become a period of their own. Without a wipe restart nothing is held back.
func TestNoPeriodStartsInsideTheWipeWindow(t *testing.T) {
	wipe := time.Date(2026, 10, 4, 11, 56, 0, 0, time.UTC)
	bootAt := wipe.Add(2 * time.Minute)
	scheduled := wipe.Add(4 * time.Minute)
	cfg := Settings{EveryRestarts: 1, Order: OrderSequence}
	maps := []Map{{ID: 1, Name: "A", MapFile: "a.json", SpawnFile: "a.xml", Enabled: true, Position: 0}, {ID: 2, Name: "B", MapFile: "b.json", SpawnFile: "b.xml", Enabled: true, Position: 1}}
	st := State{CurrentMapID: 1, LastBootFile: "boot1", Phase: PhaseIdle, PhaseStartedAt: bootAt}
	obs := Observation{BootFile: "boot1", BootAt: bootAt, NextRestart: &scheduled}
	now := bootAt.Add(30 * time.Second)

	// As today (no wipe restart): with the restart this close the rotation decides at once.
	if s := Plan(cfg, st, maps, obs, now, nil); s.Kind != StepDecide {
		t.Fatalf("without a wipe restart: %s, want %s", s.Kind, StepDecide)
	}
	st.WipeRestartAt = wipe
	if s := Plan(cfg, st, maps, obs, now, nil); s.Kind != StepNone {
		t.Fatalf("inside the window: %s, want %s", s.Kind, StepNone)
	}
	st.Phase, st.NextMapID, st.NextDecidedBy = PhaseDecided, 2, DecidedRotation
	if s := Plan(cfg, st, maps, Observation{BootFile: "boot1", BootAt: bootAt}, now, nil); s.Kind != StepNone {
		t.Fatalf("inside the window nothing is written: %s", s.Kind)
	}
	// After the window the planner carries on as always.
	if s := Plan(cfg, st, maps, Observation{BootFile: "boot1", BootAt: bootAt}, wipe.Add(WipeRestartWindow+time.Minute), nil); s.Kind != StepApply {
		t.Fatalf("after the window: %s, want %s", s.Kind, StepApply)
	}
	if !InWipeWindow(wipe, wipe) || InWipeWindow(wipe, wipe.Add(-time.Second)) || InWipeWindow(time.Time{}, wipe) {
		t.Fatal("the window starts at the wipe restart and does not exist without one")
	}
}
