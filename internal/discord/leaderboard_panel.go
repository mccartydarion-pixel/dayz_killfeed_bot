package discord

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// LeaderboardConfig sizes the Auto Leaderboard V3 categories. Every category
// is a Top 15 (presentation.MaxBoardEntries); a smaller positive limit is
// honoured, anything larger is capped at 15.
type LeaderboardConfig struct {
	TopKillsLimit   int
	TopStreaksLimit int
	TopRanksLimit   int
	TopDeathsLimit  int
	TopLongestLimit int
}

func DefaultLeaderboardConfig() LeaderboardConfig {
	n := presentation.MaxBoardEntries
	return LeaderboardConfig{TopKillsLimit: n, TopStreaksLimit: n, TopRanksLimit: n, TopDeathsLimit: n, TopLongestLimit: n}
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

func (p *LeaderboardPanel) MessageID() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.messageID
}

// ChannelID is the legacy leaderboard channel this panel publishes to.
func (p *LeaderboardPanel) ChannelID() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channelID
}

// ForgetChannel drops the legacy channel after Discord reported it gone, so the
// scheduler's sweep stops listing it. The panel then publishes nowhere until a
// routed channel takes over, which is already the case once routes exist.
func (p *LeaderboardPanel) ForgetChannel() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.channelID = ""
	p.messageID = ""
	p.lastHash = ""
	p.mu.Unlock()
}

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

// errNoMultiEmbed is returned when a Discord surface cannot send one message
// carrying several embeds (the Auto Leaderboard is never split into several).
var errNoMultiEmbed = errors.New("discord api does not support multi-embed messages")

