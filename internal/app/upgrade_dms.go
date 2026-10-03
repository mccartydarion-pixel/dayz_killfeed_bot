package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Direct messages the automations send (docs/FEATURE_UPGRADES.md). Every message is claimed in
// upgrade_notices before it is sent, so a restart or a second bot never sends it twice; a message
// Discord refuses (closed DMs) is not retried.

var errNoDiscordLink = errors.New("player has no verified Discord link")

// sendPlayerDM sends msg to the player's verified Discord account.
func (a *App) sendPlayerDM(ctx context.Context, guildID, playerID int64, msg *discordgo.MessageSend) error {
	if a.VIPNotices == nil || a.Upgrades == nil {
		return errors.New("Discord is unavailable")
	}
	userID, err := a.Upgrades.DiscordUser(ctx, guildID, playerID)
	if err != nil {
		return err
	}
	if userID == "" {
		return errNoDiscordLink
	}
	return a.sendUserDM(userID, msg)
}

func (a *App) sendUserDM(userID string, msg *discordgo.MessageSend) error {
	if a.VIPNotices == nil {
		return errors.New("Discord is unavailable")
	}
	if msg.AllowedMentions == nil {
		msg.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: []string{}, Roles: []string{}}
	}
	ch, err := a.VIPNotices.UserChannelCreate(userID)
	if err != nil {
		return err
	}
	_, err = a.VIPNotices.ChannelMessageSendComplex(ch.ID, msg)
	return err
}

// notifyOnce claims the notice and sends it. It reports whether a message went out.
func (a *App) notifyOnce(ctx context.Context, kind string, guildID, serverID, playerID int64, ref string, now time.Time, msg *discordgo.MessageSend) bool {
	claimed, err := a.Upgrades.ClaimNotice(ctx, kind, serverID, playerID, ref, now)
	if err != nil || !claimed {
		return false
	}
	if err := a.sendPlayerDM(ctx, guildID, playerID, msg); err != nil {
		if !errors.Is(err, errNoDiscordLink) {
			slog.Info("component=upgrades", "event", "dm_failed", "kind", kind, "player_id", playerID, "err", err.Error())
		}
		return false
	}
	return true
}

func upgradeEmbed(author, title, description string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Author: &discordgo.MessageEmbedAuthor{Name: author}, Title: title, Description: description, Color: color}
}

func dm(embed *discordgo.MessageEmbed) *discordgo.MessageSend {
	return &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}}
}

func serverWord(name string) string {
	if strings.TrimSpace(name) == "" {
		return "the server"
	}
	return name
}

// --- 2. win-back -----------------------------------------------------------------------------------

const (
	winbackEvery   = time.Hour
	winbackPerPass = 20
	winbackMaxDays = 60
)

// buildWinbackDM is the message to a player who stopped playing. hooks are the reasons to come back.
func buildWinbackDM(name, serverName string, days int, hooks []string, hubURL string) *discordgo.MessageSend {
	desc := fmt.Sprintf("It's been %d days since we saw **%s** on %s.", days, strings.TrimSpace(name), serverWord(serverName))
	if len(hooks) > 0 {
		desc += "\n\n" + strings.Join(hooks, "\n")
	}
	embed := upgradeEmbed("CHAMPIONS®", "👋 We miss you on "+serverWord(serverName), desc, 0xE7B94A)
	if hubURL != "" {
		embed.Fields = []*discordgo.MessageEmbedField{{Name: "See what's new in the Player Hub", Value: hubURL}}
	}
	return dm(embed)
}

func (a *App) runWinback(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	if !s.Settings.WinbackEnabled || a.Retention == nil || !a.upgradeRuns.due(fmt.Sprintf("winback:%d", s.ServerID), winbackEvery, now) {
		return
	}
	lapsed, err := a.Retention.Lapsed(ctx, guildID, s.ServerID, now, s.Settings.WinbackDays, winbackMaxDays, 200)
	if err != nil {
		slog.Warn("component=upgrades", "msg", "win-back list failed", "server_id", s.ServerID, "err", err.Error())
		return
	}
	serverName := ""
	if a.Servers != nil {
		serverName = a.serverName(s.ServerID)
	}
	// What is happening now, shared by every message of this pass.
	var shared []string
	if a.Ranked != nil {
		if b, err := a.Ranked.CurrentRPBoost(ctx, s.ServerID, now); err == nil && b != nil {
			if b.Status == repository.RPBoostLive {
				shared = append(shared, fmt.Sprintf("⚡ **%d× RP is live** until <t:%d:f>.", b.Multiplier, b.EndsAt.Unix()))
			} else {
				shared = append(shared, fmt.Sprintf("⚡ **%d× RP** starts <t:%d:R>.", b.Multiplier, b.StartsAt.Unix()))
			}
		}
	}
	sent := 0
	for _, l := range lapsed {
		if sent >= winbackPerPass {
			break
		}
		if !l.Linked {
			continue
		}
		// One message per time away, and never two within a fortnight.
		if recent, err := a.Upgrades.NoticeSentSince(ctx, "WINBACK", s.ServerID, l.PlayerID, now.Add(-14*24*time.Hour)); err != nil || recent {
			continue
		}
		hooks := append([]string{}, shared...)
		if a.Ranked != nil {
			if p, err := a.Ranked.ServerPlayerProgress(ctx, guildID, s.ServerID, l.PlayerID); err == nil && p.NextTier != "" && p.RP > 0 {
				hooks = append(hooks, fmt.Sprintf("🏅 You're **%d RP** from %s.", p.Remaining, repository.TierName(p.NextTier)))
			}
		}
		if l.Kills > 0 {
			hooks = append(hooks, fmt.Sprintf("🔫 Your %d kills are still on the board.", l.Kills))
		}
		msg := buildWinbackDM(l.PlayerName, serverName, l.DaysSinceSeen, hooks, a.siteURL()+"/dashboard/player")
		if a.notifyOnce(ctx, "WINBACK", guildID, s.ServerID, l.PlayerID, l.LastSeenDay.Format("2006-01-02"), now, msg) {
			sent++
		}
	}
	if sent > 0 {
		slog.Info("component=upgrades", "event", "winback_sent", "server_id", s.ServerID, "count", sent)
	}
}
