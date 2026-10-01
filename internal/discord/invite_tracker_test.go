package discord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

func TestAttributeInvite(t *testing.T) {
	before := map[string]inviteState{"a": {uses: 3}, "b": {uses: 0}, "last": {uses: 4, maxUses: 5}}
	cases := []struct {
		name  string
		after map[string]inviteState
		want  string
		ok    bool
	}{
		{"one grew", map[string]inviteState{"a": {uses: 4}, "b": {}, "last": {uses: 4, maxUses: 5}}, "a", true},
		{"last use deleted", map[string]inviteState{"a": {uses: 3}, "b": {}}, "last", true},
		{"two grew", map[string]inviteState{"a": {uses: 4}, "b": {uses: 1}, "last": {uses: 4, maxUses: 5}}, "", false},
		{"nothing changed (vanity)", map[string]inviteState{"a": {uses: 3}, "b": {}, "last": {uses: 4, maxUses: 5}}, "", false},
		{"new and used", map[string]inviteState{"a": {uses: 3}, "b": {}, "last": {uses: 4, maxUses: 5}, "new": {uses: 1}}, "new", true},
	}
	for _, c := range cases {
		got, ok := AttributeInvite(before, c.after)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

type fakeInvites struct {
	mu   sync.Mutex
	list []*discordgo.Invite
	err  error
}

func (f *fakeInvites) GuildInvites(string, ...discordgo.RequestOption) ([]*discordgo.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*discordgo.Invite, len(f.list))
	for i, inv := range f.list {
		c := *inv
		out[i] = &c
	}
	return out, f.err
}

type memJoins struct {
	joins  []repository.InviteJoin
	leaves []string
}

func (m *memJoins) RecordInviteJoin(_ context.Context, j repository.InviteJoin) error {
	m.joins = append(m.joins, j)
	return nil
}
func (m *memJoins) RecordInviteLeave(_ context.Context, _, member string, _ time.Time) error {
	m.leaves = append(m.leaves, member)
	return nil
}

func TestInviteTrackerAttributesJoins(t *testing.T) {
	api := &fakeInvites{list: []*discordgo.Invite{{Code: "abc", Uses: 2, Inviter: &discordgo.User{ID: "u1", Username: "recruiter"}}}}
	store := &memJoins{}
	tr := NewInviteTracker(api, store)
	tr.Snapshot("g")
	if ready, _, _ := tr.Status("g"); !ready {
		t.Fatal("snapshot should make tracking ready")
	}
	api.list[0].Uses = 3
	tr.MemberJoined(context.Background(), "g", "m1", false)
	tr.MemberJoined(context.Background(), "g", "bot", true)
	tr.MemberJoined(context.Background(), "g", "m2", false) // nothing changed: vanity or unknown
	tr.MemberLeft(context.Background(), "g", "m1")
	if len(store.joins) != 2 || !store.joins[0].Attributed || store.joins[0].Code != "abc" || store.joins[0].InviterDiscordID != "u1" || store.joins[1].Attributed {
		t.Fatalf("joins: %+v", store.joins)
	}
	if len(store.leaves) != 1 {
		t.Fatalf("leaves: %+v", store.leaves)
	}

	// Without Manage Server: joins are still recorded, unattributed, and the status says why.
	api.err = errors.New("403 Missing Permissions")
	tr.MemberJoined(context.Background(), "g", "m3", false)
	if ready, problem, _ := tr.Status("g"); ready || problem == "" || store.joins[2].Attributed {
		t.Fatalf("expected a not-ready status: %v %q %+v", ready, problem, store.joins[2])
	}
}
