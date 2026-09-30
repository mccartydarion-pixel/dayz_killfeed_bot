package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/caseintel"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// caseDetectorCard is the plain-English name and icon staff see for each
// detector. It matches the website's anti-cheat pages.
var caseDetectorCard = map[string]struct{ icon, name string }{
	"CASE-LOGIN-001":    {"🔑", "Suspicious Logins"},
	"CASE-TELEPORT-001": {"⚡", "Teleporting"},
	"CASE-BASE-001":     {"🏠", "Base Boosting"},
	"CASE-SKYWALK-001":  {"☁️", "Skywalking"},
	"CASE-UNDERMAP-001": {"⛏️", "Under the Map"},
	"CASE-NOCLIP-001":   {"🧱", "Walking Through Walls"},
	"CASE-DUPE-001":     {"📦", "Item Duplication"},
	"CASE-PC-XBOX-001":  {"🖥️", "PC Player on Xbox"},
}

// caseRuledOut turns the detector's exclusion codes into what staff care
// about: the normal explanations that were already checked and ruled out.
var caseRuledOut = map[string]string{
	"RESPAWN":                      "respawning",
	"SERVER_RESTART":               "a server restart",
	"VEHICLE":                      "driving or flying",
	"TELEPORT_EXEMPT_ZONE":         "admin or scripted teleports",
	"SAMPLE_GAP":                   "gaps in the log",
	"STRUCTURE_ZONE":               "towers and buildings",
	"UNDERGROUND_ZONE":             "bunkers and tunnels",
	"SINGLE_SAMPLE":                "a one-off glitch",
	"LEGITIMATE_ENTRANCE":          "doors and gaps",
	"SPARSE_SAMPLES":               "missing movement data",
	"BASE_OWNER":                   "the base owner",
	"AUTHORIZED_PLAYER":            "players on the friend list",
	"AUTHORIZED_FACTION":           "allowed factions",
	"REGISTERED_AT_EVENT_TIME":     "bases registered after the fact",
	"ITEM_RELEASE":                 "the item being dropped",
	"ITEM_PROVENANCE":              "unverified item records",
	"RECONNECT_OR_RESTART_CONTEXT": "unrelated item pickups",
	"ATTESTATION_SIGNATURE":        "unsigned platform data",
	"ATTESTATION_SOURCE":           "unknown platform sources",
	"ORDINARY_RECONNECT":           "a few normal reconnects",
}

// BuildCASECore8StaffEmbed renders one Core Eight finding for a private staff
// channel, in plain English. It is a layout only: it is NOT connected to a
// publisher, route, scheduler or send path. Delivery must go through the
// existing casealert eligibility and caseoutbox idempotency (keyed by
// IncidentKey), and only to a private staff route resolved by /setup.
//
// reviewURL, when it is an https link, makes the title open the player's page
// on the dashboard.
//
// It returns nil for a finding without an incident key or evidence, so an
// unvalidated result cannot be rendered as an alert.
func BuildCASECore8StaffEmbed(f caseintel.Finding, detectorName, serverName, reviewURL string) *discordgo.MessageEmbed {
	if f.IncidentKey == "" || len(f.EvidenceIDs) == 0 || strings.TrimSpace(detectorName) == "" {
		return nil
	}
	icon, name := "🛡️", caseSafeText(detectorName, 64)
	if c, ok := caseDetectorCard[f.DetectorID]; ok {
		icon, name = c.icon, c.name
	}
	color, headline, status := presentation.InfoSteel, "noticed", "👀 **Noticed.** Recorded for your records. No action needed yet."
	switch f.Tier {
	case caseintel.TierSuspicious:
		color, headline = presentation.WarningAmber, "needs a staff look"
		status = "⚠️ **Needs a staff look.** This is not proof of cheating. Check the evidence and decide."
	case caseintel.TierStaffConfirmed:
		color, headline = presentation.ErrorRed, "confirmed by staff"
		status = "✅ **Confirmed by staff** (" + caseSafeText(f.ConfirmedBy, 64) + "). Any action is up to your team."
	}
	embed := &discordgo.MessageEmbed{
		Author:      &discordgo.MessageEmbedAuthor{Name: "CHAMPIONS® C.A.S.E. · ANTI-CHEAT"},
		Title:       icon + " " + name + " — " + headline,
		Color:       color,
		Description: "**" + caseFallback(caseSafeText(f.Behavior, 200), "Something unusual was spotted") + "**\n" + caseSafeText(f.Explanation, 900),
	}
	if strings.HasPrefix(reviewURL, "https://") && !strings.ContainsAny(reviewURL, " \n") {
		embed.URL = reviewURL
	}
	player := caseSafeText(f.PlayerName, 64)
	if player == "" {
		player = fmt.Sprintf("Player #%d", f.PlayerID)
	}
	when := "Not recorded"
	if !f.EventAt.IsZero() {
		when = fmt.Sprintf("<t:%d:f> (<t:%d:R>)", f.EventAt.Unix(), f.EventAt.Unix())
	}
	evidence := fmt.Sprintf("%d record%s", len(f.EvidenceIDs), casePlural(len(f.EvidenceIDs)))
	if f.EvidenceCompleteness == "COMPLETE" {
		evidence += " · complete"
	} else if len(f.MissingEvidence) > 0 || f.EvidenceCompleteness == "PARTIAL" {
		evidence += " · some records missing"
	}
	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "👤 Player", Value: player, Inline: true},
		{Name: "🖥️ Server", Value: caseFallback(caseSafeText(serverName, 100), fmt.Sprintf("Server %d", f.Scope.ServerID)), Inline: true},
		{Name: "🕒 When", Value: when, Inline: true},
		{Name: "📋 Evidence", Value: evidence, Inline: true},
	}
	if ruled := caseRuledOutText(f.ExclusionsChecked); ruled != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "✔️ Already ruled out", Value: ruled, Inline: false})
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "What this means", Value: status, Inline: false})
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "Staff only · No automatic bans · Ref " + f.IncidentKey[:8]}
	presentation.StampEmbed(embed, f.ObservedAt)
	return embed
}

func caseRuledOutText(codes []string) string {
	seen := map[string]bool{}
	var out []string
	for _, c := range codes {
		if label, ok := caseRuledOut[c]; ok && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return caseSafeText(strings.ToUpper(out[0][:1])+out[0][1:]+strings.Join(append([]string{""}, out[1:]...), ", "), 300)
}

func casePlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
