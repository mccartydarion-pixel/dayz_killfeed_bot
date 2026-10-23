package discord

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// The panel's tools row (server_panel.go): Nitrado's game settings, the newest ADM log, the
// restart schedule, the maintenance switch, and posting a shared panel in a channel.

// PanelSetting is one of Nitrado's on/off game settings.
type PanelSetting struct {
	ID, Label, Help string
	Enabled         bool
}

// PanelTask is one of the server's scheduled tasks on Nitrado.
type PanelTask struct {
	// Action is Nitrado's action ("restart", "stop", ...).
	Action string
	// Next is the next run, nil when Nitrado gives none.
	Next *time.Time
}

// PanelLog is the end of the newest ADM log, already redacted.
type PanelLog struct {
	Name string
	Size int64
	Text string
	// Earlier is true when the file is longer than Text.
	Earlier bool
}

// PanelPost is a panel posted in a channel.
type PanelPost struct {
	MessageID, ChannelID, DiscordGuildID string
	InstallationID                       int64
}

const serverPanelSettingID = "srvpanel:setting:"

// panelSharedEvery is how often posted panels are brought up to date.
const panelSharedEvery = 2 * time.Minute

// handleTool answers the tools row and the messages it opens. It reports false for a custom id
// that is not one of them.
func (h *ServerPanelHandler) handleTool(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, rest string) bool {
	verb, id, ok := strings.Cut(rest, ":")
	if !ok {
		return false
	}
	installationID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || installationID <= 0 {
		return false
	}
	switch verb {
	case "settings":
		h.sendSettings(ctx, s, i, caller, installationID)
	case "setting":
		h.changeSetting(ctx, s, i, caller, installationID)
	case "logs":
		h.sendLog(ctx, s, i, caller, installationID)
	case "schedule":
		h.sendSchedule(ctx, s, i, caller, installationID)
	case "maint":
		h.askMaintenance(ctx, s, i, caller, installationID)
	case "maint-on", "maint-off":
		h.setMaintenance(ctx, s, i, caller, installationID, verb == "maint-on")
	case "post":
		h.post(ctx, s, i, caller, installationID)
	default:
		return false
	}
	return true
}

// serverOf is installationID's server of the caller's Discord server.
func (h *ServerPanelHandler) serverOf(ctx context.Context, caller PanelCaller, installationID int64) (PanelServer, error) {
	servers, err := h.backend.PanelServers(ctx, caller.DiscordGuildID)
	if err != nil {
		return PanelServer{}, err
	}
	for _, srv := range servers {
		if srv.InstallationID == installationID {
			return srv, nil
		}
	}
	return PanelServer{}, ErrPanelServerNotFound
}

// requireLevel reports whether the caller holds at least min on installationID, answering them
// when not.
func (h *ServerPanelHandler) requireLevel(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64, min permissions.Level) bool {
	level, err := h.backend.PanelLevel(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return false
	}
	if level < min {
		h.reportError(s, i, ErrPanelForbidden)
		return false
	}
	return true
}

// --- game settings ---------------------------------------------------------------------------

