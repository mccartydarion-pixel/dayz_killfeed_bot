package presentation

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every message the bot posts or DMs is a card. This test reads the source under internal/ and
// fails on a channel message or DM that would go out as plain text: a discordgo.MessageSend
// literal with Content and no Embeds, or a call to the plain-text senders. Interaction
// replies (InteractionResponseData, WebhookEdit) are not scanned: an ephemeral one-line
// answer to the person who typed a command is a reply, not a message the bot posts.

// plainTextAllowed lists the files and literals that may stay plain text, each with its reason.
// Keep it empty unless a message truly cannot be a card.
var plainTextAllowed = map[string]string{}

var (
	plainSendCall  = regexp.MustCompile(`\.ChannelMessageSend(Content)?\(`)
	messageSendLit = regexp.MustCompile(`discordgo\.MessageSend\{`)
)

func TestNoPlainTextMessagesToDiscord(t *testing.T) {
	root := filepath.Join("..")
	var problems []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		rel := filepath.ToSlash(strings.TrimPrefix(path, ".."+string(filepath.Separator)))
		scanned++
		for _, m := range plainSendCall.FindAllStringIndex(src, -1) {
			// The SessionAPI wrapper is the one place a discordgo call is allowed to be named; it
			// has no plain-text sender any more, so a hit there is a regression too.
			problems = append(problems, rel+":"+lineOf(src, m[0])+": calls a plain-text sender "+strings.TrimSuffix(src[m[0]+1:m[1]], "("))
		}
		for _, m := range messageSendLit.FindAllStringIndex(src, -1) {
			lit := braceBody(src, m[1]-1)
			if strings.Contains(lit, "Content:") && !strings.Contains(lit, "Embeds") {
				if why, ok := plainTextAllowed[rel]; ok {
					t.Logf("%s is allowed to be plain text: %s", rel, why)
					continue
				}
				problems = append(problems, rel+":"+lineOf(src, m[0])+": a MessageSend with Content and no Embeds")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d files; is the test running from internal/presentation?", scanned)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// braceBody returns the source from the opening brace at i to its matching close brace.
func braceBody(src string, i int) string {
	depth := 0
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[i : j+1]
			}
		}
	}
	return src[i:]
}

func lineOf(src string, offset int) string {
	return strconv.Itoa(strings.Count(src[:offset], "\n") + 1)
}
