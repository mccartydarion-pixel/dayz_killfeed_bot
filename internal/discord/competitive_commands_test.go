package discord

import (
	"context"
	"testing"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// fakeEventStore is an in-memory EventStore keyed by event ID, each carrying
// its own GuildID, used to prove leaderboard reads never cross guild bounds.
type fakeEventStore struct {
	events            map[int64]repository.CompetitiveEvent
	leaderboardCalled bool
}

func (f *fakeEventStore) GetRecentEvents(ctx context.Context, guildID int64, limit int) ([]repository.CompetitiveEvent, error) {
	var out []repository.CompetitiveEvent
	for _, e := range f.events {
		if e.GuildID == guildID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeEventStore) GetEvent(ctx context.Context, guildID, eventID int64) (*repository.CompetitiveEvent, error) {
	e, ok := f.events[eventID]
	if !ok || e.GuildID != guildID {
		return nil, context.Canceled // any error means "not found for this guild"
	}
	return &e, nil
}

func (f *fakeEventStore) Leaderboard(ctx context.Context, eventID int64, limit int) ([]repository.EventScore, error) {
	f.leaderboardCalled = true
	return []repository.EventScore{{PlayerID: 999, Score: 42}}, nil
}

func TestEventLeaderboardForGuildRefusesCrossGuildEvent(t *testing.T) {
	// Event 501 belongs to guild row 2; the caller is guild row 1.
	store := &fakeEventStore{events: map[int64]repository.CompetitiveEvent{
		501: {ID: 501, GuildID: 2, Name: "Other Guild's Event"},
	}}
	h := &EventCommandHandler{events: store}

	rows, err := h.leaderboardForGuild(context.Background(), 1, 501, 10)
	if err == nil {
		t.Fatal("expected an ownership error for a cross-guild event ID")
	}
	if rows != nil {
		t.Fatal("expected no rows returned for a cross-guild event ID")
	}
	if store.leaderboardCalled {
		t.Fatal("leaderboard must never be fetched for an event belonging to another guild")
	}
}

func TestEventLeaderboardForGuildAllowsOwnGuildEvent(t *testing.T) {
	store := &fakeEventStore{events: map[int64]repository.CompetitiveEvent{
		501: {ID: 501, GuildID: 1, Name: "Our Event"},
	}}
	h := &EventCommandHandler{events: store}

	rows, err := h.leaderboardForGuild(context.Background(), 1, 501, 10)
	if err != nil {
		t.Fatalf("expected success for the owning guild, got: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the fake leaderboard rows to be returned, got %d", len(rows))
	}
	if !store.leaderboardCalled {
		t.Fatal("expected leaderboard to be fetched once ownership is confirmed")
	}
}
