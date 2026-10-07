package discord

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/tournament"
	"github.com/yourname/dayz-killfeed/internal/tournamentcard"
)

type fakeTournamentSender struct {
	mu      sync.Mutex
	sent    []*discordgo.MessageSend
	edits   []*discordgo.MessageEdit
	deleted []string
	next    int
}

func (f *fakeTournamentSender) Send(channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, msg)
	f.next++
	return &discordgo.Message{ID: fmt.Sprint("m", f.next), ChannelID: channelID}, nil
}

func (f *fakeTournamentSender) Edit(channelID, messageID string, edit *discordgo.MessageEdit) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, edit)
	return &discordgo.Message{ID: messageID}, nil
}

func (f *fakeTournamentSender) Delete(channelID, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return nil
}

func sampleTournament(t *testing.T) *tournament.Tournament {
	t.Helper()
	t0 := time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC)
	tr := &tournament.Tournament{ID: 12, Name: "Friday Night 1v1", Status: tournament.StatusSignup, TeamSize: 1, BracketSize: 4, BestOf: 3, Seeding: tournament.SeedingRanked,
		StartsAt: t0, CheckinMinutes: 30, DiscordChannelID: "chan", CreatedByDiscordID: "admin-1",
		Rules:  tournament.Rules{AllowedWeapons: []string{"M4-A1"}, Arenas: []tournament.Arena{{No: 1, Name: "Stadium", X: 4618, Z: 10439, Radius: 150}}, MatchTimerMinutes: 10, ReadyMinutes: 2},
		Prizes: []tournament.Prize{{Place: 1, Points: 1000, Title: strPtr("Friday Champion")}, {Place: 2, Points: 500}}}
	for i := 1; i <= 4; i++ {
		at := t0
		tr.Entries = append(tr.Entries, &tournament.Entry{ID: int64(i), TeamNo: i, Status: tournament.EntryActive, CheckedInAt: &at,
			Players: []tournament.EntryPlayer{{PlayerID: int64(100 + i), DiscordUserID: fmt.Sprint("user", i), Name: fmt.Sprint("Player", i)}}})
	}
	return tr
}

func strPtr(s string) *string { return &s }

func TestTournamentSignupCardAndButtons(t *testing.T) {
	tr := sampleTournament(t)
	e := SignupEmbed(tr, "Champions 1v1")
	if problems := presentation.CheckEmbed(e); len(problems) > 0 {
		t.Fatalf("design rules: %v", problems)
	}
	if !strings.Contains(e.Description, "Sign-up is open") || !strings.Contains(e.Description, "<t:") {
		t.Fatalf("description: %q", e.Description)
	}
	joined := ""
	for _, f := range e.Fields {
		joined += f.Name + ": " + f.Value + "\n"
	}
	for _, want := range []string{"Entries (4 of 4)", "✅ Player1", "M4-A1", "Stadium (4618, 10439, 150 m)", "1st place: 1,000 pts and the title **Friday Champion**", "2nd place: 500 pts"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fields miss %q:\n%s", want, joined)
		}
	}
	buttons := SignupButtons(tr)[0].(discordgo.ActionsRow).Components
	if len(buttons) != 3 || buttons[0].(discordgo.Button).CustomID != "champion_tourney_join:12" || !buttons[2].(discordgo.Button).Disabled {
		t.Fatalf("buttons: %+v", buttons)
	}
	tr.Status = tournament.StatusCheckin
	if b := SignupButtons(tr)[0].(discordgo.ActionsRow).Components[2].(discordgo.Button); b.Disabled {
		t.Fatal("check in is enabled during check-in")
	}
	tr.Status = tournament.StatusLive
	if len(SignupButtons(tr)) != 0 {
		t.Fatal("no buttons once live")
	}
}

