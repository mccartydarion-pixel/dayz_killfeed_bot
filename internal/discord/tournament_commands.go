package discord

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/tournament"
)

// TournamentCommandHandler serves /tournament (docs/TOURNAMENTS.md):
//
//	admin:  create, open, start, pause, resume, cancel, result, replay, dq, call
//	player: join [partner], leave, checkin, status, bracket
//
// Admin means Administrator or Manage Server (isAdminInteraction), as for every other admin
// command. Players need a VERIFIED gamertag link. The sign-up card's Join, Leave and Check in
// buttons go through the same code (HandleComponent). Every reply is private except the
// bracket, which is posted for the channel.
type TournamentCommandHandler struct {
	svc    *tournament.Service
	guilds GuildStore
	// server is the guild's selected public server (the tournament's server).
	server func(ctx context.Context, guildRowID int64) (int64, bool)
	// target resolves the server to its installation (0 when the server backs none).
	target func(ctx context.Context, serverID int64) (installationID int64, serverName string)
	// current is the id of the tournament a player can act on for the server (0 when none).
	current func(ctx context.Context, serverID int64) (int64, error)
	// linkedPlayer resolves a Discord user to their VERIFIED player on the guild.
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool)
	// defaultArenas are the arenas a new tournament gets when the command names none (the
	// stadium, when one is built).
	defaultArenas func(ctx context.Context, installationID int64) []tournament.Arena
	// bracket renders the bracket image for /tournament bracket.
	bracket func(ctx context.Context, t *tournament.Tournament) ([]byte, error)
	// draft finds the server's newest DRAFT (what /tournament open opens when nothing is open).
	draft func(ctx context.Context, serverID int64) (int64, error)
}

func NewTournamentCommandHandler(svc *tournament.Service, guilds GuildStore,
	server func(ctx context.Context, guildRowID int64) (int64, bool),
	target func(ctx context.Context, serverID int64) (int64, string),
	current func(ctx context.Context, serverID int64) (int64, error),
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool),
	defaultArenas func(ctx context.Context, installationID int64) []tournament.Arena,
	bracket func(ctx context.Context, t *tournament.Tournament) ([]byte, error)) *TournamentCommandHandler {
	return &TournamentCommandHandler{svc: svc, guilds: guilds, server: server, target: target, current: current, linkedPlayer: linkedPlayer, defaultArenas: defaultArenas, bracket: bracket}
}

// RegisterTournamentCommands registers /tournament.
func RegisterTournamentCommands(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	str := func(name, desc string, required bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Name: name, Description: desc, Type: discordgo.ApplicationCommandOptionString, Required: required}
	}
	num := func(name, desc string, required bool, choices ...int) *discordgo.ApplicationCommandOption {
		o := &discordgo.ApplicationCommandOption{Name: name, Description: desc, Type: discordgo.ApplicationCommandOptionInteger, Required: required}
		for _, c := range choices {
			o.Choices = append(o.Choices, &discordgo.ApplicationCommandOptionChoice{Name: strconv.Itoa(c), Value: c})
		}
		return o
	}
	sub := func(name, desc string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Name: name, Description: desc, Type: discordgo.ApplicationCommandOptionSubCommand, Options: opts}
	}
	seeding := &discordgo.ApplicationCommandOption{Name: "seeding", Description: "How the bracket is drawn", Type: discordgo.ApplicationCommandOptionString,
		Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "Random draw", Value: "RANDOM"}, {Name: "By Ranked points", Value: "RANKED"}}}
	cmd := &discordgo.ApplicationCommand{Name: "tournament", Description: "Tournaments: sign up, check in, and (admins) run them", Options: []*discordgo.ApplicationCommandOption{
		sub("create", "Admin: create a tournament in this channel",
			str("name", "Tournament name", true),
			str("start", "Start time, UTC (2026-10-10 19:00) or from now (90m, 2h)", true),
			num("team_size", "1v1 or 2v2", false, 1, 2),
			num("bracket_size", "Slots", false, 4, 8, 16, 32),
			num("best_of", "Rounds per match", false, 1, 3, 5),
			seeding,
			str("weapons", "Allowed weapons, comma-separated (empty: any)", false),
			str("prizes", "Champion Points for 1st, 2nd, 3rd... comma-separated (1000,500,250)", false),
			str("title", "Title the winner earns", false),
			num("checkin_minutes", "Check-in window before the start (default 30)", false),
			str("arena", "Arena as x,z,radius (default: the stadium)", false)),
		sub("open", "Admin: open sign-up"),
		sub("start", "Admin: draw the bracket and start now"),
		sub("pause", "Admin: pause (kills stop counting)"),
		sub("resume", "Admin: resume"),
		sub("cancel", "Admin: cancel the tournament"),
		sub("call", "Admin: call the next match again"),
		sub("result", "Admin: decide a match", num("match", "Match number (see /tournament status)", true), str("winner", "Winning player's name", true), str("note", "Why", false)),
		sub("replay", "Admin: replay a match", num("match", "Match number (see /tournament status)", true)),
		sub("dq", "Admin: disqualify a player", str("player", "Player name", true), str("reason", "Why", false)),
		sub("join", "Enter the tournament", &discordgo.ApplicationCommandOption{Name: "partner", Description: "Your partner (2v2)", Type: discordgo.ApplicationCommandOptionUser}),
		sub("leave", "Withdraw your entry"),
		sub("checkin", "Check in for the draw"),
		sub("status", "Where the tournament stands"),
		sub("bracket", "Post the bracket"),
	}}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

