package discord

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const sweepBotID = "bot-1"

// historyDiscord is fakeDiscord plus channel history: every message carries an
// author, type and first-embed title, so the sweep can be exercised end to end.
type historyDiscord struct {
	*fakeDiscord
	mu    sync.Mutex
	meta  map[string]*discordgo.Message // channel/id -> metadata
	order map[string][]string           // channel -> ids, oldest first
}

func newHistoryDiscord() *historyDiscord {
	return &historyDiscord{fakeDiscord: newFakeDiscord(), meta: map[string]*discordgo.Message{}, order: map[string][]string{}}
}

func (h *historyDiscord) put(channelID, id, author, title string, typ discordgo.MessageType, interaction bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &discordgo.Message{ID: id, ChannelID: channelID, Author: &discordgo.User{ID: author}, Type: typ}
	if title != "" {
		m.Embeds = []*discordgo.MessageEmbed{{Title: title}}
	}
	if interaction {
		m.Interaction = &discordgo.MessageInteraction{ID: "i-" + id}
	}
	key := channelID + "/" + id
	if _, ok := h.meta[key]; !ok {
		h.order[channelID] = append(h.order[channelID], id)
	}
	h.meta[key] = m
}

// seedMessage puts a pre-existing message (any author) into a channel.
func (h *historyDiscord) seedMessage(channelID, id, author, title string) {
	h.fakeDiscord.seed(channelID, id)
	h.put(channelID, id, author, title, discordgo.MessageTypeDefault, false)
}

func (h *historyDiscord) ChannelMessageSendEmbeds(channelID string, embeds []*discordgo.MessageEmbed, c []discordgo.MessageComponent) (*discordgo.Message, error) {
	msg, err := h.fakeDiscord.ChannelMessageSendEmbeds(channelID, embeds, c)
	if err == nil {
		h.put(channelID, msg.ID, sweepBotID, embeds[0].Title, discordgo.MessageTypeDefault, false)
	}
	return msg, err
}

func (h *historyDiscord) ChannelMessageSendComplex(channelID string, e *discordgo.MessageEmbed, c []discordgo.MessageComponent) (*discordgo.Message, error) {
	msg, err := h.fakeDiscord.ChannelMessageSendComplex(channelID, e, c)
	if err == nil {
		title := ""
		if e != nil {
			title = e.Title
		}
		h.put(channelID, msg.ID, sweepBotID, title, discordgo.MessageTypeDefault, false)
	}
	return msg, err
}

func (h *historyDiscord) ChannelMessages(channelID string, limit int) ([]*discordgo.Message, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*discordgo.Message
	ids := h.order[channelID]
	for i := len(ids) - 1; i >= 0 && len(out) < limit; i-- { // newest first
		if h.fakeDiscord.messages[channelID][ids[i]] {
			out = append(out, h.meta[channelID+"/"+ids[i]])
		}
	}
	return out, nil
}

func (h *historyDiscord) BotUserID() string { return sweepBotID }