func TestTournamentAnnouncerFlow(t *testing.T) {
	tr := sampleTournament(t)
	sender := &fakeTournamentSender{}
	var recorded []string
	a := NewTournamentAnnouncer(sender, func(_ context.Context, id int64, channel, signup, bracket string) error {
		recorded = append(recorded, fmt.Sprint(id, "|", signup, "|", bracket))
		return nil
	}, func(context.Context, *tournament.Tournament) map[int64]ranked.Tier {
		return map[int64]ranked.Tier{101: ranked.Gold}
	},
		func(context.Context, *tournament.Tournament) string { return "admins" }, func(context.Context, *tournament.Tournament) string { return "Champions 1v1" }, "championshp.vip")
	a.render = func(b tournamentcard.Bracket) ([]byte, error) { return []byte("png"), nil }
	a.renderWinner = func(c tournamentcard.Champion) ([]byte, error) { return []byte("png"), nil }
	ctx := context.Background()

	// Opening posts the sign-up card and records its id; a change edits it in place.
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvOpened}})
	if len(sender.sent) != 1 || len(sender.sent[0].Components) != 1 || tr.SignupMessageID != "m1" || len(recorded) != 1 {
		t.Fatalf("sign-up card: sent=%d recorded=%v", len(sender.sent), recorded)
	}
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvSignupChanged}})
	if len(sender.sent) != 1 || len(sender.edits) != 1 {
		t.Fatalf("edit in place: sent=%d edits=%d", len(sender.sent), len(sender.edits))
	}
	// The start: the card loses its buttons, the first match is called with mentions, the bracket
	// image is posted.
	tr.Status = tournament.StatusCheckin
	if _, err := tr.Start(tr.StartsAt, nil, rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	for i, m := range tr.Matches {
		m.ID = int64(100 + i)
	}
	tr.LinkNext()
	m := tr.Current()
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvStarted}, {Kind: tournament.EvMatchCalled, Match: m}})
	if len(sender.edits) != 2 || len(*sender.edits[1].Components) != 0 {
		t.Fatalf("the card keeps buttons after the start: %+v", sender.edits[1].Components)
	}
	// The bracket is posted first, then the call (the call follows the picture).
	bracket := sender.sent[1]
	if len(bracket.Files) != 1 || !strings.Contains(bracket.Content, "now playing: Match 1") || tr.BracketMessageID != "m2" {
		t.Fatalf("bracket post: %+v", bracket)
	}
	call := sender.sent[2]
	if !strings.Contains(call.Content, "<@user") || len(call.AllowedMentions.Users) != 2 || !strings.Contains(call.Embeds[0].Title, "Match 1 · Semi-final") || !strings.Contains(call.Embeds[0].Description, "Stadium at 4618, 10439") {
		t.Fatalf("match call: %+v %+v", call.Content, call.Embeds[0])
	}
	if problems := presentation.CheckEmbed(call.Embeds[0]); len(problems) > 0 {
		t.Fatalf("call design rules: %v", problems)
	}
	// A flagged round pings the admin channel with the creator mentioned; a decided match
	// re-posts the bracket (the old message is deleted).
	a1, b1 := tr.Entry(*m.EntryA), tr.Entry(*m.EntryB)
	flagged := tournament.Round{N: 1, KillerName: a1.Players[0].Name, VictimName: b1.Players[0].Name, Weapon: "KA-M", At: tr.StartsAt, Flag: strPtr(tournament.FlagWeapon)}
	m.Rounds = append(m.Rounds, flagged)
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvRound, Match: m, Round: &m.Rounds[0]}, {Kind: tournament.EvAdminPing, Match: m, Text: "KA-M is not allowed."}})
	ping := sender.sent[len(sender.sent)-1]
	if ping.Content != "<@admin-1>" || !strings.Contains(ping.Embeds[0].Description, "Match 1 · Semi-final: KA-M is not allowed.") {
		t.Fatalf("admin ping: %+v", ping)
	}
	round := sender.sent[len(sender.sent)-2]
	if !strings.Contains(round.Embeds[0].Description, "which is not allowed") || round.Embeds[0].Color != presentation.Amber {
		t.Fatalf("round line: %+v", round.Embeds[0])
	}
	before := len(sender.sent)
	if _, err := tr.AdminResult(m.ID, a1.ID, "admin-1", "", tr.StartsAt); err != nil {
		t.Fatal(err)
	}
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvMatchDone, Match: m}})
	if len(sender.deleted) != 1 || sender.deleted[0] != "m2" || len(sender.sent) != before+2 {
		t.Fatalf("bracket refresh: deleted=%v sent=%d", sender.deleted, len(sender.sent)-before)
	}
	// The champion card after the final.
	for tr.Status == tournament.StatusLive {
		cur := tr.Current()
		if _, err := tr.AdminResult(cur.ID, *cur.EntryA, "admin-1", "", tr.StartsAt); err != nil {
			t.Fatal(err)
		}
	}
	a.Notify(ctx, tr, []tournament.Event{{Kind: tournament.EvFinished, Entry: tr.Champion()}})
	winner := sender.sent[len(sender.sent)-1]
	if !strings.HasPrefix(winner.Embeds[0].Title, "👑 Champion: ") || len(winner.Files) != 1 || !strings.Contains(winner.Embeds[0].Description, "Friday Champion") || !strings.Contains(winner.Embeds[0].Description, "1st place, 1,000 pts each") {
		t.Fatalf("champion: %+v", winner.Embeds[0])
	}
	for _, msg := range sender.sent {
		for _, e := range msg.Embeds {
			if problems := presentation.CheckEmbed(e); len(problems) > 0 {
				t.Errorf("%q: %v", e.Title, problems)
			}
		}
	}
	// A tournament without a channel is silent.
	quiet := sampleTournament(t)
	quiet.DiscordChannelID = ""
	n := len(sender.sent)
	a.Notify(ctx, quiet, []tournament.Event{{Kind: tournament.EvOpened}})
	if len(sender.sent) != n {
		t.Fatal("no channel, no messages")
	}
}

