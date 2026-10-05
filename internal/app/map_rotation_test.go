package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/entitlements"
	"github.com/yourname/dayz-killfeed/internal/featureflags"
	"github.com/yourname/dayz-killfeed/internal/maprotation"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Map rotation (docs/MAP_ROTATION.md): the parts of the API layer that need no database. The
// handlers themselves are exercised against PostgreSQL in map_rotation_integration_test.go.

func TestMapRotationIsOffByDefaultAtEveryLevel(t *testing.T) {
	// 1. The feature flag: off without the environment switch, and it is a catalog flag the
	// platform owner can override per installation.
	a := &App{Config: &config.Config{}}
	if a.mapRotationFlag(7) || (&App{}).mapRotationFlag(7) {
		t.Fatal("the map_rotation flag must default to off")
	}
	if !featureflags.Known(featureflags.MapRotation) {
		t.Fatal("map_rotation must be an Owner Hub feature flag")
	}
	a.Config.MapRotationEnabled = true
	if !a.mapRotationFlag(7) {
		t.Fatal("CHAMPION_MAP_ROTATION_ENABLED turns the default on")
	}
	// 2. The plan: Survivor does not include it once plans are enforced.
	prev := entitlements.Enforced()
	defer entitlements.SetEnforced(prev)
	entitlements.SetEnforced(true)
	if entitlements.Has(entitlements.ForOrganization(7, entitlements.PlanSurvivor), entitlements.MapRotation) || !entitlements.Has(entitlements.ForOrganization(7, "PREMIUM"), entitlements.MapRotation) {
		t.Fatal("map rotation is a Champion plan feature")
	}
	// 3. The owner's switch: a server nobody configured is not enabled.
	view := toMapRotationAdminDTO(repository.MapRotationSnapshot{Settings: repository.MapRotationSettings{EveryRestarts: 1, Order: "SEQUENCE", VoteMinutes: 30, Phase: "IDLE"}}, "", time.Now())
	if view.Enabled || !view.Available {
		t.Fatalf("default view: %+v", view)
	}
}

func TestMapRotationCapabilityIsOwnerOnly(t *testing.T) {
	if permissions.Allows(permissions.LevelAdministrator, permissions.CapMapRotationManage) || !permissions.Allows(permissions.LevelOwner, permissions.CapMapRotationManage) {
		t.Fatal("MAP_ROTATION_MANAGE is an Owner capability")
	}
	found := false
	for _, c := range permissions.CapabilitiesForLevel(permissions.LevelOwner) {
		found = found || c == "MAP_ROTATION_MANAGE"
	}
	if !found {
		t.Fatal(".../admin/me must list MAP_ROTATION_MANAGE for an owner")
	}
}

func str(s string) *string { return &s }

const maprotationMaxSpawnBytes = maprotation.MaxSpawnBytes

// A save may carry five spawn files of the maximum size, and not much more.
func TestMapRotationBodyLimit(t *testing.T) {
	if mapRotationMaxBody < maprotation.MaxMaps*maprotation.MaxSpawnBytes+64<<10 || mapRotationMaxBody > 2*maprotation.MaxMaps*maprotation.MaxSpawnBytes {
		t.Fatalf("body limit: %d bytes", mapRotationMaxBody)
	}
}

const testSpawnXML = `<playerspawnpoints><fresh><generator_posbubbles><pos x="100" z="100"/></generator_posbubbles></fresh></playerspawnpoints>`

func validBody() mapRotationBody {
	maps := []mapRotationMapBody{
		{Name: "Arena 1", MapFile: "arena1.json", SpawnFile: "arena1_spawns.xml", SpawnXML: str(testSpawnXML), ImageURL: str("https://cdn.example.com/a.png"), Enabled: true},
		{Name: "Arena 2", MapFile: "arena2.json", SpawnFile: "arena2_spawns.xml", Enabled: true},
	}
	return mapRotationBody{Enabled: true, EveryRestarts: 2, Order: "random", VoteEnabled: true, VoteMinutesBeforeRestart: 30, AnnounceChannelID: str(" 123456789012345678 "), Maps: &maps}
}

