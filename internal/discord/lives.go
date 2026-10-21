package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Lives (docs/LIVES.md): the /life command and the opt-in death recap DM. A life is the stretch
// between two deaths of one player on one server; every figure shown here is read from
// player_lives or derived by LifeRepository - nothing is estimated in this file.

// LifeStore is the read/write surface /life and the recap need.
type LifeStore interface {
	PlayerLives(ctx context.Context, guildID, serverID, playerID int64, limit int) ([]repository.Life, error)
	TopLives(ctx context.Context, guildID, serverID int64, metric string, since *time.Time, limit int) ([]repository.Life, error)
	CurrentLife(ctx context.Context, guildID, serverID, playerID int64, now time.Time) (*repository.CurrentLife, error)
	LongestAlive(ctx context.Context, guildID, serverID int64, now, activeSince time.Time, limit int) ([]repository.CurrentLife, error)
	Summary(ctx context.Context, guildID, serverID, playerID int64) (repository.LifeSummary, error)
	SetDeathRecap(ctx context.Context, guildID int64, discordUserID string, on bool) error
	DeathRecapEnabled(ctx context.Context, guildID int64, discordUserID string) (bool, error)
	DeathRecapRecipient(ctx context.Context, guildID, playerID int64) (string, bool, error)
}

// lifeAliveWindow is how recently a player must have been seen to rank on the "still alive" board.
const lifeAliveWindow = 14 * 24 * time.Hour

// FormatLifeDuration renders observed playtime: "47s", "12m", "3h 05m", "2d 4h".
func FormatLifeDuration(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh %02dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd %dh", seconds/86400, (seconds%86400)/3600)
	}
}

// formatTrackedDistance renders a tracked distance: "840 m", "12.4 km".
func formatTrackedDistance(meters float64) string {
	if meters < 1000 {
		return fmt.Sprintf("%.0f m", meters)
	}
	return fmt.Sprintf("%.1f km", meters/1000)
}

func lifeName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "Unknown"
	}
	return presentation.SafeName(name, presentation.MaxCardNameRunes)
}

// lifeEnding is the one-line cause of a life's end, using only what was recorded.
func lifeEnding(l repository.Life) string {
	switch l.Cause {
	case repository.LifeCausePVP:
		line := "Killed by **" + lifeName(l.KillerName) + "**"
		if w := strings.TrimSpace(l.Weapon); w != "" {
			line += " with " + presentation.EscapeMarkdown(presentation.CleanName(w, maxWeaponLen))
		}
		if l.DistanceM != nil {
			line += " from " + presentation.FormatDistance(*l.DistanceM)
		}
		return line
	case repository.LifeCauseSuicide:
		return "Suicide"
	default:
		return "Died (no killer recorded)"
	}
}

// BuildLifeRecapEmbed is the death recap card: what the life that just ended amounted to.
func BuildLifeRecapEmbed(l repository.Life, serverName string) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("🪦 Life recap", presentation.NeutralGraphite)
	// A direct message is read outside the server, so it names the server in the sentence.
	desc := "**" + lifeName(l.PlayerName) + "**"
	if s := strings.TrimSpace(serverName); s != "" {
		desc += " on " + presentation.SafeName(s, 60)
	}
	embed.Description = desc + "\n" + lifeEnding(l)
	if l.PlaytimeSeconds != nil {
		presentation.AppendFields(embed, presentation.MetricField("Survived", FormatLifeDuration(*l.PlaytimeSeconds)+" played", true))
	}
	kills := presentation.Plural(int64(l.Kills), "kill", "kills")
	if l.Headshots > 0 {
		kills += " (" + presentation.Plural(int64(l.Headshots), "headshot", "headshots") + ")"
	}
	presentation.AppendFields(embed, presentation.MetricField("Kills", kills, true))
	if l.LongestKillM != nil {
		presentation.AppendFields(embed, presentation.MetricField("Longest kill", presentation.FormatDistance(*l.LongestKillM), true))
	}
	if l.TrackedDistance != nil && *l.TrackedDistance >= 1 {
		presentation.AppendFields(embed, presentation.MetricField("Tracked distance", "at least "+formatTrackedDistance(*l.TrackedDistance), true))
	}
	embed.Footer = presentation.Footer("", "Turn these off with /life recap off")
	presentation.StampEmbed(embed, l.EndedAt)
	return presentation.FitEmbed(embed)
}

