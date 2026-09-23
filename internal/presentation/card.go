package presentation

import (
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// Discord embed limits (characters). Discord counts characters; runes are the
// safe approximation used throughout.
const (
	LimitTitle       = 256
	LimitDescription = 4096
	LimitFields      = 25
	LimitFieldName   = 256
	LimitFieldValue  = 1024
	LimitFooter      = 2048
	LimitAuthor      = 256
	LimitTotal       = 6000
)

// MetricField is a labelled value. Empty values produce nil so callers can
// append optional metrics without placeholder cards ("N/A", "Unknown").
func MetricField(name, value string, inline bool) *discordgo.MessageEmbedField {
	if value == "" {
		return nil
	}
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
}

// AppendFields appends the non-nil fields.
func AppendFields(e *discordgo.MessageEmbed, fields ...*discordgo.MessageEmbedField) {
	for _, f := range fields {
		if f != nil {
			e.Fields = append(e.Fields, f)
		}
	}
}

// EmbedLength is Discord's combined-size measure: title, description, field
// names and values, footer text and author name.
func EmbedLength(e *discordgo.MessageEmbed) int {
	if e == nil {
		return 0
	}
	n := utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
	for _, f := range e.Fields {
		if f != nil {
			n += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
	}
	if e.Footer != nil {
		n += utf8.RuneCountInString(e.Footer.Text)
	}
	if e.Author != nil {
		n += utf8.RuneCountInString(e.Author.Name)
	}
	return n
}

// FitEmbed defensively enforces every Discord embed limit in place, so a
// pathological name or stored string can never make Discord reject a card.
// Well-formed cards are untouched. Returns e for chaining.
func FitEmbed(e *discordgo.MessageEmbed) *discordgo.MessageEmbed {
	if e == nil {
		return nil
	}
	e.Title = Truncate(e.Title, LimitTitle)
	e.Description = Truncate(e.Description, LimitDescription)
	if e.Author != nil {
		e.Author.Name = Truncate(e.Author.Name, LimitAuthor)
	}
	if e.Footer != nil {
		e.Footer.Text = Truncate(e.Footer.Text, LimitFooter)
	}
	kept := e.Fields[:0]
	for _, f := range e.Fields {
		if f == nil {
			continue
		}
		if f.Name == "" {
			f.Name = "\u200b"
		}
		if f.Value == "" {
			f.Value = "\u200b"
		}
		f.Name = Truncate(f.Name, LimitFieldName)
		f.Value = Truncate(f.Value, LimitFieldValue)
		kept = append(kept, f)
	}
	if len(kept) > LimitFields {
		kept = kept[:LimitFields]
	}
	e.Fields = kept
	// Over the combined budget: drop trailing (lowest-priority) fields first,
	// then shorten the description.
	for EmbedLength(e) > LimitTotal && len(e.Fields) > 0 {
		e.Fields = e.Fields[:len(e.Fields)-1]
	}
	if over := EmbedLength(e) - LimitTotal; over > 0 {
		keep := utf8.RuneCountInString(e.Description) - over
		e.Description = Truncate(e.Description, keep)
	}
	return e
}
