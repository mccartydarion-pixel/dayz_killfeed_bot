package presentation

import "testing"

func TestRankTierIconURL(t *testing.T) {
	cases := []struct{ base, tier, want string }{
		{"https://championshp.vip", "GOLD", "https://championshp.vip/ranks/gold.png"},
		{"https://championshp.vip/", " Master ", "https://championshp.vip/ranks/master.png"},
		{"http://localhost:3000", "rookie", "http://localhost:3000/ranks/rookie.png"},
		{"https://championshp.vip", "UNRANKED", "https://championshp.vip/ranks/unranked.png"},
		{"https://championshp.vip", "LEGEND", "https://championshp.vip/ranks/unranked.png"},
		{"https://championshp.vip", "", "https://championshp.vip/ranks/unranked.png"},
		{"https://championshp.vip", "../secret", "https://championshp.vip/ranks/unranked.png"},
		{"", "GOLD", ""},
		{"   ", "GOLD", ""},
		{"championshp.vip", "GOLD", ""},
	}
	for _, c := range cases {
		if got := RankTierIconURL(c.base, c.tier); got != c.want {
			t.Errorf("RankTierIconURL(%q, %q) = %q, want %q", c.base, c.tier, got, c.want)
		}
	}
	for _, tier := range []string{"UNRANKED", "ROOKIE", "BRONZE", "SILVER", "GOLD", "PLATINUM", "DIAMOND", "MASTER"} {
		if RankTierIconSlug(tier) == "unranked" && tier != "UNRANKED" {
			t.Errorf("%s has no icon of its own", tier)
		}
	}
}

func TestRankTierThumbnail(t *testing.T) {
	if th := RankTierThumbnail("", "GOLD"); th != nil {
		t.Fatalf("no site address must mean no thumbnail, got %+v", th)
	}
	th := RankTierThumbnail("https://championshp.vip", "DIAMOND")
	if th == nil || th.URL != "https://championshp.vip/ranks/diamond.png" {
		t.Fatalf("thumbnail: %+v", th)
	}
}
