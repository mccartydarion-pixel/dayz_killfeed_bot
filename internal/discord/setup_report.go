package discord

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// /setup and /setup repair report. A guild can be connected to several installations (Champions has
// 15 from repeated onboarding), and every installation's layout resolves to the SAME guild channels.
// The report is therefore keyed by Discord channel ID: each channel appears once per run with one
// outcome, whatever number of installations touched it. It is built only after every installation
// has been processed, and is rendered as one bounded embed.

// SetupChannelOutcome is what one run did to one Discord channel.
type SetupChannelOutcome string

const (
	SetupChannelCreated SetupChannelOutcome = "CREATED" // did not exist; Champion created it
	SetupChannelReused  SetupChannelOutcome = "REUSED"  // already correct; nothing changed
	SetupChannelUpdated SetupChannelOutcome = "UPDATED" // kept, and at least one route was moved to it
	SetupChannelFailed  SetupChannelOutcome = "FAILED"  // exists but failed verification (permissions, content)
)

// outcomeRank: when installations disagree about one channel, the most significant outcome wins.
var outcomeRank = map[SetupChannelOutcome]int{SetupChannelReused: 0, SetupChannelUpdated: 1, SetupChannelCreated: 2, SetupChannelFailed: 3}

// SetupChannel is one Discord channel in the report.
type SetupChannel struct {
	ID      string
	Name    string
	System  string // e.g. "Combat Feed"
	Outcome SetupChannelOutcome
}

// SetupLayoutResult summarizes one /setup run of the Channel System V2 layout engine across the
// guild's installations. Build it with AddChannel/AddSystem/AddLegacy so every channel and system
// is counted exactly once.
type SetupLayoutResult struct {
	Installations int

	channels    []SetupChannel
	channelIdx  map[string]int
	verified    []string
	verifiedSet map[string]bool
	blocked     []string
	blockedSet  map[string]bool
	failed      []string // systems with no usable channel (no ID to dedupe on)
	failedSet   map[string]bool
	legacy      map[string]bool
}

// AddChannel records a channel's outcome, deduplicated by channel ID.
func (r *SetupLayoutResult) AddChannel(c SetupChannel) {
	if c.ID == "" {
		return
	}
	if r.channelIdx == nil {
		r.channelIdx = map[string]int{}
	}
	if i, ok := r.channelIdx[c.ID]; ok {
		cur := &r.channels[i]
		if outcomeRank[c.Outcome] > outcomeRank[cur.Outcome] {
			cur.Outcome = c.Outcome
		}
		if cur.Name == "" {
			cur.Name = c.Name
		}
		return
	}
	r.channelIdx[c.ID] = len(r.channels)
	r.channels = append(r.channels, c)
}

func addUnique(list *[]string, set *map[string]bool, v string) {
	if v == "" {
		return
	}
	if *set == nil {
		*set = map[string]bool{}
	}
	if (*set)[v] {
		return
	}
	(*set)[v] = true
	*list = append(*list, v)
}

// AddVerifiedSystem records a system whose channel passed verification.
func (r *SetupLayoutResult) AddVerifiedSystem(label string) {
	addUnique(&r.verified, &r.verifiedSet, label)
}

// AddBlockedSystem records a system with no producer yet (no channel needed).
func (r *SetupLayoutResult) AddBlockedSystem(label string) {
	addUnique(&r.blocked, &r.blockedSet, label)
}

// AddFailedSystem records a system that could not be given a working channel.
func (r *SetupLayoutResult) AddFailedSystem(label string) { addUnique(&r.failed, &r.failedSet, label) }

// AddLegacy records an unused Champion channel, deduplicated by channel ID. Never deleted.
func (r *SetupLayoutResult) AddLegacy(channelID string) {
	if channelID == "" {
		return
	}
	if r.legacy == nil {
		r.legacy = map[string]bool{}
	}
	r.legacy[channelID] = true
}

// Channels returns each channel once, in first-seen (layout) order.
func (r SetupLayoutResult) Channels() []SetupChannel {
	return append([]SetupChannel(nil), r.channels...)
}

// Count returns how many distinct channels had the given outcome.
func (r SetupLayoutResult) Count(o SetupChannelOutcome) int {
	n := 0
	for _, c := range r.channels {
		if c.Outcome == o {
			n++
		}
	}
	return n
}