func (h *historyDiscord) liveIDs(channelID string) []string {
	h.fakeDiscord.mu.Lock()
	defer h.fakeDiscord.mu.Unlock()
	var ids []string
	for id, alive := range h.fakeDiscord.messages[channelID] {
		if alive {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestLeaderboardBoardTitleMatchesOnlyBoards(t *testing.T) {
	// "📊 AUTO LEADERBOARD 📊" is the V3 header as it was posted before the sentence-case titles:
	// a board left behind with that title must still be recognised and cleaned up.
	for _, title := range []string{"🏆 SEASON LEADERBOARD", "SEASON LEADERBOARD", "CHAMPION KILLFEED\nSEASON LEADERBOARD", "🏆 CHAMPION LEADERBOARD", AutoLeaderboardTitle, "📊 AUTO LEADERBOARD 📊", "📊 Auto leaderboard"} {
		if !isLeaderboardBoardTitle(title) {
			t.Errorf("%q must be recognised as a leaderboard board", title)
		}
	}
	for _, title := range []string{"🏆 PLAYER LEADERBOARD", "🏆 Player leaderboard", "PLAYER STATS", "Player stats", "🎖️ SERVER RANKS 🎖️", "🎖️ Server ranks", "🔫 All Time Top 15 Kills 🔫",
		AutoBoardKillsTitle, "SEASON LEADERBOARD NOTES", "", "🏆 SEASON COMPLETE", "🏆 Season complete"} {
		if isLeaderboardBoardTitle(title) {
			t.Errorf("%q must never be swept", title)
		}
	}
}

func TestSweepDeletesOnlyTheBotsObsoleteBoards(t *testing.T) {
	h := newHistoryDiscord()
	h.seedMessage("lb", "old-season", sweepBotID, "🏆 SEASON LEADERBOARD")
	h.seedMessage("lb", "older-season", sweepBotID, "CHAMPION KILLFEED\nSEASON LEADERBOARD")
	h.seedMessage("lb", "dup-v3", sweepBotID, AutoLeaderboardTitle)
	h.seedMessage("lb", "current", sweepBotID, AutoLeaderboardTitle)
	h.seedMessage("lb", "stats-panel", sweepBotID, "PLAYER STATS")
	h.seedMessage("lb", "player-post", "player-9", "🏆 SEASON LEADERBOARD") // a player quoting the board
	h.seedMessage("lb", "chat", "player-9", "")
	h.fakeDiscord.seed("lb", "slash-reply")
	h.put("lb", "slash-reply", sweepBotID, "🏆 SEASON LEADERBOARD", discordgo.MessageTypeChatInputCommand, true)

	if n, _ := sweepObsoleteLeaderboards(h, []string{"lb", "lb", ""}, map[string]bool{"current": true}); n != 3 {
		t.Fatalf("expected 3 obsolete boards removed, got %d", n)
	}
	want := []string{"chat", "current", "player-post", "slash-reply", "stats-panel"}
	if got := h.liveIDs("lb"); !equalStrings(got, want) {
		t.Fatalf("live after sweep = %v, want %v", got, want)
	}
}

// goneChannelHistory answers Unknown Channel for one channel id, as Discord does
// for a channel that was deleted after the bot recorded it.
type goneChannelHistory struct {
	*historyDiscord
	gone string
}

func (g goneChannelHistory) ChannelMessages(channelID string, limit int) ([]*discordgo.Message, error) {
	if channelID == g.gone {
		return nil, unknownChannelErr()
	}
	return g.historyDiscord.ChannelMessages(channelID, limit)
}

// The reported production log: the legacy guild_settings leaderboard channel
// was deleted, and every refresh warned leaderboard_sweep_list_failed for it.
func TestSweepReportsDeletedChannelAsGone(t *testing.T) {
	h := newHistoryDiscord()
	h.seedMessage("lb", "old", sweepBotID, "🏆 SEASON LEADERBOARD")
	n, gone := sweepObsoleteLeaderboards(goneChannelHistory{h, "legacy-deleted"}, []string{"lb", "legacy-deleted"}, nil)
	if n != 1 {
		t.Fatalf("the live channel must still be swept, got %d", n)
	}
	if !equalStrings(gone, []string{"legacy-deleted"}) {
		t.Fatalf("gone = %v, want the deleted channel only", gone)
	}
}

func TestSchedulerForgetsDeletedLegacyChannel(t *testing.T) {
	h := newHistoryDiscord()
	panel := NewLeaderboardPanel(goneChannelHistory{h, "legacy-deleted"}, "legacy-deleted", "", DefaultLeaderboardConfig())
	s := NewLeaderboardScheduler(panel, nil, 1, DefaultLeaderboardConfig(), nil)
	s.sweep([]string{"lb", panel.ChannelID()}, nil)
	if panel.ChannelID() != "" {
		t.Fatalf("legacy channel still %q after Discord reported it gone", panel.ChannelID())
	}
	// A second sweep no longer asks Discord about the deleted channel.
	s.sweep([]string{"lb", panel.ChannelID()}, nil)
}

type noBotHistory struct{ *historyDiscord }

func (noBotHistory) BotUserID() string { return "" }

func TestSweepIsANoOpWithoutBotIdentity(t *testing.T) {
	h := newHistoryDiscord()
	h.seedMessage("lb", "old", sweepBotID, "🏆 SEASON LEADERBOARD")
	if n, _ := sweepObsoleteLeaderboards(noBotHistory{h}, []string{"lb"}, nil); n != 0 || len(h.liveIDs("lb")) != 1 {
		t.Fatal("without the bot's own id nothing may be deleted")
	}
}

// The reported production problem: the leaderboards channel still shows the
// old single-embed season board next to (or instead of) the V3 grid.
func TestRoutedRefreshReplacesObsoleteSingleEmbedBoard(t *testing.T) {
	f := newRoutedFixture(t)
	h := newHistoryDiscord()
	f.api = h.fakeDiscord
	f.panels = NewRoutePanels(h, f.store)
	h.seedMessage("leaderboards", "old-board", sweepBotID, "🏆 SEASON LEADERBOARD")
	h.seedMessage("leaderboards", "stats-panel", sweepBotID, "PLAYER STATS")
	h.seedMessage("leaderboards", "player-msg", "player-1", "")

	panel := NewLeaderboardPanel(h, "leaderboards", "", DefaultLeaderboardConfig())
	s := NewLeaderboardScheduler(panel, newFixtureStats(), f.guild, DefaultLeaderboardConfig(), func(string) {})
	servers := func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }
	s.SetRouting(f.resolver, servers, f.panels, NewLegacyLeaderboardRetirer(h, f.setup, "g1"))
	f.resolver.set(f.guild, 1, routeKeyAutoLeaderboard, "leaderboards")

	for i := 0; i < 2; i++ {
		if err := s.RefreshOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	board := f.store.messageFor(f.guild, routeKeyAutoLeaderboard, "leaderboards")
	want := []string{board, "player-msg", "stats-panel"}
	sort.Strings(want)
	if got := h.liveIDs("leaderboards"); !equalStrings(got, want) {
		t.Fatalf("live = %v, want exactly the V3 board, the Player Stats panel and the player message %v", got, want)
	}
	if embeds := h.embedsIn("leaderboards"); len(embeds) < 5 || embeds[0].Title != AutoLeaderboardTitle {
		t.Fatalf("the surviving board must be the V3 multi-embed package, got %d embeds", len(embeds))
	}
}

func TestLegacyRefreshRecoversFromADeletedBoard(t *testing.T) {
	h := newHistoryDiscord()
	h.seedMessage("legacy", "gone", sweepBotID, AutoLeaderboardTitle)
	_ = h.fakeDiscord.ChannelMessageDelete("legacy", "gone") // an admin deleted it
	var saved []string
	panel := NewLeaderboardPanel(h, "legacy", "gone", DefaultLeaderboardConfig())
	s := NewLeaderboardScheduler(panel, newFixtureStats(), 7, DefaultLeaderboardConfig(), func(id string) { saved = append(saved, id) })
	if err := s.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("a deleted board must be re-posted, not fail every refresh: %v", err)
	}
	if len(saved) != 1 || saved[0] == "gone" || len(h.liveIDs("legacy")) != 1 {
		t.Fatalf("expected one fresh board persisted, saved=%v live=%v", saved, h.liveIDs("legacy"))
	}
	// Transient errors are still errors (no duplicate board on a 5xx).
	h.fakeDiscord.editErr["legacy/"+saved[0]] = errors.New("503")
	s.panel.lastHash = ""
	if err := s.RefreshOnce(context.Background()); err == nil || len(h.liveIDs("legacy")) != 1 {
		t.Fatal("a transient edit failure must not post a second board")
	}
}

type clearingSetupStore struct {
	*InMemorySetupStore
	cleared []string
}

func (c *clearingSetupStore) ClearMessageIDs(_ string, cols ...string) error {
	c.cleared = append(c.cleared, cols...)
	return nil
}

func TestLegacyRetireDurablyClearsTheMessageID(t *testing.T) {
	store := &clearingSetupStore{InMemorySetupStore: NewInMemorySetupStore()}
	_ = store.Save(GuildSetup{GuildID: "g1", LeaderboardsChannelID: "lb", LeaderboardMessageID: "m1", LinkPanelMessageID: "keep"})
	NewLegacyLeaderboardRetirer(newFakeDiscord(), store, "g1")()
	if !equalStrings(store.cleared, []string{"leaderboard_message_id"}) {
		t.Fatalf("expected only leaderboard_message_id cleared, got %v", store.cleared)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
