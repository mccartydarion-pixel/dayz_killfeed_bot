package tournament

import (
	"sort"
	"strconv"
	"time"
)

// The public JSON shape (docs/TOURNAMENTS.md "Public live shape"): what the website's live
// tournament page reads. The owner and player routes carry the same TournamentDTO.

type ArenaDTO = Arena

type RulesDTO struct {
	AllowedWeapons    []string   `json:"allowedWeapons"`
	Arenas            []ArenaDTO `json:"arenas"`
	MatchTimerMinutes int        `json:"matchTimerMinutes"`
	ReadyMinutes      int        `json:"readyMinutes"`
}

type PrizeDTO struct {
	Place  int     `json:"place"`
	Points int64   `json:"points"`
	Title  *string `json:"title"`
}

type FactionDTO struct {
	Tag   string `json:"tag"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type EntryPlayerDTO struct {
	PlayerID int64       `json:"playerId"`
	Name     string      `json:"name"`
	RankTier string      `json:"rankTier"`
	Faction  *FactionDTO `json:"faction"`
}

type EntryDTO struct {
	ID        int64            `json:"id"`
	Seed      *int             `json:"seed"`
	TeamNo    int              `json:"teamNo"`
	CheckedIn bool             `json:"checkedIn"`
	Status    string           `json:"status"`
	Players   []EntryPlayerDTO `json:"players"`
}

type RoundDTO struct {
	N          int      `json:"n"`
	Winner     *int64   `json:"winner"`
	KillerName string   `json:"killerName"`
	VictimName string   `json:"victimName"`
	Weapon     string   `json:"weapon"`
	Distance   *float64 `json:"distance"`
	At         string   `json:"at"`
	Counted    bool     `json:"counted"`
	Flag       *string  `json:"flag"`
}

type ResultDTO struct {
	MatchID int64 `json:"matchId"`
	RoundDTO
}

type MatchDTO struct {
	ID          int64      `json:"id"`
	Round       int        `json:"round"`
	Position    int        `json:"position"`
	RoundName   string     `json:"roundName"`
	A           *int64     `json:"a"`
	B           *int64     `json:"b"`
	Status      string     `json:"status"`
	ScoreA      int        `json:"scoreA"`
	ScoreB      int        `json:"scoreB"`
	Winner      *int64     `json:"winner"`
	Arena       *int       `json:"arena"`
	CalledAt    *string    `json:"calledAt"`
	StartedAt   *string    `json:"startedAt"`
	EndedAt     *string    `json:"endedAt"`
	NextMatchID *int64     `json:"nextMatchId"`
	Rounds      []RoundDTO `json:"rounds"`
}

type CurrentDTO struct {
	MatchID     int64   `json:"matchId"`
	TimerEndsAt *string `json:"timerEndsAt"`
}

// ChampionDTO is the winner of a finished tournament.
type ChampionDTO struct {
	EntryID int64            `json:"entryId"`
	Players []EntryPlayerDTO `json:"players"`
	Title   *string          `json:"title"`
}

type TournamentDTO struct {
	ID             int64        `json:"id"`
	Name           string       `json:"name"`
	Status         string       `json:"status"`
	Format         string       `json:"format"`
	TeamSize       int          `json:"teamSize"`
	BracketSize    int          `json:"bracketSize"`
	BestOf         int          `json:"bestOf"`
	Seeding        string       `json:"seeding"`
	StartsAt       string       `json:"startsAt"`
	SignupOpensAt  *string      `json:"signupOpensAt"`
	CheckinMinutes int          `json:"checkinMinutes"`
	StartedAt      *string      `json:"startedAt"`
	FinishedAt     *string      `json:"finishedAt"`
	Rules          RulesDTO     `json:"rules"`
	Prizes         []PrizeDTO   `json:"prizes"`
	Entries        []EntryDTO   `json:"entries"`
	Matches        []MatchDTO   `json:"matches"`
	Current        *CurrentDTO  `json:"current"`
	Results        []ResultDTO  `json:"results"`
	Champion       *ChampionDTO `json:"champion"`
}

type RecordDTO struct {
	Wins       int `json:"wins"`
	Losses     int `json:"losses"`
	RoundsWon  int `json:"roundsWon"`
	RoundsLost int `json:"roundsLost"`
}

// FighterDTO is a player's server figures plus their tournament record.
type FighterDTO struct {
	Kills             int       `json:"kills"`
	Deaths            int       `json:"deaths"`
	KD                float64   `json:"kd"`
	Headshots         int       `json:"headshots"`
	LongestKillMeters float64   `json:"longestKillMeters"`
	FavouriteWeapon   string    `json:"favouriteWeapon"`
	Record            RecordDTO `json:"record"`
}

type ServerDTO struct {
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	Map           string `json:"map"`
	DiscordInvite string `json:"discordInvite"`
	OnlinePlayers int    `json:"onlinePlayers"`
}

// LiveDTO is GET /api/live/{installationID}/tournament.
type LiveDTO struct {
	InstallationID int64                 `json:"installationId"`
	Server         ServerDTO             `json:"server"`
	Tournament     *TournamentDTO        `json:"tournament"`
	Fighters       map[string]FighterDTO `json:"fighters"`
	GeneratedAt    string                `json:"generatedAt"`
}

// PlayerInfo is what the caller knows about a player beyond the tournament: their tier and
// faction on the server.
type PlayerInfo struct {
	RankTier string
	Faction  *FactionDTO
}

// Stats is a player's server figures as the fighters block shows them.
type Stats struct {
	Kills, Deaths, Headshots int
	LongestKillMeters        float64
	FavouriteWeapon          string
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func rfcPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := rfc(*t)
	return &s
}

// ResultsLimit is how many recent results the public shape lists.
const ResultsLimit = 10

// ToDTO renders a tournament. info may be nil (tiers are UNRANKED, factions null).
func ToDTO(t *Tournament, info map[int64]PlayerInfo) *TournamentDTO {
	if t == nil {
		return nil
	}
	d := &TournamentDTO{
		ID: t.ID, Name: t.Name, Status: t.Status, Format: t.Format, TeamSize: t.TeamSize, BracketSize: t.BracketSize, BestOf: t.BestOf, Seeding: t.Seeding,
		StartsAt: rfc(t.StartsAt), SignupOpensAt: rfcPtr(t.SignupOpensAt), CheckinMinutes: t.CheckinMinutes, StartedAt: rfcPtr(t.StartedAt), FinishedAt: rfcPtr(t.FinishedAt),
		Rules:   RulesDTO{AllowedWeapons: append([]string{}, t.Rules.AllowedWeapons...), Arenas: append([]ArenaDTO{}, t.Rules.Arenas...), MatchTimerMinutes: t.Rules.MatchTimerMinutes, ReadyMinutes: t.Rules.ReadyMinutes},
		Prizes:  []PrizeDTO{},
		Entries: []EntryDTO{},
		Matches: []MatchDTO{},
		Results: []ResultDTO{},
	}
	for _, p := range t.Prizes {
		d.Prizes = append(d.Prizes, PrizeDTO{Place: p.Place, Points: p.Points, Title: p.Title})
	}
	players := func(e *Entry) []EntryPlayerDTO {
		out := make([]EntryPlayerDTO, 0, len(e.Players))
		for _, p := range e.Players {
			pd := EntryPlayerDTO{PlayerID: p.PlayerID, Name: p.Name, RankTier: "UNRANKED"}
			if i, ok := info[p.PlayerID]; ok {
				if i.RankTier != "" {
					pd.RankTier = i.RankTier
				}
				pd.Faction = i.Faction
			}
			out = append(out, pd)
		}
		return out
	}
	for _, e := range t.Entries {
		d.Entries = append(d.Entries, EntryDTO{ID: e.ID, Seed: e.Seed, TeamNo: e.TeamNo, CheckedIn: e.CheckedIn(), Status: e.Status, Players: players(e)})
	}
	var results []ResultDTO
	for _, m := range t.Matches {
		md := MatchDTO{ID: m.ID, Round: m.Round, Position: m.Position, RoundName: m.RoundName, A: m.EntryA, B: m.EntryB, Status: m.Status, ScoreA: m.ScoreA, ScoreB: m.ScoreB,
			Winner: m.WinnerEntry, Arena: m.ArenaNo, CalledAt: rfcPtr(m.CalledAt), StartedAt: rfcPtr(m.StartedAt), EndedAt: rfcPtr(m.EndedAt), NextMatchID: m.NextMatchID, Rounds: []RoundDTO{}}
		for _, r := range m.Rounds {
			rd := RoundDTO{N: r.N, Winner: r.WinnerEntry, KillerName: r.KillerName, VictimName: r.VictimName, Weapon: r.Weapon, Distance: r.Distance, At: rfc(r.At), Counted: r.Counted, Flag: r.Flag}
			md.Rounds = append(md.Rounds, rd)
			results = append(results, ResultDTO{MatchID: m.ID, RoundDTO: rd})
		}
		d.Matches = append(d.Matches, md)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].At > results[j].At || (results[i].At == results[j].At && results[i].N > results[j].N)
	})
	if len(results) > ResultsLimit {
		results = results[:ResultsLimit]
	}
	d.Results = append(d.Results, results...)
	if m := t.Current(); m != nil && t.Running() {
		d.Current = &CurrentDTO{MatchID: m.ID, TimerEndsAt: rfcPtr(m.TimerEndsAt)}
	}
	if c := t.Champion(); c != nil {
		cd := &ChampionDTO{EntryID: c.ID, Players: players(c)}
		for _, p := range t.Prizes {
			if p.Place == 1 {
				cd.Title = p.Title
			}
		}
		d.Champion = cd
	}
	return d
}

// Fighters renders the fighters block: every player of every entry, keyed by player id.
func Fighters(t *Tournament, stats map[int64]Stats) map[string]FighterDTO {
	out := map[string]FighterDTO{}
	if t == nil {
		return out
	}
	for _, e := range t.Entries {
		rec := t.RecordOf(e.ID)
		for _, p := range e.Players {
			s := stats[p.PlayerID]
			kd := float64(s.Kills)
			if s.Deaths > 0 {
				kd = float64(s.Kills) / float64(s.Deaths)
			}
			out[strconv.FormatInt(p.PlayerID, 10)] = FighterDTO{Kills: s.Kills, Deaths: s.Deaths, KD: round2(kd), Headshots: s.Headshots, LongestKillMeters: s.LongestKillMeters,
				FavouriteWeapon: s.FavouriteWeapon, Record: RecordDTO{Wins: rec.Wins, Losses: rec.Losses, RoundsWon: rec.RoundsWon, RoundsLost: rec.RoundsLost}}
		}
	}
	return out
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