// Handle dispatches /tournament.
func (h *TournamentCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.svc == nil || i == nil || i.Member == nil || i.Member.User == nil || i.GuildID == "" {
		respondEphemeral(s, i, ReplyAreUnavailable("Tournaments"))
		return
	}
	data := i.ApplicationCommandData()
	if len(data.Options) == 0 {
		respondEphemeral(s, i, ReplyChooseSubcommand)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sub := data.Options[0]
	if sub.Name == "bracket" {
		h.postBracket(ctx, s, i)
		return
	}
	respondEphemeral(s, i, h.dispatch(ctx, i, sub.Name, sub, nil))
}

// HandleComponent serves the sign-up card's buttons.
func (h *TournamentCommandHandler) HandleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || h.svc == nil || i == nil || i.Member == nil || i.Member.User == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Tournaments"))
		return
	}
	id := i.MessageComponentData().CustomID
	action, rest, _ := strings.Cut(strings.TrimPrefix(id, TournamentComponentPrefix), ":")
	tid, _ := strconv.ParseInt(rest, 10, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	respondEphemeral(s, i, h.dispatch(ctx, i, action, nil, &tid))
}

// tournamentContext is what every subcommand needs: the guild, its server and installation.
type tournamentContext struct {
	guildRowID     int64
	serverID       int64
	installationID int64
	serverName     string
}

func (h *TournamentCommandHandler) resolve(ctx context.Context, i *discordgo.InteractionCreate) (tournamentContext, string) {
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		return tournamentContext{}, ReplyNotSetUp
	}
	serverID, ok := h.server(ctx, guildRowID)
	if !ok {
		return tournamentContext{}, ReplyNoServerSelected
	}
	tc := tournamentContext{guildRowID: guildRowID, serverID: serverID}
	if h.target != nil {
		tc.installationID, tc.serverName = h.target(ctx, serverID)
	}
	if tc.installationID == 0 {
		return tournamentContext{}, "This DayZ server is not connected to a Champion installation yet, so it cannot host a tournament."
	}
	return tc, ""
}

// currentID is the tournament to act on: the button's, else the server's current one.
func (h *TournamentCommandHandler) currentID(ctx context.Context, tc tournamentContext, fixed *int64) (int64, string) {
	if fixed != nil && *fixed > 0 {
		return *fixed, ""
	}
	id, err := h.current(ctx, tc.serverID)
	if err != nil {
		return 0, ReplyCouldNot("find the tournament")
	}
	if id == 0 {
		return 0, "There is no tournament running or open for sign-up right now."
	}
	return id, ""
}