func TestValidateMapRotationBody(t *testing.T) {
	in, problem := validateMapRotationBody(validBody())
	if problem != "" || in.Order != "RANDOM" || len(in.Maps) != 2 || in.AnnounceChannelID == nil || *in.AnnounceChannelID != "123456789012345678" || in.Maps[1].ImageURL != nil {
		t.Fatalf("valid body: %q", problem)
	}
	// spawnXml is carried as sent; left out, there is nothing to store (the stored contents stay).
	if string(in.Maps[0].SpawnXML) != testSpawnXML || in.Maps[1].SpawnXML != nil {
		t.Fatal("spawnXml was not carried into the save as sent")
	}
	// Switched off, any number of maps from 0 is fine.
	off := validBody()
	off.Enabled = false
	none := []mapRotationMapBody{}
	off.Maps = &none
	if _, problem := validateMapRotationBody(off); problem != "" {
		t.Fatalf("no maps while off: %q", problem)
	}
	empty := validBody()
	empty.AnnounceChannelID = str("  ")
	if in, problem := validateMapRotationBody(empty); problem != "" || in.AnnounceChannelID != nil {
		t.Fatalf("an empty channel is no channel: %q", problem)
	}

	withMap := func(m mapRotationMapBody) mapRotationBody {
		b := validBody()
		maps := append(*b.Maps, m)
		b.Maps = &maps
		return b
	}
	bad := map[string]mapRotationBody{
		"everyRestarts 0":      func() mapRotationBody { b := validBody(); b.EveryRestarts = 0; return b }(),
		"everyRestarts 4":      func() mapRotationBody { b := validBody(); b.EveryRestarts = 4; return b }(),
		"order":                func() mapRotationBody { b := validBody(); b.Order = "LOOP"; return b }(),
		"vote minutes low":     func() mapRotationBody { b := validBody(); b.VoteMinutesBeforeRestart = 4; return b }(),
		"vote minutes high":    func() mapRotationBody { b := validBody(); b.VoteMinutesBeforeRestart = 121; return b }(),
		"maps missing":         func() mapRotationBody { b := validBody(); b.Maps = nil; return b }(),
		"channel with a space": func() mapRotationBody { b := validBody(); b.AnnounceChannelID = str("12 34"); return b }(),
		"one enabled map":      func() mapRotationBody { b := validBody(); (*b.Maps)[1].Enabled = false; return b }(),
		"six maps": func() mapRotationBody {
			b := validBody()
			maps := *b.Maps
			for _, n := range []string{"c", "d", "e", "f"} {
				maps = append(maps, mapRotationMapBody{Name: n, MapFile: n + ".json", SpawnFile: n + ".xml", Enabled: true})
			}
			b.Maps = &maps
			return b
		}(),
		"name empty":          withMap(mapRotationMapBody{Name: "  ", MapFile: "c.json", SpawnFile: "c.xml"}),
		"name too long":       withMap(mapRotationMapBody{Name: strings.Repeat("x", 61), MapFile: "c.json", SpawnFile: "c.xml"}),
		"map file path":       withMap(mapRotationMapBody{Name: "C", MapFile: "custom/c.json", SpawnFile: "c.xml"}),
		"map file dotdot":     withMap(mapRotationMapBody{Name: "C", MapFile: "..json", SpawnFile: "c.xml"}),
		"map file extension":  withMap(mapRotationMapBody{Name: "C", MapFile: "c.xml", SpawnFile: "c.xml"}),
		"spawn extension":     withMap(mapRotationMapBody{Name: "C", MapFile: "c.json", SpawnFile: "c.json"}),
		"spawn file path":     withMap(mapRotationMapBody{Name: "C", MapFile: "c.json", SpawnFile: "../cfgplayerspawnpoints.xml"}),
		"champion's own file": withMap(mapRotationMapBody{Name: "C", MapFile: "champion_shop_delivery.json", SpawnFile: "c.xml"}),
		"image http":          withMap(mapRotationMapBody{Name: "C", MapFile: "c.json", SpawnFile: "c.xml", ImageURL: str("http://cdn.example.com/c.png")}),
		"image javascript":    withMap(mapRotationMapBody{Name: "C", MapFile: "c.json", SpawnFile: "c.xml", ImageURL: str("javascript:alert(1)")}),
		"image too long":      withMap(mapRotationMapBody{Name: "C", MapFile: "c.json", SpawnFile: "c.xml", ImageURL: str("https://cdn.example.com/" + strings.Repeat("x", 480))}),
		"same files twice":    withMap(mapRotationMapBody{Name: "C", MapFile: "arena1.json", SpawnFile: "arena1_spawns.xml"}),
		"bad id":              withMap(mapRotationMapBody{ID: func() *int64 { v := int64(-1); return &v }(), Name: "C", MapFile: "c.json", SpawnFile: "c.xml"}),
	}
	for name, body := range bad {
		if _, problem := validateMapRotationBody(body); problem == "" {
			t.Errorf("%s: accepted", name)
		}
	}
	// spawnXml: each refusal names the map and never repeats the file.
	big := "<playerspawnpoints><pos x=\"1\" z=\"1\"/>" + strings.Repeat(" ", maprotationMaxSpawnBytes) + "</playerspawnpoints>"
	for name, xml := range map[string]string{
		"empty": "", "not XML": "<playerspawnpoints><fresh>", "wrong root": `<types><pos x="1" z="1"/></types>`,
		"no position": "<playerspawnpoints></playerspawnpoints>", "too large": big,
	} {
		_, problem := validateMapRotationBody(withMap(mapRotationMapBody{Name: "Dust", MapFile: "c.json", SpawnFile: "c.xml", SpawnXML: str(xml)}))
		if !strings.HasPrefix(problem, "Map 3 (Dust): ") || (xml != "" && strings.Contains(problem, xml)) {
			t.Errorf("spawnXml %s: %q", name, problem)
		}
	}
	// Exactly the limit is accepted.
	atLimit := "<playerspawnpoints><pos x=\"1\" z=\"1\"/>" + strings.Repeat(" ", maprotationMaxSpawnBytes-57) + "</playerspawnpoints>"
	if _, problem := validateMapRotationBody(withMap(mapRotationMapBody{Name: "Dust", MapFile: "c.json", SpawnFile: "c.xml", SpawnXML: str(atLimit)})); len(atLimit) != maprotationMaxSpawnBytes || problem != "" {
		t.Fatalf("spawnXml of exactly the limit (%d bytes): %q", len(atLimit), problem)
	}
	// Name limits are counted in characters, and exactly 60 and 500 are allowed.
	ok := withMap(mapRotationMapBody{Name: strings.Repeat("é", 60), MapFile: "c.json", SpawnFile: "c.xml", ImageURL: str("https://cdn.example.com/" + strings.Repeat("x", 476))})
	if _, problem := validateMapRotationBody(ok); problem != "" {
		t.Fatalf("limits: %q", problem)
	}
}

