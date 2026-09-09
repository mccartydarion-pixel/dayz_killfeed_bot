package killfeed

import (
	"context"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

type contextStore struct{ event *Event }

func (s *contextStore) UpsertPlayer(context.Context, int64, string, string, time.Time) (int64, error) {
	return 1, nil
}
func (s *contextStore) InsertKill(context.Context, repository.KillRecord) error   { return nil }
func (s *contextStore) InsertDeath(context.Context, repository.DeathRecord) error { return nil }
func TestQueueStampsContextBeforeProcessing(t *testing.T) {
	store := &contextStore{}
	q := NewPersistenceQueueWithServerID(store, 11, 22, "session")
	ev := &Event{Type: EventPlayerConnect, Player: &PlayerRef{ID: "p", Name: "Player"}}
	if !q.Enqueue(ev) {
		t.Fatal("enqueue failed")
	}
	if ev.GuildID != 11 || ev.ServerID != 22 || ev.SessionID != "session" {
		t.Fatalf("context not stamped: %+v", ev)
	}
}
