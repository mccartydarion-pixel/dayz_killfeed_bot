package discord

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// withFakeChannelsEndpoint redirects discordgo's guild/channels/member REST
// calls to a local test server for the duration of the test - all three
// (EndpointGuild, EndpointGuildChannels, EndpointGuildMember) are built from
// the single exported discordgo.EndpointGuilds var (endpoints.go), so
// overriding it redirects all of them at once, the same technique
// withFakeGuildEndpoint (client_test.go) uses for just the guild endpoint.
func withFakeChannelsEndpoint(t *testing.T, onGuild, onChannels, onMember http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/members/@me"):
			if onMember == nil {
				t.Fatalf("unexpected member REST call: %s", r.URL.Path)
			}
			onMember(w, r)
		case strings.HasSuffix(r.URL.Path, "/channels"):
			if onChannels == nil {
				t.Fatalf("unexpected channels REST call: %s", r.URL.Path)
			}
			onChannels(w, r)
		default:
			if onGuild == nil {
				t.Fatalf("unexpected guild REST call: %s", r.URL.Path)
			}
			onGuild(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	original := discordgo.EndpointGuilds
	discordgo.EndpointGuilds = srv.URL + "/guilds/"
	t.Cleanup(func() { discordgo.EndpointGuilds = original })
}

func newTestClientWithGuild(t *testing.T, guild *discordgo.Guild) *Client {
	t.Helper()
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.session.State.User = &discordgo.User{ID: "bot-id"}
	if guild != nil {
		if err := client.session.State.GuildAdd(guild); err != nil {
			t.Fatal(err)
		}
	}
	return client
}

func everyoneRole(guildID string) *discordgo.Role {
	return &discordgo.Role{ID: guildID, Permissions: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages}
}

// TestListGuildChannelsUsesStateCacheOnly is the fully-cached case (section
// 4): guild, channels, and the bot's own member are all already in state -
// zero REST calls.
func TestListGuildChannelsUsesStateCacheOnly(t *testing.T) {
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{everyoneRole("guild-1")},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
		Channels: []*discordgo.Channel{
			{ID: "c-text", GuildID: "guild-1", Name: "general", Type: discordgo.ChannelTypeGuildText, Position: 1},
			{ID: "c-voice", GuildID: "guild-1", Name: "Lobby", Type: discordgo.ChannelTypeGuildVoice, Position: 0},
		},
	}
	client := newTestClientWithGuild(t, guild)

	// No handlers registered at all - any REST call fails the test via
	// withFakeChannelsEndpoint's own t.Fatal on an unexpected request.
	withFakeChannelsEndpoint(t, nil, nil, nil)

	channels, err := client.ListGuildChannels("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].ID != "c-text" {
		t.Fatalf("expected only the text channel, got %+v", channels)
	}
}

// TestListGuildChannelsFallsBackToRESTWhenGuildNotCached covers section 4's
// "if channel state is unavailable/stale, use Discord REST fallback": a
// guild the bot's local cache hasn't seen yet still returns a normalized
// channel list, sourced from a single guild fetch + a single channels fetch
// + a single self-member fetch (never one REST call per channel).
func TestListGuildChannelsFallsBackToRESTWhenGuildNotCached(t *testing.T) {
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.session.State.User = &discordgo.User{ID: "bot-id"}

	guildCalls, channelsCalls, memberCalls := 0, 0, 0
	withFakeChannelsEndpoint(t,
		func(w http.ResponseWriter, r *http.Request) {
			guildCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"rest-guild","owner_id":"someone-else","roles":[{"id":"rest-guild","permissions":"3072"}]}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			channelsCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"c-text","guild_id":"rest-guild","name":"general","type":0,"position":0}]`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			memberCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user":{"id":"bot-id"},"roles":[]}`))
		},
	)

	channels, err := client.ListGuildChannels("rest-guild")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].ID != "c-text" {
		t.Fatalf("expected the REST-fallback channel list, got %+v", channels)
	}
	if guildCalls != 1 || channelsCalls != 1 || memberCalls != 1 {
		t.Fatalf("expected exactly one call to each endpoint, got guild=%d channels=%d member=%d", guildCalls, channelsCalls, memberCalls)
	}
}

