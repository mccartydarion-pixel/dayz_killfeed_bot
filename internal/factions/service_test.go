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
