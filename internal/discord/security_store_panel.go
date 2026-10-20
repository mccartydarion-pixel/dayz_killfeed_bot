package discord

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// SecurityPanelItem is one base service on sale.
type SecurityPanelItem struct {
	ServiceID    string
	PricePoints  int64
	DurationDays int
	Includes     []string // for the Sentinel Pro bundle
}

var securityPanelCopy = map[string][2]string{
	"BASE_RAID_ALARM":      {"🚨 Base Raid Alarm", "A Discord ping when someone starts breaking into your base."},
	"PERIMETER_MONITORING": {"👀 Perimeter Watch", "A Discord ping when a stranger comes near your base."},
	"BASE_BLACK_BOX":       {"📼 Base Black Box", "A history of who came near your base and what they took apart."},
	"FACTION_SECURITY":     {"👥 Faction Security", "Your base's alerts go to your faction too."},
	"SENTINEL_PRO":         {"⭐ Sentinel Pro", "Every base service in one purchase."},
}

// SecurityStorePanel renders the Security Store panel posted in Discord. It
// shows only what the server owner is selling right now.
func SecurityStorePanel(items []SecurityPanelItem, serverName, storeURL string, at time.Time) *discordgo.MessageSend {
	embed := presentation.NewChampionEmbed("🛡️ Security Store", presentation.Crimson)
	server := caseFallback(caseSafeText(serverName, 100), "this server")
	if len(items) == 0 {
		embed.Description = "Nothing is on sale on " + server + " right now. Check back later."
	} else {
		embed.Description = "Protect your base on " + server + ". Pay with Champion Points; nothing renews by itself."
		for _, it := range items {
			c, ok := securityPanelCopy[it.ServiceID]
			if !ok {
				continue
			}
			value := c[1]
			if len(it.Includes) > 0 {
				names := make([]string, 0, len(it.Includes))
				for _, id := range it.Includes {
					if n, ok := securityPanelCopy[id]; ok {
						names = append(names, strings.TrimSpace(n[0][strings.Index(n[0], " ")+1:]))
					}
				}
				value += "\nIncludes: " + strings.Join(names, ", ")
			}
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:  fmt.Sprintf("%s · %s pts / %d days", c[0], presentation.FormatThousands(it.PricePoints), it.DurationDays),
				Value: value,
			})
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "Need a base registered first?",
			Value: "Stand at your base in game and use `/registerbase`. Check yours with `/mybase`."})
	}
	embed.Footer = presentation.Footer(serverName, presentation.FooterAutoRefresh)
	presentation.StampEmbed(embed, at)
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}}
	if strings.HasPrefix(storeURL, "https://") {
		msg.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Style: discordgo.LinkButton, Label: "Open the Security Store", URL: storeURL},
		}}}
	}
	return msg
}
