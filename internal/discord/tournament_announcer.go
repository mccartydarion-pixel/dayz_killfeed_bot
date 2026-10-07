package discord

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/ranked"
	"github.com/yourname/dayz-killfeed/internal/tournament"
	"github.com/yourname/dayz-killfeed/internal/tournamentcard"
)

// Tournament mode in Discord (docs/TOURNAMENTS.md): the sign-up card with its Join, Leave and
// Check in buttons, the bracket image posted at the start and after every change, the match
// calls with their mentions, the round results, the admin pings and the champion card. The
// announcer is the engine's Notifier: it runs after each committed transition.

// TournamentSender is the narrow Discord surface the announcer uses (*discordgo.Session through
// SessionTournamentSender; a fake in tests).
type TournamentSender interface {
	Send(channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error)
	Edit(channelID, messageID string, edit *discordgo.MessageEdit) (*discordgo.Message, error)
	Delete(channelID, messageID string) error
}

// SessionTournamentSender adapts a live session.
type SessionTournamentSender struct{ S *discordgo.Session }

func (s SessionTournamentSender) Send(channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
	return s.S.ChannelMessageSendComplex(channelID, msg)
}

func (s SessionTournamentSender) Edit(channelID, messageID string, edit *discordgo.MessageEdit) (*discordgo.Message, error) {
	edit.Channel, edit.ID = channelID, messageID
	return s.S.ChannelMessageEditComplex(edit)
}

func (s SessionTournamentSender) Delete(channelID, messageID string) error {
	return s.S.ChannelMessageDelete(channelID, messageID)
}

// TournamentAnnouncer posts a tournament's messages.
type TournamentAnnouncer struct {
	sender TournamentSender
	// messages records the sign-up and bracket message ids on the tournament.
	messages func(ctx context.Context, tournamentID int64, channelID, signupMessageID, bracketMessageID string) error
	// tiers is the Ranked tier of each player on the server (for the images); nil-safe.
	tiers func(ctx context.Context, t *tournament.Tournament) map[int64]ranked.Tier
	// adminChannel is where admin pings go ("" falls back to the tournament's channel).
	adminChannel func(ctx context.Context, t *tournament.Tournament) string
	serverName   func(ctx context.Context, t *tournament.Tournament) string
	siteHost     string
	render       func(tournamentcard.Bracket) ([]byte, error)
	renderWinner func(tournamentcard.Champion) ([]byte, error)

	mu sync.Mutex // one tournament's messages at a time
}

// NewTournamentAnnouncer wires the announcer.
func NewTournamentAnnouncer(sender TournamentSender,
	messages func(ctx context.Context, tournamentID int64, channelID, signupMessageID, bracketMessageID string) error,
	tiers func(ctx context.Context, t *tournament.Tournament) map[int64]ranked.Tier,
	adminChannel func(ctx context.Context, t *tournament.Tournament) string,
	serverName func(ctx context.Context, t *tournament.Tournament) string, siteHost string) *TournamentAnnouncer {
	return &TournamentAnnouncer{sender: sender, messages: messages, tiers: tiers, adminChannel: adminChannel, serverName: serverName, siteHost: siteHost,
		render: tournamentcard.Render, renderWinner: tournamentcard.RenderChampion}
}

// Button custom ids of the sign-up card: "<prefix><action>:<tournament id>".
const (
	TournamentComponentPrefix = "champion_tourney_"
	tournamentJoinButton      = TournamentComponentPrefix + "join:"
	tournamentLeaveButton     = TournamentComponentPrefix + "leave:"
	tournamentCheckinButton   = TournamentComponentPrefix + "checkin:"
)

var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

