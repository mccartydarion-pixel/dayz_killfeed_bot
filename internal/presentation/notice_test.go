package presentation

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNoticeToneColours(t *testing.T) {
	for tone, want := range map[NoticeTone]int{NoticeInfo: Neutral, NoticeSuccess: Green, NoticeWarning: Amber, NoticeCritical: Red} {
		if got := tone.Color(); got != want {
			t.Errorf("%s tone: colour #%06X, want #%06X", tone, got, want)
		}
		if e := NoticeEmbed("Title", "body", tone, time.Time{}); e.Color != want || !InPalette(e.Color) {
			t.Errorf("%s notice: colour #%06X, want #%06X", tone, e.Color, want)
		}
	}
	if NoticeTone(99).Color() != Neutral {
		t.Fatal("an unknown tone reads as information")
	}
}

func TestNoticeEmbedShape(t *testing.T) {
	at := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	e := NoticeEmbed("  Deploy recovered  ", "\nHEALTHY commit=abc\n", NoticeSuccess, at)
	if e.Author == nil || e.Author.Name != AuthorName {
		t.Fatalf("the brand belongs in the author line: %#v", e.Author)
	}
	if e.Title != "Deploy recovered" || e.Description != "HEALTHY commit=abc" {
		t.Fatalf("title/body are trimmed: %q / %q", e.Title, e.Description)
	}
	if e.Timestamp != "2026-10-15T09:30:00.000Z" {
		t.Fatalf("the moment goes in the embed timestamp: %q", e.Timestamp)
	}
	if e.Footer == nil || e.Footer.Text != ChampionSlogan {
		t.Fatalf("a notice has the standard footer until a caller names a better context: %#v", e.Footer)
	}
	if problems := CheckEmbed(e); len(problems) > 0 {
		t.Fatalf("a notice follows the design rules: %v", problems)
	}
	if zero := NoticeEmbed("x", "y", NoticeInfo, time.Time{}); zero.Timestamp != "" {
		t.Fatal("a zero time leaves the timestamp unset")
	}
}

func TestNoticeEmbedTruncates(t *testing.T) {
	long := strings.Repeat("é", LimitDescription+500)
	e := NoticeEmbed(strings.Repeat("T", LimitTitle+10), long, NoticeWarning, time.Time{})
	if n := utf8.RuneCountInString(e.Description); n != LimitDescription || !strings.HasSuffix(e.Description, "…") || !utf8.ValidString(e.Description) {
		t.Fatalf("description is cut to %d runes without splitting one: %d runes", LimitDescription, n)
	}
	if n := utf8.RuneCountInString(e.Title); n != LimitTitle {
		t.Fatalf("title is cut to %d runes: %d", LimitTitle, n)
	}
	if EmbedLength(e) > LimitTotal {
		t.Fatalf("over the combined budget: %d", EmbedLength(e))
	}
}

func TestNoticeMessageDisablesMentions(t *testing.T) {
	msg := NoticeMessage(NoticeEmbed("Champion incident: outage", "Hello @everyone <@123>", NoticeCritical, time.Time{}), nil)
	if msg.Content != "" || len(msg.Embeds) != 1 {
		t.Fatalf("a notice is one embed and no text: %+v", msg)
	}
	if msg.AllowedMentions == nil || len(msg.AllowedMentions.Parse) != 0 || len(msg.AllowedMentions.Users) != 0 || len(msg.AllowedMentions.Roles) != 0 {
		t.Fatalf("every mention must be disabled: %+v", msg.AllowedMentions)
	}
}

func TestNoticeText(t *testing.T) {
	e := NoticeEmbed("Champion briefing · Thu 15 Oct 2026", "", NoticeInfo, time.Time{})
	AppendFields(e, StatusField("Revenue", "$14.99 MRR", true), StatusField("Fleet", "5 live", true))
	if got, want := NoticeText(e), "**Champion briefing · Thu 15 Oct 2026**\n\n**Revenue**\n$14.99 MRR\n\n**Fleet**\n5 live"; got != want {
		t.Fatalf("text form:\n%q\nwant\n%q", got, want)
	}
	if got := NoticeText(NoticeEmbed("Deploy recovered", "line", NoticeSuccess, time.Time{})); got != "**Deploy recovered**\nline" {
		t.Fatalf("text form: %q", got)
	}
	if NoticeText(nil) != "" {
		t.Fatal("nil embed has no text")
	}
}