// dispatch runs one action and returns the private reply.
func (h *TournamentCommandHandler) dispatch(ctx context.Context, i *discordgo.InteractionCreate, action string, sub *discordgo.ApplicationCommandInteractionDataOption, fixed *int64) string {
	tc, msg := h.resolve(ctx, i)
	if msg != "" {
		return msg
	}
	user := i.Member.User.ID
	switch action {
	case "create", "open", "start", "pause", "resume", "cancel", "call", "result", "replay", "dq":
		if !isAdminInteraction(i) {
			return ReplyNeedsPermission("run tournaments")
		}
	}
	switch action {
	case "create":
		return h.create(ctx, i, tc, sub)
	case "open", "start", "pause", "resume", "cancel", "call":
		return h.admin(ctx, tc, action, user)
	case "result":
		return h.result(ctx, tc, sub, user)
	case "replay":
		id, msg := h.currentID(ctx, tc, nil)
		if msg != "" {
			return msg
		}
		t, err := h.svc.Get(ctx, id)
		if err != nil {
			return ReplyCouldNot("load the tournament")
		}
		m := MatchByNumber(t, int(optionInt(sub, "match")))
		if m == nil {
			return "No match has that number. `/tournament status` lists them."
		}
		if _, err := h.svc.Replay(ctx, id, m.ID, user); err != nil {
			return tournamentError("replay the match", err)
		}
		return "🔁 " + MatchLabel(t, m) + " will be replayed."
	case "dq":
		return h.dq(ctx, tc, sub, user)
	case "join":
		return h.join(ctx, i, tc, sub, fixed)
	case "leave":
		id, msg := h.currentID(ctx, tc, fixed)
		if msg != "" {
			return msg
		}
		if _, err := h.svc.Leave(ctx, id, user); err != nil {
			return tournamentError("withdraw", err)
		}
		return "You have withdrawn from the tournament."
	case "checkin":
		id, msg := h.currentID(ctx, tc, fixed)
		if msg != "" {
			return msg
		}
		if _, err := h.svc.Checkin(ctx, id, user); err != nil {
			return tournamentError("check in", err)
		}
		return "✅ You are checked in. Be ready at the start."
	case "status":
		return h.status(ctx, tc, user)
	}
	return ReplyChooseSubcommand
}

// tournamentError turns an engine error into a reply.
func tournamentError(action string, err error) string {
	switch {
	case errors.Is(err, tournament.ErrAlreadyEntered):
		return "You are already entered."
	case errors.Is(err, tournament.ErrNotEntered):
		return "You are not entered in this tournament."
	case errors.Is(err, tournament.ErrFull):
		return "The bracket is full."
	case errors.Is(err, tournament.ErrPartner):
		return "This is a 2v2 tournament: run `/tournament join` with your partner (`partner: @name`). Your partner needs a linked gamertag too."
	case errors.Is(err, tournament.ErrNotEnough):
		return "At least two checked-in entries are needed to start."
	case errors.Is(err, tournament.ErrNoMatch):
		return "There is no match to call right now."
	case errors.Is(err, tournament.ErrWrongStatus), errors.Is(err, tournament.ErrInvalid):
		return ReplyCouldNotBecause(action, strings.TrimPrefix(strings.TrimPrefix(err.Error(), tournament.ErrWrongStatus.Error()+": "), tournament.ErrInvalid.Error()+": "))
	case errors.Is(err, tournament.ErrNotFound):
		return "That tournament no longer exists."
	}
	return ReplyCouldNot(action)
}

// --- admin --------------------------------------------------------------------------------------------

// parseStart reads the start option: "2026-10-10 19:00" (UTC), "19:00" (today, UTC, or
// tomorrow when past), or a delay from now ("90m", "2h", "1h30m").
func parseStart(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if d, err := time.ParseDuration(strings.ToLower(raw)); err == nil && d > 0 {
		return now.Add(d).Truncate(time.Minute), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", time.RFC3339, "2006-01-02 15:04 MST"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	if t, err := time.Parse("15:04", raw); err == nil {
		at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
		if !at.After(now) {
			at = at.Add(24 * time.Hour)
		}
		return at, nil
	}
	return time.Time{}, fmt.Errorf("start must be a UTC time like 2026-10-10 19:00, a clock time like 19:00, or a delay like 90m")
}

