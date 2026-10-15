package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

type fakeExpiryStore struct {
	due    []repository.SecurityExpiry
	marked []int64
}

func (f *fakeExpiryStore) DueExpiries(context.Context, int) ([]repository.SecurityExpiry, error) {
	d := f.due
	f.due = nil
	return d, nil
}
func (f *fakeExpiryStore) MarkExpiryNotified(_ context.Context, id int64) error {
	f.marked = append(f.marked, id)
	return nil
}

func TestSecurityExpiryWorkerSendsOneDMPerEndedPurchase(t *testing.T) {
	store := &fakeExpiryStore{due: []repository.SecurityExpiry{
		{PurchaseID: 1, DiscordUserID: "u1", ServerID: 4},
		{PurchaseID: 2, DiscordUserID: "", ServerID: 4},   // not linked: marked, no DM
		{PurchaseID: 3, DiscordUserID: "u3", ServerID: 4}, // DMs closed: marked, not retried
	}}
	var sent []string
	var last *discordgo.MessageSend
	w := &securityExpiryWorker{store: store, serverName: func(int64) string { return "@everyone Champions" }, siteURL: "https://champions.example",
		dm: func(user string, msg *discordgo.MessageSend) error {
			sent, last = append(sent, user), msg
			if user == "u3" {
				return errors.New("cannot send messages to this user")
			}
			return nil
		}}
	w.tick(context.Background())
	if strings.Join(sent, ",") != "u1,u3" || len(store.marked) != 3 {
		t.Fatalf("sent=%v marked=%v", sent, store.marked)
	}
	w.tick(context.Background())
	if len(sent) != 2 {
		t.Fatal("expiry DM repeated")
	}
	// The DM is a card, not plain text: a warning-coloured notice with mentions disabled.
	if last.Content != "" || len(last.Embeds) != 1 || last.AllowedMentions == nil || len(last.AllowedMentions.Parse) != 0 {
		t.Fatalf("expiry DM: %+v", last)
	}
	e := last.Embeds[0]
	if strings.Contains(e.Title, "@everyone") || !strings.HasSuffix(e.Title, "has ended") || e.Color != presentation.Amber || e.Timestamp == "" ||
		!strings.Contains(e.Description, "Nothing was charged") || !strings.Contains(e.Description, "https://champions.example/dashboard/player/security-store") {
		t.Fatalf("expiry card: %+v", e)
	}
}
