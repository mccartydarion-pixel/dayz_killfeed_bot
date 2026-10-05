package discord

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// HotZoneAnnouncement is what the "hot zone opened" card says (docs/HOT_ZONES.md).
type HotZoneAnnouncement struct {
	Name          string
	ServerName    string
	CenterX       float64
	CenterZ       float64
	RadiusM       float64
	KillsObserved int
	WindowMinutes int
	EndsAt        *time.Time
	FirstPoints   int
	SecondPoints  int
	ThirdPoints   int
}

// BuildHotZoneOpenedEmbed announces a hot zone: where it is, how long it lasts and what it pays.
func BuildHotZoneOpenedEmbed(a HotZoneAnnouncement) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("🔥 Hot zone open", presentation.CombatRed)
	desc := "**" + presentation.SafeName(a.Name, 60) + "**" // the server is named in the footer
	if a.KillsObserved > 0 && a.WindowMinutes > 0 {
		desc += fmt.Sprintf("\n%s there in the last %s.", presentation.Plural(int64(a.KillsObserved), "kill", "kills"), presentation.Plural(int64(a.WindowMinutes), "minute", "minutes"))
	}
	embed.Description = desc
	presentation.AppendFields(embed,
		presentation.MetricField("Where", fmt.Sprintf("Within %s of %.0f / %.0f", presentation.FormatWholeDistance(a.RadiusM), a.CenterX, a.CenterZ), true))
	if a.EndsAt != nil {
		presentation.AppendFields(embed, presentation.MetricField("Ends", presentation.Timestamp(*a.EndsAt, 'R'), true))
	}
	var prizes []string
	for i, pts := range []int{a.FirstPoints, a.SecondPoints, a.ThirdPoints} {
		if pts > 0 {
			prizes = append(prizes, fmt.Sprintf("%s %s", []string{"🥇", "🥈", "🥉"}[i], presentation.FormatPoints(int64(pts))))
		}
	}
	presentation.AppendFields(embed, presentation.MetricField("Prizes", strings.Join(prizes, " • "), false))
	presentation.AppendFields(embed, presentation.MetricField("How it scores", "One point per kill on a player inside the zone.", false))
	embed.Footer = presentation.Footer(a.ServerName, "Hot zone")
	return presentation.FitEmbed(embed)
}

// Announce posts an embed where completion cards go (the SERVER_STATUS route, else the legacy
// status/leaderboard channel).
func (p *LiveCompletionPublisher) Announce(ctx context.Context, embed *discordgo.MessageEmbed) error {
	if p == nil {
		return fmt.Errorf("announcement publisher unavailable")
	}
	return p.send(ctx, embed)
}

// AnnounceEvent posts an event's "upcoming" or "started" card to the EVENTS channel, falling back
// to where Announce posts when the installation has no events channel yet.
func (p *LiveCompletionPublisher) AnnounceEvent(ctx context.Context, embed *discordgo.MessageEmbed) error {
	if p == nil {
		return fmt.Errorf("announcement publisher unavailable")
	}
	return p.sendEvent(ctx, embed)
}
