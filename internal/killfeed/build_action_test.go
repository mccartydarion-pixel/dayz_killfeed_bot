package killfeed

import "testing"

func TestParseBuildActionLines(t *testing.T) {
	cases := []struct {
		line                         string
		action, object, target, tool string
		pos                          bool
	}{
		{`18:05:12 | Player "Survivor One" (id=Abc123= pos=<5420.3, 8931.2, 312.1>) placed Sea Chest<SeaChest>`, "Placed", "Sea Chest", "", "", true},
		{`18:05:13 | Player "Survivor" (id=Abc123= pos=<5420.3, 8931.2, 312.1>) placed Fence Kit`, "Placed", "Fence Kit", "", "", true},
		{`18:06:00 | Player "Builder" (id=b1 pos=<100.0, 200.0, 3.0>) Built Wall Base Lower on Fence with Hammer`, "Built", "Wall Base Lower", "Fence", "Hammer", true},
		{`18:06:01 | Player "Builder" (id=b1) built Platform on Watchtower`, "Built", "Platform", "Watchtower", "", false},
		{`18:07:00 | Player "Raider" (id=r1 pos=<1.0, 2.0, 3.0>) Dismantled Wall Upper from Fence with Hatchet`, "Dismantled", "Wall Upper", "Fence", "Hatchet", true},
	}
	p := &ADMParser{}
	for _, c := range cases {
		ev, err := p.ParseLine(c.line)
		if err != nil || ev == nil {
			t.Fatalf("%q: want a build event, got %v %v", c.line, ev, err)
		}
		if ev.Type != EventBuildAction || ev.Build == nil || ev.Player == nil {
			t.Fatalf("%q: want BUILD_ACTION with player, got %+v", c.line, ev)
		}
		b := ev.Build
		if b.Action != c.action || b.Object != c.object || b.Target != c.target || b.Tool != c.tool {
			t.Fatalf("%q: got %+v", c.line, b)
		}
		if (ev.Player.Position != nil) != c.pos {
			t.Fatalf("%q: position presence want %v", c.line, c.pos)
		}
	}
}

func TestParseBuildActionRejectsOtherLines(t *testing.T) {
	p := &ADMParser{}
	for _, line := range []string{
		`18:05:12 | Player "placed" (id=x) is connected`,
		`18:05:12 | Player "Survivor" (id=x) has been disconnected`,
		`18:05:12 | Chat("Survivor"(id=x)): I placed a tent`,
		`18:05:12 | Player "Survivor" (id=x) placed `,
		`18:05:12 | Player "Survivor" (DEAD) (id=x pos=<1.0, 2.0, 3.0>) died. Stats> Water: 0 Energy: 0 Bleed sources: 0`,
		`18:05:12 | Player "Survivor" said placed Sea Chest`,
	} {
		if ev, _ := p.ParseLine(line); ev != nil && ev.Type == EventBuildAction {
			t.Fatalf("%q must not parse as a build action: %+v", line, ev.Build)
		}
	}
}

func TestBuildActionFingerprintSeparatesDistinctActions(t *testing.T) {
	p := &ADMParser{}
	a, _ := p.ParseLine(`18:06:00 | Player "Builder" (id=b1) Built Wall Base Lower on Fence with Hammer`)
	b, _ := p.ParseLine(`18:06:00 | Player "Builder" (id=b1) Built Wall Base Upper on Fence with Hammer`)
	again, _ := p.ParseLine(`18:06:00 | Player "Builder" (id=b1) Built Wall Base Lower on Fence with Hammer`)
	if fingerprint(a) == fingerprint(b) {
		t.Fatal("two different parts in the same second are two actions")
	}
	if fingerprint(a) != fingerprint(again) {
		t.Fatal("a replayed line must dedupe")
	}
}
