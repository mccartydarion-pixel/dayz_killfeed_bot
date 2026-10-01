package discord

import (
	"strings"
	"testing"
	"time"
)

func TestMyBaseEmbed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	empty := MyBaseEmbed(BaseCommandSummary{}, "", now)
	if len(empty.Fields) != 2 || !strings.Contains(empty.Fields[0].Value, "/registerbase") || !strings.Contains(empty.Fields[1].Value, "None") {
		t.Fatalf("empty: %+v", empty.Fields)
	}
	full := MyBaseEmbed(BaseCommandSummary{
		Bases: []string{"Hill*top*", "Shack"}, Pending: "Cabin",
		PaidUntil: []BasePaidTime{{Label: "Sentinel Pro", Until: now.Add(48 * time.Hour)}},
	}, "https://site.example/dashboard/player/security-store", now)
	if len(full.Fields) != 4 || strings.Contains(full.Fields[0].Value, "*top*") || full.Fields[1].Name != "Waiting for the server owner" ||
		!strings.Contains(full.Fields[2].Value, "Sentinel Pro") || !strings.Contains(full.Fields[3].Value, "security-store") {
		t.Fatalf("full: %+v", full.Fields)
	}
	answered := MyBaseEmbed(BaseCommandSummary{LastAnswer: "Shack was declined: too close"}, "", now)
	if answered.Fields[1].Name != "Last request" {
		t.Fatalf("answered: %+v", answered.Fields)
	}
	if len(BaseCommandSizes) != 5 || BaseCommandSizes[0] != 25 || BaseCommandSizes[4] != 150 {
		t.Fatalf("sizes: %v", BaseCommandSizes)
	}
}

func TestMyBaseEmbedRent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	e := MyBaseEmbed(BaseCommandSummary{Bases: []string{"Hut"}, Rent: []BaseRentLine{
		{BaseName: "Hut", DueAt: now.Add(48 * time.Hour)},
		{BaseName: "Shack", DueAt: now.Add(-time.Hour)},
		{BaseName: "Cabin", DueAt: now.Add(-96 * time.Hour), Paused: true},
	}}, "", now)
	var rent string
	for _, f := range e.Fields {
		if f.Name == "Rent" {
			rent = f.Value
		}
	}
	if !strings.Contains(rent, "Hut: rent paid until") || !strings.Contains(rent, "Shack: rent was due") || !strings.Contains(rent, "Cabin: paused") {
		t.Fatalf("rent lines: %q", rent)
	}
}