// Update renders the full Auto Leaderboard package and sends/edits it as ONE
// message carrying every embed, so all categories change together. It returns
// the resulting message ID so callers can persist it in GuildSetup.
func (p *LeaderboardPanel) Update(snapshot LeaderboardSnapshot) (string, bool, error) {
	if p == nil || p.editor == nil {
		return "", false, nil
	}
	if p.ChannelID() == "" {
		return p.MessageID(), false, nil
	}
	api, ok := p.editor.(MultiEmbedMessageAPI)
	if !ok {
		return p.messageID, false, errNoMultiEmbed
	}
	embeds := BuildAutoLeaderboardEmbeds(snapshot, p.config)
	h := hashEmbeds(embeds)
	p.mu.Lock()
	if h == p.lastHash && p.messageID != "" {
		p.mu.Unlock()
		return p.messageID, false, nil
	}
	p.mu.Unlock()

	var msg *discordgo.Message
	var err error
	if p.messageID != "" {
		msg, err = api.ChannelMessageEditEmbeds(p.channelID, p.messageID, embeds, nil)
		if err != nil && isUnknownMessage(err) {
			// The recorded board was deleted (by an admin or a failed
			// migration): post a fresh one instead of failing every refresh.
			slog.Warn("component=discord", "event", "leaderboard_message_missing", "channel_id", p.channelID, "message_id", p.messageID)
			p.mu.Lock()
			p.messageID = ""
			p.mu.Unlock()
			msg, err = nil, nil
		}
	}
	if err == nil && p.messageID == "" {
		msg, err = api.ChannelMessageSendEmbeds(p.channelID, embeds, nil)
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

// RankEntry is one row of the Current Ranks board: a player and the label of
// their CURRENT rank in Champion's rank system ("Diamond III", ...).
type RankEntry struct {
	DisplayName string
	Rank        string
}

// LeaderboardSnapshot contains already-queried data; rendering does not touch
// SQL. Kills, Streaks, Deaths and Longest are ALL-TIME guild rankings;
// CurrentRanks is the CURRENT rank standing and is only rendered when
// RanksEnabled (an authoritative rank source is wired).
type LeaderboardSnapshot struct {
	TopKills     []repository.LeaderboardEntry
	TopStreaks   []repository.LeaderboardEntry
	CurrentRanks []RankEntry
	RanksEnabled bool
	TopDeaths    []repository.LeaderboardEntry
	TopLongest   []repository.LeaderboardEntry
	GeneratedAt  time.Time
	ServerName   string
	// TopKillsWeek is this week's kills (Monday 00:00 UTC on); shown only when WeekEnabled.
	TopKillsWeek []repository.LeaderboardEntry
	WeekEnabled  bool
	// KillMoves and LongestMoves are each listed player's movement since the day before
	// ("▲2", "▼1", "🆕"), by display name; empty when not tracked.
	KillMoves    map[string]string
	LongestMoves map[string]string
}

// Auto Leaderboard V3 product copy.
const (
	AutoLeaderboardTitle  = "📊 Auto leaderboard"
	AutoBoardKillsTitle   = "🔫 All-time top 15 kills"
	AutoBoardStreaksTitle = "🥷 All-time top 15 kill streaks"
	AutoBoardRanksTitle   = "🎖️ Current top 15 ranks"
	AutoBoardDeathsTitle  = "💀 All-time top 15 deaths"
	AutoBoardLongestTitle = "🔭 All-time top 15 longest kills"
	AutoBoardWeekTitle    = "📅 This week's top 15 kills"
)

// withMoves adds each player's movement to their board value.
func withMoves(entries []presentation.BoardEntry, moves map[string]string) []presentation.BoardEntry {
	if len(moves) == 0 {
		return entries
	}
	for i, e := range entries {
		if m := moves[e.Name]; m != "" {
			entries[i].Value = e.Value + " " + m
		}
	}
	return entries
}

// autoBoardNameCaps are the player-name caps tried in turn until the whole
// package fits Discord's combined 6000-character message limit. Real gamer
// tags (<= 16 runes on PSN) never hit even the smallest.
var autoBoardNameCaps = []int{presentation.MaxRankNameRunes, 24, 20, 16}

// BuildAutoLeaderboardEmbeds renders the Auto Leaderboard V3 package - ONE
// message, embeds in this fixed order:
//
//	0 header   📊 AUTO LEADERBOARD 📊  (server, last updated, refresh cadence)
//	1 kills    🔫 All Time Top 15 Kills 🔫
//	2 streaks  🥷 All Time Top 15 Killstreaks 🥷
//	3 ranks    🎖️ Current Top 15 Ranks 🎖️      (only when s.RanksEnabled)
//	4 deaths   💀 All Time Top 15 Deaths 💀
//	5 longest  🔭 All Time Top 15 Longest Kills 🔭
//
// Each category is its own embed with one INLINE field per player (3 columns
// x 5 rows in Discord). An empty category keeps its embed with a single
// "no qualifying players" line so the order never shifts. Only the header
// carries the brand (author + footer).
func BuildAutoLeaderboardEmbeds(s LeaderboardSnapshot, cfg LeaderboardConfig) []*discordgo.MessageEmbed {
	var embeds []*discordgo.MessageEmbed
	for _, nameCap := range autoBoardNameCaps {
		embeds = buildAutoLeaderboardEmbeds(s, cfg, nameCap)
		if autoBoardLength(embeds) <= presentation.LimitTotal {
			return embeds
		}
	}
	// Last resort (pathological stored values): drop trailing cells from the
	// fullest category until the combined message fits.
	for autoBoardLength(embeds) > presentation.LimitTotal {
		fullest := embeds[1]
		for _, e := range embeds[1:] {
			if len(e.Fields) > len(fullest.Fields) {
				fullest = e
			}
		}
		if len(fullest.Fields) == 0 {
			break
		}
		fullest.Fields = fullest.Fields[:len(fullest.Fields)-1]
	}
	return embeds
}

func buildAutoLeaderboardEmbeds(s LeaderboardSnapshot, cfg LeaderboardConfig, nameCap int) []*discordgo.MessageEmbed {
	embeds := []*discordgo.MessageEmbed{
		autoLeaderboardHeader(s),
		autoBoardEmbed(AutoBoardKillsTitle, presentation.ChampionGold, withMoves(boardEntries(s.TopKills, presentation.FormatBoardKills), s.KillMoves), cfg.TopKillsLimit, nameCap),
	}
	if s.WeekEnabled {
		week := autoBoardEmbed(AutoBoardWeekTitle, presentation.ChampionGold, boardEntries(s.TopKillsWeek, presentation.FormatBoardKills), cfg.TopKillsLimit, nameCap)
		week.Description = "Resets every Monday 00:00 UTC"
		embeds = append(embeds, week)
	}
	embeds = append(embeds, autoBoardEmbed(AutoBoardStreaksTitle, presentation.EventGold, boardEntries(s.TopStreaks, presentation.FormatBoardStreak), cfg.TopStreaksLimit, nameCap))
	if s.RanksEnabled {
		ranks := make([]presentation.BoardEntry, 0, len(s.CurrentRanks))
		for _, r := range s.CurrentRanks {
			ranks = append(ranks, presentation.BoardEntry{Name: r.DisplayName, Value: presentation.SafeName(r.Rank, 32)})
		}
		rankEmbed := autoBoardEmbed(AutoBoardRanksTitle, presentation.ChampionGold, ranks, cfg.TopRanksLimit, nameCap)
		if len(rankEmbed.Fields) > 0 {
			rankEmbed.Description = "Selected public server • active Ranked season"
		}
		embeds = append(embeds, rankEmbed)
	}
	return append(embeds,
		autoBoardEmbed(AutoBoardDeathsTitle, presentation.Gold, boardEntries(s.TopDeaths, presentation.FormatBoardDeaths), cfg.TopDeathsLimit, nameCap),
		autoBoardEmbed(AutoBoardLongestTitle, presentation.Gold, withMoves(boardEntries(s.TopLongest, presentation.FormatBoardDistance), s.LongestMoves), cfg.TopLongestLimit, nameCap),
	)
}

// autoLeaderboardHeader is the compact status embed. "Last Updated" is a
// Discord relative timestamp (<t:UNIX:R>), rendered client-side as "just now"
// / "14 minutes ago" / "3 hours ago" - nothing is recomputed between
// refreshes. It carries the refresh time on purpose: a real scheduled refresh
// edits the message so the board shows its actual last successful refresh.
func autoLeaderboardHeader(s LeaderboardSnapshot) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Author: presentation.ChampionAuthor(),
		Title:  AutoLeaderboardTitle,
		Color:  presentation.ChampionGold,
		Footer: presentation.AutoRefreshFooter(),
	}
	var lines []string
	if name := strings.TrimSpace(s.ServerName); name != "" {
		lines = append(lines, "**"+presentation.SafeName(name, 100)+"**")
	}
	if !s.GeneratedAt.IsZero() {
		lines = append(lines, "Last updated "+presentation.Timestamp(s.GeneratedAt, 'R'))
	}
	lines = append(lines, "Refreshes "+refreshCadence(LeaderboardRefreshInterval))
	embed.Description = strings.Join(lines, "\n")
	return presentation.FitEmbed(embed)
}

