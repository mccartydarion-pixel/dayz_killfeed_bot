package vip

import "testing"

func TestNormalize(t *testing.T) {
	got, err := Normalize(Tier{Name: " Gold Supporter ", Badge: "💎 @VIP", Color: "#e7b94a", DiscordRoleID: "123456789012345678", RewardMultiplier: 1.257})
	if err != nil || got.Name != "Gold Supporter" || got.Badge != "💎 VIP" || got.Color != "#E7B94A" || got.RewardMultiplier != 1.26 {
		t.Fatalf("%+v %v", got, err)
	}
	if d, err := Normalize(Tier{Name: "x", Badge: "y"}); err != nil || d.RewardMultiplier != 1 || d.Color == "" {
		t.Fatalf("defaults: %+v %v", d, err)
	}
	for _, bad := range []Tier{{Badge: "y"}, {Name: "x"}, {Name: "x", Badge: "y", Color: "red"}, {Name: "x", Badge: "y", DiscordRoleID: "abc"}, {Name: "x", Badge: "y", RewardMultiplier: 5}, {Name: "x", Badge: "y", RewardMultiplier: 0.5}} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("expected rejection: %+v", bad)
		}
	}
}

func TestMultiply(t *testing.T) {
	if Multiply(100, 1.5) != 150 || Multiply(99, 1.25) != 123 || Multiply(100, 0) != 100 || Multiply(100, 9) != 100 {
		t.Fatal("multiply")
	}
}
