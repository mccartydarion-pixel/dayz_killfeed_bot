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
	embed := &discordgo.MessageEmbed{Title: "🏆 CHAMPION LEADERBOARD", Description: "Competitive rankings", Color: ColorChampionGold, Footer: &discordgo.MessageEmbedFooter{Text: "CHAMPION KILLFEED • EVERY KILL TELLS A STORY"}}
	appendRankFields(embed, "⚔️ TOP KILLERS", s.TopKills)
	appendRankFields(embed, "🎯 LONGEST KILL", s.TopLongest)
	appendRankFields(embed, "🔥 BEST K/D", s.TopKD)
	if len(s.LiveEvents) > 0 {
		for idx, line := range s.LiveEvents {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: fmt.Sprintf("🔥 LIVE EVENT #%d", idx+1), Value: safePanelText(line), Inline: false})
		}
	}
	if len(s.ActiveBounties) > 0 {
		for idx, line := range s.ActiveBounties {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: fmt.Sprintf("🎯 MOST WANTED #%d", idx+1), Value: safePanelText(line), Inline: false})
		}
	}
	if len(s.Points) > 0 {
		appendRankFields(embed, "🏆 CHAMPION POINTS", s.Points)
	}
	return embed
}

func appendRankFields(embed *discordgo.MessageEmbed, heading string, entries []repository.LeaderboardEntry) {
	if len(entries) == 0 {
		return
	}
	for i, entry := range entries {
		medal := fmt.Sprintf("#%d", i+1)
		if i == 0 {
			medal = "#1"
		}
		if i == 1 {
			medal = "#2"
		}
		if i == 2 {
			medal = "#3"
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: heading + " • " + medal + " " + safePanelText(entry.DisplayName), Value: "**Value**  " + safePanelText(entry.Value), Inline: false})
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