// refreshCadence renders the scheduler interval: "every 3 hours".
func refreshCadence(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return "every " + presentation.Plural(int64(d/time.Hour), "hour", "hours")
	}
	return "every " + presentation.Plural(int64(d/time.Minute), "minute", "minutes")
}

func autoBoardEmbed(title string, color int, entries []presentation.BoardEntry, limit, nameCap int) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{Title: title, Color: color}
	embed.Fields = presentation.BoardGridFields(entries, limit, nameCap)
	if len(embed.Fields) == 0 {
		embed.Description = presentation.EmptyBoard
	}
	return presentation.FitEmbed(embed)
}

func boardEntries(entries []repository.LeaderboardEntry, format func(string) string) []presentation.BoardEntry {
	out := make([]presentation.BoardEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, presentation.BoardEntry{Name: e.DisplayName, Value: format(e.Value)})
	}
	return out
}

// autoBoardLength is Discord's combined character count for one message: the
// 6000-character limit applies across ALL embeds of a message, not per embed.
func autoBoardLength(embeds []*discordgo.MessageEmbed) int {
	n := 0
	for _, e := range embeds {
		n += presentation.EmbedLength(e)
	}
	return n
}

func safePanelText(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "@", ""), "#", "")
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:79]) + "…"
	}
	return s
}

// hashEmbeds fingerprints a multi-embed message: the embed count, their order
// and every visible part of each (see hashEmbed). Reordering, adding or
// removing an embed, or any change inside one, changes the hash.
func hashEmbeds(embeds []*discordgo.MessageEmbed) string {
	h := sha256.New()
	fmt.Fprintf(h, "embeds:%d|", len(embeds))
	for i, e := range embeds {
		fmt.Fprintf(h, "%d:%s|", i, hashEmbed(e))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashPanelContent fingerprints what a routed panel shows: the multi-embed
// package when Embeds is set, otherwise the single Embed (unchanged).
func hashPanelContent(c PanelContent) string {
	if len(c.Embeds) > 0 {
		return hashEmbeds(c.Embeds)
	}
	return hashEmbed(c.Embed)
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
	embed := presentation.NewChampionEmbed("Player stats", presentation.Crimson)
	embed.Description = "View your Champion profile or search another player."
	return embed
}
