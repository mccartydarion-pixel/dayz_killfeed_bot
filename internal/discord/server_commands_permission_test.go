package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// countingGuilds fails the test's expectations by counting any lookup: a
// non-admin's /server autocomplete must not reach the guild's Nitrado token.
type countingGuilds struct{ lookups int }

func (g *countingGuilds) UpsertGuild(context.Context, repository.GuildRecord) (int64, error) {
	return 0, nil
}
func (g *countingGuilds) GetGuild(context.Context, string) (*repository.GuildRecord, int64, error) {
	g.lookups++
	return nil, 0, nil
}

func TestServerAutocompleteIsAdminOnly(t *testing.T) {
	s, rec := recordingSession(t)
	guilds := &countingGuilds{}
	h := NewServerCommandHandler(&repository.ServerRepository{}, guilds, nil, nil)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "901", AppID: "app", Token: "tok", GuildID: "g", ChannelID: "c",
		Type:   discordgo.InteractionApplicationCommandAutocomplete,
		Member: &discordgo.Member{User: &discordgo.User{ID: "player"}, Permissions: discordgo.PermissionSendMessages},
		Data: discordgo.ApplicationCommandInteractionData{Name: "server", Options: []*discordgo.ApplicationCommandInteractionDataOption{
			{Name: "select", Type: discordgo.ApplicationCommandOptionSubCommand, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "service_id", Type: discordgo.ApplicationCommandOptionString, Value: "1", Focused: true}}},
		}},
	}}
	h.Handle(s, i)
	if guilds.lookups != 0 {
		t.Fatalf("a non-admin's autocomplete looked up the guild's Nitrado connection")
	}
	var answers int
	for _, r := range rec.reqs {
		if r.method == http.MethodPost && strings.HasSuffix(r.path, "/callback") {
			answers++
			if data, _ := r.body["data"].(map[string]any); data["choices"] != nil && len(data["choices"].([]any)) != 0 {
				t.Fatalf("non-admin got suggestions: %v", data)
			}
		}
	}
	if answers != 1 {
		t.Fatalf("want one empty autocomplete answer, got requests %+v", rec.reqs)
	}
}

func TestServerCommandIsHiddenFromMembers(t *testing.T) {
	batch := NewCommandBatch("app")
	if err := RegisterServerCommands(batch, "g"); err != nil {
		t.Fatal(err)
	}
	cmd := batch.Commands()[0]
	if cmd.DefaultMemberPermissions == nil || *cmd.DefaultMemberPermissions&discordgo.PermissionManageServer == 0 {
		t.Fatalf("/server is visible to every member: %+v", cmd.DefaultMemberPermissions)
	}
}
