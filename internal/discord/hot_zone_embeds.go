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
	embed := presentation.NewFeedEmbed("🔥 HOT ZONE OPEN", presentation.CombatRed)
	desc := "**" + presentation.SafeName(a.Name, 60) + "**"
	if s := strings.TrimSpace(a.ServerName); s != "" {
		desc += " on " + presentation.SafeName(s, 60)
	}
	if a.KillsObserved > 0 && a.WindowMinutes > 0 {
		desc += fmt.Sprintf("\n%s there in the last %d minutes.", presentation.Plural(int64(a.KillsObserved), "kill", "kills"), a.WindowMinutes)
	}
	embed.Description = desc
	presentation.AppendFields(embed,
		presentation.MetricField("WHERE", fmt.Sprintf("Within %.0f m of %.0f / %.0f", a.RadiusM, a.CenterX, a.CenterZ), true))
	if a.EndsAt != nil {
		presentation.AppendFields(embed, presentation.MetricField("ENDS", fmt.Sprintf("<t:%d:R>", a.EndsAt.Unix()), true))
	}
	var prizes []string
	for i, pts := range []int{a.FirstPoints, a.SecondPoints, a.ThirdPoints} {
		if pts > 0 {
			prizes = append(prizes, fmt.Sprintf("%s %s", []string{"🥇", "🥈", "🥉"}[i], presentation.FormatPoints(int64(pts))))
		}
	}
	presentation.AppendFields(embed, presentation.MetricField("PRIZES", strings.Join(prizes, " • "), false))
	presentation.AppendFields(embed, presentation.MetricField("HOW IT SCORES", "One point per kill on a player inside the zone.", false))
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