func (h *TournamentCommandHandler) create(ctx context.Context, i *discordgo.InteractionCreate, tc tournamentContext, sub *discordgo.ApplicationCommandInteractionDataOption) string {
	now := time.Now().UTC()
	startsAt, err := parseStart(optionString(sub, "start"), now)
	if err != nil {
		return ReplyCouldNotBecause("create the tournament", err.Error())
	}
	if !startsAt.After(now) {
		return ReplyCouldNotBecause("create the tournament", "the start must be in the future")
	}
	p := tournament.Params{Name: optionString(sub, "name"), TeamSize: int(optionInt(sub, "team_size")), BracketSize: int(optionInt(sub, "bracket_size")), BestOf: int(optionInt(sub, "best_of")),
		Seeding: optionString(sub, "seeding"), StartsAt: startsAt, CheckinMinutes: tournament.DefaultCheckin}
	if v := optionInt(sub, "checkin_minutes"); v > 0 || hasOption(sub, "checkin_minutes") {
		p.CheckinMinutes = int(v)
	}
	for _, w := range strings.Split(optionString(sub, "weapons"), ",") {
		if w = strings.TrimSpace(w); w != "" {
			p.Rules.AllowedWeapons = append(p.Rules.AllowedWeapons, w)
		}
	}
	title := strings.TrimSpace(optionString(sub, "title"))
	for n, raw := range strings.Split(optionString(sub, "prizes"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		pts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || pts < 0 {
			return ReplyCouldNotBecause("create the tournament", "prizes must be whole numbers of points, like 1000,500,250")
		}
		prize := tournament.Prize{Place: n + 1, Points: pts}
		if n == 0 && title != "" {
			prize.Title = &title
		}
		p.Prizes = append(p.Prizes, prize)
	}
	if title != "" && len(p.Prizes) == 0 {
		p.Prizes = []tournament.Prize{{Place: 1, Title: &title}}
	}
	if raw := strings.TrimSpace(optionString(sub, "arena")); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) != 3 {
			return ReplyCouldNotBecause("create the tournament", "arena must be x,z,radius like 4618,10439,150")
		}
		var vals [3]float64
		for k, part := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil {
				return ReplyCouldNotBecause("create the tournament", "arena must be x,z,radius like 4618,10439,150")
			}
			vals[k] = v
		}
		p.Rules.Arenas = []tournament.Arena{{No: 1, Name: "Arena 1", X: vals[0], Z: vals[1], Radius: vals[2]}}
	} else if h.defaultArenas != nil {
		p.Rules.Arenas = h.defaultArenas(ctx, tc.installationID)
	}
	t := &tournament.Tournament{InstallationID: tc.installationID, GuildID: tc.guildRowID, ServerID: tc.serverID, DiscordChannelID: i.ChannelID, CreatedByDiscordID: i.Member.User.ID}
	created, err := h.svc.Create(ctx, t, p)
	if err != nil {
		return tournamentError("create the tournament", err)
	}
	lines := []string{fmt.Sprintf("🏆 **%s** is created (draft). It starts %s.", presentation.CleanName(created.Name, 80), presentation.TimestampWithRelative(created.StartsAt))}
	if len(created.Rules.Arenas) > 0 {
		a := created.Rules.Arenas[0]
		lines = append(lines, fmt.Sprintf("Arena: %s at %.0f, %.0f within %.0f m.", presentation.CleanName(a.Name, 40), a.X, a.Z, a.Radius))
	} else {
		lines = append(lines, "No arena is set: kills anywhere on the map count. Build the stadium on the website or pass `arena: x,z,radius` to limit them.")
	}
	lines = append(lines, "Run `/tournament open` to post the sign-up card in this channel.")
	return strings.Join(lines, "\n")
}

