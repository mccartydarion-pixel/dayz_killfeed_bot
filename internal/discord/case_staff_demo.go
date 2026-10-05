package discord

import (
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// BuildCASEStaffDemoEmbed is a fixed, synthetic design preview. It cannot
// accept player data or evidence IDs and is NOT connected to a publisher,
// route, endpoint, command, scheduler, webhook or Discord send operation.
//
// The ADM feed has date-less second-resolution event clocks and selected-event
// positions. No movement finding, score or player accusation is supported.
func BuildCASEStaffDemoEmbed() *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("C.A.S.E. • STAFF PREVIEW — DEMO ONLY", presentation.WarningAmber)
	embed.Description = "**SYNTHETIC PREVIEW — NOT A REAL PLAYER OR DETECTION**\n" +
		"This card demonstrates source-quality reporting only. No player has been flagged, no finding has been created, and nothing has been sent to Discord."
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Installation", Value: "DEMO SERVER (fixture only)", Inline: true},
		{Name: "Detector", Value: "CASE-MOV-001 • v0.1.0", Inline: true},
		{Name: "Detector status", Value: "BLOCKED", Inline: true},
		{Name: "Observed sample", Value: "Synthetic retained-event sample; not full ADM coverage", Inline: false},
		{Name: "Missing prerequisites", Value: "Trusted elapsed gameplay time • validated movement samples • source continuity • tested exception model", Inline: false},
		{Name: "Safe speed pairs", Value: "0", Inline: true},
		{Name: "Enforcement", Value: "DISABLED", Inline: true},
		{Name: "Action", Value: "None — no alert, accusation, case, score, or sanction", Inline: false},
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "CHAMPIONS • C.A.S.E. SYNTHETIC DEMO • NOT LIVE EVIDENCE"}
	// A stable fixture timestamp is visually explicit; never insert runtime
	// timestamps that could make this static design appear to be live telemetry.
	presentation.StampEmbed(embed, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	return embed
}