// BuildLifeProfileEmbed is /life me: the life in progress, the lifetime summary and recent lives.
func BuildLifeProfileEmbed(name string, cur *repository.CurrentLife, sum repository.LifeSummary, recent []repository.Life, recapOn bool) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed("🧬 Lives", presentation.Neutral)
	embed.Description = "**" + lifeName(name) + "**"
	if cur != nil {
		value := presentation.Plural(int64(cur.Kills), "kill", "kills") + " • began " + presentation.Timestamp(cur.StartedAt, 'R')
		if cur.PlaytimeSeconds != nil {
			value = FormatLifeDuration(*cur.PlaytimeSeconds) + " played • " + value
		}
		presentation.AppendFields(embed, presentation.MetricField("Current life", value, false))
	}
	if sum.Lives > 0 {
		parts := []string{presentation.Plural(int64(sum.Lives), "life", "lives") + " recorded"}
		if sum.LongestPlaytime != nil {
			parts = append(parts, "longest "+FormatLifeDuration(*sum.LongestPlaytime))
		}
		if sum.AveragePlaytime != nil {
			parts = append(parts, "average "+FormatLifeDuration(int64(*sum.AveragePlaytime)))
		}
		if sum.MostKills > 0 {
			parts = append(parts, "best "+presentation.Plural(int64(sum.MostKills), "kill", "kills"))
		}
		presentation.AppendFields(embed, presentation.MetricField("Record", strings.Join(parts, " • "), false))
	}
	if len(recent) > 0 {
		var b strings.Builder
		for _, l := range recent {
			played := "playtime not recorded"
			if l.PlaytimeSeconds != nil {
				played = FormatLifeDuration(*l.PlaytimeSeconds)
			}
			fmt.Fprintf(&b, "%s • %s • %s • %s\n", presentation.Timestamp(l.EndedAt, 'd'), played, presentation.Plural(int64(l.Kills), "kill", "kills"), lifeEnding(l))
		}
		presentation.AppendFields(embed, presentation.MetricField("Recent lives", presentation.Truncate(b.String(), presentation.LimitFieldValue), false))
	}
	if cur == nil && sum.Lives == 0 {
		embed.Description += "\nNo lives recorded on this server yet."
	}
	state := "Death recap DMs are off. Turn them on with /life recap on"
	if recapOn {
		state = "Death recap DMs are on"
	}
	embed.Footer = presentation.Footer("", state)
	return presentation.FitEmbed(embed)
}

// BuildLifeBoardEmbed renders one lives leaderboard from pre-formatted rows.
func BuildLifeBoardEmbed(title, subtitle string, rows []string) *discordgo.MessageEmbed {
	embed := presentation.NewFeedEmbed(title, presentation.ChampionGold)
	if len(rows) == 0 {
		embed.Description = subtitle + "\n\nNothing recorded yet."
		return embed
	}
	var b strings.Builder
	b.WriteString(subtitle + "\n\n")
	for i, r := range rows {
		fmt.Fprintf(&b, "`%2d.` %s\n", i+1, r)
	}
	embed.Description = presentation.Truncate(b.String(), presentation.LimitDescription)
	return presentation.FitEmbed(embed)
}

// LifeCommandHandler serves /life.
type LifeCommandHandler struct {
	lives  LifeStore
	guilds GuildStore
	// linkedPlayer resolves the caller's VERIFIED player id; server resolves the game server the
	// guild's public panels report on.
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool)
	server       func(ctx context.Context, guildRowID int64) (int64, bool)
	now          func() time.Time
}

func NewLifeCommandHandler(lives LifeStore, guilds GuildStore,
	linkedPlayer func(ctx context.Context, guildRowID int64, discordUserID string) (int64, string, bool),
	server func(ctx context.Context, guildRowID int64) (int64, bool)) *LifeCommandHandler {
	return &LifeCommandHandler{lives: lives, guilds: guilds, linkedPlayer: linkedPlayer, server: server, now: time.Now}
}