// The admin view always carries every contract key, with null where there is nothing.
func TestMapRotationAdminViewShape(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	restart := now.Add(20 * time.Minute)
	id1, id2 := int64(1), int64(2)
	yes := true
	by := "VOTE"
	name := "Arena 1"
	snap := repository.MapRotationSnapshot{
		Settings: repository.MapRotationSettings{Enabled: true, EveryRestarts: 2, Order: "SEQUENCE", VoteEnabled: true, VoteMinutes: 20, RestartsSinceSwitch: 1, Phase: "DECIDED",
			CurrentMapID: &id1, CurrentMapName: &name, CurrentSince: &now, NextMapID: &id2, NextDecidedBy: &by, NextRestartAt: &restart, HaltedReason: "stopped"},
		Maps: []repository.MapRotationMap{
			// Map 1 was checked when its spawn file was still looked up on the server (found there);
			// nothing is stored in Champion, and that is what the view reports now.
			{ID: 1, Name: "Arena 1", MapFile: "a.json", SpawnFile: "a.xml", Enabled: true, Position: 0, MapFileFound: &yes, SpawnFileFound: &yes, CheckedAt: &now},
			{ID: 2, Name: "Arena 2", MapFile: "b.json", SpawnFile: "b.xml", SpawnBytes: 2048, Enabled: true, Position: 1},
		},
		Vote:       &repository.MapRotationVote{ID: 9, Status: "CLOSED", OpensAt: now, ClosesAt: now, Options: []repository.MapRotationVoteOption{{MapID: 2, Name: "Arena 2", Votes: 3}}, TotalVotes: 3},
		LastSwitch: &repository.MapRotationSwitch{MapID: &id2, MapName: "Arena 2", Status: repository.MapSwitchRolledBack, Message: "cfgplayerspawnpoints.xml could not be written.", CreatedAt: now},
	}
	raw, _ := json.Marshal(toMapRotationAdminDTO(snap, "", now))
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	next := v["next"].(map[string]any)
	if next["mapId"].(float64) != 2 || next["decidedBy"] != "VOTE" || next["restartsUntilSwitch"].(float64) != 1 || next["switchAt"] != "2026-10-04T12:20:00Z" {
		t.Fatalf("next: %v", next)
	}
	if cur := v["current"].(map[string]any); cur["mapId"].(float64) != 1 || cur["since"] != "2026-10-04T12:00:00Z" {
		t.Fatalf("current: %v", cur)
	}
	checks := v["filesCheck"].([]any)
	if len(checks) != 1 || checks[0].(map[string]any)["spawnFileFound"] != false || checks[0].(map[string]any)["mapFileFound"] != true {
		t.Fatalf("filesCheck holds only checked maps: %v", checks)
	}
	last := v["lastSwitch"].(map[string]any)
	if last["ok"] != false || !strings.Contains(last["message"].(string), "rotation is stopped") || last["at"] != "2026-10-04T12:00:00Z" {
		t.Fatalf("lastSwitch: %v", last)
	}
	if vote := v["vote"].(map[string]any); vote["totalVotes"].(float64) != 3 || vote["options"].([]any)[0].(map[string]any)["imageUrl"] != nil {
		t.Fatalf("vote: %v", vote)
	}
	m1, m2 := v["maps"].([]any)[0].(map[string]any), v["maps"].([]any)[1].(map[string]any)
	if len(m1) != 9 || m1["imageUrl"] != nil {
		t.Fatalf("a map entry has exactly the contract's nine fields: %v", m1)
	}
	if m1["spawnUploaded"] != false || m1["spawnBytes"].(float64) != 0 || m1["spawnFile"] != "a.xml" || m2["spawnUploaded"] != true || m2["spawnBytes"].(float64) != 2048 {
		t.Fatalf("spawn fields: %v %v", m1, m2)
	}
	if _, has := m2["spawnXml"]; has {
		t.Fatal("the spawn contents must never be returned")
	}
	// Not available: the reason is a string; available: it is null.
	raw, _ = json.Marshal(toMapRotationAdminDTO(repository.MapRotationSnapshot{}, mapRotationReasonFlag, now))
	if !strings.Contains(string(raw), `"available":false`) || !strings.Contains(string(raw), `"reason":"Map rotation is not switched on`) || !strings.Contains(string(raw), `"maps":[]`) ||
		!strings.Contains(string(raw), `"filesCheck":[]`) || !strings.Contains(string(raw), `"vote":null`) || !strings.Contains(string(raw), `"lastSwitch":null`) {
		t.Fatalf("unavailable view: %s", raw)
	}
}

