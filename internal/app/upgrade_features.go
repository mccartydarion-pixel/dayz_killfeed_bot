package app

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 29, announce new automations: when staff switch an automation on and the server's
// "announce new automations" switch is on, the guild's features channel (made with /features) gets
// a post saying what is new. Nothing is posted when the guild has no features channel.

// automationNews says, for each switch, what players get. Switches that only staff see are absent.
var automationNews = map[string][2]string{
	"priorityRankedTop":  {"🎟️ Priority queue for top players", "The best ranked players skip the queue when the server is full."},
	"priorityVip":        {"🎟️ Priority queue for supporters", "Supporter tier holders skip the queue when the server is full."},
	"winbackEnabled":     {"👋 Welcome-back messages", "Been away a while? We'll message you with what's new."},
	"seasonRewards":      {"🏆 Season-end rewards", "The top 3 of each ranked season win champ credits."},
	"eventScoreboard":    {"📋 Live event scoreboard", "Events now have a live standings message that updates as you play."},
	"killfeedRankedTags": {"🏷️ RP on kill cards", "Kill cards show the RP you earned, with your bonuses."},
	"lifeStoryDms":       {"🪦 Life recaps", "When you die you get a recap of your life by direct message (turn it off with /life recap off)."},
	"bountyDms":          {"💌 Bounty updates", "Placed a bounty? You'll hear when it's claimed or runs out."},
	"rentReminders":      {"🏠 Earlier rent reminders", "Base owners and their faction hear three days before rent is due."},
	"dailyLoginCredits":  {"🪙 Daily play reward", "Champ credits for every day you play, more for each day in a row."},
	"shopProgressDms":    {"📦 Order updates", "Shop orders now message you at each step of the delivery."},
	"perkReminderDms":    {"⏰ Renewal reminders", "A heads-up three days before a subscription renews or a supporter tier ends."},
	"caseAppeals":        {"⚖️ Appeals", "You can now send staff an appeal from the Player Hub."},
}

// automationNewsFor lists the posts for the switches that went on, in a stable order.
func automationNewsFor(switched []string) [][2]string {
	sort.Strings(switched)
	var out [][2]string
	for _, key := range switched {
		if news, ok := automationNews[key]; ok {
			out = append(out, news)
		}
	}
	return out
}

func buildAutomationNews(news [][2]string, serverName string) *discordgo.MessageEmbed {
	lines := make([]string, 0, len(news))
	for _, n := range news {
		lines = append(lines, "**"+n[0]+"**\n"+n[1])
	}
	return &discordgo.MessageEmbed{Author: presentation.ChampionAuthor(), Color: presentation.Crimson,
		Title: "✨ New on " + serverWord(serverName), Description: strings.Join(lines, "\n\n")}
}

// announceSwitchedOn posts in the features channel about automations that were just switched on.
func (a *App) announceSwitchedOn(ctx context.Context, guildID, serverID int64, before, after repository.UpgradeSettings) {
	if !after.FeatureAnnouncements || a.Upgrades == nil || a.Discord == nil || a.Discord.Session() == nil {
		return
	}
	news := automationNewsFor(repository.UpgradeSwitches(before, after))
	if len(news) == 0 {
		return
	}
	channelID, err := a.Upgrades.FeaturesChannel(ctx, guildID)
	if err != nil || channelID == "" {
		return
	}
	if _, err := a.Discord.Session().ChannelMessageSendEmbed(channelID, buildAutomationNews(news, a.serverName(serverID))); err != nil {
		slog.Warn("component=upgrades", "msg", "features channel post failed", "err", err.Error())
	}
}
