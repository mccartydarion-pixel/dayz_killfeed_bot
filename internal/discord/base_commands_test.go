package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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

func TestRentButtons(t *testing.T) {
	if MyBaseComponents(BaseCommandSummary{Rent: []BaseRentLine{{BaseID: 1, BaseName: "Hut"}}}) != nil {
		t.Fatal("no price: no buttons")
	}
	sum := BaseCommandSummary{RentPrice: 400, RentDays: 7}
	for i := 1; i <= 7; i++ {
		sum.Rent = append(sum.Rent, BaseRentLine{BaseID: int64(i), BaseName: strings.Repeat("x", 90)})
	}
	rows := MyBaseComponents(sum)
	buttons := rows[0].(discordgo.ActionsRow).Components
	if len(rows) != 1 || len(buttons) != 5 {
		t.Fatalf("at most 5 buttons: %d", len(buttons))
	}
	b := buttons[0].(discordgo.Button)
	if b.CustomID != "baserent:ask:1" || len([]rune(b.Label)) > 80 || !IsBaseRentInteraction(b.CustomID) {
		t.Fatalf("button: %+v", b)
	}
	text, confirm := RentConfirmMessage(9, RentQuote{BaseName: "Hill*top*", PricePoints: 12500, PeriodDays: 7})
	if !strings.Contains(text, "12,500") || !strings.Contains(text, "7 more days") || strings.Contains(text, "*top*") {
		t.Fatalf("confirm text: %q", text)
	}
	cb := confirm[0].(discordgo.ActionsRow).Components
	if cb[0].(discordgo.Button).CustomID != "baserent:pay:9" || cb[1].(discordgo.Button).CustomID != "baserent:cancel" {
		t.Fatalf("confirm buttons: %+v", cb)
	}
	for _, bad := range []string{"baserent:pay:", "baserent:pay:-3", "baserent:pay:x", "baserent:ask:5"} {
		if _, ok := parseRentBaseID(bad, rentPayPrefix); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
	if id, ok := parseRentBaseID("baserent:pay:42", rentPayPrefix); !ok || id != 42 {
		t.Fatal("parse 42")
	}
	if plainLabel("\x00\x01 ", 10) != "base" {
		t.Fatal("empty label")
	}
	if !strings.Contains(RentPaidMessage(RentPaid{BaseName: "Hut", PaidUntil: time.Unix(1_800_000_000, 0), Balance: 1200}), "1,200") {
		t.Fatal("paid message")
	}
}
