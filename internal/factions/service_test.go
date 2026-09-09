package factions

import "testing"

func TestValidateNameTag(t *testing.T) {
	valid := [][2]string{{"Champion", "CHP"}, {"狼 Squad", "WLF"}}
	for _, pair := range valid {
		if err := ValidateNameTag(pair[0], pair[1]); err != nil {
			t.Fatalf("valid faction rejected: %v", err)
		}
	}
	invalid := [][2]string{{"", "CHP"}, {"A", "CHP"}, {"Champion", "C"}, {"Champion", "TOOLONG"}, {"@everyone", "CHP"}}
	for _, pair := range invalid {
		if err := ValidateNameTag(pair[0], pair[1]); err == nil {
			t.Fatalf("invalid faction accepted: %q/%q", pair[0], pair[1])
		}
	}
}

func TestCapabilities(t *testing.T) {
	if !Can(RoleOwner, CanDisband) || !Can(RoleLeader, CanInvite) || !Can(RoleOfficer, CanKick) {
		t.Fatal("expected hierarchy capabilities")
	}
	if Can(RoleMember, CanKick) || Can(RoleLeader, CanTransfer) || Can(RoleOfficer, CanDisband) {
		t.Fatal("unexpected elevated capability")
	}
}

func TestWarCapabilitiesFollowFactionLeadership(t *testing.T) {
	for _, role := range []string{RoleOwner, RoleLeader} {
		for _, capability := range []Capability{CanChallengeWar, CanAcceptWar, CanDeclineWar, CanEndWar} {
			if !Can(role, capability) {
				t.Fatalf("%s should have %s", role, capability)
			}
		}
	}
	for _, role := range []string{RoleOfficer, RoleMember} {
		for _, capability := range []Capability{CanChallengeWar, CanAcceptWar, CanDeclineWar, CanEndWar} {
			if Can(role, capability) {
				t.Fatalf("%s should not have %s", role, capability)
			}
		}
		if !Can(role, CanViewWar) {
			t.Fatalf("%s should view wars", role)
		}
	}
}
