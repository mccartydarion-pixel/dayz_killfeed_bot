package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BuildCASECore8StaffEmbed renders one Core Eight finding for a private staff
// channel. It is a layout contract only: it is NOT connected to a publisher,
// route, scheduler or send path. Delivery must go through the existing
// casealert eligibility and caseoutbox idempotency (keyed by IncidentKey),
// and only to a private staff route resolved by /setup.
//
// It returns nil for a finding without an incident key or evidence, so an
// unvalidated result cannot be rendered as an alert.
func BuildCASECore8StaffEmbed(f caseintel.Finding, detectorName, serverName string) *discordgo.MessageEmbed {
	if f.IncidentKey == "" || len(f.EvidenceIDs) == 0 || strings.TrimSpace(detectorName) == "" {
		return nil
	}
	color, status := presentation.WarningAmber, "Observed activity — not a confirmed violation"
	switch f.Tier {
	case caseintel.TierSuspicious:
		status = "Suspicious — pending staff review. Not a confirmed violation."
	case caseintel.TierStaffConfirmed:
		status = "Staff-confirmed violation (" + caseSafeText(f.ConfirmedBy, 64) + ")"
	}
	embed := presentation.NewChampionEmbed("CHAMPIONS® C.A.S.E. • "+caseSafeText(detectorName, 64), color)
	player := caseSafeText(f.PlayerName, 64)
	if player == "" {
		player = "Unnamed player"
	}
	eventTime := "Not recorded"
	if !f.EventAt.IsZero() {
		eventTime = fmt.Sprintf("<t:%d:F>", f.EventAt.Unix())
	}
	evidence := fmt.Sprintf("%d retained evidence record(s) • %s", len(f.EvidenceIDs), f.EvidenceCompleteness)
	if len(f.MissingEvidence) > 0 {
		evidence += "\nMissing: " + caseSafeText(strings.Join(f.MissingEvidence, ", "), 300)
	}
	if len(f.ExclusionsChecked) > 0 {
		evidence += "\nExclusions checked: " + caseSafeText(strings.Join(f.ExclusionsChecked, ", "), 300)
	}
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Detection type", Value: caseSafeText(detectorName, 100) + " • " + f.DetectorID, Inline: true},
		{Name: "Player", Value: fmt.Sprintf("%s (ID %d)", player, f.PlayerID), Inline: true},
		{Name: "Affected server", Value: caseFallback(caseSafeText(serverName, 100), fmt.Sprintf("Server %d", f.Scope.ServerID)), Inline: true},
		{Name: "Observed behavior", Value: caseFallback(caseSafeText(f.Behavior, 200), "Not described"), Inline: false},
		{Name: "Explanation", Value: caseFallback(caseSafeText(f.Explanation, 1000), "Not described"), Inline: false},
		{Name: "Supporting evidence", Value: evidence, Inline: false},
		{Name: "Event time", Value: eventTime, Inline: true},
		{Name: "Investigation status", Value: status, Inline: true},
		{Name: "Case reference", Value: "`" + f.IncidentKey[:16] + "`", Inline: true},
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "CHAMPIONS • C.A.S.E. • STAFF ONLY • NO AUTOMATIC ENFORCEMENT"}
	presentation.StampEmbed(embed, f.ObservedAt)
	return embed
}

// caseSafeText trims, bounds and neutralises mentions and code fences in
// player-controlled strings.
func caseSafeText(s string, max int) string {
	s = strings.TrimSpace(strings.NewReplacer("@", "@​", "`", "'", "\n", " ").Replace(s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

func caseFallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
