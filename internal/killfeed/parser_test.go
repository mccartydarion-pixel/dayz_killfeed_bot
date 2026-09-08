package killfeed

import "testing"

func parse(t *testing.T, line string) *Event {
	t.Helper()
	ev, err := NewADMParser().ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine returned error for %q: %v", line, err)
	}
	return ev
}

func TestParseExplicitKillM4A1(t *testing.T) {
	line := `16:40:12 | Player "ookylianoo" (DEAD) (id=deadbeef01 pos=<7434.4, 1401.4, 5.7>) killed by Player "MmeyAFK_7" (id=cafe02 pos=<7500.1, 1390.0, 4.2>) with M4-A1 from 62.1978 meters`
	ev := parse(t, line)
	if ev == nil {
		t.Fatal("expected a parsed event")
	}
	if ev.Type != EventPlayerKill {
		t.Fatalf("expected PLAYER_KILL, got %s", ev.Type)
	}
	if ev.Victim == nil || ev.Victim.Name != "ookylianoo" {
		t.Fatalf("expected victim ookylianoo, got %+v", ev.Victim)
	}
	if ev.Killer == nil || ev.Killer.Name != "MmeyAFK_7" {
		t.Fatalf("expected killer MmeyAFK_7, got %+v", ev.Killer)
	}
	if ev.Weapon != "M4-A1" {
		t.Fatalf("expected weapon M4-A1, got %q", ev.Weapon)
	}
	if ev.Distance == nil || *ev.Distance != 62.1978 {
		t.Fatalf("expected distance 62.1978, got %v", ev.Distance)
	}
	if ev.Victim.ID == "" || ev.Killer.ID == "" {
		t.Fatal("expected player IDs to be parsed")
	}
	if ev.Victim.Position == nil || ev.Killer.Position == nil {
		t.Fatal("expected positions to be parsed")
	}
}

func TestParseExplicitKillSCR17(t *testing.T) {
	line := `17:01:02 | Player "SomeOne" (DEAD) (id=aa11 pos=<100.0, 200.0, 3.0>) killed by Player "Other" (id=bb22 pos=<90.0, 210.0, 3.1>) with SCR 17 from 15.5 meters`
	ev := parse(t, line)
	if ev == nil || ev.Type != EventPlayerKill {
		t.Fatalf("expected PLAYER_KILL, got %+v", ev)
	}
	if ev.Weapon != "SCR 17" {
		t.Fatalf("expected weapon 'SCR 17' (not truncated to SCR), got %q", ev.Weapon)
	}
}

func TestParseHitWithAKMAndAmmo(t *testing.T) {
	line := `16:30:00 | Player "Victim" (id=v001 pos=<7504.7, 1334.4, 0.9>) [HP: 42.5] hit by Player "Shooter" (id=s002 pos=<7490.2, 1528.2, 43.0>) into Torso(3) for 28.8605 damage (Bullet_762x39Tracer) with testing AKM from 115.058 meters`
	ev := parse(t, line)
	if ev == nil || ev.Type != EventPlayerHit {
		t.Fatalf("expected PLAYER_HIT, got %+v", ev)
	}
	if ev.Weapon != "testing AKM" {
		t.Fatalf("expected weapon 'testing AKM', got %q", ev.Weapon)
	}
	if ev.Ammo != "Bullet_762x39Tracer" {
		t.Fatalf("expected ammo Bullet_762x39Tracer, got %q", ev.Ammo)
	}
	if ev.HitZone != "Torso" || ev.HitZoneID != "3" {
		t.Fatalf("expected Torso(3), got %q(%q)", ev.HitZone, ev.HitZoneID)
	}
	if ev.Damage == nil || *ev.Damage != 28.8605 {
		t.Fatalf("expected damage 28.8605, got %v", ev.Damage)
	}
	if ev.Distance == nil || *ev.Distance != 115.058 {
		t.Fatalf("expected distance 115.058, got %v", ev.Distance)
	}
}

func TestParseHitLethalHP0IsNotAKill(t *testing.T) {
	line := `16:31:00 | Player "Victim" (DEAD) (id=v001 pos=<1.0, 2.0, 3.0>) [HP: 0] hit by Player "Shooter" (id=s002 pos=<4.0, 5.0, 6.0>) into Head(1) for 50.0 damage (Bullet_556x45) with M4-A1 from 10.0 meters`
	ev := parse(t, line)
	if ev == nil || ev.Type != EventPlayerHit {
		t.Fatalf("expected PLAYER_HIT (never a kill from a hit line), got %+v", ev)
	}
	if ev.HP == nil || *ev.HP != 0 {
		t.Fatalf("expected HP=0, got %v", ev.HP)
	}
	if !ev.Dead {
		t.Fatal("expected Dead flag from (DEAD) marker")
	}
}

