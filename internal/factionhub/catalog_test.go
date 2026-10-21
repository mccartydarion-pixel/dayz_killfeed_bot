package factionhub

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

func TestCatalogsAreSortedUniqueAndStable(t *testing.T) {
	// inCatalog binary-searches, so the catalogs must stay sorted; keys are the stable contract
	// the website mirrors, so pin them.
	if !sort.StringsAreSorted(DayzFlags) || !sort.StringsAreSorted(Armbands) {
		t.Fatal("catalogs must be kept sorted (binary search)")
	}
	if got := strings.Join(DayzFlags, ","); got != "ALTIS,APA,BABYDEER,BEAR,BOHEMIA,BRAINZ,CANNIBALS,CDF,CHEDAKI,CHEL,CHERNARUS,CMC,CROOK,DAYZ,HUNTERZ,LIVONIA,LIVONIAARMY,LIVONIAPOLICE,NAPA,NSAHRANI,PIRATES,REFUGE,REX,ROOSTER,RSTA,SNAKE,SSAHRANI,TEC,UEC,WHITE,WOLF,ZAGORKY,ZENIT" {
		t.Fatalf("flag catalog changed: %s", got)
	}
	if len(DayzFlags) != 33 {
		t.Fatalf("DayZ ships 33 flags, catalog has %d", len(DayzFlags))
	}
	for _, k := range DayzFlags {
		if _, ok := FlagClassName(k); !ok {
			t.Fatalf("flag %s has no class name", k)
		}
	}
	if cn, _ := FlagClassName("BABYDEER"); cn != "Flag_BabyDeer" {
		t.Fatalf("class name spelling: %s", cn)
	}
	if _, ok := FlagClassName("BLACK"); ok {
		t.Fatal("BLACK is not a DayZ flag")
	}
	if got := strings.Join(Armbands, ","); got != "BLACK,BLUE,GREEN,ORANGE,PINK,RED,WHITE,YELLOW" {
		t.Fatalf("armband catalog changed: %s", got)
	}
	for _, c := range [][]string{DayzFlags, Armbands} {
		seen := map[string]bool{}
		for _, k := range c {
			if seen[k] || k != strings.ToUpper(k) || strings.ContainsAny(k, " /:.") {
				t.Fatalf("bad catalog key %q", k)
			}
			seen[k] = true
		}
	}
}

func TestValidateFlagAndArmbandKeys(t *testing.T) {
	for _, k := range DayzFlags {
		if got, err := ValidateFlagKey(strings.ToLower(" " + k + " ")); err != nil || got != k {
			t.Errorf("flag %s: %q %v", k, got, err)
		}
	}
	for _, k := range Armbands {
		if got, err := ValidateArmbandKey(k); err != nil || got != k {
			t.Errorf("armband %s: %q %v", k, got, err)
		}
	}
	if got, err := ValidateFlagKey("  "); err != nil || got != "" {
		t.Fatalf("empty clears: %q %v", got, err)
	}
	bad := []string{"PURPLE", "blackflag", "Flag_CDF", "https://evil.example/flag.png", "red;color:red", "<b>RED</b>", "RED BLUE", "../RED", "url(x)", "NULL", "0", "REDD"}
	for _, in := range bad {
		var v *ValidationError
		if _, err := ValidateFlagKey(in); !errors.As(err, &v) {
			t.Errorf("flag %q must be rejected", in)
		}
		if _, err := ValidateArmbandKey(in); !errors.As(err, &v) {
			t.Errorf("armband %q must be rejected", in)
		}
	}
	// A key valid for one catalog is not automatically valid for the other.
	if _, err := ValidateFlagKey("ORANGE"); err == nil {
		t.Error("ORANGE is an armband, not a flag")
	}
	if _, err := ValidateArmbandKey("ORANGE"); err != nil {
		t.Errorf("ORANGE is an armband: %v", err)
	}
}

func TestColorValidationRejectsCSSInjection(t *testing.T) {
	good := map[string]string{"#d4af37": "#D4AF37", "#D4AF37": "#D4AF37", " #2b2f33 ": "#2B2F33", "": ""}
	for in, want := range good {
		if got, err := ValidateColor("primaryColor", in); err != nil || got != want {
			t.Errorf("ValidateColor(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"rgb(1,2,3)", "rgba(0,0,0,1)", "hsl(0,0%,0%)", "url(https://evil.example/x)", "var(--accent)", "expression(alert(1))",
		"#fff", "#ffff", "#12345g", "#1234567", "123456", "red", "transparent", "currentColor", "inherit",
		"#aabbcc;background:url(x)", "#aabbcc\n", "#aabbcc\"onload=\"x", "#aabbcc}body{x:y", "#aabbcc /* c */", "#aa\x00bbcc", "##aabbc", "#AABBCC;", "\\#aabbcc",
	}
	for _, in := range bad {
		if got, err := ValidateColor("primaryColor", in); err == nil {
			// A trailing newline is trimmed like any surrounding whitespace, but never passes
			// through as part of the value.
			if strings.TrimSpace(in) == "#aabbcc" && got == "#AABBCC" {
				continue
			}
			t.Errorf("ValidateColor(%q) must fail, got %q", in, got)
		}
	}
}
