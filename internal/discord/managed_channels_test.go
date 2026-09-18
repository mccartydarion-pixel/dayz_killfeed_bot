package discord

import (
	"io"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// TestListAllGuildChannelsIncludesCategories covers section 4: unlike
// ListGuildChannels, the raw list used for auto-setup's own matching must
// include category channels (and unviewable ones), since the bot is
// orchestrating structure, not offering customer-facing choices.
func TestListAllGuildChannelsIncludesCategories(t *testing.T) {
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{{ID: "guild-1", Permissions: 0}},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
		Channels: []*discordgo.Channel{
			{ID: "cat-1", GuildID: "guild-1", Name: "CHAMPION KILLFEED", Type: discordgo.ChannelTypeGuildCategory, Position: 0},
			{ID: "c-text", GuildID: "guild-1", Name: "champion-killfeed", Type: discordgo.ChannelTypeGuildText, Position: 1, ParentID: "cat-1"},
			{ID: "c-voice", GuildID: "guild-1", Name: "Voice", Type: discordgo.ChannelTypeGuildVoice, Position: 2},
		},
	}
	client := newTestClientWithGuild(t, guild)

	channels, err := client.ListAllGuildChannels("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 3 {
		t.Fatalf("expected all 3 channels including the category and voice channel, got %+v", channels)
	}
	var foundCategory bool
	for _, c := range channels {
		if c.ID == "cat-1" && c.Type == discordgo.ChannelTypeGuildCategory {
			foundCategory = true
		}
	}
	if !foundCategory {
		t.Fatal("expected the category channel to be included")
	}
}

// TestGuildPermissionsComputesFromCachedRoles covers the guild-level
// (no channel overwrite) permission check auto-setup uses to gate category/
// channel creation (section 5).
func TestGuildPermissionsComputesFromCachedRoles(t *testing.T) {
	everyone := &discordgo.Role{ID: "guild-1", Permissions: discordgo.PermissionManageChannels | discordgo.PermissionViewChannel}
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{everyone},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
	}
	client := newTestClientWithGuild(t, guild)

	perms, err := client.GuildPermissions("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if perms&discordgo.PermissionManageChannels == 0 {
		t.Fatalf("expected PermissionManageChannels to be set, got %d", perms)
	}
}

// TestGuildPermissionsWithoutManageChannels covers the negative case (section
// 5): a guild where the bot's only role lacks Manage Channels must report it
// missing.
func TestGuildPermissionsWithoutManageChannels(t *testing.T) {
	everyone := &discordgo.Role{ID: "guild-1", Permissions: discordgo.PermissionViewChannel}
	guild := &discordgo.Guild{
		ID:      "guild-1",
		OwnerID: "someone-else",
		Roles:   []*discordgo.Role{everyone},
		Members: []*discordgo.Member{{GuildID: "guild-1", User: &discordgo.User{ID: "bot-id"}}},
	}
	client := newTestClientWithGuild(t, guild)

	perms, err := client.GuildPermissions("guild-1")
	if err != nil {
		t.Fatal(err)
	}
	if perms&discordgo.PermissionManageChannels != 0 {
		t.Fatal("expected PermissionManageChannels to be absent")
	}
}

// TestCreateGuildCategoryAndTextChannel covers the REST create calls
// (section 2): both hit the guild channels endpoint with the expected type/
// parent.
func TestCreateGuildCategoryAndTextChannel(t *testing.T) {
	client, err := New("test-token")
	if err != nil {
		t.Fatal(err)
	}
	client.session.State.User = &discordgo.User{ID: "bot-id"}

	var lastBody string
	withFakeChannelsEndpoint(t, nil, func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		lastBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"new-channel","name":"champion-killfeed","type":0,"parent_id":"cat-1"}`))
	}, nil)

	cat, err := client.CreateGuildCategory("guild-1", "CHAMPION KILLFEED")
	if err != nil {
		t.Fatal(err)
	}
	if cat.ID != "new-channel" {
		t.Fatalf("expected the created category to be returned, got %+v", cat)
	}

	ch, err := client.CreateGuildTextChannel("guild-1", "champion-killfeed", "cat-1")
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != "new-channel" {
		t.Fatalf("expected the created channel to be returned, got %+v", ch)
	}
	if lastBody == "" {
		t.Fatal("expected a request body to have been sent")
	}
}
