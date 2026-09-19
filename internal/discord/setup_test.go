package discord

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// fakeGuildAPI is an in-memory GuildAPI for setup tests.
type fakeGuildAPI struct {
	channels  []*discordgo.Channel
	nextID    int
	createErr map[string]error
}

func newFakeGuildAPI() *fakeGuildAPI {
	return &fakeGuildAPI{createErr: map[string]error{}}
}

func (f *fakeGuildAPI) GuildChannels(guildID string) ([]*discordgo.Channel, error) {
	return f.channels, nil
}

func (f *fakeGuildAPI) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData) (*discordgo.Channel, error) {
	if err, ok := f.createErr[data.Name]; ok {
		return nil, err
	}
	f.nextID++
	ch := &discordgo.Channel{
		ID:       "ch-" + itoa(f.nextID), // unique per creation
		Name:     data.Name,
		Type:     data.Type,
		ParentID: data.ParentID,
		GuildID:  guildID,
	}
	f.channels = append(f.channels, ch)
	return ch, nil
}

func itoa(n int) string {
	return strings.TrimSpace(fmt.Sprintf("%d", n))
}

func (f *fakeGuildAPI) ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return &discordgo.Message{ID: "msg-" + channelID, ChannelID: channelID}, nil
}

func (f *fakeGuildAPI) ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	f.nextID++
	return &discordgo.Message{ID: "msg-" + itoa(f.nextID), ChannelID: channelID}, nil
}

func (f *fakeGuildAPI) ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent) (*discordgo.Message, error) {
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeGuildAPI) ChannelMessage(channelID, messageID string) (*discordgo.Message, error) {
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeGuildAPI) Channel(channelID string) (*discordgo.Channel, error) {
	for _, ch := range f.channels {
		if ch.ID == channelID {
			return ch, nil
		}
	}
	return nil, errNotFound
}

func (f *fakeGuildAPI) ChannelEdit(channelID string, data *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	for _, ch := range f.channels {
		if ch.ID == channelID {
			if data.Name != "" {
				ch.Name = data.Name
			}
			return ch, nil
		}
	}
	return nil, errNotFound
}

type errString string

func (e errString) Error() string { return string(e) }

var errNotFound = errString("not found")

func TestSetupCreatesStructureOnce(t *testing.T) {
	api := newFakeGuildAPI()
	store := NewInMemorySetupStore()
	m := NewSetupManager(api, store, "bot-1")

	setup, report, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if setup.CategoryID == "" || setup.KillfeedChannelID == "" || setup.OnlinePlayersChannelID == "" {
		t.Fatalf("expected category and channel IDs stored, got %+v", setup)
	}
	if !report.CategoryCreated {
		t.Fatal("expected category to be created on first run")
	}

	// Second run: idempotent, nothing new created.
	channelsBefore := len(api.channels)
	setup2, report2, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("second setup failed: %v", err)
	}
	if len(api.channels) != channelsBefore {
		t.Fatalf("expected no new channels on duplicate setup, had %d now %d", channelsBefore, len(api.channels))
	}
	if setup2.CategoryID != setup.CategoryID {
		t.Fatal("expected same category ID on duplicate setup")
	}
	if len(report2.Created) != 0 {
		t.Fatalf("expected 0 created on duplicate, got %v", report2.Created)
	}
}

func TestSetupRepairsOnlyMissingChannel(t *testing.T) {
	api := newFakeGuildAPI()
	store := NewInMemorySetupStore()
	m := NewSetupManager(api, store, "bot-1")

	setup, _, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Simulate the killfeed channel being manually deleted.
	killfeedID := setup.KillfeedChannelID
	filtered := api.channels[:0]
	for _, ch := range api.channels {
		if ch.ID != killfeedID {
			filtered = append(filtered, ch)
		}
	}
	api.channels = filtered

	setup2, report, err := m.EnsureConfigured("g1")
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if setup2.KillfeedChannelID == killfeedID {
		t.Fatal("expected a new killfeed channel ID after repair")
	}
	if setup2.KillfeedChannelID == "" {
		t.Fatal("expected killfeed channel to be recreated")
	}
	if len(report.Repaired) == 0 {
		t.Fatal("expected repair to report a recreated resource")
	}
	// Category and other channels should NOT be recreated.
	if report.CategoryCreated {
		t.Fatal("category must not be recreated during repair")
	}
}