func TestMapRotationDiscordMessages(t *testing.T) {
	closes := time.Date(2026, 10, 4, 18, 0, 0, 0, time.UTC)
	vote := repository.MapRotationVote{ID: 1, Status: "OPEN", OpensAt: closes.Add(-30 * time.Minute), ClosesAt: closes,
		Options: []repository.MapRotationVoteOption{{MapID: 1, Name: "Arena @everyone **1**"}, {MapID: 2, Name: "Arena 2"}}}
	link := "https://championshp.vip/vote/42"

	quiet := buildMapVoteOpenMessage("Deadzone", vote, link, false)
	if quiet.Content != "" || len(quiet.AllowedMentions.Parse) != 0 {
		t.Fatalf("without the ping setting nothing may ping: %q %+v", quiet.Content, quiet.AllowedMentions)
	}
	e := quiet.Embeds[0]
	if !strings.Contains(e.Title, "Pick the next map") || !strings.Contains(e.Description, link) || !strings.Contains(e.Fields[0].Value, "Arena 2") || !strings.Contains(e.Fields[1].Value, fmt.Sprintf("<t:%d:", closes.Unix())) {
		t.Fatalf("vote embed: %+v", e)
	}
	if strings.Contains(e.Fields[0].Value, "**1**") {
		t.Fatalf("map names are escaped: %q", e.Fields[0].Value)
	}
	loud := buildMapVoteOpenMessage("Deadzone", vote, link, true)
	if !strings.HasPrefix(loud.Content, "@everyone") || len(loud.AllowedMentions.Parse) != 1 || loud.AllowedMentions.Parse[0] != discordgo.AllowedMentionTypeEveryone ||
		len(loud.AllowedMentions.Users) != 0 || len(loud.AllowedMentions.Roles) != 0 {
		t.Fatalf("ping: %q %+v", loud.Content, loud.AllowedMentions)
	}

	winner, by := int64(2), "VOTE"
	vote.Status, vote.WinnerMapID, vote.WinnerName, vote.DecidedBy, vote.TotalVotes = "CLOSED", &winner, "Arena 2", &by, 3
	vote.Options[0].Votes, vote.Options[1].Votes = 1, 2
	res := buildMapVoteResultMessage("Deadzone", vote)
	if res.Content != "" || len(res.AllowedMentions.Parse) != 0 || !strings.Contains(res.Embeds[0].Title, "Next map: Arena 2") ||
		!strings.Contains(res.Embeds[0].Fields[0].Value, "Arena 2: 2 votes") || !strings.Contains(res.Embeds[0].Fields[0].Value, ": 1 vote") {
		t.Fatalf("result: %+v", res.Embeds[0])
	}
	rot := "ROTATION"
	vote.DecidedBy, vote.TotalVotes = &rot, 0
	if res := buildMapVoteResultMessage("", vote); !strings.Contains(res.Embeds[0].Description, "Nobody voted") {
		t.Fatalf("no votes: %q", res.Embeds[0].Description)
	}
	changed := buildMapChangedMessage("Deadzone", "Arena 2", closes)
	if !strings.Contains(changed.Embeds[0].Title, "Map changed to Arena 2") || len(changed.AllowedMentions.Parse) != 0 {
		t.Fatalf("changed: %+v", changed.Embeds[0])
	}
}
