package tournament

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveShapeMatchesFixture builds a mid-tournament state like the one in
// testdata/live-fixture.json (the contract the website is built against), renders it through
// the same types the public route uses and compares the key sets recursively.
func TestLiveShapeMatchesFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/live-fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}

	tr := started(t, 8, 8, 3)
	title := "Friday Champion"
	tr.Prizes = []Prize{{Place: 1, Points: 1000, Title: &title}, {Place: 2, Points: 500}, {Place: 3, Points: 250}}
	tr.Rules.AllowedWeapons = []string{"M4-A1"}
	// Quarter-final 1 decided 2-0, quarter-final 2 decided 2-1, quarter-final 3 forfeited,
	// quarter-final 4 live at 1-1 with a flagged round.
	at := t0.Add(2 * time.Minute)
	for i := 0; i < 2; i++ {
		m := tr.Current()
		a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
		for n := 0; n < 2; n++ {
			d := tr.Attribute(kill(int64(10*i+n+1), a, b, "M4-A1", at))
			if _, err := tr.RecordRound(m, d.Round, at); err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Minute)
		}
	}
	m := tr.Current()
	if _, err := tr.Forfeit(m, m.EntryA, at, "absent"); err != nil {
		t.Fatal(err)
	}
	m = tr.Current()
	a, b := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	d := tr.Attribute(kill(31, a, b, "M4-A1", at))
	if _, err := tr.RecordRound(m, d.Round, at); err != nil {
		t.Fatal(err)
	}
	d = tr.Attribute(kill(32, b, a, "KA-M", at.Add(time.Minute)))
	if _, err := tr.RecordRound(m, d.Round, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if tr.Status != StatusLive || m.Status != MatchLive || tr.Current() != m {
		t.Fatalf("state: %s %s", tr.Status, m.Status)
	}
	info := map[int64]PlayerInfo{}
	stats := map[int64]Stats{}
	for i, e := range tr.Entries {
		p := e.Players[0].PlayerID
		pi := PlayerInfo{RankTier: "GOLD"}
		if i%2 == 0 {
			pi.Faction = &FactionDTO{Tag: "NEX", Name: "Nex", Color: "#E0162A"}
		}
		info[p] = pi
		stats[p] = Stats{Kills: 10 * i, Deaths: 3, Headshots: 1, LongestKillMeters: 120, FavouriteWeapon: "M4-A1"}
	}
	live := LiveDTO{InstallationID: 7, Server: ServerDTO{Name: "Server", Platform: "PLAYSTATION", Map: "chernarusplus", DiscordInvite: "https://discord.gg/x", OnlinePlayers: 3},
		Tournament: ToDTO(tr, info), Fighters: Fighters(tr, stats), GeneratedAt: rfc(at)}
	out, err := json.Marshal(live)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	var problems []string
	compareShape("$", want, got, &problems)
	if len(problems) > 0 {
		t.Fatalf("shape differs from the fixture:\n%s\n%s", strings.Join(problems, "\n"), out)
	}
	// Spot checks that go beyond keys: the current match, the results order, a flagged round.
	td := live.Tournament
	if td.Current == nil || td.Current.MatchID != m.ID || td.Current.TimerEndsAt == nil || td.Champion != nil {
		t.Fatalf("current: %+v", td.Current)
	}
	if len(td.Results) == 0 || td.Results[0].MatchID != m.ID || td.Results[0].N != 2 || td.Results[0].Flag == nil || *td.Results[0].Flag != FlagWeapon || td.Results[0].Counted {
		t.Fatalf("results: %+v", td.Results)
	}
	f := live.Fighters[strconv.FormatInt(a.Players[0].PlayerID, 10)]
	if f.Record.RoundsWon != 1 || f.Record.RoundsLost != 0 || f.FavouriteWeapon != "M4-A1" {
		t.Fatalf("fighter: %+v", f)
	}
}

// compareShape compares key sets recursively. A null on either side matches anything (the
// fixture shows a null where a value is optional); arrays and numeric-keyed maps compare the
// union of their elements' keys.
func compareShape(path string, want, got any, problems *[]string) {
	if want == nil || got == nil {
		return
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s: fixture has an object, got %T", path, got))
			return
		}
		if numericKeys(w) {
			var wv, gv []any
			for _, v := range w {
				wv = append(wv, v)
			}
			for _, v := range g {
				gv = append(gv, v)
			}
			compareShape(path+"[*]", merge(wv), merge(gv), problems)
			return
		}
		wk, gk := keys(w), keys(g)
		if strings.Join(wk, ",") != strings.Join(gk, ",") {
			*problems = append(*problems, fmt.Sprintf("%s: keys %v, fixture %v", path, gk, wk))
		}
		for k, v := range w {
			if gvv, ok := g[k]; ok {
				compareShape(path+"."+k, v, gvv, problems)
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s: fixture has an array, got %T", path, got))
			return
		}
		compareShape(path+"[*]", merge(w), merge(g), problems)
	default:
		wt, gt := fmt.Sprintf("%T", want), fmt.Sprintf("%T", got)
		if wt != gt {
			*problems = append(*problems, fmt.Sprintf("%s: fixture is %s, got %s", path, wt, gt))
		}
	}
}

// merge folds the elements of an array into one representative: objects merge their keys (a
// null value is replaced by a non-null one), nested arrays merge recursively.
func merge(items []any) any {
	if len(items) == 0 {
		return nil
	}
	var out any
	for _, it := range items {
		switch v := it.(type) {
		case map[string]any:
			m, _ := out.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			for k, val := range v {
				if cur, ok := m[k]; !ok || cur == nil {
					m[k] = val
				} else if arr, ok := cur.([]any); ok {
					if more, ok := val.([]any); ok {
						m[k] = append(arr, more...)
					}
				} else if obj, ok := cur.(map[string]any); ok {
					if more, ok := val.(map[string]any); ok {
						m[k] = merge([]any{obj, more})
					}
				}
			}
			out = m
		case []any:
			arr, _ := out.([]any)
			out = append(arr, v...)
		default:
			if out == nil {
				out = v
			}
		}
	}
	return out
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func numericKeys(m map[string]any) bool {
	if len(m) == 0 {
		return false
	}
	for k := range m {
		if _, err := strconv.ParseInt(k, 10, 64); err != nil {
			return false
		}
	}
	return true
}
