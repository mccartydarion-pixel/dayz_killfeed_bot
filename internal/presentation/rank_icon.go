package presentation

import (
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Ranked Points tier icons. The website serves one picture per tier at
// <site>/ranks/<tier>.png (128 px, transparent). They belong to the Ranked Points tier only:
// leaderboard positions, kill-count rankings, faction ranks, battle pass levels and supporter
// tiers never use them.

var rankTierIcons = map[string]bool{
	"unranked": true, "rookie": true, "bronze": true, "silver": true,
	"gold": true, "platinum": true, "diamond": true, "master": true,
}

// RankTierIconSlug is the file name (without extension) of a tier's icon: "gold" for "GOLD".
// A tier this build does not know falls back to "unranked".
func RankTierIconSlug(tier string) string {
	slug := strings.ToLower(strings.TrimSpace(tier))
	if rankTierIcons[slug] {
		return slug
	}
	return "unranked"
}

// RankTierIconURL is the public address of a tier's icon on the website, or "" when no site
// address is known (a card then carries no picture rather than a broken one).
func RankTierIconURL(siteBase, tier string) string {
	base := strings.TrimRight(strings.TrimSpace(siteBase), "/")
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return ""
	}
	return base + "/ranks/" + RankTierIconSlug(tier) + ".png"
}

// RankTierThumbnail is the embed thumbnail for a card about one player's Ranked tier; nil when
// no site address is known. High-volume feed cards (kills, hits) never carry it.
func RankTierThumbnail(siteBase, tier string) *discordgo.MessageEmbedThumbnail {
	url := RankTierIconURL(siteBase, tier)
	if url == "" {
		return nil
	}
	return &discordgo.MessageEmbedThumbnail{URL: url}
}