func (h *ServerPanelHandler) sendSettings(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	srv, err := h.serverOf(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	settings, err := h.backend.PanelSettings(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondPrivate(s, i, BuildPanelSettings(installationID, srv.Name, settings))
}

// changeSetting applies a choice from the settings menu ("<id>:on" or "<id>:off").
func (h *ServerPanelHandler) changeSetting(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	values := i.MessageComponentData().Values
	if len(values) != 1 {
		return
	}
	id, state, ok := strings.Cut(values[0], ":")
	if !ok || (state != "on" && state != "off") {
		return
	}
	deferAs(s, i, AckUpdate)
	if err := h.backend.PanelSetSetting(ctx, caller, installationID, id, state == "on"); err != nil {
		h.reportError(s, i, err)
		return
	}
	// The change is made; a list that can't be read again just stays as it was shown.
	label := id
	if settings, err := h.backend.PanelSettings(ctx, caller, installationID); err == nil {
		for _, st := range settings {
			if st.ID == id {
				label = st.Label
			}
		}
		if srv, err := h.serverOf(ctx, caller, installationID); err == nil {
			respondUpdate(s, i, BuildPanelSettings(installationID, srv.Name, settings))
		}
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{
		Content: fmt.Sprintf("⚙️ **%s** is now **%s**. Nitrado applies it when the server next restarts.", presentation.EscapeMarkdown(label), state),
	})
}

// BuildPanelSettings lists Nitrado's on/off settings and a menu that turns one on or off. The
// storage wipe is left to the website, behind its typed confirmation.
func BuildPanelSettings(installationID int64, serverName string, settings []PanelSetting) *discordgo.InteractionResponseData {
	var b strings.Builder
	options := make([]discordgo.SelectMenuOption, 0, len(settings))
	for _, st := range settings {
		mark, verb, to := "⬜", "Turn on", "on"
		if st.Enabled {
			mark, verb, to = "✅", "Turn off", "off"
		}
		fmt.Fprintf(&b, "%s **%s**\n%s\n", mark, presentation.EscapeMarkdown(st.Label), presentation.EscapeMarkdown(st.Help))
		if len(options) < 25 {
			options = append(options, discordgo.SelectMenuOption{Label: panelClip(verb+": "+st.Label, 100), Value: st.ID + ":" + to, Description: panelClip(st.Help, 100)})
		}
	}
	if len(settings) == 0 {
		b.WriteString("Nitrado offers none of these settings on this server.\n")
	}
	b.WriteString("\nNitrado applies changes when the server next restarts. The storage wipe is on the Champion website only.")
	data := &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{{
			Author: presentation.BrandAuthor(""), Title: "⚙️ Game settings", Description: b.String(),
			Color: presentation.Neutral, Footer: presentation.Footer(serverName, "Staff only"),
		}},
		Components:      []discordgo.MessageComponent{},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
	if len(options) > 0 {
		data.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: serverPanelSettingID + strconv.FormatInt(installationID, 10), Placeholder: "Change a setting", Options: options},
		}}}
	}
	return data
}

// --- logs --------------------------------------------------------------------------------------

// panelLogLines is how many of the log's last lines the message shows; the whole read part comes
// as a file.
const panelLogLines = 12

func (h *ServerPanelHandler) sendLog(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	srv, err := h.serverOf(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	log, err := h.backend.PanelLog(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondPrivate(s, i, BuildPanelLog(srv.Name, log))
}

// BuildPanelLog shows the last lines of the newest ADM log and attaches what was read of it.
func BuildPanelLog(serverName string, log PanelLog) *discordgo.InteractionResponseData {
	lines := strings.Split(strings.TrimRight(log.Text, "\n"), "\n")
	if len(lines) > panelLogLines {
		lines = lines[len(lines)-panelLogLines:]
	}
	tail := strings.Join(lines, "\n")
	// A description holds 4096 characters; keep the block well inside it.
	if r := []rune(tail); len(r) > 3000 {
		tail = string(r[len(r)-3000:])
	}
	tail = strings.ReplaceAll(tail, "```", "'''")
	desc := fmt.Sprintf("**%s** · %s\n```\n%s\n```", presentation.EscapeMarkdown(log.Name), panelSize(log.Size), tail)
	if log.Earlier {
		desc += "\nThe attached file is the end of the log. The whole file is in the Champion website's Server Hub › Logs."
	}
	return &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{{
			Author: presentation.BrandAuthor(""), Title: "📜 Newest ADM log", Description: desc,
			Color: presentation.Neutral, Footer: presentation.Footer(serverName, "Staff only"),
		}},
		Files:           []*discordgo.File{{Name: panelLogFileName(log.Name), ContentType: "text/plain", Reader: bytes.NewReader([]byte(log.Text))}},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}

func panelLogFileName(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < ' ' {
			return '_'
		}
		return r
	}, name)
	if clean == "" {
		clean = "server"
	}
	return clean + ".txt"
}

func panelSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// --- restart schedule --------------------------------------------------------------------------