func hasOption(o *discordgo.ApplicationCommandInteractionDataOption, name string) bool {
	if o == nil {
		return false
	}
	for _, v := range o.Options {
		if v.Name == name {
			return true
		}
	}
	return false
}

// admin runs the one-word admin actions.
func (h *TournamentCommandHandler) admin(ctx context.Context, tc tournamentContext, action, user string) string {
	id, err := h.current(ctx, tc.serverID)
	if err != nil {
		return ReplyCouldNot("find the tournament")
	}
	if id == 0 && action == "open" {
		// The newest draft of the server.
		id, err = h.latestDraft(ctx, tc)
		if err != nil || id == 0 {
			return "There is no tournament to open. Create one with `/tournament create`."
		}
	}
	if id == 0 {
		return "There is no tournament running or open for sign-up right now."
	}
	var t *tournament.Tournament
	switch action {
	case "open":
		t, err = h.svc.Open(ctx, id)
	case "start":
		t, err = h.svc.Start(ctx, id)
	case "pause":
		t, err = h.svc.Pause(ctx, id)
	case "resume":
		t, err = h.svc.Resume(ctx, id)
	case "cancel":
		t, err = h.svc.Cancel(ctx, id, "An admin cancelled the tournament.")
	case "call":
		t, err = h.svc.Call(ctx, id)
	}
	if err != nil {
		return tournamentError(action+" the tournament", err)
	}
	switch action {
	case "open":
		return "Sign-up is open. The card is posted in the tournament's channel."
	case "start":
		m := t.Current()
		if m == nil {
			return "The tournament started."
		}
		return "The bracket is drawn and " + MatchLabel(t, m) + " is called."
	case "pause":
		return "The tournament is paused. Kills do not count until `/tournament resume`."
	case "resume":
		return "The tournament is running again."
	case "cancel":
		return "The tournament is cancelled."
	case "call":
		if m := t.Current(); m != nil {
			return MatchLabel(t, m) + " is called again; the players are mentioned in the channel."
		}
		return "The next match is called."
	}
	return ""
}

// latestDraft finds the server's newest draft (what /tournament open opens).
func (h *TournamentCommandHandler) latestDraft(ctx context.Context, tc tournamentContext) (int64, error) {
	if h.draft == nil {
		return 0, nil
	}
	return h.draft(ctx, tc.serverID)
}

// SetDraftLookup sets how the newest DRAFT of a server is found.
func (h *TournamentCommandHandler) SetDraftLookup(fn func(ctx context.Context, serverID int64) (int64, error)) {
	h.draft = fn
}

func (h *TournamentCommandHandler) result(ctx context.Context, tc tournamentContext, sub *discordgo.ApplicationCommandInteractionDataOption, user string) string {
	id, msg := h.currentID(ctx, tc, nil)
	if msg != "" {
		return msg
	}
	t, err := h.svc.Get(ctx, id)
	if err != nil {
		return ReplyCouldNot("load the tournament")
	}
	m := MatchByNumber(t, int(optionInt(sub, "match")))
	if m == nil {
		return "No match has that number. `/tournament status` lists them."
	}
	winner := findEntryByName(t, optionString(sub, "winner"))
	if winner == nil || !m.Has(winner.ID) {
		return "The winner must be one of the players in that match. Give the name as it appears in the bracket."
	}
	if _, err := h.svc.Result(ctx, id, m.ID, winner.ID, user, strings.TrimSpace(optionString(sub, "note"))); err != nil {
		return tournamentError("record the result", err)
	}
	return fmt.Sprintf("Recorded: **%s** wins %s.", EntryName(t, &winner.ID), MatchLabel(t, m))
}

