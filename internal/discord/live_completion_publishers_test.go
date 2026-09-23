package discord

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Fake repositories for the completion publisher. IDs are distinctive
// five-digit numbers so a leaked raw ID is easy to detect.

type completionSeasonFake struct {
	seasons map[int64]*repository.Season
	results map[int64]*repository.SeasonResult
}

func (f *completionSeasonFake) GetSeasonByID(_ context.Context, _, id int64) (*repository.Season, error) {
	return f.seasons[id], nil
}
func (f *completionSeasonFake) GetSeasonResults(_ context.Context, _, id int64) (*repository.SeasonResult, error) {
	return f.results[id], nil
}
func (f *completionSeasonFake) GetEndedSeasons(context.Context, int64, int) ([]repository.Season, error) {
	return nil, nil
}

type completionWarFake struct {
	war            repository.War
	scoreA, scoreB int64
	topID, topKill int64
	longID         int64
	longest        float64
	noKills        bool
}

func (f *completionWarFake) GetWar(context.Context, int64, int64) (*repository.War, error) {
	w := f.war
	return &w, nil
}
func (f *completionWarFake) GetWarScore(context.Context, int64, int64) (int64, int64, error) {
	return f.scoreA, f.scoreB, nil
}
func (f *completionWarFake) GetWarTopKiller(context.Context, int64, int64) (int64, int64, error) {
	if f.noKills {
		return 0, 0, pgx.ErrNoRows
	}
	return f.topID, f.topKill, nil
}
func (f *completionWarFake) GetWarLongestKill(context.Context, int64, int64) (int64, float64, error) {
	if f.noKills {
		return 0, 0, pgx.ErrNoRows
	}
	return f.longID, f.longest, nil
}
func (f *completionWarFake) GetEndedWars(context.Context, int64, int) ([]repository.War, error) {
	return nil, nil
}

type completionEventFake struct {
	event repository.CompetitiveEvent
	rows  []repository.EventScore
}

func (f *completionEventFake) GetEvent(context.Context, int64, int64) (*repository.CompetitiveEvent, error) {
	e := f.event
	return &e, nil
}
func (f *completionEventFake) Leaderboard(_ context.Context, _ int64, limit int) ([]repository.EventScore, error) {
	if len(f.rows) > limit {
		return f.rows[:limit], nil
	}
	return f.rows, nil
}
func (f *completionEventFake) GetEndedEvents(context.Context, int64, int) ([]repository.CompetitiveEvent, error) {
	return nil, nil
}

type completionPlayersFake map[int64]string

