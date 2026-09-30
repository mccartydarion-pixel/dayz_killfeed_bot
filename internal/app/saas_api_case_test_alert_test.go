package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
)

func caseAlertGuild(categoryPrivate, categoryPublicOverride, channelPublicOverride bool) *designerDiscordFake {
	return &designerDiscordFake{channels: []discord.RawGuildChannel{
		{ID: "cat", Name: "🔒 CHAMPION • C.A.S.E.", Type: discordgo.ChannelTypeGuildCategory, Private: categoryPrivate, PublicViewOverride: categoryPublicOverride},
		{ID: "alerts", Name: "🚨・case-alerts", Type: discordgo.ChannelTypeGuildText, ParentID: "cat", PublicViewOverride: channelPublicOverride},
	}}
}

func TestCaseTestAlertSendsOnlyToPrivateStaffChannel(t *testing.T) {
	at := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	routes := &designerRoutesFake{routes: map[string]string{caseAlertsRoute: "alerts"}}
	d := caseAlertGuild(true, false, false)
	resp, derr := sendCaseTestAlert(context.Background(), routes, d, 1, 2, "guild", "Your server", at)
	if derr != nil || !resp.Sent || resp.ChannelID != "alerts" || len(d.sent) != 1 {
		t.Fatalf("private channel send: %+v %v", resp, derr)
	}
	msg := d.sent[0].msg
	if !strings.Contains(msg.Content, "Test alert") || !strings.Contains(msg.Content, "made up") ||
		len(msg.Embeds) != 1 || !strings.HasPrefix(msg.Embeds[0].Title, "🧪 TEST · ") || !strings.HasPrefix(msg.Embeds[0].Footer.Text, "TEST · sample data") {
		t.Fatalf("test card not clearly marked: %+v", msg)
	}
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 || msg.AllowedMentions.RepliedUser {
		t.Fatalf("mentions not disabled: %+v", msg.AllowedMentions)
	}

	for name, guild := range map[string]*designerDiscordFake{
		"public category":          caseAlertGuild(false, false, false),
		"category allows everyone": caseAlertGuild(true, true, false),
		"channel allows everyone":  caseAlertGuild(true, false, true),
	} {
		_, derr := sendCaseTestAlert(context.Background(), routes, guild, 1, 2, "guild", "Your server", at)
		if derr == nil || derr.code != codeEmbedSendForbidden || len(guild.sent) != 0 {
			t.Fatalf("%s: sent to a public channel: %v", name, derr)
		}
	}

	none := caseAlertGuild(true, false, false)
	_, derr = sendCaseTestAlert(context.Background(), &designerRoutesFake{routes: map[string]string{}}, none, 1, 2, "guild", "", at)
	if derr == nil || derr.code != codeEmbedRouteNotConfigured || !strings.Contains(derr.message, "Repair") || len(none.sent) != 0 {
		t.Fatalf("missing route: %v", derr)
	}

	noPerm := caseAlertGuild(true, false, false)
	noPerm.missing = []string{"Send Messages"}
	if _, derr := sendCaseTestAlert(context.Background(), routes, noPerm, 1, 2, "guild", "", at); derr == nil || len(noPerm.sent) != 0 {
		t.Fatalf("sent without permission: %v", derr)
	}
}