// Notify implements tournament.Notifier.
func (a *TournamentAnnouncer) Notify(ctx context.Context, t *tournament.Tournament, events []tournament.Event) {
	if a == nil || a.sender == nil || t == nil || t.DiscordChannelID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	signup, bracket, winner := false, false, false
	var calls []tournament.Event // posted after the bracket, so the call follows the picture
	for _, e := range events {
		switch e.Kind {
		case tournament.EvOpened:
			signup = true
		case tournament.EvSignupChanged, tournament.EvCheckin:
			signup = true
			if e.Kind == tournament.EvCheckin {
				a.post(t.DiscordChannelID, a.checkinEmbed(t), "")
			}
		case tournament.EvStarted:
			signup, bracket = true, true
		case tournament.EvMatchCalled:
			calls = append(calls, e)
		case tournament.EvMatchStarted:
			a.post(t.DiscordChannelID, a.matchStartedEmbed(t, e.Match), "")
		case tournament.EvRound:
			a.post(t.DiscordChannelID, a.roundEmbed(t, e), "")
		case tournament.EvMatchDone:
			bracket = true
			a.post(t.DiscordChannelID, a.matchDoneEmbed(t, e), "")
		case tournament.EvBracket:
			bracket = true
		case tournament.EvAdminPing:
			a.postAdminPing(ctx, t, e)
		case tournament.EvPaused:
			a.post(t.DiscordChannelID, a.simpleEmbed(t, "⏸️ Tournament paused", "Kills do not count until an admin resumes it.", presentation.Amber), "")
		case tournament.EvResumed:
			a.post(t.DiscordChannelID, a.simpleEmbed(t, "▶️ Tournament resumed", "Kills count again.", presentation.Green), "")
		case tournament.EvCancelled:
			signup = true
			why := e.Text
			if why == "" {
				why = "An admin cancelled the tournament."
			}
			a.post(t.DiscordChannelID, a.simpleEmbed(t, "Tournament cancelled", why, presentation.Red), "")
		case tournament.EvFinished:
			bracket, winner = true, true
		}
	}
	if signup {
		a.syncSignupCard(ctx, t)
	}
	if bracket {
		a.postBracket(ctx, t)
	}
	for _, e := range calls {
		a.postMatchCall(t, e)
	}
	if winner {
		a.postChampion(ctx, t)
	}
}

func (a *TournamentAnnouncer) post(channelID string, embed *discordgo.MessageEmbed, content string) *discordgo.Message {
	msg, err := a.sender.Send(channelID, &discordgo.MessageSend{Content: content, Embeds: []*discordgo.MessageEmbed{embed}, AllowedMentions: noMentions})
	if err != nil {
		slog.Warn("component=tournament", "event", "discord_post_failed", "channel_id", channelID, "err", err.Error())
	}
	return msg
}

func (a *TournamentAnnouncer) name(ctx context.Context, t *tournament.Tournament) string {
	if a.serverName == nil {
		return ""
	}
	return a.serverName(ctx, t)
}

func (a *TournamentAnnouncer) embed(t *tournament.Tournament, title string, color int) *discordgo.MessageEmbed {
	e := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Tournament"), Title: title, Color: color, Footer: presentation.Footer("", presentation.CleanName(t.Name, 60))}
	return e
}

func (a *TournamentAnnouncer) simpleEmbed(t *tournament.Tournament, title, text string, color int) *discordgo.MessageEmbed {
	e := a.embed(t, title, color)
	e.Description = text
	presentation.StampEmbed(e, time.Now())
	return e
}

// --- the sign-up card ---------------------------------------------------------------------------------

// entryLine names an entry's players, with its check-in mark.
func entryLine(e *tournament.Entry) string {
	names := make([]string, 0, len(e.Players))
	for _, p := range e.Players {
		names = append(names, presentation.CleanName(p.Name, 32))
	}
	mark := "▫️"
	switch {
	case e.Status == tournament.EntryDQ:
		mark = "⛔"
	case e.CheckedIn():
		mark = "✅"
	}
	return mark + " " + strings.Join(names, " & ")
}