func (r SetupLayoutResult) VerifiedSystems() []string { return append([]string(nil), r.verified...) }
func (r SetupLayoutResult) BlockedSystems() []string  { return append([]string(nil), r.blocked...) }
func (r SetupLayoutResult) LegacyChannels() int       { return len(r.legacy) }

// verifiedNotFailed drops from the verified list any system that failed for another installation
// in the same run: a system is never reported as both.
func (r SetupLayoutResult) verifiedNotFailed() []string {
	failed := map[string]bool{}
	for _, f := range r.FailedSystems() {
		failed[f] = true
	}
	var out []string
	for _, v := range r.verified {
		if !failed[v] {
			out = append(out, v)
		}
	}
	return out
}

// FailedSystems lists systems that failed, from channels that failed verification and systems
// with no working channel, each once.
func (r SetupLayoutResult) FailedSystems() []string {
	var out []string
	var set map[string]bool
	for _, c := range r.channels {
		if c.Outcome == SetupChannelFailed {
			addUnique(&out, &set, c.System)
		}
	}
	for _, s := range r.failed {
		addUnique(&out, &set, s)
	}
	return out
}

// Discord embed limits (https://discord.com/developers/docs/resources/message#embed-object-embed-limits).
const (
	embedDescriptionLimit = 4096
	embedFieldValueLimit  = 1024
	embedTotalLimit       = 6000
)

// bulletList renders items as a bulleted list bounded by limit; anything beyond is summarized as a
// count, never cut mid-line.
func bulletList(items []string, limit int) string {
	var b strings.Builder
	for i, it := range items {
		line := "• " + it + "\n"
		if b.Len()+len(line) > limit-24 {
			fmt.Fprintf(&b, "…and %d more", len(items)-i)
			break
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SetupLayoutEmbed renders the /setup or /setup repair report as one Champions-branded embed.
func SetupLayoutEmbed(r SetupLayoutResult, repair bool) *discordgo.MessageEmbed {
	title := "🏆 CHAMPIONS® DISCORD SETUP"
	if repair {
		title = "🔧 CHAMPIONS® DISCORD REPAIR"
	}
	created, reused, updated := r.Count(SetupChannelCreated), r.Count(SetupChannelReused), r.Count(SetupChannelUpdated)
	failedSystems := r.FailedSystems()
	failed := len(failedSystems)

	status, color, result := "Repair Complete", presentation.ChampionGold, "All required systems are configured."
	if !repair {
		status = "Setup Complete"
	}
	if failed > 0 {
		status, color, result = "Needs Attention", presentation.WarningAmber, fmt.Sprintf("%d system(s) need attention. Check Champion's channel permissions, then run `/setup repair` again.", failed)
	}

	fields := []*discordgo.MessageEmbedField{{
		Name:  "Channel Summary",
		Value: fmt.Sprintf("• Created: %d\n• Reused: %d\n• Updated: %d\n• Failed: %d", created, reused, updated, failed),
	}}
	if v := r.verifiedNotFailed(); len(v) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Systems Verified", Value: bulletList(v, embedFieldValueLimit)})
	}
	if failed > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Needs Attention", Value: bulletList(failedSystems, embedFieldValueLimit)})
	}
	if b := r.BlockedSystems(); len(b) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Not Available Yet", Value: bulletList(b, embedFieldValueLimit)})
	}
	if n := r.LegacyChannels(); n > 0 {
		noun := "channel"
		if n != 1 {
			noun = "channels"
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Legacy Channels",
			Value: fmt.Sprintf("%d unused Champion %s detected.\nReview obsolete channels under **Website → Setup → Discord Channels**.", n, noun)})
	}
	fields = append(fields, &discordgo.MessageEmbedField{Name: "Result", Value: result})
	return &discordgo.MessageEmbed{
		Author:      presentation.ChampionAuthor(),
		Title:       title,
		Description: "**Status:** " + status,
		Color:       color,
		Fields:      fields,
		Footer:      &discordgo.MessageEmbedFooter{Text: "CHAMPIONS® • Discord Setup"},
	}
}

// setupErrorMessage is the plain reply for a run that could not start.
func setupErrorMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoInstallation):
		return "ℹ️ This Discord server is not connected to Champion yet.\nConnect it in **Setup** on the Champion website, then run `/setup` again."
	case errors.Is(err, ErrMissingManageChannels):
		return "❌ Champion needs the **Manage Channels** permission to set up its channels. Grant it and run `/setup repair`."
	}
	return "❌ Setup failed. Try `/setup repair` again in a moment."
}
