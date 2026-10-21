//go:build integration

package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/yourname/dayz-killfeed/internal/routing"
)

// fakeRecruitAPI records the recruitment cards the app posts, edits and deletes.
type fakeRecruitAPI struct {
	mu      sync.Mutex
	next    int
	sent    []string // "channel/message"
	edited  map[string]int
	deleted []string
	embeds  map[string]*discordgo.MessageEmbed
}

func newFakeRecruitAPI() *fakeRecruitAPI {
	return &fakeRecruitAPI{edited: map[string]int{}, embeds: map[string]*discordgo.MessageEmbed{}}
}

func (f *fakeRecruitAPI) ChannelMessageSendComplex(channelID string, embed *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("m%d", f.next)
	f.sent = append(f.sent, channelID+"/"+id)
	f.embeds[id] = embed
	return &discordgo.Message{ID: id, ChannelID: channelID}, nil
}

func (f *fakeRecruitAPI) ChannelMessageEditComplex(channelID, messageID string, embed *discordgo.MessageEmbed, _ []discordgo.MessageComponent) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.embeds[messageID]; !ok {
		return nil, &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage}}
	}
	f.edited[messageID]++
	f.embeds[messageID] = embed
	return &discordgo.Message{ID: messageID, ChannelID: channelID}, nil
}

func (f *fakeRecruitAPI) ChannelMessageDelete(_ string, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.embeds, messageID)
	f.deleted = append(f.deleted, messageID)
	return nil
}

func (f *fakeRecruitAPI) snapshot() (sent []string, edited map[string]int, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.sent...), map[string]int(func() map[string]int {
		m := map[string]int{}
		for k, v := range f.edited {
			m[k] = v
		}
		return m
	}()), append([]string{}, f.deleted...)
}

// OPEN factions are joined instantly; INVITE_ONLY ones take applications; CLOSED take nothing.
func TestFactionJoinAndApplySemantics(t *testing.T) {
	w := newFactionWorld(t)
	leader, fid, fp := w.leaderWithFaction("Open Gates", "OG")
	player, other := w.players[3], w.players[4]

	res := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/join"), player, nil), http.StatusCreated, "join open").JSON(t)
	if res["joined"] != true || res["member"].(map[string]any)["role"] != "MEMBER" {
		t.Fatalf("join: %v", res)
	}
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/join"), player, nil), http.StatusConflict, "already in")
	mine := w.expect(w.do(http.MethodGet, w.path(w.a1, "/me"), player, nil), http.StatusOK, "me").JSON(t)
	if mine["faction"] == nil || int64(mine["faction"].(map[string]any)["id"].(float64)) != fid {
		t.Fatalf("member after join: %v", mine)
	}

	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"recruitmentStatus": "INVITE_ONLY"}), http.StatusOK, "invite only")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/join"), other, nil), http.StatusConflict, "no instant join on invite-only")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), other, map[string]any{"message": "let me in"}), http.StatusCreated, "apply on invite-only")

	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"recruitmentStatus": "CLOSED"}), http.StatusOK, "closed")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/join"), w.players[5], nil), http.StatusConflict, "closed: no join")
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/applications"), w.players[5], nil), http.StatusConflict, "closed: no apply")
}

// The recruitment card: needs a routed channel, is posted once, edited in place on changes,
// re-posted when its message vanished, and deleted on unpublish and on dissolve.
func TestFactionRecruitCardLifecycle(t *testing.T) {
	w := newFactionWorld(t)
	api := newFakeRecruitAPI()
	w.a.factionRecruitAPI = api
	w.a.ChannelRoutes = routing.NewResolver(w.a.SaaSChannelRoutes, 0)
	leader, fid, fp := w.leaderWithFaction("Card Carriers", "CC")
	officerless := w.players[3]

	// Without a routed channel the publish is refused with a clear conflict.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/recruit"), leader, nil), http.StatusConflict, "no channel yet")
	// Only leader/officers may publish.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/recruit"), officerless, nil), http.StatusForbidden, "outsider")

	if err := w.a.SaaSChannelRoutes.UpsertRoute(context.Background(), w.a1.OrgID, w.a1.InstallationID, "FACTION_RECRUITMENT", "chan-recruit", true); err != nil {
		t.Fatal(err)
	}
	w.a.ChannelRoutes.InvalidateAll()
	res := w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/recruit"), leader, nil), http.StatusOK, "publish").JSON(t)
	post := res["recruitPost"].(map[string]any)
	if post["channelId"] != "chan-recruit" || post["messageId"] != "m1" {
		t.Fatalf("post: %v", post)
	}
	if sent, _, _ := api.snapshot(); len(sent) != 1 || sent[0] != "chan-recruit/m1" {
		t.Fatalf("sent: %v", sent)
	}
	profile := w.expect(w.do(http.MethodGet, w.path(w.a1, fp), leader, nil), http.StatusOK, "profile").JSON(t)
	if profile["recruitPost"] == nil || profile["recruitPost"].(map[string]any)["messageId"] != "m1" {
		t.Fatalf("profile carries the card: %v", profile["recruitPost"])
	}

	// Publishing again edits the same message rather than posting a second card.
	w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/recruit"), leader, nil), http.StatusOK, "re-publish")
	if sent, edited, _ := api.snapshot(); len(sent) != 1 || edited["m1"] != 1 {
		t.Fatalf("re-publish edits in place: sent=%v edited=%v", sent, edited)
	}

	// A faction change refreshes the card (off the request path).
	w.expect(w.do(http.MethodPut, w.path(w.a1, fp), leader, map[string]any{"recruitmentStatus": "INVITE_ONLY"}), http.StatusOK, "update")
	waitFor(t, func() bool { _, edited, _ := api.snapshot(); return edited["m1"] >= 2 })
	api.mu.Lock()
	desc := api.embeds["m1"].Description
	api.mu.Unlock()
	if !containsAll(desc, "**Apply**") {
		t.Fatalf("refreshed card reflects invite-only: %q", desc)
	}

	// A card whose message was deleted in Discord is posted again on the next publish.
	api.ChannelMessageDelete("chan-recruit", "m1")
	res = w.expect(w.do(http.MethodPost, w.path(w.a1, fp+"/recruit"), leader, nil), http.StatusOK, "re-post").JSON(t)
	if res["recruitPost"].(map[string]any)["messageId"] != "m2" {
		t.Fatalf("re-post: %v", res)
	}

	// Unpublish deletes the message and forgets the card.
	w.expect(w.do(http.MethodDelete, w.path(w.a1, fp+"/recruit"), leader, nil), http.StatusOK, "unpublish")
	if _, _, deleted := api.snapshot(); deleted[len(deleted)-1] != "m2" {
		t.Fatalf("unpublish deletes: %v", deleted)
	}
	if p, _ := w.a.FactionHub.RecruitPost(context.Background(), w.a1.InstallationID, fid); p != nil {
		t.Fatalf("card forgotten: %+v", p)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