// SignupEmbed is the sign-up card.
func SignupEmbed(t *tournament.Tournament, serverName string) *discordgo.MessageEmbed {
	color := presentation.Crimson
	title := "🏆 " + presentation.CleanName(t.Name, 80)
	switch t.Status {
	case tournament.StatusCheckin:
		color = presentation.Amber
	case tournament.StatusLive, tournament.StatusPaused:
		color = presentation.Gold
	case tournament.StatusFinished:
		color = presentation.Gold
	case tournament.StatusCancelled:
		color = presentation.Neutral
	}
	e := &discordgo.MessageEmbed{Author: presentation.BrandAuthor("Tournament"), Title: title, Color: color}
	format := "1v1"
	if t.TeamSize == 2 {
		format = "2v2"
	}
	var lines []string
	switch t.Status {
	case tournament.StatusSignup:
		lines = append(lines, "Sign-up is open. Press **Join** to enter"+map[bool]string{true: " with your partner (`/tournament join @partner`)", false: ""}[t.TeamSize == 2]+".")
	case tournament.StatusCheckin:
		lines = append(lines, "**Check-in is open.** Press **Check in** to confirm you are here, or you will not be in the draw.")
	case tournament.StatusLive, tournament.StatusPaused:
		lines = append(lines, "The tournament is under way. Follow the bracket below.")
	case tournament.StatusFinished:
		lines = append(lines, "The tournament is over.")
	case tournament.StatusCancelled:
		lines = append(lines, "The tournament was cancelled.")
	}
	lines = append(lines, "", "**Starts** "+presentation.TimestampWithRelative(t.StartsAt))
	if t.CheckinMinutes > 0 && (t.Status == tournament.StatusSignup || t.Status == tournament.StatusCheckin) {
		lines = append(lines, "**Check-in opens** "+presentation.Timestamp(t.CheckinOpensAt(), 't'))
	}
	e.Description = strings.Join(lines, "\n")
	rules := fmt.Sprintf("%s · %d slots · best of %d · %s seeding", format, t.BracketSize, t.BestOf, strings.ToLower(t.Seeding))
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Format", Value: rules})
	if len(t.Rules.AllowedWeapons) > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Allowed weapons", Value: presentation.CleanName(strings.Join(t.Rules.AllowedWeapons, ", "), 200)})
	}
	if len(t.Rules.Arenas) > 0 {
		names := make([]string, 0, len(t.Rules.Arenas))
		for _, ar := range t.Rules.Arenas {
			names = append(names, fmt.Sprintf("%s (%.0f, %.0f, %.0f m)", presentation.CleanName(ar.Name, 40), ar.X, ar.Z, ar.Radius))
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Arenas", Value: strings.Join(names, "\n")})
	}
	if len(t.Prizes) > 0 {
		var prizes []string
		for _, p := range t.Prizes {
			line := fmt.Sprintf("%s place: %s", ordinalWord(p.Place), presentation.FormatPoints(p.Points))
			if p.Title != nil {
				line += " and the title **" + presentation.CleanName(*p.Title, 40) + "**"
			}
			prizes = append(prizes, line)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "Prizes", Value: strings.Join(prizes, "\n")})
	}
	var entries []string
	n := 0
	for _, en := range t.Entries {
		if en.Status == tournament.EntryWithdrawn {
			continue
		}
		n++
		entries = append(entries, entryLine(en))
	}
	value := "Nobody yet."
	if len(entries) > 0 {
		value = strings.Join(entries, "\n")
		if len(value) > 1000 {
			value = value[:1000] + "…"
		}
	}
	e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: fmt.Sprintf("Entries (%d of %d)", n, t.BracketSize), Value: value})
	if serverName != "" {
		e.Footer = presentation.Footer(serverName, "")
	}
	presentation.StampEmbed(e, time.Now())
	return e
}

// SignupButtons are the card's buttons for the tournament's state (none once it started).
func SignupButtons(t *tournament.Tournament) []discordgo.MessageComponent {
	if t.Status != tournament.StatusSignup && t.Status != tournament.StatusCheckin {
		return []discordgo.MessageComponent{}
	}
	id := fmt.Sprint(t.ID)
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Join", Style: discordgo.PrimaryButton, CustomID: tournamentJoinButton + id, Disabled: t.TeamSize == 2},
		discordgo.Button{Label: "Leave", Style: discordgo.SecondaryButton, CustomID: tournamentLeaveButton + id},
		discordgo.Button{Label: "Check in", Style: discordgo.SuccessButton, CustomID: tournamentCheckinButton + id, Disabled: t.Status != tournament.StatusCheckin},
	}}}
}

