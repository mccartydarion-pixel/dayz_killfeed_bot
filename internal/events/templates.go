package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Template is a ready-made event an owner can schedule in one click. It fills
// the type, config, length and prizes; the owner can still change any of them.
type Template struct {
	Key           string          `json:"key"`
	Name          string          `json:"name"`
	Description   string          `json:"description"`
	Type          string          `json:"type"`
	Config        json.RawMessage `json:"config"`
	DurationHours int             `json:"durationHours"`
	FirstPoints   int             `json:"firstPoints"`
	SecondPoints  int             `json:"secondPoints"`
	ThirdPoints   int             `json:"thirdPoints"`
	// NeedsWeapons marks templates whose config needs owner input (weapon names).
	NeedsWeapons bool `json:"needsWeapons,omitempty"`
}

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// Templates is the built-in catalogue. Every one maps onto an event type the
// kill scorer already understands - nothing here invents a new mechanic.
func Templates() []Template {
	mid := 150.0
	return []Template{
		{Key: "KILL_FRENZY", Name: "Kill Frenzy", Description: "Most PvP kills wins.", Type: TypeMostKills, Config: raw(MostKillsConfig{}), DurationHours: 2, FirstPoints: 1000, SecondPoints: 500, ThirdPoints: 250},
		{Key: "SNIPER_WEEKEND", Name: "Sniper Weekend", Description: "Longest single kill of 200 m or more wins.", Type: TypeLongestKill, Config: raw(LongestKillConfig{MinimumDistance: 200}), DurationHours: 48, FirstPoints: 2000, SecondPoints: 1000, ThirdPoints: 500},
		{Key: "LONG_RANGE_HUNT", Name: "Long Range Hunt", Description: "Most kills from 150 m or further.", Type: TypeMostKills, Config: raw(MostKillsConfig{MinimumDistance: &mid}), DurationHours: 3, FirstPoints: 1000, SecondPoints: 500, ThirdPoints: 250},
		{Key: "HEADSHOT_HUNT", Name: "Headshot Hunt", Description: "Most headshot kills wins.", Type: TypeHeadshotHunt, Config: raw(struct{}{}), DurationHours: 2, FirstPoints: 1000, SecondPoints: 500, ThirdPoints: 250},
		{Key: "STREAK_MASTER", Name: "Streak Master", Description: "Highest kill streak of 3 or more wins.", Type: TypeKillStreak, Config: raw(KillStreakConfig{MinimumStreak: 3}), DurationHours: 4, FirstPoints: 1500, SecondPoints: 750, ThirdPoints: 300},
		{Key: "WEAPON_CHALLENGE", Name: "Weapon Challenge", Description: "Most kills with the weapons you choose.", Type: TypeWeaponChallenge, Config: raw(WeaponChallengeConfig{}), DurationHours: 2, FirstPoints: 1000, SecondPoints: 500, ThirdPoints: 250, NeedsWeapons: true},
		{Key: "FACTION_SHOWDOWN", Name: "Faction Showdown", Description: "The faction with the most kills on enemy factions wins.", Type: TypeFactionKills, Config: raw(FactionKillsConfig{EnemyFactionsOnly: true}), DurationHours: 24, FirstPoints: 2000, SecondPoints: 1000, ThirdPoints: 500},
		// Short, punchy formats for a quiet evening (added with the event templates upgrade).
		{Key: "POWER_HOUR", Name: "Power Hour", Description: "One hour, most PvP kills wins.", Type: TypeMostKills, Config: raw(MostKillsConfig{}), DurationHours: 1, FirstPoints: 750, SecondPoints: 400, ThirdPoints: 200},
		{Key: "HEADSHOT_HOUR", Name: "Headshot Hour", Description: "One hour, most headshot kills wins.", Type: TypeHeadshotHunt, Config: raw(struct{}{}), DurationHours: 1, FirstPoints: 750, SecondPoints: 400, ThirdPoints: 200},
		{Key: "SNIPER_SUNDAY", Name: "Sniper Sunday", Description: "Longest single kill of 300 m or more wins. Schedule it for a Sunday.", Type: TypeLongestKill, Config: raw(LongestKillConfig{MinimumDistance: 300}), DurationHours: 12, FirstPoints: 1500, SecondPoints: 750, ThirdPoints: 400},
		{Key: "STREAK_SPRINT", Name: "Streak Sprint", Description: "Two hours, highest kill streak of 3 or more wins.", Type: TypeKillStreak, Config: raw(KillStreakConfig{MinimumStreak: 3}), DurationHours: 2, FirstPoints: 1000, SecondPoints: 500, ThirdPoints: 250},
		{Key: "WEEKEND_MARATHON", Name: "Weekend Marathon", Description: "All weekend, most PvP kills wins.", Type: TypeMostKills, Config: raw(MostKillsConfig{}), DurationHours: 48, FirstPoints: 3000, SecondPoints: 1500, ThirdPoints: 750},
	}
}

// TemplateByKey finds a template ("" or unknown -> false).
func TemplateByKey(key string) (Template, bool) {
	for _, t := range Templates() {
		if strings.EqualFold(t.Key, strings.TrimSpace(key)) {
			return t, true
		}
	}
	return Template{}, false
}

