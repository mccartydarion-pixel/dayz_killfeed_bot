package discord

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
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

// Reset forgets the current message so the next Update posts a fresh one. Used
// after the legacy panel message was retired in favour of routed panels.
func (p *LeaderboardPanel) Reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.messageID = ""
	p.lastHash = ""
	p.mu.Unlock()
}

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

// Section caps for the stored-string blocks of the season board.
const (
	leaderboardPointsLimit  = 5
	leaderboardEventsLimit  = 5
	leaderboardWantedLimit  = 5
	leaderboardKillsDefault = 10
	leaderboardOtherDefault = 5
)

// BuildLeaderboardEmbed renders the persistent season board as a scoreboard:
// ONE field per category (never one field per player), podium medals, and
// category-formatted values through the shared presentation rank formatter.
//
//	author  CHAMPIONS® KILLFEED
//	title   🏆 SEASON LEADERBOARD
//	desc    Competitive Rankings
//	fields  ⚔️ TOP KILLERS (10) · 🎯 LONGEST KILLS (5) · 📈 BEST K/D (5)
//	        🏆 CHAMPION POINTS · 🔥 LIVE EVENTS · 🎯 MOST WANTED   (when present)
//	footer  CHAMPION • AUTO-REFRESH
//
// No separate "kill leader" spotlight: it would only repeat the 🥇 row
// directly beneath it. Rendering only: the snapshot was already queried
// (eligibility filters such as MinKillsForKD live in the queries and are
// untouched here).
func BuildLeaderboardEmbed(s LeaderboardSnapshot, cfg LeaderboardConfig) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("🏆 SEASON LEADERBOARD", presentation.ChampionGold)
	embed.Footer = presentation.AutoRefreshFooter()

	desc := "Competitive Rankings"
	presentation.AppendFields(embed,
		presentation.RankingField(presentation.RankKills, rankedEntries(s.TopKills), limitOr(cfg.TopKillsLimit, leaderboardKillsDefault)),
		presentation.RankingField(presentation.RankLongest, rankedEntries(s.TopLongest), limitOr(cfg.TopLongestLimit, leaderboardOtherDefault)),
		presentation.RankingField(presentation.RankKD, rankedEntries(s.TopKD), limitOr(cfg.TopKDLimit, leaderboardOtherDefault)),
		presentation.RankingField(presentation.RankPoints, rankedEntries(s.Points), leaderboardPointsLimit),
		presentation.MetricField("🔥 LIVE EVENTS", presentation.BulletBlock(s.LiveEvents, leaderboardEventsLimit), false),
		presentation.MetricField("🎯 MOST WANTED", presentation.BulletBlock(s.ActiveBounties, leaderboardWantedLimit), false),
	)
	if len(embed.Fields) == 0 {
		desc += "\n\n" + presentation.EmptyRanking
	}
	embed.Description = desc
	return presentation.FitEmbed(embed)
}

func rankedEntries(entries []repository.LeaderboardEntry) []presentation.RankedEntry {
	out := make([]presentation.RankedEntry, 0, len(entries))
	for i, e := range entries {
		out = append(out, presentation.RankedEntry{Rank: i + 1, Name: e.DisplayName, Value: e.Value})
	}
	return out
}

func limitOr(n, def int) int {
	if n > 0 {
		return n
	}
	return def
}

func safePanelText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "@", ""), "#", "")
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

// hashEmbed fingerprints everything a viewer sees on a persistent panel, so an
// unchanged panel is never re-edited and a real change is never skipped:
// title, description, color, author, footer, every field (name, value,
// inline) and image/thumbnail URLs. The embed timestamp is deliberately
// excluded - it is volatile (refresh time) and would force an edit on every
// cycle. Fields are length-prefixed so content cannot shift between parts.
func hashEmbed(e *discordgo.MessageEmbed) string {
	h := sha256.New()
	put := func(v string) { fmt.Fprintf(h, "%d:%s|", len(v), v) }
	if e == nil {
		put("nil")
		return hex.EncodeToString(h.Sum(nil))
	}
	put(e.Title)
	put(e.Description)
	put(e.URL)
	put(fmt.Sprint(e.Color))
	if e.Author != nil {
		put("author")
		put(e.Author.Name)
		put(e.Author.IconURL)
		put(e.Author.URL)
	}
	if e.Footer != nil {
		put("footer")
		put(e.Footer.Text)
		put(e.Footer.IconURL)
	}
	if e.Thumbnail != nil {
		put("thumb")
		put(e.Thumbnail.URL)
	}
	if e.Image != nil {
		put("image")
		put(e.Image.URL)
	}
	put(fmt.Sprint(len(e.Fields)))
	for _, f := range e.Fields {
		if f == nil {
			put("nilfield")
			continue
		}
		put(f.Name)
		put(f.Value)
		put(fmt.Sprint(f.Inline))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PlayerStatsInfoEmbed is the persistent instruction panel for #player-stats.
func PlayerStatsInfoEmbed() *discordgo.MessageEmbed {
	embed := presentation.NewChampionEmbed("PLAYER STATS", presentation.InfoSteel)
	embed.Description = "View your Champion profile or search another player."
	return embed
}