func (h *ServerPanelHandler) sendSchedule(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	srv, err := h.serverOf(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	tasks, err := h.backend.PanelSchedule(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondPrivate(s, i, BuildPanelSchedule(srv.Name, tasks))
}

// BuildPanelSchedule lists the server's scheduled tasks, soonest first.
func BuildPanelSchedule(serverName string, tasks []PanelTask) *discordgo.InteractionResponseData {
	sorted := append([]PanelTask(nil), tasks...)
	sort.SliceStable(sorted, func(a, b int) bool {
		switch {
		case sorted[a].Next == nil:
			return false
		case sorted[b].Next == nil:
			return true
		}
		return sorted[a].Next.Before(*sorted[b].Next)
	})
	var b strings.Builder
	for idx, t := range sorted {
		if idx == 20 {
			fmt.Fprintf(&b, "…and %d more.\n", len(sorted)-20)
			break
		}
		action := strings.TrimSpace(strings.ReplaceAll(t.Action, "_", " "))
		if action == "" {
			action = "task"
		}
		emoji := "🗓️"
		if strings.Contains(strings.ToLower(action), "restart") {
			emoji = "🔄"
		}
		when := "no next run"
		if t.Next != nil {
			when = "next " + presentation.TimestampWithRelative(*t.Next)
		}
		fmt.Fprintf(&b, "%s **%s** · %s\n", emoji, presentation.EscapeMarkdown(strings.ToUpper(action[:1])+action[1:]), when)
	}
	if len(sorted) == 0 {
		b.WriteString("Nitrado has no scheduled tasks for this server.\n")
	}
	b.WriteString("\nChange the schedule in the Nitrado web interface.")
	return &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{{
			Author: presentation.BrandAuthor(""), Title: "⏰ Restart schedule", Description: b.String(),
			Color: presentation.Neutral, Footer: presentation.Footer(serverName, "Staff only"),
		}},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}

// --- maintenance -------------------------------------------------------------------------------

func (h *ServerPanelHandler) askMaintenance(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	if !h.requireLevel(ctx, s, i, caller, installationID, permissions.LevelAdministrator) {
		return
	}
	srv, err := h.serverOf(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	respondPrivate(s, i, BuildPanelMaintenanceAsk(srv))
}

// BuildPanelMaintenanceAsk asks to switch the server's maintenance on, or off.
func BuildPanelMaintenanceAsk(srv PanelServer) *discordgo.InteractionResponseData {
	id := strconv.FormatInt(srv.InstallationID, 10)
	text := "Turn maintenance **on**? Champion stops posting this server's public feeds (killfeed, deaths, " +
		"connections, bounties and the other feed channels) and the server status board shows it as under maintenance. " +
		"The game server keeps running, and staff alerts keep coming."
	button := discordgo.Button{Label: "Turn maintenance on", Emoji: &discordgo.ComponentEmoji{Name: "🛠️"}, Style: discordgo.DangerButton, CustomID: ServerPanelPrefix + "maint-on:" + id}
	if srv.Maintenance {
		text = "Maintenance is **on**: this server's public feeds are paused. Turn it off to post them again."
		button = discordgo.Button{Label: "Turn maintenance off", Emoji: &discordgo.ComponentEmoji{Name: "✅"}, Style: discordgo.SuccessButton, CustomID: ServerPanelPrefix + "maint-off:" + id}
	}
	return &discordgo.InteractionResponseData{
		Embeds: []*discordgo.MessageEmbed{{
			Author: presentation.BrandAuthor(""), Title: "🛠️ Maintenance", Description: text,
			Color: presentation.Amber, Footer: presentation.Footer(srv.Name, "Staff only"),
		}},
		Components:      []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{button}}},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
}

func (h *ServerPanelHandler) setMaintenance(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64, on bool) {
	deferAs(s, i, AckUpdate)
	if err := h.backend.PanelSetMaintenance(ctx, caller, installationID, on); err != nil {
		h.reportError(s, i, err)
		return
	}
	text := "🛠️ Maintenance is **on**. This server's public feeds are paused until it is turned off. Open `/server panel` again to see it."
	if !on {
		text = "✅ Maintenance is **off**. Champion posts this server's public feeds again."
	}
	empty := []discordgo.MessageComponent{}
	respondUpdate(s, i, &discordgo.InteractionResponseData{
		Embeds:     []*discordgo.MessageEmbed{{Author: presentation.BrandAuthor(""), Title: "🛠️ Maintenance", Description: text, Color: presentation.Green}},
		Components: empty,
	})
}

// --- shared panel ------------------------------------------------------------------------------

// post puts a shared panel in the channel the private one was opened in, replacing the one posted
// there before.
func (h *ServerPanelHandler) post(ctx context.Context, s *discordgo.Session, i *discordgo.InteractionCreate, caller PanelCaller, installationID int64) {
	if !h.requireLevel(ctx, s, i, caller, installationID, permissions.LevelAdministrator) {
		return
	}
	srv, err := h.serverOf(ctx, caller, installationID)
	if err != nil {
		h.reportError(s, i, err)
		return
	}
	data := h.sharedPanel(ctx, caller.DiscordGuildID, srv)
	msg, err := s.ChannelMessageSendComplex(i.ChannelID, &discordgo.MessageSend{Embeds: data.Embeds, Components: data.Components, AllowedMentions: data.AllowedMentions})
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel post failed", "err", err.Error())
		respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "I couldn't post in this channel. Give Champion **Send Messages** and **Embed Links** here, then try again."})
		return
	}
	replaced, err := h.backend.PanelPosted(ctx, caller, installationID, i.ChannelID, msg.ID)
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel post not recorded", "err", err.Error())
	}
	if replaced != "" {
		_ = s.ChannelMessageDelete(i.ChannelID, replaced)
	}
	respondPrivate(s, i, &discordgo.InteractionResponseData{Content: "📌 Posted the server panel in this channel. It updates every 2 minutes, and every click is checked against the clicker's own staff level. Posting it again here replaces it."})
}

