package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestFormatLifeDuration(t *testing.T) {
	for seconds, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m", 3599: "59m", 3600: "1h 00m", 11100: "3h 05m", 86400: "1d 0h", 187200: "2d 4h"} {
		if got := FormatLifeDuration(seconds); got != want {
			t.Errorf("FormatLifeDuration(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func fieldValue(fields []string, name string) string {
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == name {
			return fields[i+1]
		}
	}
	return ""
}

func TestBuildLifeRecapEmbedShowsOnlyWhatWasRecorded(t *testing.T) {
	played, longest, dist, tracked := int64(11100), 412.3, 51.44, 2600.0
	killer := int64(9)
	life := repository.Life{PlayerName: "Semillita_azul", EndedAt: time.Unix(1_700_000_000, 0), PlaytimeSeconds: &played, Kills: 3, Headshots: 1,
		LongestKillM: &longest, TrackedDistance: &tracked, Cause: repository.LifeCausePVP, KillerPlayerID: &killer, KillerName: "@everyone", Weapon: "M4-A1", DistanceM: &dist}
	embed := BuildLifeRecapEmbed(life, "Chernarus PvP")
	var flat []string
	for _, f := range embed.Fields {
		flat = append(flat, f.Name, f.Value)
	}
	if !strings.Contains(embed.Description, `Semillita\_azul`) || !strings.Contains(embed.Description, "Chernarus PvP") {
		t.Fatalf("description = %q", embed.Description)
	}
	if strings.Contains(embed.Description, "@everyone") {
		t.Fatalf("killer name can ping: %q", embed.Description)
	}
	if !strings.Contains(embed.Description, "with M4-A1 from 51.4m") {
		t.Fatalf("ending line = %q", embed.Description)
	}
	if got := fieldValue(flat, "SURVIVED"); got != "3h 05m played" {
		t.Fatalf("SURVIVED = %q", got)
	}
	if got := fieldValue(flat, "KILLS"); got != "3 kills (1 headshot)" {
		t.Fatalf("KILLS = %q", got)
	}
	if got := fieldValue(flat, "TRACKED DISTANCE"); got != "at least 2.6 km" {
		t.Fatalf("TRACKED DISTANCE = %q", got)
	}

	// A life that began before recording, ended by a bare "died" line: no playtime, no distance,
	// no invented cause.
	bare := BuildLifeRecapEmbed(repository.Life{PlayerName: "X", Cause: repository.LifeCauseOther}, "")
	for _, f := range bare.Fields {
		if f.Name == "SURVIVED" || f.Name == "TRACKED DISTANCE" || f.Name == "LONGEST KILL" {
			t.Fatalf("unrecorded field shown: %s=%s", f.Name, f.Value)
		}
	}
	if !strings.Contains(bare.Description, "no killer recorded") {
		t.Fatalf("description = %q", bare.Description)
	}
}

func TestLifeBoardRowAndEmptyBoard(t *testing.T) {
	played, tracked := int64(7200), 950.0
	l := repository.Life{PlayerName: "Ace", PlaytimeSeconds: &played, Kills: 4, TrackedDistance: &tracked}
	if got := LifeBoardRow(l, repository.LifeMetricPlaytime); got != "**Ace** — 2h 00m • 4 kills" {
		t.Fatalf("playtime row = %q", got)
	}
	if got := LifeBoardRow(l, repository.LifeMetricKills); got != "**Ace** — 4 kills • 2h 00m" {
		t.Fatalf("kills row = %q", got)
	}
	if got := LifeBoardRow(l, repository.LifeMetricDistance); got != "**Ace** — 950 m • 4 kills" {
		t.Fatalf("distance row = %q", got)
	}
	if e := BuildLifeBoardEmbed("T", "S", nil); !strings.Contains(e.Description, "Nothing recorded yet") {
		t.Fatalf("empty board = %q", e.Description)
	}
	if e := BuildLifeBoardEmbed("T", "S", []string{"a", "b"}); !strings.Contains(e.Description, "` 2.` b") {
		t.Fatalf("board = %q", e.Description)
	}
}

func TestLifeRecapNotifierDropsWhenFullAndNilSafe(t *testing.T) {
	var none *LifeRecapNotifier
	none.Notify(repository.Life{}) // must not panic
	n := NewLifeRecapNotifier(nil, nil, nil)
	for i := 0; i < 100; i++ {
		n.Notify(repository.Life{})
	}
	if _, dropped := n.Stats(); dropped != 100-64 {
		t.Fatalf("dropped = %d, want 36", dropped)
	}
}
