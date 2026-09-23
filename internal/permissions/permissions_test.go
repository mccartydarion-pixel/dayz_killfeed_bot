package permissions

import "testing"

func TestParseLevelRoundTrips(t *testing.T) {
	for _, name := range []string{"OWNER", "ADMINISTRATOR", "MODERATOR", "GATEKEEPER"} {
		l, ok := ParseLevel(name)
		if !ok {
			t.Fatalf("ParseLevel(%q) should succeed", name)
		}
		if l.String() != name {
			t.Fatalf("round trip mismatch: %q -> %v -> %q", name, l, l.String())
		}
	}
}

func TestParseLevelRejectsUnknown(t *testing.T) {
	for _, bad := range []string{"", "owner", "SUPERADMIN", "MEMBER"} {
		if _, ok := ParseLevel(bad); ok {
			t.Fatalf("ParseLevel(%q) should fail closed", bad)
		}
	}
}

func TestAllowsHierarchyInheritance(t *testing.T) {
	// Every capability an ADMINISTRATOR needs must also be allowed for OWNER (task: "Higher
	// permission levels inherit lower-level permissions").
	for cap, need := range requiredLevel {
		for level := need; level <= LevelOwner; level++ {
			if !Allows(level, cap) {
				t.Fatalf("level %v should satisfy capability %s (needs %v)", level, cap, need)
			}
		}
		for level := LevelNone; level < need; level++ {
			if Allows(level, cap) {
				t.Fatalf("level %v should NOT satisfy capability %s (needs %v)", level, cap, need)
			}
		}
	}
}

func TestAllowsUnknownCapabilityFailsClosed(t *testing.T) {
	if Allows(LevelOwner, Capability("NOT_A_REAL_CAPABILITY")) {
		t.Fatal("an unrecognized capability key must never be granted, even to OWNER")
	}
}

func TestAllowsLevelNoneNeverAllowed(t *testing.T) {
	for cap := range requiredLevel {
		if Allows(LevelNone, cap) {
			t.Fatalf("LevelNone must never satisfy any capability, got true for %s", cap)
		}
	}
}

func TestCanGrantNeverEscalatesAboveActorCeiling(t *testing.T) {
	cases := []struct {
		actor, target Level
		want          bool
	}{
		{LevelOwner, LevelOwner, true},
		{LevelOwner, LevelAdministrator, true},
		{LevelAdministrator, LevelOwner, false}, // an Administrator can never grant Owner
		{LevelModerator, LevelOwner, false},     // task's explicit example: Moderator cannot grant Owner
		{LevelModerator, LevelModerator, true},
		{LevelModerator, LevelGatekeeper, true},
		{LevelGatekeeper, LevelModerator, false}, // cannot grant above own level
		{LevelNone, LevelGatekeeper, false},
		{LevelModerator, LevelNone, false},
	}
	for _, c := range cases {
		got := CanGrant(c.actor, c.target)
		if got != c.want {
			t.Errorf("CanGrant(%v, %v) = %v, want %v", c.actor, c.target, got, c.want)
		}
	}
}

func TestRequiredLevelReportsUnknownKeys(t *testing.T) {
	if _, ok := RequiredLevel(CapServerRestart); !ok {
		t.Fatal("SERVER_RESTART should be a known capability")
	}
	if _, ok := RequiredLevel(Capability("BOGUS")); ok {
		t.Fatal("an unrecognized capability should report ok=false")
	}
}
