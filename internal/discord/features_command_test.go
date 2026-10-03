package discord

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeFeaturesStore struct {
	rowID    int64
	category string
	channel  string
	setErr   error
	sets     []string
}

func (f *fakeFeaturesStore) GetGuild(context.Context, string) (*repository.GuildRecord, int64, error) {
	if f.rowID == 0 {
		return nil, 0, nil
	}
	return &repository.GuildRecord{CategoryID: f.category}, f.rowID, nil
}
func (f *fakeFeaturesStore) FeaturesChannel(context.Context, string) (string, error) {
	return f.channel, nil
}
func (f *fakeFeaturesStore) SetFeaturesChannel(_ context.Context, _ string, id string) error {
	f.sets = append(f.sets, id)
	if f.setErr != nil {
		return f.setErr
	}
	f.channel = id
	return nil
}

type fakeFeaturesDiscord struct {
	channels  map[string]*discordgo.Channel
	created   []discordgo.GuildChannelCreateData
	deleted   []string
	sent      []*discordgo.MessageSend
	createErr func(data discordgo.GuildChannelCreateData) error
	sendErr   error
	lookupErr error
}

func (f *fakeFeaturesDiscord) Channel(id string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if ch, ok := f.channels[id]; ok {
		return ch, nil
	}
	return nil, &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}}
}
func (f *fakeFeaturesDiscord) GuildChannelCreateComplex(guildID string, data discordgo.GuildChannelCreateData, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.created = append(f.created, data)
	if f.createErr != nil {
		if err := f.createErr(data); err != nil {
			return nil, err
		}
	}
	ch := &discordgo.Channel{ID: "900", GuildID: guildID, Name: data.Name}
	f.channels[ch.ID] = ch
	return ch, nil
}
func (f *fakeFeaturesDiscord) ChannelDelete(id string, _ ...discordgo.RequestOption) (*discordgo.Channel, error) {
	f.deleted = append(f.deleted, id)
	delete(f.channels, id)
	return nil, nil
}
func (f *fakeFeaturesDiscord) ChannelMessageSendComplex(_ string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent = append(f.sent, data)
	return &discordgo.Message{}, nil
}

func newFakeFeaturesDiscord() *fakeFeaturesDiscord {
	return &fakeFeaturesDiscord{channels: map[string]*discordgo.Channel{}}
}

func TestFeaturesToggleCreatesThenRemoves(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, category: "cat"}
	api := newFakeFeaturesDiscord()
	h := NewFeaturesCommandHandler(store)

	reply := h.toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "created") || store.channel != "900" {
		t.Fatalf("first toggle: reply %q, channel %q", reply, store.channel)
	}
	if len(api.created) != 1 || api.created[0].ParentID != "cat" || api.created[0].Name != FeaturesChannelName {
		t.Fatalf("created %+v", api.created)
	}
	if len(api.sent) != len(featuresGuideMessages()) {
		t.Fatalf("sent %d messages, want %d", len(api.sent), len(featuresGuideMessages()))
	}
	everyone := api.created[0].PermissionOverwrites[0]
	if everyone.ID != "g1" || everyone.Deny&discordgo.PermissionSendMessages == 0 || everyone.Allow&discordgo.PermissionViewChannel == 0 {
		t.Fatalf("channel is not read-only for everyone: %+v", everyone)
	}

	reply = h.toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "removed") || store.channel != "" || len(api.deleted) != 1 || api.deleted[0] != "900" {
		t.Fatalf("second toggle: reply %q, channel %q, deleted %v", reply, store.channel, api.deleted)
	}
}

func TestFeaturesToggleRecreatesWhenChannelWasDeletedByHand(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, channel: "gone"}
	api := newFakeFeaturesDiscord()
	reply := NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "created") || len(api.deleted) != 0 || store.channel != "900" {
		t.Fatalf("reply %q, deleted %v, channel %q", reply, api.deleted, store.channel)
	}
}

func TestFeaturesToggleNeverDeletesAChannelInAnotherServer(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, channel: "other"}
	api := newFakeFeaturesDiscord()
	api.channels["other"] = &discordgo.Channel{ID: "other", GuildID: "g2"}
	NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if len(api.deleted) != 0 {
		t.Fatalf("deleted %v", api.deleted)
	}
}

func TestFeaturesToggleLeavesSettingAloneWhenDiscordIsDown(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, channel: "900"}
	api := newFakeFeaturesDiscord()
	api.lookupErr = &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusBadGateway}}
	reply := NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "Could not check") || len(api.created) != 0 || len(store.sets) != 0 {
		t.Fatalf("reply %q, created %d, sets %v", reply, len(api.created), store.sets)
	}
}

func TestFeaturesToggleFallsBackWhenCategoryIsGone(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, category: "deleted"}
	api := newFakeFeaturesDiscord()
	api.createErr = func(data discordgo.GuildChannelCreateData) error {
		if data.ParentID != "" {
			return errors.New("unknown parent")
		}
		return nil
	}
	reply := NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "created") || len(api.created) != 2 || api.created[1].ParentID != "" {
		t.Fatalf("reply %q, created %+v", reply, api.created)
	}
}

func TestFeaturesToggleCleansUpWhenPostingFails(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1}
	api := newFakeFeaturesDiscord()
	api.sendErr = errors.New("boom")
	reply := NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "Could not post") || store.channel != "" || len(api.deleted) != 1 {
		t.Fatalf("reply %q, channel %q, deleted %v", reply, store.channel, api.deleted)
	}
}

func TestFeaturesToggleCleansUpWhenSaveFails(t *testing.T) {
	store := &fakeFeaturesStore{rowID: 1, setErr: errors.New("db down")}
	api := newFakeFeaturesDiscord()
	reply := NewFeaturesCommandHandler(store).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "Could not save") || len(api.deleted) != 1 || len(api.sent) != 0 {
		t.Fatalf("reply %q, deleted %v, sent %d", reply, api.deleted, len(api.sent))
	}
}

func TestFeaturesToggleNeedsSetup(t *testing.T) {
	api := newFakeFeaturesDiscord()
	reply := NewFeaturesCommandHandler(&fakeFeaturesStore{}).toggle(context.Background(), api, "g1", "bot")
	if !strings.Contains(reply, "/setup") || len(api.created) != 0 {
		t.Fatalf("reply %q", reply)
	}
}

func TestFeaturesGuideFitsDiscordLimits(t *testing.T) {
	total := 0
	for _, msg := range featuresGuideMessages() {
		if len(msg.Embeds) > 10 {
			t.Fatalf("message has %d embeds", len(msg.Embeds))
		}
		size := 0
		for _, e := range msg.Embeds {
			size += embedTextLength(e)
			if len([]rune(e.Title)) > 256 || len([]rune(e.Description)) > 4096 {
				t.Fatalf("embed %q too long", e.Title)
			}
			for _, f := range e.Fields {
				if len([]rune(f.Value)) > 1024 {
					t.Fatalf("field in %q too long", e.Title)
				}
			}
		}
		if size > 6000 {
			t.Fatalf("message is %d characters", size)
		}
		total += len(msg.Embeds)
	}
	if total != len(featuresGuideEmbeds()) {
		t.Fatalf("posted %d embeds, guide has %d", total, len(featuresGuideEmbeds()))
	}
}
