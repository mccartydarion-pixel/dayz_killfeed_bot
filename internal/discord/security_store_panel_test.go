package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestSecurityStorePanel(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	empty := SecurityStorePanel(nil, "Champions", "", at)
	if e := empty.Embeds[0]; !strings.Contains(e.Description, "Nothing is on sale") || len(e.Fields) != 0 || empty.Components != nil {
		t.Fatalf("empty: %+v", e)
	}
	full := SecurityStorePanel([]SecurityPanelItem{
		{ServiceID: "BASE_RAID_ALARM", PricePoints: 1500, DurationDays: 7},
		{ServiceID: "SENTINEL_PRO", PricePoints: 4000, DurationDays: 30, Includes: []string{"BASE_RAID_ALARM", "BASE_BLACK_BOX"}},
		{ServiceID: "MADE_UP", PricePoints: 1, DurationDays: 1},
	}, "Champions", "https://site.example/dashboard/player/security-store", at)
	e := full.Embeds[0]
	if len(e.Fields) != 3 || !strings.Contains(e.Fields[0].Name, "1,500 pts / 7 days") {
		t.Fatalf("fields: %+v", e.Fields)
	}
	if !strings.Contains(e.Fields[1].Value, "Includes: Base Raid Alarm, Base Black Box") || !strings.Contains(e.Fields[2].Value, "/registerbase") {
		t.Fatalf("bundle/how-to: %+v %+v", e.Fields[1], e.Fields[2])
	}
	row, ok := full.Components[0].(discordgo.ActionsRow)
	if !ok || row.Components[0].(discordgo.Button).URL != "https://site.example/dashboard/player/security-store" {
		t.Fatalf("button: %+v", full.Components)
	}
	if insecure := SecurityStorePanel(nil, "", "http://site.example", at); insecure.Components != nil {
		t.Fatal("a non-https store link must not get a button")
	}
	if len(full.AllowedMentions.Parse) != 0 {
		t.Fatal("mentions must be disabled")
	}
}