func (h *TournamentCommandHandler) dq(ctx context.Context, tc tournamentContext, sub *discordgo.ApplicationCommandInteractionDataOption, user string) string {
	id, msg := h.currentID(ctx, tc, nil)
	if msg != "" {
		return msg
	}
	t, err := h.svc.Get(ctx, id)
	if err != nil {
		return ReplyCouldNot("load the tournament")
	}
	e := findEntryByName(t, optionString(sub, "player"))
	if e == nil {
		return "No entry has a player with that name."
	}
	if _, err := h.svc.DQ(ctx, id, e.ID, user, strings.TrimSpace(optionString(sub, "reason"))); err != nil {
		return tournamentError("disqualify the player", err)
	}
	return fmt.Sprintf("**%s** is disqualified.", EntryName(t, &e.ID))
}

// findEntryByName finds the entry one of whose players has the name (case-insensitive).
func findEntryByName(t *tournament.Tournament, name string) *tournament.Entry {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return nil
	}
	for _, e := range t.Entries {
		for _, p := range e.Players {
			if strings.ToLower(strings.TrimSpace(p.Name)) == name {
				return e
			}
		}
	}
	return nil
}

// --- players --------------------------------------------------------------------------------------------

func (h *TournamentCommandHandler) join(ctx context.Context, i *discordgo.InteractionCreate, tc tournamentContext, sub *discordgo.ApplicationCommandInteractionDataOption, fixed *int64) string {
	user := i.Member.User.ID
	playerID, name, linked := h.linkedPlayer(ctx, tc.guildRowID, user)
	if !linked {
		return ReplyNotLinked("Link your gamertag with `/link` to enter a tournament.")
	}
	id, msg := h.currentID(ctx, tc, fixed)
	if msg != "" {
		return msg
	}
	players := []tournament.EntryPlayer{{PlayerID: playerID, DiscordUserID: user, Name: name}}
	if sub != nil {
		if partnerID := optionUserID(sub, "partner"); partnerID != "" {
			if partnerID == user {
				return "Your partner must be another player."
			}
			pid, pname, ok := h.linkedPlayer(ctx, tc.guildRowID, partnerID)
			if !ok {
				return "Your partner has not linked a gamertag yet. They need to run `/link` first."
			}
			players = append(players, tournament.EntryPlayer{PlayerID: pid, DiscordUserID: partnerID, Name: pname})
		}
	}
	t, e, err := h.svc.Join(ctx, id, players)
	if err != nil {
		return tournamentError("enter the tournament", err)
	}
	if e.CheckedIn() {
		return fmt.Sprintf("You are in **%s** and checked in. Be ready at %s.", presentation.CleanName(t.Name, 80), presentation.Timestamp(t.StartsAt, 't'))
	}
	return fmt.Sprintf("You are in **%s**. Check in when check-in opens (%s) or you will miss the draw.", presentation.CleanName(t.Name, 80), presentation.Timestamp(t.CheckinOpensAt(), 't'))
}

func optionUserID(o *discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, v := range o.Options {
		if v.Name == name {
			if s, ok := v.Value.(string); ok {
				return s
			}
		}
	}
	return ""
}

func (h *TournamentCommandHandler) status(ctx context.Context, tc tournamentContext, user string) string {
	id, msg := h.currentID(ctx, tc, nil)
	if msg != "" {
		return msg
	}
	t, err := h.svc.Get(ctx, id)
	if err != nil {
		return ReplyCouldNot("load the tournament")
	}
	return StatusText(t, user)
}