func TestTournamentStatusTextAndParseStart(t *testing.T) {
	tr := sampleTournament(t)
	text := StatusText(tr, "user2")
	if !strings.Contains(text, "Sign-up is open") || !strings.Contains(text, "You are entered and checked in") || !strings.Contains(text, "4 of 4 slots") {
		t.Fatalf("status: %q", text)
	}
	tr.Status = tournament.StatusCheckin
	if _, err := tr.Start(tr.StartsAt, nil, rand.New(rand.NewSource(1))); err != nil {
		t.Fatal(err)
	}
	for i, m := range tr.Matches {
		m.ID = int64(100 + i)
	}
	text = StatusText(tr, "user1")
	if !strings.Contains(text, "Your next match: Match") || !strings.Contains(text, "Now: Match 1 · Semi-final") || !strings.Contains(text, "📣 Match 1") {
		t.Fatalf("live status: %q", text)
	}
	if m := MatchByNumber(tr, 3); m == nil || m.RoundName != "Final" || MatchNumber(tr, m) != 3 || MatchByNumber(tr, 4) != nil {
		t.Fatal("match numbers")
	}
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"90m":              now.Add(90 * time.Minute),
		"2h":               now.Add(2 * time.Hour),
		"2026-10-10 19:00": time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC),
		"19:00":            time.Date(2026, 10, 7, 19, 0, 0, 0, time.UTC),
		"09:00":            time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
	}
	for raw, want := range cases {
		got, err := parseStart(raw, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v (want %v)", raw, got, err, want)
		}
	}
	if _, err := parseStart("soon", now); err == nil {
		t.Fatal("garbage must fail")
	}
	if e := findEntryByName(tr, " player3 "); e == nil || e.ID != 3 {
		t.Fatal("name lookup is case-insensitive and trimmed")
	}
}