func (a *TournamentAnnouncer) syncSignupCard(ctx context.Context, t *tournament.Tournament) {
	embed := SignupEmbed(t, a.name(ctx, t))
	components := SignupButtons(t)
	if t.SignupMessageID != "" {
		if _, err := a.sender.Edit(t.DiscordChannelID, t.SignupMessageID, &discordgo.MessageEdit{Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components, AllowedMentions: noMentions}); err == nil {
			return
		} else {
			slog.Debug("component=tournament", "event", "signup_edit_failed", "err", err.Error())
		}
	}
	if t.Over() {
		return
	}
	msg, err := a.sender.Send(t.DiscordChannelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: components, AllowedMentions: noMentions})
	if err != nil {
		slog.Warn("component=tournament", "event", "signup_post_failed", "err", err.Error())
		return
	}
	t.SignupMessageID = msg.ID
	if a.messages != nil {
		if err := a.messages(ctx, t.ID, "", msg.ID, ""); err != nil {
			slog.Warn("component=tournament", "event", "signup_record_failed", "err", err.Error())
		}
	}
}

func (a *TournamentAnnouncer) checkinEmbed(t *tournament.Tournament) *discordgo.MessageEmbed {
	e := a.embed(t, "✅ Check-in is open", presentation.Amber)
	e.Description = fmt.Sprintf("Press **Check in** on the sign-up card (or run `/tournament checkin`) before %s. Entries that do not check in are left out of the draw.", presentation.Timestamp(t.StartsAt, 't'))
	presentation.StampEmbed(e, time.Now())
	return e
}

// --- the bracket --------------------------------------------------------------------------------------

func (a *TournamentAnnouncer) tierMap(ctx context.Context, t *tournament.Tournament) map[int64]ranked.Tier {
	if a.tiers == nil {
		return nil
	}
	return a.tiers(ctx, t)
}

// postBracket replaces the bracket message with a fresh image (Discord cannot swap an
// attachment in place without re-uploading, so the old message goes and a new one is posted).
func (a *TournamentAnnouncer) postBracket(ctx context.Context, t *tournament.Tournament) {
	data, err := a.render(tournamentcard.FromTournament(t, a.tierMap(ctx, t), a.name(ctx, t), a.siteHost))
	if err != nil {
		slog.Warn("component=tournament", "event", "bracket_render_failed", "err", err.Error())
		return
	}
	if t.BracketMessageID != "" {
		_ = a.sender.Delete(t.DiscordChannelID, t.BracketMessageID)
	}
	caption := "🏆 **" + presentation.CleanName(t.Name, 80) + "** · " + bracketCaption(t)
	msg, err := a.sender.Send(t.DiscordChannelID, &discordgo.MessageSend{Content: caption, AllowedMentions: noMentions,
		Files: []*discordgo.File{{Name: "bracket.png", ContentType: "image/png", Reader: bytes.NewReader(data)}}})
	if err != nil {
		slog.Warn("component=tournament", "event", "bracket_post_failed", "err", err.Error())
		return
	}
	t.BracketMessageID = msg.ID
	if a.messages != nil {
		if err := a.messages(ctx, t.ID, "", "", msg.ID); err != nil {
			slog.Warn("component=tournament", "event", "bracket_record_failed", "err", err.Error())
		}
	}
}

func bracketCaption(t *tournament.Tournament) string {
	switch t.Status {
	case tournament.StatusFinished:
		return "final bracket"
	case tournament.StatusPaused:
		return "bracket (paused)"
	}
	if m := t.Current(); m != nil {
		return "bracket · now playing: " + MatchLabel(t, m)
	}
	return "bracket"
}

