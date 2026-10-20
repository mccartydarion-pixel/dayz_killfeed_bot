package app

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func recruitButtons(t *testing.T, comps []discordgo.MessageComponent) []discordgo.Button {
	t.Helper()
	if len(comps) != 1 {
		t.Fatalf("one action row expected, got %d", len(comps))
	}
	row, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("action row expected, got %T", comps[0])
	}
	out := []discordgo.Button{}
	for _, c := range row.Components {
		b, ok := c.(discordgo.Button)
		if !ok {
			t.Fatalf("button expected, got %T", c)
		}
		out = append(out, b)
	}
	return out
}

func TestFactionRecruitCardButtonsFollowRecruitment(t *testing.T) {
	red, wolf, band := "#FF1726", "WOLF", "RED"
	hours := 200
	f := repository.HubFaction{ID: 42, Name: "Wasteland Kings", Tag: "WK", Description: "NWAF regulars.", RecruitmentStatus: factionhub.RecruitmentOpen, MemberCount: 7, PrimaryColor: &red, FlagKey: &wolf, ArmbandKey: &band, Settings: factionhub.Settings{MinimumHours: &hours, MicRequired: true, CustomRequirements: "EU evenings"}}
	leader := &repository.HubMember{User: repository.HubUser{Username: "deelo", GlobalName: "Deelo"}}

	embed, comps := BuildFactionRecruitCard(f, leader, "https://cdn.example/logo.png", "https://championshp.vip")
	if embed.Title != "[WK] Wasteland Kings" || embed.Color != 0xFF1726 || embed.Thumbnail == nil || embed.Thumbnail.URL != "https://cdn.example/logo.png" {
		t.Fatalf("embed header: %+v", embed)
	}
	if !strings.Contains(embed.Description, "NWAF regulars.") || !strings.Contains(embed.Description, "**Join**") {
		t.Fatalf("description: %q", embed.Description)
	}
	if embed.URL != "https://championshp.vip/dashboard/player/factions/42" {
		t.Fatalf("url: %s", embed.URL)
	}
	joined := map[string]string{}
	for _, fld := range embed.Fields {
		joined[fld.Name] = fld.Value
	}
	if joined["Members"] != "7" || joined["Leader"] != "Deelo" || joined["Recruitment"] != "Open" || !strings.Contains(joined["In game"], "Wolf") || !strings.Contains(joined["In game"], "Red armband") || !strings.Contains(joined["Requirements"], "200+ hours") || !strings.Contains(joined["Requirements"], "EU evenings") {
		t.Fatalf("fields: %v", joined)
	}
	buttons := recruitButtons(t, comps)
	if len(buttons) != 2 || buttons[0].CustomID != "champion:faction:join:42" || buttons[0].Style != discordgo.SuccessButton || buttons[1].Style != discordgo.LinkButton {
		t.Fatalf("open buttons: %+v", buttons)
	}

	f.RecruitmentStatus = factionhub.RecruitmentInviteOnly
	embed, comps = BuildFactionRecruitCard(f, nil, "", "https://championshp.vip")
	buttons = recruitButtons(t, comps)
	if len(buttons) != 2 || buttons[0].CustomID != "champion:faction:apply:42" || buttons[0].Style != discordgo.PrimaryButton || embed.Thumbnail != nil || !strings.Contains(embed.Description, "**Apply**") {
		t.Fatalf("invite-only buttons: %+v %q", buttons, embed.Description)
	}

	f.RecruitmentStatus = factionhub.RecruitmentClosed
	f.PrimaryColor = nil
	embed, comps = BuildFactionRecruitCard(f, nil, "", "https://championshp.vip")
	buttons = recruitButtons(t, comps)
	if len(buttons) != 1 || buttons[0].Style != discordgo.LinkButton || embed.Color == 0xFF1726 {
		t.Fatalf("closed buttons: %+v colour %x", buttons, embed.Color)
	}
	if !IsFactionRecruitInteraction("champion:faction:join:42") || IsFactionRecruitInteraction("champion_reset_confirm") {
		t.Fatal("custom id routing")
	}
	if action, id := splitRecruitCustomID("champion:faction:applymodal:42"); action != "applymodal" || id != "42" {
		t.Fatalf("split: %s %s", action, id)
	}
}