// RegisterLifeCommands registers /life.
func RegisterLifeCommands(session CommandRegistrar, guildID string) error {
	applicationID, err := ApplicationID(session)
	if err != nil {
		return err
	}
	cmd := &discordgo.ApplicationCommand{
		Name:        "life",
		Description: "Lives: how long you survive between deaths",
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "me", Description: "Your current life and recent lives"},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "top", Description: "Lives leaderboards", Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: "board", Description: "Which board", Required: false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "Still alive (longest current lives)", Value: "alive"},
					{Name: "Longest lives", Value: "longest"},
					{Name: "Deadliest lives", Value: "kills"},
					{Name: "Farthest travelled", Value: "distance"},
				},
			}}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "recap", Description: "Get a DM recap when you die", Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionString, Name: "state", Description: "on or off", Required: true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "on", Value: "on"}, {Name: "off", Value: "off"}},
			}}},
		},
	}
	_, err = session.ApplicationCommandCreate(applicationID, guildID, cmd)
	return err
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// Handle processes /life. Every response is ephemeral.
func (h *LifeCommandHandler) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if h == nil || i == nil || i.GuildID == "" || h.lives == nil || h.guilds == nil {
		respondEphemeral(s, i, ReplyAreUnavailable("Lives"))
		return
	}
	options := i.ApplicationCommandData().Options
	if len(options) == 0 {
		respondEphemeral(s, i, "Choose `/life me`, `/life top` or `/life recap`.")
		return
	}
	deferEphemeral(s, i)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, guildRowID, err := h.guilds.GetGuild(ctx, i.GuildID)
	if err != nil || guildRowID == 0 {
		respondEphemeral(s, i, ReplyNotSetUp)
		return
	}
	sub := options[0]
	userID := interactionUserID(i)

	if sub.Name == "recap" {
		on := len(sub.Options) > 0 && sub.Options[0].StringValue() == "on"
		if on {
			if _, _, linked := h.linkedPlayer(ctx, guildRowID, userID); !linked {
				respondEphemeral(s, i, ReplyNotLinked("Link your gamertag with `/link` first, so the bot knows which deaths are yours."))
				return
			}
		}
		if err := h.lives.SetDeathRecap(ctx, guildRowID, userID, on); err != nil {
			slog.Warn("component=lives", "event", "recap_pref_failed", "err", err.Error())
			respondEphemeral(s, i, ReplyCouldNot("save that"))
			return
		}
		if on {
			respondEphemeral(s, i, "Death recaps are **on**. You'll get a DM each time one of your lives ends. Your DMs from this server must be open.")
		} else {
			respondEphemeral(s, i, "Death recaps are **off**.")
		}
		return
	}

	serverID, ok := h.server(ctx, guildRowID)
	if !ok {
		respondEphemeral(s, i, ReplyNoServerSelected)
		return
	}
	switch sub.Name {
	case "me":
		playerID, name, linked := h.linkedPlayer(ctx, guildRowID, userID)
		if !linked {
			respondEphemeral(s, i, ReplyNotLinked("Link your gamertag with `/link` to see your lives."))
			return
		}
		cur, err1 := h.lives.CurrentLife(ctx, guildRowID, serverID, playerID, h.now())
		sum, err2 := h.lives.Summary(ctx, guildRowID, serverID, playerID)
		recent, err3 := h.lives.PlayerLives(ctx, guildRowID, serverID, playerID, 5)
		if err1 != nil || err2 != nil || err3 != nil {
			slog.Warn("component=lives", "event", "profile_failed", "errs", fmt.Sprint(err1, err2, err3))
			respondEphemeral(s, i, ReplyCouldNot("load your lives"))
			return
		}
		recapOn, _ := h.lives.DeathRecapEnabled(ctx, guildRowID, userID)
		respondLeaderboardEmbed(s, i, BuildLifeProfileEmbed(name, cur, sum, recent, recapOn))
	case "top":
		board := "alive"
		if len(sub.Options) > 0 {
			board = sub.Options[0].StringValue()
		}
		embed, err := h.board(ctx, guildRowID, serverID, board)
		if err != nil {
			slog.Warn("component=lives", "event", "board_failed", "board", board, "err", err.Error())
			respondEphemeral(s, i, ReplyCouldNot("load that board"))
			return
		}
		respondLeaderboardEmbed(s, i, embed)
	default:
		respondEphemeral(s, i, "Choose `/life me`, `/life top` or `/life recap`.")
	}
}

func (h *LifeCommandHandler) board(ctx context.Context, guildRowID, serverID int64, board string) (*discordgo.MessageEmbed, error) {
	now := h.now()
	switch board {
	case "longest", "kills", "distance":
		metric := map[string]string{"longest": repository.LifeMetricPlaytime, "kills": repository.LifeMetricKills, "distance": repository.LifeMetricDistance}[board]
		lives, err := h.lives.TopLives(ctx, guildRowID, serverID, metric, nil, 10)
		if err != nil {
			return nil, err
		}
		rows := make([]string, 0, len(lives))
		for _, l := range lives {
			rows = append(rows, LifeBoardRow(l, metric))
		}
		title := map[string]string{"longest": "⏳ Longest lives", "kills": "☠️ Deadliest lives", "distance": "🧭 Farthest travelled"}[board]
		subtitle := map[string]string{
			"longest":  "Most playtime between two deaths.",
			"kills":    "Most kills in a single life.",
			"distance": "Greatest tracked distance in a single life (a lower bound).",
		}[board]
		return BuildLifeBoardEmbed(title, subtitle, rows), nil
	default:
		alive, err := h.lives.LongestAlive(ctx, guildRowID, serverID, now, now.Add(-lifeAliveWindow), 10)
		if err != nil {
			return nil, err
		}
		rows := make([]string, 0, len(alive))
		for _, c := range alive {
			row := "**" + lifeName(c.PlayerName) + "** — " + FormatLifeDuration(*c.PlaytimeSeconds) + " • " + presentation.Plural(int64(c.Kills), "kill", "kills")
			if c.Online {
				row += " • 🟢"
			}
			rows = append(rows, row)
		}
		return BuildLifeBoardEmbed("🫀 Still alive", "Longest lives in progress, by playtime since the last death.", rows), nil
	}
}

