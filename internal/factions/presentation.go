package factions

import "time"

type WarSummary struct {
	Tag           string
	Name          string
	Score         int64
	OpponentScore int64
}

type InfoPresentation struct {
	Name, Tag, OwnerName                                              string
	MemberCount, MaxMembers                                           int
	SeasonName                                                        string
	SeasonRank                                                        *int
	SeasonKills, SeasonDeaths, EnemyFactionKills, TeamKills, WarKills int64
	SeasonKD                                                          float64
	WarWins, WarLosses, WarDraws                                      int
	LongestKillDistance                                               *float64
	LongestKillPlayerName                                             string
	BestStreak                                                        int
	BestStreakPlayerName                                              string
	ChampionPoints                                                    int64
	PrimaryRivalName, PrimaryRivalTag                                 string
	RivalKillsFor, RivalKillsAgainst                                  int64
	ActiveWars                                                        []WarSummary
	CreatedAt                                                         time.Time
}

func (p InfoPresentation) HasWarRecord() bool   { return p.WarWins+p.WarLosses+p.WarDraws > 0 }
func (p InfoPresentation) HasLongestKill() bool { return p.LongestKillDistance != nil }
func (p InfoPresentation) HasRival() bool       { return p.PrimaryRivalTag != "" }
