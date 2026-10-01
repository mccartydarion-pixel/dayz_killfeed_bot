package discord

import (
	"strings"
	"testing"
)

func TestBaseRequestDecisionMessage(t *testing.T) {
	ok := BaseRequestDecisionMessage(true, "Hill@top", "Champions", "")
	if e := ok.Embeds[0]; !strings.Contains(e.Title, "is registered") || len(e.Fields) != 0 || len(ok.AllowedMentions.Parse) != 0 {
		t.Fatalf("approved: %+v", e)
	}
	no := BaseRequestDecisionMessage(false, "Shack", "Champions", "Inside the safe zone @everyone")
	e := no.Embeds[0]
	if !strings.Contains(e.Title, "wasn't approved") || len(e.Fields) != 1 || strings.Contains(e.Fields[0].Value, "@everyone") {
		t.Fatalf("declined: %+v %+v", e, e.Fields)
	}
	if blank := BaseRequestDecisionMessage(false, "", "", "").Embeds[0]; len(blank.Fields) != 0 || !strings.Contains(blank.Description, "your server") {
		t.Fatalf("blank: %+v", blank)
	}
}

func TestNewBaseRequestNotices(t *testing.T) {
	dm := NewBaseRequestMessage("Builder @everyone", "Hilltop", "Champions", 50, "https://site.example/dashboard/anti-cheat?tab=bases")
	e := dm.Embeds[0]
	if !strings.Contains(e.Title, "Champions") || strings.Contains(e.Description, "@everyone") || len(e.Fields) != 2 || len(dm.AllowedMentions.Parse) != 0 {
		t.Fatalf("owner DM: %+v", e)
	}
	if noLink := NewBaseRequestMessage("", "", "", 25, "").Embeds[0]; len(noLink.Fields) != 1 || noLink.Footer == nil {
		t.Fatalf("no link: %+v", noLink)
	}
	if !operationalAdminAlertKind(AlertKindBaseRequest) {
		t.Fatal("base requests must be allowed on ADMIN_ALERTS")
	}
	notice := BuildAdminAlertEmbed(AdminAlert{Kind: AlertKindBaseRequest, Severity: AlertInfo, Headline: "NEW BASE REQUEST", Detail: "x"}, "Champions")
	if notice.Title != "📍 STAFF NOTICE" {
		t.Fatalf("notice title: %q", notice.Title)
	}
	for _, f := range notice.Fields {
		if f.Name == "Severity" {
			t.Fatal("a staff notice must not show a severity")
		}
	}
}
