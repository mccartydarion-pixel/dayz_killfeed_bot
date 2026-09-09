package repository

import "testing"

func TestCanonicalPair(t *testing.T) {
	a, b, err := CanonicalPair(9, 3)
	if err != nil || a != 3 || b != 9 {
		t.Fatalf("got %d,%d,%v", a, b, err)
	}
	if _, _, err := CanonicalPair(4, 4); err == nil {
		t.Fatal("same faction must be rejected")
	}
}
func TestWarClassification(t *testing.T) {
	a, b := int64(1), int64(2)
	if WarClassification(&a, &b) != "ENEMY_FACTION_KILL" {
		t.Fatal("enemy factions should score")
	}
	if WarClassification(&a, &a) != "TEAM_KILL" {
		t.Fatal("same faction should be team kill")
	}
	if WarClassification(nil, &b) != "NO_FACTION" {
		t.Fatal("unfactioned kill should be unclassified")
	}
}
