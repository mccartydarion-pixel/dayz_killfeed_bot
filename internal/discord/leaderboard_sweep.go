package discord

import (
	"log/slog"
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

// leaderboardSweepScanLimit is how many recent messages per channel the sweep
// inspects (one Discord request per leaderboard channel per refresh).
const leaderboardSweepScanLimit = 100

// ChannelHistoryAPI lists recent channel messages and names the bot's own user,
// so the sweep can recognise the bot's persistent panels. *SessionAPI
// implements it; an API without it simply skips the sweep.
type ChannelHistoryAPI interface {
	ChannelMessages(channelID string, limit int) ([]*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string) error
	BotUserID() string
}

// isLeaderboardBoardTitle reports whether an embed title is one the persistent
// leaderboard board has ever used: the pre-V2 "SEASON LEADERBOARD" /
// "CHAMPION KILLFEED\nSEASON LEADERBOARD" / "🏆 CHAMPION LEADERBOARD" cards,
// the V2 "🏆 SEASON LEADERBOARD" card and the V3 "📊 AUTO LEADERBOARD 📊"
// header. The /leaderboard command card ("🏆 PLAYER LEADERBOARD") and every
// other panel are deliberately NOT matched.
func isLeaderboardBoardTitle(title string) bool {
	if title == AutoLeaderboardTitle {
		return true
	}
	norm := strings.Join(strings.FieldsFunc(strings.ToUpper(title), func(r rune) bool {
		return !unicode.IsLetter(r)
	}), " ")
	// "AUTO LEADERBOARD" covers the V3 header in both spellings: "📊 AUTO LEADERBOARD 📊" (as
	// posted before the sentence-case titles) and today's "📊 Auto leaderboard".
	return norm == "SEASON LEADERBOARD" || norm == "CHAMPION KILLFEED SEASON LEADERBOARD" || norm == "CHAMPION LEADERBOARD" || norm == "AUTO LEADERBOARD"
}

// isObsoleteLeaderboardMessage is true only for a message the bot itself posted
// as a persistent board (not an interaction reply, not a player message) whose
// first embed is a leaderboard board, and which is not a currently recorded
// board message.
func isObsoleteLeaderboardMessage(m *discordgo.Message, botID string, keep map[string]bool) bool {
	if m == nil || botID == "" || keep[m.ID] || m.Author == nil || m.Author.ID != botID {
		return false
	}
	if m.Type != discordgo.MessageTypeDefault || m.Interaction != nil || m.InteractionMetadata != nil {
		return false
	}
	if len(m.Embeds) == 0 || m.Embeds[0] == nil {
		return false
	}
	return isLeaderboardBoardTitle(m.Embeds[0].Title)
}

// sweepObsoleteLeaderboards deletes stale copies of the leaderboard board that
// the bot left in its leaderboard channels: old single-embed season boards
// from earlier releases, a legacy board whose retirement delete failed, or an
// orphaned placeholder. Only the bot's own board messages are touched; the
// recorded current board(s) in keep, player messages, the Player Stats panel
// and every other panel are never deleted. Best-effort: failures are logged
// and retried on the next refresh.
//
// A channel Discord no longer knows (deleted, or the bot was removed from it)
// is reported in gone rather than warned about on every refresh: the caller
// forgets it so the sweep stops asking.
func sweepObsoleteLeaderboards(api ChannelHistoryAPI, channels []string, keep map[string]bool) (deleted int, gone []string) {
	if api == nil {
		return 0, nil
	}
	botID := api.BotUserID()
	if botID == "" {
		return 0, nil
	}
	seen := make(map[string]bool, len(channels))
	for _, channelID := range channels {
		if channelID == "" || seen[channelID] {
			continue
		}
		seen[channelID] = true
		msgs, err := api.ChannelMessages(channelID, leaderboardSweepScanLimit)
		if err != nil {
			if isUnknownMessage(err) {
				slog.Info("component=discord", "event", "leaderboard_sweep_channel_gone", "channel_id", channelID)
				gone = append(gone, channelID)
				continue
			}
			slog.Warn("component=discord", "event", "leaderboard_sweep_list_failed", "channel_id", channelID, "err", err.Error())
			continue
		}
		for _, m := range msgs {
			if !isObsoleteLeaderboardMessage(m, botID, keep) {
				continue
			}
			if err := api.ChannelMessageDelete(channelID, m.ID); err != nil && !isUnknownMessage(err) {
				slog.Warn("component=discord", "event", "leaderboard_sweep_delete_failed", "channel_id", channelID, "message_id", m.ID, "err", err.Error())
				continue
			}
			deleted++
			slog.Info("component=discord", "event", "leaderboard_obsolete_board_removed", "channel_id", channelID, "message_id", m.ID, "title", m.Embeds[0].Title)
		}
	}
	return deleted, gone
}
