package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

type recordedRequest struct {
	method, path string
	body         map[string]any
}

type recordingTransport struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := recordedRequest{method: req.Method, path: req.URL.Path}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &rec.body)
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, rec)
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{"Content-Type": []string{"application/json"}}, Request: req}, nil
}

func recordingSession(t *testing.T) (*discordgo.Session, *recordingTransport) {
	t.Helper()
	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingTransport{}
	s.Client = &http.Client{Transport: rec}
	return s, rec
}

func TestDeferredReplyIsEditedNotAnsweredTwice(t *testing.T) {
	s, rec := recordingSession(t)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "777", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand}}
	t.Cleanup(func() { deferredReplies.Delete(i.ID) })
	if !deferEphemeral(s, i) {
		t.Fatal("defer failed")
	}
	respondEphemeral(s, i, "done")
	if len(rec.reqs) != 2 {
		t.Fatalf("want defer + edit, got %+v", rec.reqs)
	}
	first, second := rec.reqs[0], rec.reqs[1]
	if first.method != http.MethodPost || !strings.HasSuffix(first.path, "/interactions/777/tok/callback") || first.body["type"] != float64(discordgo.InteractionResponseDeferredChannelMessageWithSource) {
		t.Fatalf("defer request: %+v", first)
	}
	if data, _ := first.body["data"].(map[string]any); data["flags"] != float64(discordgo.MessageFlagsEphemeral) {
		t.Fatalf("defer is not private: %+v", first.body)
	}
	if second.method != http.MethodPatch || !strings.HasSuffix(second.path, "/webhooks/app/tok/messages/@original") || second.body["content"] != "done" {
		t.Fatalf("edit request: %+v", second)
	}
}

func TestReplyWithoutDeferIsAPrivateAnswer(t *testing.T) {
	s, rec := recordingSession(t)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "778", AppID: "app", Token: "tok2", Type: discordgo.InteractionApplicationCommand}}
	respondEphemeral(s, i, "hi")
	if len(rec.reqs) != 1 || rec.reqs[0].body["type"] != float64(discordgo.InteractionResponseChannelMessageWithSource) {
		t.Fatalf("want one answer, got %+v", rec.reqs)
	}
	if data, _ := rec.reqs[0].body["data"].(map[string]any); data["flags"] != float64(discordgo.MessageFlagsEphemeral) || data["content"] != "hi" {
		t.Fatalf("answer not private: %+v", rec.reqs[0].body)
	}
}
