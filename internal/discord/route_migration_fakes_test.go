package discord

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// rk identifies a route lookup exactly as the runtime does: (guild row, server, key).
type rk struct {
	guild, server int64
	key           string
}

// keyedResolver stands in for routing.Resolver. Lookups are keyed by the full
// (guild, server, route key), so a test that resolves by guild alone, or for
// the wrong server, gets "not found" - exactly like the real SQL.
type keyedResolver struct {
	mu     sync.Mutex
	routes map[rk]string
	errs   map[rk]error
	calls  int
}

func newKeyedResolver() *keyedResolver {
	return &keyedResolver{routes: map[rk]string{}, errs: map[rk]error{}}
}

func (r *keyedResolver) set(guild, server int64, key, channel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[rk{guild, server, key}] = channel
}

func (r *keyedResolver) clear(guild, server int64, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, rk{guild, server, key})
}

func (r *keyedResolver) fail(guild, server int64, key string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.errs, rk{guild, server, key})
		return
	}
	r.errs[rk{guild, server, key}] = err
}

func (r *keyedResolver) Resolve(ctx context.Context, guildRowID, serverID int64, routeKey string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	k := rk{guildRowID, serverID, routeKey}
	if err := r.errs[k]; err != nil {
		return "", false, err
	}
	ch, ok := r.routes[k]
	return ch, ok, nil
}

// unknownMessageErr is what Discord returns for a deleted message.
func unknownMessageErr() error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage, Message: "Unknown Message"},
	}
}

// fakeDiscord is an in-memory message board implementing every Discord surface
// the migrated publishers use (RoutePanelAPI, MessageEditor, messageDeleter).
type fakeDiscord struct {
	mu       sync.Mutex
	next     int
	messages map[string]map[string]bool // channel -> message id -> alive
	sends    []string                   // "channel/id"
	edits    []string
	deletes  []string
	// editErr / getErr, when set for "channel/id", is returned instead of acting.
	editErr map[string]error
	getErr  map[string]error
	sendErr map[string]error // by channel
}

func newFakeDiscord() *fakeDiscord {
	return &fakeDiscord{messages: map[string]map[string]bool{}, editErr: map[string]error{}, getErr: map[string]error{}, sendErr: map[string]error{}}
}

func (f *fakeDiscord) send(channelID string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.sendErr[channelID]; err != nil {
		return nil, err
	}
	f.next++
	id := fmt.Sprintf("m%d", f.next)
	if f.messages[channelID] == nil {
		f.messages[channelID] = map[string]bool{}
	}
	f.messages[channelID][id] = true
	f.sends = append(f.sends, channelID+"/"+id)
	return &discordgo.Message{ID: id, ChannelID: channelID}, nil
}

func (f *fakeDiscord) edit(channelID, messageID string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := channelID + "/" + messageID
	if err := f.editErr[key]; err != nil {
		return nil, err
	}
	if !f.messages[channelID][messageID] {
		return nil, unknownMessageErr() // a message id that is not in that channel
	}
	f.edits = append(f.edits, key)
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeDiscord) ChannelMessageSendComplex(channelID string, _ *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return f.send(channelID)
}

func (f *fakeDiscord) ChannelMessageEditComplex(channelID, messageID string, _ *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	return f.edit(channelID, messageID)
}

func (f *fakeDiscord) ChannelMessageSendEmbed(channelID string, _ *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return f.send(channelID)
}

func (f *fakeDiscord) ChannelMessageEditEmbed(channelID, messageID string, _ *discordgo.MessageEmbed) (*discordgo.Message, error) {
	return f.edit(channelID, messageID)
}

func (f *fakeDiscord) ChannelMessage(channelID, messageID string) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.getErr[channelID+"/"+messageID]; err != nil {
		return nil, err
	}
	if !f.messages[channelID][messageID] {
		return nil, unknownMessageErr()
	}
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeDiscord) ChannelMessageDelete(channelID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, channelID+"/"+messageID)
	if !f.messages[channelID][messageID] {
		return unknownMessageErr()
	}
	delete(f.messages[channelID], messageID)
	return nil
}

// seed places an already-live message (e.g. a legacy panel) on the board.
func (f *fakeDiscord) seed(channelID, messageID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.messages[channelID] == nil {
		f.messages[channelID] = map[string]bool{}
	}
	f.messages[channelID][messageID] = true
}

// live returns the sorted "channel/id" list of messages still on the board.
func (f *fakeDiscord) live() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for ch, ids := range f.messages {
		for id, alive := range ids {
			if alive {
				out = append(out, ch+"/"+id)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeDiscord) liveIn(channelID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, alive := range f.messages[channelID] {
		if alive {
			n++
		}
	}
	return n
}

func (f *fakeDiscord) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

// memPanelStore is an in-memory RoutePanelStore.
type memPanelStore struct {
	mu      sync.Mutex
	rows    map[panelRow]string // row -> message id
	upErr   error
	listErr error
}

type panelRow struct {
	guild          int64
	route, channel string
}

func newMemPanelStore() *memPanelStore { return &memPanelStore{rows: map[panelRow]string{}} }

func (s *memPanelStore) List(_ context.Context, guildRowID int64, routeKey string) ([]RoutePanelMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []RoutePanelMessage
	for row, msg := range s.rows {
		if row.guild == guildRowID && row.route == routeKey {
			out = append(out, RoutePanelMessage{ChannelID: row.channel, MessageID: msg})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out, nil
}

func (s *memPanelStore) Upsert(_ context.Context, guildRowID int64, routeKey, channelID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upErr != nil {
		return s.upErr
	}
	s.rows[panelRow{guildRowID, routeKey, channelID}] = messageID
	return nil
}

func (s *memPanelStore) Delete(_ context.Context, guildRowID int64, routeKey, channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, panelRow{guildRowID, routeKey, channelID})
	return nil
}

func (s *memPanelStore) count(guild int64, route string) int {
	rows, _ := s.List(context.Background(), guild, route)
	return len(rows)
}

func (s *memPanelStore) messageFor(guild int64, route, channel string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[panelRow{guild, route, channel}]
}

// Compile-time proof the repository row type still maps onto the store adapter.
var _ = repository.GuildRoutePanel{}

// routedFixture wires a guild (row 7) with two servers (1 and 2) to a real
// RouteSyncer over the fakes.
type routedFixture struct {
	t        *testing.T
	guild    int64
	servers  []int64
	resolver *keyedResolver
	api      *fakeDiscord
	store    *memPanelStore
	setup    *InMemorySetupStore
	panels   *RoutePanels
	syncer   *RouteSyncer
}

func newRoutedFixture(t *testing.T) *routedFixture {
	t.Helper()
	f := &routedFixture{t: t, guild: 7, servers: []int64{1, 2}, resolver: newKeyedResolver(), api: newFakeDiscord(), store: newMemPanelStore(), setup: NewInMemorySetupStore()}
	f.panels = NewRoutePanels(f.api, f.store)
	f.syncer = NewRouteSyncer(f.resolver, func(context.Context) (int64, []int64, error) { return f.guild, f.servers, nil }, f.panels, f.api, f.setup, "g1")
	return f
}
