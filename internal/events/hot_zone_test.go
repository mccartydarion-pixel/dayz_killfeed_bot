package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestQualifyHotZoneScoresKillsInsideTheCircleOnItsServer(t *testing.T) {
	config, _ := json.Marshal(HotZoneConfig{ServerID: 7, CenterX: 7750, CenterZ: 12750, RadiusM: 500})
	event := Event{Status: StatusActive, Type: TypeHotZone, Config: config}
	inside, outside := &Point{X: 7750 + 300, Z: 12750 - 400}, &Point{X: 7750 + 400, Z: 12750 + 400}
	kill := KillInput{KillerPlayerID: 1, VictimPlayerID: 2, EventTime: time.Now(), ServerID: 7, VictimPos: inside, KillerPos: outside}
	q := Qualify(event, kill)
	if !q.Qualifies || q.Points != 1 || q.PlayerID != 1 {
		t.Fatalf("victim inside the zone should score one point for the killer: %+v", q)
	}
	// The victim position decides, even when the killer stands inside.
	kill.VictimPos, kill.KillerPos = outside, inside
	if Qualify(event, kill).Qualifies {
		t.Fatal("victim outside the zone should not score")
	}
	// With no victim position logged, the killer position stands in.
	kill.VictimPos = nil
	if !Qualify(event, kill).Qualifies {
		t.Fatal("killer inside the zone should score when the victim position is unknown")
	}
	kill.KillerPos = nil
	if Qualify(event, kill).Qualifies {
		t.Fatal("a kill with no logged position scored")
	}
	kill.VictimPos = inside
	kill.ServerID = 8
	if Qualify(event, kill).Qualifies {
		t.Fatal("a kill on another server scored")
	}
	kill.ServerID = 0
	if Qualify(event, kill).Qualifies {
		t.Fatal("a kill with no server scored")
	}
}

func TestHotZoneConfigBoundaryAndValidation(t *testing.T) {
	edge := HotZoneConfig{CenterX: 0, CenterZ: 0, RadiusM: 500}
	if !edge.Contains(Point{X: 300, Z: 400}) || edge.Contains(Point{X: 300, Z: 401}) {
		t.Fatal("the boundary is inclusive at exactly the radius and exclusive beyond it")
	}
	if err := ValidateConfig(TypeHotZone, HotZoneConfig{ServerID: 1, RadiusM: 0}); err == nil {
		t.Fatal("a zero radius was accepted")
	}
	if err := ValidateConfig(TypeHotZone, HotZoneConfig{ServerID: 0, RadiusM: 100}); err == nil {
		t.Fatal("a hot zone with no server was accepted")
	}
	if err := ValidateConfig(TypeHotZone, HotZoneConfig{ServerID: 1, RadiusM: 100}); err != nil {
		t.Fatal(err)
	}
}