// TestListGuildChannelsExcludesVoiceCategoryAndUnviewable covers section 2/5:
// voice/category channels are dropped regardless of permissions, and a text
// channel the bot cannot view is dropped too.
func TestListGuildChannelsExcludesVoiceCategoryAndUnviewable(t *testing.T) {
	everyone := &discordgo.Role{ID: "guild-1", Permissions: discordgo.PermissionSendMessages} // no View Channel at @everyone
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{everyone},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
		Channels: []*discordgo.Channel{
			{ID: "c-text-visible", GuildID: "guild-1", Name: "general", Type: discordgo.ChannelTypeGuildText, Position: 1,
				PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "bot-id", Type: discordgo.PermissionOverwriteTypeMember, Allow: discordgo.PermissionViewChannel}}},
			{ID: "c-text-hidden", GuildID: "guild-1", Name: "secret", Type: discordgo.ChannelTypeGuildText, Position: 2},
			{ID: "c-category", GuildID: "guild-1", Name: "Category", Type: discordgo.ChannelTypeGuildCategory, Position: 0},
			{ID: "c-voice", GuildID: "guild-1", Name: "Voice", Type: discordgo.ChannelTypeGuildVoice, Position: 3},
		},
	}
	client := newTestClientWithGuild(t, guild)

	channels, err := client.ListGuildChannels("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].ID != "c-text-visible" {
		t.Fatalf("expected only the visible text channel, got %+v", channels)
	}
}

// TestListGuildChannelsSortsByPositionThenName covers the documented sort
// order (section 4).
func TestListGuildChannelsSortsByPositionThenName(t *testing.T) {
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{everyoneRole("guild-1")},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
		Channels: []*discordgo.Channel{
			{ID: "c-b", GuildID: "guild-1", Name: "b-channel", Type: discordgo.ChannelTypeGuildText, Position: 1},
			{ID: "c-a", GuildID: "guild-1", Name: "a-channel", Type: discordgo.ChannelTypeGuildText, Position: 1},
			{ID: "c-first", GuildID: "guild-1", Name: "z-first", Type: discordgo.ChannelTypeGuildText, Position: 0},
		},
	}
	client := newTestClientWithGuild(t, guild)

	channels, err := client.ListGuildChannels("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 3 {
		t.Fatalf("expected 3 channels, got %+v", channels)
	}
	gotOrder := []string{channels[0].ID, channels[1].ID, channels[2].ID}
	wantOrder := []string{"c-first", "c-a", "c-b"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("expected order %v, got %v", wantOrder, gotOrder)
		}
	}
}

// TestListGuildChannelsMarksCanSendFromSendMessagesPermission covers the
// canSend boolean (section 5): view-only access must report canSend=false.
func TestListGuildChannelsMarksCanSendFromSendMessagesPermission(t *testing.T) {
	everyone := &discordgo.Role{ID: "guild-1", Permissions: discordgo.PermissionViewChannel}
	guild := &discordgo.Guild{
		ID:       "guild-1",
		OwnerID:  "someone-else",
		Roles:    []*discordgo.Role{everyone},
		Members:  []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
		Channels: []*discordgo.Channel{{ID: "c-view-only", GuildID: "guild-1", Name: "announcements", Type: discordgo.ChannelTypeGuildText, Position: 0}},
	}
	client := newTestClientWithGuild(t, guild)

	channels, err := client.ListGuildChannels("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 {
		t.Fatalf("expected the view-only channel to still be listed, got %+v", channels)
	}
	if channels[0].CanSend {
		t.Fatal("expected canSend=false without Send Messages permission")
	}
}

// TestListGuildChannelsNilAndEmptyGuildID covers the defensive guards.
func TestListGuildChannelsNilAndEmptyGuildID(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.ListGuildChannels("guild-1"); err == nil {
		t.Fatal("expected an error for a nil client")
	}
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListGuildChannels(""); err == nil {
		t.Fatal("expected an error for an empty guild id")
	}
}
