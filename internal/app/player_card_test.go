package app

import (
	"testing"

	"github.com/yourname/dayz-killfeed/internal/config"
	"github.com/yourname/dayz-killfeed/internal/playercard"
	"github.com/yourname/dayz-killfeed/internal/ranked"
)

// The Ranked standing in the card JSON (docs/CHAMPION_CARD.md): status, then the progress fields
// only while a season is active, and the next-tier fields only below Master.

func TestCardDTOCarriesTheRankedStanding(t *testing.T) {
	a := &App{}
	none := asMap(t, a.toCardDTO(playercard.Card{PlayerName: "Ace"}))["ranked"].(map[string]any)
	if none["status"] != "NOT_STARTED" || len(none) != 1 {
		t.Fatalf("no season: ranked = %v", none)
	}

	position, next := int64(5), int64(1500)
	gold := playercard.Ranked{Tier: ranked.Gold, RP: 1240, Position: &position, NextTier: ranked.Platinum, Remaining: 260, TierStartRP: 1000, NextTierRP: &next}
	active := asMap(t, a.toCardDTO(playercard.Card{PlayerName: "Ace", Ranked: &gold}))["ranked"].(map[string]any)
	want := map[string]any{"status": "ACTIVE", "tier": "GOLD", "rp": float64(1240), "position": float64(5), "nextTier": "PLATINUM", "remainingRp": float64(260), "tierStartRp": float64(1000), "nextTierRp": float64(1500)}
	if len(active) != len(want) {
		t.Fatalf("active ranked = %v", active)
	}
	for k, v := range want {
		if active[k] != v {
			t.Errorf("ranked.%s = %v, want %v", k, active[k], v)
		}
	}

	// Not placed yet: position is present and null. At Master: nothing above to climb to.
	fresh := gold
	fresh.Tier, fresh.RP, fresh.Position = ranked.Unranked, 0, nil
	unranked := asMap(t, a.toCardDTO(playercard.Card{Ranked: &fresh}))["ranked"].(map[string]any)
	if v, ok := unranked["position"]; !ok || v != nil || unranked["rp"] != float64(0) || unranked["tier"] != "UNRANKED" {
		t.Fatalf("unranked = %v", unranked)
	}
	master := asMap(t, a.toCardDTO(playercard.Card{Ranked: &playercard.Ranked{Tier: ranked.Master, RP: 9870, Position: &position, TierStartRP: 2800}}))["ranked"].(map[string]any)
	for _, k := range []string{"nextTier", "remainingRp", "nextTierRp"} {
		if _, ok := master[k]; ok {
			t.Errorf("master carries %s: %v", k, master)
		}
	}
	if master["status"] != "ACTIVE" || master["tier"] != "MASTER" || master["rp"] != float64(9870) || master["tierStartRp"] != float64(2800) || master["position"] != float64(5) {
		t.Fatalf("master = %v", master)
	}

	// The tiles still mirror the image: the server rank tile says what it counts.
	rank := 3
	tiles := asMap(t, a.toCardDTO(playercard.Card{Kills: 9, Rank: &rank, RankedPlayers: 123}))["tiles"].([]any)
	if last := tiles[7].(map[string]any); last["label"] != "SERVER RANK" || last["value"] != "#3" || last["note"] != "of 123 · by kills" {
		t.Fatalf("server rank tile = %v", last)
	}
}

func TestSiteHostComesFromTheSiteBaseURL(t *testing.T) {
	for in, want := range map[string]string{"https://championshp.vip": "championshp.vip", "https://championshp.vip/": "championshp.vip", "http://localhost:3000": "localhost", "": "", "   ": ""} {
		if got := (&App{Config: &config.Config{SiteBaseURL: in}}).siteHost(); got != want {
			t.Errorf("siteHost(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (&App{}).siteHost(); got != "" {
		t.Errorf("siteHost without config = %q", got)
	}
}
