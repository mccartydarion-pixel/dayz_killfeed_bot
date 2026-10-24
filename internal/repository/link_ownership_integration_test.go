//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yourname/dayz-killfeed/internal/linking"
)

// secondPlayer adds another player with observed playtime on f's server.
func secondPlayer(t *testing.T, ctx context.Context, f linkBindingFixture, name string) int64 {
	t.Helper()
	db := saasIntegrationDB(t)
	id, err := NewPlayerRepository(db.Pool).UpsertPlayer(ctx, f.GuildRowID, fmt.Sprintf("dayz-%s-%d", name, time.Now().UnixNano()), name, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	activity := NewActivityRepository(db.Pool)
	start := time.Now().Add(-6 * time.Minute)
	if err := activity.Connect(ctx, f.GuildRowID, f.ServerID, id, start); err != nil {
		t.Fatal(err)
	}
	for at := start.Add(time.Minute); at.Before(time.Now()); at = at.Add(time.Minute) {
		if err := activity.CheckpointConnected(ctx, f.GuildRowID, f.ServerID, at); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// Unlink then /link another player: the link moves to the new player, and the old player's
// disconnect and reconnect change nothing (there is no reconnect step any more).
func TestRelinkPointsAtTheNewPlayer(t *testing.T) {
	ctx := context.Background()
	f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
	otherName := fmt.Sprintf("Other%d", time.Now().UnixNano()%100000)
	other := secondPlayer(t, ctx, f, otherName)
	db := saasIntegrationDB(t)
	links := NewLinkRepository(db.Pool)
	svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
	user := fmt.Sprintf("discord-relink-%d", time.Now().UnixNano())

	if link, err := svc.Request(ctx, f.GuildRowID, user, f.PlayerName); err != nil || link.Status != linking.StatusVerified {
		t.Fatalf("first link: %+v %v", link, err)
	}
	if err := svc.Unlink(ctx, f.GuildRowID, user); err != nil {
		t.Fatal(err)
	}
	if link, err := svc.Request(ctx, f.GuildRowID, user, otherName); err != nil || link.Status != linking.StatusVerified || link.PlayerID != other {
		t.Fatalf("relink: %+v %v", link, err)
	}
	now := time.Now()
	if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	link, err := svc.Status(ctx, f.GuildRowID, user)
	if err != nil || link == nil || link.PlayerID != other || link.Status != linking.StatusVerified {
		t.Fatalf("after the old player reconnected: %+v %v", link, err)
	}
}

// A claim another Discord user gave up (unlinked) does not block the player's owner; a
// VERIFIED one does.
func TestUnlinkedClaimDoesNotBlockTheOwner(t *testing.T) {
	ctx := context.Background()
	f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
	db := saasIntegrationDB(t)
	links := NewLinkRepository(db.Pool)
	svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
	squatter := fmt.Sprintf("discord-sq-%d", time.Now().UnixNano())
	owner := fmt.Sprintf("discord-own-%d", time.Now().UnixNano())

	if _, err := svc.Request(ctx, f.GuildRowID, squatter, f.PlayerName); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Request(ctx, f.GuildRowID, owner, f.PlayerName); err == nil {
		t.Fatal("a VERIFIED link blocks everyone else")
	}
	if err := svc.Unlink(ctx, f.GuildRowID, squatter); err != nil {
		t.Fatal(err)
	}
	if link, err := svc.Request(ctx, f.GuildRowID, owner, f.PlayerName); err != nil || link.Status != linking.StatusVerified {
		t.Fatalf("an unlinked claim must not block the owner: %+v %v", link, err)
	}
	if link, _ := svc.Status(ctx, f.GuildRowID, squatter); link != nil && link.Status == linking.StatusVerified {
		t.Fatal("the squatter is no longer linked")
	}
	if _, err := svc.Request(ctx, f.GuildRowID, squatter, f.PlayerName); err == nil {
		t.Fatal("the owner's VERIFIED link blocks everyone else")
	}
}
