package presentation

import (
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// A notice is an operational message - a briefing, an incident, a deploy verdict, a platform
// broadcast, "your link is verified" - that used to go out as plain text. Every message the
// bot sends is a card, so a notice is one too: the brand in the author line, the message's
// first line as the title, the rest as the description, a palette colour chosen by what the
// notice MEANS, and the moment it was written as the embed timestamp.

// NoticeTone is what a notice means; it decides the colour.
type NoticeTone int

const (
	NoticeInfo     NoticeTone = iota // routine: a briefing, a notice, a heads-up
	NoticeSuccess                    // fixed, recovered, done
	NoticeWarning                    // needs attention soon
	NoticeCritical                   // broken, failed, needs someone now
)

// Color is the palette colour of a tone.
func (t NoticeTone) Color() int {
	switch t {
	case NoticeSuccess:
		return Green
	case NoticeWarning:
		return Amber
	case NoticeCritical:
		return Red
	}
	return Neutral
}

// String names a tone for logs and tests.
func (t NoticeTone) String() string {
	switch t {
	case NoticeSuccess:
		return "success"
	case NoticeWarning:
		return "warning"
	case NoticeCritical:
		return "critical"
	}
	return "info"
}

// FooterOwnerHub is the footer context of a notice to the platform owner and admins.
const FooterOwnerHub = "Owner Hub"

// NoticeEmbed is a notice as a card. The title is the message's headline (sentence case, at
// most one leading emoji, as every title), the body its text; both are trimmed and cut to
// Discord's limits without splitting a rune. The footer is the slogan; a caller with a better
// context (FooterOwnerHub, FooterStaffOnly) replaces it. A zero time leaves the timestamp out.
func NoticeEmbed(title, body string, tone NoticeTone, at time.Time) *discordgo.MessageEmbed {
	e := NewChampionEmbed(strings.TrimSpace(title), tone.Color())
	e.Description = strings.TrimSpace(body)
	e.Footer = ChampionFooter()
	StampEmbed(e, at)
	return FitEmbed(e)
}

// NoticeMessage wraps one or more notice cards as a message with every mention disabled: a
// notice can quote stored names and owner-written text, and none of it may ping anyone.
func NoticeMessage(embeds ...*discordgo.MessageEmbed) *discordgo.MessageSend {
	kept := make([]*discordgo.MessageEmbed, 0, len(embeds))
	for _, e := range embeds {
		if e != nil {
			kept = append(kept, FitEmbed(e))
		}
	}
	return &discordgo.MessageSend{Embeds: kept, AllowedMentions: &discordgo.MessageAllowedMentions{}}
}

// NoticeText is the notice as the plain text it replaced: the bold headline, the body, then
// each field as a bold label over its value. Logs and test fakes use it; Discord never does.
func NoticeText(e *discordgo.MessageEmbed) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	if e.Title != "" {
		b.WriteString("**" + e.Title + "**")
	}
	if e.Description != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(e.Description)
	}
	for _, f := range e.Fields {
		if f == nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("**" + f.Name + "**\n" + f.Value)
	}
	return b.String()
}