// MatchLabel names a match for people: "Match 3 · Quarter-final".
func MatchLabel(t *tournament.Tournament, m *tournament.Match) string {
	return fmt.Sprintf("Match %d · %s", MatchNumber(t, m), m.RoundName)
}

// MatchNumber is a match's 1-based number in bracket order (what /tournament result takes).
func MatchNumber(t *tournament.Tournament, m *tournament.Match) int {
	for i, o := range t.Matches {
		if o == m || (m != nil && o.ID == m.ID && m.ID != 0) {
			return i + 1
		}
	}
	return 0
}

// MatchByNumber finds a match by its number.
func MatchByNumber(t *tournament.Tournament, n int) *tournament.Match {
	if n < 1 || n > len(t.Matches) {
		return nil
	}
	return t.Matches[n-1]
}

// EntryName names an entry: the players, joined.
func EntryName(t *tournament.Tournament, id *int64) string {
	if id == nil {
		return "TBD"
	}
	e := t.Entry(*id)
	if e == nil {
		return "TBD"
	}
	names := make([]string, 0, len(e.Players))
	for _, p := range e.Players {
		names = append(names, presentation.CleanName(p.Name, 32))
	}
	return strings.Join(names, " & ")
}

// EntryMentions mentions every linked player of an entry.
func EntryMentions(t *tournament.Tournament, id *int64) (text string, users []string) {
	if id == nil {
		return "", nil
	}
	e := t.Entry(*id)
	if e == nil {
		return "", nil
	}
	var parts []string
	for _, p := range e.Players {
		if p.DiscordUserID != "" {
			parts = append(parts, "<@"+p.DiscordUserID+">")
			users = append(users, p.DiscordUserID)
		} else {
			parts = append(parts, presentation.CleanName(p.Name, 32))
		}
	}
	return strings.Join(parts, " & "), users
}

func (a *TournamentAnnouncer) postMatchCall(t *tournament.Tournament, ev tournament.Event) {
	m := ev.Match
	if m == nil {
		return
	}
	title := "📣 " + MatchLabel(t, m)
	if ev.Text == "replay" {
		title = "🔁 Replay: " + MatchLabel(t, m)
	}
	e := a.embed(t, title, presentation.Crimson)
	am, usersA := EntryMentions(t, m.EntryA)
	bm, usersB := EntryMentions(t, m.EntryB)
	lines := []string{fmt.Sprintf("**%s** vs **%s**", EntryName(t, m.EntryA), EntryName(t, m.EntryB)), fmt.Sprintf("Best of %d · first to %d.", t.BestOf, tournament.RoundsToWin(t.BestOf))}
	if ar := t.Arena(m); ar != nil {
		lines = append(lines, fmt.Sprintf("**Arena:** %s at %.0f, %.0f (within %.0f m).", presentation.CleanName(ar.Name, 40), ar.X, ar.Z, ar.Radius))
	}
	if len(t.Rules.AllowedWeapons) > 0 {
		lines = append(lines, "**Weapons:** "+presentation.CleanName(strings.Join(t.Rules.AllowedWeapons, ", "), 200)+".")
	}
	if m.TimerEndsAt != nil && m.Status == tournament.MatchCalled {
		lines = append(lines, fmt.Sprintf("Be in the arena by %s (%s) or the match is forfeited.", presentation.Timestamp(*m.TimerEndsAt, 't'), presentation.Timestamp(*m.TimerEndsAt, 'R')))
	}
	e.Description = strings.Join(lines, "\n")
	presentation.StampEmbed(e, time.Now())
	content := strings.TrimSpace(am + " " + bm)
	users := append(usersA, usersB...)
	_, err := a.sender.Send(t.DiscordChannelID, &discordgo.MessageSend{Content: content, Embeds: []*discordgo.MessageEmbed{e},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}})
	if err != nil {
		slog.Warn("component=tournament", "event", "match_call_failed", "err", err.Error())
	}
}

