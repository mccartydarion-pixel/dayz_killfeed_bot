package app

import (
	"strings"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/ownerops"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Every owner-ops message is a card: the headline as the title, the colour by meaning, the
// moment in the timestamp, the Owner Hub as the footer of what the admins get.

func TestOwnerOpsNoticeEmbed(t *testing.T) {
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	n := ownerOpsNotice{Title: "Deploy recovered", Body: "HEALTHY commit=abc", Tone: presentation.NoticeSuccess, Footer: presentation.FooterOwnerHub, At: at}
	e := n.embed()
	if e.Title != "Deploy recovered" || e.Description != "HEALTHY commit=abc" || e.Color != presentation.Green {
		t.Fatalf("card: %+v", e)
	}
	if e.Footer == nil || e.Footer.Text != "Owner Hub" || e.Timestamp != "2026-10-15T08:00:00.000Z" {
		t.Fatalf("footer/timestamp: %+v %q", e.Footer, e.Timestamp)
	}
	if got := n.text(); got != "**Deploy recovered**\nHEALTHY commit=abc" {
		t.Fatalf("the fakes get the text form: %q", got)
	}
	if problems := presentation.CheckEmbed(e); len(problems) > 0 {
		t.Fatalf("design rules: %v", problems)
	}

	// The fake DM and channel post receive the text form; nothing else is called.
	var dm, post string
	a := &App{ownerOpsDM: func(id, text string) error { dm = id + "|" + text; return nil },
		ownerOpsChannelPost: func(channel, text string) error { post = channel + "|" + text; return nil }}
	if err := a.ownerOpsSend("u1", n); err != nil || dm != "u1|**Deploy recovered**\nHEALTHY commit=abc" {
		t.Fatalf("dm = %q, %v", dm, err)
	}
	if err := a.ownerOpsPost("c1", n); err != nil || post != "c1|**Deploy recovered**\nHEALTHY commit=abc" {
		t.Fatalf("post = %q, %v", post, err)
	}
	// Without a fake and without Discord the send fails cleanly.
	if err := (&App{}).ownerOpsSend("u1", n); err == nil {
		t.Fatal("no Discord: an error, not a panic")
	}
}

func TestIncidentTone(t *testing.T) {
	for kind, want := range map[string]presentation.NoticeTone{
		ownerops.KindWorkerDown: presentation.NoticeWarning, ownerops.KindFeedStalled: presentation.NoticeWarning,
		ownerops.KindNitradoAccess: presentation.NoticeCritical, ownerops.KindDiscordAccess: presentation.NoticeCritical,
	} {
		if got := incidentTone(kind); got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
	}
}

func TestCustomerIncidentMessage(t *testing.T) {
	n := customerIncidentMessage(ownerops.KindDiscordAccess, "Ops Server", "https://champions.example/dashboard")
	if n.Title != "Discord access lost" || n.Tone != presentation.NoticeCritical || n.At.IsZero() {
		t.Fatalf("notice: %+v", n)
	}
	if !strings.HasPrefix(n.Body, "The Champion bot is no longer in the Discord server for Ops Server") || !strings.HasSuffix(n.Body, "\nhttps://champions.example/dashboard") {
		t.Fatalf("body: %q", n.Body)
	}
	nitrado := customerIncidentMessage(ownerops.KindNitradoAccess, "Ops Server", "")
	if nitrado.Title != "Nitrado access lost" || !strings.Contains(nitrado.Body, "Reconnect Nitrado") || strings.Contains(nitrado.Body, "https://") {
		t.Fatalf("nitrado: %+v", nitrado)
	}
	other := customerIncidentMessage(ownerops.KindWorkerDown, "Ops Server", "")
	if other.Title != "Champion found a problem" || other.Body != "Champion found a problem with your DayZ server Ops Server." {
		t.Fatalf("other: %+v", other)
	}
}

func TestBriefingNoticeFields(t *testing.T) {
	day := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	b := ownerops.Briefing{Day: day, MRRCents: 1499, Currency: "usd", ActiveSubscribers: 1, TrialOrganizations: 6, PaidCents24h: 1499,
		NewUsers24h: 4, NewOrganizations: 2, StuckOnboarding: 3, LiveServers: 5, BrokenServers: 1, PlayersOnline: 22, Kills24h: 310,
		OpenIncidents: []string{"Worker down: Alpha / Lost City"}, ConfigProblems: 2}
	n := briefingNotice(b, day)
	e := n.embed()
	if e.Title != "Champion briefing · Thu 15 Oct 2026" || e.Description != "" || e.Color != presentation.Amber {
		t.Fatalf("a day with an open incident is amber, the title carries the date: %+v", e)
	}
	if e.Footer == nil || e.Footer.Text != presentation.FooterOwnerHub || e.Timestamp == "" {
		t.Fatalf("footer/timestamp: %+v %q", e.Footer, e.Timestamp)
	}
	names := []string{}
	for _, f := range e.Fields {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, "|"); got != "Revenue|Growth|Fleet|Open incidents (1)|Configuration" {
		t.Fatalf("fields: %s", got)
	}
	if !e.Fields[0].Inline || !e.Fields[1].Inline || !e.Fields[2].Inline || e.Fields[3].Inline {
		t.Fatal("the three numbers are inline, the incidents take a row")
	}
	if e.Fields[0].Value != "$14.99 MRR from 1 paying, 6 on trial. Collected $14.99 in the last 24h." || e.Fields[3].Value != "- Worker down: Alpha / Lost City" {
		t.Fatalf("values: %q / %q", e.Fields[0].Value, e.Fields[3].Value)
	}
	if problems := presentation.CheckEmbed(e); len(problems) > 0 {
		t.Fatalf("design rules: %v", problems)
	}
	// The text form the fakes see starts with the bold headline, so a test can match on the date.
	if !strings.HasPrefix(n.text(), "**Champion briefing · Thu 15 Oct 2026**\n\n**Revenue**\n") {
		t.Fatalf("text form: %q", n.text())
	}
	quiet := briefingNotice(ownerops.Briefing{Day: day, LiveServers: 3}, day)
	if quiet.Tone != presentation.NoticeInfo || len(quiet.Fields) != 4 || quiet.Fields[3].Name != "Incidents" || quiet.Fields[3].Value != "No open incidents." {
		t.Fatalf("quiet day: %+v", quiet)
	}
}

func TestBroadcastNotice(t *testing.T) {
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	for severity, want := range map[string]struct {
		prefix string
		tone   presentation.NoticeTone
	}{
		repository.BroadcastInfo: {"Champion notice", presentation.NoticeInfo}, repository.BroadcastMaintenance: {"Champion maintenance", presentation.NoticeWarning},
		repository.BroadcastIncident: {"Champion incident", presentation.NoticeCritical},
	} {
		n := broadcastNotice(repository.PlatformBroadcast{Title: "Tonight", Body: "Hello @everyone", Severity: severity, CreatedAt: at})
		if n.Title != want.prefix+": Tonight" || n.Body != "Hello @everyone" || n.Tone != want.tone || !n.At.Equal(at) {
			t.Errorf("%s: %+v", severity, n)
		}
		if got := n.text(); got != "**"+want.prefix+": Tonight**\nHello @everyone" {
			t.Errorf("%s text: %q", severity, got)
		}
	}
	// A body past Discord's description limit is cut, never rejected.
	long := broadcastNotice(repository.PlatformBroadcast{Title: "Long", Body: strings.Repeat("x", 5000), Severity: repository.BroadcastInfo}).embed()
	if n := len([]rune(long.Description)); n != presentation.LimitDescription || long.Timestamp == "" {
		t.Fatalf("long body: %d runes, timestamp %q", n, long.Timestamp)
	}
}
