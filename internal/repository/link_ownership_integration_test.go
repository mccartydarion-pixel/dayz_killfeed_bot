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

// Unlink then /link another player: the old player's challenge is closed, so reconnecting on
// one's own account can never verify the link as somebody else's.
func TestRelinkCannotBeVerifiedByTheOldPlayersChallenge(t *testing.T) {
	ctx := context.Background()
	f := newLinkBindingFixture(t, "ONLINE", 6*time.Minute)
	victimName := fmt.Sprintf("Victim%d", time.Now().UnixNano()%100000)
	victim := secondPlayer(t, ctx, f, victimName)
	db := saasIntegrationDB(t)
	links := NewLinkRepository(db.Pool)
	svc := linking.NewService(links, NewActivityRepository(db.Pool), NewServerRepository(db.Pool), links)
	attacker := fmt.Sprintf("discord-att-%d", time.Now().UnixNano())

	if _, err := svc.Request(ctx, f.GuildRowID, attacker, f.PlayerName); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unlink(ctx, f.GuildRowID, attacker); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Request(ctx, f.GuildRowID, attacker, victimName); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// The attacker's own account disconnects and reconnects.
	if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	link, err := svc.Status(ctx, f.GuildRowID, attacker)
	if err != nil || link == nil {
		t.Fatal(err)
	}
	if link.Status == linking.StatusVerified {
		t.Fatalf("the link to %d was verified by player %d's reconnect", victim, f.PlayerID)
	}
}

// A claim another Discord user abandoned (still pending, or unlinked) does not block the
// player's owner from requesting the link; a VERIFIED one still does.
func TestStaleClaimDoesNotBlockTheOwner(t *testing.T) {
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
	if _, err := svc.Request(ctx, f.GuildRowID, owner, f.PlayerName); err != nil {
		t.Fatalf("a pending claim by someone else must not block the owner: %v", err)
	}
	// The owner completes the challenge; it is the owner who is verified.
	now := time.Now()
	if err := svc.ObserveDisconnect(ctx, f.GuildRowID, f.PlayerID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := svc.ObserveConnect(ctx, f.GuildRowID, f.PlayerID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if link, _ := svc.Status(ctx, f.GuildRowID, owner); link == nil || link.Status != linking.StatusVerified {
		t.Fatalf("the owner's link should be verified, got %+v", link)
	}
	if link, _ := svc.Status(ctx, f.GuildRowID, squatter); link != nil && link.Status == linking.StatusVerified {
		t.Fatal("the squatter must not be verified")
	}
	if _, err := svc.Request(ctx, f.GuildRowID, squatter, f.PlayerName); err == nil {
		t.Fatal("a VERIFIED link still blocks everyone else")
	}
}