func (f completionPlayersFake) DisplayNamesByID(_ context.Context, _ int64, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		if name, ok := f[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

type completionFactionsFake map[int64]repository.Faction

func (f completionFactionsFake) GetFactionsByID(_ context.Context, _ int64, ids []int64) (map[int64]repository.Faction, error) {
	out := map[int64]repository.Faction{}
	for _, id := range ids {
		if fa, ok := f[id]; ok {
			out[id] = fa
		}
	}
	return out, nil
}

var testPlayers = completionPlayersFake{
	70101: "WilliamAle--10",
	70102: "Semillita-azul-_",
	70103: "zTonii99",
}

var testFactions = completionFactionsFake{
	80201: {ID: 80201, Name: "Wolfpack", Tag: "WOLF"},
	80202: {ID: 80202, Name: "Ravens", Tag: "RVN"},
}

func completionEmbedText(e *discordgo.MessageEmbed) string {
	var b strings.Builder
	b.WriteString(e.Title + "\n" + e.Description + "\n")
	for _, f := range e.Fields {
		b.WriteString(f.Name + "\n" + f.Value + "\n")
	}
	if e.Footer != nil {
		b.WriteString(e.Footer.Text)
	}
	return b.String()
}

var rawIDPattern = regexp.MustCompile(`\b[78]0[12]\d\d\b`)

// assertNoPlaceholders fails on any placeholder identity or leaked raw ID.
func assertNoPlaceholders(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"Player ", "Faction ", "Winner ", "\nPlayer\n", "\nFaction\n", "Season\n", "Unknown", "N/A"} {
		if strings.Contains(text, bad) {
			t.Errorf("card contains placeholder %q:\n%s", bad, text)
		}
	}
	if id := rawIDPattern.FindString(text); id != "" {
		t.Errorf("card leaks raw ID %s:\n%s", id, text)
	}
}

func fieldByName(e *discordgo.MessageEmbed, name string) *discordgo.MessageEmbedField {
	for _, f := range e.Fields {
		if f.Name == name {
			return f
		}
	}
	return nil
}

func TestSeasonCompletionResolvesHolderNames(t *testing.T) {
	p := &LiveCompletionPublisher{
		seasons: &completionSeasonFake{
			seasons: map[int64]*repository.Season{5: {ID: 5, Name: "Season 3"}},
			results: map[int64]*repository.SeasonResult{5: {
				SeasonID: 5, TopPlayerID: 70101, TopPlayerKills: 20,
				TopFactionID: 80201, TopFactionKills: 55,
				LongestKillPlayerID: 70102, LongestKillValue: 298.4,
				BestStreakPlayerID: 70103, BestStreakValue: 8,
			}},
		},
		players:  testPlayers,
		factions: testFactions,
	}
	e, err := p.buildSeasonCompletion(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	text := completionEmbedText(e)
	assertNoPlaceholders(t, text)
	for field, want := range map[string]string{
		"👑 TOP PLAYER":   "WilliamAle--10",
		"⚔️ TOP FACTION": `\[WOLF\] Wolfpack`,
		"🎯 LONGEST KILL": `Semillita-azul-\_`,
		"🔥 BEST STREAK":  "zTonii99",
	} {
		f := fieldByName(e, field)
		if f == nil || !strings.Contains(f.Value, want) {
			t.Errorf("field %q = %+v, want holder %q", field, f, want)
		}
	}
}

func TestSeasonCompletionOmitsUnresolvedHolders(t *testing.T) {
	p := &LiveCompletionPublisher{
		seasons: &completionSeasonFake{
			seasons: map[int64]*repository.Season{5: {ID: 5, Name: "Season 3"}},
			// 70199 / 80299 have no rows; 0 means no holder at all.
			results: map[int64]*repository.SeasonResult{5: {SeasonID: 5, TopPlayerID: 70199, TopPlayerKills: 3, TopFactionID: 80299, TopFactionKills: 4}},
		},
		players:  testPlayers,
		factions: testFactions,
	}
	e, err := p.buildSeasonCompletion(context.Background(), 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	assertNoPlaceholders(t, completionEmbedText(e))
	if v := fieldByName(e, "👑 TOP PLAYER").Value; v != "**3 Kills**" {
		t.Errorf("unresolved top player value = %q, want count only", v)
	}
}

func TestWarCompletionResolvesFactionsWinnerAndRecords(t *testing.T) {
	winner := int64(80201)
	p := &LiveCompletionPublisher{
		seasons: &completionSeasonFake{seasons: map[int64]*repository.Season{5: {ID: 5, Name: "Season 3"}}},
		wars: &completionWarFake{
			war:    repository.War{ID: 9, SeasonID: 5, FactionAID: 80201, FactionBID: 80202, WinnerFactionID: &winner},
			scoreA: 14, scoreB: 9,
			topID: 70101, topKill: 7,
			longID: 70103, longest: 312.6,
		},
		players:  testPlayers,
		factions: testFactions,
	}
	e, err := p.buildWarCompletion(context.Background(), 1, 9)
	if err != nil {
		t.Fatal(err)
	}
	text := completionEmbedText(e)
	assertNoPlaceholders(t, text)
	for _, want := range []string{`**\[WOLF\] Wolfpack** vs **\[RVN\] Ravens**`, `👑 **\[WOLF\] Wolfpack** wins the war`, "CHAMPION • Season 3 •"} {
		if !strings.Contains(text, want) {
			t.Errorf("card missing %q:\n%s", want, text)
		}
	}
	if f := fieldByName(e, "[WOLF] Wolfpack"); f == nil || f.Value != "**14 Kills**" || !f.Inline {
		t.Errorf("faction A field = %+v", f)
	}
	if f := fieldByName(e, "[RVN] Ravens"); f == nil || f.Value != "**9 Kills**" || !f.Inline {
		t.Errorf("faction B field = %+v", f)
	}
	if f := fieldByName(e, "🔥 TOP KILLER"); f == nil || f.Value != "**7 Kills**\nWilliamAle--10" {
		t.Errorf("top killer field = %+v", f)
	}
	if f := fieldByName(e, "🎯 LONGEST KILL"); f == nil || !strings.Contains(f.Value, "zTonii99") {
		t.Errorf("longest kill field = %+v", f)
	}
}

func TestWarCompletionWithoutKillsIsADrawWithNoRecords(t *testing.T) {
	p := &LiveCompletionPublisher{
		wars:     &completionWarFake{war: repository.War{ID: 9, FactionAID: 80201, FactionBID: 80202}, noKills: true},
		players:  testPlayers,
		factions: testFactions,
	}
	e, err := p.buildWarCompletion(context.Background(), 1, 9)
	if err != nil {
		t.Fatal(err)
	}
	text := completionEmbedText(e)
	assertNoPlaceholders(t, text)
	if !strings.Contains(e.Description, "🤝 **DRAW**") {
		t.Errorf("tied war without winner should be a draw: %q", e.Description)
	}
	if fieldByName(e, "🔥 TOP KILLER") != nil || fieldByName(e, "🎯 LONGEST KILL") != nil {
		t.Errorf("war with no kills must not show records:\n%s", text)
	}
	if strings.Contains(e.Footer.Text, "CHAMPION •") {
		t.Errorf("war without a season must not invent one: %q", e.Footer.Text)
	}
}

func TestWarCompletionUnresolvedFactionsAndHoldersAreOmitted(t *testing.T) {
	p := &LiveCompletionPublisher{
		wars: &completionWarFake{
			war:    repository.War{ID: 9, FactionAID: 80298, FactionBID: 80299},
			scoreA: 3, scoreB: 1,
			topID: 70199, topKill: 2, longID: 70199, longest: 50,
		},
		players:  testPlayers,
		factions: testFactions,
	}
	e, err := p.buildWarCompletion(context.Background(), 1, 9)
	if err != nil {
		t.Fatal(err)
	}
	text := completionEmbedText(e)
	assertNoPlaceholders(t, text)
	if e.Description != "" {
		t.Errorf("no resolved factions/winner: description should be empty, got %q", e.Description)
	}
	if fieldByName(e, "🔥 TOP KILLER") != nil || fieldByName(e, "🎯 LONGEST KILL") != nil {
		t.Errorf("unresolved holders must be omitted:\n%s", text)
	}
}

func TestEventCompletionResolvesPodiumNames(t *testing.T) {
	p := &LiveCompletionPublisher{
		seasons: &completionSeasonFake{seasons: map[int64]*repository.Season{5: {ID: 5, Name: "Season 3"}}},
		events: &completionEventFake{
			event: repository.CompetitiveEvent{ID: 4, SeasonID: 5, Name: "Sniper Sunday"},
			rows: []repository.EventScore{
				{PlayerID: 70102, Score: 1250},
				{PlayerID: 70199, Score: 900}, // no player row: dropped
				{PlayerID: 70101, Score: 412.5},
			},
		},
		players: testPlayers,
	}
	e, err := p.buildEventCompletion(context.Background(), 1, 4)
	if err != nil {
		t.Fatal(err)
	}
	text := completionEmbedText(e)
	assertNoPlaceholders(t, text)
	f := fieldByName(e, "🏆 FINAL STANDINGS")
	if f == nil {
		t.Fatalf("missing standings field:\n%s", text)
	}
	want := "🥇 Semillita-azul-\\_ • **1,250 pts**\n🥉 WilliamAle--10 • **412.5 pts**"
	if f.Value != want {
		t.Errorf("standings = %q, want %q", f.Value, want)
	}
	if !strings.Contains(e.Description, "Sniper Sunday") || !strings.Contains(e.Footer.Text, "Season 3") {
		t.Errorf("event name/season missing:\n%s", text)
	}
}

func TestNewLiveCompletionPublisherNilRepositoriesAreSafe(t *testing.T) {
	p := NewLiveCompletionPublisher(nil, nil, nil, nil, nil, nil, nil, nil, nil, "")
	if p.seasons != nil || p.wars != nil || p.events != nil || p.players != nil || p.factions != nil {
		t.Fatal("nil repositories must stay nil interfaces")
	}
	if err := p.RecoverPending(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}