func (a *TournamentAnnouncer) matchStartedEmbed(t *tournament.Tournament, m *tournament.Match) *discordgo.MessageEmbed {
	e := a.embed(t, "🟢 "+MatchLabel(t, m)+" is live", presentation.Green)
	text := fmt.Sprintf("**%s** vs **%s** · best of %d.", EntryName(t, m.EntryA), EntryName(t, m.EntryB), t.BestOf)
	if m.TimerEndsAt != nil {
		text += fmt.Sprintf("\nMatch timer ends %s.", presentation.Timestamp(*m.TimerEndsAt, 'R'))
	}
	e.Description = text
	presentation.StampEmbed(e, time.Now())
	return e
}

// RoundLine is the one-line result of a round.
func RoundLine(t *tournament.Tournament, m *tournament.Match, r *tournament.Round) string {
	score := fmt.Sprintf("%s %d–%d %s", EntryName(t, m.EntryA), m.ScoreA, m.ScoreB, EntryName(t, m.EntryB))
	if r.Flag != nil {
		switch *r.Flag {
		case tournament.FlagManual:
			if r.WinnerEntry != nil {
				return fmt.Sprintf("Admin decision: **%s** wins. %s", EntryName(t, r.WinnerEntry), score)
			}
			return "Admin decision: " + presentation.CleanName(r.Weapon, 60) + ". " + score
		case tournament.FlagNonPlayer:
			return fmt.Sprintf("**%s** died to %s. Not counted. %s", presentation.CleanName(r.VictimName, 32), presentation.CleanName(r.Weapon, 40), score)
		case tournament.FlagInterference:
			return fmt.Sprintf("**%s** killed **%s** from outside the match. Not counted. %s", presentation.CleanName(r.KillerName, 32), presentation.CleanName(r.VictimName, 32), score)
		case tournament.FlagWeapon:
			return fmt.Sprintf("**%s** killed **%s** with %s, which is not allowed. Not counted, waiting for an admin. %s", presentation.CleanName(r.KillerName, 32), presentation.CleanName(r.VictimName, 32), presentation.CleanName(r.Weapon, 40), score)
		case tournament.FlagOutsideArena:
			return fmt.Sprintf("**%s** killed **%s** outside the arena. Not counted, waiting for an admin. %s", presentation.CleanName(r.KillerName, 32), presentation.CleanName(r.VictimName, 32), score)
		}
	}
	dist := ""
	if r.Distance != nil {
		dist = fmt.Sprintf(" from %.0f m", *r.Distance)
	}
	return fmt.Sprintf("**%s** killed **%s** with %s%s. %s", presentation.CleanName(r.KillerName, 32), presentation.CleanName(r.VictimName, 32), presentation.CleanName(r.Weapon, 40), dist, score)
}

func (a *TournamentAnnouncer) roundEmbed(t *tournament.Tournament, ev tournament.Event) *discordgo.MessageEmbed {
	m, r := ev.Match, ev.Round
	color := presentation.Crimson
	if r.Flag != nil && *r.Flag != tournament.FlagManual {
		color = presentation.Amber
	}
	e := a.embed(t, fmt.Sprintf("Round %d · %s", r.N, MatchLabel(t, m)), color)
	e.Description = RoundLine(t, m, r)
	presentation.StampEmbed(e, r.At)
	return e
}

func (a *TournamentAnnouncer) matchDoneEmbed(t *tournament.Tournament, ev tournament.Event) *discordgo.MessageEmbed {
	m := ev.Match
	e := a.embed(t, "🏁 "+MatchLabel(t, m), presentation.Gold)
	switch {
	case m.Status == tournament.MatchForfeit && m.WinnerEntry != nil:
		why := ev.Text
		if why == "" {
			why = "forfeit"
		}
		e.Description = fmt.Sprintf("**%s** wins by forfeit (%s).", EntryName(t, m.WinnerEntry), why)
	case m.WinnerEntry != nil:
		e.Description = fmt.Sprintf("**%s** wins %d–%d against %s.", EntryName(t, m.WinnerEntry), max(m.ScoreA, m.ScoreB), min(m.ScoreA, m.ScoreB), EntryName(t, loserID(m)))
	default:
		e.Description = "The match ended with no winner."
	}
	presentation.StampEmbed(e, time.Now())
	return e
}