func TestSetupStoreGuildKeyed(t *testing.T) {
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: "kf-1"})
	_ = store.Save(GuildSetup{GuildID: "g2", KillfeedChannelID: "kf-2"})

	a, _ := store.Get("g1")
	b, _ := store.Get("g2")
	if a.KillfeedChannelID != "kf-1" || b.KillfeedChannelID != "kf-2" {
		t.Fatalf("expected guild-keyed isolation, got %q and %q", a.KillfeedChannelID, b.KillfeedChannelID)
	}

	missing, err := store.Get("nope")
	if err != nil || missing != nil {
		t.Fatalf("expected nil for unconfigured guild, got %+v err=%v", missing, err)
	}
}

func TestOnlinePlayersEmbedCapsLargeLists(t *testing.T) {
	names := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		names = append(names, "Player"+string(rune('A'+i%26)))
	}
	embed := OnlinePlayersEmbed(names, true)
	if embed == nil {
		t.Fatal("expected an embed")
	}
	// Must not exceed Discord embed description limit (4096 chars).
	if len(embed.Description) > 4000 {
		t.Fatalf("embed description too large: %d", len(embed.Description))
	}
	if maxPlayersListed >= 100 {
		t.Fatal("test requires fewer listed than total to trigger the cap")
	}
}

func TestOnlinePlayersPanelEditsInsteadOfResending(t *testing.T) {
	editor := newRecordingEditor()
	panel := NewOnlinePlayersPanel(editor, "ch-1", "")
	panel.debounce = 0 // fire the debounce timer immediately

	// MarkDirty renders on the debounce timer's goroutine; wait for that render
	// via the editor's signal instead of racing it with a second, direct render.
	panel.MarkDirty([]string{"A", "B"}, true)
	editor.waitForCall(t)
	if sends, edits := editor.counts(); sends != 1 || edits != 0 {
		t.Fatalf("expected 1 initial send and 0 edits, got %d sends %d edits", sends, edits)
	}

	// Change state -> should edit the existing message, not resend.
	panel.MarkDirty([]string{"A", "B", "C"}, true)
	editor.waitForCall(t)
	if sends, edits := editor.counts(); sends != 1 || edits != 1 {
		t.Fatalf("expected 1 send and 1 edit after update, got %d sends %d edits", sends, edits)
	}
}

// Overlapping renders (a timer callback that fired just before a newer one)
// must never both see an empty message id and post two persistent messages.
func TestOnlinePlayersPanelConcurrentRendersSendOnce(t *testing.T) {
	editor := newRecordingEditor()
	panel := NewOnlinePlayersPanel(editor, "ch-1", "")

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			panel.render([]string{"A", "B"}, true)
		}()
	}
	wg.Wait()

	if sends, edits := editor.counts(); sends != 1 || edits != 0 {
		t.Fatalf("expected exactly 1 send and no edits for identical content, got %d sends %d edits", sends, edits)
	}
}

// recordingEditor counts sends/edits under a mutex (renders run on timer
// goroutines) and signals every call so tests can wait for it deterministically.
type recordingEditor struct {
	mu     sync.Mutex
	sends  int
	edits  int
	last   *discordgo.MessageEmbed
	called chan struct{}
}

func newRecordingEditor() *recordingEditor {
	return &recordingEditor{called: make(chan struct{}, 16)}
}

func (r *recordingEditor) counts() (sends, edits int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sends, r.edits
}

// waitForCall blocks until the next send/edit has been recorded.
func (r *recordingEditor) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-r.called:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the panel to render")
	}
}

func (r *recordingEditor) ChannelMessageSendEmbed(channelID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error) {
	r.mu.Lock()
	r.sends++
	r.last = embed
	r.mu.Unlock()
	r.called <- struct{}{}
	return &discordgo.Message{ID: "m1", ChannelID: channelID}, nil
}

func (r *recordingEditor) ChannelMessageEditEmbed(channelID, messageID string, embed *discordgo.MessageEmbed) (*discordgo.Message, error) {
	r.mu.Lock()
	r.edits++
	r.last = embed
	r.mu.Unlock()
	r.called <- struct{}{}
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}
