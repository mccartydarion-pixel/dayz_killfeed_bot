package events

import (
	"encoding/json"
	"testing"
	"time"
)

func TestQualifyRespectsEventWindow(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	config, _ := json.Marshal(MostKillsConfig{})
	event := Event{Status: StatusActive, Type: TypeMostKills, StartsAt: &start, EndsAt: &end, Config: config}
	base := KillInput{KillerPlayerID: 1, VictimPlayerID: 2, EventTime: start.Add(30 * time.Minute)}
	if !Qualify(event, base).Qualifies {
		t.Fatal("kill during event should qualify")
	}
	base.EventTime = start.Add(-time.Second)
	if Qualify(event, base).Qualifies {
		t.Fatal("kill before event should not qualify")
	}
	base.EventTime = end
	if Qualify(event, base).Qualifies {
		t.Fatal("kill at event end should not qualify")
	}
}

func TestQualifyFactionAndWeaponRules(t *testing.T) {
	killerFaction, victimFaction := int64(1), int64(2)
	weaponConfig, _ := json.Marshal(WeaponChallengeConfig{WeaponNames: []string{"AKM"}})
	weaponEvent := Event{Status: StatusActive, Type: TypeWeaponChallenge, Config: weaponConfig}
	kill := KillInput{KillerPlayerID: 1, VictimPlayerID: 2, WeaponDisplay: "AKM", EventTime: time.Now()}
	if !Qualify(weaponEvent, kill).Qualifies {
		t.Fatal("exact weapon should qualify")
	}
	kill.WeaponDisplay = "testing AKM"
	if Qualify(weaponEvent, kill).Qualifies {
		t.Fatal("loose weapon substring should not qualify")
	}

	factionConfig, _ := json.Marshal(FactionKillsConfig{EnemyFactionsOnly: true})
	factionEvent := Event{Status: StatusActive, Type: TypeFactionKills, Config: factionConfig}
	kill.WeaponDisplay = ""
	kill.KillerFactionID = &killerFaction
	kill.VictimFactionID = &victimFaction
	if !Qualify(factionEvent, kill).Qualifies {
		t.Fatal("enemy faction kill should qualify")
	}
	victimFaction = killerFaction
	if Qualify(factionEvent, kill).Qualifies {
		t.Fatal("team kill should not qualify")
	}
}

func TestValidateConfigRejectsInvalidRules(t *testing.T) {
	if err := ValidateConfig(TypeLongestKill, LongestKillConfig{MinimumDistance: -1}); err == nil {
		t.Fatal("negative distance should be rejected")
	}
	if err := ValidateConfig(TypeWeaponChallenge, WeaponChallengeConfig{}); err == nil {
		t.Fatal("empty weapon challenge should be rejected")
	}
	if err := ValidateConfig("UNKNOWN", nil); err == nil {
		t.Fatal("unknown event type should be rejected")
	}
}