func loserID(m *tournament.Match) *int64 {
	if m.WinnerEntry == nil {
		return nil
	}
	if m.EntryA != nil && *m.EntryA == *m.WinnerEntry {
		return m.EntryB
	}
	return m.EntryA
}

func (a *TournamentAnnouncer) postAdminPing(ctx context.Context, t *tournament.Tournament, ev tournament.Event) {
	channel := ""
	if a.adminChannel != nil {
		channel = a.adminChannel(ctx, t)
	}
	if channel == "" {
		channel = t.DiscordChannelID
	}
	e := a.embed(t, "⚠️ Tournament needs an admin", presentation.Amber)
	text := ev.Text
	if ev.Match != nil {
		text = MatchLabel(t, ev.Match) + ": " + text
	}
	e.Description = text
	presentation.StampEmbed(e, time.Now())
	content, users := "", []string{}
	if t.CreatedByDiscordID != "" {
		content, users = "<@"+t.CreatedByDiscordID+">", []string{t.CreatedByDiscordID}
	}
	if _, err := a.sender.Send(channel, &discordgo.MessageSend{Content: content, Embeds: []*discordgo.MessageEmbed{e},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}}); err != nil {
		slog.Warn("component=tournament", "event", "admin_ping_failed", "err", err.Error())
	}
}

func (a *TournamentAnnouncer) postChampion(ctx context.Context, t *tournament.Tournament) {
	c := tournamentcard.ChampionOf(t, a.tierMap(ctx, t), a.name(ctx, t), a.siteHost)
	if c == nil {
		a.post(t.DiscordChannelID, a.simpleEmbed(t, "Tournament over", "The final had no winner.", presentation.Neutral), "")
		return
	}
	champ := t.Champion()
	mentions, users := EntryMentions(t, &champ.ID)
	e := a.embed(t, "👑 Champion: "+EntryName(t, &champ.ID), presentation.Gold)
	lines := []string{fmt.Sprintf("%s won **%s**.", mentions, presentation.CleanName(t.Name, 80))}
	if c.Title != "" {
		lines = append(lines, "They now carry the title **"+presentation.CleanName(c.Title, 40)+"**.")
	}
	var prizes []string
	places := map[int][]string{}
	for _, en := range t.Entries {
		if p := t.Place(en.ID); p > 0 {
			places[p] = append(places[p], EntryName(t, &en.ID))
		}
	}
	for _, p := range t.Prizes {
		if p.Points <= 0 || len(places[p.Place]) == 0 {
			continue
		}
		prizes = append(prizes, fmt.Sprintf("%s place, %s each: %s", ordinalWord(p.Place), presentation.FormatPoints(p.Points), strings.Join(places[p.Place], ", ")))
	}
	if len(prizes) > 0 {
		sort.Strings(prizes)
		lines = append(lines, "", "**Prizes paid**", strings.Join(prizes, "\n"))
	}
	e.Description = strings.Join(lines, "\n")
	presentation.StampEmbed(e, time.Now())
	msg := &discordgo.MessageSend{Content: mentions, Embeds: []*discordgo.MessageEmbed{e}, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}}
	if data, err := a.renderWinner(*c); err == nil {
		msg.Files = []*discordgo.File{{Name: "champion.png", ContentType: "image/png", Reader: bytes.NewReader(data)}}
	} else {
		slog.Warn("component=tournament", "event", "champion_render_failed", "err", err.Error())
	}
	if _, err := a.sender.Send(t.DiscordChannelID, msg); err != nil {
		slog.Warn("component=tournament", "event", "champion_post_failed", "err", err.Error())
	}
}

func ordinalWord(n int) string {
	switch n {
	case 1:
		return "1st"
	case 2:
		return "2nd"
	case 3:
		return "3rd"
	}
	return fmt.Sprintf("%dth", n)
}