// BuildableTypes are the event types an owner may create from the Client Hub.
// BOUNTY (has its own system), HOT_ZONE (opened automatically) and
// FACTION_WAR_KILLS (needs a war) are deliberately excluded.
var BuildableTypes = []string{TypeMostKills, TypeLongestKill, TypeKillStreak, TypeHeadshotHunt, TypeWeaponChallenge, TypeFactionKills}

func buildable(t string) bool {
	for _, b := range BuildableTypes {
		if b == t {
			return true
		}
	}
	return false
}

// DecodeConfig turns a JSON config into the typed struct ValidateConfig checks,
// rejecting unknown fields so a typo never silently means "no rule".
func DecodeConfig(eventType string, data json.RawMessage) (any, error) {
	if len(data) == 0 || string(data) == "null" {
		data = json.RawMessage("{}")
	}
	strict := func(v any) error {
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("invalid %s config: %w", eventType, err)
		}
		return nil
	}
	switch eventType {
	case TypeMostKills:
		var c MostKillsConfig
		if err := strict(&c); err != nil {
			return nil, err
		}
		if c.MinimumDistance != nil && (*c.MinimumDistance < 0 || *c.MinimumDistance > 5000) {
			return nil, fmt.Errorf("minimum distance must be between 0 and 5000 m")
		}
		return c, nil
	case TypeHeadshotHunt:
		var c struct{}
		return c, strict(&c)
	case TypeLongestKill:
		var c LongestKillConfig
		if err := strict(&c); err != nil {
			return nil, err
		}
		if c.MinimumDistance > 5000 {
			return nil, fmt.Errorf("minimum distance must be at most 5000 m")
		}
		return c, nil
	case TypeKillStreak:
		var c KillStreakConfig
		if err := strict(&c); err != nil {
			return nil, err
		}
		if c.MinimumStreak > 100 {
			return nil, fmt.Errorf("minimum streak must be at most 100")
		}
		return c, nil
	case TypeFactionKills:
		var c FactionKillsConfig
		return c, strict(&c)
	case TypeWeaponChallenge:
		var c WeaponChallengeConfig
		if err := strict(&c); err != nil {
			return nil, err
		}
		cleaned := make([]string, 0, len(c.WeaponNames))
		for _, w := range c.WeaponNames {
			if w = strings.TrimSpace(w); w != "" && len(w) <= 60 {
				cleaned = append(cleaned, w)
			}
		}
		if len(cleaned) == 0 || len(cleaned) > 20 {
			return nil, fmt.Errorf("choose between 1 and 20 weapons")
		}
		c.WeaponNames = cleaned
		return c, nil
	}
	return nil, fmt.Errorf("event type %q cannot be created here", eventType)
}

// Plan is a validated owner request to create an event.
type Plan struct {
	Type                                   string
	Name, Description                      string
	Config                                 any
	StartsAt, EndsAt                       time.Time
	Immediate                              bool
	FirstPoints, SecondPoints, ThirdPoints int
}

// Limits for owner-built events.
const (
	MaxEventDuration = 14 * 24 * time.Hour
	MaxScheduleAhead = 60 * 24 * time.Hour
	MaxPrizePoints   = 1_000_000
)

// ValidatePlan checks an owner request. A nil start means "start now".
func ValidatePlan(eventType, name, description string, config json.RawMessage, startsAt *time.Time, endsAt time.Time, prizes [3]int, now time.Time) (Plan, error) {
	if !buildable(eventType) {
		return Plan{}, fmt.Errorf("event type %q cannot be created here", eventType)
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 80 {
		return Plan{}, fmt.Errorf("name must be 1 to 80 characters")
	}
	description = strings.TrimSpace(description)
	if len([]rune(description)) > 500 {
		return Plan{}, fmt.Errorf("description must be at most 500 characters")
	}
	cfg, err := DecodeConfig(eventType, config)
	if err != nil {
		return Plan{}, err
	}
	if err := ValidateConfig(eventType, cfg); err != nil {
		return Plan{}, err
	}
	p := Plan{Type: eventType, Name: name, Description: description, Config: cfg, FirstPoints: prizes[0], SecondPoints: prizes[1], ThirdPoints: prizes[2]}
	p.StartsAt = now
	if startsAt == nil || !startsAt.After(now.Add(time.Minute)) {
		p.Immediate = true
	} else {
		p.StartsAt = startsAt.UTC()
	}
	if p.StartsAt.Sub(now) > MaxScheduleAhead {
		return Plan{}, fmt.Errorf("events can be scheduled at most 60 days ahead")
	}
	p.EndsAt = endsAt.UTC()
	if !p.EndsAt.After(p.StartsAt.Add(14 * time.Minute)) {
		return Plan{}, fmt.Errorf("an event must last at least 15 minutes")
	}
	if p.EndsAt.Sub(p.StartsAt) > MaxEventDuration {
		return Plan{}, fmt.Errorf("an event can last at most 14 days")
	}
	for _, pts := range prizes {
		if pts < 0 || pts > MaxPrizePoints {
			return Plan{}, fmt.Errorf("prizes must be between 0 and 1,000,000 points")
		}
	}
	return p, nil
}