// LifeBoardRow formats one ended life for a board ranked by metric.
func LifeBoardRow(l repository.Life, metric string) string {
	name := "**" + lifeName(l.PlayerName) + "** — "
	kills := presentation.Plural(int64(l.Kills), "kill", "kills")
	switch metric {
	case repository.LifeMetricKills:
		if l.PlaytimeSeconds != nil {
			return name + kills + " • " + FormatLifeDuration(*l.PlaytimeSeconds)
		}
		return name + kills
	case repository.LifeMetricDistance:
		if l.TrackedDistance != nil {
			return name + formatTrackedDistance(*l.TrackedDistance) + " • " + kills
		}
		return name + kills
	default:
		if l.PlaytimeSeconds != nil {
			return name + FormatLifeDuration(*l.PlaytimeSeconds) + " • " + kills
		}
		return name + kills
	}
}

// LifeRecapNotifier DMs the opt-in death recap. Notify never blocks: recaps go through a bounded
// queue drained by Run, and a full queue drops the recap (the life itself is already recorded).
type LifeRecapNotifier struct {
	session    *discordgo.Session
	lives      LifeStore
	serverName func(serverID int64) string
	queue      chan repository.Life
	dropped    atomic.Int64
	sent       atomic.Int64
}

func NewLifeRecapNotifier(session *discordgo.Session, lives LifeStore, serverName func(serverID int64) string) *LifeRecapNotifier {
	return &LifeRecapNotifier{session: session, lives: lives, serverName: serverName, queue: make(chan repository.Life, 64)}
}

// Notify queues a recap for an ended life.
func (n *LifeRecapNotifier) Notify(l repository.Life) {
	if n == nil {
		return
	}
	select {
	case n.queue <- l:
	default:
		n.dropped.Add(1)
	}
}

// Stats reports recaps sent and dropped since start.
func (n *LifeRecapNotifier) Stats() (sent, dropped int64) {
	if n == nil {
		return 0, 0
	}
	return n.sent.Load(), n.dropped.Load()
}

// Run delivers queued recaps until ctx ends.
func (n *LifeRecapNotifier) Run(ctx context.Context) {
	if n == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case l := <-n.queue:
			n.deliver(ctx, l)
		}
	}
}

func (n *LifeRecapNotifier) deliver(ctx context.Context, l repository.Life) {
	if n.session == nil || n.lives == nil {
		return
	}
	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	userID, ok, err := n.lives.DeathRecapRecipient(lookup, l.GuildID, l.PlayerID)
	cancel()
	if err != nil {
		slog.Warn("component=lives", "event", "recap_recipient_failed", "err", err.Error())
		return
	}
	if !ok {
		return
	}
	name := ""
	if n.serverName != nil {
		name = n.serverName(l.ServerID)
	}
	channel, err := n.session.UserChannelCreate(userID)
	if err != nil {
		slog.Debug("component=lives", "event", "recap_dm_unavailable", "err", err.Error())
		return
	}
	embed := BuildLifeRecapEmbed(l, name)
	if reader, ok := n.lives.(lifeBestReader); ok && l.PlaytimeSeconds != nil {
		bestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		best, err := reader.LongestOtherLife(bestCtx, l.GuildID, l.ServerID, l.PlayerID, l.ID)
		cancel()
		if err == nil {
			embed = withPersonalBest(embed, *l.PlaytimeSeconds, best)
		}
	}
	if _, err := n.session.ChannelMessageSendComplex(channel.ID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}); err != nil {
		// Closed DMs are the player's choice, not a fault.
		slog.Debug("component=lives", "event", "recap_dm_failed", "err", err.Error())
		return
	}
	n.sent.Add(1)
}

// lifeBestReader is the optional store method the recap uses to compare a life with the player's best.
type lifeBestReader interface {
	LongestOtherLife(ctx context.Context, guildID, serverID, playerID, lifeID int64) (*int64, error)
}

// withPersonalBest adds how the life compares with the player's longest one.
func withPersonalBest(embed *discordgo.MessageEmbed, played int64, best *int64) *discordgo.MessageEmbed {
	switch {
	case best == nil || played > *best:
		presentation.AppendFields(embed, presentation.MetricField("Personal best", "🏆 Your longest life yet!", false))
	default:
		presentation.AppendFields(embed, presentation.MetricField("Personal best", FormatLifeDuration(*best)+" (this one: "+FormatLifeDuration(played)+")", false))
	}
	return embed
}
