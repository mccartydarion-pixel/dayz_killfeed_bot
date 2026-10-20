package app

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestEmbedEmojisKeepsWellFormedEntriesSortedByName(t *testing.T) {
	got := embedEmojis([]*discordgo.Emoji{
		{ID: "1558536911398895729", Name: "champ_rifle"},
		nil,
		{ID: "not-an-id", Name: "bad_id"},
		{ID: "1558536908852830318", Name: "bad name"},
		{ID: "1558536914036985916", Name: "champ_boom", Animated: true},
		{ID: "1558536877995331779", Name: "champ_crosshair"},
	})
	want := []embedEmojiDTO{
		{ID: "1558536914036985916", Name: "champ_boom", Animated: true},
		{ID: "1558536877995331779", Name: "champ_crosshair"},
		{ID: "1558536911398895729", Name: "champ_rifle"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestEmbedEmojiCacheReadsDiscordAtMostOncePerWindow(t *testing.T) {
	var c embedEmojiCache
	loads := 0
	load := func() ([]*discordgo.Emoji, error) {
		loads++
		return []*discordgo.Emoji{{ID: "1558536911398895729", Name: "champ_rifle"}}, nil
	}
	start := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	for _, d := range []time.Duration{0, 10 * time.Second, embedEmojiTTL - time.Second} {
		got, err := c.get(start.Add(d), load)
		if err != nil || len(got) != 1 {
			t.Fatalf("at %s: %+v %v", d, got, err)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d, want 1 inside the window", loads)
	}
	if _, err := c.get(start.Add(embedEmojiTTL+time.Second), load); err != nil || loads != 2 {
		t.Fatalf("after the window: loads = %d err = %v", loads, err)
	}

	// Discord failing later keeps the last good list instead of emptying the picker.
	failing := func() ([]*discordgo.Emoji, error) { return nil, errors.New("discord down") }
	got, err := c.get(start.Add(3*embedEmojiTTL), failing)
	if err != nil || len(got) != 1 {
		t.Fatalf("a failed refresh must fall back to the last list: %+v %v", got, err)
	}

	// With nothing cached, the failure is reported.
	var empty embedEmojiCache
	if _, err := empty.get(start, failing); err == nil {
		t.Fatal("a first read that fails must return the error")
	}
}
