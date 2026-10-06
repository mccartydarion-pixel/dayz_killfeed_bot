package discord

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/playercard"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// /card (docs/CHAMPION_CARD.md, "Discord"): the caller's card is posted animated, as the still
// when the animation fails, once per user per cooldown.

type cardGuildStore struct{}

func (cardGuildStore) UpsertGuild(context.Context, repository.GuildRecord) (int64, error) {
	return 0, nil
}

func (cardGuildStore) GetGuild(context.Context, string) (*repository.GuildRecord, int64, error) {
	return &repository.GuildRecord{}, 7, nil
}

// uploadRequest is one request Discord received, with the file it carried, if any.
type uploadRequest struct {
	method, path  string
	json          string
	fileName      string
	fileType      string
	fileBody      []byte
	multipartType string
}

type uploadTransport struct {
	mu   sync.Mutex
	reqs []uploadRequest
}

func (u *uploadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := uploadRequest{method: req.Method, path: req.URL.Path}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		if mt, params, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err == nil && mt == "multipart/form-data" {
			rec.multipartType = mt
			mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				body, _ := io.ReadAll(part)
				if part.FileName() != "" {
					rec.fileName, rec.fileType, rec.fileBody = part.FileName(), part.Header.Get("Content-Type"), body
				} else if part.FormName() == "payload_json" {
					rec.json = string(body)
				}
			}
		} else {
			rec.json = string(raw)
		}
	}
	u.mu.Lock()
	u.reqs = append(u.reqs, rec)
	u.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}, nil
}

func cardTestHandler(t *testing.T) (*CardCommandHandler, *discordgo.Session, *uploadTransport) {
	t.Helper()
	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	rec := &uploadTransport{}
	s.Client = &http.Client{Transport: rec}
	h := NewCardCommandHandler(cardGuildStore{},
		func(context.Context, int64, string) (int64, string, bool) { return 42, "Ace", true },
		func(context.Context, int64) (int64, bool) { return 9, true },
		func(ctx context.Context, guildRowID, serverID, playerID int64) (*playercard.Card, error) {
			if guildRowID != 7 || serverID != 9 || playerID != 42 {
				t.Errorf("card built for guild %d server %d player %d", guildRowID, serverID, playerID)
			}
			return &playercard.Card{PlayerName: "Ace", Kills: 3}, nil
		})
	h.animate = func(c playercard.Card) ([]byte, error) { return []byte("GIF89a-" + c.PlayerName), nil }
	h.still = func(c playercard.Card) ([]byte, error) { return []byte("PNG-" + c.PlayerName), nil }
	return h, s, rec
}

func cardInteraction(id, user string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: id, AppID: "app", Token: "tok-" + id, GuildID: "g", ChannelID: "c", Type: discordgo.InteractionApplicationCommand,
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
	}}
}

func TestCardCommandPostsTheAnimatedCard(t *testing.T) {
	h, s, rec := cardTestHandler(t)
	i := cardInteraction("card-1", "u1")
	t.Cleanup(func() { deferredReplies.Delete(i.ID) })
	h.Handle(s, i)
	if len(rec.reqs) != 2 {
		t.Fatalf("want a public deferral then the upload, got %+v", rec.reqs)
	}
	deferral, upload := rec.reqs[0], rec.reqs[1]
	if deferral.method != http.MethodPost || !strings.HasSuffix(deferral.path, "/interactions/card-1/tok-card-1/callback") || !strings.Contains(deferral.json, `"type":5`) || strings.Contains(deferral.json, `"flags":64`) {
		t.Fatalf("deferral: %+v", deferral)
	}
	if upload.method != http.MethodPatch || !strings.HasSuffix(upload.path, "/webhooks/app/tok-card-1/messages/@original") || upload.multipartType != "multipart/form-data" {
		t.Fatalf("upload: %+v", upload)
	}
	if upload.fileName != "champion-card.gif" || upload.fileType != "image/gif" || string(upload.fileBody) != "GIF89a-Ace" {
		t.Fatalf("attachment: %q %q %q", upload.fileName, upload.fileType, upload.fileBody)
	}
	if !strings.Contains(upload.json, `"allowed_mentions":{"parse":[]`) {
		t.Fatalf("the card message may ping people: %s", upload.json)
	}
}

func TestCardCommandFallsBackToTheStillWhenTheAnimationFails(t *testing.T) {
	h, s, rec := cardTestHandler(t)
	h.animate = func(playercard.Card) ([]byte, error) { return nil, errors.New("no gif today") }
	i := cardInteraction("card-2", "u2")
	t.Cleanup(func() { deferredReplies.Delete(i.ID) })
	h.Handle(s, i)
	if len(rec.reqs) != 2 {
		t.Fatalf("want a deferral then the upload, got %+v", rec.reqs)
	}
	if upload := rec.reqs[1]; upload.fileName != "champion-card.png" || upload.fileType != "image/png" || string(upload.fileBody) != "PNG-Ace" {
		t.Fatalf("attachment: %q %q %q", upload.fileName, upload.fileType, upload.fileBody)
	}
	// Neither form could be made: the deferred reply says so, with no attachment.
	h.still = func(playercard.Card) ([]byte, error) { return nil, errors.New("no png either") }
	j := cardInteraction("card-3", "u3")
	t.Cleanup(func() { deferredReplies.Delete(j.ID) })
	h.Handle(s, j)
	if len(rec.reqs) != 4 {
		t.Fatalf("got %+v", rec.reqs)
	}
	if reply := rec.reqs[3]; reply.fileName != "" || !strings.Contains(reply.json, "build your card") {
		t.Fatalf("reply after both renders failed: %+v", reply)
	}
}

func TestCardCommandKeepsThePerUserCooldown(t *testing.T) {
	h, s, rec := cardTestHandler(t)
	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }
	first, second, later := cardInteraction("card-4", "u4"), cardInteraction("card-5", "u4"), cardInteraction("card-6", "u4")
	t.Cleanup(func() {
		for _, i := range []*discordgo.InteractionCreate{first, second, later} {
			deferredReplies.Delete(i.ID)
		}
	})
	h.Handle(s, first)
	now = now.Add(10 * time.Second)
	h.Handle(s, second)
	if len(rec.reqs) != 3 {
		t.Fatalf("got %+v", rec.reqs)
	}
	// The second call inside the cooldown is answered privately, with no deferral and no file.
	if refusal := rec.reqs[2]; refusal.fileName != "" || !strings.Contains(refusal.json, "You just posted your card") || !strings.Contains(refusal.json, `"flags":64`) {
		t.Fatalf("cooldown reply: %+v", refusal)
	}
	now = now.Add(cardCommandCooldown)
	h.Handle(s, later)
	if len(rec.reqs) != 5 || rec.reqs[4].fileName != "champion-card.gif" {
		t.Fatalf("after the cooldown: %+v", rec.reqs)
	}
}

// A card for a player the server does not know, or that cannot be built, is an error reply after
// the deferral, never an empty upload.
func TestCardCommandReportsACardItCannotBuild(t *testing.T) {
	h, s, rec := cardTestHandler(t)
	h.card = func(context.Context, int64, int64, int64) (*playercard.Card, error) { return nil, nil }
	i := cardInteraction("card-7", "u7")
	t.Cleanup(func() { deferredReplies.Delete(i.ID) })
	h.Handle(s, i)
	if len(rec.reqs) != 2 || rec.reqs[1].fileName != "" || !strings.Contains(rec.reqs[1].json, "build your card") {
		t.Fatalf("got %+v", rec.reqs)
	}
}
