package presentation

import (
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Champion's restrained semantic palette. Keep color selection attached to
// meaning rather than to individual commands or message authors.
const (
	ChampionGold    = 0xC9A227
	CombatRed       = 0x8B0000
	SuccessGreen    = 0x3F8F68
	WarningAmber    = 0xB7791F
	InfoSteel       = 0x4682B4
	NeutralGraphite = 0x2B2F33
	ErrorRed        = 0xB83232
	FactionGold     = 0xA9822B
	EventGold       = 0xD4AF37
)

const ChampionSlogan = "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"

// NewChampionEmbed establishes the common title hierarchy and footer.
func NewChampionEmbed(section string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:  "CHAMPION KILLFEED\n" + section,
		Color:  color,
		Footer: ChampionFooter(),
	}
}

func ChampionFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: ChampionSlogan}
}

func UpdatedFooter(at time.Time) *discordgo.MessageEmbedFooter {
	if at.IsZero() {
		at = time.Now()
	}
	return &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("CHAMPION KILLFEED • Updated <t:%d:R>", at.Unix())}
}

func AutoRefreshFooter() *discordgo.MessageEmbedFooter {
	return &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED • Auto-refresh"}
}

func StatusField(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
}
