package discord

import (
	"strings"
	"testing"
	"time"
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
	if !operationalAdminAlertKind(AlertKindRentPaused) {
		t.Fatal("rent digest must be allowed on the staff alerts route")
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

func TestSecurityGiftMessage(t *testing.T) {
	m := SecurityGiftMessage("Sentinel Pro", "Champions", 7, time.Unix(1_800_000_000, 0), "For the event @here", "https://site.example/store")
	e := m.Embeds[0]
	if !strings.Contains(e.Title, "7 days of Sentinel Pro") || len(e.Fields) != 3 || strings.Contains(e.Fields[1].Value, "@here") || len(m.AllowedMentions.Parse) != 0 {
		t.Fatalf("gift DM: %+v %+v", e, e.Fields)
	}
	if bare := SecurityGiftMessage("Base Raid Alarm", "", 1, time.Now(), "", "").Embeds[0]; len(bare.Fields) != 1 {
		t.Fatalf("bare gift DM: %+v", bare.Fields)
	}
}

func TestBaseRentMessages(t *testing.T) {
	due := time.Unix(1_800_000_000, 0)
	soon := BaseRentNoticeMessage(true, "Hut", "Champions", due, 1500, 7, "https://site.example/store").Embeds[0]
	if !strings.Contains(soon.Title, "is due") || !strings.Contains(soon.Description, "1,500") || !strings.Contains(soon.Description, "Nothing is taken automatically") || len(soon.Fields) != 1 {
		t.Fatalf("due soon: %+v", soon)
	}
	paused := BaseRentNoticeMessage(false, "Hut", "Champions", due, 1500, 7, "").Embeds[0]
	if !strings.Contains(paused.Title, "paused") || !strings.Contains(paused.Description, "your base is kept") || len(paused.Fields) != 0 {
		t.Fatalf("paused: %+v", paused)
	}
	approved := WithRentNotice(BaseRequestDecisionMessage(true, "Hut", "Champions", ""), 1500, 7, 3).Embeds[0]
	if len(approved.Fields) != 1 || !strings.Contains(approved.Fields[0].Value, "every 7 days") || !strings.Contains(approved.Fields[0].Value, "within 3 days") {
		t.Fatalf("approval with rent: %+v", approved.Fields)
	}
	if same := WithRentNotice(BaseRequestDecisionMessage(true, "Hut", "", ""), 0, 7, 3).Embeds[0]; len(same.Fields) != 0 {
		t.Fatal("no rent terms when rent has no price")
	}
}

func TestBaseRentGiftMessage(t *testing.T) {
	m := BaseRentGiftMessage("Hut @everyone", "Champions", 10, time.Unix(1_800_000_000, 0), "Thanks `all`", "https://site.example/store")
	e := m.Embeds[0]
	if !strings.Contains(e.Title, "10 days of free rent") || strings.Contains(e.Title, "@everyone") || len(m.AllowedMentions.Parse) != 0 {
		t.Fatalf("title/mentions: %q", e.Title)
	}
	if len(e.Fields) != 3 || strings.Contains(e.Fields[1].Value, "`") {
		t.Fatalf("fields: %+v", e.Fields)
	}
}

func TestBaseRentPaidForYouMessage(t *testing.T) {
	m := BaseRentPaidForYouMessage("Mate", "Clan @here Hall", "Champions", 7, time.Unix(1_800_000_000, 0))
	e := m.Embeds[0]
	if !strings.Contains(e.Title, "Mate paid rent") || strings.Contains(e.Title, "@here") || !strings.Contains(e.Description, "7 more days") {
		t.Fatalf("message: %q %q", e.Title, e.Description)
	}
}