// StatusText is /tournament status: where the tournament stands and where the caller is in it.
func StatusText(t *tournament.Tournament, discordUserID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🏆 **%s** · %s\n", presentation.CleanName(t.Name, 80), strings.ToLower(t.Status))
	switch t.Status {
	case tournament.StatusSignup:
		fmt.Fprintf(&b, "Sign-up is open; it starts %s.\n", presentation.TimestampWithRelative(t.StartsAt))
	case tournament.StatusCheckin:
		fmt.Fprintf(&b, "Check-in is open; it starts %s.\n", presentation.TimestampWithRelative(t.StartsAt))
	}
	if e := t.EntryOfDiscordUser(discordUserID); e != nil {
		switch {
		case e.Status == tournament.EntryWithdrawn:
			b.WriteString("You withdrew.\n")
		case e.Status == tournament.EntryDQ:
			b.WriteString("You were disqualified.\n")
		case e.Status == tournament.EntryWinner:
			b.WriteString("You won. 👑\n")
		case e.Status == tournament.EntryEliminated:
			b.WriteString("You are out of the running.\n")
		case !t.Running() && !e.CheckedIn():
			b.WriteString("You are entered but not checked in yet.\n")
		case !t.Running():
			b.WriteString("You are entered and checked in.\n")
		default:
			next := nextMatchOf(t, e.ID)
			if next != nil {
				opp := next.EntryB
				if opp != nil && *opp == e.ID {
					opp = next.EntryA
				}
				fmt.Fprintf(&b, "Your next match: %s against %s (%s).\n", MatchLabel(t, next), EntryName(t, opp), strings.ToLower(next.Status))
			}
		}
	}
	if t.Running() {
		if m := t.Current(); m != nil {
			fmt.Fprintf(&b, "Now: %s, **%s** %d–%d **%s** (%s).\n", MatchLabel(t, m), EntryName(t, m.EntryA), m.ScoreA, m.ScoreB, EntryName(t, m.EntryB), strings.ToLower(m.Status))
		}
		b.WriteString("\n")
		for _, m := range t.Matches {
			if m.EntryA == nil && m.EntryB == nil {
				continue
			}
			mark := "▫️"
			switch m.Status {
			case tournament.MatchLive:
				mark = "🟢"
			case tournament.MatchCalled:
				mark = "📣"
			case tournament.MatchDone, tournament.MatchForfeit:
				mark = "✅"
			}
			fmt.Fprintf(&b, "%s %s: %s %d–%d %s\n", mark, MatchLabel(t, m), EntryName(t, m.EntryA), m.ScoreA, m.ScoreB, EntryName(t, m.EntryB))
		}
	} else if !t.Over() {
		n := 0
		for _, e := range t.Entries {
			if e.Status == tournament.EntryActive {
				n++
			}
		}
		fmt.Fprintf(&b, "%d of %d slots taken.\n", n, t.BracketSize)
	}
	if c := t.Champion(); c != nil {
		fmt.Fprintf(&b, "Champion: **%s**.\n", EntryName(t, &c.ID))
	}
	out := b.String()
	if len(out) > 1900 {
		out = out[:1900] + "…"
	}
	return out
}

// nextMatchOf is the entry's next unfinished match.
func nextMatchOf(t *tournament.Tournament, entryID int64) *tournament.Match {
	for _, m := range t.Matches {
		if m.Has(entryID) && !m.Finished() {
			return m
		}
	}
	return nil
}

func (h *TournamentCommandHandler) postBracket(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate) {
	tc, msg := h.resolve(ctx, i)
	if msg != "" {
		respondEphemeral(s, i, msg)
		return
	}
	id, msg := h.currentID(ctx, tc, nil)
	if msg != "" {
		respondEphemeral(s, i, msg)
		return
	}
	t, err := h.svc.Get(ctx, id)
	if err != nil {
		respondEphemeral(s, i, ReplyCouldNot("load the tournament"))
		return
	}
	if !t.Running() && !t.Over() {
		respondEphemeral(s, i, "The bracket is drawn at the start. "+StatusText(t, i.Member.User.ID))
		return
	}
	if h.bracket == nil || !deferPublic(s, i) {
		respondEphemeral(s, i, StatusText(t, i.Member.User.ID))
		return
	}
	data, err := h.bracket(ctx, t)
	if err != nil {
		text := ReplyCouldNot("draw the bracket")
		_ = editDeferred(s, i, &discordgo.WebhookEdit{Content: &text})
		return
	}
	caption := "🏆 **" + presentation.CleanName(t.Name, 80) + "** · " + bracketCaption(t)
	_ = editDeferred(s, i, &discordgo.WebhookEdit{Content: &caption, Files: []*discordgo.File{{Name: "bracket.png", ContentType: "image/png", Reader: strings.NewReader(string(data))}}, AllowedMentions: noMentions})
}
