package analytics

import "time"

type Scope string

const (
	ScopeLifetime Scope = "LIFETIME"
	ScopeSeason   Scope = "SEASON"
	ScopeSession  Scope = "SESSION"
)

type Range string

const (
	CloseRange   Range = "CLOSE"
	MediumRange  Range = "MEDIUM"
	LongRange    Range = "LONG"
	ExtremeRange Range = "EXTREME"
)

type PlayerAnalytics struct {
	Name                                             string
	Kills, Deaths                                    int64
	KD                                               float64
	CurrentStreak, BestStreak                        int
	AverageDistance, MedianDistance                  *float64
	LongestKill                                      *float64
	Headshots                                        int64
	HeadshotRate                                     float64
	CloseKills, MediumKills, LongKills, ExtremeKills int64
	UniqueVictims, UniqueKillers                     int64
	TopWeapon                                        string
	TopWeaponKills                                   int64
	MostKilled, Nemesis                              string
	MostKilledCount, NemesisCount                    int64
	RecentKills, RecentDeaths                        int64
	RecentKD                                         float64
	RecentForm                                       string
}
type Matchup struct {
	PlayerA, PlayerB string
	AKills, BKills   int64
	Total            int64
	LastEncounter    *time.Time
	Longest          *float64
	AWeapon, BWeapon string
}
type WeaponStats struct {
	Weapon                           string
	Kills, UniqueUsers, Headshots    int64
	AverageDistance, LongestDistance *float64
	HeadshotRate                     float64
	TopPlayer, TopFaction            string
}

func Form(kills, deaths int64) string {
	engagements := kills + deaths
	if engagements < 10 {
		return "STEADY"
	}
	kd := float64(kills)
	if deaths > 0 {
		kd /= float64(deaths)
	}
	if kd >= 2 {
		return "HOT"
	}
	if kd < .75 {
		return "COLD"
	}
	return "STEADY"
}
func Rate(headshots, kills int64) float64 {
	if kills == 0 {
		return 0
	}
	return float64(headshots) / float64(kills) * 100
}