func TestParseGenericDeathHasNoKiller(t *testing.T) {
	line := `16:35:00 | Player "Ceiyxe" (DEAD) (id=c003 pos=<7000.0, 1200.0, 8.0>) died. Stats> Water: 100 Energy: 80`
	ev := parse(t, line)
	if ev == nil || ev.Type != EventPlayerDeath {
		t.Fatalf("expected PLAYER_DEATH, got %+v", ev)
	}
	if ev.Killer != nil {
		t.Fatalf("expected no killer attribution for generic death, got %+v", ev.Killer)
	}
	if ev.Player == nil || ev.Player.Name != "Ceiyxe" {
		t.Fatalf("expected player Ceiyxe, got %+v", ev.Player)
	}
}

func TestParseSuicideAction(t *testing.T) {
	line := `16:36:00 | Player "Ceiyxe" (id=c003 pos=<7000.0, 1200.0, 8.0>) performed EmoteSuicide with M4A1`
	ev := parse(t, line)
	if ev == nil || ev.Type != EventSuicideAction {
		t.Fatalf("expected SUICIDE_ACTION, got %+v", ev)
	}
	if ev.Weapon != "M4A1" {
		t.Fatalf("expected weapon M4A1, got %q", ev.Weapon)
	}
}

func TestParseConnectingConnectedDisconnected(t *testing.T) {
	connecting := parse(t, `16:16:10 | Player "Andromede54" (id=a001) is connecting`)
	if connecting == nil || connecting.Type != EventPlayerConnecting {
		t.Fatalf("expected PLAYER_CONNECTING, got %+v", connecting)
	}

	connected := parse(t, `16:25:41 | Player "NoxiKillNewB" (id=n002 pos=<7504.7, 1334.4, 0.9>) is connected`)
	if connected == nil || connected.Type != EventPlayerConnect {
		t.Fatalf("expected PLAYER_CONNECT, got %+v", connected)
	}
	if connected.Player.ID != "n002" {
		t.Fatalf("expected player id n002, got %q", connected.Player.ID)
	}

	disconnected := parse(t, `16:32:47 | Player "Mister-Splosion" (id=m003 pos=<7490.2, 1528.2, 43.0>) has been disconnected`)
	if disconnected == nil || disconnected.Type != EventPlayerDisconnect {
		t.Fatalf("expected PLAYER_DISCONNECT, got %+v", disconnected)
	}
}

func TestParseUnconsciousConsciousRespawn(t *testing.T) {
	un := parse(t, `16:20:00 | Player "Survivor" (id=u004 pos=<1.0, 2.0, 3.0>) is unconscious`)
	if un == nil || un.Type != EventPlayerUnconscious {
		t.Fatalf("expected PLAYER_UNCONSCIOUS, got %+v", un)
	}
	con := parse(t, `16:21:00 | Player "Survivor" (id=u004 pos=<1.0, 2.0, 3.0>) regained consciousness`)
	if con == nil || con.Type != EventPlayerConscious {
		t.Fatalf("expected PLAYER_CONSCIOUS, got %+v", con)
	}
	res := parse(t, `16:33:00 | Player "Mister-Splosion" (DEAD) (id=m003 pos=<7490.2, 1528.2, 43.0>) is choosing to respawn`)
	if res == nil || res.Type != EventPlayerRespawn {
		t.Fatalf("expected PLAYER_RESPAWN, got %+v", res)
	}
}

func TestParseNegativeCoordinates(t *testing.T) {
	line := `16:40:12 | Player "A" (DEAD) (id=a1 pos=<-7434.4, 1401.4, -5.7>) killed by Player "B" (id=b1 pos=<-100.5, -200.25, 4.2>) with M4-A1 from 30.0 meters`
	ev := parse(t, line)
	if ev == nil || ev.Victim.Position == nil {
		t.Fatalf("expected victim position, got %+v", ev)
	}
	if ev.Victim.Position.X != -7434.4 || ev.Victim.Position.Z != -5.7 {
		t.Fatalf("expected negative coords preserved, got %+v", ev.Victim.Position)
	}
	if ev.Killer.Position.Y != -200.25 {
		t.Fatalf("expected killer negative Y, got %+v", ev.Killer.Position)
	}
}

func TestParseUnknownAndMalformedLinesAreIgnored(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"some random server log line with no structure",
		"16:00:00 | Server: something happened",
		"Player without quotes is weird",
	}
	for _, c := range cases {
		ev, err := NewADMParser().ParseLine(c)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", c, err)
		}
		if ev != nil {
			t.Fatalf("expected nil for unknown/malformed line %q, got %+v", c, ev)
		}
	}
}

func TestExplicitKillCheckedBeforeDeath(t *testing.T) {
	// A "killed by" line also contains the victim and could be misread as death;
	// it must produce PLAYER_KILL, not PLAYER_DEATH.
	line := `16:40:12 | Player "V" (DEAD) (id=v1 pos=<1.0, 2.0, 3.0>) killed by Player "K" (id=k1 pos=<4.0, 5.0, 6.0>) with SCR 17 from 5.0 meters`
	ev := parse(t, line)
	if ev.Type != EventPlayerKill {
		t.Fatalf("expected explicit kill to win ordering, got %s", ev.Type)
	}
}
