package discord

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// LeaderboardConfig controls compact panel sizes and KD eligibility.
type LeaderboardConfig struct {
	TopKillsLimit   int
	TopKDLimit      int
	TopLongestLimit int
	MinKillsForKD   int
}

func DefaultLeaderboardConfig() LeaderboardConfig {
	return LeaderboardConfig{TopKillsLimit: 10, TopKDLimit: 5, TopLongestLimit: 5, MinKillsForKD: 5}
}

// LeaderboardPanel edits one persistent message and skips identical snapshots.
type LeaderboardPanel struct {
	editor    MessageEditor
	channelID string
	messageID string
	config    LeaderboardConfig
	mu        sync.Mutex
	lastHash  string
}

func NewLeaderboardPanel(editor MessageEditor, channelID, messageID string, cfg LeaderboardConfig) *LeaderboardPanel {
	return &LeaderboardPanel{editor: editor, channelID: channelID, messageID: messageID, config: cfg}
}

func (p *LeaderboardPanel) MessageID() string { return p.messageID }

// Update renders and edits/sends exactly one persistent panel. It returns the
// resulting message ID so callers can persist it in GuildSetup.
func (p *LeaderboardPanel) Update(snapshot LeaderboardSnapshot) (string, bool, error) {
	if p == nil || p.editor == nil || p.channelID == "" {
		return p.messageID, false, nil
	}
	embed := BuildLeaderboardEmbed(snapshot, p.config)
	h := hashEmbed(embed)
	p.mu.Lock()
	if h == p.lastHash && p.messageID != "" {
		p.mu.Unlock()
		return p.messageID, false, nil
	}
	p.mu.Unlock()

	var msg *discordgo.Message
	var err error
	if p.messageID == "" {
		msg, err = p.editor.ChannelMessageSendEmbed(p.channelID, embed)
	} else {
		msg, err = p.editor.ChannelMessageEditEmbed(p.channelID, p.messageID, embed)
	}
	if err != nil {
		return p.messageID, false, err
	}
	if msg != nil && p.messageID == "" {
		p.messageID = msg.ID
	}
	p.mu.Lock()
	p.lastHash = h
	p.mu.Unlock()
	return p.messageID, true, nil
}

// LeaderboardSnapshot contains already-queried data; rendering does not touch SQL.
type LeaderboardSnapshot struct {
	TopKills       []repository.LeaderboardEntry
	TopKD          []repository.LeaderboardEntry
	TopLongest     []repository.LeaderboardEntry
	GeneratedAt    time.Time
	LiveEvents     []string
	ActiveBounties []string
	Points         []repository.LeaderboardEntry
}

func BuildLeaderboardEmbed(s LeaderboardSnapshot, cfg LeaderboardConfig) *discordgo.MessageEmbed {
	var b strings.Builder
	b.WriteString("🏆 **CHAMPION LEADERBOARD**\n\n")
	b.WriteString("⚔️ **TOP KILLERS**\n")
	appendEntries(&b, s.TopKills)
	b.WriteString("\n━━━━━━━━━━━━━━━━\n\n🎯 **LONGEST KILL**\n")
	appendEntries(&b, s.TopLongest)
	b.WriteString("\n━━━━━━━━━━━━━━━━\n\n🔥 **BEST K/D**\n")
	appendEntries(&b, s.TopKD)
	if len(s.LiveEvents) > 0 {
		b.WriteString("\n━━━━━━━━━━━━━━━━\n\n🔥 **LIVE EVENTS**\n")
		for _, line := range s.LiveEvents {
			fmt.Fprintf(&b, "%s\n", safePanelText(line))
		}
	}
	if len(s.ActiveBounties) > 0 {
		b.WriteString("\n━━━━━━━━━━━━━━━━\n\n🎯 **MOST WANTED**\n")
		for _, line := range s.ActiveBounties {
			fmt.Fprintf(&b, "%s\n", safePanelText(line))
		}
	}
	if len(s.Points) > 0 {
		b.WriteString("\n━━━━━━━━━━━━━━━━\n\n🏆 **CHAMPION POINTS**\n")
		appendEntries(&b, s.Points)
	}
	fmt.Fprintf(&b, "\nLast Updated\n<t:%d:R>", s.GeneratedAt.Unix())
	return &discordgo.MessageEmbed{
		Title:       "🏆 CHAMPION LEADERBOARD",
		Description: b.String(),
		Color:       ColorChampionGold,
		Footer:      &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"},
	}
}

func appendEntries(b *strings.Builder, entries []repository.LeaderboardEntry) {
	for i, entry := range entries {
		medal := ""
		switch i {
		case 0:
			medal = "🥇 "
		case 1:
			medal = "🥈 "
		case 2:
			medal = "🥉 "
		}
		fmt.Fprintf(b, "%s%d. %s — %s\n", medal, i+1, safePanelText(entry.DisplayName), entry.Value)
	}
	if len(entries) == 0 {
		b.WriteString("_No data yet._\n")
	}
}

func safePanelText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "@", ""), "#", "")
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

func hashEmbed(e *discordgo.MessageEmbed) string {
	v := e.Title + "\x00" + e.Description + "\x00" + fmt.Sprint(e.Color) + "\x00" + e.Footer.Text
	s := sha256.Sum256([]byte(v))
	return hex.EncodeToString(s[:])
}

// PlayerStatsInfoEmbed is the persistent instruction panel for #player-stats.
func PlayerStatsInfoEmbed() *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       "🏆 CHAMPION PLAYER STATS",
		Description: "Track your combat record.\n\nUse `/stats` to view kills, deaths, K/D, longest kill, top weapon, and server records.\n\n🎮 Link your PlayStation account with `/link username:<PSN>`.\n\nVerified linked players can use `/stats` without a player argument.",
		Color:       ColorChampionGold,
		Footer:      &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED"},
	}
}