// sharedPanel renders a posted panel with the server's current status.
func (h *ServerPanelHandler) sharedPanel(ctx context.Context, discordGuildID string, srv PanelServer) *discordgo.InteractionResponseData {
	status, err := h.backend.PanelStatus(ctx, discordGuildID, srv.InstallationID, false)
	if err != nil {
		status = PanelStatus{ReadAt: h.now()}
	}
	var lock *panelLock
	if l, ok := h.lockOf(srv.InstallationID); ok {
		lock = &l
	}
	return BuildSharedServerPanel(srv, status, lock)
}

// panelMessageEditor edits and deletes posted panels; *discordgo.Session satisfies it.
type panelMessageEditor interface {
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// RunSharedRefresh keeps every posted panel up to date until ctx ends. It runs in the one
// process that does singleton work.
func (h *ServerPanelHandler) RunSharedRefresh(api panelMessageEditor) func(ctx context.Context) {
	return func(ctx context.Context) {
		ticker := time.NewTicker(panelSharedEvery)
		defer ticker.Stop()
		for {
			h.refreshShared(ctx, api)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

func (h *ServerPanelHandler) refreshShared(ctx context.Context, api panelMessageEditor) {
	if h == nil || h.backend == nil || api == nil {
		return
	}
	posts, err := h.backend.PanelPosts(ctx)
	if err != nil {
		slog.Warn("component=discord", "msg", "server panel posts lookup failed", "err", err.Error())
		return
	}
	servers := map[string][]PanelServer{}
	for _, p := range posts {
		if ctx.Err() != nil {
			return
		}
		list, ok := servers[p.DiscordGuildID]
		if !ok {
			if list, err = h.backend.PanelServers(ctx, p.DiscordGuildID); err != nil {
				continue
			}
			servers[p.DiscordGuildID] = list
		}
		var srv *PanelServer
		for idx := range list {
			if list[idx].InstallationID == p.InstallationID {
				srv = &list[idx]
			}
		}
		if srv == nil {
			_ = h.backend.PanelPostGone(ctx, p.MessageID)
			continue
		}
		data := h.sharedPanel(ctx, p.DiscordGuildID, *srv)
		edit := discordgo.NewMessageEdit(p.ChannelID, p.MessageID)
		edit.Embeds = &data.Embeds
		edit.Components = &data.Components
		if _, err := api.ChannelMessageEditComplex(edit); err != nil {
			if isUnknownMessage(err) {
				_ = h.backend.PanelPostGone(ctx, p.MessageID)
				continue
			}
			slog.Warn("component=discord", "msg", "server panel refresh failed", "err", err.Error())
		}
	}
}
